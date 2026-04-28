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
// the NATS connection is CONNECTED.
//
// PR 1 does not pin the response body shape — only that 200 surfaces.
// Subsequent feature-pack PRs add their own e2e files; this test stays
// here as the canary for "the Compose orchestration itself works".
func TestE2E_GatewayReadyz(t *testing.T) {
	WaitReady(t)

	resp, err := http.Get(GatewayURL(t) + "/readyz")
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	assert.Equal(t, http.StatusOK, resp.StatusCode)
}
