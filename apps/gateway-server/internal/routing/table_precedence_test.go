package routing

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLookup_StaticTemplateBeatsParamTemplate pins the segment-level
// precedence rule: when a static template and a parameterized template
// both match a request, the static one wins — in BOTH insertion
// orders, because the winner must not depend on the map-iteration
// order the table was built from (split-brain routing across pods).
func TestLookup_StaticTemplateBeatsParamTemplate(t *testing.T) {
	static := Route{Subject: "svc.cmd.users.me", Method: "GET", PathTemplate: "/users/me"}
	param := Route{Subject: "svc.cmd.users.get", Method: "GET", PathTemplate: "/users/:id"}

	orders := map[string][]Route{
		"static-first": {static, param},
		"param-first":  {param, static},
	}

	for name, routes := range orders {
		t.Run(name, func(t *testing.T) {
			table := BuildTableFromRoutes(routes)

			route, params, ok := table.Lookup("GET", "/users/me")
			require.True(t, ok)
			assert.Equal(t, static.Subject, route.Subject,
				"the static template must win over the parameterized one")
			assert.Nil(t, params)

			route, params, ok = table.Lookup("GET", "/users/42")
			require.True(t, ok)
			assert.Equal(t, param.Subject, route.Subject)
			assert.Equal(t, map[string]string{"id": "42"}, params)
		})
	}
}

// TestLookup_LeftmostStaticSegmentWins pins precedence for templates
// that diverge deeper than the first segment: /a/b/:y outranks /a/:x/c
// for the request /a/b/c because the earliest differing position (the
// second segment) is static in the winner.
func TestLookup_LeftmostStaticSegmentWins(t *testing.T) {
	deepParam := Route{Subject: "svc.cmd.a.x.c", Method: "GET", PathTemplate: "/a/:x/c"}
	midStatic := Route{Subject: "svc.cmd.a.b.y", Method: "GET", PathTemplate: "/a/b/:y"}

	for name, routes := range map[string][]Route{
		"mid-static-first": {midStatic, deepParam},
		"deep-param-first": {deepParam, midStatic},
	} {
		t.Run(name, func(t *testing.T) {
			table := BuildTableFromRoutes(routes)

			route, params, ok := table.Lookup("GET", "/a/b/c")
			require.True(t, ok)
			assert.Equal(t, midStatic.Subject, route.Subject,
				"the template whose leftmost differing segment is static must win")
			assert.Equal(t, map[string]string{"y": "c"}, params)
		})
	}
}

// TestLookup_RootTemplateMatchesRootPath pins the zero-segment case:
// a "/" template lands in the segment-count-0 bucket and matches "/",
// "" and "///" (all of which split to zero segments), and nothing
// deeper.
func TestLookup_RootTemplateMatchesRootPath(t *testing.T) {
	table := BuildTableFromRoutes([]Route{
		{Subject: "svc.cmd.root", Method: "GET", PathTemplate: "/"},
	})

	for _, path := range []string{"/", "", "///"} {
		route, params, ok := table.Lookup("GET", path)
		require.True(t, ok, "path %q must match the root template", path)
		assert.Equal(t, "svc.cmd.root", route.Subject)
		assert.Nil(t, params)
	}

	_, _, ok := table.Lookup("GET", "/users")
	assert.False(t, ok)
}

// TestLookup_TrailingSlashEquivalence pins the Hertz / net/http
// convention carried over from the previous implementation: "/users"
// and "/users/" resolve to the same route.
func TestLookup_TrailingSlashEquivalence(t *testing.T) {
	table := BuildTableFromRoutes([]Route{
		{Subject: "svc.cmd.users.list", Method: "GET", PathTemplate: "/users"},
	})

	for _, path := range []string{"/users", "/users/", "users"} {
		_, _, ok := table.Lookup("GET", path)
		assert.True(t, ok, "path %q must match the /users template", path)
	}
}

// TestLookup_PathDeeperThanStackBufferStillMatches exercises the
// heap-fallback branch of the stack-resident segment buffer: a path
// with more than maxStackPathSegments segments must still match its
// template instead of corrupting or truncating the segment list.
func TestLookup_PathDeeperThanStackBufferStillMatches(t *testing.T) {
	depth := maxStackPathSegments + 4

	template := ""
	path := ""
	for i := 0; i < depth; i++ {
		template += fmt.Sprintf("/s%d", i)
		path += fmt.Sprintf("/s%d", i)
	}
	template += "/:id"
	path += "/deep"

	table := BuildTableFromRoutes([]Route{
		{Subject: "svc.cmd.deep", Method: "GET", PathTemplate: template},
	})

	route, params, ok := table.Lookup("GET", path)
	require.True(t, ok)
	assert.Equal(t, "svc.cmd.deep", route.Subject)
	assert.Equal(t, map[string]string{"id": "deep"}, params)
}

// TestLookup_AllocationProfile pins the hot-path allocation contract
// documented on Lookup: 0 allocs for a static match and for a miss,
// exactly 1 alloc (the params map) for a parameterized match. A
// regression in any of these numbers is a hot-path budget change that
// must be made deliberately, not slipped in.
func TestLookup_AllocationProfile(t *testing.T) {
	table := buildBenchTable(100)

	cases := []struct {
		name   string
		path   string
		allocs float64
	}{
		{"static-hit", "/resource50/info", 0},
		{"param-hit", "/resource51/abc", 1},
		{"miss", "/absent/path", 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			allocs := testing.AllocsPerRun(100, func() {
				benchRoute, benchParams, benchOK = table.Lookup("GET", c.path)
			})
			assert.Equal(t, c.allocs, allocs)
		})
	}
}

// TestAppendPathSegments_MatchesSplitPath pins appendPathSegments to
// the exact semantics of splitPath across the same edge-case table
// TestSplitPath uses, so the two splitters cannot drift apart while
// one serves the build path and the other the lookup hot path.
func TestAppendPathSegments_MatchesSplitPath(t *testing.T) {
	inputs := []string{
		"",
		"/",
		"///",
		"/users",
		"/users/",
		"/users/42/items",
		"/users/42/",
		"users/42",
		"/foo//bar",
		"//foo/bar",
	}

	for _, in := range inputs {
		t.Run(fmt.Sprintf("%q", in), func(t *testing.T) {
			var buf [maxStackPathSegments]string
			got := appendPathSegments(buf[:0], in)

			want := splitPath(in)
			require.Len(t, got, len(want))
			for i := range want {
				assert.Equal(t, want[i], got[i])
			}
		})
	}
}
