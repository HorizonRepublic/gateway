package nats

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"github.com/sony/gobreaker"
	"golang.org/x/sync/semaphore"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/registry"
)

// ErrInflightQueueFull is returned when the in-flight semaphore could
// not be acquired within the queue timeout. Deliberately NOT a
// context.DeadlineExceeded wrapper: the proxy handler classifies
// timeout-shaped errors as 504 Gateway Timeout, and "the gateway's
// own admission queue is full" is a 503 Service Unavailable condition
// — the upstream was never consulted.
var ErrInflightQueueFull = errors.New("nats requester: in-flight queue full")

// ErrCircuitOpen is returned when the circuit breaker for the target
// upstream service is open (or its half-open probe budget is
// exhausted) and the request was fast-failed without touching the
// NATS connection. Maps to 503 downstream.
var ErrCircuitOpen = errors.New("nats requester: circuit open")

// defaultMaxBreakerSubjects is the dedicated-breaker cardinality cap
// applied when ResilientConfig.MaxBreakerSubjects is unset. Sized far
// above any realistic upstream-service count (tens, not hundreds)
// while bounding worst-case memory to a few hundred KiB of breaker
// state if the registry is compromised.
const defaultMaxBreakerSubjects = 1024

// sharedBreakerName labels the fallback breaker in state-change logs
// and snapshots. The "!" prefix cannot collide with a real service
// name: NATS subject tokens never start the service prefix with it
// under the nestjs-jetstream convention.
const sharedBreakerName = "!shared-overflow"

// innerRequester is the narrow contract ResilientRequester decorates.
// Identical to proxy.NatsRequester; declared locally so this package
// does not import the consumer (which would invert the dependency
// direction proxy.NatsRequester exists to preserve).
type innerRequester interface {
	Request(ctx context.Context, subject string, payload []byte, timeout time.Duration) ([]byte, error)
}

// ResilientConfig carries the admission-control knobs for
// NewResilientRequester. Zero values disable the corresponding layer
// (except MaxBreakerSubjects, which falls back to
// defaultMaxBreakerSubjects when the breaker is enabled).
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

	// BreakerEnabled wires one gobreaker.CircuitBreaker per upstream
	// service around the inner requester. During an upstream outage
	// the affected service's breaker fast-fails requests after
	// FailureThreshold consecutive failures instead of letting every
	// incoming request pile up a goroutine for the full timeout —
	// while routes to healthy services keep flowing.
	BreakerEnabled bool

	// FailureThreshold is the consecutive-failure count that trips
	// a service's breaker open. Applied independently per upstream
	// service.
	FailureThreshold uint32

	// RecoveryTimeout is how long an open breaker stays open before
	// moving to half-open and letting probe requests through.
	// Applied independently per upstream service.
	RecoveryTimeout time.Duration

	// HalfOpenProbes is how many concurrent probe requests the
	// half-open state admits; their collective success closes the
	// breaker, any failure re-opens it. Applied independently per
	// upstream service.
	HalfOpenProbes uint32

	// MaxBreakerSubjects caps how many dedicated per-service
	// breakers may be created. Services beyond the cap share a
	// single fallback breaker (coarser blast radius, bounded
	// memory), so a compromised or buggy registry emitting unbounded
	// subject cardinality cannot grow the breaker map without limit.
	// 0 applies defaultMaxBreakerSubjects.
	MaxBreakerSubjects int
}

// BreakerSnapshot is a point-in-time view of one circuit breaker,
// exposed for operator surfaces (metrics, admin endpoints). Snapshots
// are internally consistent per breaker but not across breakers — the
// set is collected without a global lock.
type BreakerSnapshot struct {
	// Service is the upstream service identity the breaker guards
	// (registry.ServiceFromSubject of the subjects it has seen), or
	// sharedBreakerName for the overflow fallback.
	Service string
	// Shared reports whether this is the fallback breaker that
	// services beyond MaxBreakerSubjects collectively land on.
	Shared bool
	// State is the breaker state (closed / half-open / open).
	State gobreaker.State
	// Counts are gobreaker's internal counters for the current
	// generation (requests, failures, consecutive streaks).
	Counts gobreaker.Counts
}

// ResilientRequester decorates an inner requester with two admission
// layers, outermost first:
//
//  1. In-flight semaphore — bounds concurrency, sheds load with
//     ErrInflightQueueFull when the queue timeout expires.
//  2. Per-service circuit breakers — fast-fail with ErrCircuitOpen
//     while the target upstream is known-sick, so one dead service
//     costs one error check per request instead of a pinned goroutine,
//     and — critically — does NOT 503 routes served by healthy
//     services. The breaker key is the upstream service identity
//     derived from the subject prefix (registry.ServiceFromSubject),
//     because one service = one deployment = one failure domain;
//     keying on the full subject would let a single broken handler
//     hide its service's sickness from sibling routes, and a single
//     global breaker (the previous design) let one dead service take
//     down every route on the gateway.
//
// The semaphore sits OUTSIDE the breakers on purpose: when a breaker
// is open, fast-fails release their slot immediately, so the
// semaphore never becomes the bottleneck during an outage; and when
// an upstream recovers, the half-open probes are already
// admission-bounded.
type ResilientRequester struct {
	inner        innerRequester
	sem          *semaphore.Weighted
	queueTimeout time.Duration
	logger       zerolog.Logger

	// breakerCfg holds the per-breaker knobs used for lazy creation.
	// Only consulted when breakerEnabled is true.
	breakerCfg     ResilientConfig
	breakerEnabled bool

	// breakers maps service identity -> *gobreaker.CircuitBreaker.
	// sync.Map fits the read-mostly profile: the key set stabilises
	// after warmup (services come from the registry), after which
	// every request is a lock-free Load hit; a sharded-mutex map
	// would pay lock acquisition on the hot path for no benefit.
	breakers sync.Map

	// breakerMu serialises the miss path only (first request per
	// service), keeping the dedicated-breaker count accounting exact
	// so the cardinality cap is a hard bound, not a best effort.
	breakerMu    sync.Mutex
	breakerCount int

	// sharedBreaker absorbs services beyond MaxBreakerSubjects.
	// Created eagerly at construction so the overflow path is a
	// plain field read.
	sharedBreaker *gobreaker.CircuitBreaker
}

// NewResilientRequester wraps inner with the layers enabled in cfg.
// With every layer disabled the wrapper adds two nil checks per
// request — cheap enough to keep the wiring unconditional.
func NewResilientRequester(
	inner innerRequester,
	cfg ResilientConfig,
	logger zerolog.Logger,
) *ResilientRequester {
	r := &ResilientRequester{
		inner:        inner,
		queueTimeout: cfg.QueueTimeout,
		logger:       logger,
	}

	if cfg.MaxInflight > 0 {
		r.sem = semaphore.NewWeighted(int64(cfg.MaxInflight))
	}

	if cfg.BreakerEnabled {
		if cfg.MaxBreakerSubjects <= 0 {
			cfg.MaxBreakerSubjects = defaultMaxBreakerSubjects
		}
		r.breakerCfg = cfg
		r.breakerEnabled = true
		r.sharedBreaker = r.newBreaker(sharedBreakerName)
	}

	return r
}

// newBreaker constructs one circuit breaker carrying the configured
// knobs. name is the service identity (or sharedBreakerName) and
// surfaces in state-change logs and snapshots.
func (r *ResilientRequester) newBreaker(name string) *gobreaker.CircuitBreaker {
	return gobreaker.NewCircuitBreaker(gobreaker.Settings{
		Name:        name,
		MaxRequests: r.breakerCfg.HalfOpenProbes,
		Timeout:     r.breakerCfg.RecoveryTimeout,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			return counts.ConsecutiveFailures >= r.breakerCfg.FailureThreshold
		},
		// context.Canceled means the CLIENT went away (HTTP peer
		// disconnect or gateway shutdown) — it carries no signal
		// about upstream health, so counting it as a failure would
		// let a burst of impatient clients open the breaker for a
		// perfectly healthy service. context.DeadlineExceeded (the
		// route timeout fired while the upstream stayed silent) is
		// genuine upstream sickness and still counts. The error is
		// returned to the caller either way — IsSuccessful only
		// affects breaker accounting, not response semantics.
		IsSuccessful: func(err error) bool {
			return err == nil || errors.Is(err, context.Canceled)
		},
		OnStateChange: func(name string, from, to gobreaker.State) {
			r.logger.Warn().
				Str("breaker", name).
				Str("from", from.String()).
				Str("to", to.String()).
				Msg("nats: circuit breaker state change")
		},
	})
}

// breakerFor returns the circuit breaker guarding the upstream service
// that owns subject, lazily creating it on first sight. Once the
// dedicated-breaker cap is reached, new services land on the shared
// fallback breaker instead — bounded memory beats per-service blast
// radius when the registry misbehaves.
//
// Hot path: a single lock-free sync.Map.Load. The mutex is taken only
// on the first request a service ever makes.
func (r *ResilientRequester) breakerFor(subject string) *gobreaker.CircuitBreaker {
	key := registry.ServiceFromSubject(subject)

	if b, ok := r.breakers.Load(key); ok {
		return b.(*gobreaker.CircuitBreaker)
	}

	r.breakerMu.Lock()
	defer r.breakerMu.Unlock()

	// Re-check under the lock: another goroutine may have created the
	// breaker between the lock-free miss and lock acquisition.
	if b, ok := r.breakers.Load(key); ok {
		return b.(*gobreaker.CircuitBreaker)
	}

	if r.breakerCount >= r.breakerCfg.MaxBreakerSubjects {
		return r.sharedBreaker
	}

	b := r.newBreaker(key)
	r.breakers.Store(key, b)
	r.breakerCount++

	return b
}

// BreakerSnapshots returns a point-in-time view of every breaker: all
// dedicated per-service breakers plus the shared overflow fallback.
// Intended for operator surfaces (metrics scrape, admin endpoint);
// allocates one slice per call, so keep it off the request hot path.
// Returns nil when the breaker layer is disabled.
func (r *ResilientRequester) BreakerSnapshots() []BreakerSnapshot {
	if !r.breakerEnabled {
		return nil
	}

	var out []BreakerSnapshot
	r.breakers.Range(func(key, value any) bool {
		b := value.(*gobreaker.CircuitBreaker)
		out = append(out, BreakerSnapshot{
			Service: key.(string),
			State:   b.State(),
			Counts:  b.Counts(),
		})

		return true
	})

	out = append(out, BreakerSnapshot{
		Service: sharedBreakerName,
		Shared:  true,
		State:   r.sharedBreaker.State(),
		Counts:  r.sharedBreaker.Counts(),
	})

	return out
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

	if !r.breakerEnabled {
		data, err := r.inner.Request(ctx, subject, payload, timeout)
		if err != nil {
			// %w keeps the errors.Is chain intact — the proxy's
			// 504 classification depends on reaching
			// nats.ErrTimeout through this wrapper.
			return nil, fmt.Errorf("resilient requester: %w", err)
		}

		return data, nil
	}

	result, err := r.breakerFor(subject).Execute(func() (any, error) {
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
