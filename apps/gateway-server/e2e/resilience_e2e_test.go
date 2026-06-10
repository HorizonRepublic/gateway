//go:build e2e

package e2e

import (
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hitPathFromIP issues a single GET path against baseURL with the
// spoofed XFF set to ip. Returns the resolved status. Drains the
// body so the connection returns to the keep-alive pool.
func hitPathFromIP(t *testing.T, baseURL, path, ip string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, baseURL+path, nil)
	require.NoError(t, err)
	req.Header.Set("X-Forwarded-For", ip)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer drainBody(resp)
	return resp.StatusCode
}

// TestE2E_Resilience_MemoryStoreSaturation_PerRouteFailPolicy pins
// both fail-policy branches AND the route-over-env precedence on ONE
// replica (gateway-wide RATELIMIT_FAIL_POLICY=open, memory cap 2):
//
//  1. Two distinct IPs on /rl/burst create two buckets — cap full.
//  2. A third fresh IP on /rl/burst hits ErrMemoryStoreSaturated;
//     the route declares no failPolicy, inherits env open ⇒ 200.
//  3. A fresh IP on /rl/burst-closed (per-route failPolicy: closed)
//     hits the same saturation ⇒ 503 — the route-level value beats
//     the availability-first env default.
//
// The previous shape proved the two branches via two replicas with
// opposite env values; per-route wiring makes the contrast expressible
// in route metadata, which is the actual operator-facing contract.
func TestE2E_Resilience_MemoryStoreSaturation_PerRouteFailPolicy(t *testing.T) {
	WaitReadyAt(t, OperatorURL(t, "gateway-server-mem-open"))

	const ipA = "10.99.20.1"
	const ipB = "10.99.20.2"
	const ipC = "10.99.20.3"
	const ipD = "10.99.20.4"

	require.Equal(t, http.StatusOK, hitPathFromIP(t, GatewayURLMemOpen(t), "/rl/burst", ipA),
		"first IP must succeed (bucket A created)")
	require.Equal(t, http.StatusOK, hitPathFromIP(t, GatewayURLMemOpen(t), "/rl/burst", ipB),
		"second IP must succeed (bucket B created, cap reached)")

	gotOpen := hitPathFromIP(t, GatewayURLMemOpen(t), "/rl/burst", ipC)
	assert.Equal(t, http.StatusOK, gotOpen,
		"saturated key on the inherit-env route MUST be allowed (env open); got %d", gotOpen)

	gotClosed := hitPathFromIP(t, GatewayURLMemOpen(t), "/rl/burst-closed", ipD)
	assert.Equal(t, http.StatusServiceUnavailable, gotClosed,
		"saturated key under per-route failPolicy=closed MUST short-circuit to 503 "+
			"even though the gateway-wide policy is open; got %d", gotClosed)
}

func TestE2E_Resilience_ConcurrencyLimitReturns503(t *testing.T) {
	WaitReadyAt(t, OperatorURL(t, "gateway-server-conc"))

	// Replica caps in-flight requests at 1. The handler sleeps
	// 500ms, so the second request races the bounded semaphore
	// while the first holds the slot.
	url := GatewayURLConc(t) + "/__res/slow"

	var (
		wg          sync.WaitGroup
		muStatuses  sync.Mutex
		statuses    []int
		retryAfters []string
	)

	wg.Add(2)
	go func() {
		defer wg.Done()
		// First request grabs the slot.
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Get(url)
		require.NoError(t, err)
		defer drainBody(resp)
		muStatuses.Lock()
		statuses = append(statuses, resp.StatusCode)
		retryAfters = append(retryAfters, resp.Header.Get("Retry-After"))
		muStatuses.Unlock()
	}()

	// Tiny gap so request 1 starts (server-side accept + middleware
	// enter) before request 2 fires. 50 ms is well below the
	// handler's 500 ms hold and well above local-loopback RTT
	// jitter.
	time.Sleep(50 * time.Millisecond)

	go func() {
		defer wg.Done()
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Get(url)
		require.NoError(t, err)
		defer drainBody(resp)
		muStatuses.Lock()
		statuses = append(statuses, resp.StatusCode)
		retryAfters = append(retryAfters, resp.Header.Get("Retry-After"))
		muStatuses.Unlock()
	}()

	wg.Wait()

	// Sort the observations so order doesn't matter — request 1 may
	// finish first or after the 503, depending on scheduling.
	require.Len(t, statuses, 2)

	// One must be 200 (the slot-holder), the other 503 (rejected).
	gotOK, got503 := false, false
	var rejectedRetryAfter string
	for i, s := range statuses {
		switch s {
		case http.StatusOK:
			gotOK = true
		case http.StatusServiceUnavailable:
			got503 = true
			rejectedRetryAfter = retryAfters[i]
		}
	}

	assert.True(t, gotOK, "one request must hold the slot and return 200; got %v", statuses)
	assert.True(t, got503, "second concurrent request must reject with 503; got %v", statuses)

	// Retry-After: 1 per concurrency_middleware.go. A non-integer
	// value would mean a future regression broke the format.
	require.NotEmpty(t, rejectedRetryAfter, "503 must stamp Retry-After")
	secs, err := strconv.Atoi(rejectedRetryAfter)
	require.NoError(t, err, "Retry-After must be an integer-seconds value: %q", rejectedRetryAfter)
	assert.GreaterOrEqual(t, secs, 1,
		"Retry-After ≥ 1 keeps client retry libs from interpreting 0 as 'retry immediately'")
}
