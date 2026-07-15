package routing

// Table is the narrow contract the proxy layer depends on to resolve an
// incoming HTTP request to a Route.
//
// The interface is intentionally minimal: two methods, no lifecycle, no
// mutation. All construction happens in BuildTableFromRoutes; once
// built, a Table is immutable from the caller's perspective and MUST
// be published atomically (e.g. via atomic.Pointer) rather than
// mutated in place. This keeps the matching hot path lock-free and
// makes swapping the default indexedTable implementation a one-file
// change.
type Table interface {
	// Lookup resolves (method, path) to a Route. On a hit it returns
	// the matched Route and the extracted path parameters (nil map
	// if the template has no `:param` placeholders). On a miss it
	// returns the zero Route, a nil params map, and ok=false. Callers
	// MUST NOT mutate the returned params map — it is owned by the
	// caller for the lifetime of the request only.
	//
	// When several registered templates match the same request, the
	// winner is deterministic: at each segment position, left to
	// right, a static segment outranks a `:param` segment; remaining
	// ties break on the template string and then the NATS subject.
	// The result never depends on registration order, so every
	// gateway pod resolves the same request to the same route.
	Lookup(method, path string) (route Route, params map[string]string, ok bool)

	// Methods returns the set of HTTP verbs registered for the given
	// path. It is used by the transport layer to populate the `Allow`
	// header on 405 Method Not Allowed responses.
	//
	// IMPORTANT: the current implementation matches `path` against
	// stored templates by exact string equality, not by template
	// matching. This is sufficient for static templates ("/users"),
	// where the router path and the registered template coincide, but
	// it is a simplification: routes with parameters
	// ("/users/:id") will not be listed here when the caller passes a
	// concrete request path ("/users/42"). The proxy layer currently
	// only calls Methods for static paths, so this limitation is
	// acceptable; it will be revisited alongside any future
	// implementation swap.
	Methods(path string) []string

	// Routes returns every route in the table as an immutable
	// snapshot, ordered deterministically by (method, path template,
	// subject). It backs the operator admin introspection surface —
	// answering "what routing table is this pod actually serving?" —
	// and is never called on the request hot path. Callers MUST NOT
	// mutate the returned slice or its elements.
	Routes() []Route
}
