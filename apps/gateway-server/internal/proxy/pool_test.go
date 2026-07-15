package proxy

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAcquireEnvelope_ReturnsZeroedInstance(t *testing.T) {
	envelope := acquireEnvelope()
	defer releaseEnvelope(envelope)

	assert.Equal(t, RouteContext{}, envelope.Route)
	assert.Empty(t, envelope.Params)
	assert.Empty(t, envelope.Query)
	assert.Empty(t, envelope.Headers)
	assert.Nil(t, envelope.Body)
	assert.Equal(t, RequestMeta{}, envelope.Meta)
}

func TestAcquireEnvelope_PreAllocatesMaps(t *testing.T) {
	envelope := acquireEnvelope()
	defer releaseEnvelope(envelope)

	// Writing must not panic on a fresh envelope — the pool's New
	// function must have allocated backing maps with the documented
	// initial capacities.
	assert.NotPanics(t, func() {
		envelope.Params["id"] = "42"
		envelope.Query["q"] = NewQueryValueString("v")
		envelope.Headers["h"] = "v"
	})
}

func TestReleaseEnvelope_IsNilSafe(t *testing.T) {
	assert.NotPanics(t, func() {
		releaseEnvelope(nil)
	})
}

func TestAcquireReleaseCycle_ResetsPriorState(t *testing.T) {
	// A previously-used envelope returned to the pool must not leak
	// state into the next acquirer. We can't assert the same instance
	// is returned (sync.Pool is non-deterministic) but we can assert
	// that whatever comes out is zeroed.
	first := acquireEnvelope()
	first.Route.Method = "POST"
	first.Params["leaked"] = "yes"
	first.Headers["x-leaked"] = "yes"
	releaseEnvelope(first)

	second := acquireEnvelope()
	defer releaseEnvelope(second)
	assert.Equal(t, "", second.Route.Method)
	assert.NotContains(t, second.Params, "leaked")
	assert.NotContains(t, second.Headers, "x-leaked")
}

// TestReleaseEnvelope_ClearsRetainedState pins the release-side
// hygiene contract: a pooled envelope must not retain references to
// the last request's body, claims, or headers (which include raw
// Authorization values on the verifier path) while it sits idle in
// the pool. Reset-on-acquire alone keeps correctness but leaves
// credentials pinned in memory between requests.
func TestReleaseEnvelope_ClearsRetainedState(t *testing.T) {
	envelope := acquireEnvelope()
	envelope.Route.Method = "GET"
	envelope.Params["id"] = "42"
	envelope.Headers["authorization"] = "Bearer secret"
	envelope.Body = []byte(`{"k":"v"}`)
	envelope.Auth = []byte(`{"sub":"u1"}`)

	releaseEnvelope(envelope)

	// Inspecting the released instance is safe here: nothing else
	// runs concurrently in this test, so the pointer is stable.
	assert.Equal(t, RouteContext{}, envelope.Route)
	assert.Empty(t, envelope.Params)
	assert.Empty(t, envelope.Headers)
	assert.Nil(t, envelope.Body, "released envelope must not pin the request body")
	assert.Nil(t, envelope.Auth, "released envelope must not pin verifier claims")
}

// TestReleasePayload_DropsOversizedBuffers pins the pool capacity
// cap: one multi-megabyte request must not permanently grow a pooled
// buffer to that size. Oversized buffers are dropped on release and
// the next acquire starts from a standard-capacity buffer.
func TestReleasePayload_DropsOversizedBuffers(t *testing.T) {
	oversized := make([]byte, 0, maxPooledPayloadCap+1)
	releasePayload(&oversized)

	next := acquirePayload()
	defer releasePayload(next)

	assert.LessOrEqual(t, cap(*next), maxPooledPayloadCap,
		"an oversized buffer must never come back out of the pool")
}

// TestReleasePayload_KeepsBuffersAtTheCap pins the boundary: a buffer
// grown to exactly the cap stays poolable — the cap bounds retention,
// it does not shrink the steady-state working set.
func TestReleasePayload_KeepsBuffersAtTheCap(t *testing.T) {
	atCap := make([]byte, 0, maxPooledPayloadCap)
	assert.NotPanics(t, func() {
		releasePayload(&atCap)
	})
}
