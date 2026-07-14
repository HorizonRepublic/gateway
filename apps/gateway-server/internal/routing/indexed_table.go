package routing

import (
	"slices"
	"strings"
)

// maxStackPathSegments caps the stack-resident segment buffer Lookup
// uses to split the request path. Paths deeper than this fall back to
// a heap-grown slice, trading one allocation for correctness on
// pathological inputs; realistic API paths stay well below the cap.
const maxStackPathSegments = 32

// templateSegment is one pre-compiled segment of a route's path
// template. For a static segment, literal holds the exact text that a
// request path segment must equal. For a parameter segment, literal
// holds the parameter name without the leading ':' and any request
// path segment matches.
type templateSegment struct {
	literal string
	param   bool
}

// compiledRoute pairs a Route with its template pre-split into
// segments at table-build time, so Lookup never re-parses the
// immutable template on the per-request hot path. paramCount sizes the
// extracted-params map exactly, avoiding both over-allocation and
// growth reallocation during extraction.
type compiledRoute struct {
	route      Route
	segments   []templateSegment
	paramCount int
}

// bucketKey addresses the candidate set for one (HTTP method, path
// segment count) pair. Both dimensions are exact-match filters: a
// request can only ever match templates that share its method and its
// segment count, so everything else is pruned by a single map access.
type bucketKey struct {
	method   string
	segments int
}

// bucket holds the candidate routes for one bucketKey, partitioned by
// the kind of their first template segment:
//
//   - staticFirst indexes candidates whose first segment is static,
//     keyed by that literal — one more map access narrows the scan to
//     routes sharing the request's first path segment.
//   - paramFirst lists candidates whose first segment is a parameter,
//     plus zero-segment templates ("/"), which have no first segment
//     to index on.
//
// Both candidate lists are kept sorted by template precedence (see
// compareTemplates), so the first match found is the deterministic
// winner. Checking staticFirst before paramFirst is itself the
// static-over-param rule applied to the first segment.
type bucket struct {
	staticFirst map[string][]compiledRoute
	paramFirst  []compiledRoute
}

// indexedTable resolves (method, path) via two map hops — bucketKey,
// then first static segment — followed by a scan over the few
// candidates that survive both filters. A registry of 1000 routes
// resolves in the same sub-microsecond budget as a registry of 10,
// because the scan length depends on template overlap, not on registry
// size. Benchmark numbers live in table_bench_test.go.
//
// Overlapping templates resolve deterministically: at each segment
// position, a static segment outranks a parameter segment; remaining
// ties break on the template string and then the NATS subject. Every
// gateway pod therefore picks the same route for the same request
// regardless of the map-iteration order its table was built in.
//
// Concurrency: safe for concurrent readers after construction. Callers
// MUST publish a fully-built table atomically (e.g. via
// atomic.Pointer) rather than mutating an existing one. The add method
// is package-private and only used by BuildTableFromRoutes, which
// constructs the table once and never exposes a mid-build view.
type indexedTable struct {
	// routes preserves the insertion snapshot for Methods, which
	// answers by exact template equality and is off the hot path.
	routes  []Route
	buckets map[bucketKey]*bucket
}

// Compile-time assertion that indexedTable satisfies the Table
// contract. Adding a new method to Table will fail the build here
// before any downstream caller even references the routing package,
// making the interface an enforced contract rather than a runtime
// assumption.
var _ Table = (*indexedTable)(nil)

// newIndexedTable returns an empty indexedTable ready for
// BuildTableFromRoutes to populate via add. The returned value is not
// safe to share across goroutines until construction completes.
func newIndexedTable() *indexedTable {
	return &indexedTable{
		routes:  make([]Route, 0, 16),
		buckets: make(map[bucketKey]*bucket, 16),
	}
}

// add compiles a Route and inserts it into its bucket at the position
// template precedence dictates, so candidate lists are always sorted
// and Lookup can return the first match it finds. It is
// package-private because callers must go through BuildTableFromRoutes
// — manual mutation would violate the "publish atomically, never
// mutate in place" invariant documented on indexedTable.
func (t *indexedTable) add(route Route) {
	t.routes = append(t.routes, route)

	compiled := compileRoute(route)
	key := bucketKey{method: route.Method, segments: len(compiled.segments)}

	b := t.buckets[key]
	if b == nil {
		b = &bucket{}
		t.buckets[key] = b
	}

	if len(compiled.segments) > 0 && !compiled.segments[0].param {
		if b.staticFirst == nil {
			b.staticFirst = make(map[string][]compiledRoute)
		}

		first := compiled.segments[0].literal
		b.staticFirst[first] = insertByPrecedence(b.staticFirst[first], compiled)

		return
	}

	b.paramFirst = insertByPrecedence(b.paramFirst, compiled)
}

// Lookup splits the request path once into a stack-resident segment
// buffer, addresses the (method, segment count) bucket, and scans the
// static-first candidates for the request's first segment before the
// param-first candidates. The first match wins and is deterministic
// because candidate lists are precedence-sorted at build time.
//
// Allocation profile: 0 allocs for a static-template match or a miss;
// exactly 1 alloc (the params map) for a parameterized match. Pinned
// by TestLookup_AllocationProfile.
func (t *indexedTable) Lookup(method, path string) (Route, map[string]string, bool) {
	var stack [maxStackPathSegments]string
	segments := appendPathSegments(stack[:0], path)

	b, ok := t.buckets[bucketKey{method: method, segments: len(segments)}]
	if !ok {
		return Route{}, nil, false
	}

	if len(segments) > 0 && b.staticFirst != nil {
		if route, params, ok := matchCandidates(b.staticFirst[segments[0]], segments); ok {
			return route, params, true
		}
	}

	return matchCandidates(b.paramFirst, segments)
}

// Methods returns the verbs registered for an EXACT template match.
// See the Table interface godoc for the rationale behind the exact-
// match simplification and its acceptable scope for the MVP. The
// linear scan is fine here: Methods only runs on the 405 error path,
// never on the per-request hot path.
func (t *indexedTable) Methods(path string) []string {
	var methods []string
	for i := range t.routes {
		if t.routes[i].PathTemplate == path {
			methods = append(methods, t.routes[i].Method)
		}
	}

	return methods
}

// compileRoute pre-splits a route's path template into
// templateSegments and counts its parameters. Runs once per route at
// table-build time; the result is immutable alongside the table.
func compileRoute(route Route) compiledRoute {
	raw := splitPath(route.PathTemplate)

	segments := make([]templateSegment, len(raw))
	paramCount := 0
	for i, seg := range raw {
		if strings.HasPrefix(seg, ":") {
			segments[i] = templateSegment{literal: seg[1:], param: true}
			paramCount++

			continue
		}

		segments[i] = templateSegment{literal: seg}
	}

	return compiledRoute{route: route, segments: segments, paramCount: paramCount}
}

// insertByPrecedence inserts a compiled route into an already-sorted
// candidate list at the position compareTemplates dictates. Insertion
// sort keeps add free of a separate finalize step, so a table is valid
// for Lookup after every add — the same always-consistent property the
// previous append-only implementation had.
func insertByPrecedence(list []compiledRoute, compiled compiledRoute) []compiledRoute {
	idx, _ := slices.BinarySearchFunc(list, compiled, compareTemplates)

	return slices.Insert(list, idx, compiled)
}

// compareTemplates orders two compiled templates by match precedence:
//
//  1. Segment-by-segment, left to right: a static segment outranks a
//     parameter segment; two static segments order lexicographically;
//     two parameter segments tie regardless of name.
//  2. Fewer segments first (only reachable when comparing across
//     lengths; buckets group by count, so in-bucket lists never hit it).
//  3. Template string, then NATS subject — total-order tiebreakers
//     that keep the sort deterministic even for pathological
//     registries carrying duplicate (method, template) pairs.
//
// The ordering is a total order independent of insertion order, which
// is what makes route resolution identical across gateway pods that
// built their tables from differently-ordered map iterations.
func compareTemplates(a, b compiledRoute) int {
	for i := 0; i < len(a.segments) && i < len(b.segments); i++ {
		segA, segB := a.segments[i], b.segments[i]
		if segA.param != segB.param {
			if segA.param {
				return 1
			}

			return -1
		}

		if !segA.param {
			if c := strings.Compare(segA.literal, segB.literal); c != 0 {
				return c
			}
		}
	}

	if c := len(a.segments) - len(b.segments); c != 0 {
		return c
	}

	if c := strings.Compare(a.route.PathTemplate, b.route.PathTemplate); c != 0 {
		return c
	}

	return strings.Compare(a.route.Subject, b.route.Subject)
}

// matchCandidates walks a precedence-sorted candidate list and returns
// the first template that matches the pre-split request path. The
// zero Route, nil params, and ok=false signal a miss.
func matchCandidates(candidates []compiledRoute, segments []string) (Route, map[string]string, bool) {
	for i := range candidates {
		if params, ok := matchCompiled(&candidates[i], segments); ok {
			return candidates[i].route, params, true
		}
	}

	return Route{}, nil, false
}

// matchCompiled compares one compiled template against the pre-split
// request path segments. Callers guarantee len(segments) equals the
// template's segment count (the bucketKey groups by count).
//
// Static segments require byte equality; parameter segments match any
// segment and record it under the parameter name. The params map is
// allocated lazily on the first parameter segment, sized exactly by
// the pre-computed paramCount, so static templates match with zero
// allocations and return a nil map.
func matchCompiled(candidate *compiledRoute, segments []string) (map[string]string, bool) {
	var params map[string]string
	for i := range candidate.segments {
		seg := &candidate.segments[i]
		if seg.param {
			if params == nil {
				params = make(map[string]string, candidate.paramCount)
			}
			params[seg.literal] = segments[i]

			continue
		}

		if seg.literal != segments[i] {
			return nil, false
		}
	}

	return params, true
}

// appendPathSegments appends the segments of a URL path to buf and
// returns the extended slice. Semantics are identical to splitPath —
// leading and trailing slashes are ignored, inner empty segments are
// preserved — but the segments land in a caller-provided buffer
// (typically stack-resident) instead of a freshly allocated slice, and
// each segment is a zero-copy substring of path. Pinned against
// splitPath by TestAppendPathSegments_MatchesSplitPath.
func appendPathSegments(buf []string, path string) []string {
	start := 0
	for start < len(path) && path[start] == '/' {
		start++
	}

	end := len(path)
	for end > start && path[end-1] == '/' {
		end--
	}

	for start < end {
		i := strings.IndexByte(path[start:end], '/')
		if i < 0 {
			return append(buf, path[start:end])
		}

		buf = append(buf, path[start:start+i])
		start += i + 1
	}

	return buf
}

// splitPath slices a URL path into its segments. Leading and trailing
// slashes are ignored so that "/users" and "/users/" yield the same
// segment list — this matches the convention used by Hertz and
// net/http routers. Allocates a fresh slice per call, so it is used at
// table-build time only; Lookup goes through appendPathSegments.
func splitPath(path string) []string {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "/")
}
