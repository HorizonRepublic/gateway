package nats

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	natsgo "github.com/nats-io/nats.go"
)

// Requester implements proxy.NatsRequester by round-robin-ing requests
// across a pool of nats.Conn instances.
//
// A single nats.Conn is goroutine-safe but funnels all sends through
// one socket, which becomes a contention point at very high RPS.
// Holding N parallel connections and distributing requests across them
// scales linearly up to NIC saturation. The pool size is configured
// via NATS_CONNECTION_POOL (default 1); only increase it when
// profiling shows socket-level contention, never as a speculative
// optimisation.
//
// The Requester is safe for concurrent use from any number of
// goroutines — the only shared state is the atomic round-robin
// counter, and each underlying nats.Conn is independently
// goroutine-safe.
type Requester struct {
	conns   []*natsgo.Conn
	counter atomic.Uint64
}

// errNoConns is returned by NewRequester when the caller supplied an
// empty connection slice. Construction-time validation avoids an
// unreachable divide-by-zero in the request path.
var errNoConns = errors.New("nats requester: at least one connection required")

// NewRequester constructs a Requester wrapping the supplied connections.
// At least one connection is required; an empty slice returns
// errNoConns so the caller can fail startup loudly.
func NewRequester(conns []*natsgo.Conn) (*Requester, error) {
	if len(conns) == 0 {
		return nil, errNoConns
	}
	return &Requester{conns: conns}, nil
}

// Request sends an RPC request to subject and waits for a reply.
//
// The supplied ctx propagates the inbound HTTP request lifetime down
// into the NATS round trip via nats.Conn.RequestWithContext. If the
// HTTP client disconnects mid-flight or the caller cancels ctx, the
// request returns immediately with the wrapped ctx error instead of
// running to the full timeout — matching the no-orphan-IO contract the
// gateway expects from every outbound dependency.
//
// The timeout argument is layered ON TOP of ctx: a child context with
// timeout is derived from ctx so the effective deadline is min(ctx
// deadline, now+timeout). Callers that already attached a deadline to
// ctx still get the per-route hard cap, and callers that pass a
// background ctx still get the per-route timeout. Cancellation from
// either the HTTP path or the per-route deadline tears down the
// in-flight request through the same code path.
//
// Errors are wrapped with the subject name and propagated verbatim
// from nats.go so callers can use errors.Is against nats.ErrTimeout
// (or context.DeadlineExceeded / context.Canceled, which nats.go
// surfaces from RequestWithContext) to discriminate timeouts from
// connection failures upstream.
//
// Invariant: this gateway intentionally sets NO user headers on
// outbound NATS messages — all per-request metadata (request id,
// traceparent, remote addr, timeout budget, auth claims, forwarded
// HTTP headers) travels in the JSON envelope body rendered by the
// proxy encoder. The envelope body is the single source of truth
// for what the downstream handler sees; mixing transport headers in
// would break the zero-trust header contract and require the
// SDK-side transport to merge two metadata planes on every call.
// Future contributors adding outbound header writes MUST first
// validate the chosen names against the SDK's reserved header set
// (error discrimination, retry metadata, reply-to routing) before
// changing this — a collision with a reserved name silently
// corrupts the downstream control plane.
func (r *Requester) Request(
	ctx context.Context,
	subject string,
	payload []byte,
	timeout time.Duration,
) ([]byte, error) {
	idx := r.counter.Add(1) % uint64(len(r.conns))

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	msg, err := r.conns[idx].RequestWithContext(reqCtx, subject, payload)
	if err != nil {
		return nil, fmt.Errorf("nats request %q: %w", subject, err)
	}
	return msg.Data, nil
}

// Close drains every underlying connection. Drain waits for in-flight
// subscriptions to finish before tearing down the socket, giving
// handlers a chance to complete cleanly on shutdown.
func (r *Requester) Close() {
	for _, c := range r.conns {
		_ = c.Drain()
	}
}
