//go:build e2e

package e2e

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestE2E_GatewayReadyz is the bootstrap smoke test. It asserts that
// the entire stack came up in the right order (NATS healthy →
// example-app healthy → gateway-server reachable) and that the
// gateway's readiness probe reports OK once snapshotLanded is true and
// the NATS connection is CONNECTED. Probes live on the operator
// listener; the body shape is not pinned — only that 200 surfaces.
func TestE2E_GatewayReadyz(t *testing.T) {
	WaitReady(t)

	resp, err := http.Get(OperatorURL(t, "gateway-server") + "/readyz")
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

// TestE2E_ProbesAbsentFromPublicPort pins the operator trust
// boundary: /healthz and /readyz are reachable ONLY on the operator
// listener. On the public port they fall through to the routing
// table like any unregistered path — an outside client learns
// nothing about the gateway's probe surface, and every future
// operator endpoint (metrics, pprof, admin) inherits this isolation
// by construction.
func TestE2E_ProbesAbsentFromPublicPort(t *testing.T) {
	WaitReady(t)

	for _, path := range []string{"/healthz", "/readyz"} {
		resp, err := http.Get(GatewayURL(t) + path)
		require.NoError(t, err)
		_ = resp.Body.Close()
		assert.Equal(t, http.StatusNotFound, resp.StatusCode,
			"%s must not exist on the public listener", path)
	}

	respLive, err := http.Get(OperatorURL(t, "gateway-server") + "/healthz")
	require.NoError(t, err)
	_ = respLive.Body.Close()
	assert.Equal(t, http.StatusOK, respLive.StatusCode,
		"/healthz must answer on the operator listener")
}
