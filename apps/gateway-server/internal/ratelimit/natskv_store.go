package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/rs/zerolog"
	"github.com/sony/gobreaker"
)

// kvEntry abstracts the subset of a NATS KeyValue entry the store
// needs: value bytes and the revision used for optimistic CAS.
type kvEntry interface {
	Value() []byte
	Revision() uint64
}

// kvAPI is the minimal NATS KeyValue surface consumed by
// NATSKVStore. Abstracted so tests can swap in a deterministic
// in-memory fake without a JetStream dependency.
//
// Create MUST return errCASConflict when the key already exists.
// Update MUST return errCASConflict when revision does not match
// the stored revision. Get MUST return errKVKeyNotFound when the
// key is absent.
type kvAPI interface {
	Get(ctx context.Context, key string) (kvEntry, error)
	Create(ctx context.Context, key string, value []byte) (uint64, error)
	Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error)
}

// Package-internal sentinel errors the kvAPI implementation signals
// to NATSKVStore. The store translates them to decisions (fresh
// bucket, CAS retry) without surfacing backend-specific shapes.
var (
	errKVKeyNotFound = errors.New("ratelimit: kv key not found")
	errCASConflict   = errors.New("ratelimit: kv cas conflict")
)

// ErrCircuitOpen is returned by NATSKVStore.Allow when the backend
// circuit breaker is in open or half-open-rejecting state. The
// gateway MUST consult FailPolicy to decide whether this surfaces
// as HTTP 503 or an allow pass-through.
var ErrCircuitOpen = errors.New("ratelimit: circuit open")

// ErrCASBudgetExhausted is returned when the CAS retry loop runs
// past its wall-clock budget without winning a write. Indicates
// sustained contention on a single key; callers SHOULD treat it
// the same as a transient backend outage for fail-policy purposes.
// Bumps the budgetExhausted counter (exposed as
// ratelimit_natskv_budget_exhausted) — distinct from the
// attempt-cap counter so operators can tell time-based exhaustion
// apart from a KV that always conflicts.
var ErrCASBudgetExhausted = errors.New("ratelimit: cas budget exhausted")

// errKVBudgetElapsed marks a backend call that consumed the store's
// OWN wall-clock budget while the caller's context was still healthy —
// the signature of a hung or pathologically slow KV. The wrap
// deliberately severs the errors.Is chain to context.DeadlineExceeded
// (the underlying cause is rendered with %v, not %w) so the breaker's
// IsSuccessful whitelist for caller-side cancellations cannot
// misclassify a dead backend as a benign termination. Without the
// distinction the breaker never opens on a hung JetStream — the exact
// outage it exists to short-circuit — and every request stalls for the
// full budget indefinitely.
var errKVBudgetElapsed = errors.New("ratelimit: kv call exceeded store budget")

// ErrCASMaxAttempts is returned when the CAS retry loop hits its
// hard attempt cap. Defensive bound; reaching it implies a broken
// KV (every attempt races), not ordinary contention. Bumps the
// casAttemptsExceeded counter (exposed as
// ratelimit_natskv_cas_attempts_exceeded), which is intentionally
// separate from the time-budget counter — the two failure modes
// have different operational responses.
var ErrCASMaxAttempts = errors.New("ratelimit: cas max attempts")

const (
	// casBudget caps the wall-clock time a single Allow call may
	// spend retrying on CAS conflicts. Chosen so that even under
	// heavy contention on one key the gateway's per-request latency
	// stays bounded.
	casBudget = 10 * time.Millisecond

	// maxCASAttempts is a hard defensive bound on retry count. The
	// wall-clock budget is the primary stop signal; this guard
	// exists to catch pathological loops (e.g., a KV that always
	// returns conflict) before they burn the whole budget.
	maxCASAttempts = 64

	// breakerFailures is the number of consecutive failures that
	// trip the circuit breaker from closed to open.
	breakerFailures uint32 = 10

	// breakerTimeout is the cool-down after which an open breaker
	// transitions to half-open and probes the backend again.
	breakerTimeout = 5 * time.Second
)

// Breaker state values exposed via the circuit_state gauge counter.
const (
	breakerStateClosed   int64 = 0
	breakerStateHalfOpen int64 = 1
	breakerStateOpen     int64 = 2
)

// NATSKVStore is a GCRA rate limiter whose TAT lives in a NATS
// JetStream KV bucket. Semantically identical to MemoryStore: the
// same (key, rps, burst) inputs produce the same decision, so a
// deployment can start on MemoryStore and migrate to NATSKVStore
// without behavioral drift.
//
// Cross-replica correctness is enforced by optimistic CAS on the
// KV revision: each Allow reads the current TAT + revision,
// computes the new TAT via Check, and writes back with the revision
// as a precondition. Lost CAS means another replica advanced the
// TAT for the same key; the loop retries with a jittered backoff
// until the budget is exhausted.
//
// A circuit breaker guards the KV backend. On sustained failure,
// Allow short-circuits with ErrCircuitOpen instead of hammering a
// dead JetStream cluster; the gateway's FailPolicy decides whether
// that maps to HTTP 503 or allow-on-failure.
//
// TTL semantics: NATSKVStore configures the bucket's MaxAge from
// NATSKVStoreConfig.KeyTTL — a hard cap, every key is reaped that
// long after creation regardless of activity. This differs from
// MemoryStore, which interprets the same configuration value as an
// idle-sweep interval where active keys are retained indefinitely.
// Operators wiring RATELIMIT_KEY_TTL must understand the divergence
// when comparing per-bucket lifetime across a backend swap.
type NATSKVStore struct {
	kv      kvAPI
	breaker *gobreaker.CircuitBreaker
	logger  zerolog.Logger
	budget  time.Duration

	counters struct {
		allowed             atomic.Int64
		rejected            atomic.Int64
		backendErrors       atomic.Int64
		casRetries          atomic.Int64
		budgetExhausted     atomic.Int64
		casAttemptsExceeded atomic.Int64
		circuitState        atomic.Int64
		breakerTransitions  atomic.Int64
		circuitRejected     atomic.Int64
		corruptTAT          atomic.Int64
	}
}

// natskvOption customizes NATSKVStore construction. Options compose:
// pass any subset to newNATSKVStoreFromKV (or the public constructor).
type natskvOption func(*natskvOptions)

type natskvOptions struct {
	budget          time.Duration
	breakerFailures uint32
	breakerTimeout  time.Duration
	logger          zerolog.Logger
}

// withCASBudget overrides the default CAS wall-clock budget. Tests
// use a tight budget to exercise the exhaustion path; production
// defaults to casBudget.
func withCASBudget(d time.Duration) natskvOption {
	return func(o *natskvOptions) { o.budget = d }
}

// withBreakerFailures overrides the consecutive-failure threshold
// that trips the circuit breaker from closed to open.
func withBreakerFailures(n uint32) natskvOption {
	return func(o *natskvOptions) { o.breakerFailures = n }
}

// withLogger plumbs a zerolog.Logger into breaker state-change logs.
func withLogger(l zerolog.Logger) natskvOption {
	return func(o *natskvOptions) { o.logger = l }
}

// newNATSKVStoreFromKV constructs a store against any kvAPI
// implementation. Production code wires a real JetStream KeyValue
// adapter; tests wire an in-memory fake. The separation lets the
// CAS + breaker logic be covered without a live NATS.
func newNATSKVStoreFromKV(kv kvAPI, opts ...natskvOption) *NATSKVStore {
	cfg := natskvOptions{
		budget:          casBudget,
		breakerFailures: breakerFailures,
		breakerTimeout:  breakerTimeout,
		logger:          zerolog.Nop(),
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	s := &NATSKVStore{
		kv:     kv,
		logger: cfg.logger,
		budget: cfg.budget,
	}
	s.breaker = newBreaker(cfg.breakerFailures, cfg.breakerTimeout, cfg.logger, &s.counters.circuitState, &s.counters.breakerTransitions)
	return s
}

// newBreaker builds a gobreaker.CircuitBreaker with a
// consecutive-failure trip rule and a hook that mirrors the
// breaker's state into the provided atomic gauge for metric export.
//
// IsSuccessful reclassifies request-context cancellations and
// deadlines as breaker-side successes. An upstream handler timeout
// cascade would otherwise be misattributed to the KV — the breaker
// would trip open on a healthy backend simply because callers stopped
// waiting. The error is still returned to the Allow caller so its
// FailPolicy applies; only the breaker's failure-streak accounting
// excludes these benign terminations.
//
// The whitelist relies on allowInternal's error classification: a
// context deadline produced by the STORE's own budget (hung backend)
// reaches the breaker as errKVBudgetElapsed, which does not satisfy
// errors.Is(err, context.DeadlineExceeded) and therefore counts as a
// real failure. Only deadlines and cancellations originating from the
// caller's context keep their sentinel identity and pass as success.
func newBreaker(failures uint32, timeout time.Duration, logger zerolog.Logger, stateGauge, transitions *atomic.Int64) *gobreaker.CircuitBreaker {
	settings := gobreaker.Settings{
		Name:    "ratelimit-natskv",
		Timeout: timeout,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			return counts.ConsecutiveFailures >= failures
		},
		IsSuccessful: func(err error) bool {
			return err == nil ||
				errors.Is(err, context.Canceled) ||
				errors.Is(err, context.DeadlineExceeded)
		},
		OnStateChange: func(name string, from, to gobreaker.State) {
			transitions.Add(1)
			stateGauge.Store(stateToInt(to))
			logger.Warn().
				Str("event", "ratelimit.circuit.statechange").
				Str("name", name).
				Str("from", from.String()).
				Str("to", to.String()).
				Msg("circuit breaker state change")
		},
	}
	return gobreaker.NewCircuitBreaker(settings)
}

func stateToInt(s gobreaker.State) int64 {
	switch s {
	case gobreaker.StateClosed:
		return breakerStateClosed
	case gobreaker.StateHalfOpen:
		return breakerStateHalfOpen
	case gobreaker.StateOpen:
		return breakerStateOpen
	default:
		return breakerStateClosed
	}
}

// Allow implements Store by running GCRA against a TAT stored in the
// KV bucket. The backend call is wrapped by the circuit breaker: an
// open breaker short-circuits with ErrCircuitOpen before any network
// I/O. Errors from allowInternal (budget exhausted, max attempts,
// propagated KV errors) propagate to the caller and are counted as
// breaker failures.
//
// Wall-clock contract: Allow returns within
// min(s.budget, time.Until(ctx.Deadline())) of entry. The store's
// own CAS budget caps the total time spent retrying on conflict;
// the caller's ctx deadline (if any) further tightens that cap so a
// downstream timeout chain stays consistent. Each Get/Create/Update
// call is invoked with a derived context bounded by the remaining
// wall time, ensuring a hung backend cannot hold Allow past the
// effective budget.
func (s *NATSKVStore) Allow(ctx context.Context, key string, rps, burst int) (Decision, error) {
	result, err := s.breaker.Execute(func() (any, error) {
		return s.allowInternal(ctx, key, rps, burst)
	})
	if err != nil {
		if errors.Is(err, gobreaker.ErrOpenState) || errors.Is(err, gobreaker.ErrTooManyRequests) {
			s.counters.circuitRejected.Add(1)
			return Decision{}, ErrCircuitOpen
		}

		// Any other propagated error is a real backend fault — CAS
		// budget exhaustion, attempt-cap exhaustion, or a KV
		// network/server failure. Bump the unified backend_errors
		// counter so dashboards can graph the total fault rate
		// uniformly across backends; the more specific counters
		// (budgetExhausted, casAttemptsExceeded, circuitRejected)
		// remain available for triage.
		s.counters.backendErrors.Add(1)

		return Decision{}, fmt.Errorf("ratelimit breaker execute: %w", err)
	}
	d, ok := result.(Decision)
	if !ok {
		return Decision{}, fmt.Errorf("ratelimit: unexpected breaker result type %T", result)
	}
	if d.Allowed {
		s.counters.allowed.Add(1)
	} else {
		s.counters.rejected.Add(1)
	}
	return d, nil
}

// allowInternal is the CAS retry loop invoked through the breaker.
// Each attempt: read the current TAT + revision, run Check, and on
// allow either Create (fresh key) or Update (existing revision). A
// lost CAS increments casRetries and retries with backoff+jitter
// until the wall-clock budget or attempt cap is hit.
//
// Wall-clock budget = min(s.budget, time.Until(ctx.Deadline()))
// computed once at entry. The deadline gates loop iteration AND
// every per-call backend ctx so a hung KV cannot hold Allow open
// past the effective budget. When the caller's ctx has no deadline
// the store budget alone is the bound.
func (s *NATSKVStore) allowInternal(ctx context.Context, key string, rps, burst int) (Decision, error) {
	deadline := s.computeDeadline(ctx)
	for attempt := 0; attempt < maxCASAttempts; attempt++ {
		if time.Now().After(deadline) {
			s.counters.budgetExhausted.Add(1)
			return Decision{}, ErrCASBudgetExhausted
		}
		if attempt > 0 {
			if !sleepCtx(ctx, nextBackoff(attempt)) {
				return Decision{}, fmt.Errorf("ratelimit cas wait: %w", ctx.Err())
			}
		}

		callCtx, cancel := context.WithDeadline(ctx, deadline)
		entry, err := s.kv.Get(callCtx, key)
		var currentTAT time.Time
		var rev uint64
		switch {
		case err == nil:
			rev = entry.Revision()
			decoded, decodeErr := decodeTAT(entry.Value())
			if decodeErr != nil {
				// Corrupt entry (wrong version byte, wrong length, or
				// the byte layout has drifted from a future schema).
				// Fall back to a fresh bucket — safer than panicking
				// or refusing service — but emit a loud structured
				// WARN so operators spot the drift in logs.
				s.counters.corruptTAT.Add(1)
				s.logger.Warn().
					Str("event", "ratelimit.kv.corrupt_tat").
					Str("key", key).
					Uint64("revision", rev).
					Err(decodeErr).
					Msg("ratelimit: corrupt TAT in KV; resetting bucket")
			} else {
				currentTAT = decoded
			}
		case errors.Is(err, errKVKeyNotFound):
			// Fresh bucket; currentTAT stays zero, rev stays 0.
		default:
			cancel()
			return Decision{}, fmt.Errorf("nats-kv get: %w", s.classifyBackendErr(ctx, err))
		}

		decision, newTAT := Check(currentTAT, time.Now(), rps, burst)
		if !decision.Allowed {
			cancel()
			return decision, nil
		}

		encoded := encodeTAT(newTAT)
		if rev == 0 {
			_, err = s.kv.Create(callCtx, key, encoded)
		} else {
			_, err = s.kv.Update(callCtx, key, encoded, rev)
		}
		cancel()
		if err == nil {
			return decision, nil
		}
		if errors.Is(err, errCASConflict) {
			s.counters.casRetries.Add(1)
			continue
		}
		return Decision{}, fmt.Errorf("nats-kv write: %w", s.classifyBackendErr(ctx, err))
	}
	s.counters.casAttemptsExceeded.Add(1)
	return Decision{}, ErrCASMaxAttempts
}

// classifyBackendErr disambiguates a context-shaped backend error by
// asking WHICH context produced it. Every KV call runs under a derived
// context capped by the store's CAS budget; when the backend hangs,
// that derived deadline fires and the error surfaces as
// context.DeadlineExceeded — byte-identical to a caller-side timeout.
// The two demand opposite breaker accounting:
//
//   - caller ctx done (ctx.Err() != nil): the client stopped waiting
//     or an upstream timeout chain fired. Not the KV's fault — keep
//     the context sentinel in the chain so IsSuccessful whitelists it.
//   - caller ctx healthy: the store's own budget elapsed against an
//     unresponsive backend. Rewrap as errKVBudgetElapsed, severing the
//     context sentinel from the chain (%v, not %w) so the breaker
//     counts a real failure and can eventually short-circuit.
//
// Non-context errors (connection refused, server fault) pass through
// untouched — they already count as failures.
func (s *NATSKVStore) classifyBackendErr(ctx context.Context, err error) error {
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		return err
	}
	if ctx.Err() != nil {
		return err
	}
	return fmt.Errorf("%w: %v", errKVBudgetElapsed, err)
}

// computeDeadline returns the effective wall-clock cap for a single
// Allow invocation as min(now+s.budget, ctx.Deadline()). The store's
// budget is the floor — even a context without a deadline gets the
// store's own bound — and the ctx deadline only tightens it further.
func (s *NATSKVStore) computeDeadline(ctx context.Context) time.Time {
	storeDeadline := time.Now().Add(s.budget)
	ctxDeadline, ok := ctx.Deadline()
	if ok && ctxDeadline.Before(storeDeadline) {
		return ctxDeadline
	}
	return storeDeadline
}

// FlushPrefix removes every key in the backing bucket whose name
// starts with prefix. Used by the gateway's hot-reload path to drop
// stale GCRA state after a route reconfiguration so that a tightened
// limit does not keep honoring a burst accumulated under the old
// config.
//
// When the store is backed by a non-JetStream kvAPI (the in-memory
// test fake), this is a no-op: test suites drive state directly and
// do not need a prefix sweep. Returning nil here keeps test wiring
// simple and matches the contract — "best-effort flush" — without
// leaking a fake-specific branch upward.
//
// On the JetStream path the method iterates ListKeys in streaming
// mode and issues a Delete (tombstone, not Purge) for each matching
// key. The first Delete error aborts the sweep and is returned;
// already-deleted keys are reported by the iterator as future no-ops
// and do not surface as errors.
//
// Iteration error semantics: jetstream.KeyLister exposes only Keys
// and Stop — there is no Err() accessor (verified against
// nats.go v1.x). When the iterator closes its channel because of an
// upstream failure (network drop, server-side error) the loop
// terminates without surfacing the cause. Operators see the
// inability to clean up a prefix only through the next reconcile
// pass; this is a documented limitation pending an upstream
// nats.go API addition.
func (s *NATSKVStore) FlushPrefix(ctx context.Context, prefix string) error {
	adapter, ok := s.kv.(*jsKVAdapter)
	if !ok {
		return nil
	}

	lister, err := adapter.kv.ListKeys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			return nil
		}

		return fmt.Errorf("nats-kv list: %w", err)
	}
	defer func() { _ = lister.Stop() }()

	for key := range lister.Keys() {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		if err := adapter.kv.Delete(ctx, key); err != nil {
			return fmt.Errorf("nats-kv delete %q: %w", key, err)
		}
	}

	return nil
}

// Close releases resources. Idempotent.
func (s *NATSKVStore) Close() error { return nil }

// nextBackoff returns a jittered backoff for CAS retry attempt N
// (1-indexed). Base grows as min(1ms << (attempt-1), 32ms), with
// full jitter in [0, base]. Jitter avoids synchronized retries
// across concurrent callers on the same hot key.
func nextBackoff(attempt int) time.Duration {
	const (
		baseStep = time.Millisecond
		maxBase  = 32 * time.Millisecond
	)
	shift := attempt - 1
	if shift < 0 {
		shift = 0
	}
	if shift > 30 {
		shift = 30
	}
	base := baseStep << shift
	if base > maxBase {
		base = maxBase
	}
	return time.Duration(rand.Int64N(int64(base) + 1))
}

// sleepCtx sleeps for d or until ctx is canceled. Returns true if
// the full duration elapsed, false if ctx was canceled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// Counters returns a point-in-time snapshot of the store's internal
// metrics. Each value is read atomically so concurrent Allow calls
// cannot produce a torn read. Intended for OpenTelemetry plumbing.
//
// Schema obeys the Store.Counters minimum: decisions_allowed,
// decisions_rejected, and backend_errors are always present. The
// natskv-specific keys (cas_retries, budget_exhausted,
// cas_attempts_exceeded, circuit_state, breaker_transitions,
// circuit_rejected, corrupt_tat) are extra triage signals layered on
// top — backend_errors aggregates them at the dashboard level.
func (s *NATSKVStore) Counters() map[string]int64 {
	return map[string]int64{
		"ratelimit_natskv_decisions_allowed_total":     s.counters.allowed.Load(),
		"ratelimit_natskv_decisions_rejected_total":    s.counters.rejected.Load(),
		"ratelimit_natskv_backend_errors_total":        s.counters.backendErrors.Load(),
		"ratelimit_natskv_cas_retries_total":           s.counters.casRetries.Load(),
		"ratelimit_natskv_budget_exhausted_total":      s.counters.budgetExhausted.Load(),
		"ratelimit_natskv_cas_attempts_exceeded_total": s.counters.casAttemptsExceeded.Load(),
		"ratelimit_natskv_circuit_state":               s.counters.circuitState.Load(),
		"ratelimit_natskv_breaker_transitions_total":   s.counters.breakerTransitions.Load(),
		"ratelimit_natskv_circuit_rejected_total":      s.counters.circuitRejected.Load(),
		"ratelimit_natskv_corrupt_tat_total":           s.counters.corruptTAT.Load(),
	}
}
