//go:build e2e

package e2e

import (
	"io"
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// drainBody fully reads and closes the response body so the underlying
// connection returns to the keep-alive pool. Required when looping
// rapid requests against the same route — leaking bodies caps the
// effective parallelism at the http.DefaultClient connection limit and
// occasionally surfaces as flaky 429 timing.
func drainBody(resp *http.Response) {
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

// rateLimitGet issues a GET with an optional X-Forwarded-For spoof
// (used for keyBy:ip isolation), an optional X-API-Key header (keyBy:
// header), and an optional Bearer token (keyBy: user:sub on auth-
// protected routes).
func rateLimitGet(t *testing.T, path, xff, apiKey, bearer string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, GatewayURL(t)+path, nil)
	require.NoError(t, err)
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

func TestE2E_RateLimit_BurstAllowsThenLimits(t *testing.T) {
	WaitReady(t)

	// Use a unique XFF so this test's bucket is independent of any
	// other test's bucket on /rl/burst. Memory store survives across
	// the test process; coupling buckets between tests would couple
	// outcomes.
	const ip = "10.99.0.1"

	statuses := make([]int, 3)
	for i := 0; i < 3; i++ {
		resp := rateLimitGet(t, "/rl/burst", ip, "", "")
		statuses[i] = resp.StatusCode
		drainBody(resp)
	}

	assert.Equal(t, []int{http.StatusOK, http.StatusOK, http.StatusTooManyRequests}, statuses,
		"rps=1 burst=2 ⇒ first two pass, third is rejected with 429")
}

func TestE2E_RateLimit_HeadersAlwaysPresent(t *testing.T) {
	WaitReady(t)

	const ip = "10.99.0.2"

	respAllow := rateLimitGet(t, "/rl/burst", ip, "", "")
	t.Cleanup(func() { drainBody(respAllow) })
	assert.Equal(t, http.StatusOK, respAllow.StatusCode)
	assert.NotEmpty(t, respAllow.Header.Get("X-RateLimit-Limit"),
		"allow path stamps X-RateLimit-Limit")
	assert.NotEmpty(t, respAllow.Header.Get("X-RateLimit-Remaining"),
		"allow path stamps X-RateLimit-Remaining")
	assert.NotEmpty(t, respAllow.Header.Get("X-RateLimit-Reset"),
		"allow path stamps X-RateLimit-Reset")

	// Exhaust budget: one allow already consumed (burst was 2 ⇒ 1
	// token left), so two more allows the third gets rejected.
	for i := 0; i < 2; i++ {
		drainBody(rateLimitGet(t, "/rl/burst", ip, "", ""))
	}

	respReject := rateLimitGet(t, "/rl/burst", ip, "", "")
	t.Cleanup(func() { drainBody(respReject) })
	require.Equal(t, http.StatusTooManyRequests, respReject.StatusCode,
		"setup must produce a 429")
	assert.NotEmpty(t, respReject.Header.Get("X-RateLimit-Limit"),
		"reject path stamps X-RateLimit-Limit")
	assert.NotEmpty(t, respReject.Header.Get("X-RateLimit-Remaining"),
		"reject path stamps X-RateLimit-Remaining (typically '0')")
	assert.NotEmpty(t, respReject.Header.Get("X-RateLimit-Reset"),
		"reject path stamps X-RateLimit-Reset")
}

func TestE2E_RateLimit_RetryAfterOnReject(t *testing.T) {
	WaitReady(t)

	const ip = "10.99.0.3"

	// Exhaust burst (3 hits ⇒ third is 429).
	var rejected *http.Response
	for i := 0; i < 3; i++ {
		resp := rateLimitGet(t, "/rl/burst", ip, "", "")
		if resp.StatusCode == http.StatusTooManyRequests {
			rejected = resp
			break
		}
		drainBody(resp)
	}
	require.NotNil(t, rejected, "must receive a 429 within 3 hits")
	t.Cleanup(func() { drainBody(rejected) })

	retryAfter := rejected.Header.Get("Retry-After")
	require.NotEmpty(t, retryAfter, "429 must stamp Retry-After")
	secs, err := strconv.Atoi(retryAfter)
	require.NoError(t, err, "Retry-After must be an integer-seconds value: %q", retryAfter)
	assert.GreaterOrEqual(t, secs, 1,
		"gateway clamps sub-second waits to ≥1 (Retry-After: 0 misleads many client libs)")
}

func TestE2E_RateLimit_KeyByIpIsolation(t *testing.T) {
	WaitReady(t)

	const ipA = "10.99.1.1"
	const ipB = "10.99.1.2"

	// Exhaust burst on /rl/burst for ipA.
	for i := 0; i < 3; i++ {
		drainBody(rateLimitGet(t, "/rl/burst", ipA, "", ""))
	}

	// ipB starts with a fresh bucket; first request must succeed.
	respB := rateLimitGet(t, "/rl/burst", ipB, "", "")
	t.Cleanup(func() { drainBody(respB) })
	assert.Equal(t, http.StatusOK, respB.StatusCode,
		"keyBy:'ip' isolation: ipB must have its own bucket independent of ipA's exhaustion")
}

func TestE2E_RateLimit_KeyByHeaderIsolation(t *testing.T) {
	WaitReady(t)

	// Exhaust burst on /rl/by-header for X-API-Key=alpha.
	for i := 0; i < 3; i++ {
		drainBody(rateLimitGet(t, "/rl/by-header", "", "alpha", ""))
	}

	// Different header value must have its own bucket.
	respBeta := rateLimitGet(t, "/rl/by-header", "", "beta", "")
	t.Cleanup(func() { drainBody(respBeta) })
	assert.Equal(t, http.StatusOK, respBeta.StatusCode,
		"keyBy:'header:x-api-key' isolation: 'beta' has its own bucket")
}

func TestE2E_RateLimit_KeyByUserIsolation(t *testing.T) {
	WaitReady(t)

	// Exhaust burst on /rl/by-user for token demo-alice (sub=alice).
	for i := 0; i < 3; i++ {
		drainBody(rateLimitGet(t, "/rl/by-user", "", "", "demo-alice"))
	}

	// Different authenticated subject ⇒ independent bucket.
	respAdmin := rateLimitGet(t, "/rl/by-user", "", "", "demo-admin")
	t.Cleanup(func() { drainBody(respAdmin) })
	assert.Equal(t, http.StatusOK, respAdmin.StatusCode,
		"keyBy:'user:sub' isolation: subject 'admin' has its own bucket independent of 'alice'")
}
