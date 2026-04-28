package proxy

import (
	"context"
	"time"
)

// NatsRequester is the narrow contract the Handler needs from the
// NATS transport layer. Defining it here (in the proxy package) means
// the Handler depends on the abstraction, not a concrete nats.Conn —
// classic Go dependency inversion, and the reason the Handler can be
// unit-tested without any NATS server running.
//
// Implementations MUST be safe for concurrent use; Handler.Handle may
// be called from many goroutines in parallel.
//
// The ctx argument propagates the inbound HTTP request context through
// to the NATS round trip. If the HTTP client disconnects mid-flight,
// the cancellation reaches the NATS layer and the in-flight request is
// torn down instead of running to its full timeout. The timeout
// argument is the per-route hard deadline; implementations are
// expected to derive a child context whose deadline is min(ctx
// deadline, now+timeout), so neither budget can exceed the other.
type NatsRequester interface {
	Request(ctx context.Context, subject string, payload []byte, timeout time.Duration) ([]byte, error)
}
