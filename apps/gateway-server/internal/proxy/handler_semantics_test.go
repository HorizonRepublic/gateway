package proxy

// Handler semantics pack: pins the proxy-layer contracts fixed in the
// proxy-semantics correctness pass — plain-OPTIONS routing, CORS on
// error responses, the shared verifier/route timeout budget, rate-limit
// gate ordering around the auth flow, case-insensitive gateway-owned
// header handling, and envelope auth-field hygiene.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gerrors "github.com/HorizonRepublic/gateway/apps/gateway-server/internal/errors"
	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/ratelimit"
	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/registry"
	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/routing"
)

// --- Plain OPTIONS routing ---

// TestHandler_PlainOPTIONSReachesRegisteredRoute pins the WHATWG Fetch
// preflight definition: only an OPTIONS request carrying
// Access-Control-Request-Method is a CORS preflight. A plain OPTIONS
// request must go through normal route lookup so SDK-declared OPTIONS
// routes (a first-class verb on both sides of the contract) are
// reachable.
func TestHandler_PlainOPTIONSReachesRegisteredRoute(t *testing.T) {
	table := &fakeTable{routes: map[string]routing.Route{
		"OPTIONS /capabilities": {
			Subject: "svc.cmd.capabilities", PathTemplate: "/capabilities",
			Method: "OPTIONS",
		},
	}}
	nats := newFakeNats()
	nats.reply = []byte(`{"status":200,"headers":{},"body":{"caps":["a","b"]}}`)

	h := NewHandler(HandlerConfig{
		Table:   func() routing.Table { return table },
		Nats:    nats,
		Encoder: NewDefaultEncoder(),
		Decoder: NewDefaultDecoder(),
		Timeout: 30 * time.Second,
		Logger:  zerolog.Nop(),
	})

	result := h.Handle(context.Background(), emptyServeInput("OPTIONS", "/capabilities"))

	assert.Equal(t, 200, result.Status)
	require.Len(t, nats.requests, 1, "plain OPTIONS must forward to the registered route")
	assert.Equal(t, "svc.cmd.capabilities", nats.requests[0].subject)
}

// TestHandler_PreflightStillWinsOverRegisteredOPTIONSRoute pins the
// discriminator: when Access-Control-Request-Method IS present, the
// request is a preflight and is answered locally — even if an OPTIONS
// route happens to be registered for the same path.
func TestHandler_PreflightStillWinsOverRegisteredOPTIONSRoute(t *testing.T) {
	cors := &registry.CORSMeta{Origins: []string{"https://app.example.com"}}
	table := &fakeTable{routes: map[string]routing.Route{
		"GET /caps":     {Subject: "svc.cmd.caps.get", PathTemplate: "/caps", Method: "GET", CORS: cors},
		"OPTIONS /caps": {Subject: "svc.cmd.caps.options", PathTemplate: "/caps", Method: "OPTIONS"},
	}}
	nats := newFakeNats()
	h := NewHandler(HandlerConfig{
		Table:   func() routing.Table { return table },
		Nats:    nats,
		Encoder: NewDefaultEncoder(),
		Decoder: NewDefaultDecoder(),
		Timeout: 30 * time.Second,
		Logger:  zerolog.Nop(),
	})

	in := emptyServeInput("OPTIONS", "/caps")
	in.Headers["origin"] = "https://app.example.com"
	in.Headers["access-control-request-method"] = "GET"

	result := h.Handle(context.Background(), in)

	assert.Equal(t, 204, result.Status, "ACRM-carrying OPTIONS is a preflight, answered locally")
	assert.Empty(t, nats.requests, "preflight must not reach NATS")
}

// TestHandler_PlainOPTIONSFollowsMethodMismatchSemantics pins the
// fall-through: plain OPTIONS to a path registered under other verbs
// only gets the standard 405 + Allow treatment instead of the previous
// unconditional preflight-404 hijack.
func TestHandler_PlainOPTIONSFollowsMethodMismatchSemantics(t *testing.T) {
	table := routing.BuildTableFromRoutes([]routing.Route{
		{Subject: "svc.cmd.users.list", PathTemplate: "/users", Method: "GET"},
	})
	h := buildHandler(table, nil, nil)

	in := emptyServeInput("OPTIONS", "/users")
	in.Headers["origin"] = "https://example.com"

	result := h.Handle(context.Background(), in)

	assert.Equal(t, 405, result.Status)
	assert.Equal(t, []string{"GET"}, result.Headers["Allow"])
}

// --- 405 for parameterized routes ---

// TestHandler_MethodMismatchOnParameterizedRouteReturns405 pins RFC
// 9110 §15.5.6 semantics for template routes: a wrong-method request
// to a concrete path matching "/users/:id" must yield 405 + Allow,
// not 404 — the same classification static routes already get.
func TestHandler_MethodMismatchOnParameterizedRouteReturns405(t *testing.T) {
	table := routing.BuildTableFromRoutes([]routing.Route{
		{Subject: "svc.cmd.users.get", PathTemplate: "/users/:id", Method: "GET"},
	})
	h := buildHandler(table, nil, nil)

	result := h.Handle(context.Background(), emptyServeInput("POST", "/users/42"))

	assert.Equal(t, 405, result.Status)
	assert.Equal(t, gerrors.MethodNotAllowed.Body, result.Body)
	assert.Equal(t, []string{"GET"}, result.Headers["Allow"],
		"Allow must be template-matched, not exact-path-matched")
}

// --- CORS headers on error responses ---

// TestHandler_ErrorResponsesCarryCORSHeaders pins the Fetch CORS check
// for non-2xx outcomes on a matched CORS route: without
// Access-Control-Allow-Origin on the 401/429/5xx, cross-origin
// JavaScript cannot read the status (a session-expired 401 becomes
// indistinguishable from a network outage) and the documented
// Access-Control-Expose-Headers contract for X-RateLimit-*/Retry-After
// is moot. Every response of a matched route is also origin-varying
// content, so Vary: Origin must ride along.
func TestHandler_ErrorResponsesCarryCORSHeaders(t *testing.T) {
	const origin = "https://app.example.com"
	cors := &registry.CORSMeta{Origins: []string{origin}}

	verifierSubject := "auth-svc.cmd.auth.verifier.jwt"
	routeSubject := "svc.cmd.users.create"

	baseRoute := func() routing.Route {
		return routing.Route{
			Subject: routeSubject, PathTemplate: "/users", Method: "POST", CORS: cors,
		}
	}

	cases := []struct {
		name       string
		setup      func(t *testing.T) (*Handler, *ServeInput)
		wantStatus int
	}{
		{
			name: "400 invalid body",
			setup: func(t *testing.T) (*Handler, *ServeInput) {
				t.Helper()
				h := buildHandler(stubTable(baseRoute()), nil, nil)
				in := emptyServeInput("POST", "/users")
				in.Body = []byte("not-json")

				return h, in
			},
			wantStatus: 400,
		},
		{
			name: "401 from verifier",
			setup: func(t *testing.T) (*Handler, *ServeInput) {
				t.Helper()
				route := baseRoute()
				route.Auth = &routing.RouteAuth{VerifierSubject: verifierSubject}
				nats := newFakeNats()
				nats.program(verifierSubject,
					[]byte(`{"status":401,"headers":{},"body":{"error":"Unauthorized"}}`), nil)

				return newAuthHandler(stubTable(route), nats), emptyServeInput("POST", "/users")
			},
			wantStatus: 401,
		},
		{
			name: "429 rate limited",
			setup: func(t *testing.T) (*Handler, *ServeInput) {
				t.Helper()
				route := baseRoute()
				route.RateLimit = &registry.RateLimitMeta{RPS: 10}
				h := NewHandler(HandlerConfig{
					Table:       func() routing.Table { return stubTable(route) },
					Nats:        newFakeNats(),
					Encoder:     NewDefaultEncoder(),
					Decoder:     NewDefaultDecoder(),
					Timeout:     30 * time.Second,
					Logger:      zerolog.Nop(),
					RateLimiter: routerWithStore(t, &fakeRateLimiter{allowed: false}),
				})

				return h, emptyServeInput("POST", "/users")
			},
			wantStatus: 429,
		},
		{
			name: "503 nats failure",
			setup: func(t *testing.T) (*Handler, *ServeInput) {
				t.Helper()
				h := buildHandler(stubTable(baseRoute()), nil, assert.AnError)

				return h, emptyServeInput("POST", "/users")
			},
			wantStatus: 503,
		},
		{
			name: "504 nats timeout",
			setup: func(t *testing.T) (*Handler, *ServeInput) {
				t.Helper()
				h := buildHandler(stubTable(baseRoute()), nil, context.DeadlineExceeded)

				return h, emptyServeInput("POST", "/users")
			},
			wantStatus: 504,
		},
		{
			name: "502 malformed reply",
			setup: func(t *testing.T) (*Handler, *ServeInput) {
				t.Helper()
				h := buildHandler(stubTable(baseRoute()), []byte(`not json`), nil)

				return h, emptyServeInput("POST", "/users")
			},
			wantStatus: 502,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, in := tc.setup(t)
			in.Headers["origin"] = origin

			result := h.Handle(context.Background(), in)

			require.Equal(t, tc.wantStatus, result.Status)
			assert.Equal(t, []string{origin}, result.Headers["Access-Control-Allow-Origin"],
				"error responses on a matched CORS route must be readable cross-origin")
			assert.Contains(t, result.Headers["Vary"], "Origin",
				"origin-varying error content must carry Vary: Origin")
		})
	}
}

// --- Rate-limit gate ordering around the auth flow ---

// TestHandler_IPKeyedRateLimitGatesBeforeVerifier pins the pre-auth
// gate for keyBy chains that never read claims: an unauthenticated
// flood against a protected route must burn rate-limit tokens and see
// 429 — not hammer the verifier at line rate while the declared limit
// never fires.
func TestHandler_IPKeyedRateLimitGatesBeforeVerifier(t *testing.T) {
	verifierSubject := "auth-svc.cmd.auth.verifier.jwt"
	route := routing.Route{
		Subject: "svc.cmd.users.me", PathTemplate: "/users/me", Method: "GET",
		Auth:      &routing.RouteAuth{VerifierSubject: verifierSubject},
		RateLimit: &registry.RateLimitMeta{RPS: 10, KeyBy: []string{"ip"}},
	}
	nats := newFakeNats()
	nats.program(verifierSubject,
		[]byte(`{"status":401,"headers":{},"body":{"error":"Unauthorized"}}`), nil)

	h := NewHandler(HandlerConfig{
		Table:       func() routing.Table { return stubTable(route) },
		Nats:        nats,
		Encoder:     NewDefaultEncoder(),
		Decoder:     NewDefaultDecoder(),
		Timeout:     30 * time.Second,
		Logger:      zerolog.Nop(),
		RateLimiter: routerWithStore(t, &fakeRateLimiter{allowed: false}),
	})

	in := emptyServeInput("GET", "/users/me")
	in.RemoteAddr = "1.2.3.4"
	in.Headers["authorization"] = "Bearer garbage"

	result := h.Handle(context.Background(), in)

	assert.Equal(t, 429, result.Status)
	assert.Empty(t, nats.requests,
		"a rate-limited request must never reach the verifier")
}

// TestHandler_AuthRejectionStillChargesRateLimitBucket pins the cost
// accounting of the pre-auth gate: a request that clears the gate and
// then fails auth has consumed a token, and the 401 must carry the
// X-RateLimit-* headers so clients can size their retry budget.
func TestHandler_AuthRejectionStillChargesRateLimitBucket(t *testing.T) {
	verifierSubject := "auth-svc.cmd.auth.verifier.jwt"
	route := routing.Route{
		Subject: "svc.cmd.users.me", PathTemplate: "/users/me", Method: "GET",
		Auth:      &routing.RouteAuth{VerifierSubject: verifierSubject},
		RateLimit: &registry.RateLimitMeta{RPS: 10, KeyBy: []string{"ip"}},
	}
	nats := newFakeNats()
	nats.program(verifierSubject,
		[]byte(`{"status":401,"headers":{},"body":{"error":"Unauthorized"}}`), nil)

	rl := &fakeRateLimiter{allowed: true}
	h := NewHandler(HandlerConfig{
		Table:       func() routing.Table { return stubTable(route) },
		Nats:        nats,
		Encoder:     NewDefaultEncoder(),
		Decoder:     NewDefaultDecoder(),
		Timeout:     30 * time.Second,
		Logger:      zerolog.Nop(),
		RateLimiter: routerWithStore(t, rl),
	})

	in := emptyServeInput("GET", "/users/me")
	in.RemoteAddr = "1.2.3.4"
	in.Headers["authorization"] = "Bearer garbage"

	result := h.Handle(context.Background(), in)

	assert.Equal(t, 401, result.Status)
	require.Len(t, rl.calls, 1, "the gate must run before the verifier and charge the bucket")
	assert.Equal(t, []string{"10"}, result.Headers["X-RateLimit-Limit"],
		"the charged 401 must expose the rate-limit budget")
}

// TestHandler_ClaimsKeyedRateLimitStaysPostAuth pins the other half of
// the split: a keyBy chain containing a user: strategy needs verified
// claims, so its gate must keep running AFTER the auth flow.
func TestHandler_ClaimsKeyedRateLimitStaysPostAuth(t *testing.T) {
	verifierSubject := "auth-svc.cmd.auth.verifier.jwt"
	route := routing.Route{
		Subject: "svc.cmd.users.me", PathTemplate: "/users/me", Method: "GET",
		Auth:      &routing.RouteAuth{VerifierSubject: verifierSubject},
		RateLimit: &registry.RateLimitMeta{RPS: 10, KeyBy: []string{"user:id", "ip"}},
	}
	nats := newFakeNats()
	nats.program(verifierSubject,
		[]byte(`{"status":200,"headers":{},"body":{"id":"user-123"}}`), nil)

	rl := &fakeRateLimiter{allowed: false}
	h := NewHandler(HandlerConfig{
		Table:       func() routing.Table { return stubTable(route) },
		Nats:        nats,
		Encoder:     NewDefaultEncoder(),
		Decoder:     NewDefaultDecoder(),
		Timeout:     30 * time.Second,
		Logger:      zerolog.Nop(),
		RateLimiter: routerWithStore(t, rl),
	})

	in := emptyServeInput("GET", "/users/me")
	in.RemoteAddr = "1.2.3.4"
	in.Headers["authorization"] = "Bearer tok"

	result := h.Handle(context.Background(), in)

	assert.Equal(t, 429, result.Status)
	require.Len(t, nats.requests, 1, "verifier runs first for a claims-keyed gate")
	assert.Equal(t, verifierSubject, nats.requests[0].subject)
	require.Len(t, rl.calls, 1)
	assert.Equal(t, ratelimit.BuildBucketKey("GET", "/users/me", "user-123"), rl.calls[0].key,
		"the bucket must be keyed on the verified claim, not the IP")
}

// --- Shared verifier/route timeout budget ---

// delayRequester injects a per-subject wall-clock delay before
// delegating to the wrapped fakeRequester. Models a slow verifier or
// upstream without a real transport.
type delayRequester struct {
	inner *fakeRequester
	delay map[string]time.Duration
}

func (d *delayRequester) Request(
	ctx context.Context,
	subject string,
	payload []byte,
	timeout time.Duration,
) ([]byte, error) {
	if dl := d.delay[subject]; dl > 0 {
		time.Sleep(dl)
	}

	return d.inner.Request(ctx, subject, payload, timeout)
}

// TestHandler_VerifierAndRouteShareOneTimeoutBudget pins the
// per-route deadline contract: the verifier leg and the main route leg
// draw from ONE budget anchored at request entry. After a verifier
// that consumes ~half the budget, the main NATS request (and the
// envelope's advertised timeoutMs) must carry only the remainder —
// not the full route timeout over again.
func TestHandler_VerifierAndRouteShareOneTimeoutBudget(t *testing.T) {
	const routeTimeout = 400 * time.Millisecond
	const verifierDelay = 200 * time.Millisecond

	verifierSubject := "auth-svc.cmd.auth.verifier.jwt"
	routeSubject := "svc.cmd.users.me"
	route := routing.Route{
		Subject: routeSubject, PathTemplate: "/users/me", Method: "GET",
		Timeout: routeTimeout,
		Auth:    &routing.RouteAuth{VerifierSubject: verifierSubject},
	}

	inner := newFakeNats()
	inner.program(verifierSubject, []byte(`{"status":200,"headers":{},"body":{"id":"u1"}}`), nil)
	inner.program(routeSubject, []byte(`{"status":200,"headers":{},"body":{"ok":true}}`), nil)

	h := NewHandler(HandlerConfig{
		Table:   func() routing.Table { return stubTable(route) },
		Nats:    &delayRequester{inner: inner, delay: map[string]time.Duration{verifierSubject: verifierDelay}},
		Encoder: NewDefaultEncoder(),
		Decoder: NewDefaultDecoder(),
		Timeout: 30 * time.Second,
		Logger:  zerolog.Nop(),
	})

	result := h.Handle(context.Background(), authServeInput("GET", "/users/me"))

	require.Equal(t, 200, result.Status)
	require.Len(t, inner.requests, 2)

	mainCall := inner.requests[1]
	assert.LessOrEqual(t, mainCall.timeout, routeTimeout-verifierDelay,
		"main request must receive only the remaining budget")
	assert.Greater(t, mainCall.timeout, time.Duration(0))

	var payload map[string]any
	require.NoError(t, json.Unmarshal(mainCall.payload, &payload))
	meta, ok := payload["meta"].(map[string]any)
	require.True(t, ok)
	timeoutMs, ok := meta["timeoutMs"].(float64)
	require.True(t, ok)
	assert.LessOrEqual(t, timeoutMs, float64((routeTimeout - verifierDelay).Milliseconds()),
		"envelope must advertise the remaining budget, not the full route timeout")
	assert.Greater(t, timeoutMs, float64(0))
}

// TestHandler_VerifierExhaustingBudgetShortCircuits504 pins the
// degenerate case: when the verifier leg consumes the entire per-route
// budget, the gateway must return 504 without issuing the main
// request — there is nothing left for the upstream to run in.
func TestHandler_VerifierExhaustingBudgetShortCircuits504(t *testing.T) {
	const routeTimeout = 100 * time.Millisecond
	const verifierDelay = 160 * time.Millisecond

	verifierSubject := "auth-svc.cmd.auth.verifier.jwt"
	routeSubject := "svc.cmd.users.me"
	route := routing.Route{
		Subject: routeSubject, PathTemplate: "/users/me", Method: "GET",
		Timeout: routeTimeout,
		Auth:    &routing.RouteAuth{VerifierSubject: verifierSubject},
	}

	inner := newFakeNats()
	inner.program(verifierSubject, []byte(`{"status":200,"headers":{},"body":{"id":"u1"}}`), nil)

	h := NewHandler(HandlerConfig{
		Table:   func() routing.Table { return stubTable(route) },
		Nats:    &delayRequester{inner: inner, delay: map[string]time.Duration{verifierSubject: verifierDelay}},
		Encoder: NewDefaultEncoder(),
		Decoder: NewDefaultDecoder(),
		Timeout: 30 * time.Second,
		Logger:  zerolog.Nop(),
	})

	result := h.Handle(context.Background(), authServeInput("GET", "/users/me"))

	assert.Equal(t, 504, result.Status)
	assert.Equal(t, gerrors.GatewayTimeout.Body, result.Body)
	require.Len(t, inner.requests, 1, "main route must not be called on an exhausted budget")
	assert.Equal(t, verifierSubject, inner.requests[0].subject)
}

// --- Case-insensitive gateway-owned header handling ---

// TestHandler_UpstreamRequestIDCaseVariantsAreStripped pins the
// anti-spoofing invariant against case-variant bypass: a reply
// carrying "X-Request-Id" (any casing) must not survive alongside the
// gateway-stamped lowercase key — Hertz folds both onto one canonical
// wire name and the forged line would win ~half the time.
func TestHandler_UpstreamRequestIDCaseVariantsAreStripped(t *testing.T) {
	table := &fakeTable{routes: map[string]routing.Route{
		"GET /users": {Subject: "svc.cmd.users.list", PathTemplate: "/users", Method: "GET"},
	}}
	reply := []byte(`{"status":200,"headers":{"X-Request-Id":["forged"],"X-REQUEST-ID":["forged2"]},"body":null}`)
	h := buildHandler(table, reply, nil)

	result := h.Handle(context.Background(), emptyServeInput("GET", "/users"))

	assert.Equal(t, []string{"r1"}, result.Headers["x-request-id"])
	for key, values := range result.Headers {
		if key == "x-request-id" {
			continue
		}
		assert.False(t, strings.EqualFold(key, "x-request-id"),
			"case-variant x-request-id must not survive the merge: %s=%v", key, values)
	}
}

// TestHandler_UpstreamContentTypeCaseVariantFoldsToSingleEntry pins
// the same fold for the content-type default: an upstream override in
// any casing must replace the gateway default instead of producing a
// second, conflicting content-type line.
func TestHandler_UpstreamContentTypeCaseVariantFoldsToSingleEntry(t *testing.T) {
	table := &fakeTable{routes: map[string]routing.Route{
		"GET /users": {Subject: "svc.cmd.users.list", PathTemplate: "/users", Method: "GET"},
	}}
	reply := []byte(`{"status":200,"headers":{"Content-Type":["text/plain"]},"body":null}`)
	h := buildHandler(table, reply, nil)

	result := h.Handle(context.Background(), emptyServeInput("GET", "/users"))

	assert.Equal(t, []string{"text/plain"}, result.Headers["content-type"],
		"upstream content-type override must win regardless of casing")
	_, variant := result.Headers["Content-Type"]
	assert.False(t, variant, "no case-variant duplicate may survive")
}

// TestHandler_VerifierRequestIDCaseVariantIsDropped pins the merge
// rules for verifier reply headers: gateway-owned keys are excluded
// case-insensitively there as well.
func TestHandler_VerifierRequestIDCaseVariantIsDropped(t *testing.T) {
	verifierSubject := "auth-svc.cmd.auth.verifier.jwt"
	routeSubject := "svc.cmd.users.me"
	route := routing.Route{
		Subject: routeSubject, PathTemplate: "/users/me", Method: "GET",
		Auth: &routing.RouteAuth{VerifierSubject: verifierSubject},
	}

	nats := newFakeNats()
	nats.program(verifierSubject,
		[]byte(`{"status":200,"headers":{"X-Request-Id":["forged"]},"body":{"id":"u1"}}`), nil)
	nats.program(routeSubject,
		[]byte(`{"status":200,"headers":{},"body":{"ok":true}}`), nil)

	sut := newAuthHandler(stubTable(route), nats)

	result := sut.Handle(context.Background(), authServeInput("GET", "/users/me"))

	require.Equal(t, 200, result.Status)
	assert.Equal(t, []string{"r1"}, result.Headers["x-request-id"])
	for key := range result.Headers {
		if key == "x-request-id" {
			continue
		}
		assert.False(t, strings.EqualFold(key, "x-request-id"))
	}
}

// TestHandler_VerifierSetCookieCaseVariantStillMergesFirst pins the
// verifier-first Set-Cookie ordering across case variants: "Set-Cookie"
// from the verifier must fold into the canonical lowercase entry, not
// land as a separate key that silently loses the documented ordering.
func TestHandler_VerifierSetCookieCaseVariantStillMergesFirst(t *testing.T) {
	verifierSubject := "auth-svc.cmd.auth.verifier.jwt"
	routeSubject := "svc.cmd.users.me"
	route := routing.Route{
		Subject: routeSubject, PathTemplate: "/users/me", Method: "GET",
		Auth: &routing.RouteAuth{VerifierSubject: verifierSubject},
	}

	nats := newFakeNats()
	nats.program(verifierSubject,
		[]byte(`{"status":200,"headers":{"Set-Cookie":["sid=rotated; Path=/"]},"body":{"id":"u1"}}`), nil)
	nats.program(routeSubject,
		[]byte(`{"status":200,"headers":{"set-cookie":["theme=dark; Path=/"]},"body":null}`), nil)

	sut := newAuthHandler(stubTable(route), nats)

	result := sut.Handle(context.Background(), authServeInput("GET", "/users/me"))

	require.Equal(t, 200, result.Status)
	assert.Equal(t,
		[]string{"sid=rotated; Path=/", "theme=dark; Path=/"},
		result.Headers["set-cookie"],
		"verifier cookies first, then route cookies, in one canonical entry")
	_, variant := result.Headers["Set-Cookie"]
	assert.False(t, variant, "no case-variant duplicate may survive")
}

// TestHandler_RateLimitHeadersDedupeAgainstUpstreamCaseVariants pins
// the non-destructive merge across casings: an upstream that emits its
// own x-ratelimit-limit must win (existing keys win), and the gateway
// must not add a second, conflicting line under a different casing —
// Hertz would fold both onto one wire name with two values.
func TestHandler_RateLimitHeadersDedupeAgainstUpstreamCaseVariants(t *testing.T) {
	route := routing.Route{
		Subject: "svc.cmd.users.list", PathTemplate: "/users", Method: "GET",
		RateLimit: &registry.RateLimitMeta{RPS: 100},
	}
	nats := newFakeNats()
	nats.reply = []byte(`{"status":200,"headers":{"x-ratelimit-limit":["30"],"x-ratelimit-remaining":["4"]},"body":null}`)

	h := NewHandler(HandlerConfig{
		Table:       func() routing.Table { return stubTable(route) },
		Nats:        nats,
		Encoder:     NewDefaultEncoder(),
		Decoder:     NewDefaultDecoder(),
		Timeout:     30 * time.Second,
		Logger:      zerolog.Nop(),
		RateLimiter: routerWithStore(t, &fakeRateLimiter{allowed: true}),
	})

	result := h.Handle(context.Background(), emptyServeInput("GET", "/users"))

	require.Equal(t, 200, result.Status)
	assert.Equal(t, []string{"30"}, result.Headers["x-ratelimit-limit"],
		"upstream-supplied rate-limit header wins")
	_, dup := result.Headers["X-RateLimit-Limit"]
	assert.False(t, dup, "gateway must not add a case-variant duplicate")
	assert.Equal(t, []string{"4"}, result.Headers["x-ratelimit-remaining"])
	_, dupRemaining := result.Headers["X-RateLimit-Remaining"]
	assert.False(t, dupRemaining)
}

// TestHandler_RouteHeadersDedupeAgainstUpstreamCaseVariants extends
// the same fold to operator-configured static route headers.
func TestHandler_RouteHeadersDedupeAgainstUpstreamCaseVariants(t *testing.T) {
	table := &fakeTable{routes: map[string]routing.Route{
		"GET /users": {
			Subject: "svc.cmd.users.list", PathTemplate: "/users", Method: "GET",
			Headers: map[string]string{"Cache-Control": "public, max-age=60"},
		},
	}}
	reply := []byte(`{"status":200,"headers":{"cache-control":["no-store"]},"body":null}`)
	h := buildHandler(table, reply, nil)

	result := h.Handle(context.Background(), emptyServeInput("GET", "/users"))

	assert.Equal(t, []string{"no-store"}, result.Headers["cache-control"],
		"upstream header wins over static route header across casings")
	_, dup := result.Headers["Cache-Control"]
	assert.False(t, dup, "no case-variant duplicate may survive")
}

// --- Envelope auth-field hygiene ---

// TestHandler_VerifierNullClaimsOmitAuthField pins the envelope
// invariant: a verifier that legitimately replies 200 with a JSON
// null body (authenticated, no claims payload) must not leak an
// "auth":null field — the contract promises the key is absent when
// there are no claims.
func TestHandler_VerifierNullClaimsOmitAuthField(t *testing.T) {
	verifierSubject := "auth-svc.cmd.auth.verifier.jwt"
	routeSubject := "svc.cmd.users.me"
	route := routing.Route{
		Subject: routeSubject, PathTemplate: "/users/me", Method: "GET",
		Auth: &routing.RouteAuth{VerifierSubject: verifierSubject},
	}

	nats := newFakeNats()
	nats.program(verifierSubject, []byte(`{"status":200,"headers":{},"body":null}`), nil)
	nats.program(routeSubject, []byte(`{"status":200,"headers":{},"body":{"ok":true}}`), nil)

	sut := newAuthHandler(stubTable(route), nats)

	result := sut.Handle(context.Background(), authServeInput("GET", "/users/me"))

	require.Equal(t, 200, result.Status)
	require.Len(t, nats.requests, 2)

	var routePayload map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(nats.requests[1].payload, &routePayload))
	_, hasAuth := routePayload["auth"]
	assert.False(t, hasAuth, `a null verifier body must not emit "auth":null`)
}
