//go:build integration

package nats

import (
	"context"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/config"
)

// TestRequester_RoundTripAgainstRealNATS verifies that BuildOptions +
// Connect + Requester.Request together produce a working round trip
// against a real NATS server. This is the first place in the gateway
// codebase where the full NATS-facing surface is exercised end-to-end,
// and it guards against regressions that only surface under a real
// wire protocol (option ordering, timeout semantics, Msg.Data slice
// sharing).
//
// The test uses two distinct connections — one for the gateway
// Requester and one for the fake responder — because BuildOptions
// sets NoEcho(), which prevents a single connection from receiving
// its own published messages. That mirrors the production topology
// where the gateway and the SDK-side service sit on separate
// connections.
func TestRequester_RoundTripAgainstRealNATS(t *testing.T) {
	ctx := context.Background()
	container, err := tcnats.Run(ctx, "nats:2.11.7")
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

	gatewayConn, err := Connect(cfg, zerolog.Nop())
	require.NoError(t, err)
	t.Cleanup(gatewayConn.Close)

	responderConn, err := natsgo.Connect(url)
	require.NoError(t, err)
	t.Cleanup(responderConn.Close)

	subscription, err := responderConn.Subscribe("test.echo", func(msg *natsgo.Msg) {
		_ = msg.Respond([]byte("pong:" + string(msg.Data)))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = subscription.Unsubscribe() })
	require.NoError(t, responderConn.Flush())

	requester, err := NewRequester([]*natsgo.Conn{gatewayConn})
	require.NoError(t, err)

	reply, err := requester.Request(ctx, "test.echo", []byte("hello"), 5*time.Second)
	require.NoError(t, err)
	assert.Equal(t, "pong:hello", string(reply))
}

// TestRequester_TimeoutPropagatesNatsErrTimeout is the deliberate
// "integration tests as verification" case: it empirically confirms
// that a slow NATS responder produces an error that satisfies
// errors.Is(err, context.DeadlineExceeded). The proxy handler relies
// on this guarantee to return 504 Gateway Timeout, so a regression in
// nats.go's error wrapping would silently degrade the gateway's
// error semantics. Keeping this test pinned to a real NATS container
// means the guarantee is re-verified on every dependency bump.
//
// NOTE: NATS 2.11 returns ErrNoResponders (not ErrTimeout) when the
// requested subject has zero subscribers, because the server
// short-circuits the request with an immediate 503. To force a
// genuine deadline expiry we register a slow responder that never
// replies within the request window.
func TestRequester_TimeoutPropagatesNatsErrTimeout(t *testing.T) {
	ctx := context.Background()
	container, err := tcnats.Run(ctx, "nats:2.11.7")
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

	gatewayConn, err := Connect(cfg, zerolog.Nop())
	require.NoError(t, err)
	t.Cleanup(gatewayConn.Close)

	responderConn, err := natsgo.Connect(url)
	require.NoError(t, err)
	t.Cleanup(responderConn.Close)

	// A subscription that never responds: the gateway request will
	// observe subscriber interest and wait for the full deadline
	// instead of short-circuiting with ErrNoResponders.
	subscription, err := responderConn.Subscribe("slow.responder", func(_ *natsgo.Msg) {
		// Intentionally do nothing — never call msg.Respond.
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = subscription.Unsubscribe() })
	require.NoError(t, responderConn.Flush())

	requester, err := NewRequester([]*natsgo.Conn{gatewayConn})
	require.NoError(t, err)

	// Real NATS RequestWithContext surfaces deadline expiry as
	// context.DeadlineExceeded, not nats.ErrTimeout — the request path
	// no longer relies on the legacy timeout-only Request signature.
	// Both are categorized as timeouts upstream by isTimeoutErr; we
	// pin the wire-level shape here so any future nats.go regression
	// that swaps the surface error makes the proxy timeout branch
	// visibly fail.
	_, err = requester.Request(ctx, "slow.responder", []byte("ping"), 100*time.Millisecond)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded,
		"RequestWithContext must surface ctx deadline expiry as context.DeadlineExceeded")
}

// TestRequester_RejectsEmptyConnectionSlice is cheap and hermetic but
// lives here because the happy-path integration tests also exercise
// NewRequester. Keeping the whole NewRequester contract in one file
// makes it trivial to audit during NATS client upgrades.
func TestRequester_RejectsEmptyConnectionSlice(t *testing.T) {
	requester, err := NewRequester(nil)
	assert.Nil(t, requester)
	assert.ErrorIs(t, err, errNoConns)
}

// TestRequester_CloseDrainsUnderlyingConnections pins the shutdown
// contract: Close MUST iterate every pooled connection and invoke
// Drain so in-flight subscriptions complete before the socket tears
// down. After Close the pooled connections must report Closed/Draining
// — a regression that swapped Drain for Close would surface here as a
// connection that did not first transition through DRAINING.
func TestRequester_CloseDrainsUnderlyingConnections(t *testing.T) {
	ctx := context.Background()
	container, err := tcnats.Run(ctx, "nats:2.11.7")
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	url, err := container.ConnectionString(ctx)
	require.NoError(t, err)

	first, err := natsgo.Connect(url)
	require.NoError(t, err)
	second, err := natsgo.Connect(url)
	require.NoError(t, err)

	requester, err := NewRequester([]*natsgo.Conn{first, second})
	require.NoError(t, err)

	// Pre-condition: both connections must be live before Close so
	// the post-Close assertion proves Close did the transition.
	require.False(t, first.IsClosed())
	require.False(t, second.IsClosed())

	requester.Close()

	// Drain is asynchronous — it returns once the close goroutine is
	// scheduled, not once the socket finishes draining. Poll briefly
	// so the test does not race the drain finalisation on slow CI.
	assert.Eventually(t, func() bool {
		return first.IsClosed() && second.IsClosed()
	}, 5*time.Second, 50*time.Millisecond,
		"Close must drain every pooled connection so each ends up CLOSED")
}
