package routing

import (
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/registry"
	"github.com/rs/zerolog"
)

// RouteDelta summarises the difference between two routing-table
// snapshots. Added holds routes present in the new slice but not in
// the old one; Removed holds the reverse. Modified holds human-readable
// descriptions of routes present in both slices but with changed config
// (e.g. "PUT /users/:id (cors, timeout)"). Routes present in both
// slices with identical config are counted by Unchanged only — they are
// not materialised because an operator reviewing logs after a production
// change cares about churn, not stability.
//
// Every slice is sorted lexicographically by "METHOD PATH" so log
// output is deterministic across rebuilds, which matters when
// diffing two log snapshots during an incident review.
type RouteDelta struct {
	Added     []Route
	Removed   []Route
	Modified  []string
	Unchanged int
}

// IsEmpty reports whether the delta represents a no-op rebuild —
// the new and old slices were identical in both membership and config.
// The caller uses this to suppress logging entirely for stable rebuilds
// so production log volume stays bounded when the KV bucket churns
// without actually changing the set of registered routes or their config.
func (d RouteDelta) IsEmpty() bool {
	return len(d.Added) == 0 && len(d.Removed) == 0 && len(d.Modified) == 0
}

// ComputeDelta returns the sorted delta between two route slices.
// Identity is determined by the (method, path-template) pair:
// changing a route's Subject alone is treated as an unchanged
// route because clients address routes by method+path, not by
// upstream subject. Config changes (CORS, RateLimit, Headers, Timeout,
// Auth) on an existing method+path key are reported in Modified.
//
// Pure function: both input slices are treated as immutable and
// the returned slices are freshly allocated. Safe to call
// concurrently.
func ComputeDelta(previous, next []Route) RouteDelta {
	prevByKey := make(map[string]Route, len(previous))
	for _, r := range previous {
		prevByKey[routeKey(r)] = r
	}

	nextByKey := make(map[string]Route, len(next))
	for _, r := range next {
		nextByKey[routeKey(r)] = r
	}

	var added []Route
	for key, route := range nextByKey {
		if _, existed := prevByKey[key]; !existed {
			added = append(added, route)
		}
	}

	var removed []Route
	for key, route := range prevByKey {
		if _, stillExists := nextByKey[key]; !stillExists {
			removed = append(removed, route)
		}
	}

	var modified []string
	unchanged := 0

	for key, newRoute := range nextByKey {
		oldRoute, existed := prevByKey[key]
		if !existed {
			continue // already counted as added
		}

		changes := diffRouteConfig(oldRoute, newRoute)
		if len(changes) > 0 {
			modified = append(modified, key+" ("+strings.Join(changes, ", ")+")")
		} else {
			unchanged++
		}
	}

	sortRoutes(added)
	sortRoutes(removed)
	sort.Strings(modified)

	return RouteDelta{
		Added:     added,
		Removed:   removed,
		Modified:  modified,
		Unchanged: unchanged,
	}
}

// LogInitialLoad emits a single INFO entry carrying the full set of
// routes that the gateway just hydrated for the first time. Called
// exactly once per process, from the first rebuild callback before
// the watcher starts streaming deltas. The full route list is
// intentionally dumped in one entry so a startup-time operator sees
// the complete topology in one log line rather than piecing it
// together from N per-route entries.
func LogInitialLoad(routes []Route, logger zerolog.Logger) {
	sorted := make([]Route, len(routes))
	copy(sorted, routes)
	sortRoutes(sorted)

	logger.Info().
		Int("count", len(sorted)).
		Array("routes", routesArray(sorted)).
		Msg("routing: initial route set published")
}

// LogDelta emits a log entry describing the transition from the
// previous route set to the new one. When the delta is empty (no
// additions, removals, or modifications) nothing is logged — silence
// signals stability. Real changes log at INFO so production operators
// see every churn event without needing to raise the log level.
//
// The full current route table is included in the log entry so
// operators can see the complete topology in a single entry after
// each change, without needing to reconstruct it from prior events.
func LogDelta(delta RouteDelta, allRoutes []Route, logger zerolog.Logger) {
	if delta.IsEmpty() {
		return
	}

	sorted := make([]Route, len(allRoutes))
	copy(sorted, allRoutes)
	sortRoutes(sorted)

	logger.Info().
		Int("added", len(delta.Added)).
		Int("removed", len(delta.Removed)).
		Int("modified", len(delta.Modified)).
		Int("unchanged", delta.Unchanged).
		Int("total", len(allRoutes)).
		Array("added_routes", routesArray(delta.Added)).
		Array("removed_routes", routesArray(delta.Removed)).
		Strs("modified_routes", delta.Modified).
		Array("routes", routesArray(sorted)).
		Msg("routing: table updated")
}

// routeKey produces the identity string used to match routes
// across rebuilds. Subject is deliberately excluded so upstream
// subject renames do not show up as a remove-then-add pair.
func routeKey(r Route) string {
	return r.Method + " " + r.PathTemplate
}

// sortRoutes sorts a slice of routes in place by method+path so
// log output is deterministic between rebuilds that happen to see
// Go map iteration produce a different order.
func sortRoutes(routes []Route) {
	sort.Slice(routes, func(i, j int) bool {
		return routeKey(routes[i]) < routeKey(routes[j])
	})
}

// routesArray adapts a []Route into a zerolog.LogArrayMarshaler so
// every route is emitted as a JSON object with method/path/subject
// and extended policy fields. Uses a zerolog Array so the resulting
// log entry is one structured field, not a string representation.
func routesArray(routes []Route) zerolog.LogArrayMarshaler {
	return &routeLogArray{routes: routes}
}

// routeLogArray is the zerolog.LogArrayMarshaler adapter for a
// slice of routes. Kept file-private and only constructed via
// routesArray — external consumers should never touch it.
type routeLogArray struct {
	routes []Route
}

// MarshalZerologArray implements zerolog.LogArrayMarshaler. Each
// route is written as a nested object so zerolog consumers can
// filter by "added_routes.path" or "removed_routes.subject"
// without parsing string fields. Extended policy fields (auth, cors,
// rateLimit, timeout) are included so operators can read the full
// effective config from a single log line.
func (a *routeLogArray) MarshalZerologArray(arr *zerolog.Array) {
	for _, route := range a.routes {
		d := zerolog.Dict().
			Str("method", route.Method).
			Str("path", route.PathTemplate).
			Str("subject", route.Subject).
			Bool("auth", route.Auth != nil).
			Bool("cors", route.CORS != nil)

		if route.RateLimit != nil {
			d = d.Str("rateLimit", strconv.Itoa(route.RateLimit.RPS)+" rps")
		}

		if route.Timeout > 0 {
			d = d.Str("timeout", route.Timeout.String())
		}

		arr.Dict(d)
	}
}

// diffRouteConfig returns a slice of human-readable field names that
// differ between prev and next for a route with the same method+path
// identity. Returns nil when the config is identical.
func diffRouteConfig(prev, next Route) []string {
	var changes []string

	if !corsEqual(prev.CORS, next.CORS) {
		changes = append(changes, "cors")
	}

	if !rateLimitEqual(prev.RateLimit, next.RateLimit) {
		changes = append(changes, "rateLimit")
	}

	if !headersEqual(prev.Headers, next.Headers) {
		changes = append(changes, "headers")
	}

	if prev.Timeout != next.Timeout {
		changes = append(changes, "timeout")
	}

	if !authEqual(prev.Auth, next.Auth) {
		changes = append(changes, "auth")
	}

	return changes
}

func corsEqual(a, b *registry.CORSMeta) bool {
	if a == nil && b == nil {
		return true
	}

	if a == nil || b == nil {
		return false
	}

	return reflect.DeepEqual(a, b)
}

func rateLimitEqual(a, b *registry.RateLimitMeta) bool {
	if a == nil && b == nil {
		return true
	}

	if a == nil || b == nil {
		return false
	}

	return reflect.DeepEqual(a, b)
}

func authEqual(a, b *RouteAuth) bool {
	if a == nil && b == nil {
		return true
	}

	if a == nil || b == nil {
		return false
	}

	return reflect.DeepEqual(a, b)
}

// headersEqual reports whether two header maps carry the same set of
// keys with the same values. A bare `b[k] != v` lookup wrongly treats
// a missing key as equal to an empty-string value because Go returns
// the zero value for absent map entries — which would silently mask a
// route swap from `{x: ""}` to `{y: ""}`. The two-value access via the
// ok flag closes that gap.
func headersEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}

	for k, v := range a {
		bv, ok := b[k]
		if !ok || bv != v {
			return false
		}
	}

	return true
}
