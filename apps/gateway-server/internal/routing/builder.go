package routing

import (
	"slices"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/auth"
	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/registry"
)

// containsCRLF reports whether s contains any byte that would split
// an HTTP header line into two: CR (\r), LF (\n), or NUL (\x00). NUL
// is included because some HTTP intermediaries fold it into a line
// break, and trailing-NUL truncation in C-string consumers makes any
// embedded NUL a header-injection primitive.
//
// Returns true if any forbidden byte is present anywhere in s. Used
// by sanitizeHeaders and sanitizeCORS to drop registry-supplied values
// that would otherwise let an upstream service inject arbitrary HTTP
// headers (Set-Cookie hijack, cache poisoning, response splitting)
// through a single misconfigured route entry.
func containsCRLF(s string) bool {
	return strings.ContainsAny(s, "\r\n\x00")
}

// CollectRoutes projects a registry.Snapshot into a flat slice of
// HTTP-facing Route values. Routes that declare an `auth` block are
// resolved against the supplied VerifierRegistry; routes whose
// referenced verifier is missing (explicit id not found, or implicit
// default requested when no default verifier exists) are dropped
// from the output with a WARN log so the gateway never forwards
// unauthenticated requests to them.
//
// Skip policy:
//
//  1. Pure-RPC handlers (entry.HTTP == nil) are silently skipped,
//     same as before.
//  2. Malformed KV keys produce a WARN log and are skipped.
//  3. Routes whose auth block references an unknown verifier produce
//     a WARN log and are skipped. Once the verifier registers, the
//     next rebuild reinstates the route — this self-heals the cold-
//     boot race where a route entry lands in KV before its verifier.
//
// The returned slice is in Go-map iteration order, i.e. effectively
// random. Callers that need a deterministic order must sort it
// themselves — the routing table does not care, and the lifecycle
// logger sorts its own output explicitly.
func CollectRoutes(
	snapshot *registry.Snapshot,
	verifiers *auth.VerifierRegistry,
	logger zerolog.Logger,
) []Route {
	routes := make([]Route, 0, len(snapshot.Entries))

	for key, entry := range snapshot.Entries {
		if entry.HTTP == nil {
			continue
		}

		subject, err := registry.SubjectFromKey(key)
		if err != nil {
			logger.Warn().
				Err(err).
				Str("key", key).
				Msg("routing: skipping entry with malformed KV key")

			continue
		}

		route := Route{
			Subject:      subject,
			Method:       entry.HTTP.Method,
			PathTemplate: entry.HTTP.Path,
		}

		route.CORS = sanitizeCORS(entry.CORS, key, logger)
		route.RateLimit = sanitizeRateLimit(entry.RateLimit, key, logger)
		route.Headers = sanitizeHeaders(entry.Headers, key, logger)

		if entry.Timeout != nil {
			route.Timeout = time.Duration(*entry.Timeout) * time.Millisecond
		}

		if entry.Auth != nil {
			resolved, ok := resolveVerifier(entry.Auth, verifiers)
			if !ok {
				logger.Warn().
					Str("key", key).
					Str("verifier", entry.Auth.Verifier).
					Msg("routing: dropping route with unresolved verifier")

				continue
			}

			route.Auth = &RouteAuth{
				VerifierSubject: resolved,
				Optional:        entry.Auth.Optional,
			}
		}

		routes = append(routes, route)
	}

	return routes
}

// resolveVerifier looks up the NATS subject for the verifier a route
// references. An empty Verifier id means the route wants the default
// verifier — success requires one to exist in the registry.
func resolveVerifier(meta *registry.RouteAuthMeta, verifiers *auth.VerifierRegistry) (string, bool) {
	if meta.Verifier == "" {
		return verifiers.LookupDefault()
	}

	return verifiers.Lookup(meta.Verifier)
}

// sanitizeHeaders strips registry-supplied response-header entries
// whose name or value contains CR / LF / NUL bytes. Such bytes would
// terminate the current header line on the wire and let an upstream
// service inject arbitrary additional headers (cache-poisoning,
// Set-Cookie hijack, response splitting) through a single misconfigured
// route entry.
//
// Each offending entry is dropped individually with a WARN log; clean
// entries on the same route survive untouched. When every entry on a
// non-empty input is rejected the function returns nil so a fully
// malformed Headers map degrades to "no extra headers" instead of an
// empty map (downstream consumers treat the two equivalently, but nil
// avoids gratuitous allocation on the hot reload path).
//
// Header names themselves are NOT lowercased here — the routing layer
// preserves whatever casing the SDK emitted, and the HTTP adapter
// canonicalises on write. Validating CRLF here is purely a wire-safety
// check, not a normalisation step.
func sanitizeHeaders(h map[string]string, key string, logger zerolog.Logger) map[string]string {
	if len(h) == 0 {
		return h
	}

	out := make(map[string]string, len(h))
	for name, value := range h {
		if containsCRLF(name) || containsCRLF(value) {
			logger.Warn().
				Str("key", key).
				Str("header", name).
				Msg("routing: dropping header with CRLF/NUL injection attempt")

			continue
		}

		out[name] = value
	}

	if len(out) == 0 {
		return nil
	}

	return out
}

// sanitizeCORS enforces the Fetch Living Standard invariant that
// `Access-Control-Allow-Origin: *` is illegal when paired with
// `Access-Control-Allow-Credentials: true`, AND drops the entire CORS
// block on any CRLF / NUL injection in Origins, Methods, Headers, or
// ExposeHeaders.
//
// Browsers reject the wildcard+credentials combination silently, so
// emitting it would produce a "CORS is broken on this endpoint"
// symptom that only surfaces in browser consoles. The SDK layer
// validates this at registration time and should have caught any
// such configuration before it reached KV. This guard is
// defense-in-depth for cases where operators bypass the SDK (manual
// KV writes, alternative SDK implementations, schema drift).
//
// CRLF in any CORS string is treated as fail-closed: a partial CORS
// block (some origins valid, some invalid) is strictly worse than no
// CORS — surviving entries would still surface the CORS contract,
// hiding the misconfiguration behind apparently-working preflights
// while the malformed string lets an attacker inject arbitrary
// response headers on every preflight or actual request. Drop the
// entire block, log WARN once per route per reload, and let the
// route serve without CORS until the operator notices.
func sanitizeCORS(cors *registry.CORSMeta, key string, logger zerolog.Logger) *registry.CORSMeta {
	if cors == nil {
		return nil
	}

	if injectedField := corsInjectionField(cors); injectedField != "" {
		logger.Warn().
			Str("key", key).
			Str("field", injectedField).
			Msg("routing: dropping CORS block with CRLF/NUL injection attempt")

		return nil
	}

	if !cors.Credentials || !slices.Contains(cors.Origins, "*") {
		return cors
	}

	logger.Warn().
		Str("key", key).
		Strs("origins", cors.Origins).
		Msg("routing: dropping CORS block that combines origins:[*] with credentials:true (Fetch Living Standard violation)")

	return nil
}

// corsInjectionField reports the name of the first CORSMeta string
// field that contains a CRLF/NUL byte, or "" if every string is clean.
// Returning the field name (not just bool) lets the caller log a
// targeted WARN — operators can tell which side of their CORS config
// went malformed without grepping the bucket for the offending value.
//
// The check covers Origins, Methods, Headers, and ExposeHeaders. Any
// future []string field added to CORSMeta MUST also be checked here;
// scalar fields (Credentials, MaxAge) cannot carry CRLF by type.
func corsInjectionField(cors *registry.CORSMeta) string {
	checks := []struct {
		name   string
		values []string
	}{
		{"origins", cors.Origins},
		{"methods", cors.Methods},
		{"headers", cors.Headers},
		{"exposeHeaders", cors.ExposeHeaders},
	}

	for _, check := range checks {
		for _, value := range check.values {
			if containsCRLF(value) {
				return check.name
			}
		}
	}

	return ""
}

// sanitizeRateLimit enforces the invariant that a rate-limit block
// only ships with a positive RPS. The SDK's typia validator should
// reject rps <= 0 at module init, but that catches only legitimate
// SDK consumers. This guard is defense-in-depth for operators who
// hand-craft KV writes, run older SDK versions, or get bitten by
// schema drift — the proxy handler already interprets RPS <= 0 as
// "skip rate limiting" (fail-safe), but silently ignoring a
// misconfigured block hides it from operators who expect their limit
// to be active. Dropping the block to nil and logging WARN at build
// time surfaces the mistake once per route per reload instead of
// zero times.
//
// A negative Burst is similarly untrustworthy (GCRA.Check would
// either deny-all or allow-all depending on currentTAT) and causes
// the whole block to be dropped.
func sanitizeRateLimit(rl *registry.RateLimitMeta, key string, logger zerolog.Logger) *registry.RateLimitMeta {
	if rl == nil {
		return nil
	}

	if rl.RPS <= 0 {
		logger.Warn().
			Str("key", key).
			Int("rps", rl.RPS).
			Msg("routing: dropping rate-limit block with non-positive rps (SDK validation bypassed)")

		return nil
	}

	if rl.Burst < 0 {
		logger.Warn().
			Str("key", key).
			Int("burst", rl.Burst).
			Msg("routing: dropping rate-limit block with negative burst (SDK validation bypassed)")

		return nil
	}

	return rl
}

// BuildTableFromRoutes constructs a Table from a pre-collected slice
// of routes. It performs no snapshot access and emits no log output:
// callers (typically the lifecycle-aware rebuild closure in main.go)
// are responsible for having already logged and filtered the input.
// Keeping this step purely mechanical makes it trivial to reason
// about in tests and benchmarks.
func BuildTableFromRoutes(routes []Route) Table {
	table := newLinearTable()
	for _, route := range routes {
		table.add(route)
	}

	return table
}
