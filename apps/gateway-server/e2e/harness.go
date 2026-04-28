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
// resolved gateway URLs discovered after the stack came up. The
// `b` URL is the second replica added in PR 7 for multi-replica
// rate-limit tests; both replicas share NATS and the handler_registry
// KV.
type liveStack struct {
	compose     compose.ComposeStack
	gatewayURL  string
	gatewayURLB string
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

	return &liveStack{
		compose:     c,
		gatewayURL:  urlA,
		gatewayURLB: urlB,
	}, nil
}

// resolveGatewayURL returns the host-resolved http://host:port URL for
// the named gateway-server replica. Both replicas in the e2e Compose
// expose port 8080 inside the network and a different ephemeral host
// port; testcontainers picks each independently.
func resolveGatewayURL(ctx context.Context, c compose.ComposeStack, service string) (string, error) {
	gw, err := c.ServiceContainer(ctx, service)
	if err != nil {
		return "", fmt.Errorf("resolve %s container: %w", service, err)
	}
	host, err := gw.Host(ctx)
	if err != nil {
		return "", fmt.Errorf("%s host: %w", service, err)
	}
	port, err := gw.MappedPort(ctx, "8080/tcp")
	if err != nil {
		return "", fmt.Errorf("%s port: %w", service, err)
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

// WaitReady blocks until the primary gateway accepts a GET on /readyz
// and returns 200. Times out after readyTimeout.
func WaitReady(t *testing.T) {
	waitReadyAt(t, GatewayURL(t))
}

// WaitReadyB blocks until the second gateway replica accepts a GET on
// /readyz and returns 200. Multi-replica tests call WaitReadyB after
// WaitReady so both replicas are confirmed live before traffic splits.
func WaitReadyB(t *testing.T) {
	waitReadyAt(t, GatewayURLB(t))
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
