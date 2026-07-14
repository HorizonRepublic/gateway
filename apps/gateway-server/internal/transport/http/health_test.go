package http

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
)

// probeStatus drives one handler with a synthetic GET and returns the
// response status code. The operator handlers are plain net/http
// funcs, so httptest.NewRecorder exercises them hermetically.
func probeStatus(handler http.HandlerFunc) int {
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	return rec.Code
}

// TestLiveHandler_AlwaysReturns200 pins the unconditional liveness
// contract: as long as the handler dispatches, the probe succeeds.
// K8s liveness failure restarts the pod; the gateway has no
// "alive but unhealthy" state worth surfacing through this probe.
func TestLiveHandler_AlwaysReturns200(t *testing.T) {
	assert.Equal(t, http.StatusOK, probeStatus(liveHandler()))
}

// TestReadyHandler_ReturnsServiceUnavailableWhenNotReady pins the
// readiness contract during cold boot: a probe arriving before the
// initial routing snapshot lands gets 503 so the load balancer
// withholds traffic.
func TestReadyHandler_ReturnsServiceUnavailableWhenNotReady(t *testing.T) {
	var ready atomic.Bool

	status := probeStatus(readyHandler(ReadinessFunc(ready.Load)))

	assert.Equal(t, http.StatusServiceUnavailable, status,
		"readyz must return 503 while the readiness signal reports false")
}

// TestReadyHandler_Returns200WhenReady pins the steady-state
// behaviour after the initial snapshot lands.
func TestReadyHandler_Returns200WhenReady(t *testing.T) {
	var ready atomic.Bool
	ready.Store(true)

	assert.Equal(t, http.StatusOK, probeStatus(readyHandler(ReadinessFunc(ready.Load))))
}

// TestReadyHandler_NilSignalIsAlwaysReady documents the degraded
// fallback: a server constructed without a real signal still
// answers /readyz so test harnesses do not need to fabricate one.
func TestReadyHandler_NilSignalIsAlwaysReady(t *testing.T) {
	assert.Equal(t, http.StatusOK, probeStatus(readyHandler(nil)))
}
