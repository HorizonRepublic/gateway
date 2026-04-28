package routing

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/registry"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestComputeDelta_EmptyToSome(t *testing.T) {
	next := []Route{
		{Subject: "svc.cmd.a", Method: "GET", PathTemplate: "/a"},
		{Subject: "svc.cmd.b", Method: "POST", PathTemplate: "/b"},
	}

	delta := ComputeDelta(nil, next)

	assert.Len(t, delta.Added, 2)
	assert.Empty(t, delta.Removed)
	assert.Equal(t, 0, delta.Unchanged)
	assert.False(t, delta.IsEmpty())
}

func TestComputeDelta_SomeToEmpty(t *testing.T) {
	previous := []Route{
		{Subject: "svc.cmd.a", Method: "GET", PathTemplate: "/a"},
		{Subject: "svc.cmd.b", Method: "POST", PathTemplate: "/b"},
	}

	delta := ComputeDelta(previous, nil)

	assert.Empty(t, delta.Added)
	assert.Len(t, delta.Removed, 2)
	assert.Equal(t, 0, delta.Unchanged)
	assert.False(t, delta.IsEmpty())
}

func TestComputeDelta_NoOp(t *testing.T) {
	routes := []Route{
		{Subject: "svc.cmd.a", Method: "GET", PathTemplate: "/a"},
		{Subject: "svc.cmd.b", Method: "POST", PathTemplate: "/b"},
	}

	delta := ComputeDelta(routes, routes)

	assert.Empty(t, delta.Added)
	assert.Empty(t, delta.Removed)
	assert.Empty(t, delta.Modified)
	assert.Equal(t, 2, delta.Unchanged)
	assert.True(t, delta.IsEmpty())
}

func TestComputeDelta_AddRemove(t *testing.T) {
	previous := []Route{
		{Subject: "svc.cmd.a", Method: "GET", PathTemplate: "/a"},
		{Subject: "svc.cmd.b", Method: "GET", PathTemplate: "/b"},
	}
	next := []Route{
		{Subject: "svc.cmd.b", Method: "GET", PathTemplate: "/b"},
		{Subject: "svc.cmd.c", Method: "GET", PathTemplate: "/c"},
	}

	delta := ComputeDelta(previous, next)

	require.Len(t, delta.Added, 1)
	assert.Equal(t, "/c", delta.Added[0].PathTemplate)

	require.Len(t, delta.Removed, 1)
	assert.Equal(t, "/a", delta.Removed[0].PathTemplate)

	assert.Equal(t, 1, delta.Unchanged)
	assert.False(t, delta.IsEmpty())
}

func TestComputeDelta_IgnoresSubjectRename(t *testing.T) {
	// Deliberate invariant: identity is (method, path) only. An
	// upstream subject rename must surface as zero churn so operators
	// are not paged over a purely internal refactor.
	previous := []Route{
		{Subject: "svc.cmd.a.old", Method: "GET", PathTemplate: "/a"},
	}
	next := []Route{
		{Subject: "svc.cmd.a.new", Method: "GET", PathTemplate: "/a"},
	}

	delta := ComputeDelta(previous, next)

	assert.Empty(t, delta.Added)
	assert.Empty(t, delta.Removed)
	assert.Empty(t, delta.Modified)
	assert.Equal(t, 1, delta.Unchanged)
	assert.True(t, delta.IsEmpty())
}

func TestComputeDelta_SortsDeterministically(t *testing.T) {
	previous := []Route{
		{Subject: "svc.cmd.z", Method: "GET", PathTemplate: "/z"},
		{Subject: "svc.cmd.y", Method: "GET", PathTemplate: "/y"},
		{Subject: "svc.cmd.x", Method: "GET", PathTemplate: "/x"},
	}
	next := []Route{
		{Subject: "svc.cmd.c", Method: "POST", PathTemplate: "/c"},
		{Subject: "svc.cmd.a", Method: "GET", PathTemplate: "/a"},
		{Subject: "svc.cmd.b", Method: "DELETE", PathTemplate: "/b"},
	}

	delta := ComputeDelta(previous, next)

	require.Len(t, delta.Added, 3)
	assert.Equal(t, "/b", delta.Added[0].PathTemplate)
	assert.Equal(t, "DELETE", delta.Added[0].Method)
	assert.Equal(t, "/a", delta.Added[1].PathTemplate)
	assert.Equal(t, "GET", delta.Added[1].Method)
	assert.Equal(t, "/c", delta.Added[2].PathTemplate)
	assert.Equal(t, "POST", delta.Added[2].Method)

	require.Len(t, delta.Removed, 3)
	assert.Equal(t, "/x", delta.Removed[0].PathTemplate)
	assert.Equal(t, "/y", delta.Removed[1].PathTemplate)
	assert.Equal(t, "/z", delta.Removed[2].PathTemplate)
}

func TestComputeDelta_DetectsModifiedCORS(t *testing.T) {
	previous := []Route{
		{
			Subject:      "svc.cmd.a",
			Method:       "GET",
			PathTemplate: "/a",
			CORS:         nil,
		},
	}
	next := []Route{
		{
			Subject:      "svc.cmd.a",
			Method:       "GET",
			PathTemplate: "/a",
			CORS:         &registry.CORSMeta{Origins: []string{"https://example.com"}},
		},
	}

	delta := ComputeDelta(previous, next)

	assert.Empty(t, delta.Added)
	assert.Empty(t, delta.Removed)
	require.Len(t, delta.Modified, 1)
	assert.Contains(t, delta.Modified[0], "GET /a")
	assert.Contains(t, delta.Modified[0], "cors")
	assert.Equal(t, 0, delta.Unchanged)
	assert.False(t, delta.IsEmpty())
}

func TestComputeDelta_DetectsMultipleFieldChanges(t *testing.T) {
	previous := []Route{
		{
			Subject:      "svc.cmd.users",
			Method:       "PUT",
			PathTemplate: "/users/:id",
		},
	}

	next := []Route{
		{
			Subject:      "svc.cmd.users",
			Method:       "PUT",
			PathTemplate: "/users/:id",
			CORS:         &registry.CORSMeta{Origins: []string{"*"}},
			Timeout:      5_000_000_000, // 5s
		},
	}

	delta := ComputeDelta(previous, next)

	require.Len(t, delta.Modified, 1)
	assert.Contains(t, delta.Modified[0], "cors")
	assert.Contains(t, delta.Modified[0], "timeout")
	assert.False(t, delta.IsEmpty())
}

func TestComputeDelta_ModifiedSortsDeterministically(t *testing.T) {
	previous := []Route{
		{Method: "GET", PathTemplate: "/z"},
		{Method: "GET", PathTemplate: "/a"},
	}
	next := []Route{
		{Method: "GET", PathTemplate: "/z", Timeout: 1_000_000_000},
		{Method: "GET", PathTemplate: "/a", Timeout: 1_000_000_000},
	}

	delta := ComputeDelta(previous, next)

	require.Len(t, delta.Modified, 2)
	assert.Less(t, delta.Modified[0], delta.Modified[1])
}

// TestHeadersEqual_DistinctKeysWithEmptyValuesAreNotEqual pins the
// fix for the false-positive in the previous bare lookup: when the
// two maps carry the same length but different key sets and one of
// the values is the empty string, `b[k]` returned the zero value and
// the loop wrongly accepted them as equal. The two-value access via
// the ok flag closes that gap and ensures key-set differences are
// always detected.
func TestHeadersEqual_DistinctKeysWithEmptyValuesAreNotEqual(t *testing.T) {
	a := map[string]string{"x": ""}
	b := map[string]string{"y": ""}

	assert.False(t, headersEqual(a, b), "distinct key sets must not be equal")
	assert.False(t, headersEqual(b, a), "comparison must be symmetric")
}

// TestHeadersEqual_LengthMismatchShortCircuits asserts the cheap
// length precheck still wins on inputs of obviously different size.
func TestHeadersEqual_LengthMismatchShortCircuits(t *testing.T) {
	a := map[string]string{"x": "1"}
	b := map[string]string{"x": "1", "y": "2"}

	assert.False(t, headersEqual(a, b), "different-length maps cannot match")
}

// TestHeadersEqual_IdenticalMapsAreEqual is the happy-path baseline:
// same keys with same values must be reported as equal even after the
// safer two-value lookup is in place.
func TestHeadersEqual_IdenticalMapsAreEqual(t *testing.T) {
	a := map[string]string{"x-debug": "1", "x-trace": "abc"}
	b := map[string]string{"x-debug": "1", "x-trace": "abc"}

	assert.True(t, headersEqual(a, b))
	assert.True(t, headersEqual(map[string]string{}, map[string]string{}))
	assert.True(t, headersEqual(nil, nil))
}

func TestRouteDelta_IsEmpty_TrueWhenBothZero(t *testing.T) {
	delta := RouteDelta{Unchanged: 42}
	assert.True(t, delta.IsEmpty())
}

func TestRouteDelta_IsEmpty_FalseWhenAdded(t *testing.T) {
	delta := RouteDelta{
		Added: []Route{{Method: "GET", PathTemplate: "/new"}},
	}
	assert.False(t, delta.IsEmpty())
}

func TestRouteDelta_IsEmpty_FalseWhenRemoved(t *testing.T) {
	delta := RouteDelta{
		Removed: []Route{{Method: "GET", PathTemplate: "/gone"}},
	}
	assert.False(t, delta.IsEmpty())
}

func TestRouteDelta_IsEmpty_FalseWhenOnlyModified(t *testing.T) {
	delta := RouteDelta{
		Modified:  []string{"GET /a (cors)"},
		Unchanged: 5,
	}
	assert.False(t, delta.IsEmpty())
}

func TestLogInitialLoad_DoesNotPanic(t *testing.T) {
	routes := []Route{
		{Subject: "svc.cmd.a", Method: "GET", PathTemplate: "/a"},
		{Subject: "svc.cmd.b", Method: "POST", PathTemplate: "/b"},
	}

	assert.NotPanics(t, func() {
		LogInitialLoad(routes, zerolog.Nop())
	})
}

func TestLogInitialLoad_IncludesAllRoutes(t *testing.T) {
	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	routes := []Route{
		{Subject: "svc.cmd.users.list", Method: "GET", PathTemplate: "/users"},
		{Subject: "svc.cmd.users.create", Method: "POST", PathTemplate: "/users"},
	}

	LogInitialLoad(routes, logger)

	var entry map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(buf.Bytes(), &entry))

	var count int
	require.NoError(t, json.Unmarshal(entry["count"], &count))
	assert.Equal(t, 2, count)

	var routesArr []map[string]any
	require.NoError(t, json.Unmarshal(entry["routes"], &routesArr))
	require.Len(t, routesArr, 2)

	// LogInitialLoad sorts by method+path, so GET /users comes first.
	assert.Equal(t, "GET", routesArr[0]["method"])
	assert.Equal(t, "/users", routesArr[0]["path"])
	assert.Equal(t, "svc.cmd.users.list", routesArr[0]["subject"])

	assert.Equal(t, "POST", routesArr[1]["method"])
	assert.Equal(t, "/users", routesArr[1]["path"])
	assert.Equal(t, "svc.cmd.users.create", routesArr[1]["subject"])
}

func TestLogDelta_DoesNotPanic(t *testing.T) {
	allRoutes := []Route{
		{Subject: "svc.cmd.new", Method: "GET", PathTemplate: "/new"},
		{Subject: "svc.cmd.kept", Method: "POST", PathTemplate: "/kept"},
	}

	delta := RouteDelta{
		Added: []Route{
			{Subject: "svc.cmd.new", Method: "GET", PathTemplate: "/new"},
		},
		Removed: []Route{
			{Subject: "svc.cmd.old", Method: "GET", PathTemplate: "/old"},
		},
		Unchanged: 3,
	}

	assert.NotPanics(t, func() {
		LogDelta(delta, allRoutes, zerolog.Nop())
	})
}

func TestLogDelta_SilentWhenEmpty(t *testing.T) {
	// Given: a buffer-backed logger so we can assert nothing was written.
	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	// When: LogDelta is called with an empty delta.
	LogDelta(RouteDelta{Unchanged: 5}, nil, logger)

	// Then: no output is produced — silence signals stability.
	assert.Empty(t, buf.String())
}
