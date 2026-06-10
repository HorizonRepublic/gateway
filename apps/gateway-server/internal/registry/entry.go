// Package registry mirrors the nestjs-jetstream handler_registry NATS KV
// bucket into an in-memory snapshot. It is the single source of truth for
// the gateway's HTTP routing table — the routing layer reads entries, the
// proxy layer uses the stored metadata to build the NATS RPC subject, and
// the watcher keeps the snapshot in sync with KV changes as they happen.
//
// The KV contract is: keys formatted as "{service}.cmd.{pattern}", values
// JSON-encoded with an "http" sub-object when the handler is exposed to
// the gateway. Entries without an "http" field are pure-RPC handlers that
// remain invisible to the HTTP routing table but are still watched and
// stored for completeness.
package registry

// HTTPMeta is the HTTP-routing descriptor stored under "meta.http" (wire
// shape) or "http" (at-rest KV value) in a handler_registry entry.
//
// This struct mirrors the IGatewayHttpMeta interface published by the
// gateway-sdk. Any field addition, rename, or removal is a breaking
// change for BOTH packages and requires a synchronized release. New
// optional fields may be added without a major version bump, but both
// sides must tolerate unknown fields gracefully (Go's encoding/json
// does this by default).
type HTTPMeta struct {
	// Method is the HTTP verb (GET, POST, PUT, PATCH, DELETE, HEAD, OPTIONS)
	// the gateway accepts for this handler.
	Method string `json:"method"`

	// Path is the URL path template with `:param` placeholders.
	// Example: "/users/:id".
	Path string `json:"path"`

	// StatusCode is the HTTP status returned on a successful reply. When
	// nil, the gateway applies the default rules: 200 for non-null body,
	// 204 for void. Stored as a pointer so the zero value is distinguishable
	// from an explicit 0 (which no HTTP implementation accepts anyway, but
	// the pointer keeps the JSON omitempty semantics clean).
	StatusCode *int `json:"statusCode,omitempty"`
}

// RouteAuthMeta is the auth descriptor stored under "auth" on a route
// entry in the handler_registry KV bucket. Present when the route was
// declared with a non-null `auth` block on the TypeScript side.
//
// Zero-valued Verifier means "use the default verifier". Optional is
// the route-level opt-in for "anonymous proceed" behavior: when true,
// the gateway still calls the verifier but treats a 401 reply as
// "proceed anonymously" rather than short-circuiting. 403 and transport
// errors still short-circuit regardless of this flag.
type RouteAuthMeta struct {
	// Verifier is the id of the verifier the gateway must invoke
	// before forwarding this route. Empty string selects the default
	// verifier, resolved at routing-table build time.
	Verifier string `json:"verifier"`

	// Optional relaxes the 401 short-circuit on missing or invalid
	// credentials so the main handler still runs, with a nil auth
	// blob in the envelope. Handlers that opt into this MUST treat
	// the `@GatewayUser()` parameter as possibly undefined.
	Optional bool `json:"optional"`
}

// VerifierMeta is the discriminator stored under "verifier" on a
// standalone verifier entry in the handler_registry KV bucket.
// Present when the entry was registered via `@GatewayAuthVerifier`
// on the TypeScript side.
//
// Route and verifier entries share the same bucket but never carry
// both fields on the same entry — the gateway discriminates by the
// HTTP and Verifier field presence at parse time. An entry with
// neither field is a pure-RPC handler that neither layer reads
// from.
type VerifierMeta struct {
	// ID is the logical handle routes reference via
	// `auth: { verifier: '<id>' }`. Uniqueness is operator
	// responsibility; collisions resolve to first-match by the
	// lexicographically-smallest KV key so behavior is deterministic
	// across gateway pods without coordination.
	ID string `json:"id"`

	// Default marks this verifier as the fallback for routes that
	// declare `auth` without naming a verifier explicitly. At most
	// one verifier in the bucket should set this; collisions are
	// logged as ERROR at routing-table build time.
	Default bool `json:"default"`
}

// CORSMeta holds the CORS policy for a route, written by the SDK.
// The gateway uses it to handle OPTIONS preflight and set response
// headers without a NATS round-trip.
//
// Mirrors IGatewayCorsConfig in the gateway-sdk
// (libs/gateway-sdk/src/types/gateway-cors-config.interface.ts).
// Field additions, renames, or removals require a synchronized
// release of both sides.
type CORSMeta struct {
	Origins     []string `json:"origins"`
	Methods     []string `json:"methods,omitempty"`
	Headers     []string `json:"headers,omitempty"`
	Credentials bool     `json:"credentials,omitempty"`
	MaxAge      int      `json:"maxAge,omitempty"`
	// ExposeHeaders is the per-route override for
	// `Access-Control-Expose-Headers`. When nil or empty the gateway
	// emits its standard list of gateway-stamped headers
	// (`X-Request-Id`, `X-RateLimit-*`, `Retry-After`) so
	// cross-origin JavaScript can always read them. A non-empty
	// slice replaces that default entirely — shallow-replace
	// semantics, matching the other CORS fields.
	ExposeHeaders []string `json:"exposeHeaders,omitempty"`
}

// RateLimitMeta holds the rate-limiting policy for a route.
//
// FailPolicy selects the per-route behaviour when the rate-limit store
// itself fails: "open" allows, "closed" rejects. Empty inherits the
// gateway-wide RATELIMIT_FAIL_POLICY default. Any other value is reset
// to empty (inherit) by the routing builder's sanitisation pass.
type RateLimitMeta struct {
	RPS        int      `json:"rps"`
	Burst      int      `json:"burst,omitempty"`
	KeyBy      []string `json:"keyBy,omitempty"`
	Store      string   `json:"store,omitempty"`
	FailPolicy string   `json:"failPolicy,omitempty"`
}

// HandlerEntry is a single deserialized record from the handler_registry
// KV bucket.
//
// Entries without an HTTP, Auth, or Verifier field represent pure-RPC
// handlers that the gateway does not expose. The watcher still stores
// them (so future features — health checks, debug dashboards, service
// discovery — can read arbitrary metadata) but both the routing table
// build step and the verifier registry build step skip them.
//
// Unknown JSON fields in the KV value are silently ignored by Go's
// default json unmarshal behavior. This is intentional: it keeps the
// gateway forward-compatible with future nestjs-jetstream metadata
// extensions (rate-limit rules, schema references, etc.) without
// requiring a gateway upgrade in lockstep.
type HandlerEntry struct {
	HTTP      *HTTPMeta         `json:"http,omitempty"`
	Auth      *RouteAuthMeta    `json:"auth,omitempty"`
	Verifier  *VerifierMeta     `json:"verifier,omitempty"`
	CORS      *CORSMeta         `json:"cors,omitempty"`
	RateLimit *RateLimitMeta    `json:"rateLimit,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	Timeout   *int              `json:"timeout,omitempty"`
}
