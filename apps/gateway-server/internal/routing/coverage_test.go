package routing

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/registry"
)

// TestMarshalZerologArray_EmitsRateLimitFieldOnlyWhenSet pins the
// optional-field shape of the route log array: a route with a
// non-nil RateLimit MUST surface the rateLimit string field, while
// a route without one MUST NOT — operators filtering on
// `routes.rateLimit` rely on the field's absence/presence as a
// signal for which routes have a policy attached.
func TestMarshalZerologArray_EmitsRateLimitFieldOnlyWhenSet(t *testing.T) {
	// Given: two routes — one with rate limit, one without.
	routes := []Route{
		{
			Method:       "GET",
			PathTemplate: "/limited",
			Subject:      "svc.cmd.limited",
			RateLimit:    &registry.RateLimitMeta{RPS: 50, Burst: 100},
		},
		{
			Method:       "GET",
			PathTemplate: "/open",
			Subject:      "svc.cmd.open",
		},
	}

	// When: marshalling through a real zerolog logger.
	var buf bytes.Buffer
	logger := zerolog.New(&buf)
	logger.Info().Array("routes", routesArray(routes)).Msg("snapshot")

	var entry map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(buf.Bytes(), &entry))

	var arr []map[string]any
	require.NoError(t, json.Unmarshal(entry["routes"], &arr))
	require.Len(t, arr, 2)

	// Then: the rate-limited route carries a rateLimit string,
	// formatted as "<rps> rps" — the unlimited route omits the key
	// entirely so a downstream filter can branch on its absence.
	assert.Equal(t, "50 rps", arr[0]["rateLimit"])
	_, hasRateLimit := arr[1]["rateLimit"]
	assert.False(t, hasRateLimit, "routes without a rate limit must omit the rateLimit field")
}

// TestMarshalZerologArray_EmitsTimeoutFieldOnlyWhenPositive pins the
// optional-timeout shape: zero/unset Timeout omits the field, a
// positive Timeout surfaces it as a Duration string. Operators read
// the field to spot routes that intentionally override the global
// budget; a zero-valued "0s" entry would clutter the log without
// adding information.
func TestMarshalZerologArray_EmitsTimeoutFieldOnlyWhenPositive(t *testing.T) {
	// Given: one route with a per-route timeout, one without.
	routes := []Route{
		{
			Method:       "GET",
			PathTemplate: "/slow",
			Subject:      "svc.cmd.slow",
			Timeout:      5 * time.Second,
		},
		{
			Method:       "GET",
			PathTemplate: "/default",
			Subject:      "svc.cmd.default",
		},
	}

	// When: marshalling through a real zerolog logger.
	var buf bytes.Buffer
	logger := zerolog.New(&buf)
	logger.Info().Array("routes", routesArray(routes)).Msg("snapshot")

	var entry map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(buf.Bytes(), &entry))

	var arr []map[string]any
	require.NoError(t, json.Unmarshal(entry["routes"], &arr))
	require.Len(t, arr, 2)

	// Then: the override route carries a duration string; the
	// default route omits the field entirely.
	assert.Equal(t, "5s", arr[0]["timeout"])
	_, hasTimeout := arr[1]["timeout"]
	assert.False(t, hasTimeout, "routes using the global timeout must omit the timeout field")
}

// TestMarshalZerologArray_FullPolicyRouteEmitsEveryField exercises a
// route with every optional policy block populated. Each `if d != nil`
// or `if route.X > 0` predicate in MarshalZerologArray must light
// up so an operator inspecting the log entry sees the complete
// effective policy in one pass.
func TestMarshalZerologArray_FullPolicyRouteEmitsEveryField(t *testing.T) {
	// Given: a route with auth, cors, rate-limit, headers, and
	// timeout all set.
	route := Route{
		Method:       "POST",
		PathTemplate: "/everything/:id",
		Subject:      "svc.cmd.everything",
		Auth:         &RouteAuth{VerifierSubject: "svc.cmd.auth.verifier.jwt"},
		CORS:         &registry.CORSMeta{Origins: []string{"https://example.com"}},
		RateLimit:    &registry.RateLimitMeta{RPS: 30, Burst: 60},
		Headers:      map[string]string{"x-frame-options": "DENY"},
		Timeout:      2500 * time.Millisecond,
	}

	// When: marshalling through a real zerolog logger.
	var buf bytes.Buffer
	logger := zerolog.New(&buf)
	logger.Info().Array("routes", routesArray([]Route{route})).Msg("full")

	var entry map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(buf.Bytes(), &entry))

	var arr []map[string]any
	require.NoError(t, json.Unmarshal(entry["routes"], &arr))
	require.Len(t, arr, 1)
	got := arr[0]

	// Then: every conditional field is present in the marshalled
	// output. The rateLimit and timeout fields use string formatting
	// so the assertions match their human-readable shape.
	//
	// route.Headers is intentionally NOT in the marshalled shape —
	// the godoc on MarshalZerologArray scopes "extended policy fields"
	// to auth/cors/rateLimit/timeout. Adding headers would expose the
	// full route-Header map (potentially containing operator-set
	// secrets) in every routing-rebuild log line. The omission is a
	// security choice, not an oversight; pin the absence so a future
	// refactor that expands the marshaller surfaces this test failure.
	assert.Equal(t, "POST", got["method"])
	assert.Equal(t, "/everything/:id", got["path"])
	assert.Equal(t, "svc.cmd.everything", got["subject"])
	assert.Equal(t, true, got["auth"], "auth flag follows route.Auth presence")
	assert.Equal(t, true, got["cors"], "cors flag follows route.CORS presence")
	assert.Equal(t, "30 rps", got["rateLimit"])
	assert.Equal(t, "2.5s", got["timeout"])
	_, hasHeaders := got["headers"]
	assert.False(t, hasHeaders,
		"headers must be omitted from the routing log to avoid leaking "+
			"operator-set static-header values (potentially carrying secrets) "+
			"on every routing-table rebuild")
}

// TestDiffRouteConfig_SubjectChangeIsNotReportedAsModified pins the
// Subject-rename invariant on diffRouteConfig: the Subject field is
// not part of route identity OR config diffing, so a pure subject
// rename produces zero "modified" entries. ComputeDelta also rolls
// such a change into Unchanged, matching the higher-level
// "operators don't get paged for an internal refactor" contract.
func TestDiffRouteConfig_SubjectChangeIsNotReportedAsModified(t *testing.T) {
	// Given: two routes whose Subject differs but every other field
	// is identical.
	prev := Route{
		Method:       "GET",
		PathTemplate: "/users",
		Subject:      "svc.cmd.users.list.v1",
	}
	next := prev
	next.Subject = "svc.cmd.users.list.v2"

	// When: diffing the two routes.
	changes := diffRouteConfig(prev, next)

	// Then: no fields are reported as modified.
	assert.Empty(t, changes,
		"a Subject change is not part of route config — diff must report no modifications")
}

// TestDiffRouteConfig_PathTemplateChangesAreNotReported pins that
// PathTemplate is part of identity, not of config: ComputeDelta
// keys on (method, template), so a different template is a brand-
// new route, not a "modified" one. diffRouteConfig deliberately
// stays silent on it; the change shows up as add+remove at the
// ComputeDelta level instead.
func TestDiffRouteConfig_PathTemplateChangesAreNotReported(t *testing.T) {
	// Given: two routes with the same method but different
	// templates. (This is an unrealistic input for diffRouteConfig
	// because ComputeDelta would never feed it such a pair, but it
	// exercises the contract that diffRouteConfig itself does not
	// inspect identity fields.)
	prev := Route{Method: "GET", PathTemplate: "/users/:id"}
	next := Route{Method: "GET", PathTemplate: "/accounts/:id"}

	// When: diffing the two routes.
	changes := diffRouteConfig(prev, next)

	// Then: no fields are reported as modified.
	assert.Empty(t, changes,
		"PathTemplate is identity, not config — diff must stay silent on it")
}

// TestDiffRouteConfig_ReportsRateLimitChange pins the rateLimit
// field-detection branch: when the rate-limit policy changes, the
// diff must surface "rateLimit" so operators reviewing a delta log
// see the field name without inspecting the routing-table snapshot.
func TestDiffRouteConfig_ReportsRateLimitChange(t *testing.T) {
	// Given: same identity, different rate-limit RPS.
	prev := Route{
		Method:       "GET",
		PathTemplate: "/users",
		RateLimit:    &registry.RateLimitMeta{RPS: 10},
	}
	next := Route{
		Method:       "GET",
		PathTemplate: "/users",
		RateLimit:    &registry.RateLimitMeta{RPS: 20},
	}

	// When: diffing.
	changes := diffRouteConfig(prev, next)

	// Then: rateLimit appears in the change list.
	assert.Contains(t, changes, "rateLimit")
}

// TestDiffRouteConfig_ReportsHeadersChange pins the headers
// field-detection branch.
func TestDiffRouteConfig_ReportsHeadersChange(t *testing.T) {
	// Given: same identity, different static-response-headers.
	prev := Route{
		Method:       "GET",
		PathTemplate: "/users",
		Headers:      map[string]string{"x-frame-options": "DENY"},
	}
	next := Route{
		Method:       "GET",
		PathTemplate: "/users",
		Headers:      map[string]string{"x-frame-options": "SAMEORIGIN"},
	}

	// When: diffing.
	changes := diffRouteConfig(prev, next)

	// Then: headers appears in the change list.
	assert.Contains(t, changes, "headers")
}

// TestDiffRouteConfig_ReportsAuthChange pins the auth field-detection
// branch: a change to the verifier subject (or to the optional flag)
// must surface "auth" in the change list so operators reviewing a
// delta log see the security-relevant flip without inspecting two
// routing-table snapshots side by side.
func TestDiffRouteConfig_ReportsAuthChange(t *testing.T) {
	// Given: same identity, different verifier subject.
	prev := Route{
		Method:       "GET",
		PathTemplate: "/users/me",
		Auth:         &RouteAuth{VerifierSubject: "svc.cmd.auth.verifier.jwt"},
	}
	next := Route{
		Method:       "GET",
		PathTemplate: "/users/me",
		Auth:         &RouteAuth{VerifierSubject: "svc.cmd.auth.verifier.session"},
	}

	// When: diffing.
	changes := diffRouteConfig(prev, next)

	// Then: auth appears in the change list.
	assert.Contains(t, changes, "auth")
}

// TestCorsEqual covers the corsEqual nil-and-value cross-product so
// every conditional branch lights up: nil/nil, nil/non-nil,
// non-nil/nil, populated equal, populated unequal.
func TestCorsEqual(t *testing.T) {
	populated := &registry.CORSMeta{
		Origins: []string{"https://example.com"},
		Methods: []string{"GET", "POST"},
	}
	differentValue := &registry.CORSMeta{
		Origins: []string{"https://other.com"},
		Methods: []string{"GET", "POST"},
	}

	cases := []struct {
		name string
		a, b *registry.CORSMeta
		want bool
	}{
		{"both nil", nil, nil, true},
		{"left nil, right non-nil", nil, populated, false},
		{"left non-nil, right nil", populated, nil, false},
		{"both populated and equal", populated, &registry.CORSMeta{
			Origins: []string{"https://example.com"},
			Methods: []string{"GET", "POST"},
		}, true},
		{"both populated, origins differ", populated, differentValue, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, corsEqual(c.a, c.b))
			assert.Equal(t, c.want, corsEqual(c.b, c.a),
				"corsEqual must be symmetric")
		})
	}
}

// TestRateLimitEqual covers the full nil-and-value cross-product
// plus per-field differences. The 40% baseline of the function
// signals that the populated-and-equal branch and several
// populated-but-different field variants were unexercised; the
// table-driven cases below pin every documented invariant.
func TestRateLimitEqual(t *testing.T) {
	populated := &registry.RateLimitMeta{
		RPS:   30,
		Burst: 60,
		KeyBy: []string{"ip"},
		Store: "memory",
	}

	cases := []struct {
		name string
		a, b *registry.RateLimitMeta
		want bool
	}{
		{"both nil", nil, nil, true},
		{"left nil, right non-nil", nil, populated, false},
		{"left non-nil, right nil", populated, nil, false},
		{"both populated and field-by-field equal", populated, &registry.RateLimitMeta{
			RPS: 30, Burst: 60, KeyBy: []string{"ip"}, Store: "memory",
		}, true},
		{"RPS differs", populated, &registry.RateLimitMeta{
			RPS: 50, Burst: 60, KeyBy: []string{"ip"}, Store: "memory",
		}, false},
		{"Burst differs", populated, &registry.RateLimitMeta{
			RPS: 30, Burst: 120, KeyBy: []string{"ip"}, Store: "memory",
		}, false},
		{"Store differs", populated, &registry.RateLimitMeta{
			RPS: 30, Burst: 60, KeyBy: []string{"ip"}, Store: "nats-kv",
		}, false},
		{"KeyBy length differs", populated, &registry.RateLimitMeta{
			RPS: 30, Burst: 60, KeyBy: []string{"ip", "user"}, Store: "memory",
		}, false},
		{"KeyBy values differ at same length", populated, &registry.RateLimitMeta{
			RPS: 30, Burst: 60, KeyBy: []string{"user"}, Store: "memory",
		}, false},
		{"KeyBy order differs", &registry.RateLimitMeta{
			RPS: 30, Burst: 60, KeyBy: []string{"ip", "user"}, Store: "memory",
		}, &registry.RateLimitMeta{
			RPS: 30, Burst: 60, KeyBy: []string{"user", "ip"}, Store: "memory",
		}, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, rateLimitEqual(c.a, c.b))
			assert.Equal(t, c.want, rateLimitEqual(c.b, c.a),
				"rateLimitEqual must be symmetric")
		})
	}
}

// TestAuthEqual covers the full nil-and-value cross-product for
// authEqual. Like rateLimitEqual it sits at 40% baseline, so the
// table fans out every documented variant: nil/nil, nil/non-nil
// pairs (in both orders), populated-equal, VerifierSubject diff,
// Optional-flag diff.
func TestAuthEqual(t *testing.T) {
	populated := &RouteAuth{
		VerifierSubject: "svc.cmd.auth.verifier.jwt",
		Optional:        false,
	}

	cases := []struct {
		name string
		a, b *RouteAuth
		want bool
	}{
		{"both nil", nil, nil, true},
		{"left nil, right non-nil", nil, populated, false},
		{"left non-nil, right nil", populated, nil, false},
		{"both populated and equal", populated, &RouteAuth{
			VerifierSubject: "svc.cmd.auth.verifier.jwt",
			Optional:        false,
		}, true},
		{"VerifierSubject differs", populated, &RouteAuth{
			VerifierSubject: "svc.cmd.auth.verifier.session",
			Optional:        false,
		}, false},
		{"Optional flag differs", populated, &RouteAuth{
			VerifierSubject: "svc.cmd.auth.verifier.jwt",
			Optional:        true,
		}, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, authEqual(c.a, c.b))
			assert.Equal(t, c.want, authEqual(c.b, c.a),
				"authEqual must be symmetric")
		})
	}
}

// TestSplitPath covers every documented edge case of the path
// splitter so the table is the single source of truth for how the
// Trim+Split combination handles trailing slashes, root, and
// pathological inputs.
func TestSplitPath(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty string", "", nil},
		{"root path", "/", nil},
		{"only slashes", "///", nil},
		{"single segment", "/users", []string{"users"}},
		{"single segment trailing slash", "/users/", []string{"users"}},
		{"multiple segments", "/users/42/items", []string{"users", "42", "items"}},
		{"multiple segments trailing slash", "/users/42/", []string{"users", "42"}},
		{"no leading slash", "users/42", []string{"users", "42"}},
		{"empty inner segments preserved", "/foo//bar", []string{"foo", "", "bar"}},
		{"leading multi-slashes are trimmed", "//foo/bar", []string{"foo", "bar"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, splitPath(c.in))
		})
	}
}
