package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/codec"
	gerrors "github.com/HorizonRepublic/gateway/apps/gateway-server/internal/errors"
	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/ratelimit"
	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/registry"
	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/routing"
)

// TableProvider returns the currently-active routing Table. The gateway
// rebuilds its Table on every registry change; the provider closure
// gives Handler an atomic view without requiring it to coordinate with
// the watcher directly.
//
// Implementations MUST return a non-nil Table. A nil return crashes
// the handler — an acceptable failure mode for a pure programming bug
// because it surfaces immediately instead of silently 404-ing.
type TableProvider func() routing.Table

// HandlerConfig bundles the dependencies of a Handler. Passed by value
// at construction; all fields are required (except RateLimiter and
// RateLimitTimeout) and the zero value of a HandlerConfig is NOT safe
// to use.
type HandlerConfig struct {
	Table   TableProvider
	Nats    NatsRequester
	Encoder Encoder
	Decoder Decoder
	Timeout time.Duration
	Logger  zerolog.Logger
	// RateLimiter is the per-route store router. nil = rate limiting
	// disabled globally for this handler. Backends are registered via
	// Router.EnsureBackend by the gateway bootstrap.
	RateLimiter *ratelimit.Router
	// RateLimitTimeout bounds the wall-clock budget for the rate-limit
	// gate on a single request. Separate from the route request timeout
	// so a hot-key CAS storm cannot burn through the upstream deadline
	// before the NATS round trip even starts. When zero, the gate falls
	// back to the route Timeout (equivalent to pre-S.2 behaviour) — this
	// keeps test harnesses that build HandlerConfig manually working,
	// but production bootstrap MUST set a distinct, shorter budget
	// (default 50ms per the cfg.RateLimitTimeout env knob).
	RateLimitTimeout time.Duration
}

// Handler is the HTTP→NATS→HTTP orchestrator. It owns one request from
// lookup to response write. All I/O dependencies are injected via
// HandlerConfig, so Handle is trivially unit-testable with fakes.
type Handler struct {
	cfg HandlerConfig
	// claimsUnmarshalLogged dedupes the per-route
	// "ratelimit.claims.unmarshal_failed" WARN. Keys are
	// `<method>:<pathTemplate>`; LoadOrStore decides whether the
	// current goroutine emits the log line for that route. The
	// underlying counter (Router.RecordClaimsUnmarshalError) still
	// bumps per-request — only the log message is throttled so a
	// misbehaving verifier under sustained load cannot DoS the log
	// pipeline.
	claimsUnmarshalLogged sync.Map
}

// NewHandler constructs a Handler from the supplied configuration.
// The caller retains ownership of cfg.Logger — Handler clones it with
// no additional fields, so log entries carry whatever context the
// caller pre-configured.
func NewHandler(cfg HandlerConfig) *Handler {
	return &Handler{cfg: cfg}
}

// ServeInput is a framework-agnostic view of an incoming HTTP request.
// The Hertz (and any future) HTTP adapter populates this struct and
// calls Handle, which contains all lookup/encoding/decoding logic
// independent of any HTTP framework.
type ServeInput struct {
	Method      string
	Path        string
	Body        []byte
	Query       map[string]QueryValue
	Headers     map[string]string
	RequestID   string
	Traceparent string
	RemoteAddr  string
	ReceivedAt  int64
}

// ServeResult is the framework-agnostic outcome of handling a request.
// Adapters translate it into their HTTP response representation.
//
// Headers is a multi-value map so the HTTP adapter can emit each
// slice entry as a separate header on the client response. This is
// the critical shape for Set-Cookie, where two cookies set by the
// same handler MUST land on the wire as two distinct Set-Cookie
// lines instead of a single joined value.
//
// GatewayOwnedBody marks responses whose Body is a pre-encoded
// `gerrors.*` payload produced by the gateway itself (404/502/503/
// 504/...), as opposed to a handler-thrown error that the upstream
// service serialized through its own exception filter. The HTTP
// adapter uses this flag to stamp `Cache-Control: no-store` on
// gateway-owned responses so intermediate caches never memoize a
// transient infrastructure failure; handler-thrown errors are left
// alone because cache policy for those is part of the application
// contract.
type ServeResult struct {
	Status           int
	Headers          map[string][]string
	Body             []byte
	GatewayOwnedBody bool
}

// Handle performs the full request lifecycle: route lookup, envelope
// encode, NATS request, reply decode, response construction. Errors
// are translated to the appropriate HTTP status with a pre-encoded
// JSON error body from the internal/errors package.
//
// The ctx argument is the inbound HTTP request's context. It is
// propagated into every blocking dependency call (rate-limit store,
// NATS round trip, verifier round trip) so a client disconnect or a
// caller-imposed deadline tears down in-flight work instead of letting
// it run to its independent timeout. Callers that have no parent
// context to pass (legacy entry points, fuzz tests) may use
// context.Background() — the per-route timeout still bounds every
// downstream call.
//
// Payload ownership: the request envelope is marshalled into a
// pooled scratch []byte acquired from payloadPool. The defer
// releases the buffer back to the pool when Handle returns. This is
// safe because nats.Conn.RequestWithContext synchronously copies the
// outgoing message into its write buffer before returning the reply —
// by the time Request returns, the payload slice is no longer
// referenced by NATS and is safe to reuse. Any future refactor that
// keeps the payload slice alive beyond this function MUST stop using
// the pool.
func (h *Handler) Handle(ctx context.Context, in *ServeInput) *ServeResult {
	table := h.cfg.Table()

	if in.Method == "OPTIONS" {
		return h.handlePreflight(table, in)
	}

	// Bind a request-scoped logger before route lookup so even the
	// 404/405 short-circuits carry the request_id + traceparent
	// fields that operators rely on for cross-service correlation
	// during postmortems. The route field is appended after lookup.
	reqLog := h.cfg.Logger.With().
		Str("request_id", in.RequestID).
		Str("traceparent", in.Traceparent).
		Logger()

	route, params, ok := table.Lookup(in.Method, in.Path)
	if !ok {
		if allow := table.Methods(in.Path); len(allow) > 0 {
			result := toServeResult(gerrors.MethodNotAllowed)
			result.Headers["Allow"] = []string{strings.Join(allow, ", ")}

			return result
		}

		return toServeResult(gerrors.NotFound)
	}

	reqLog = reqLog.With().
		Str("route", route.Method+":"+route.PathTemplate).
		Logger()

	// Intake guard: the request envelope is one JSON document
	// (RFC 8259 §2), so a non-JSON inbound body can never be
	// embedded verbatim — upstream JSON.parse would throw and the
	// client would see an opaque 5xx indistinguishable from an
	// outage. Reject with 400 before spending a verifier hop or a
	// rate-limit store round-trip on a request that cannot be
	// forwarded. Empty bodies skip the check (encoded as `null`).
	if len(in.Body) > 0 && !codec.Valid(in.Body) {
		reqLog.Debug().Msg("proxy: rejecting non-JSON request body")
		return toServeResult(gerrors.BadRequest)
	}

	var claims json.RawMessage
	var authHeaders map[string][]string

	timeout := h.cfg.Timeout
	if route.Timeout > 0 {
		timeout = route.Timeout
	}

	routeHeaders := in.Headers
	if route.Auth != nil {
		authOutcome := h.runAuthFlow(ctx, in, route, params, timeout, reqLog)
		if !authOutcome.Proceed {
			return authOutcome.ShortCircuit
		}

		claims = authOutcome.Claims
		authHeaders = authOutcome.AuthHeaders
		// Once the verifier has decoded the bearer token into
		// structured claims, the raw credentials MUST NOT travel onto
		// the route envelope. The claims are the contract the route
		// handler consumes; forwarding the token alongside them lets
		// any downstream service bypass the verifier (re-decode,
		// store, replay) and silently breaks rotation, blacklists,
		// and revocation. Cookie-auth is left untouched because
		// cookie-auth is not yet wired through the verifier path.
		routeHeaders = stripAuthHeaders(in.Headers)
	}

	rlHeaders, rlShortCircuit := h.applyRateLimitGate(ctx, route, in, claims, timeout, reqLog)
	if rlShortCircuit != nil {
		return rlShortCircuit
	}

	payload := acquirePayload()
	defer releasePayload(payload)

	err := h.cfg.Encoder.Encode(payload, &EncodeInput{
		Method:      in.Method,
		Path:        in.Path,
		Body:        in.Body,
		Query:       in.Query,
		Headers:     routeHeaders,
		Route:       route,
		PathParams:  params,
		RequestID:   in.RequestID,
		Traceparent: in.Traceparent,
		RemoteAddr:  in.RemoteAddr,
		ReceivedAt:  in.ReceivedAt,
		TimeoutMs:   timeout.Milliseconds(),
		Auth:        claims,
	})
	if err != nil {
		reqLog.Error().Err(err).Msg("proxy encode failed")
		return mergeRateLimitHeaders(toServeResult(gerrors.InternalError), rlHeaders)
	}

	replyBytes, err := h.cfg.Nats.Request(ctx, route.Subject, *payload, timeout)
	if err != nil {
		if isTimeoutErr(err) {
			return mergeRateLimitHeaders(toServeResult(gerrors.GatewayTimeout), rlHeaders)
		}
		reqLog.Error().Err(err).Str("subject", route.Subject).Msg("nats request failed")
		return mergeRateLimitHeaders(toServeResult(gerrors.ServiceUnavailable), rlHeaders)
	}

	reply, err := h.cfg.Decoder.Decode(replyBytes)
	if err != nil {
		reqLog.Error().Err(err).Msg("reply decode failed")
		return mergeRateLimitHeaders(toServeResult(gerrors.BadGateway), rlHeaders)
	}

	mergedHeaders := mergeHeaders(reply.Headers, in.RequestID)
	mergeAuthHeaders(mergedHeaders, authHeaders)

	for k, v := range rlHeaders {
		if _, exists := mergedHeaders[k]; !exists {
			mergedHeaders[k] = []string{v}
		}
	}

	for k, v := range route.Headers {
		if _, exists := mergedHeaders[k]; !exists {
			mergedHeaders[k] = []string{v}
		}
	}

	stampResponseCORS(mergedHeaders, route.CORS, in.Headers["origin"])

	// RFC 9110 §15.5.2 binds whoever GENERATES the 401 — on the wire
	// that is this gateway, regardless of whether the upstream route
	// handler remembered the challenge header. The verifier path has
	// carried this stamp since the auth port; without it here, an
	// SDK-side handler replying a bare 401 leaks a spec-violating
	// response to the client.
	stampDefaultWWWAuthenticate(reply.Status, mergedHeaders)

	return &ServeResult{
		Status:  reply.Status,
		Headers: mergedHeaders,
		Body:    reply.Body,
	}
}

// handlePreflight handles CORS OPTIONS preflight requests. It uses the
// Access-Control-Request-Method header to find the actual route, then
// returns 204 with the appropriate CORS headers if the origin matches.
func (h *Handler) handlePreflight(table routing.Table, in *ServeInput) *ServeResult {
	acrm := in.Headers["access-control-request-method"]
	if acrm == "" {
		return toServeResult(gerrors.NotFound)
	}

	route, _, ok := table.Lookup(strings.ToUpper(acrm), in.Path)
	if !ok || route.CORS == nil {
		return toServeResult(gerrors.NotFound)
	}

	origin := in.Headers["origin"]
	matched := MatchOrigin(route.CORS, origin)
	if matched == "" {
		// The denial is origin-dependent content: a shared cache
		// storing this 404 without Vary: Origin would serve it to a
		// legitimate origin's preflight (the poisoning case in the
		// Fetch standard's caching section).
		result := toServeResult(gerrors.NotFound)
		appendVaryOrigin(result.Headers)

		return result
	}

	preflight := BuildPreflightHeaders(route.CORS, matched)

	headers := make(map[string][]string, len(preflight))
	for k, v := range preflight {
		headers[k] = []string{v}
	}

	// The gateway has already proved the route exists for the
	// requested method (that is how preflight lookup works), so an
	// empty cors.Methods config defaults ACAM to the validated
	// request method instead of omitting the header — an omitted
	// ACAM makes the browser fail a non-safelisted method the
	// gateway would happily serve.
	if _, present := headers["Access-Control-Allow-Methods"]; !present {
		headers["Access-Control-Allow-Methods"] = []string{strings.ToUpper(acrm)}
	}

	return &ServeResult{Status: 204, Headers: headers}
}

// stampResponseCORS applies the route's CORS policy to an outgoing
// response's headers. Every response of a CORS-configured route is
// origin-varying content — including responses to requests with a
// foreign or absent Origin — so Vary: Origin is appended
// unconditionally; stamping it only on matches would let a shared
// cache store the header-less variant and poison subsequent CORS
// requests (the exact example in the Fetch standard's caching
// section). The Access-Control-* headers are stamped only for a
// matched origin.
func stampResponseCORS(headers map[string][]string, cors *registry.CORSMeta, origin string) {
	if cors == nil {
		return
	}

	appendVaryOrigin(headers)

	if matched := MatchOrigin(cors, origin); matched != "" {
		for k, v := range BuildResponseCORSHeaders(cors, matched) {
			if k == "Vary" {
				continue // already appended, preserving upstream members
			}
			headers[k] = []string{v}
		}
	}
}

// appendVaryOrigin adds Origin to the response's Vary list without
// clobbering upstream-supplied members — Vary is list-valued, and
// dropping an upstream `Vary: Accept-Encoding` would be a caching
// correctness bug of its own. Upstream replies arrive with
// lowercase keys (mergeHeaders copies them verbatim), so any case
// variant of the key is folded into the canonical "Vary" entry.
// No-op when Origin is already present (as a standalone value or
// inside a comma-joined member).
func appendVaryOrigin(headers map[string][]string) {
	members := headers["Vary"]

	for k, v := range headers {
		if k != "Vary" && strings.EqualFold(k, "Vary") {
			members = append(members, v...)
			delete(headers, k)
		}
	}

	for _, member := range members {
		for _, token := range strings.Split(member, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "Origin") {
				headers["Vary"] = members
				return
			}
		}
	}

	headers["Vary"] = append(members, "Origin")
}

// applyRateLimitGate runs the per-route rate-limit check for a
// request that has already cleared route matching and (if configured)
// the auth flow. It returns the response headers the caller must
// stamp onto the eventual reply (X-RateLimit-Limit, -Remaining, -Reset),
// and a non-nil short-circuit ServeResult when the bucket rejected
// the request — the caller MUST return that result verbatim without
// proceeding to the upstream NATS request.
//
// The gate short-circuits when:
//   - the store.Allow decision is disallowed under a healthy backend
//     (429 Too Many Requests), or
//   - the store returns an error AND the configured FailPolicy
//     resolves to reject (closed-on-failure deployments).
//   - the verifier-supplied claims fail to unmarshal AND the
//     configured FailPolicy resolves to reject. Surfacing the
//     unmarshal failure through FailPolicy is critical for
//     multi-tenant deployments — silently falling back to clientIP
//     would collapse every NAT'd tenant onto one bucket.
//
// Rate-limit headers are computed on every path. On the happy path
// and on a 429 the full triplet (Limit, Remaining, Reset) is emitted.
// On the fail-open branch (store errored or claims failed to parse,
// FailPolicy resolved to allow) the Decision is unpopulated, so
// BuildHeaders only emits the static X-RateLimit-Limit — Remaining/
// Reset would otherwise convey "bucket exhausted, resets in year 1"
// because time.Time{}.Unix() is negative.
//
// No gate is applied when the route has no RateLimit block, the
// Router dependency is nil, or RPS <= 0 — the last case being the
// fail-safe interpretation of a malformed limit (treat zero/negative
// RPS as "no limit" instead of "block everything").
//
// The ctx argument is the inbound request context. The rate-limit
// check derives a child context with the per-route timeout so a slow
// or hung backend cannot stall the request beyond its own deadline
// AND a client disconnect (ctx cancellation) tears down the in-flight
// store call. Without ctx propagation a 30s NATS-KV stall would
// outlive a client that gave up after 1s — leaking goroutine and
// connection budget for nothing.
// failPolicyFor resolves the fail policy for one route: a non-empty
// route-level value (already sanitised by the routing builder to
// open/closed) overrides the gateway-wide policy; empty inherits it.
// Resolve returns zero-size policy values, so the per-request call is
// allocation-free.
func (h *Handler) failPolicyFor(route routing.Route) ratelimit.Policy {
	if route.RateLimit != nil && route.RateLimit.FailPolicy != "" {
		return ratelimit.FailPolicy(route.RateLimit.FailPolicy).Resolve()
	}

	return h.cfg.RateLimiter.FailPolicy()
}

func (h *Handler) applyRateLimitGate(
	ctx context.Context,
	route routing.Route,
	in *ServeInput,
	claims json.RawMessage,
	timeout time.Duration,
	reqLog zerolog.Logger,
) (map[string]string, *ServeResult) {
	if route.RateLimit == nil || route.RateLimit.RPS <= 0 || h.cfg.RateLimiter == nil {
		return nil, nil
	}

	rlKey, claimsErr := h.resolveRateLimitKey(in, route, claims)
	if claimsErr != nil {
		// Multi-tenant safety: a NAT'd fleet whose verifier ships
		// malformed claims would otherwise collapse onto a single
		// IP-keyed bucket, defeating per-user isolation. Route the
		// failure through the same FailPolicy that handles store
		// outages so closed-on-error deployments reject (likely 503)
		// while open-on-error deployments fall back to IP and emit
		// the WARN line below for operator visibility.
		h.cfg.RateLimiter.RecordClaimsUnmarshalError()
		// Per-route WARN dedupe: first goroutine to observe the
		// failure on this route emits the log line; subsequent
		// requests on the same route only tick the counter. A
		// misbehaving verifier at 10k RPS would otherwise emit 10k
		// identical WARN lines per second — swamping the log pipeline
		// while conveying exactly one piece of actionable information.
		routeKey := route.Method + ":" + route.PathTemplate
		if _, alreadyLogged := h.claimsUnmarshalLogged.LoadOrStore(routeKey, struct{}{}); !alreadyLogged {
			reqLog.Warn().
				Err(claimsErr).
				Str("event", "ratelimit.claims.unmarshal_failed").
				Strs("key_by", route.RateLimit.KeyBy).
				Str("claims_preview", previewClaimsForLog(claims)).
				Msg("ratelimit: verifier claims failed to unmarshal; routing through FailPolicy")
		}

		allowed := h.failPolicyFor(route).Apply(claimsErr, route, rlKey, reqLog)

		rlHeaders := ratelimit.BuildHeaders(route.RateLimit, ratelimit.Decision{})
		if !allowed {
			result := toServeResult(gerrors.ServiceUnavailable)
			for k, v := range rlHeaders {
				result.Headers[k] = []string{v}
			}

			return rlHeaders, result
		}
		// Fail-open: continue with the IP-fallback key the resolver
		// already produced. Partial enforcement during the
		// degraded-claims window is strictly better than no
		// enforcement.
	}

	burst := route.RateLimit.Burst
	if burst == 0 {
		burst = route.RateLimit.RPS * 2
	}

	fullKey := ratelimit.BuildBucketKey(route.Method, route.PathTemplate, rlKey)
	store := h.cfg.RateLimiter.StoreFor(route)

	// Clamp the gate's wall-clock budget to the smaller of the route
	// timeout and the dedicated rate-limit budget. The latter is usually
	// an order of magnitude shorter (50ms vs 10s) so a hot-key CAS storm
	// cannot drain the upstream round-trip allowance. A zero
	// RateLimitTimeout disables the clamp — legacy harnesses see the
	// same per-route-timeout behaviour they had before the budget knob
	// was introduced.
	rlBudget := timeout
	if h.cfg.RateLimitTimeout > 0 && h.cfg.RateLimitTimeout < rlBudget {
		rlBudget = h.cfg.RateLimitTimeout
	}

	rlCtx, cancel := context.WithTimeout(ctx, rlBudget)
	decision, rlErr := store.Allow(rlCtx, fullKey, route.RateLimit.RPS, burst)
	cancel()

	allowed := decision.Allowed
	if rlErr != nil {
		// Drop the partial Decision returned alongside the error so
		// BuildHeaders sees Decision{}.IsZero() and emits the static
		// X-RateLimit-Limit only. The previous behaviour (forwarding
		// the unpopulated Decision verbatim) leaked Remaining: 0 /
		// Reset: -62135596800 to clients on the fail-open branch.
		decision = ratelimit.Decision{}
		allowed = h.failPolicyFor(route).Apply(rlErr, route, fullKey, reqLog)
	}

	rlHeaders := ratelimit.BuildHeaders(route.RateLimit, decision)

	if !allowed {
		// Distinguish "client over their budget" from "our store is down":
		//   429 Too Many Requests — bucket truly empty under a healthy
		//                            backend. The client SHOULD slow down.
		//   503 Service Unavailable — the rate-limit store errored AND
		//                              FailPolicy resolved to reject. The
		//                              client did nothing wrong; the
		//                              gateway is degraded.
		// Conflating the two collapses operator alerting (a 429-rate
		// spike during an incident is indistinguishable from organic
		// traffic) and misleads clients (a 429 instructs them to back
		// off, a 503 invites a retry once the gateway recovers). The
		// claims-unmarshal branch above already returns 503 on the same
		// fail-closed path; keeping the store-error branch on 429 here
		// would leave the two paths inconsistent for no defensible
		// reason.
		errBody := gerrors.TooManyRequests
		if rlErr != nil {
			errBody = gerrors.ServiceUnavailable
		}
		result := toServeResult(errBody)
		for k, v := range rlHeaders {
			result.Headers[k] = []string{v}
		}

		return rlHeaders, result
	}

	return rlHeaders, nil
}

// resolveRateLimitKey computes the rate-limit bucket key from the
// route's keyBy chain, falling back to clientIP if nothing resolves.
//
// The returned error is non-nil only when the verifier-supplied
// claims payload is non-empty but fails JSON unmarshal. Callers MUST
// route that error through their FailPolicy instead of treating it
// as a clean key resolution: a multi-tenant deployment that silently
// fell back to clientIP for tenants with malformed claims would
// collapse every NAT'd tenant onto one bucket, defeating per-user
// isolation. Every other keyBy strategy resolves through pure
// header / cookie / IP reads that cannot fail.
func (h *Handler) resolveRateLimitKey(
	in *ServeInput,
	route routing.Route,
	claims json.RawMessage,
) (string, error) {
	keyBy := route.RateLimit.KeyBy
	if len(keyBy) == 0 {
		keyBy = []string{"ip"}
	}

	var claimsMap map[string]any
	var unmarshalErr error
	if len(claims) > 0 {
		if err := json.Unmarshal(claims, &claimsMap); err != nil {
			unmarshalErr = fmt.Errorf("ratelimit: claims unmarshal: %w", err)
		}
	}

	key := ratelimit.ResolveKey(
		keyBy,
		in.RemoteAddr,
		func(name string) string { return in.Headers[name] },
		func(name string) (string, bool) { return extractCookie(in.Headers, name) },
		claimsMap,
	)

	return key, unmarshalErr
}

// claimsRedactPattern matches JSON object-key prefixes likely to
// contain secrets in a verifier reply payload. Compiled once at
// package init so the redaction step on the WARN log path stays
// cheap; matched substrings are replaced with `***`. Match is
// deliberately permissive — false positives only obscure preview
// data, false negatives leak credentials to operator logs.
var claimsRedactPattern = regexp.MustCompile(
	`(?i)"(password|token|secret|key|authorization|auth)"\s*:\s*"[^"]*"`,
)

// previewClaimsForLog returns up to 256 bytes of claims with any
// password/token/secret/key field values replaced by `***`. Used in
// the structured WARN line emitted when a verifier reply fails to
// parse — operators need enough context to spot a multi-tenant NAT
// collision (the original failure mode), but they MUST NOT see raw
// credentials in cleartext logs.
func previewClaimsForLog(claims json.RawMessage) string {
	if len(claims) == 0 {
		return ""
	}

	// Slice the byte view before the string allocation so a 4 KiB
	// claim payload does not allocate 4 KiB of string just to be
	// truncated to 256 bytes. The previous (s := string(claims); s[:n])
	// shape allocated proportional to the input size — wasted memory
	// on the WARN log path, which fires once per misbehaving verifier
	// per route after dedupe but during a startup error storm can
	// still be hot.
	const maxPreview = 256
	n := len(claims)
	if n > maxPreview {
		n = maxPreview
	}
	preview := string(claims[:n])

	return claimsRedactPattern.ReplaceAllString(preview, `"$1":"***"`)
}

// strippedAuthHeaders enumerates the request-side credential headers
// that MUST NOT travel onto the route envelope after the verifier has
// successfully decoded the bearer token into structured claims.
//
// Keys are lowercase because the HTTP adapter folds inbound header
// names to lowercase before they reach the proxy layer. Adding a new
// credential header to the list is a single-line change; the lookup
// is O(1) per check.
//
// Cookie is intentionally NOT in this list today: cookie-auth is not
// yet wired through the verifier path, so stripping the Cookie header
// would break unrelated session cookies forwarded to the route. Once
// cookie-auth lands, the cookie-name actually consumed by the verifier
// will be excised — but this requires a more surgical edit on the
// Cookie header value, not a wholesale strip.
var strippedAuthHeaders = map[string]struct{}{
	"authorization":       {},
	"proxy-authorization": {},
}

// stripAuthHeaders returns a copy of in with the credential headers
// (Authorization, Proxy-Authorization) removed. The input map is not
// mutated — the proxy layer caches in.Headers in several places and
// expects them to stay stable; the small per-request allocation is
// the right tradeoff for that immutability invariant.
//
// Returns nil when the input is nil or empty so the encoder treats
// "no headers" identically to "no auth-stripped headers". The empty-
// input fast path keeps the public-route surface clean — a route
// without an Auth block does not allocate here at all because the
// caller never invokes stripAuthHeaders on the public path.
func stripAuthHeaders(in map[string]string) map[string]string {
	if len(in) == 0 {
		return in
	}

	out := make(map[string]string, len(in))
	for name, value := range in {
		if _, strip := strippedAuthHeaders[name]; strip {
			continue
		}

		out[name] = value
	}

	return out
}

// extractCookie parses a single named cookie from the Cookie header
// and reports whether the name appeared more than once.
//
// The first return is the value of the first matching cookie-pair
// (RFC 6265 §5.4 cookie-pair = cookie-name "=" cookie-value). The
// second return signals collision: when true the rate-limit caller
// MUST treat the cookie strategy as unresolvable and fall through to
// the next keyBy entry, because RFC 6265 permits multiple same-name
// cookies and an attacker can sandwich a victim's session next to
// their own to defeat per-session rate-limit isolation.
//
// Avoids allocating a full cookie map per request — most rate-limit
// keyBy chains resolve before reaching the cookie strategy. The
// duplicate check still walks the whole header even after a hit so
// that injection attempts surface as collision rather than silently
// returning the first match.
//
// Per RFC 6265 §5.4 cookie-values MAY be wrapped in DQUOTE characters.
// The returned value is trimmed of surrounding whitespace and a
// single pair of matching quotes so that "session=abc",
// `session="abc"`, and `session= abc` all collapse to "abc". Without
// this normalisation, equivalent cookies would land in distinct
// rate-limit buckets.
func extractCookie(headers map[string]string, name string) (string, bool) {
	cookieHeader := headers["cookie"]
	if cookieHeader == "" {
		return "", false
	}

	var (
		value    string
		matched  bool
		collided bool
	)

	for _, part := range strings.Split(cookieHeader, ";") {
		part = strings.TrimSpace(part)
		eqIdx := strings.IndexByte(part, '=')
		if eqIdx <= 0 || part[:eqIdx] != name {
			continue
		}

		if matched {
			collided = true

			break
		}

		value = trimCookieQuotes(strings.TrimSpace(part[eqIdx+1:]))
		matched = true
	}

	return value, collided
}

// trimCookieQuotes strips a single matching pair of DQUOTE characters
// wrapping a cookie value, leaving an unquoted value untouched. A
// stray opening or closing quote is preserved verbatim — RFC 6265
// only sanctions paired quoting, so half-quoted values are treated
// as part of the value rather than as a parsing artifact.
func trimCookieQuotes(value string) string {
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		return value[1 : len(value)-1]
	}

	return value
}

// authFlowResult captures the outcome of a pre-flight verifier
// sub-request. Exactly one of ShortCircuit and the (Claims,
// AuthHeaders) pair is populated:
//
//   - When Proceed is false, ShortCircuit holds the response that
//     must be returned to the HTTP client verbatim; Claims and
//     AuthHeaders are zero.
//   - When Proceed is true, the caller injects Claims as the
//     `auth` field of the main request envelope and merges
//     AuthHeaders into the main route reply headers via
//     mergeAuthHeaders before the HTTP write.
type authFlowResult struct {
	Proceed      bool
	ShortCircuit *ServeResult
	Claims       json.RawMessage
	AuthHeaders  map[string][]string
}

// runAuthFlow issues the verifier sub-request for a protected route
// and decides whether the main route request proceeds.
//
// The returned authFlowResult is discriminated on Proceed:
//
//   - Proceed == false → the caller MUST return ShortCircuit
//     verbatim. Covers 401/403 short-circuits, verifier transport
//     errors (503/504/502), and decoder failures.
//   - Proceed == true → the caller continues to the main route
//     request. Claims carries the verifier's reply body for
//     injection into the main envelope's auth field (nil on the
//     optional-auth 401 swallow). AuthHeaders carries the verifier
//     reply's response headers for merge into the main reply — only
//     set on a 200 verifier reply so failed verifier replies never
//     leak headers onto the main response.
//
// The ctx argument flows through to the verifier NATS round trip so
// a client disconnect or a caller-imposed deadline tears down the
// verifier sub-request alongside the main request. The verifier and
// the main route share one timeout budget — the per-route Timeout —
// because an upstream auth check that consumes the full budget
// leaves nothing for the actual handler.
func (h *Handler) runAuthFlow(
	ctx context.Context,
	in *ServeInput,
	route routing.Route,
	params map[string]string,
	timeout time.Duration,
	reqLog zerolog.Logger,
) *authFlowResult {
	verifyPayload := acquirePayload()
	defer releasePayload(verifyPayload)

	// The verify-request envelope is identical to the main envelope
	// except Body is always nil — verifiers never see the request
	// body by design. Auth decisions must stay independent of body
	// content so the verifier path can be cached without cache-key
	// explosion on body variance.
	err := h.cfg.Encoder.Encode(verifyPayload, &EncodeInput{
		Method:      in.Method,
		Path:        in.Path,
		Body:        nil,
		Query:       in.Query,
		Headers:     in.Headers,
		Route:       route,
		PathParams:  params,
		RequestID:   in.RequestID,
		Traceparent: in.Traceparent,
		RemoteAddr:  in.RemoteAddr,
		ReceivedAt:  in.ReceivedAt,
		TimeoutMs:   timeout.Milliseconds(),
	})
	if err != nil {
		reqLog.Error().Err(err).Msg("auth: verify encode failed")
		return &authFlowResult{Proceed: false, ShortCircuit: toServeResult(gerrors.InternalError)}
	}

	replyBytes, err := h.cfg.Nats.Request(ctx, route.Auth.VerifierSubject, *verifyPayload, timeout)
	if err != nil {
		if isTimeoutErr(err) {
			return &authFlowResult{Proceed: false, ShortCircuit: toServeResult(gerrors.GatewayTimeout)}
		}
		reqLog.Error().
			Err(err).
			Str("subject", route.Auth.VerifierSubject).
			Msg("auth: verifier nats request failed")

		return &authFlowResult{Proceed: false, ShortCircuit: toServeResult(gerrors.ServiceUnavailable)}
	}

	reply, err := h.cfg.Decoder.Decode(replyBytes)
	if err != nil {
		reqLog.Error().Err(err).Msg("auth: verifier reply decode failed")
		return &authFlowResult{Proceed: false, ShortCircuit: toServeResult(gerrors.BadGateway)}
	}

	if reply.Status == 200 {
		return &authFlowResult{
			Proceed:     true,
			Claims:      reply.Body,
			AuthHeaders: reply.Headers,
		}
	}

	// Optional-auth: swallow 401 only. 403 and every other non-200
	// status still short-circuits. Transport errors above already
	// returned before this branch. Verifier headers are intentionally
	// dropped on this path — only 200-path verifier replies
	// contribute headers to the main response.
	if route.Auth.Optional && reply.Status == 401 {
		return &authFlowResult{Proceed: true}
	}

	// Forward the verifier's reply verbatim — this is how verifier-set
	// headers like WWW-Authenticate reach the client.
	merged := mergeHeaders(reply.Headers, in.RequestID)
	stampDefaultWWWAuthenticate(reply.Status, merged)

	return &authFlowResult{
		Proceed: false,
		ShortCircuit: &ServeResult{
			Status:  reply.Status,
			Headers: merged,
			Body:    reply.Body,
		},
	}
}

// stampDefaultWWWAuthenticate enforces RFC 9110 §11.6.1: a 401
// response MUST include a `WWW-Authenticate` challenge header. When a
// verifier replies 401 without supplying its own challenge (the common
// case for token-only APIs that just map "no/invalid token" to 401),
// the gateway stamps a generic `Bearer realm="gateway"` so the wire
// response stays spec-compliant regardless of the upstream verifier's
// hygiene.
//
// Applies only to 401 replies. Other statuses are untouched. The
// presence check is case-insensitive because mergeHeaders preserves
// the upstream casing and browsers/clients match case-insensitively
// per RFC 9110 §5.1.
func stampDefaultWWWAuthenticate(status int, headers map[string][]string) {
	if status != 401 {
		return
	}

	for key := range headers {
		if strings.EqualFold(key, "www-authenticate") {
			return
		}
	}

	headers["www-authenticate"] = []string{`Bearer realm="gateway"`}
}

// mergeHeaders combines the reply headers with gateway-owned defaults.
// Multi-value entries from the reply (e.g. multiple Set-Cookie lines)
// are forwarded verbatim so RFC-mandated multi-value headers survive
// the wire. The gateway always stamps its own x-request-id on top of
// whatever the upstream service emitted, so a compromised upstream
// cannot forge correlator ids.
func mergeHeaders(reply map[string][]string, requestID string) map[string][]string {
	out := make(map[string][]string, len(reply)+2)
	out["content-type"] = []string{"application/json"}
	for k, v := range reply {
		out[k] = v
	}
	out["x-request-id"] = []string{requestID}
	return out
}

// mergeAuthHeaders layers a verifier reply's response headers onto
// an already-merged main reply headers map using these rules:
//
//   - set-cookie is appended with verifier values first, then the
//     route's existing values — so the client sees the verifier's
//     rotated cookies alongside any cookies the main handler set,
//     in a stable order that matches the canonical auth contract.
//   - Other headers from the verifier are added only when the
//     merged map does not already contain the key. The main route
//     reply (and the gateway's x-request-id / content-type stamps
//     from mergeHeaders) own conflicting single-value slots
//     unconditionally, so verifier headers never overwrite gateway
//     state or silently shadow a route-chosen value.
//
// The merged map is mutated in place. Callers are expected to have
// already run mergeHeaders so gateway defaults are baked in.
func mergeAuthHeaders(merged map[string][]string, authHeaders map[string][]string) {
	if len(authHeaders) == 0 {
		return
	}

	for verifierKey, verifierValues := range authHeaders {
		if len(verifierValues) == 0 {
			continue
		}

		if verifierKey == "set-cookie" {
			existing := merged["set-cookie"]
			combined := make([]string, 0, len(verifierValues)+len(existing))
			combined = append(combined, verifierValues...)
			combined = append(combined, existing...)
			merged["set-cookie"] = combined

			continue
		}

		if _, exists := merged[verifierKey]; exists {
			continue
		}

		merged[verifierKey] = verifierValues
	}
}

// jsonHeaders returns a fresh header map carrying only content-type.
// Allocates on every call because ServeResult.Headers is expected to
// be caller-owned — the shared pre-encoded error body is paired with
// this per-request header map so no caller ever sees aliased state.
func jsonHeaders() map[string][]string {
	return map[string][]string{"content-type": {"application/json"}}
}

// toServeResult materializes a ServeResult from a pre-encoded
// HTTPError. The ServeResult allocates its own headers map because
// the HTTPError is shared across goroutines and must never be
// aliased by a caller-owned mutable map.
//
// GatewayOwnedBody is set to true because every gerrors.* value is
// a gateway-produced failure shape (404/502/503/504/...) — the HTTP
// adapter uses that to stamp `Cache-Control: no-store` so shared
// caches never memoize a transient infrastructure failure.
func toServeResult(e gerrors.HTTPError) *ServeResult {
	return &ServeResult{
		Status:           e.Status,
		Headers:          jsonHeaders(),
		Body:             e.Body,
		GatewayOwnedBody: true,
	}
}

// mergeRateLimitHeaders stamps every X-RateLimit-* header from the
// rate-limit gate's pre-computed map onto a gateway-owned error
// response. Non-destructive: existing keys win, mirroring the
// happy-path merge below at line 249.
//
// Why error responses MUST carry rate-limit headers: a client whose
// upstream call 5xx'd has STILL consumed a token from their rate-
// limit budget (the gate fired before the upstream attempt). If the
// 5xx response strips X-RateLimit-Remaining / Reset, the client has
// no signal to size its retry-with-backoff against the actual
// budget — it can either retry too aggressively (and trip 429 next)
// or back off too conservatively (under-utilising its allowance).
// Same applies to 504 timeouts and 502 decode failures: the gate
// ran, the budget was charged, the headers belong on the wire.
//
// Nil-safe so callers in the encode-fail / decode-fail branches do
// not need to thread an extra nil check; rlHeaders is nil when the
// route has no rate-limit block configured, in which case there is
// nothing to merge.
func mergeRateLimitHeaders(result *ServeResult, rlHeaders map[string]string) *ServeResult {
	if result == nil || len(rlHeaders) == 0 {
		return result
	}
	for k, v := range rlHeaders {
		if _, exists := result.Headers[k]; !exists {
			result.Headers[k] = []string{v}
		}
	}

	return result
}
