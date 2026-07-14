package routing

// Table is the narrow contract the proxy layer depends on to resolve an
// incoming HTTP request to a Route.
//
// The interface is intentionally minimal: two methods, no lifecycle, no
// mutation. All construction happens in BuildTableFromRoutes; once
// built, a Table is immutable from the caller's perspective and MUST
// be published atomically (e.g. via atomic.Pointer) rather than
// mutated in place. This keeps the matching hot path lock-free and
// makes the swap from the current linear-scan implementation to a
// future trie a one-file change.
type Table interface {
	// Lookup resolves (method, path) to a Route. On a hit it returns
	// the matched Route and the extracted path parameters (empty map
	// if the template has no `:param` placeholders). On a miss it
	// returns the zero Route, a nil params map, and ok=false. Callers
	// MUST NOT mutate the returned params map — it is owned by the
	// caller for the lifetime of the request only.
	Lookup(method, path string) (route Route, params map[string]string, ok bool)

	// Methods returns the set of HTTP verbs whose registered
	// templates match the given concrete request path, using the
	// same template semantics as Lookup ("/users/42" matches a
	// registered "/users/:id"). It is used by the proxy layer to
	// populate the `Allow` header on 405 Method Not Allowed
	// responses; the proxy calls it with every unmatched request
	// path, static or parameterized, so implementations MUST
	// template-match rather than compare template strings for
	// equality. Returned verbs are deduplicated.
	Methods(path string) []string
}
