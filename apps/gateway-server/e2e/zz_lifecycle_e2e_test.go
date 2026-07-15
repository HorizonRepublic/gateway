//go:build e2e

// Filename prefix `zz_` puts this file at the end of `go test`'s
// alphabetical sort across all e2e test files. The NATS-restart test
// in here disrupts the entire stack — every gateway replica's NATS
// connection breaks while the bus is down. Subsequent tests would
// see flapping requests until reconnect lands. Sequencing this file
// last means by the time `compose.Down` runs in TestMain, recovery
// has already been observed (the test polls until /users/alice
// returns 200 again).

package e2e

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestE2E_Resilience_NATSRestartRecovery(t *testing.T) {
	WaitReady(t)

	// Pre-condition: gateway healthy end-to-end. A failure here
	// means the test is misconfigured — not a regression.
	respPre, err := http.Get(GatewayURL(t) + "/users/alice")
	require.NoError(t, err)
	_ = respPre.Body.Close()
	require.Equal(t, http.StatusOK, respPre.StatusCode,
		"sanity probe before disruption must return 200")

	// Disrupt: stop the nats container. Every gateway and the
	// example-app lose their NATS connection.
	StopNATS(t)

	// Issue a request while NATS is down. Gateway has no upstream
	// to reach; outcome MUST be a 5xx (typically 504 Gateway
	// Timeout from the proxy handler when the NATS request fails).
	// We don't pin the exact status — what matters is the gateway
	// is still serving the HTTP listener (no crash) and degrades
	// to a 5xx rather than serving stale data.
	respDown, err := http.Get(GatewayURL(t) + "/users/alice")
	require.NoError(t, err, "gateway HTTP listener must still accept connections while NATS is down")
	t.Cleanup(func() { _ = respDown.Body.Close() })
	assert.GreaterOrEqual(t, respDown.StatusCode, 500,
		"with NATS down, gateway MUST surface a 5xx (got %d)", respDown.StatusCode)
	assert.Less(t, respDown.StatusCode, 600,
		"5xx range only — anything outside is a regression")

	// Restart NATS and wait for full end-to-end recovery. The chain
	// is: nats container start → JetStream stream/KV reload →
	// nestjs-jetstream reconnect → example-app re-registers handler
	// metadata → gateway-server watcher delta → request serves.
	StartNATS(t)
	WaitForGatewayHealthy(t, GatewayURL(t))

	// Final probe: explicit re-check that recovery is observable for
	// THIS test. WaitForGatewayHealthy confirms the readiness signal
	// (NATS reconnected AND a routing snapshot has landed), but the
	// route entry itself re-registers through a separate async chain
	// — example-app re-publishes its handler metadata to KV via
	// nestjs-jetstream after ITS own reconnect, and the gateway
	// watcher applies that delta a beat later. Recovery of a specific
	// route is therefore eventual, not simultaneous with readyz; poll
	// until it serves rather than pinning a single instant that races
	// the re-registration.
	require.Eventually(t, func() bool {
		resp, err := http.Get(GatewayURL(t) + "/users/alice")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()

		return resp.StatusCode == http.StatusOK
	}, 30*time.Second, 250*time.Millisecond,
		"after NATS restart + recovery wait, gateway MUST serve the route again")

	// Brief settle margin: lets in-flight metadata refreshes finish
	// before TestMain runs compose.Down. Not load-bearing for the
	// assertion above.
	time.Sleep(100 * time.Millisecond)
}
