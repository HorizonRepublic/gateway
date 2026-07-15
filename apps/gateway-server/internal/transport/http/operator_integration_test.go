//go:build integration

package http

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/config"
	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/observability"
	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/proxy"
	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/routing"
)

// operatorTable is a single-route routing.Table fixture for driving
// real requests through the proxy handler in front of the metrics
// registry.
type operatorTable struct{ route routing.Route }

func (t operatorTable) Lookup(method, path string) (routing.Route, map[string]string, bool) {
	if method == t.route.Method && path == t.route.PathTemplate {
		return t.route, map[string]string{}, true
	}

	return routing.Route{}, nil, false
}

func (operatorTable) Methods(string) []string { return nil }

// operatorFakeNats replies with a canned success envelope for every
// subject — enough to complete the proxy round trip that feeds the
// RED metrics.
type operatorFakeNats struct{}

func (operatorFakeNats) Request(context.Context, string, []byte, time.Duration) ([]byte, error) {
	return []byte(`{"status":200,"headers":{},"body":{"ok":true}}`), nil
}

// startOperator boots an OperatorServer on an ephemeral port and
// returns its base URL plus a shutdown func the test MUST call.
func startOperator(t *testing.T, metrics *observability.Metrics) (string, func()) {
	t.Helper()

	cfg := &config.Config{OperatorHTTPAddr: "127.0.0.1:0"}
	srv := NewOperatorServer(cfg, ReadinessFunc(func() bool { return true }), metrics.Handler())

	runErr := make(chan error, 1)
	go func() { runErr <- srv.Run() }()

	require.Eventually(t, func() bool { return srv.Addr() != "" },
		2*time.Second, 5*time.Millisecond, "operator listener must bind")

	base := "http://" + srv.Addr()
	shutdown := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, srv.Shutdown(ctx))
		require.NoError(t, <-runErr, "Shutdown-driven exit must return nil from Run")
	}

	return base, shutdown
}

func operatorGET(t *testing.T, url string) (int, string) {
	t.Helper()

	resp, err := http.Get(url)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	return resp.StatusCode, string(body)
}

// TestIntegration_OperatorListenerServesMetricsAfterTraffic drives
// real requests through the proxy handler (route hit and route miss)
// and asserts the operator listener's /metrics endpoint exposes the
// resulting RED series alongside the runtime collectors and the
// registry reload pair.
func TestIntegration_OperatorListenerServesMetricsAfterTraffic(t *testing.T) {
	metrics := observability.NewMetrics()
	metrics.RecordRegistryReload()

	handler := proxy.NewHandler(proxy.HandlerConfig{
		Table: func() routing.Table {
			return operatorTable{route: routing.Route{
				Method: "GET", PathTemplate: "/orders/:id", Subject: "svc.cmd.orders.get",
			}}
		},
		Nats:    operatorFakeNats{},
		Encoder: proxy.NewDefaultEncoder(),
		Decoder: proxy.NewDefaultDecoder(),
		Timeout: time.Second,
		Logger:  zerolog.Nop(),
		Metrics: metrics,
	})

	for i := 0; i < 5; i++ {
		result := handler.Handle(context.Background(), &proxy.ServeInput{
			Method: "GET", Path: "/orders/:id",
			Query: map[string]proxy.QueryValue{}, Headers: map[string]string{},
			RequestID: fmt.Sprintf("r%d", i),
		})
		require.Equal(t, 200, result.Status)
	}
	miss := handler.Handle(context.Background(), &proxy.ServeInput{
		Method: "GET", Path: "/nope",
		Query: map[string]proxy.QueryValue{}, Headers: map[string]string{},
		RequestID: "r-miss",
	})
	require.Equal(t, 404, miss.Status)

	base, shutdown := startOperator(t, metrics)
	defer shutdown()

	status, body := operatorGET(t, base+"/metrics")
	require.Equal(t, http.StatusOK, status)

	assert.Contains(t, body,
		`gateway_http_requests_total{method="GET",route="/orders/:id",status="2xx"} 5`)
	assert.Contains(t, body,
		`gateway_http_requests_total{method="GET",route="unmatched",status="4xx"} 1`)
	assert.Contains(t, body,
		`gateway_http_request_duration_seconds_count{method="GET",route="/orders/:id",status="2xx"} 5`)
	assert.Contains(t, body, "gateway_http_inflight_requests 0")
	assert.Contains(t, body, "gateway_registry_reloads_total 1")
	assert.Contains(t, body, "go_goroutines",
		"Go runtime collector must be exported")
}

// TestIntegration_OperatorListenerServesPprofAndProbes pins the debug
// surface on the operator socket: the pprof index and a concrete
// profile respond, and the probe endpoints keep working next to them.
func TestIntegration_OperatorListenerServesPprofAndProbes(t *testing.T) {
	base, shutdown := startOperator(t, observability.NewMetrics())
	defer shutdown()

	status, body := operatorGET(t, base+"/debug/pprof/")
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, body, "goroutine", "pprof index must list the goroutine profile")

	status, body = operatorGET(t, base+"/debug/pprof/goroutine?debug=1")
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, body, "goroutine profile")

	status, _ = operatorGET(t, base+"/healthz")
	assert.Equal(t, http.StatusOK, status)
	status, _ = operatorGET(t, base+"/readyz")
	assert.Equal(t, http.StatusOK, status)
}

// TestIntegration_OperatorListenerShutdownLeaksNoGoroutines boots and
// drains the operator listener and verifies the goroutine count
// settles back to its starting level — the serve loop, the connection
// goroutines, and the metrics handler must all unwind on Shutdown.
func TestIntegration_OperatorListenerShutdownLeaksNoGoroutines(t *testing.T) {
	before := runtime.NumGoroutine()

	base, shutdown := startOperator(t, observability.NewMetrics())

	// A keep-alive connection keeps a persistConn goroutine pair alive
	// on the client AND a conn.serve goroutine on the server until the
	// idle connection is actually torn down, whose timing is OS-
	// scheduler-dependent (settles fast on the Linux CI runner, lags
	// several seconds on macOS) and read as a phantom leak. Disabling
	// keep-alive closes the connection the moment the response is read,
	// so both goroutines unwind synchronously with Shutdown and the
	// assertion measures a real leak, not client-pool latency.
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Get(base + "/metrics")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_ = resp.Body.Close()

	shutdown()
	client.CloseIdleConnections()

	// Poll with a plain loop, NOT assert.Eventually: testify runs its
	// condition in a spawned goroutine, which the count inside the
	// condition would include — the measurement would then race its
	// own harness and never settle to the pre-boot level.
	settled := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if runtime.NumGoroutine() <= before {
			settled = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	assert.True(t, settled,
		"goroutine count must settle to the pre-boot level after Shutdown")
}
