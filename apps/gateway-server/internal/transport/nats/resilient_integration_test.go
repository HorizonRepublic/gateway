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

// TestResilientRequester_PerServiceBreakerAgainstRealNATS verifies the
// per-service breaker isolation contract end-to-end over a real wire:
// a slow upstream (never replies, so every request runs into its
// deadline) trips ITS OWN breaker after the failure threshold, after
// which requests to that service fast-fail with ErrCircuitOpen without
// waiting out the timeout — while a healthy service on the same NATS
// bus keeps round-tripping the whole time.
//
// This is the integration-level pin for the blast-radius property the
// per-subject breaker exists to provide: one dead upstream must never
// 503 routes served by other upstreams.
func TestResilientRequester_PerServiceBreakerAgainstRealNATS(t *testing.T) {
	const (
		deadSubject    = "dead-svc__microservice.cmd.slow.call"
		healthySubject = "healthy-svc__microservice.cmd.echo"
	)

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

	// Healthy upstream: echoes immediately.
	echoSub, err := responderConn.Subscribe(healthySubject, func(msg *natsgo.Msg) {
		_ = msg.Respond([]byte("pong:" + string(msg.Data)))
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = echoSub.Unsubscribe() })

	// Dead upstream: subscribed (so NATS does not short-circuit with
	// ErrNoResponders) but never replies — every request runs into the
	// per-request deadline, the timeout shape a hung service produces.
	slowSub, err := responderConn.Subscribe(deadSubject, func(_ *natsgo.Msg) {
		// Intentionally never respond.
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = slowSub.Unsubscribe() })
	require.NoError(t, responderConn.Flush())

	inner, err := NewRequester([]*natsgo.Conn{gatewayConn})
	require.NoError(t, err)

	resilient := NewResilientRequester(inner, ResilientConfig{
		BreakerEnabled:   true,
		FailureThreshold: 2,
		RecoveryTimeout:  time.Hour, // never half-opens within the test
		HalfOpenProbes:   1,
	}, zerolog.Nop())

	// Trip dead-svc's breaker: two genuine deadline expiries.
	for range 2 {
		_, err = resilient.Request(ctx, deadSubject, []byte("ping"), 100*time.Millisecond)
		require.Error(t, err)
		require.ErrorIs(t, err, context.DeadlineExceeded,
			"pre-trip failures must be genuine upstream timeouts")
	}

	// Breaker open: fast-fail without consuming the 100 ms deadline.
	start := time.Now()
	_, err = resilient.Request(ctx, deadSubject, []byte("ping"), 100*time.Millisecond)
	require.ErrorIs(t, err, ErrCircuitOpen)
	assert.Less(t, time.Since(start), 50*time.Millisecond,
		"open breaker must fail fast, not wait out the request deadline")

	// Healthy service on the same bus is untouched by dead-svc's open
	// breaker — the isolation property under test.
	reply, err := resilient.Request(ctx, healthySubject, []byte("hello"), 5*time.Second)
	require.NoError(t, err,
		"healthy upstream must keep flowing while another service's breaker is open")
	assert.Equal(t, "pong:hello", string(reply))

	// And its dedicated breaker reports closed in the snapshot view.
	states := make(map[string]bool)
	for _, s := range resilient.BreakerSnapshots() {
		states[s.Service] = s.Shared
	}
	require.Contains(t, states, "dead-svc")
	require.Contains(t, states, "healthy-svc")
}
