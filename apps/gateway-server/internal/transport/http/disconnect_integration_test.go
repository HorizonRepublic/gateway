//go:build integration

package http

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/config"
	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/proxy"
	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/routing"
)

// blockingRequester is a proxy.NatsRequester that parks every request
// on its context exactly like the real transport does (child context =
// min(ctx deadline, now+timeout)) and reports how the request ended.
// It stands in for a slow upstream so the test can observe whether a
// client TCP disconnect reaches the in-flight NATS round trip.
type blockingRequester struct {
	started   chan struct{}
	unblocked chan error
}

func (b *blockingRequester) Request(
	ctx context.Context,
	_ string,
	_ []byte,
	timeout time.Duration,
) ([]byte, error) {
	close(b.started)

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	<-reqCtx.Done()
	err := reqCtx.Err()
	b.unblocked <- err

	return nil, err
}

// TestServer_ClientDisconnectCancelsInFlightRequestContext is the
// empirical proof behind WithSenseClientDisconnection in NewServer.
//
// Hertz defaults SenseClientDisconnection to false, in which case the
// stdCtx it hands the adapter is never cancelled on client disconnect
// and an abandoned request holds its NATS round trip (plus one NATS
// in-flight semaphore slot and one HTTP concurrency slot) for the
// full per-route timeout. With the option enabled, the transport
// cancels the per-connection context the moment the client's TCP
// connection drops, and that cancellation must propagate through the
// adapter and proxy.Handler into the requester's ctx.
//
// The route timeout here is deliberately far longer than the test's
// own deadline: the only way the requester unblocks in time is the
// disconnect-driven cancellation, and the error shape must be
// context.Canceled (client went away), not context.DeadlineExceeded
// (budget expired).
func TestServer_ClientDisconnectCancelsInFlightRequestContext(t *testing.T) {
	const routeTimeout = 30 * time.Second

	requester := &blockingRequester{
		started:   make(chan struct{}),
		unblocked: make(chan error, 1),
	}

	table := routing.BuildTableFromRoutes([]routing.Route{{
		Subject:      "it.disconnect.slow",
		Method:       "GET",
		PathTemplate: "/slow",
	}})

	handler := proxy.NewHandler(proxy.HandlerConfig{
		Table:   func() routing.Table { return table },
		Nats:    requester,
		Encoder: proxy.NewDefaultEncoder(),
		Decoder: proxy.NewDefaultDecoder(),
		Timeout: routeTimeout,
		Logger:  zerolog.Nop(),
	})

	cfg := &config.Config{
		HTTPAddr:                  pickFreeAddr(t),
		MaxBodyBytes:              1 << 20,
		MaxHeaderBytes:            16384,
		ReadTimeout:               10 * time.Second,
		WriteTimeout:              35 * time.Second,
		IdleTimeout:               120 * time.Second,
		ShutdownTimeout:           5 * time.Second,
		HTTPMaxConcurrentRequests: 64,
	}

	srv, err := NewServer(cfg, handler, nil, zerolog.Nop())
	require.NoError(t, err)

	go func() { _ = srv.Run() }()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	})
	waitForListener(t, cfg.HTTPAddr)

	conn, err := net.Dial("tcp", cfg.HTTPAddr)
	require.NoError(t, err)

	_, err = conn.Write([]byte("GET /slow HTTP/1.1\r\nHost: gateway\r\n\r\n"))
	require.NoError(t, err)

	select {
	case <-requester.started:
	case <-time.After(5 * time.Second):
		t.Fatal("request never reached the NATS requester")
	}

	// The client walks away mid-flight. Nothing else in this test can
	// unblock the requester before the 30s route timeout — only the
	// disconnect-sensing path.
	require.NoError(t, conn.Close())

	select {
	case err := <-requester.unblocked:
		require.ErrorIs(t, err, context.Canceled,
			"a client disconnect must surface as ctx cancellation, not as route-timeout expiry")
	case <-time.After(5 * time.Second):
		t.Fatal("client disconnect did not cancel the in-flight request context; " +
			"the request would have pinned admission capacity for the full route timeout")
	}
}

// pickFreeAddr reserves an ephemeral loopback port and returns it as
// host:port. The listener is closed before returning, so a tiny reuse
// race exists; acceptable for a test that binds immediately after.
func pickFreeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	return addr
}

// waitForListener blocks until the server accepts TCP connections on
// addr, failing the test after a bounded number of attempts. Hertz's
// Run does not expose a ready signal, so probing the socket is the
// most direct readiness check available.
func waitForListener(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			require.NoError(t, conn.Close())

			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server never started listening on %s", addr)
}
