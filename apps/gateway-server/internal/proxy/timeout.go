package proxy

import (
	"context"
	"errors"

	natsgo "github.com/nats-io/nats.go"
)

// isTimeoutErr reports whether err represents a NATS request timeout
// or a context-deadline expiry surfacing through the NATS layer.
//
// The check uses errors.Is against three sentinels:
//
//   - nats.ErrTimeout: emitted by the legacy nats.Conn.Request signature
//     (timeout-only) and by some internal nats.go paths.
//   - context.DeadlineExceeded: emitted by nats.Conn.RequestWithContext
//     when the supplied context's deadline expires before a reply lands.
//     This is the dominant case after the gateway moved its requester
//     onto the ctx-aware API so HTTP-client cancellation propagates
//     into NATS.
//   - context.Canceled: emitted when the caller cancels ctx explicitly
//     (e.g., the inbound HTTP client disconnects mid-flight). The
//     handler treats this as a timeout-class outcome rather than a
//     generic upstream failure because no upstream reply will ever
//     materialize for a cancelled request.
//
// Wrapped errors (e.g., `fmt.Errorf("nats request %q: %w", subject,
// err)` produced by the transport Requester) still match because
// errors.Is unwraps. Handler translates a positive result into a
// 504 Gateway Timeout response; anything else is treated as an
// upstream failure and returned as 503 Service Unavailable.
func isTimeoutErr(err error) bool {
	return errors.Is(err, natsgo.ErrTimeout) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, context.Canceled)
}

// isPayloadTooLargeErr reports whether err is nats.go's client-side
// rejection of a message exceeding the server-advertised max_payload
// (nats.ErrMaxPayload, raised at publish time before the message
// touches the wire). The handler maps a positive result to 413
// Content Too Large: the failure describes the request's size, not
// upstream health, so 503 would mislead clients into retrying a
// permanently-oversized request and pollute outage dashboards. The
// bootstrap's payload-budget check makes this branch unreachable for
// in-contract requests; it remains as defense-in-depth for residual
// envelope inflation (e.g. oversized verifier claims).
func isPayloadTooLargeErr(err error) bool {
	return errors.Is(err, natsgo.ErrMaxPayload)
}
