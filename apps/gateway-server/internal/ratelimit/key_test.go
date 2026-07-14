package ratelimit_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/ratelimit"
)

func TestBuildBucketKey_Schema(t *testing.T) {
	k := ratelimit.BuildBucketKey("GET", "/users/:id", "192.0.2.1")
	parts := strings.Split(k, ".")
	assert.Len(t, parts, 3)
	assert.Equal(t, "GET", parts[0])
	assert.Regexp(t, `^[a-z2-7]{13}$`, parts[1])
	assert.Regexp(t, `^[a-z2-7]{13}$`, parts[2])
}

func TestBuildBucketKey_NATSKVSafe(t *testing.T) {
	// Input contains characters NATS KV would reject directly.
	k := ratelimit.BuildBucketKey("POST", "/auth:login/v1", "header:x-api-key=abc 123")
	for _, forbidden := range []byte{':', ' ', '>', '*', '/'} {
		assert.NotContains(t, k, string(forbidden))
	}
}

func noHeader(_ string) string         { return "" }
func noCookie(_ string) (string, bool) { return "", false }

func TestResolveKey_IPAlwaysResolves(t *testing.T) {
	key := ratelimit.ResolveKey([]string{"ip"}, "1.2.3.4", noHeader, noCookie, nil)

	assert.Equal(t, "1.2.3.4", key)
}

// TestResolveKey_HeaderLookupIsCaseInsensitive pins the case-folding
// of the header suffix passed in keyBy entries. Adapters lowercase
// header names on the wire, so a config entry like "header:X-Api-Key"
// MUST resolve against the lowercase map without forcing the operator
// to write the keyBy chain in lowercase — otherwise mixed-case
// configs silently fall back to clientIP and collapse all NAT'd
// clients onto one bucket.
func TestResolveKey_HeaderLookupIsCaseInsensitive(t *testing.T) {
	headerFn := func(name string) string {
		if name == "x-api-key" {
			return "api-key-val"
		}

		return ""
	}

	cases := []struct {
		name string
		key  string
	}{
		{"all-lowercase", "header:x-api-key"},
		{"canonical-mime", "header:X-Api-Key"},
		{"all-uppercase", "header:X-API-KEY"},
		{"mixed-case", "header:x-API-key"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ratelimit.ResolveKey([]string{c.key, "ip"}, "1.2.3.4", headerFn, noCookie, nil)
			assert.Equal(t, "api-key-val", got)
		})
	}
}

func TestResolveKey_HeaderResolvesWhenPresent(t *testing.T) {
	headerFn := func(name string) string {
		if name == "x-api-key" {
			return "my-key"
		}

		return ""
	}

	key := ratelimit.ResolveKey([]string{"header:x-api-key", "ip"}, "1.2.3.4", headerFn, noCookie, nil)

	assert.Equal(t, "my-key", key)
}

func TestResolveKey_CookieResolvesWhenPresent(t *testing.T) {
	cookieFn := func(name string) (string, bool) {
		if name == "session" {
			return "sess-abc", false
		}

		return "", false
	}

	key := ratelimit.ResolveKey([]string{"cookie:session", "ip"}, "1.2.3.4", noHeader, cookieFn, nil)

	assert.Equal(t, "sess-abc", key)
}

// TestResolveKey_CookieCollisionFallsThrough pins the cookie-collision
// safety contract: RFC 6265 allows multiple cookies with the same
// name. An attacker who can inject a Cookie header (XSS, plain HTTP
// where the gateway sits behind a TLS terminator that does not strip
// CL spoof) can ship `Cookie: session=victim_id; session=attacker_id`
// to make the gateway either bucket two distinct identities under one
// key (allowing a quota share/overflow) or flip-flop randomly between
// them as Cookie-parser implementations differ.
//
// The fix: on collision, the cookie strategy is treated as
// unresolvable and the resolver falls through to the next keyBy
// candidate. A counter bump surfaces the event for operator
// observability so an unusual rate of collision-detected requests
// triggers investigation.
func TestResolveKey_CookieCollisionFallsThrough(t *testing.T) {
	before := ratelimit.CookieCollisionCount()

	cookieFn := func(name string) (string, bool) {
		if name == "session" {
			return "victim_id", true
		}

		return "", false
	}

	key := ratelimit.ResolveKey([]string{"cookie:session", "ip"}, "1.2.3.4", noHeader, cookieFn, nil)

	assert.Equal(t, "1.2.3.4", key,
		"collision must skip the cookie strategy and fall through to the next keyBy entry")
	assert.Equal(t, before+1, ratelimit.CookieCollisionCount(),
		"each collision must bump the ratelimit_cookie_collision counter so the event reaches metrics")
}

// TestResolveKey_CookieCollisionWithoutFallback_StaysAtIPDefault pins
// the behaviour when collision happens and no further keyBy candidate
// can resolve. The function falls back to the clientIP default — same
// as if every keyBy had been a miss.
func TestResolveKey_CookieCollisionWithoutFallback_StaysAtIPDefault(t *testing.T) {
	cookieFn := func(name string) (string, bool) {
		return "victim_id", true
	}

	key := ratelimit.ResolveKey([]string{"cookie:session"}, "1.2.3.4", noHeader, cookieFn, nil)

	assert.Equal(t, "1.2.3.4", key,
		"a collision with no fallback keyBy still resolves to the clientIP default")
}

func TestResolveKey_UserFieldResolvesFromClaims(t *testing.T) {
	claims := map[string]any{"id": "user-123"}

	key := ratelimit.ResolveKey([]string{"user:id", "ip"}, "1.2.3.4", noHeader, noCookie, claims)

	assert.Equal(t, "user-123", key)
}

func TestResolveKey_FallsBackToIPWhenNothingResolves(t *testing.T) {
	key := ratelimit.ResolveKey([]string{"user:id", "header:x-api-key"}, "1.2.3.4", noHeader, noCookie, nil)

	assert.Equal(t, "1.2.3.4", key)
}

func TestResolveKey_PriorityChainStopsAtFirstMatch(t *testing.T) {
	headerFn := func(name string) string {
		if name == "x-api-key" {
			return "api-key-val"
		}

		return ""
	}
	claims := map[string]any{"id": "user-123"}

	// user:id should win because it comes first in the chain
	key := ratelimit.ResolveKey([]string{"user:id", "header:x-api-key", "ip"}, "1.2.3.4", headerFn, noCookie, claims)

	assert.Equal(t, "user-123", key)
}

// TestResolveKey_MapClaimIsDeterministic pins the fix for the
// fmt.Sprint nondeterminism: when a JWT claim is a map, the rendered
// rate-limit key MUST be stable across calls because Go map iteration
// order is randomized. Without deterministic encoding, the same user
// would land in different buckets across goroutines/pods and the
// configured rate would dilute or collide.
func TestResolveKey_MapClaimIsDeterministic(t *testing.T) {
	claims := map[string]any{
		"meta": map[string]any{
			"a": 1,
			"b": 2,
			"c": "three",
			"d": true,
		},
	}

	first := ratelimit.ResolveKey([]string{"user:meta"}, "1.2.3.4", noHeader, noCookie, claims)
	for i := 0; i < 100; i++ {
		got := ratelimit.ResolveKey([]string{"user:meta"}, "1.2.3.4", noHeader, noCookie, claims)
		assert.Equalf(t, first, got, "ResolveKey must be deterministic across runs (iteration %d)", i)
	}
}

// TestResolveKey_SliceClaimIsDeterministic mirrors the map case for
// array-shaped claims. JSON marshalling preserves slice order, but
// the assertion guards against a future regression that swaps in a
// formatter with non-deterministic output.
func TestResolveKey_SliceClaimIsDeterministic(t *testing.T) {
	claims := map[string]any{
		"roles": []any{"admin", "ops", "support"},
	}

	first := ratelimit.ResolveKey([]string{"user:roles"}, "1.2.3.4", noHeader, noCookie, claims)
	for i := 0; i < 50; i++ {
		got := ratelimit.ResolveKey([]string{"user:roles"}, "1.2.3.4", noHeader, noCookie, claims)
		assert.Equalf(t, first, got, "slice claim must render deterministically (iteration %d)", i)
	}
}

// TestResolveKey_AnyKeyMapClaimIsDeterministic pins the fix for the
// map[interface{}]interface{} branch. json.Marshal rejects such maps
// outright, so the previous code path fell back to fmt.Sprintf("%v",
// v). While Go 1.12+ sorts keys when printing maps with %v, the
// fallback is fragile across runtime versions and silently differs
// from the json.Marshal output that map[string]any uses. The expected
// behaviour is that a map[interface{}]interface{} payload produces
// the SAME key as the equivalent map[string]any — otherwise an
// upstream verifier swap silently re-buckets every user.
func TestResolveKey_AnyKeyMapClaimIsDeterministic(t *testing.T) {
	stringKeyed := map[string]any{
		"meta": map[string]any{"a": 1, "b": "two", "c": true},
	}
	anyKeyed := map[string]any{
		"meta": map[interface{}]interface{}{"a": 1, "b": "two", "c": true},
	}

	wantA := ratelimit.ResolveKey([]string{"user:meta"}, "1.2.3.4", noHeader, noCookie, stringKeyed)
	gotA := ratelimit.ResolveKey([]string{"user:meta"}, "1.2.3.4", noHeader, noCookie, anyKeyed)
	assert.Equal(t, wantA, gotA,
		"map[interface{}]interface{} must render to the same key as the equivalent map[string]any")

	// Stability across many runs guards against any future code path
	// that introduces iteration-order dependence.
	for i := 0; i < 200; i++ {
		got := ratelimit.ResolveKey([]string{"user:meta"}, "1.2.3.4", noHeader, noCookie, anyKeyed)
		assert.Equalf(t, gotA, got, "map[any]any claim must render deterministically (iteration %d)", i)
	}
}

// TestResolveKey_NestedAnyKeyMapInSliceIsDeterministic covers the
// recursive case: a JWT claim that is a slice carrying map[any]any
// elements must render stably and identically to the equivalent
// slice-of-map[string]any payload. JSON marshalling preserves slice
// order but cannot serialise map[any]any directly, so the
// stringifyClaim path must walk into the slice and rewrite each
// element before encoding.
func TestResolveKey_NestedAnyKeyMapInSliceIsDeterministic(t *testing.T) {
	stringKeyed := map[string]any{
		"audit": []any{
			map[string]any{"ip": "1.1.1.1", "ts": 1, "ok": true},
			map[string]any{"ip": "2.2.2.2", "ts": 2, "ok": false},
		},
	}
	anyKeyed := map[string]any{
		"audit": []any{
			map[interface{}]interface{}{"ip": "1.1.1.1", "ts": 1, "ok": true},
			map[interface{}]interface{}{"ip": "2.2.2.2", "ts": 2, "ok": false},
		},
	}

	want := ratelimit.ResolveKey([]string{"user:audit"}, "1.2.3.4", noHeader, noCookie, stringKeyed)
	got := ratelimit.ResolveKey([]string{"user:audit"}, "1.2.3.4", noHeader, noCookie, anyKeyed)
	assert.Equal(t, want, got,
		"slice carrying map[any]any must render to the same key as slice-of-map[string]any")
}

// TestResolveKey_ScalarClaimsRenderConsistently verifies the
// fast-path branches for primitive types still produce the wire shape
// callers expect — strings unchanged, numbers via fmt.Sprint.
func TestResolveKey_ScalarClaimsRenderConsistently(t *testing.T) {
	cases := []struct {
		name  string
		claim any
		want  string
	}{
		{"string", "user-123", "user-123"},
		{"int", 42, "42"},
		{"int64", int64(9000), "9000"},
		{"float", 3.5, "3.5"},
		{"bool", true, "true"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			claims := map[string]any{"id": c.claim}
			got := ratelimit.ResolveKey([]string{"user:id"}, "1.2.3.4", noHeader, noCookie, claims)
			assert.Equal(t, c.want, got)
		})
	}
}
