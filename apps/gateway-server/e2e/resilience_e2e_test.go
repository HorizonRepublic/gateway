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

// hitBurstFromIP issues a single GET /rl/burst against baseURL with
// the spoofed XFF set to ip. Returns the resolved status. Drains the
// body so the connection returns to the keep-alive pool.
func hitBurstFromIP(t *testing.T, baseURL, ip string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, baseURL+"/rl/burst", nil)
	require.NoError(t, err)
	req.Header.Set("X-Forwarded-For", ip)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer drainBody(resp)
	return resp.StatusCode
}

func TestE2E_Resilience_MemoryStoreSaturation_FailOpen(t *testing.T) {
	WaitReadyAt(t, GatewayURLMemOpen(t))

	// Cap is 2. First two distinct IPs each create a fresh bucket
	// (cap fills). The third tries to admit a new key, hits
	// ErrMemoryStoreSaturated, and the fail-open policy lets it
	// through — surface status MUST be 200.
	const ipA = "10.99.20.1"
	const ipB = "10.99.20.2"
	const ipC = "10.99.20.3"

	require.Equal(t, http.StatusOK, hitBurstFromIP(t, GatewayURLMemOpen(t), ipA),
		"first IP must succeed (bucket A created)")
	require.Equal(t, http.StatusOK, hitBurstFromIP(t, GatewayURLMemOpen(t), ipB),
		"second IP must succeed (bucket B created, cap reached)")

	// The third IP would create a NEW bucket; the cap is already at
	// 2 so admission fails ⇒ saturation ⇒ fail-open ⇒ 200.
	got := hitBurstFromIP(t, GatewayURLMemOpen(t), ipC)
	assert.Equal(t, http.StatusOK, got,
		"saturated key under fail-open MUST be allowed through; got %d", got)
}

func TestE2E_Resilience_MemoryStoreSaturation_FailClosed(t *testing.T) {
	WaitReadyAt(t, GatewayURLMemClosed(t))

	const ipA = "10.99.21.1"
	const ipB = "10.99.21.2"
	const ipC = "10.99.21.3"

	require.Equal(t, http.StatusOK, hitBurstFromIP(t, GatewayURLMemClosed(t), ipA),
		"first IP must succeed (bucket A created)")
	require.Equal(t, http.StatusOK, hitBurstFromIP(t, GatewayURLMemClosed(t), ipB),
		"second IP must succeed (bucket B created, cap reached)")

	got := hitBurstFromIP(t, GatewayURLMemClosed(t), ipC)
	assert.Equal(t, http.StatusServiceUnavailable, got,
		"saturated key under fail-closed MUST short-circuit to 503; got %d", got)
}

func TestE2E_Resilience_ConcurrencyLimitReturns503(t *testing.T) {
	WaitReadyAt(t, GatewayURLConc(t))

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
