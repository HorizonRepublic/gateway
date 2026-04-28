package routing

import (
	"io"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/auth"
	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/registry"
)

func silentLogger() zerolog.Logger {
	return zerolog.New(io.Discard).Level(zerolog.Disabled)
}

func emptyVerifiers() *auth.VerifierRegistry {
	return auth.BuildVerifierRegistry(
		&registry.Snapshot{Entries: map[string]registry.HandlerEntry{}},
		silentLogger(),
	)
}

func TestBuildTable_SkipsEntriesWithoutHTTP(t *testing.T) {
	snapshot := &registry.Snapshot{
		Entries: map[string]registry.HandlerEntry{
			"users-svc.cmd.users.internal": {HTTP: nil},
		},
	}

	table := BuildTableFromRoutes(CollectRoutes(snapshot, emptyVerifiers(), silentLogger()))
	_, _, ok := table.Lookup("GET", "/users")
	assert.False(t, ok)
}

func TestBuildTable_IncludesHTTPEntries(t *testing.T) {
	snapshot := &registry.Snapshot{
		Entries: map[string]registry.HandlerEntry{
			"users-svc.cmd.users.list": {
				HTTP: &registry.HTTPMeta{Method: "GET", Path: "/users"},
			},
			"users-svc.cmd.users.create": {
				HTTP: &registry.HTTPMeta{Method: "POST", Path: "/users"},
			},
		},
	}

	table := BuildTableFromRoutes(CollectRoutes(snapshot, emptyVerifiers(), silentLogger()))

	listRoute, _, ok := table.Lookup("GET", "/users")
	assert.True(t, ok)
	assert.Equal(t, "users-svc__microservice.cmd.users.list", listRoute.Subject)

	createRoute, _, ok := table.Lookup("POST", "/users")
	assert.True(t, ok)
	assert.Equal(t, "users-svc__microservice.cmd.users.create", createRoute.Subject)
}

func TestBuildTable_SkipsMalformedKeys(t *testing.T) {
	// The "broken-key" entry has no ".cmd." infix, so SubjectFromKey
	// returns an error. The builder must log-and-skip it rather than
	// failing the whole rebuild — a single bad KV entry should never
	// take the gateway offline.
	snapshot := &registry.Snapshot{
		Entries: map[string]registry.HandlerEntry{
			"broken-key": {
				HTTP: &registry.HTTPMeta{Method: "GET", Path: "/broken"},
			},
			"users-svc.cmd.users.list": {
				HTTP: &registry.HTTPMeta{Method: "GET", Path: "/users"},
			},
		},
	}

	table := BuildTableFromRoutes(CollectRoutes(snapshot, emptyVerifiers(), silentLogger()))

	// The malformed entry is absent.
	_, _, ok := table.Lookup("GET", "/broken")
	assert.False(t, ok)

	// The well-formed entry survived the skip.
	route, _, ok := table.Lookup("GET", "/users")
	assert.True(t, ok)
	assert.Equal(t, "users-svc__microservice.cmd.users.list", route.Subject)
}

func TestBuildTable_EmptySnapshot(t *testing.T) {
	// Empty snapshot is a real production state during warmup: the
	// gateway is up, the watcher has not yet received any KV events.
	// BuildTable must return a non-nil Table whose Lookup uniformly
	// returns ok=false rather than panicking on a nil map.
	snapshot := &registry.Snapshot{Entries: map[string]registry.HandlerEntry{}}

	table := BuildTableFromRoutes(CollectRoutes(snapshot, emptyVerifiers(), silentLogger()))
	assert.NotNil(t, table)

	_, _, ok := table.Lookup("GET", "/anything")
	assert.False(t, ok)
	assert.Empty(t, table.Methods("/anything"))
}

func TestCollectRoutes_ResolvesExplicitVerifier(t *testing.T) {
	snapshot := &registry.Snapshot{
		Entries: map[string]registry.HandlerEntry{
			"users-svc.cmd.users.me": {
				HTTP: &registry.HTTPMeta{Method: "GET", Path: "/users/me"},
				Auth: &registry.RouteAuthMeta{Verifier: "jwt"},
			},
			"users-svc.cmd.auth.verifier.jwt": {
				Verifier: &registry.VerifierMeta{ID: "jwt", Default: true},
			},
		},
	}
	verifiers := auth.BuildVerifierRegistry(snapshot, silentLogger())

	routes := CollectRoutes(snapshot, verifiers, silentLogger())

	require.Len(t, routes, 1)
	require.NotNil(t, routes[0].Auth)
	assert.Equal(t, "users-svc__microservice.cmd.auth.verifier.jwt", routes[0].Auth.VerifierSubject)
	assert.False(t, routes[0].Auth.Optional)
}

func TestCollectRoutes_UsesDefaultVerifierWhenRouteOmitsId(t *testing.T) {
	snapshot := &registry.Snapshot{
		Entries: map[string]registry.HandlerEntry{
			"users-svc.cmd.users.me": {
				HTTP: &registry.HTTPMeta{Method: "GET", Path: "/users/me"},
				Auth: &registry.RouteAuthMeta{}, // empty Verifier → default
			},
			"users-svc.cmd.auth.verifier.jwt": {
				Verifier: &registry.VerifierMeta{ID: "jwt", Default: true},
			},
		},
	}
	verifiers := auth.BuildVerifierRegistry(snapshot, silentLogger())

	routes := CollectRoutes(snapshot, verifiers, silentLogger())

	require.Len(t, routes, 1)
	require.NotNil(t, routes[0].Auth)
	assert.Equal(t, "users-svc__microservice.cmd.auth.verifier.jwt", routes[0].Auth.VerifierSubject)
}

func TestCollectRoutes_OptionalAuthPreservesFlag(t *testing.T) {
	snapshot := &registry.Snapshot{
		Entries: map[string]registry.HandlerEntry{
			"users-svc.cmd.articles.get": {
				HTTP: &registry.HTTPMeta{Method: "GET", Path: "/articles/:id"},
				Auth: &registry.RouteAuthMeta{Verifier: "jwt", Optional: true},
			},
			"users-svc.cmd.auth.verifier.jwt": {
				Verifier: &registry.VerifierMeta{ID: "jwt"},
			},
		},
	}
	verifiers := auth.BuildVerifierRegistry(snapshot, silentLogger())

	routes := CollectRoutes(snapshot, verifiers, silentLogger())

	require.Len(t, routes, 1)
	require.NotNil(t, routes[0].Auth)
	assert.True(t, routes[0].Auth.Optional)
}

func TestCollectRoutes_DropsRouteWithUnknownVerifier(t *testing.T) {
	// Route references verifier 'jwt' but no such verifier is
	// registered. The route must be excluded from the routing table;
	// matching HTTP requests return 404 until the verifier registers.
	snapshot := &registry.Snapshot{
		Entries: map[string]registry.HandlerEntry{
			"users-svc.cmd.users.me": {
				HTTP: &registry.HTTPMeta{Method: "GET", Path: "/users/me"},
				Auth: &registry.RouteAuthMeta{Verifier: "jwt"},
			},
		},
	}
	verifiers := auth.BuildVerifierRegistry(snapshot, silentLogger())

	routes := CollectRoutes(snapshot, verifiers, silentLogger())

	assert.Empty(t, routes)
}

func TestCollectRoutes_DropsRouteWithImplicitDefaultWhenNoDefaultRegistered(t *testing.T) {
	snapshot := &registry.Snapshot{
		Entries: map[string]registry.HandlerEntry{
			"users-svc.cmd.users.me": {
				HTTP: &registry.HTTPMeta{Method: "GET", Path: "/users/me"},
				Auth: &registry.RouteAuthMeta{}, // implicit default
			},
			// Verifier exists but does NOT set Default:true.
			"users-svc.cmd.auth.verifier.jwt": {
				Verifier: &registry.VerifierMeta{ID: "jwt"},
			},
		},
	}
	verifiers := auth.BuildVerifierRegistry(snapshot, silentLogger())

	routes := CollectRoutes(snapshot, verifiers, silentLogger())

	assert.Empty(t, routes)
}

func TestCollectRoutes_PublicRouteUnaffectedByVerifierRegistry(t *testing.T) {
	// Regression: routes without an Auth block must still land in the
	// table even when the verifier registry is empty.
	snapshot := &registry.Snapshot{
		Entries: map[string]registry.HandlerEntry{
			"users-svc.cmd.healthcheck": {
				HTTP: &registry.HTTPMeta{Method: "GET", Path: "/health"},
			},
		},
	}
	verifiers := auth.BuildVerifierRegistry(snapshot, silentLogger())

	routes := CollectRoutes(snapshot, verifiers, silentLogger())

	require.Len(t, routes, 1)
	assert.Nil(t, routes[0].Auth)
}

func TestCollectRoutes_DropsCORSWithWildcardOriginAndCredentials(t *testing.T) {
	// Defense-in-depth: SDK rejects this at registration time, but the
	// Go side still guards against malformed KV entries reaching the
	// routing table. When origins:[*] and credentials:true coexist we
	// drop the CORS block entirely — the route remains registered but
	// serves no CORS headers (no-CORS beats broken-CORS).
	snapshot := &registry.Snapshot{
		Entries: map[string]registry.HandlerEntry{
			"svc__microservice.cmd.users.list": {
				HTTP: &registry.HTTPMeta{Method: "GET", Path: "/users"},
				CORS: &registry.CORSMeta{
					Origins:     []string{"*"},
					Credentials: true,
				},
			},
		},
	}

	routes := CollectRoutes(snapshot, emptyVerifiers(), silentLogger())

	require.Len(t, routes, 1)
	assert.Nil(t, routes[0].CORS, "invalid CORS block must be dropped to avoid broken-CORS semantics")
}

func TestCollectRoutes_KeepsCORSWithWildcardOriginWithoutCredentials(t *testing.T) {
	snapshot := &registry.Snapshot{
		Entries: map[string]registry.HandlerEntry{
			"svc__microservice.cmd.users.list": {
				HTTP: &registry.HTTPMeta{Method: "GET", Path: "/users"},
				CORS: &registry.CORSMeta{Origins: []string{"*"}},
			},
		},
	}

	routes := CollectRoutes(snapshot, emptyVerifiers(), silentLogger())

	require.Len(t, routes, 1)
	require.NotNil(t, routes[0].CORS)
	assert.Equal(t, []string{"*"}, routes[0].CORS.Origins)
}

func TestCollectRoutes_PropagatesExtendedFields(t *testing.T) {
	timeout := 5000
	snapshot := &registry.Snapshot{
		Entries: map[string]registry.HandlerEntry{
			"svc__microservice.cmd.users.create": {
				HTTP:      &registry.HTTPMeta{Method: "POST", Path: "/users"},
				CORS:      &registry.CORSMeta{Origins: []string{"https://app.com"}, Credentials: true},
				RateLimit: &registry.RateLimitMeta{RPS: 10, Burst: 20, KeyBy: []string{"ip"}},
				Headers:   map[string]string{"x-frame-options": "DENY"},
				Timeout:   &timeout,
			},
		},
	}

	routes := CollectRoutes(snapshot, emptyVerifiers(), silentLogger())

	require.Len(t, routes, 1)
	route := routes[0]

	require.NotNil(t, route.CORS)
	assert.Equal(t, []string{"https://app.com"}, route.CORS.Origins)
	assert.True(t, route.CORS.Credentials)

	require.NotNil(t, route.RateLimit)
	assert.Equal(t, 10, route.RateLimit.RPS)

	assert.Equal(t, "DENY", route.Headers["x-frame-options"])
	assert.Equal(t, 5*time.Second, route.Timeout)
}

func TestCollectRoutes_DropsRateLimitWithNonPositiveRPS(t *testing.T) {
	cases := []struct {
		name string
		rps  int
	}{
		{"zero", 0},
		{"negative", -5},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := &registry.Snapshot{
				Entries: map[string]registry.HandlerEntry{
					"svc.cmd.users.list": {
						HTTP:      &registry.HTTPMeta{Method: "GET", Path: "/users"},
						RateLimit: &registry.RateLimitMeta{RPS: tc.rps, Burst: 20},
					},
				},
			}

			routes := CollectRoutes(snapshot, emptyVerifiers(), silentLogger())

			require.Len(t, routes, 1)
			assert.Nil(t, routes[0].RateLimit,
				"routing builder must drop a non-positive rps block so operators get a WARN instead of silent no-op")
		})
	}
}

// TestCollectRoutes_DropsHeaderWithCRLF pins the header-injection
// guard: a registry-supplied response-header value containing CR/LF
// is dropped silently from the route's Headers map. The route itself
// survives so the rest of the application traffic continues to flow
// — partial coverage beats taking the route offline because of one
// malformed header.
func TestCollectRoutes_DropsHeaderWithCRLF(t *testing.T) {
	cases := []struct {
		name    string
		header  string
		value   string
		dropped string
	}{
		{
			name:    "CRLF in value injects Set-Cookie",
			header:  "x-frame-options",
			value:   "DENY\r\nSet-Cookie: stolen=1",
			dropped: "x-frame-options",
		},
		{
			name:    "LF-only in value still injects",
			header:  "x-content-type-options",
			value:   "nosniff\nSet-Cookie: stolen=1",
			dropped: "x-content-type-options",
		},
		{
			name:    "NUL in value still poisons the line",
			header:  "x-frame-options",
			value:   "DENY\x00",
			dropped: "x-frame-options",
		},
		{
			name:    "CRLF in name terminates the previous header",
			header:  "x-bad\r\nSet-Cookie",
			value:   "DENY",
			dropped: "x-bad\r\nSet-Cookie",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := &registry.Snapshot{
				Entries: map[string]registry.HandlerEntry{
					"svc.cmd.users.list": {
						HTTP: &registry.HTTPMeta{Method: "GET", Path: "/users"},
						Headers: map[string]string{
							"x-content-type-options": "nosniff",
							tc.header:                tc.value,
						},
					},
				},
			}

			routes := CollectRoutes(snapshot, emptyVerifiers(), silentLogger())

			require.Len(t, routes, 1)
			route := routes[0]
			_, hasInjected := route.Headers[tc.dropped]
			assert.False(t, hasInjected,
				"CRLF/NUL header %q must be dropped", tc.dropped)
			// Clean siblings on the same route stay intact unless the
			// malformed header IS the clean sibling key (when the name
			// itself contains CRLF, the only other key is preserved).
			if tc.dropped != "x-content-type-options" {
				assert.Equal(t, "nosniff", route.Headers["x-content-type-options"],
					"clean siblings must survive the per-entry drop")
			}
		})
	}
}

// TestCollectRoutes_HeadersAllInjectedReturnsNil pins the all-bad
// edge case: when every Headers entry carries CRLF/NUL, the function
// returns nil rather than an empty map. Downstream consumers treat
// nil and {} equivalently, but nil avoids a gratuitous allocation on
// every reload tick.
func TestCollectRoutes_HeadersAllInjectedReturnsNil(t *testing.T) {
	snapshot := &registry.Snapshot{
		Entries: map[string]registry.HandlerEntry{
			"svc.cmd.users.list": {
				HTTP: &registry.HTTPMeta{Method: "GET", Path: "/users"},
				Headers: map[string]string{
					"x-bad-1": "value\r\nset-cookie: hijack=1",
					"x-bad-2": "value\nx-poison: 1",
				},
			},
		},
	}

	routes := CollectRoutes(snapshot, emptyVerifiers(), silentLogger())

	require.Len(t, routes, 1)
	assert.Nil(t, routes[0].Headers,
		"a fully poisoned Headers map collapses to nil")
}

// TestCollectRoutes_HeadersCleanValuesSurvive pins the happy path:
// well-formed headers thread through unchanged so the sanitizer adds
// no false positives.
func TestCollectRoutes_HeadersCleanValuesSurvive(t *testing.T) {
	snapshot := &registry.Snapshot{
		Entries: map[string]registry.HandlerEntry{
			"svc.cmd.users.list": {
				HTTP: &registry.HTTPMeta{Method: "GET", Path: "/users"},
				Headers: map[string]string{
					"x-frame-options":           "DENY",
					"strict-transport-security": "max-age=31536000",
				},
			},
		},
	}

	routes := CollectRoutes(snapshot, emptyVerifiers(), silentLogger())

	require.Len(t, routes, 1)
	assert.Equal(t, "DENY", routes[0].Headers["x-frame-options"])
	assert.Equal(t, "max-age=31536000", routes[0].Headers["strict-transport-security"])
}

// TestCollectRoutes_DropsCORSWithCRLFInOrigin pins the CORS-injection
// fail-closed: any CRLF/NUL byte in any Origins, Methods, Headers, or
// ExposeHeaders entry drops the ENTIRE CORS block. Partial CORS is
// strictly worse than no CORS — a half-valid block hides the
// misconfiguration behind apparently-working preflights while the
// poisoned string remains an injection primitive on every request.
func TestCollectRoutes_DropsCORSWithCRLFInOrigin(t *testing.T) {
	cases := []struct {
		name string
		cors *registry.CORSMeta
	}{
		{
			name: "CRLF in origin",
			cors: &registry.CORSMeta{
				Origins: []string{"https://app.com\r\nSet-Cookie: stolen=1"},
			},
		},
		{
			name: "LF in origin",
			cors: &registry.CORSMeta{
				Origins: []string{"https://app.com\nx-poison: 1"},
			},
		},
		{
			name: "NUL in origin",
			cors: &registry.CORSMeta{
				Origins: []string{"https://app.com\x00"},
			},
		},
		{
			name: "CRLF in method",
			cors: &registry.CORSMeta{
				Origins: []string{"https://app.com"},
				Methods: []string{"GET\r\nX-Header: 1"},
			},
		},
		{
			name: "CRLF in allow-headers",
			cors: &registry.CORSMeta{
				Origins: []string{"https://app.com"},
				Headers: []string{"Authorization\r\nSet-Cookie: x=1"},
			},
		},
		{
			name: "CRLF in expose-headers",
			cors: &registry.CORSMeta{
				Origins:       []string{"https://app.com"},
				ExposeHeaders: []string{"X-Trace\r\nSet-Cookie: x=1"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := &registry.Snapshot{
				Entries: map[string]registry.HandlerEntry{
					"svc.cmd.users.list": {
						HTTP: &registry.HTTPMeta{Method: "GET", Path: "/users"},
						CORS: tc.cors,
					},
				},
			}

			routes := CollectRoutes(snapshot, emptyVerifiers(), silentLogger())

			require.Len(t, routes, 1)
			assert.Nil(t, routes[0].CORS,
				"CORS block with CRLF/NUL in any string MUST be dropped — partial CORS is worse than no CORS")
		})
	}
}

// TestCollectRoutes_KeepsCleanCORS pins the no-false-positive contract:
// well-formed CORS configurations thread through unchanged.
func TestCollectRoutes_KeepsCleanCORS(t *testing.T) {
	snapshot := &registry.Snapshot{
		Entries: map[string]registry.HandlerEntry{
			"svc.cmd.users.list": {
				HTTP: &registry.HTTPMeta{Method: "GET", Path: "/users"},
				CORS: &registry.CORSMeta{
					Origins:       []string{"https://app.com", "https://admin.example.com"},
					Methods:       []string{"GET", "POST"},
					Headers:       []string{"Authorization", "Content-Type"},
					ExposeHeaders: []string{"X-Trace-Id"},
				},
			},
		},
	}

	routes := CollectRoutes(snapshot, emptyVerifiers(), silentLogger())

	require.Len(t, routes, 1)
	require.NotNil(t, routes[0].CORS)
	assert.Len(t, routes[0].CORS.Origins, 2)
	assert.Equal(t, []string{"GET", "POST"}, routes[0].CORS.Methods)
}

func TestCollectRoutes_DropsRateLimitWithNegativeBurst(t *testing.T) {
	snapshot := &registry.Snapshot{
		Entries: map[string]registry.HandlerEntry{
			"svc.cmd.users.list": {
				HTTP:      &registry.HTTPMeta{Method: "GET", Path: "/users"},
				RateLimit: &registry.RateLimitMeta{RPS: 10, Burst: -1},
			},
		},
	}

	routes := CollectRoutes(snapshot, emptyVerifiers(), silentLogger())

	require.Len(t, routes, 1)
	assert.Nil(t, routes[0].RateLimit,
		"routing builder must drop a negative-burst block; GCRA.Check is undefined on negative burst")
}
