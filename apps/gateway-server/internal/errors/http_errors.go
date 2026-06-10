// Package errors exposes pre-encoded HTTP error response bodies for
// use on the gateway's hot path.
//
// Computing the JSON once at init() time and reusing both the status
// code and the byte slice for every error response eliminates
// allocations on the error path. At gateway scale — where a small
// percentage of every traffic spike returns 404/504/502 — this is
// measurably worth it in benchmarks, and it also guarantees that a
// given error class always ships the same wire shape regardless of
// which goroutine produced it.
//
// Each error is exposed as an HTTPError value rather than a loose
// pair of (int, []byte) variables so handler code passes a single
// symbolic argument and cannot accidentally pair a 404 body with a
// 500 status or vice versa.
package errors

import (
	"fmt"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/codec"
)

// HTTPError bundles an HTTP status code with a pre-encoded JSON body.
// Values of this type are constructed exactly once at package init()
// time and are safe to share across goroutines.
//
// Immutability is a package convention, not a type-system guarantee.
// Go has no read-only struct field, so external code COULD reassign
// Status or mutate the Body slice in place — doing so would corrupt
// every subsequent response that reads the same shared instance.
// Callers MUST treat both fields as read-only. The build factory
// produces a fresh slice per HTTPError so the package-level values
// do not alias each other; once init() returns, the slices are
// treated as frozen for the remainder of the process lifetime.
type HTTPError struct {
	// Status is the HTTP status code the gateway returns to the
	// client. Always a valid RFC 9110 status in the 400-599 range.
	// Treat as read-only; reassignment changes the wire shape every
	// other goroutine observes.
	Status int
	// Body is the pre-marshalled JSON response body. Always
	// non-empty. Treat as read-only; mutating the underlying byte
	// slice corrupts every subsequent response that reads the same
	// shared instance.
	Body []byte
}

// HTTP status constants extracted so the init() table reads as intent
// instead of raw numbers. Exported so handler code can reference them
// directly without hard-coding magic numbers, and to make the full
// set of statuses the gateway may produce obvious at a glance.
const (
	StatusBadRequest         = 400
	StatusTooManyRequests    = 429
	StatusNotFound           = 404
	StatusMethodNotAllowed   = 405
	StatusInternalError      = 500
	StatusBadGateway         = 502
	StatusServiceUnavailable = 503
	StatusGatewayTimeout     = 504
)

// Pre-encoded HTTP errors produced once at init() time. Every error
// that the proxy handler may emit is declared here so the complete
// set of gateway-owned error responses is auditable in a single file.
//
// Each body is a single-field JSON object of the form
// `{"error": "<RFC 9110 reason phrase>"}`. The reason phrase carries
// no implementation detail about the gateway or its upstream — it is
// exactly the standard HTTP status phrase a client would see from any
// reverse proxy. This keeps the wire minimal, avoids fingerprinting
// (no "gateway"/"upstream"/"route" wording beyond what HTTP status
// phrases already expose), and stays consistent with the SDK-side
// error shapes produced for handler-thrown HttpException instances.
var (
	// BadRequest is the 400 response returned when the inbound
	// request body is not a valid JSON document. The request
	// envelope forwarded to upstream handlers is itself one JSON
	// text (RFC 8259 §2), so a non-JSON body can never be embedded
	// — rejecting at intake keeps the failure a client-visible 400
	// instead of an opaque upstream parse error surfacing as 5xx.
	BadRequest HTTPError
	// NotFound is the 404 response returned when the routing table
	// has no match for the requested method+path combination.
	NotFound HTTPError
	// MethodNotAllowed is the 405 response returned when the path
	// matches at least one registered route but no route accepts the
	// request method. The HTTP adapter pairs this body with an
	// `Allow` response header listing the set of methods registered
	// for the same path, as required by RFC 9110 §15.5.6.
	MethodNotAllowed HTTPError
	// TooManyRequests is the 429 response returned when a client
	// exceeds the per-route rate limit configured in the handler
	// registry.
	TooManyRequests HTTPError
	// InternalError is the generic 500 response returned when the
	// proxy fails to encode the outbound envelope or hits an
	// unexpected internal condition.
	InternalError HTTPError
	// ServiceUnavailable is the 503 response returned when the
	// NATS round trip fails with any error other than a timeout
	// (connection drop, no-responders, transport closed, etc.).
	ServiceUnavailable HTTPError
	// GatewayTimeout is the 504 response returned when the NATS
	// round trip fails with nats.ErrTimeout — the upstream service
	// was reachable but did not reply within RequestTimeout.
	GatewayTimeout HTTPError
	// BadGateway is the 502 response returned when the upstream
	// service replied with a payload the decoder could not parse
	// or whose status field was out of the legal 100-599 range.
	BadGateway HTTPError
)

func init() {
	BadRequest = build(StatusBadRequest, "Bad Request")
	NotFound = build(StatusNotFound, "Not Found")
	MethodNotAllowed = build(StatusMethodNotAllowed, "Method Not Allowed")
	TooManyRequests = build(StatusTooManyRequests, "Too Many Requests")
	InternalError = build(StatusInternalError, "Internal Server Error")
	ServiceUnavailable = build(StatusServiceUnavailable, "Service Unavailable")
	GatewayTimeout = build(StatusGatewayTimeout, "Gateway Timeout")
	BadGateway = build(StatusBadGateway, "Bad Gateway")
}

// build marshals a single `{"error": reasonPhrase}` body through the
// shared codec and wraps the result in an HTTPError paired with the
// supplied status. Any marshalling failure is fatal — we are encoding
// a fixed-shape map with string values only, and a failure there
// indicates a corrupt build of the sonic/codec layer that should
// prevent the process from starting rather than silently serving
// empty bodies.
func build(status int, reasonPhrase string) HTTPError {
	body, err := codec.Marshal(map[string]string{
		"error": reasonPhrase,
	})
	if err != nil {
		panic(fmt.Sprintf("errors: failed to pre-encode %q: %v", reasonPhrase, err))
	}
	return HTTPError{Status: status, Body: body}
}
