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

