package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/rs/zerolog"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/codec"
	gerrors "github.com/HorizonRepublic/gateway/apps/gateway-server/internal/errors"
	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/observability"
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
	// Metrics receives the RED observation and in-flight gauge bumps
	// for every request. nil disables metrics collection entirely —
	// the handler then skips the timing wrapper's metric calls, which
	// keeps legacy test harnesses and benchmark fixtures working
	// without a registry.
	Metrics *observability.Metrics
	// AccessLog enables the single structured completion event per
	// request (event=http.access). Wired from ACCESS_LOG_ENABLED;
	// defaults to true in production bootstrap. The event is emitted
	// at INFO on cfg.Logger, so LOG_LEVEL=warn silences it without a
	// separate knob.
	AccessLog bool
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

// requestOutcome accumulates the per-request facts the observability
// wrapper needs after serve returns: the matched route template (or
// an unmatched/preflight sentinel), the NATS subject, and the auth /
// rate-limit gate outcomes. Lives on Handle's stack — no allocation.
type requestOutcome struct {
	route     string
	subject   string
	auth      string
	rateLimit string
}

// Outcome label values for the auth and rate-limit gates. Closed
// sets: both fields feed the access log, and a bounded vocabulary is
// what makes the log queryable ("all fail-open decisions last hour")
// instead of free-text.
const (
	// outcomeNone marks a gate that was not configured for the route.
	outcomeNone = "none"
	// outcomeOK marks a verifier 200 whose claims travel on the envelope.
	outcomeOK = "ok"
	// outcomeAnonymous marks an optional-auth route whose verifier
	// replied 401 — the request proceeded without claims.
	outcomeAnonymous = "anonymous"
	// outcomeDenied marks a verifier 4xx short-circuit (401/403/...).
	outcomeDenied = "denied"
	// outcomeError marks a gate that failed for infrastructure
	// reasons: verifier transport/decode failure, or a rate-limit
	// store error that FailPolicy resolved to reject.
	outcomeError = "error"
	// outcomeAllowed marks a healthy rate-limit pass.
	outcomeAllowed = "allowed"
	// outcomeRejected marks a 429 — bucket empty under a healthy backend.
	outcomeRejected = "rejected"
	// outcomeFailOpen marks a store/claims failure that FailPolicy
	// resolved to allow — enforcement was degraded for this request.
	outcomeFailOpen = "fail_open"
)

// Handle performs the full request lifecycle: route lookup, envelope
// encode, NATS request, reply decode, response construction. Errors
// are translated to the appropriate HTTP status with a pre-encoded
// JSON error body from the internal/errors package.
//
// Handle is the observability wrapper around serve: it times the
// request, maintains the in-flight gauge, records the RED metrics,
// and emits the single structured access-log event at completion.
// When both surfaces are disabled (nil Metrics, AccessLog false) it
// delegates straight to serve so legacy harnesses pay nothing.
func (h *Handler) Handle(ctx context.Context, in *ServeInput) *ServeResult {
	obs := requestOutcome{
		route:     observability.RouteUnmatched,
		auth:      outcomeNone,
		rateLimit: outcomeNone,
	}

	if h.cfg.Metrics == nil && !h.cfg.AccessLog {
		return h.serve(ctx, in, &obs)
	}

	if h.cfg.Metrics != nil {
		h.cfg.Metrics.HTTPRequestStarted()
	}

	start := time.Now()
	result := h.serve(ctx, in, &obs)
	elapsed := time.Since(start)

	if h.cfg.Metrics != nil {
		h.cfg.Metrics.HTTPRequestFinished()
		h.cfg.Metrics.ObserveHTTPRequest(in.Method, obs.route, result.Status, elapsed.Seconds())
	}

	if h.cfg.AccessLog {
		// Exactly one completion event per request. Fields reuse
		// values already resolved upstream (trusted-proxy client IP,
		// adapter-minted request id) — nothing is re-derived here.
		h.cfg.Logger.Info().
			Str("event", "http.access").
			Str("method", in.Method).
			Str("route", obs.route).
			Int("status", result.Status).
			Float64("duration_ms", float64(elapsed.Nanoseconds())/1e6).
			Int("bytes_out", len(result.Body)).
			Str("client_ip", in.RemoteAddr).
			Str("request_id", in.RequestID).
			Str("subject", obs.subject).
			Str("auth", obs.auth).
			Str("ratelimit", obs.rateLimit).
			Msg("request completed")
	}

	return result
}

// serve owns the request lifecycle proper. It mutates obs as facts
// become known (route match, auth outcome, rate-limit outcome) so the
// Handle wrapper can observe them after the response is built without
// re-deriving anything.
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
func (h *Handler) serve(ctx context.Context, in *ServeInput, obs *requestOutcome) *ServeResult {
	table := h.cfg.Table()

	// A CORS preflight is an OPTIONS request carrying an
	// Access-Control-Request-Method header — that pair is the WHATWG
	// Fetch definition of a preflight. Plain OPTIONS (no ACRM) falls
	// through to normal route lookup so SDK-declared OPTIONS routes
	// (a first-class verb on both sides of the wire contract) stay
	// reachable, and plain OPTIONS to a non-OPTIONS route follows the
	// standard 404/405 semantics.
	if in.Method == "OPTIONS" && in.Headers["access-control-request-method"] != "" {
		obs.route = observability.RoutePreflight
		return h.handlePreflight(table, in)
	}

	route, params, ok := table.Lookup(in.Method, in.Path)
	if !ok {
		if allow := allowedMethods(table, in.Method, in.Path); len(allow) > 0 {
			result := toServeResult(gerrors.MethodNotAllowed)
			result.Headers["Allow"] = []string{strings.Join(allow, ", ")}

			return result
		}

		return toServeResult(gerrors.NotFound)
	}

	origin := in.Headers["origin"]

	obs.route = route.PathTemplate
	obs.subject = route.Subject

	// Intake guard: the request envelope is one JSON document
	// (RFC 8259 §2), so a non-JSON inbound body can never be
	// embedded verbatim — upstream JSON.parse would throw and the
	// client would see an opaque 5xx indistinguishable from an
	// outage. The utf8.Valid leg closes the gap syntactic validation
	// leaves open: RFC 8259 §8.1 makes UTF-8 a MUST for inter-system
	// exchange, and a syntactically valid body carrying invalid UTF-8
	// bytes inside string values would otherwise be forwarded
	// verbatim and silently mangled to U+FFFD by the SDK side's
	// non-fatal TextDecoder — every other envelope string is
	// sanitised, and on a write path silent corruption is worse than
	// a 400 at the edge. Reject before spending a verifier hop or a
	// rate-limit store round-trip on a request that cannot be
	// forwarded. Empty bodies skip the check (encoded as `null`).
	if len(in.Body) > 0 && (!utf8.Valid(in.Body) || !codec.Valid(in.Body)) {
		reqLog := h.requestLogger(in, &route)
		reqLog.Debug().Msg("proxy: rejecting non-JSON request body")
		return stampCORSOnResult(toServeResult(gerrors.BadRequest), route.CORS, origin)
	}

	// One deadline governs the whole downstream pipeline: rate-limit
	// gate, verifier leg, and main route leg all draw from the same
	// per-route budget anchored here. Each leg receives only the
	// remaining slice, so the client-observable latency is bounded by
	// the declared route timeout instead of the sum of the legs'
	// independent timeouts.
	timeout := h.cfg.Timeout
	if route.Timeout > 0 {
		timeout = route.Timeout
	}
	deadline := time.Now().Add(timeout)

	var claims json.RawMessage
	var authHeaders map[string][]string
	var rlHeaders map[string]string

	// Rate-limit gate ordering splits on whether the keyBy chain
	// needs verified claims:
	//
	//   - Chains keyed purely on wire attributes (ip, header, cookie —
	//     including the empty-chain ip default) run BEFORE the auth
	//     flow. Auth failures would otherwise bypass the limiter
	//     entirely: an unauthenticated flood hammers the verifier at
	//     line rate while the route's declared defense never fires and
	//     never even appears in rate-limit metrics.
	//   - Chains containing a user:* strategy run AFTER the auth flow
	//     because they key on verified claims. Their exposure window
	//     is the verifier hop only, and collapsing them onto a
	//     pre-auth IP key would defeat per-user isolation for NAT'd
	//     tenants.
	//
	// A request that clears a pre-auth gate and then fails auth has
	// consumed a token by design — the failed attempt cost a verifier
	// round trip, and charging it is what makes the limit meaningful
	// against credential-stuffing traffic.
	claimsKeyed := rateLimitNeedsClaims(route)
	if !claimsKeyed {
		var rlShortCircuit *ServeResult
		rlHeaders, rlShortCircuit = h.applyRateLimitGate(ctx, route, in, nil, time.Until(deadline), obs)
		if rlShortCircuit != nil {
			return stampCORSOnResult(rlShortCircuit, route.CORS, origin)
		}
	}

	authStage := h.applyAuthStage(ctx, in, route, params, deadline, rlHeaders, origin, obs)
	if authStage.ShortCircuit != nil {
		return authStage.ShortCircuit
	}
	claims = authStage.Claims
	authHeaders = authStage.AuthHeaders
	routeHeaders := authStage.RouteHeaders

	if claimsKeyed {
		var rlShortCircuit *ServeResult
		rlHeaders, rlShortCircuit = h.applyRateLimitGate(ctx, route, in, claims, time.Until(deadline), obs)
		if rlShortCircuit != nil {
			return stampCORSOnResult(rlShortCircuit, route.CORS, origin)
		}
	}

	// Remaining budget for the main leg, recomputed after the gate
	// and the verifier consumed their slices. The envelope advertises
	// this remainder — not the full route timeout — so the upstream
	// handler budgets its internal work against reality. A verifier
	// that ate the whole budget leaves nothing to run the route in;
	// short-circuit 504 instead of issuing a request that is already
	// dead on arrival.
	remaining := time.Until(deadline)
	if remaining <= 0 {
		result := mergeRateLimitHeaders(toServeResult(gerrors.GatewayTimeout), rlHeaders)
		return stampCORSOnResult(result, route.CORS, origin)
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
		TimeoutMs:   remaining.Milliseconds(),
		Auth:        claims,
	})
	if err != nil {
		reqLog := h.requestLogger(in, &route)
		reqLog.Error().Err(err).Msg("proxy encode failed")
		result := mergeRateLimitHeaders(toServeResult(gerrors.InternalError), rlHeaders)
		return stampCORSOnResult(result, route.CORS, origin)
	}

	replyBytes, err := h.cfg.Nats.Request(ctx, route.Subject, *payload, remaining)
	if err != nil {
		if isTimeoutErr(err) {
			result := mergeRateLimitHeaders(toServeResult(gerrors.GatewayTimeout), rlHeaders)
			return stampCORSOnResult(result, route.CORS, origin)
		}
		reqLog := h.requestLogger(in, &route)
		if isPayloadTooLargeErr(err) {
			// Client-side max_payload rejection: the envelope never
			// touched the wire, so this is a request-shape problem
			// (413), not upstream degradation (503).
			reqLog.Warn().Err(err).Str("subject", route.Subject).Msg("nats request exceeds max_payload")
			result := mergeRateLimitHeaders(toServeResult(gerrors.ContentTooLarge), rlHeaders)
			return stampCORSOnResult(result, route.CORS, origin)
		}
		reqLog.Error().Err(err).Str("subject", route.Subject).Msg("nats request failed")
		result := mergeRateLimitHeaders(toServeResult(gerrors.ServiceUnavailable), rlHeaders)
		return stampCORSOnResult(result, route.CORS, origin)
	}

	reply, err := h.cfg.Decoder.Decode(replyBytes)
	if err != nil {
		reqLog := h.requestLogger(in, &route)
		reqLog.Error().Err(err).Msg("reply decode failed")
		result := mergeRateLimitHeaders(toServeResult(gerrors.BadGateway), rlHeaders)
		return stampCORSOnResult(result, route.CORS, origin)
	}

	mergedHeaders := mergeHeaders(reply.Headers, in.RequestID)
	mergeAuthHeaders(mergedHeaders, authHeaders)

	// Both merges below are non-destructive with a case-insensitive
	// presence check: upstream reply keys arrive lowercase from the
	// SDK, while gateway rate-limit headers and operator-configured
	// route headers are canonical-cased. An exact-case check would
	// add a second, differently-cased entry that Hertz folds onto the
	// SAME canonical wire name — two conflicting X-RateLimit-Limit
	// lines that recipients may comma-join or pick from arbitrarily.
	for k, v := range rlHeaders {
		if !headerPresentFold(mergedHeaders, k) {
			mergedHeaders[k] = []string{v}
		}
	}

	for k, v := range route.Headers {
		if !headerPresentFold(mergedHeaders, k) {
			mergedHeaders[k] = []string{v}
		}
	}

	stampResponseCORS(mergedHeaders, route.CORS, origin)

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

// authStageResult carries the auth-stage outputs back to serve.
// Exactly one shape is populated:
//
//   - ShortCircuit non-nil → the verifier denied the request or the
//     verifier round trip itself failed; serve returns it verbatim.
//     The result is already merged with the rate-limit headers and
//     CORS-stamped, so serve does not re-touch it.
//   - ShortCircuit nil → the request proceeds. Claims carries the
//     verifier's decoded body for the main envelope's auth field (nil
//     on a public route or an optional-auth 401 swallow), AuthHeaders
//     carries verifier response headers for the reply merge, and
//     RouteHeaders is the header set forwarded to the main leg
//     (credential-stripped once claims were decoded).
type authStageResult struct {
	ShortCircuit *ServeResult
	Claims       json.RawMessage
	AuthHeaders  map[string][]string
	RouteHeaders map[string]string
}

// applyAuthStage runs the verifier flow for a protected route and folds
// its outcome into the observability record (obs.auth). A public route
// (no Auth block) is a no-op that echoes the inbound headers as the
// route headers. The deadline is the shared per-request budget anchor;
// the verifier leg draws its remaining slice from it.
//
// Extracted from serve so the obs.auth accounting lives next to the
// decision that produces it while keeping serve within its
// cognitive-complexity budget — the same decomposition applied to
// runAuthFlow and applyRateLimitGate.
func (h *Handler) applyAuthStage(
	ctx context.Context,
	in *ServeInput,
	route routing.Route,
	params map[string]string,
	deadline time.Time,
	rlHeaders map[string]string,
	origin string,
	obs *requestOutcome,
) authStageResult {
	if route.Auth == nil {
		return authStageResult{RouteHeaders: in.Headers}
	}

	authOutcome := h.runAuthFlow(ctx, in, route, params, time.Until(deadline))
	if !authOutcome.Proceed {
		// 4xx short-circuits are verifier denials; 5xx means the
		// verifier round trip itself failed (transport, decode).
		obs.auth = outcomeDenied
		if authOutcome.ShortCircuit.Status >= 500 {
			obs.auth = outcomeError
		}

		result := mergeRateLimitHeaders(authOutcome.ShortCircuit, rlHeaders)

		return authStageResult{ShortCircuit: stampCORSOnResult(result, route.CORS, origin)}
	}

	obs.auth = outcomeOK
	if authOutcome.Claims == nil {
		obs.auth = outcomeAnonymous
	}

	// Once the verifier has decoded the bearer token into structured
	// claims, the raw credentials MUST NOT travel onto the route
	// envelope. The claims are the contract the route handler consumes;
	// forwarding the token alongside them lets any downstream service
	// bypass the verifier (re-decode, store, replay) and silently
	// breaks rotation, blacklists, and revocation. Cookie-auth is left
	// untouched because cookie-auth is not yet wired through the
	// verifier path.
	return authStageResult{
		Claims:       authOutcome.Claims,
		AuthHeaders:  authOutcome.AuthHeaders,
		RouteHeaders: stripAuthHeaders(in.Headers),
	}
}

// handlePreflight handles CORS OPTIONS preflight requests. It uses
// the Access-Control-Request-Method header to find the actual route,
// then returns 204 with the appropriate CORS headers if the origin
// matches. Handle dispatches here only for OPTIONS requests that
// carry the ACRM header (the Fetch definition of a preflight); plain
// OPTIONS goes through normal route lookup instead. The empty-ACRM
// guard is defense-in-depth for direct callers.
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
	obs *requestOutcome,
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
			reqLog := h.requestLogger(in, &route)
			reqLog.Warn().
				Err(claimsErr).
				Str("event", "ratelimit.claims.unmarshal_failed").
				Strs("key_by", route.RateLimit.KeyBy).
				Str("claims_preview", previewClaimsForLog(claims)).
				Msg("ratelimit: verifier claims failed to unmarshal; routing through FailPolicy")
		}

		allowed := h.failPolicyFor(route).Apply(claimsErr, route, rlKey, h.requestLogger(in, &route))

		rlHeaders := ratelimit.BuildHeaders(route.RateLimit, ratelimit.Decision{})
		if !allowed {
			obs.rateLimit = outcomeError
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
		allowed = h.failPolicyFor(route).Apply(rlErr, route, fullKey, h.requestLogger(in, &route))
	}

	// Outcome accounting for the access log: a clean decision maps to
	// allowed/rejected; a store error maps to fail_open (FailPolicy
	// let the request through with degraded enforcement) or error
	// (FailPolicy rejected on the gateway's behalf).
	switch {
	case rlErr == nil && allowed:
		obs.rateLimit = outcomeAllowed
	case rlErr == nil:
		obs.rateLimit = outcomeRejected
	case allowed:
		obs.rateLimit = outcomeFailOpen
	default:
		obs.rateLimit = outcomeError
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
//
// The cookie NAME is trimmed of surrounding whitespace before the
// comparison (RFC 6265bis cookie-string parsing removes leading and
// trailing WSP from the name string), so `session =abc` resolves the
// same way on the SDK and gateway sides. Pairs without "=" (or with
// nothing before it) are nameless cookies under the same parsing
// rules and never match a named lookup; the SDK's parseCookies skips
// them identically.
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
		if eqIdx <= 0 || strings.TrimSpace(part[:eqIdx]) != name {
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
// leaves nothing for the actual handler. The timeout argument is the
// REMAINING slice of that budget at the moment the auth flow starts;
// the caller recomputes the remainder after this function returns
// and passes only what is left to the main route request.
func (h *Handler) runAuthFlow(
	ctx context.Context,
	in *ServeInput,
	route routing.Route,
	params map[string]string,
	timeout time.Duration,
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
		reqLog := h.requestLogger(in, &route)
		reqLog.Error().Err(err).Msg("auth: verify encode failed")
		return &authFlowResult{Proceed: false, ShortCircuit: toServeResult(gerrors.InternalError)}
	}

	replyBytes, err := h.cfg.Nats.Request(ctx, route.Auth.VerifierSubject, *verifyPayload, timeout)
	if err != nil {
		if isTimeoutErr(err) {
			return &authFlowResult{Proceed: false, ShortCircuit: toServeResult(gerrors.GatewayTimeout)}
		}
		reqLog := h.requestLogger(in, &route)
		if isPayloadTooLargeErr(err) {
			// The verify envelope carries no body, so only extreme
			// header/query inflation lands here — still a request-
			// shape problem, mapped to 413 for consistency with the
			// main route branch.
			reqLog.Warn().
				Err(err).
				Str("subject", route.Auth.VerifierSubject).
				Msg("auth: verifier request exceeds max_payload")

			return &authFlowResult{Proceed: false, ShortCircuit: toServeResult(gerrors.ContentTooLarge)}
		}
		reqLog.Error().
			Err(err).
			Str("subject", route.Auth.VerifierSubject).
			Msg("auth: verifier nats request failed")

		return &authFlowResult{Proceed: false, ShortCircuit: toServeResult(gerrors.ServiceUnavailable)}
	}

	reply, err := h.cfg.Decoder.Decode(replyBytes)
	if err != nil {
		reqLog := h.requestLogger(in, &route)
		reqLog.Error().Err(err).Msg("auth: verifier reply decode failed")
		return &authFlowResult{Proceed: false, ShortCircuit: toServeResult(gerrors.BadGateway)}
	}

	if reply.Status == 200 {
		claims := reply.Body
		// A verifier may legitimately reply 200 with a JSON null body
		// (authenticated, no claims payload). json.RawMessage("null")
		// is non-empty, so without normalisation the encoder would
		// emit `"auth":null` — violating the envelope invariant that
		// the auth key is ABSENT when there are no claims (consumers
		// on pre-auth SDK builds must observe byte-identical
		// envelopes). Normalising here also keeps the rate-limit gate
		// consistent: null claims behave exactly like no claims.
		if isJSONNull(claims) {
			claims = nil
		}

		return &authFlowResult{
			Proceed:     true,
			Claims:      claims,
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
//
// Gateway-owned keys are matched case-insensitively (RFC 9110 §5.1 —
// field names are case-insensitive) because the reply map is attacker-
// influenced input: upstream keys normally arrive lowercase from the
// SDK, but a compromised or non-SDK responder can send "X-Request-Id"
// as a distinct map key. Both variants land on the same canonical
// wire name once Hertz folds them, and an exact-case overwrite would
// leave the forged line alive next to the gateway's — defeating the
// anti-spoofing invariant roughly half the time (map iteration
// order). Upstream content-type overrides fold into the canonical
// lowercase entry for the same reason.
func mergeHeaders(reply map[string][]string, requestID string) map[string][]string {
	out := make(map[string][]string, len(reply)+2)
	out["content-type"] = []string{"application/json"}
	for k, v := range reply {
		switch {
		case strings.EqualFold(k, "x-request-id"):
			// Gateway-owned; case-variant forgeries are dropped.
		case strings.EqualFold(k, "content-type"):
			out["content-type"] = v
		default:
			out[k] = v
		}
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
//
// Key comparisons are case-insensitive: verifier reply headers are
// upstream-influenced input like the main reply's, so set-cookie is
// recognised in any casing (an exact-match check would silently lose
// the documented verifier-first cookie ordering for a case-variant
// key), gateway-owned keys are excluded in any casing, and the
// exists-check folds so a case-variant verifier key cannot shadow an
// already-merged route header on the same canonical wire name.
func mergeAuthHeaders(merged map[string][]string, authHeaders map[string][]string) {
	if len(authHeaders) == 0 {
		return
	}

	for verifierKey, verifierValues := range authHeaders {
		if len(verifierValues) == 0 {
			continue
		}

		if strings.EqualFold(verifierKey, "x-request-id") ||
			strings.EqualFold(verifierKey, "content-type") {
			continue
		}

		if strings.EqualFold(verifierKey, "set-cookie") {
			existing := merged["set-cookie"]
			combined := make([]string, 0, len(verifierValues)+len(existing))
			combined = append(combined, verifierValues...)
			combined = append(combined, existing...)
			merged["set-cookie"] = combined

			continue
		}

		if headerPresentFold(merged, verifierKey) {
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
// response. Non-destructive: existing keys win (case-insensitively),
// mirroring the happy-path merge in Handle.
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
		if !headerPresentFold(result.Headers, k) {
			result.Headers[k] = []string{v}
		}
	}

	return result
}

// headerPresentFold reports whether key is present in headers under
// any casing. HTTP field names are case-insensitive (RFC 9110 §5.1)
// and the response header map mixes lowercase upstream keys with
// canonical-cased gateway keys — an exact-case presence check would
// let two casings of the same field coexist and later collapse onto
// one canonical wire name with conflicting values. The exact-match
// fast path keeps the common case at one map lookup; the fold scan
// only runs over the handful of entries a response carries.
func headerPresentFold(headers map[string][]string, key string) bool {
	if _, ok := headers[key]; ok {
		return true
	}
	for k := range headers {
		if strings.EqualFold(k, key) {
			return true
		}
	}

	return false
}

// requestLogger builds the request-scoped logger on demand. zerolog's
// With() allocates a ~500-byte context buffer per call, and the happy
// path emits nothing — binding the logger eagerly per request burned
// two heap allocations (~1 KiB) of discarded context on every
// successful proxy cycle (measured by BenchmarkHandler_PublicRoute).
// Error branches call this at the emit site instead, so the cost is
// paid only when a line is actually written. The field set matches
// the previous eager binding: request_id, traceparent, route.
func (h *Handler) requestLogger(in *ServeInput, route *routing.Route) zerolog.Logger {
	return h.cfg.Logger.With().
		Str("request_id", in.RequestID).
		Str("traceparent", in.Traceparent).
		Str("route", route.Method+":"+route.PathTemplate).
		Logger()
}

// allowProbeMethods is the verb set probed to build a 405 response's
// Allow header. Matches the registrable verb union of the wire
// contract (registry entry / SDK HTTP method type). Fixed order keeps
// the Allow header deterministic.
var allowProbeMethods = [...]string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"}

// allowedMethods returns the verbs for which the table matches path,
// excluding the verb the request already tried (its Lookup miss is
// what brought the caller here). Probing Lookup per verb is
// template-aware — a wrong-method request to "/users/42" finds the
// route registered as "/users/:id" — unlike Table.Methods, whose
// exact-template match cannot see parameterized routes and would
// misclassify the mismatch as 404. The probe loop runs only on the
// lookup-miss path, never on proxied traffic.
func allowedMethods(table routing.Table, tried, path string) []string {
	var allow []string
	for _, method := range allowProbeMethods {
		if method == tried {
			continue
		}
		if _, _, ok := table.Lookup(method, path); ok {
			allow = append(allow, method)
		}
	}

	return allow
}

// rateLimitNeedsClaims reports whether the route's keyBy chain
// contains a claims-keyed strategy (user:*). Decides the rate-limit
// gate's position relative to the auth flow: claims-keyed chains must
// gate after the verifier supplies claims; every other chain gates
// before it. See the ordering rationale in Handle.
func rateLimitNeedsClaims(route routing.Route) bool {
	if route.RateLimit == nil {
		return false
	}
	for _, key := range route.RateLimit.KeyBy {
		if strings.HasPrefix(key, "user:") {
			return true
		}
	}

	return false
}

// stampCORSOnResult applies the route's CORS policy to a short-circuit
// or error result before it returns to the client. Every response of
// a matched CORS route — 400/401/429/5xx included — is subject to the
// browser's CORS check: without Access-Control-Allow-Origin the
// cross-origin caller cannot read the status (a session-expired 401
// becomes indistinguishable from a network outage) or the exposed
// X-RateLimit-*/Retry-After headers the route contract promises. The
// stamp also appends Vary: Origin, which the origin-varying error
// content needs for shared-cache correctness. Nil-safe on both the
// result and the CORS config so call sites stay single-line.
func stampCORSOnResult(result *ServeResult, cors *registry.CORSMeta, origin string) *ServeResult {
	if result == nil || cors == nil {
		return result
	}
	stampResponseCORS(result.Headers, cors, origin)

	return result
}

// isJSONNull reports whether raw is the JSON null literal. Used to
// normalise a verifier's explicit `"body": null` (which decodes to a
// non-empty json.RawMessage) into absent claims at the auth-flow
// boundary. Whitespace tolerance costs nothing and guards against
// decoders that preserve surrounding space in raw values.
func isJSONNull(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}
