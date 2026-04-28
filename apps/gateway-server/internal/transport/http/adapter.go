// Package http wires the cloudwego/hertz HTTP server to the
// framework-agnostic proxy.Handler. It owns two responsibilities only:
//
//  1. Translating a Hertz *app.RequestContext into a proxy.ServeInput
//     (method, path, headers, query, body, request-id, remote addr).
//  2. Writing the resulting proxy.ServeResult back onto the Hertz
//     response (status, headers, body), stamping content-type and
//     x-request-id on every response regardless of what the upstream
//     handler returned.
//
// Deliberately thin — no middleware, no routing, no business logic.
// Recovery, access logging, metrics, and tracing are layered on
// separately so this translation layer stays easy to audit against
// the framework-agnostic proxy layer above it.
package http

import (
	"bytes"
	"context"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/observability"
	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/proxy"
)

// hopByHopHeaders lists the connection-control headers that MUST NOT
// be forwarded to upstream Nest handlers. Defined in RFC 7230 §6.1;
// the gateway strips them on the way in so upstream services see
// only end-to-end headers, which is the expected contract for a
// well-behaved HTTP proxy. Host is included alongside the standard
// nine because forwarding the gateway's own Host header to a
// downstream RPC is meaningless and would only confuse handlers
// that key on it for multi-tenancy.
//
// Note: the canonical spelling is "trailer" (singular), per RFC 7230.
// The older "trailers" spelling surfaces in some HTTP/2 stacks so both
// are listed to stay defensive against non-canonical inputs.
var hopByHopHeaders = map[string]struct{}{
	"connection":          {},
	"keep-alive":          {},
	"proxy-authenticate":  {},
	"proxy-authorization": {},
	"te":                  {},
	"trailer":             {},
	"trailers":            {},
	"transfer-encoding":   {},
	"upgrade":             {},
	"host":                {},
}

// initialHeadersCap and initialQueryCap pre-size the adapter's
// working maps based on observed-typical request shapes. The numbers
// match the envelope pool constants in internal/proxy/pool.go so the
// adapter and pool allocate at the same scale and avoid a resize on
// the common case.
const (
	initialHeadersCap = 16
	initialQueryCap   = 4
)

// NewHertzAdapter returns a Hertz HandlerFunc that drives a single
// request through the proxy pipeline. The returned closure captures
// handler by reference, so callers may share one adapter across the
// entire Hertz route tree — there is no per-request state stored on
// the adapter itself.
//
// The standard-library context Hertz passes as the first argument is
// forwarded into proxy.Handler.Handle so any cancellation Hertz
// surfaces (client disconnect, server shutdown deadline) propagates
// into the rate-limit check and the upstream NATS round trip. Without
// this propagation an in-flight request would continue consuming
// downstream budget for the full per-route timeout even after the
// client gave up, which is exactly the bug the ctx-aware request path
// is meant to fix.
func NewHertzAdapter(handler *proxy.Handler) app.HandlerFunc {
	return func(stdCtx context.Context, ctx *app.RequestContext) {
		input := buildServeInput(ctx)
		result := handler.Handle(stdCtx, input)
		writeServeResult(ctx, result)
	}
}

// buildServeInput translates a Hertz request context into the
// framework-agnostic proxy.ServeInput shape. All header keys are
// lowercased so downstream code can key on canonical form without
// knowing which framework produced them. Hop-by-hop headers are
// dropped. Query values are collected into the typed QueryValue
// union so single-occurrence keys marshal as strings and repeated
// keys marshal as arrays — preserving the TypeScript
// `string | readonly string[]` contract on the wire.
//
// The X-Request-Id response header is stamped here, BEFORE the
// proxy handler runs, so even error responses written further down
// the pipeline carry the correlator. The request id itself is also
// returned in the ServeInput so the proxy can echo it inside the
// envelope meta block.
func buildServeInput(ctx *app.RequestContext) *proxy.ServeInput {
	method := string(ctx.Method())
	path := string(ctx.Path())

	// Hertz's VisitAll fires once per (key, value) pair, so a request
	// carrying repeated headers (e.g. multiple Cookie lines) would
	// otherwise lose every value but the last under a plain map
	// assignment. RFC 7230 §3.2.2 allows a receiver to combine
	// repeated field values into a single ", "-joined string, so
	// merge on the way in: upstream handlers see one header with all
	// observations preserved, and the ServeInput.Headers surface stays
	// a flat map[string]string for every other consumer.
	//
	// Cookie is the documented exception (RFC 6265 §5.4): cookie pairs
	// MUST be joined with "; " — not ", " — so a server-side parser
	// (including the gateway's own extractCookie helper) sees a
	// well-formed cookie header rather than one whose pairs run
	// together with comma-comma confusion.
	headers := make(map[string]string, initialHeadersCap)
	ctx.Request.Header.VisitAll(func(key, value []byte) {
		lowerKey := string(bytes.ToLower(key))
		if _, skip := hopByHopHeaders[lowerKey]; skip {
			return
		}

		v := string(value)
		if existing, ok := headers[lowerKey]; ok {
			headers[lowerKey] = existing + headerJoinSeparator(lowerKey) + v

			return
		}

		headers[lowerKey] = v
	})

	query := collectQueryValues(ctx)

	requestID := observability.NewRequestID()
	ctx.Response.Header.Set("X-Request-Id", requestID)

	return &proxy.ServeInput{
		Method:      method,
		Path:        path,
		Body:        ctx.Request.Body(),
		Query:       query,
		Headers:     headers,
		RequestID:   requestID,
		Traceparent: headers["traceparent"],
		RemoteAddr:  resolveRemoteAddr(ctx),
		ReceivedAt:  time.Now().UnixMilli(),
	}
}

// collectQueryValues walks the Hertz query arguments and returns a
// map keyed on the raw parameter name with values wrapped in the
// typed QueryValue union. Keys observed exactly once become the
// scalar Single variant; keys observed two or more times become the
// slice Multi variant, preserving "repeated" semantics so the
// upstream handler's Array.isArray() discriminator still works.
//
// Two-pass collection — accumulate into an intermediate
// map[string][]string, then convert — is deliberate: Hertz's
// VisitAll callback fires once per (key, value) pair, and attempting
// to make the union decision in the callback requires mutating the
// target map mid-iteration, which is error-prone and harder to read.
func collectQueryValues(ctx *app.RequestContext) map[string]proxy.QueryValue {
	accumulator := make(map[string][]string, initialQueryCap)
	ctx.QueryArgs().VisitAll(func(key, value []byte) {
		k := string(key)
		accumulator[k] = append(accumulator[k], string(value))
	})

	result := make(map[string]proxy.QueryValue, len(accumulator))
	for k, values := range accumulator {
		if len(values) == 1 {
			result[k] = proxy.NewQueryValueString(values[0])
			continue
		}
		result[k] = proxy.NewQueryValueStrings(values)
	}
	return result
}

// writeServeResult copies a proxy.ServeResult onto the Hertz response
// buffer. The content-type header is forced to application/json AFTER
// the caller-supplied headers are applied so the gateway always owns
// the wire format — an upstream handler cannot change it to anything
// else, which is a deliberate anti-spoofing measure that lets HTTP
// clients parse the body without sniffing.
//
// Header.Add is used instead of Header.Set so each slice entry lands
// as a separate header line on the client response. The critical
// case is Set-Cookie: Hertz's setSpecialHeader routes every Add on
// "Set-Cookie" through its per-cookie slot (internally an append),
// so calling Add twice yields two cookie lines — exactly the RFC
// 6265 shape browsers expect. Single-value headers with a one-element
// slice land as one Add call, equivalent to Set on an empty slot.
//
// The `x-request-id` slot is explicitly cleared before the Add loop
// runs. `buildServeInput` stamps the correlator on `ctx.Response`
// up front so a panic between input-build and writeServeResult
// still gives the error response a correlator, but the proxy layer
// then re-emits it via `result.Headers` — without the Del, Add
// would produce two identical `X-Request-Id` response headers on
// every request. Clearing only this specific slot keeps the Del
// targeted to the header the adapter itself stamped; other
// header names reach the Add loop untouched and preserve
// whatever semantics Hertz middleware may have configured.
func writeServeResult(ctx *app.RequestContext, result *proxy.ServeResult) {
	ctx.Response.Header.Del("X-Request-Id")

	for key, values := range result.Headers {
		for _, value := range values {
			ctx.Response.Header.Add(key, value)
		}
	}
	ctx.Response.Header.SetContentType(consts.MIMEApplicationJSON)

	// Gateway-owned error bodies (404/502/503/504/429/...) are
	// transient infrastructure failures — stamp `Cache-Control:
	// no-store` so intermediate caches never memoize them. Handler-
	// thrown errors are untouched because their cache policy is part
	// of the application contract, not the gateway's to override.
	if result.GatewayOwnedBody {
		ctx.Response.Header.Set("Cache-Control", "no-store")
	}

	ctx.SetStatusCode(result.Status)
	ctx.Response.SetBody(result.Body)
}

// headerJoinSeparator returns the delimiter used when merging
// repeated occurrences of the same header into one value.
//
// RFC 7230 §3.2.2 allows a receiver to combine multiple field-value
// observations into one comma-joined string for every header except
// Set-Cookie. Cookie itself uses "; " per RFC 6265 §5.4 — the
// individual cookie-pair delimiter — so a request carrying multiple
// Cookie lines lands in headers["cookie"] as a single, well-formed
// cookie string that the gateway's extractCookie helper (and any
// upstream consumer) can parse without confusing comma-separated
// pairs with intra-cookie commas.
func headerJoinSeparator(lowerKey string) string {
	if lowerKey == "cookie" {
		return "; "
	}

	return ", "
}

// resolveRemoteAddr returns the client IP the handler should see.
//
// The trusted-proxy middleware stamps the resolved IP on the
// request context via ctx.Set(clientIPUserKey, ...). This helper
// reads that value; if the middleware did not run (unit tests that
// drive the adapter directly, or a future startup path that forgets
// to register it) we fall back to Hertz's built-in ClientIP() so
// the request still serves with a best-effort IP.
//
// The empty-string guard exists because a buggy middleware writing
// "" would otherwise propagate an empty RemoteAddr into the
// envelope meta — the fallback preserves the "always return
// something" contract.
func resolveRemoteAddr(ctx *app.RequestContext) string {
	if raw, ok := ctx.Get(clientIPUserKey); ok {
		if v, ok := raw.(string); ok && v != "" {
			return v
		}
	}

	return ctx.ClientIP()
}
