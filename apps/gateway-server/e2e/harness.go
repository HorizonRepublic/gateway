//go:build e2e

// Package e2e is the end-to-end harness for the gateway-server. Tests
// run under the `e2e` build tag against a live testcontainers-Compose
// stack: NATS + example-app + gateway-server. The stack is per-process
// (single instance for the whole `go test` run); tests share it.
package e2e

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go/modules/compose"
)

// composeFile is the relative path (from the e2e package directory) to
// the Compose manifest that describes the three-service stack.
const composeFile = "compose.yml"

// readyTimeout bounds how long WaitReady will poll before failing the
// test. Generous because the first run on a cold Docker buildx cache
// pulls images and rebuilds layers.
const readyTimeout = 60 * time.Second

// readyPollInterval is the cadence at which WaitReady probes the
// gateway during the initial wait.
const readyPollInterval = 250 * time.Millisecond

// stackOnce ensures a single Compose instance per `go test` process.
// Tests share the stack via the package-level helpers.
var (
	stackOnce sync.Once
	stack     *liveStack
	stackErr  error
)

// liveStack carries the testcontainers Compose handle plus the
// resolved gateway URLs and NATS host endpoint discovered after the
// stack came up. The `b` URL is the second replica added in PR 7 for
// multi-replica rate-limit tests; both replicas share NATS and the
// handler_registry KV. natsURL is consumed by NATSConn for tests that
// mutate KV directly (PR 8 onwards).
type liveStack struct {
	compose             compose.ComposeStack
	gatewayURL          string
	gatewayURLB         string
	gatewayURLRealIP    string
	gatewayURLNoTrust   string
	gatewayURLMemOpen   string
	gatewayURLConc      string
	operatorURLs        map[string]string
	natsURL             string
}

// startStack brings the Compose stack up exactly once per test process.
// Errors here are stored in stackErr; tests that call into the harness
// surface them via t.Fatal.
func startStack(ctx context.Context) (*liveStack, error) {
	c, err := compose.NewDockerCompose(composeFile)
	if err != nil {
		return nil, fmt.Errorf("compose new: %w", err)
	}

	upCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if err := c.Up(upCtx, compose.Wait(true)); err != nil {
		return nil, fmt.Errorf("compose up: %w", err)
	}

	urlA, err := resolveGatewayURL(ctx, c, "gateway-server")
	if err != nil {
		return nil, err
	}
	urlB, err := resolveGatewayURL(ctx, c, "gateway-server-b")
	if err != nil {
		return nil, err
	}
	urlRealIP, err := resolveGatewayURL(ctx, c, "gateway-server-realip")
	if err != nil {
		return nil, err
	}
	urlNoTrust, err := resolveGatewayURL(ctx, c, "gateway-server-notrust")
	if err != nil {
		return nil, err
	}
	urlMemOpen, err := resolveGatewayURL(ctx, c, "gateway-server-mem-open")
	if err != nil {
		return nil, err
	}
	urlConc, err := resolveGatewayURL(ctx, c, "gateway-server-conc")
	if err != nil {
		return nil, err
	}

	natsURL, err := resolveNATSURL(ctx, c)
	if err != nil {
		return nil, err
	}

	// Operator-listener URLs (probes) for every gateway service the
	// suite readiness-gates. Public URLs no longer answer /readyz —
	// the operator port is the only probe surface.
	operatorURLs := make(map[string]string)
	for _, svc := range []string{
		"gateway-server", "gateway-server-b", "gateway-server-realip",
		"gateway-server-notrust", "gateway-server-mem-open", "gateway-server-conc",
	} {
		u, opErr := resolveOperatorURL(ctx, c, svc)
		if opErr != nil {
			return nil, opErr
		}
		operatorURLs[svc] = u
	}

	return &liveStack{
		compose:             c,
		gatewayURL:          urlA,
		gatewayURLB:         urlB,
		gatewayURLRealIP:    urlRealIP,
		gatewayURLNoTrust:   urlNoTrust,
		gatewayURLMemOpen:   urlMemOpen,
		gatewayURLConc:      urlConc,
		operatorURLs:        operatorURLs,
		natsURL:             natsURL,
	}, nil
}

// resolveNATSURL returns the host-visible nats:// URL for the
// nats container started by the Compose stack. PR 8 reload tests use
// this to open a client connection from the test process and mutate
// the handler_registry KV bucket directly.
func resolveNATSURL(ctx context.Context, c compose.ComposeStack) (string, error) {
	n, err := c.ServiceContainer(ctx, "nats")
	if err != nil {
		return "", fmt.Errorf("resolve nats container: %w", err)
	}
	host, err := n.Host(ctx)
	if err != nil {
		return "", fmt.Errorf("nats host: %w", err)
	}
	port, err := n.MappedPort(ctx, "4222/tcp")
	if err != nil {
		return "", fmt.Errorf("nats port: %w", err)
	}
	return fmt.Sprintf("nats://%s:%s", host, port.Port()), nil
}

// resolveGatewayURL returns the host-resolved http://host:port URL for
// the named gateway-server replica. Both replicas in the e2e Compose
// expose port 8080 inside the network and a different ephemeral host
// port; testcontainers picks each independently.
func resolveGatewayURL(ctx context.Context, c compose.ComposeStack, service string) (string, error) {
	return resolveServiceURL(ctx, c, service, "8080/tcp")
}

// resolveOperatorURL maps a gateway service to its host-visible
// operator-listener URL (probes; container port 8081).
func resolveOperatorURL(ctx context.Context, c compose.ComposeStack, service string) (string, error) {
	return resolveServiceURL(ctx, c, service, "8081/tcp")
}

func resolveServiceURL(ctx context.Context, c compose.ComposeStack, service, containerPort string) (string, error) {
	gw, err := c.ServiceContainer(ctx, service)
	if err != nil {
		return "", fmt.Errorf("resolve %s container: %w", service, err)
	}
	host, err := gw.Host(ctx)
	if err != nil {
		return "", fmt.Errorf("%s host: %w", service, err)
	}
	port, err := gw.MappedPort(ctx, containerPort)
	if err != nil {
		return "", fmt.Errorf("%s port %s: %w", service, containerPort, err)
	}
	return fmt.Sprintf("http://%s:%s", host, port.Port()), nil
}

// stopStack tears the Compose stack down and removes named volumes.
// Called from TestMain's deferred cleanup; safe to call once.
func stopStack(ctx context.Context, s *liveStack) {
	if s == nil {
		return
	}
	_ = s.compose.Down(ctx, compose.RemoveOrphans(true), compose.RemoveVolumes(true))
}

// resolve returns the cached stack handle, initialising it on the
// first call. Tests use the package-level helpers below; this funnel
// keeps the once-per-process semantics in a single place.
func resolve(t *testing.T) *liveStack {
	t.Helper()
	stackOnce.Do(func() {
		stack, stackErr = startStack(context.Background())
	})
	require.NoError(t, stackErr, "compose stack failed to start")

	return stack
}

// GatewayURL returns the host-resolved URL of the primary gateway-server
// container — e.g. "http://127.0.0.1:54321". Tests use it as the base
// for every HTTP request that does not specifically need the second
// replica.
func GatewayURL(t *testing.T) string {
	return resolve(t).gatewayURL
}

// GatewayURLB returns the host-resolved URL of the second gateway-server
// replica (`gateway-server-b` in compose.yml). Both replicas share the
// same NATS connection and handler_registry bucket; tests that exercise
// multi-replica rate-limit consistency split traffic between A and B.
func GatewayURLB(t *testing.T) string {
	return resolve(t).gatewayURLB
}

// GatewayURLRealIP returns the host-resolved URL of the trusted-proxy
// replica configured with `TRUSTED_PROXY_HEADER=X-Real-IP`. PR 9 alt-
// header tests use this to exercise the single-value
// (ResolveClientIPSingle) code path.
func GatewayURLRealIP(t *testing.T) string {
	return resolve(t).gatewayURLRealIP
}

// GatewayURLNoTrust returns the host-resolved URL of the trusted-proxy
// replica configured with `TRUSTED_PROXIES=""` (operator-explicit
// "trust nothing"). PR 9 empty-trust tests use this to verify XFF
// spoofs are ignored.
func GatewayURLNoTrust(t *testing.T) string {
	return resolve(t).gatewayURLNoTrust
}

// GatewayURLMemOpen returns the host-resolved URL of the resilience
// replica with `RATELIMIT_FAIL_POLICY=open` and a tiny memory cap
// (`RATELIMIT_MEMORY_MAX_ENTRIES=2`). The saturation test drives both
// fail-policy branches against this single replica: the inherit-env
// route stays open while the per-route `failPolicy: closed` route
// short-circuits to 503.
func GatewayURLMemOpen(t *testing.T) string {
	return resolve(t).gatewayURLMemOpen
}

// GatewayURLConc returns the host-resolved URL of the resilience
// replica with `HTTP_MAX_CONCURRENT_REQUESTS=1`. PR 11 concurrency
// limit tests use this to verify the bounded-semaphore middleware
// rejects a second concurrent request with 503 + Retry-After: 1.
func GatewayURLConc(t *testing.T) string {
	return resolve(t).gatewayURLConc
}

// natsContainerStopTimeout bounds how long testcontainers waits for
// the nats process to exit gracefully before sending SIGKILL.
const natsContainerStopTimeout = 5 * time.Second

// StopNATS stops the nats Compose service so the NATS-restart test
// can observe the gateway's behaviour while the bus is unreachable.
// Pair with StartNATS to bring it back. Tests MUST poll
// WaitForGatewayHealthy before issuing further requests after a
// restart — JetStream cold-load + jetstream client reconnect can take
// several seconds.
func StopNATS(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	stack := resolve(t)
	c, err := stack.compose.ServiceContainer(ctx, "nats")
	require.NoError(t, err, "resolve nats container")
	stopCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	timeout := natsContainerStopTimeout
	require.NoError(t, c.Stop(stopCtx, &timeout), "stop nats container")
}

// StartNATS restarts the nats Compose service. Idempotent: calling on
// a running container is tolerated by testcontainers.
func StartNATS(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	stack := resolve(t)
	c, err := stack.compose.ServiceContainer(ctx, "nats")
	require.NoError(t, err, "resolve nats container")
	startCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	require.NoError(t, c.Start(startCtx), "start nats container")
}

// gatewayHealthyTimeout is the wall-clock ceiling for full
// end-to-end recovery after a NATS disruption. Generous because the
// chain is: nats container start → JetStream stream/KV reload →
// nestjs-jetstream reconnect → handler_registry republish via
// metadata heartbeat → gateway-server watcher delta → request serves.
const gatewayHealthyTimeout = 60 * time.Second

// gatewayHealthyInterval is the cadence at which WaitForGatewayHealthy
// polls a /users/alice probe. Wide enough to avoid hammering during a
// reconnect storm; narrow enough that recovery is observed promptly.
const gatewayHealthyInterval = 250 * time.Millisecond

// WaitForGatewayHealthy polls `GET /users/alice` against the supplied
// gateway URL until it returns 200 or `gatewayHealthyTimeout`
// elapses. Used by the NATS-restart test to gate downstream work on
// full reconnect, since /readyz only proves the local HTTP listener
// is up — it does NOT prove the upstream NATS request path works.
func WaitForGatewayHealthy(t *testing.T, baseURL string) {
	t.Helper()
	deadline := time.Now().Add(gatewayHealthyTimeout)
	client := &http.Client{Timeout: 3 * time.Second}
	url := baseURL + "/users/alice"

	var lastStatus int
	var lastErr error
	for {
		resp, err := client.Get(url)
		if err == nil {
			lastStatus = resp.StatusCode
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			t.Fatalf("WaitForGatewayHealthy(%s): last status=%d, last err=%v after %s",
				baseURL, lastStatus, lastErr, gatewayHealthyTimeout)
		}
		time.Sleep(gatewayHealthyInterval)
	}
}

// WaitReady blocks until the primary gateway accepts a GET on /readyz
// and returns 200. Times out after readyTimeout.
func WaitReady(t *testing.T) {
	waitReadyAt(t, OperatorURL(t, "gateway-server"))
}

// WaitReadyB blocks until the second gateway replica accepts a GET on
// /readyz and returns 200. Multi-replica tests call WaitReadyB after
// WaitReady so both replicas are confirmed live before traffic splits.
func WaitReadyB(t *testing.T) {
	waitReadyAt(t, OperatorURL(t, "gateway-server-b"))
}

// WaitReadyAt polls /readyz on the supplied base URL until it returns
// 200 or the readyTimeout elapses. Exported so trustedproxy tests can
// gate on the realip / notrust replicas without growing one
// WaitReadyX helper per replica.
func WaitReadyAt(t *testing.T, baseURL string) { waitReadyAt(t, baseURL) }

// OperatorURL returns the host-resolved operator-listener URL
// (probes) for the named gateway compose service.
func OperatorURL(t *testing.T, service string) string {
	t.Helper()
	u, ok := resolve(t).operatorURLs[service]
	if !ok {
		t.Fatalf("no operator URL resolved for service %q", service)
	}
	return u
}

// waitReadyAt polls /readyz on the supplied base URL until it returns
// 200 or the readyTimeout elapses.
func waitReadyAt(t *testing.T, baseURL string) {
	t.Helper()
	url := baseURL + "/readyz"
	deadline := time.Now().Add(readyTimeout)
	client := &http.Client{Timeout: 2 * time.Second}

	var lastErr error
	for {
		resp, err := client.Get(url)
		if err == nil {
			status := resp.StatusCode
			_ = resp.Body.Close()
			if status == http.StatusOK {
				return
			}
			lastErr = fmt.Errorf("/readyz returned %d", status)
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s /readyz did not return 200 within %s (last err: %v)", baseURL, readyTimeout, lastErr)
		}
		time.Sleep(readyPollInterval)
	}
}

// ExampleAppHealthURL returns the host-resolved URL for example-app's
// Fastify health listener (port 3001 inside the Compose network).
// Tests use it to call POST /__e2e/reset directly between scenarios
// that mutate state.
func ExampleAppHealthURL(t *testing.T) string {
	t.Helper()
	s := resolve(t)
	ctx := context.Background()
	c, err := s.compose.ServiceContainer(ctx, "example-app")
	require.NoError(t, err)
	host, err := c.Host(ctx)
	require.NoError(t, err)
	port, err := c.MappedPort(ctx, "3001/tcp")
	require.NoError(t, err)
	return fmt.Sprintf("http://%s:%s", host, port.Port())
}

// exampleAppHealthURL is the lower-case alias used by the test files
// in this package.
func exampleAppHealthURL(t *testing.T) string {
	return ExampleAppHealthURL(t)
}

// natsCacheOnce caches the per-process NATS connection that
// PR 8 reload tests use to mutate the handler_registry KV bucket.
// Tests share one connection so we don't spawn a fresh client per
// test run.
var (
	natsCacheOnce sync.Once
	natsCacheConn *nats.Conn
	natsCacheErr  error
)

// NATSConn returns a cached NATS connection to the Compose stack's
// nats container. Tests that mutate the handler_registry KV bucket
// connect through this. The connection lives for the entire `go test`
// process — there is no Close hook because Compose tear-down at
// TestMain end terminates the server end of the connection.
func NATSConn(t *testing.T) *nats.Conn {
	t.Helper()
	url := resolve(t).natsURL
	natsCacheOnce.Do(func() {
		natsCacheConn, natsCacheErr = nats.Connect(url, nats.Name("e2e-test-runner"))
	})
	require.NoError(t, natsCacheErr, "connect to compose-resolved NATS")
	return natsCacheConn
}

// HandlerBucket returns a typed JetStream KV handle on the
// handler_registry bucket. Reload tests PUT/DELETE entries through it
// to drive the gateway's watcher.
func HandlerBucket(t *testing.T) jetstream.KeyValue {
	t.Helper()
	conn := NATSConn(t)
	js, err := jetstream.New(conn)
	require.NoError(t, err, "open JetStream context")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	bucket, err := js.KeyValue(ctx, "handler_registry")
	require.NoError(t, err, "open handler_registry KV bucket")
	return bucket
}

// routeReloadDeadline bounds how long WaitForRoute polls for a routing
// table delta to surface. The watcher reacts within tens of
// milliseconds in steady state; 3 seconds is generous enough to cover
// CI jitter without letting a stuck delta hang a test.
const routeReloadDeadline = 3 * time.Second

// routeReloadInterval is the cadence at which WaitForRoute probes the
// gateway HTTP surface while waiting for a delta to land.
const routeReloadInterval = 50 * time.Millisecond

// WaitForRoute polls `<method> <path>` against the primary gateway
// until the response status equals expectStatus, or routeReloadDeadline
// elapses. Returns the matching response so the caller can read body
// or headers if needed. The poll loop runs on its own request — the
// caller gets a fresh body it owns.
//
// Use this after every KV mutation (PUT/DELETE) to wait for the
// watcher to propagate the delta into the routing table. Asserting
// the response status before the watcher has reacted would race.
func WaitForRoute(t *testing.T, method, path string, expectStatus int) *http.Response {
	t.Helper()
	return WaitForRouteAt(t, GatewayURL(t), method, path, expectStatus)
}

// WaitForRouteAt is WaitForRoute against an explicit replica base URL.
// The multi-replica reload tests use it to pin that every KV delta
// lands on EVERY replica's watcher, not just the primary's.
func WaitForRouteAt(t *testing.T, baseURL, method, path string, expectStatus int) *http.Response {
	t.Helper()
	url := baseURL + path
	deadline := time.Now().Add(routeReloadDeadline)
	client := &http.Client{Timeout: 2 * time.Second}

	var lastStatus int
	for {
		req, err := http.NewRequest(method, url, nil)
		require.NoError(t, err)
		resp, err := client.Do(req)
		if err == nil {
			lastStatus = resp.StatusCode
			if resp.StatusCode == expectStatus {
				return resp
			}
			_ = resp.Body.Close()
		}
		if time.Now().After(deadline) {
			t.Fatalf("WaitForRouteAt(%s, %s %s, %d): last status %d after %s",
				baseURL, method, path, expectStatus, lastStatus, routeReloadDeadline)
		}
		time.Sleep(routeReloadInterval)
	}
}
