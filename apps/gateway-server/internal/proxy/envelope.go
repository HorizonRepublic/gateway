// Package proxy is the request orchestration layer. It encodes HTTP
// requests into GatewayRequest envelopes, issues Core NATS RPC calls,
// and decodes reply envelopes back into HTTP responses. The encoder,
// decoder, and handler types defined here are consumed by the NATS
// and HTTP transport layers that live alongside this package.
package proxy

import "encoding/json"

// RouteContext mirrors the TypeScript IGatewayRouteContext. Carries the
// matched route information that Nest handlers may introspect via the
// @GatewayRoute param decorator.
type RouteContext struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	MatchedPath string `json:"matchedPath"`
}

// RequestMeta mirrors TypeScript IGatewayRequestMeta. Includes request
// identification, optional W3C trace propagation, caller address, the
// gateway-side receive timestamp (unix ms), and the effective NATS
// request timeout in milliseconds so the Nest handler can budget
// internal work.
type RequestMeta struct {
	RequestID   string `json:"requestId"`
	Traceparent string `json:"traceparent,omitempty"`
	RemoteAddr  string `json:"remoteAddr"`
	ReceivedAt  int64  `json:"receivedAt"`
	TimeoutMs   int64  `json:"timeoutMs"`
}

// GatewayRequest is the envelope sent from the gateway to Nest over
// Core NATS request/reply. Mirror of the TypeScript IGatewayRequest
// type from the gateway-sdk.
//
// Body is json.RawMessage so the gateway never fully deserializes the
// payload — the raw bytes are forwarded to NATS as-is, saving an
// allocation round-trip per request and giving each Nest handler
// complete control over its own body shape.
//
// Instances are pooled via sync.Pool (see pool.go). Acquire them with
// acquireEnvelope and return them with releaseEnvelope after the reply
// has been fully processed.
type GatewayRequest struct {
	Route   RouteContext          `json:"route"`
	Params  map[string]string     `json:"params"`
	Query   map[string]QueryValue `json:"query"`
	Headers map[string]string     `json:"headers"`
	Body    json.RawMessage       `json:"body"`
	Meta    RequestMeta           `json:"meta"`
	// Auth carries the verifier claims for a protected route. nil on
	// public routes and on optional-auth routes where the caller was
	// anonymous. When non-nil, the encoder emits the field as
	// `"auth":<verifier reply body>` — the raw JSON bytes are forwarded
	// verbatim, same zero-copy contract as Body. Omitted entirely when
	// nil so public-route envelopes stay byte-compatible with pre-auth
	// gateway builds.
	Auth json.RawMessage `json:"auth,omitempty"`
}

// GatewayReply is the envelope Nest sends back. Mirror of the
// TypeScript IGatewayReply type from the gateway-sdk.
//
// Headers is a multi-value map so RFC-mandated multi-value response
// headers (Set-Cookie, Vary, Link) survive the NATS wire verbatim.
// Single-value headers live here too, wrapped in a one-element
// slice. Request envelopes stay single-value — Fastify already
// joins multi-value request headers per RFC 7230 §3.2.2 before they
// reach the transport.
//
// Body is json.RawMessage for the same zero-deserialize reason as
// GatewayRequest.Body — the gateway writes it verbatim to the HTTP
// response body.
type GatewayReply struct {
	Status  int                 `json:"status"`
	Headers map[string][]string `json:"headers"`
	Body    json.RawMessage     `json:"body"`
}

// reset returns a GatewayRequest to a zero-valued state ready for the
// next request. Maps are CLEARED rather than reallocated so the
// backing arrays are retained and the next acquirer pays zero map
// allocations. The Body slice is set to nil — callers MUST NOT retain
// references to the underlying bytes after release.
func (r *GatewayRequest) reset() {
	r.Route = RouteContext{}
	for k := range r.Params {
		delete(r.Params, k)
	}
	for k := range r.Query {
		delete(r.Query, k)
	}
	for k := range r.Headers {
		delete(r.Headers, k)
	}
	r.Body = nil
	r.Auth = nil
	r.Meta = RequestMeta{}
}
