//go:build integration

package http

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/config"
	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/observability"
	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/registry"
	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/routing"
)

// startOperatorWithRoutes boots an operator listener whose admin
// route-dump surface is wired to the given static routing table.
func startOperatorWithRoutes(t *testing.T, table routing.Table) (string, func()) {
	t.Helper()

	cfg := &config.Config{OperatorHTTPAddr: "127.0.0.1:0"}
	srv := NewOperatorServer(cfg, ReadinessFunc(func() bool { return true }),
		observability.NewMetrics().Handler(),
		func() routing.Table { return table })

	runErr := make(chan error, 1)
	go func() { runErr <- srv.Run() }()

	require.Eventually(t, func() bool { return srv.Addr() != "" },
		2*time.Second, 5*time.Millisecond, "operator listener must bind")

	shutdown := func() {
		require.NoError(t, srv.Shutdown(t.Context()))
		require.NoError(t, <-runErr)
	}

	return "http://" + srv.Addr(), shutdown
}

// TestIntegration_AdminRouteDumpReflectsTable pins the operator admin
// route-dump: it serves the live routing table as JSON, projects the
// auth contract, and never leaks static header values (keys only).
func TestIntegration_AdminRouteDumpReflectsTable(t *testing.T) {
	table := routing.BuildTableFromRoutes([]routing.Route{
		{
			Method: "GET", PathTemplate: "/users/:id", Subject: "users__microservice.cmd.get",
			Auth:      &routing.RouteAuth{VerifierSubject: "users__microservice.verify", Optional: false},
			RateLimit: &registry.RateLimitMeta{RPS: 10, Burst: 5},
			Headers:   map[string]string{"X-Internal-Token": "s3cret"},
			Timeout:   2 * time.Second,
		},
		{
			Method: "POST", PathTemplate: "/health", Subject: "health__microservice.cmd.ping",
		},
	})

	base, shutdown := startOperatorWithRoutes(t, table)
	defer shutdown()

	resp, err := http.Get(base + "/admin/routes")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/json; charset=utf-8", resp.Header.Get("Content-Type"))
	assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"))

	var dump routeDumpResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&dump))

	require.Equal(t, 2, dump.Count)
	require.Len(t, dump.Routes, 2)

	// Deterministic ordering: GET/users before POST/health.
	first := dump.Routes[0]
	assert.Equal(t, "GET", first.Method)
	assert.Equal(t, "/users/:id", first.Path)
	assert.Equal(t, "users__microservice.cmd.get", first.Subject)
	require.NotNil(t, first.Auth)
	assert.True(t, first.Auth.Required)
	assert.False(t, first.Auth.Optional)
	assert.Equal(t, "users__microservice.verify", first.Auth.VerifierSubject)
	assert.NotNil(t, first.RateLimit)
	assert.Equal(t, int64(2000), first.TimeoutMs)

	// Static header VALUES must never appear on the wire — keys only.
	assert.Equal(t, []string{"X-Internal-Token"}, first.HeaderKeys)

	second := dump.Routes[1]
	assert.Equal(t, "POST", second.Method)
	assert.Nil(t, second.Auth, "public route carries no auth block")
}

// TestIntegration_AdminRouteDumpNilTableIsEmpty pins the cold-boot
// contract: before the first snapshot lands the dump is an empty
// table, not a 500.
func TestIntegration_AdminRouteDumpNilTableIsEmpty(t *testing.T) {
	base, shutdown := startOperatorWithRoutes(t, nil)
	defer shutdown()

	resp, err := http.Get(base + "/admin/routes")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var dump routeDumpResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&dump))
	assert.Equal(t, 0, dump.Count)
}
