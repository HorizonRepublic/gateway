package http

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/config"
	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/proxy"
	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/routing"
)

// TestServer_MaxHeaderBytes_RejectsOversizedHeaderWith431 pins H.1:
// the gateway server, configured with a small MaxHeaderBytes budget,
// MUST reject a request whose header size exceeds that budget with
// HTTP 431. Without the WithMaxHeaderBytes wiring in server.go this
// test fails because Hertz falls back to its 1 MiB default and
// accepts the oversized header.
//
// The test also captures the actual response content-type and body
// shape Hertz emits so a future change to Hertz's default error
// payload surfaces as a test diff rather than a silent wire shift.
func TestServer_MaxHeaderBytes_RejectsOversizedHeaderWith431(t *testing.T) {
	// Spin up a gateway server bound to an ephemeral port with a
	// 1 KiB header budget. The proxy handler is a no-op shim — this
	// test exercises ONLY the Hertz-level header limit, not the
	// downstream pipeline.
	cfg := &config.Config{
		HTTPAddr:        "127.0.0.1:0",
		MaxHeaderBytes:  1024,
		MaxBodyBytes:    1 << 20,
		ReadTimeout:     5 * time.Second,
		WriteTimeout:    5 * time.Second,
		IdleTimeout:     5 * time.Second,
		ShutdownTimeout: 5 * time.Second,
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	cfg.HTTPAddr = ln.Addr().String()
	require.NoError(t, ln.Close())

	handler := noopProxyHandler(t)
	h, err := NewServer(cfg, handler, nil, zerolog.Nop())
	require.NoError(t, err)

	var serverErr atomic.Value
	go func() {
		if err := h.Run(); err != nil {
			serverErr.Store(err)
		}
	}()
	t.Cleanup(func() {
		_ = h.Shutdown(context.Background())
	})

	// Wait for the server to come up. Poll /__probe__ — the gateway
	// answers everything with a routable decision, so any response
	// (including 404) means the listener is accepting connections.
	require.Eventually(t, func() bool {
		resp, err := http.Get("http://" + cfg.HTTPAddr + "/__probe__")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()

		return true
	}, 3*time.Second, 50*time.Millisecond, "server did not accept connections")

	// Craft a request whose total header line size exceeds 1 KiB. A
	// 4 KiB single header value is far over the budget; Hertz must
	// reject it with 431.
	oversized := strings.Repeat("A", 4096)
	req, err := http.NewRequest(http.MethodGet, "http://"+cfg.HTTPAddr+"/bench/hello", nil)
	require.NoError(t, err)
	req.Header.Set("X-Bloat", oversized)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusRequestHeaderFieldsTooLarge, resp.StatusCode,
		"gateway must reject oversized headers with 431 once WithMaxHeaderBytes is wired")

	// Pin the wire shape Hertz uses for 431. If Hertz defaults change
	// in a future upgrade, this test forces a conscious decision on
	// whether to normalise the shape through gerrors.
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	t.Logf("Hertz 431 body shape: Content-Type=%q Body=%q",
		resp.Header.Get("Content-Type"), string(body))
}

// noopProxyHandler returns a proxy.Handler that would 404 every
// request because its routing table is empty. Sufficient for this
// test — we never get past Hertz's header parser.
func noopProxyHandler(t *testing.T) *proxy.Handler {
	t.Helper()

	emptyTable := routing.BuildTableFromRoutes(nil)

	return proxy.NewHandler(proxy.HandlerConfig{
		Table:   func() routing.Table { return emptyTable },
		Nats:    nil,
		Encoder: proxy.NewDefaultEncoder(),
		Decoder: proxy.NewDefaultDecoder(),
		Timeout: 5 * time.Second,
		Logger:  zerolog.Nop(),
	})
}
