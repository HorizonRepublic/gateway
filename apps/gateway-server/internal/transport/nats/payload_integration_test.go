//go:build integration

package nats

import (
	"context"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/config"
)

// TestPayloadBudget_AgainstRealMaxPayload pins the three external
// claims the payload-budget machinery is built on, against a real
// NATS server configured with a deliberately tiny max_payload:
//
//  1. Conn.MaxPayload() reflects the server-advertised limit from the
//     INFO handshake — the startup check therefore validates against
//     the actual cluster, not an assumed default.
//  2. ValidatePayloadBudget fails closed for a configuration whose
//     HTTP caps cannot fit that live limit.
//  3. An oversized request is rejected CLIENT-SIDE with
//     nats.ErrMaxPayload before touching the wire (no responder
//     exists here, yet the error is immediate, not a timeout), and
//     the sentinel survives the Requester's error wrapping so the
//     proxy's errors.Is-based 413 mapping fires.
func TestPayloadBudget_AgainstRealMaxPayload(t *testing.T) {
	const serverMaxPayload = 1024

	// max_payload is a config-file-only option — nats-server exposes
	// no CLI flag for it, so WithArgument cannot set it.
	ctx := context.Background()
	container, err := tcnats.Run(ctx, "nats:2.11.7",
		tcnats.WithConfigFile(strings.NewReader("max_payload: 1024\n")))
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	url, err := container.ConnectionString(ctx)
	require.NoError(t, err)

	cfg := &config.Config{
		NATSUrls:             []string{url},
		NATSRandomizeUrls:    true,
		NATSMaxReconnects:    -1,
		NATSReconnectWait:    1 * time.Second,
		NATSReconnectBufSize: 1 << 20,
	}

	// The nats testcontainer with a custom config file can return from
	// Run a beat before the server is accepting connections, and the
	// production Connect uses a fail-fast timeout (no RetryOnFailed
	// Connect). On a loaded CI runner that races into a spurious dial
	// failure, so retry the initial connect until the container is
	// truly ready rather than pinning the first attempt.
	var conn *natsgo.Conn
	require.Eventually(t, func() bool {
		c, connErr := Connect(cfg, zerolog.Nop())
		if connErr != nil {
			return false
		}
		conn = c

		return true
	}, 15*time.Second, 250*time.Millisecond, "must connect to the NATS testcontainer")
	t.Cleanup(conn.Close)

	// Claim 1: the live connection reports the server's configured
	// limit, not the stock 1 MiB default.
	require.Equal(t, int64(serverMaxPayload), conn.MaxPayload(),
		"Conn.MaxPayload must reflect the server-advertised INFO value")

	// Claim 2: the gateway's default HTTP caps cannot fit this
	// cluster, and the startup check says so instead of letting
	// requests fail one by one at publish time.
	err = ValidatePayloadBudget(conn.MaxPayload(), 983040, 16384)
	require.Error(t, err)
	assert.ErrorContains(t, err, "max_payload")

	// Claim 3: an over-limit request fails immediately with the
	// ErrMaxPayload sentinel — client-side, before the wire — and the
	// sentinel survives the Requester's subject wrapping. The timeout
	// below would dominate the test's runtime if the rejection were
	// NOT client-side.
	requester, err := NewRequester([]*natsgo.Conn{conn})
	require.NoError(t, err)

	start := time.Now()
	_, err = requester.Request(ctx, "payload.overflow", make([]byte, serverMaxPayload*4), 10*time.Second)
	require.Error(t, err)
	require.ErrorIs(t, err, natsgo.ErrMaxPayload,
		"the sentinel must survive wrapping so the proxy can map it to 413")
	assert.Less(t, time.Since(start), 2*time.Second,
		"the rejection must be client-side and immediate, not a round-trip timeout")
}
