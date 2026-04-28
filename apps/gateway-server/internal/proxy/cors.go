package proxy

import (
	"strconv"
	"strings"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/registry"
)

// defaultExposedHeaders is the standard `Access-Control-Expose-Headers`
// list the gateway emits when a route's CORS config does not carry
// its own `ExposeHeaders` slice. Covers the headers the gateway itself
// stamps on every response, so cross-origin JavaScript can read
// correlators and rate-limit budgets without every operator having to
// remember to opt in.
//
// Comma-joined once at package init so the per-request write path is
// a single map assignment with no allocation.
const defaultExposedHeaders = "X-Request-Id, X-RateLimit-Limit, X-RateLimit-Remaining, X-RateLimit-Reset, Retry-After"

// MatchOrigin checks whether the request Origin is in the CORS
// allowed origins list. Returns the matched origin to echo back,
// or "" if no match. Wildcard "*" matches everything.
//
// A nil CORSMeta is treated as "no CORS configured" and returns "" so
// callers can use MatchOrigin defensively without a prior nil check.
func MatchOrigin(cors *registry.CORSMeta, origin string) string {
	if cors == nil {
		return ""
	}

	for _, allowed := range cors.Origins {
		if allowed == "*" {
			return "*"
		}

		if allowed == origin {
			return origin
		}
	}

	return ""
}

// BuildPreflightHeaders returns the full set of CORS headers for
// an OPTIONS preflight response (204 No Content).
//
// Returns nil when cors is nil so a misuse upstream (or a future
// refactor that drops the existing nil check at the call site)
// surfaces as a no-op rather than a nil-pointer dereference. Existing
// callers always nil-check before invoking; this is the second line
// of defense, not a substitute for the first.
//
// Vary: Origin is emitted unconditionally on every CORS response,
// regardless of whether the matched origin is "*" or an exact match.
// In a mixed deployment where one route serves wildcard CORS while
// another serves an allowlist on the same origin, an intermediate
// CDN that caches the preflight without seeing Vary would key on
// (URL, Method) alone and serve the wildcard preflight to a request
// that should have been pinned to its exact origin (or vice versa).
// Always-emit removes the entire class of CDN-cache-confusion bugs
// at zero wire cost.
func BuildPreflightHeaders(cors *registry.CORSMeta, matchedOrigin string) map[string]string {
	if cors == nil {
		return nil
	}

	h := make(map[string]string, 6)

	h["Access-Control-Allow-Origin"] = matchedOrigin

	if len(cors.Methods) > 0 {
		h["Access-Control-Allow-Methods"] = strings.Join(cors.Methods, ", ")
	}

	if len(cors.Headers) > 0 {
		h["Access-Control-Allow-Headers"] = strings.Join(cors.Headers, ", ")
	}

	// Defense-in-depth: browsers silently reject a response that pairs
	// Access-Control-Allow-Credentials: true with a wildcard origin, so
	// never emit the credentials header when echoing "*". The SDK
	// validator already refuses this combination at registration, but
	// operators who bypass validation or hand-write KV entries would
	// otherwise ship a broken CORS policy.
	if cors.Credentials && matchedOrigin != "*" {
		h["Access-Control-Allow-Credentials"] = "true"
	}

	if cors.MaxAge > 0 {
		h["Access-Control-Max-Age"] = strconv.Itoa(cors.MaxAge)
	}

	h["Vary"] = "Origin"

	return h
}

// BuildResponseCORSHeaders returns CORS headers for a regular
// (non-preflight) response. Only origin, credentials, expose-headers,
// and vary — methods/headers/max-age are preflight-only per the CORS
// spec.
//
// Access-Control-Expose-Headers is emitted on every response so
// cross-origin JavaScript can read gateway-stamped correlators
// (X-Request-Id) and rate-limit budget headers (X-RateLimit-*,
// Retry-After). When the route's CORSMeta.ExposeHeaders is set the
// gateway emits exactly that list; otherwise the standard gateway
// default list applies.
//
// Vary: Origin is emitted unconditionally on every CORS response,
// regardless of whether the matched origin is "*" or an exact match.
// See BuildPreflightHeaders for the CDN-cache-correctness rationale —
// it applies identically to non-preflight responses.
//
// Returns nil when cors is nil for the same defensive reason as
// BuildPreflightHeaders — see that function for details.
func BuildResponseCORSHeaders(cors *registry.CORSMeta, matchedOrigin string) map[string]string {
	if cors == nil {
		return nil
	}

	h := make(map[string]string, 4)

	h["Access-Control-Allow-Origin"] = matchedOrigin

	// See BuildPreflightHeaders for why "*" must never pair with the
	// credentials header; the same guard applies on every response.
	if cors.Credentials && matchedOrigin != "*" {
		h["Access-Control-Allow-Credentials"] = "true"
	}

	h["Access-Control-Expose-Headers"] = resolveExposeHeaders(cors.ExposeHeaders)

	h["Vary"] = "Origin"

	return h
}

// resolveExposeHeaders picks the Access-Control-Expose-Headers value
// the gateway should emit for a response. A non-empty per-route slice
// replaces the default list entirely (shallow replace, matching the
// other CORS fields' contract). Nil or empty falls back to the
// gateway's standard list.
func resolveExposeHeaders(configured []string) string {
	if len(configured) == 0 {
		return defaultExposedHeaders
	}

	return strings.Join(configured, ", ")
}
