//go:build e2e

package e2e

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rateLimitGetAt issues the same shape of request as rateLimitGet but
// targets an explicit base URL, so the multi-replica tests can split
// traffic between replicas A and B without changing the rest of the
// helper surface.
func rateLimitGetAt(t *testing.T, baseURL, path, xff string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, baseURL+path, nil)
	require.NoError(t, err)
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

func TestE2E_RateLimitMulti_NatsKVSharedAcrossReplicas(t *testing.T) {
	WaitReady(t)
	WaitReadyB(t)

	// Unique IP so this test's bucket is independent of any other
	// test that hits the natskv route. The bucket key is derived from
	// (route, IP); a fresh IP guarantees a fresh TAT in JetStream KV.
	const ip = "10.99.50.1"

	// Exhaust the budget on replica A: rps=1, burst=1 ⇒ 3 hits = [200, 429, 429]
	// (canonical GCRA: burst=1 admits exactly one before the rate gate).
	statusesA := make([]int, 3)
	for i := 0; i < 3; i++ {
		resp := rateLimitGetAt(t, GatewayURL(t), "/rl/multi-natskv", ip)
		statusesA[i] = resp.StatusCode
		drainBody(resp)
	}
	require.Equal(t, []int{http.StatusOK, http.StatusTooManyRequests, http.StatusTooManyRequests}, statusesA,
		"replica A must produce the standard burst-then-reject sequence on the natskv route")

	// One more hit on replica B with the same IP. Memory store would
	// have a fresh bucket; nats-kv reads the TAT replica A wrote.
	respB := rateLimitGetAt(t, GatewayURLB(t), "/rl/multi-natskv", ip)
	t.Cleanup(func() { drainBody(respB) })
	assert.Equal(t, http.StatusTooManyRequests, respB.StatusCode,
		"store:'nats-kv' shares TAT across replicas: replica B must see the bucket exhausted by A")
}

func TestE2E_RateLimitMulti_MemoryIndependentAcrossReplicas(t *testing.T) {
	WaitReady(t)
	WaitReadyB(t)

	// Unique IP so the memory bucket on replica A is fresh for this
	// test; replica B's matching bucket is also fresh because memory
	// state never leaves the process.
	const ip = "10.99.51.1"

	// Exhaust the budget on replica A's memory-backed /rl/burst.
	statusesA := make([]int, 3)
	for i := 0; i < 3; i++ {
		resp := rateLimitGetAt(t, GatewayURL(t), "/rl/burst", ip)
		statusesA[i] = resp.StatusCode
		drainBody(resp)
	}
	require.Equal(t, []int{http.StatusOK, http.StatusTooManyRequests, http.StatusTooManyRequests}, statusesA,
		"replica A memory bucket must follow the standard burst-then-reject sequence")

	// One hit on replica B with the same IP. Memory state is per-process;
	// replica B's bucket has never seen a request for this IP, so it
	// must allow.
	respB := rateLimitGetAt(t, GatewayURLB(t), "/rl/burst", ip)
	t.Cleanup(func() { drainBody(respB) })
	assert.Equal(t, http.StatusOK, respB.StatusCode,
		"store:'memory' (default) is per-replica: replica B's bucket is independent of A's exhaustion")
}
