package ratelimit

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHashKey_FixedLengthAndDeterministic(t *testing.T) {
	a1 := hashKey("GET:/users/:id")
	a2 := hashKey("GET:/users/:id")
	b := hashKey("POST:/users")

	assert.Len(t, a1, 13)
	assert.Equal(t, a1, a2)
	assert.NotEqual(t, a1, b)
	assert.Regexp(t, `^[a-z2-7]{13}$`, a1)
}

// TestBuildBucketKey_MatchesUncachedComposition pins the memoized
// fast path against the canonical schema: the cached pathTemplate
// digest and the buffer-composed output MUST be byte-identical to the
// naive method + "." + hashKey(template) + "." + hashKey(resolved)
// composition, across repeated calls and distinct templates. Bucket
// identity is a cross-backend migration contract — a drifted key
// silently splits a client's GCRA state.
func TestBuildBucketKey_MatchesUncachedComposition(t *testing.T) {
	cases := []struct{ method, template, resolved string }{
		{"GET", "/users/:id", "203.0.113.7"},
		{"GET", "/users/:id", "203.0.113.8"},
		{"POST", "/users/:id", "203.0.113.7"},
		{"GET", "/orders/:id/items", "user-42"},
	}
	for _, tc := range cases {
		want := tc.method + "." + hashKey(tc.template) + "." + hashKey(tc.resolved)
		for i := 0; i < 3; i++ {
			assert.Equalf(t, want, BuildBucketKey(tc.method, tc.template, tc.resolved),
				"call %d for (%s %s %s)", i+1, tc.method, tc.template, tc.resolved)
		}
	}
}

// TestStringifyClaim_JSONMarshalErrorFallsBackDeterministically pins
// the lossy-fallback path for claim shapes json.Marshal rejects (func,
// chan, complex). Every iteration MUST produce the exact same string —
// the fallback descriptor is type-only, so randomised map iteration
// or pointer addresses cannot leak into the rate-limit bucket key —
// and the claim_nondeterministic counter MUST tick on every call so
// operators see the upstream-verifier misconfiguration through metrics.
func TestStringifyClaim_JSONMarshalErrorFallsBackDeterministically(t *testing.T) {
	before := claimNondeterministicCount.Load()

	// A function value cannot be JSON-marshalled (encoding/json
	// returns "json: unsupported type: func()"). The fallback must
	// be deterministic across iterations.
	noop := func() {}

	first := stringifyClaim(noop)
	require.NotEmpty(t, first, "fallback descriptor must produce a non-empty key fragment")

	const iterations = 100
	for i := 0; i < iterations; i++ {
		got := stringifyClaim(noop)
		assert.Equalf(t, first, got, "stringifyClaim must be deterministic (iteration %d)", i)
	}

	after := claimNondeterministicCount.Load()
	assert.Equal(t, before+1+uint64(iterations), after,
		"every fallback path entry must bump claim_nondeterministic_total")
}

// TestFallbackClaimDescriptor_TypeAndCardinality exercises the helper
// directly. The descriptor commits to type + (where applicable)
// cardinality only — never to a per-instance shape — so two distinct
// claim values of the same type/length collapse onto the same bucket
// key rather than randomly fanning out into per-pointer buckets.
func TestFallbackClaimDescriptor_TypeAndCardinality(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{
			name: "map[interface{}]interface{} encodes type + length",
			in:   map[interface{}]interface{}{"a": 1, "b": 2},
			want: "map[interface {}]interface {}:2",
		},
		{
			name: "[]interface{} encodes type + length",
			in:   []interface{}{1, 2, 3},
			want: "[]interface {}:3",
		},
		{
			name: "chan int falls through to %T",
			in:   make(chan int),
			want: "chan int",
		},
		{
			name: "func() falls through to %T",
			in:   func() {},
			want: "func()",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, fallbackClaimDescriptor(tc.in))
		})
	}
}
