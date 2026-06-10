package nats

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rs/zerolog"
	"github.com/sony/gobreaker"
	"golang.org/x/sync/semaphore"
)

// ErrInflightQueueFull is returned when the in-flight semaphore could
// not be acquired within the queue timeout. Deliberately NOT a
// context.DeadlineExceeded wrapper: the proxy handler classifies
// timeout-shaped errors as 504 Gateway Timeout, and "the gateway's
// own admission queue is full" is a 503 Service Unavailable condition
// — the upstream was never consulted.
var ErrInflightQueueFull = errors.New("nats requester: in-flight queue full")

// ErrCircuitOpen is returned when the circuit breaker is open (or the
// half-open probe budget is exhausted) and the request was fast-failed
// without touching the NATS connection. Maps to 503 downstream.
var ErrCircuitOpen = errors.New("nats requester: circuit open")

// innerRequester is the narrow contract ResilientRequester decorates.
// Identical to proxy.NatsRequester; declared locally so this package
// does not import the consumer (which would invert the dependency
// direction proxy.NatsRequester exists to preserve).
type innerRequester interface {
	Request(ctx context.Context, subject string, payload []byte, timeout time.Duration) ([]byte, error)
}

// ResilientConfig carries the admission-control knobs for
// NewResilientRequester. Zero values disable the corresponding layer.
type ResilientConfig struct {
	// MaxInflight caps concurrent NATS requests across the whole
	// gateway process. 0 disables the cap. Bounding in-flight
	// requests prevents OOM under traffic spikes when the NATS
	// connection's internal pending buffer saturates and every
	// blocked request pins a goroutine + envelope buffer.
	MaxInflight int

	// QueueTimeout bounds how long a request waits for a semaphore
	// slot before being rejected with ErrInflightQueueFull. Keeping
	// it well under the route timeout converts a saturation episode
	// into fast 503s instead of a latency cliff.
	QueueTimeout time.Duration

	// BreakerEnabled wires a gobreaker.CircuitBreaker around the
	// inner requester. During a NATS outage the breaker fast-fails
	// requests after FailureThreshold consecutive failures instead
	// of letting every incoming request pile up a goroutine for the
	// full timeout — the 300k-goroutine explosion mode.
	BreakerEnabled bool

	// FailureThreshold is the consecutive-failure count that trips
	// the breaker open.
	FailureThreshold uint32

	// RecoveryTimeout is how long the breaker stays open before
	// moving to half-open and letting probe requests through.
	RecoveryTimeout time.Duration

	// HalfOpenProbes is how many concurrent probe requests the
	// half-open state admits; their collective success closes the
	// breaker, any failure re-opens it.
	HalfOpenProbes uint32
}

// ResilientRequester decorates an inner requester with two admission
// layers, outermost first:
//
//  1. In-flight semaphore — bounds concurrency, sheds load with
//     ErrInflightQueueFull when the queue timeout expires.
//  2. Circuit breaker — fast-fails with ErrCircuitOpen while NATS is
//     known-sick, so a dead bus costs one error check instead of a
//     pinned goroutine per incoming request.
//
// The semaphore sits OUTSIDE the breaker on purpose: when the breaker
// is open, fast-fails release their slot immediately, so the
// semaphore never becomes the bottleneck during an outage; and when
// NATS recovers, the half-open probes are already admission-bounded.
type ResilientRequester struct {
	inner        innerRequester
	sem          *semaphore.Weighted
	queueTimeout time.Duration
	breaker      *gobreaker.CircuitBreaker
}

// NewResilientRequester wraps inner with the layers enabled in cfg.
// With every layer disabled the wrapper adds two nil checks per
// request — cheap enough to keep the wiring unconditional.
func NewResilientRequester(
	inner innerRequester,
	cfg ResilientConfig,
	logger zerolog.Logger,
) *ResilientRequester {
	r := &ResilientRequester{inner: inner, queueTimeout: cfg.QueueTimeout}

	if cfg.MaxInflight > 0 {
		r.sem = semaphore.NewWeighted(int64(cfg.MaxInflight))
	}

	if cfg.BreakerEnabled {
		r.breaker = gobreaker.NewCircuitBreaker(gobreaker.Settings{
			Name:        "nats-request",
			MaxRequests: cfg.HalfOpenProbes,
			Timeout:     cfg.RecoveryTimeout,
			ReadyToTrip: func(counts gobreaker.Counts) bool {
				return counts.ConsecutiveFailures >= cfg.FailureThreshold
			},
			OnStateChange: func(name string, from, to gobreaker.State) {
				logger.Warn().
					Str("breaker", name).
					Str("from", from.String()).
					Str("to", to.String()).
					Msg("nats: circuit breaker state change")
			},
		})
	}

	return r
}

// Request applies the admission layers and delegates to the inner
// requester. See the type docs for layer ordering rationale.
func (r *ResilientRequester) Request(
	ctx context.Context,
	subject string,
	payload []byte,
	timeout time.Duration,
) ([]byte, error) {
	if r.sem != nil {
		acquireCtx, cancel := context.WithTimeout(ctx, r.queueTimeout)
		err := r.sem.Acquire(acquireCtx, 1)
		cancel()

		if err != nil {
			// Both queue-timeout expiry and caller-context
			// cancellation land here; the distinction does not
			// matter to the caller — the request was never
			// admitted, so 503 semantics apply either way.
			return nil, fmt.Errorf("%w (max in-flight reached, waited %s)", ErrInflightQueueFull, r.queueTimeout)
		}
		defer r.sem.Release(1)
	}

	if r.breaker == nil {
		data, err := r.inner.Request(ctx, subject, payload, timeout)
		if err != nil {
			// %w keeps the errors.Is chain intact — the proxy's
			// 504 classification depends on reaching
			// nats.ErrTimeout through this wrapper.
			return nil, fmt.Errorf("resilient requester: %w", err)
		}

		return data, nil
	}

	result, err := r.breaker.Execute(func() (any, error) {
		return r.inner.Request(ctx, subject, payload, timeout)
	})
	if err != nil {
		if errors.Is(err, gobreaker.ErrOpenState) || errors.Is(err, gobreaker.ErrTooManyRequests) {
			return nil, fmt.Errorf("%w: %w", ErrCircuitOpen, err)
		}

		return nil, fmt.Errorf("resilient requester: %w", err)
	}

	data, ok := result.([]byte)
	if !ok {
		return nil, fmt.Errorf("nats requester: unexpected breaker result type %T", result)
	}

	return data, nil
}
