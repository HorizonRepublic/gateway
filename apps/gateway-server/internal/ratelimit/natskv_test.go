package ratelimit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/rs/zerolog"
	"github.com/sony/gobreaker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEncodeDecodeTAT_RoundTrip(t *testing.T) {
	t0 := time.Unix(0, 1_735_837_293_847_123_456)
	enc := encodeTAT(t0)
	require.Len(t, enc, tatEncodedLength)
	assert.Equal(t, tatVersion1, enc[0])

	dec, err := decodeTAT(enc)
	require.NoError(t, err)
	assert.True(t, t0.Equal(dec))
}

func TestDecodeTAT_RejectsWrongLength(t *testing.T) {
	_, err := decodeTAT([]byte{0x01, 0, 0})
	assert.Error(t, err)
}

func TestDecodeTAT_RejectsUnknownVersion(t *testing.T) {
	bad := []byte{0xFF, 0, 0, 0, 0, 0, 0, 0, 0}
	_, err := decodeTAT(bad)
	assert.Error(t, err)
}

// fakeEntry implements kvEntry for tests.
type fakeEntry struct {
	value    []byte
	revision uint64
}

func (e fakeEntry) Value() []byte    { return e.value }
func (e fakeEntry) Revision() uint64 { return e.revision }

// fakeKV implements kvAPI for tests: in-memory map with per-call
// error injection and call counters. Safe for concurrent use.
type fakeKV struct {
	mu      sync.Mutex
	entries map[string]fakeEntry
	nextRev uint64

	writeErr      error
	conflictCount int

	getCalls    int
	createCalls int
	updateCalls int
}

func newFakeKV() *fakeKV {
	return &fakeKV{entries: map[string]fakeEntry{}, nextRev: 1}
}

func (k *fakeKV) setWriteError(err error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.writeErr = err
}

func (k *fakeKV) setCASConflictCount(n int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.conflictCount = n
}

func (k *fakeKV) setInitial(key string, value []byte) {
	k.mu.Lock()
	defer k.mu.Unlock()
	rev := k.nextRev
	k.nextRev++
	k.entries[key] = fakeEntry{value: value, revision: rev}
}

func (k *fakeKV) Get(_ context.Context, key string) (kvEntry, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.getCalls++
	e, ok := k.entries[key]
	if !ok {
		return nil, errKVKeyNotFound
	}
	return e, nil
}

func (k *fakeKV) Create(_ context.Context, key string, value []byte) (uint64, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.createCalls++
	if k.writeErr != nil {
		return 0, k.writeErr
	}
	if k.conflictCount > 0 {
		k.conflictCount--
		return 0, errCASConflict
	}
	if _, exists := k.entries[key]; exists {
		return 0, errCASConflict
	}
	rev := k.nextRev
	k.nextRev++
	k.entries[key] = fakeEntry{value: value, revision: rev}
	return rev, nil
}

func (k *fakeKV) Update(_ context.Context, key string, value []byte, revision uint64) (uint64, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.updateCalls++
	if k.writeErr != nil {
		return 0, k.writeErr
	}
	if k.conflictCount > 0 {
		k.conflictCount--
		return 0, errCASConflict
	}
	cur, ok := k.entries[key]
	if !ok {
		return 0, errKVKeyNotFound
	}
	if cur.revision != revision {
		return 0, errCASConflict
	}
	rev := k.nextRev
	k.nextRev++
	k.entries[key] = fakeEntry{value: value, revision: rev}
	return rev, nil
}

// testNATSKVStore builds a store wired to the provided fake KV with
// deterministic defaults. The default CAS budget is generous (1s)
// so tests pass on slow CI runners; override via withCASBudget.
func testNATSKVStore(t *testing.T, kv kvAPI, opts ...natskvOption) *NATSKVStore {
	t.Helper()
	base := []natskvOption{withCASBudget(time.Second), withBreakerFailures(100)}
	return newNATSKVStoreFromKV(kv, append(base, opts...)...)
}

func TestNATSKVStore_FirstRequestCreatesEntry(t *testing.T) {
	kv := newFakeKV()
	sut := testNATSKVStore(t, kv)

	d, err := sut.Allow(context.Background(), "k", 100, 5)

	require.NoError(t, err)
	assert.True(t, d.Allowed)
	assert.Equal(t, 1, kv.getCalls)
	assert.Equal(t, 1, kv.createCalls)
	assert.Equal(t, 0, kv.updateCalls)
	assert.Equal(t, int64(1), sut.counters.allowed.Load())
}

func TestNATSKVStore_SecondRequestUsesUpdate(t *testing.T) {
	kv := newFakeKV()
	sut := testNATSKVStore(t, kv)
	ctx := context.Background()

	_, err := sut.Allow(ctx, "k", 100, 5)
	require.NoError(t, err)

	d, err := sut.Allow(ctx, "k", 100, 5)

	require.NoError(t, err)
	assert.True(t, d.Allowed)
	assert.Equal(t, 2, kv.getCalls)
	assert.Equal(t, 1, kv.createCalls)
	assert.Equal(t, 1, kv.updateCalls)
	assert.Equal(t, int64(2), sut.counters.allowed.Load())
}

func TestNATSKVStore_RejectsWithoutWrite(t *testing.T) {
	kv := newFakeKV()
	sut := testNATSKVStore(t, kv)
	ctx := context.Background()

	// Drain the burst by hammering the key until a reject appears.
	// rps=1 burst=2 means at most 2 tokens stored; a tight loop
	// guarantees the 3rd call rejects without any timing assumptions.
	rejected := false
	var createsAtReject, updatesAtReject int
	for i := 0; i < 10; i++ {
		d, err := sut.Allow(ctx, "k", 1, 2)
		require.NoError(t, err)
		if !d.Allowed {
			rejected = true
			createsAtReject = kv.createCalls
			updatesAtReject = kv.updateCalls
			break
		}
	}
	require.True(t, rejected, "expected at least one reject")

	// A follow-up reject MUST NOT issue any write.
	d, err := sut.Allow(ctx, "k", 1, 2)
	require.NoError(t, err)
	assert.False(t, d.Allowed)

	assert.Equal(t, createsAtReject, kv.createCalls, "no create on reject")
	assert.Equal(t, updatesAtReject, kv.updateCalls, "no update on reject")
	assert.GreaterOrEqual(t, sut.counters.rejected.Load(), int64(2))
}

func TestNATSKVStore_CASConflictRetries(t *testing.T) {
	kv := newFakeKV()
	// Seed a real encoded TAT so decode succeeds.
	kv.setInitial("k", encodeTAT(time.Now().Add(-time.Second)))
	kv.setCASConflictCount(2)

	sut := testNATSKVStore(t, kv)

	d, err := sut.Allow(context.Background(), "k", 100, 5)

	require.NoError(t, err)
	assert.True(t, d.Allowed)
	assert.Equal(t, int64(2), sut.counters.casRetries.Load())
}

func TestNATSKVStore_BudgetExhausted(t *testing.T) {
	kv := newFakeKV()
	kv.setInitial("k", encodeTAT(time.Now().Add(-time.Second)))
	kv.setCASConflictCount(1000)

	sut := testNATSKVStore(t, kv, withCASBudget(5*time.Millisecond))

	_, err := sut.Allow(context.Background(), "k", 100, 5)

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrCASBudgetExhausted)
	assert.Equal(t, int64(1), sut.counters.budgetExhausted.Load())
}

// TestNATSKVStore_CorruptTATRecoversAndLogs guards the decodeTAT
// fail-soft path: a corrupt KV entry (wrong version byte, truncated
// layout) MUST NOT panic or silently pass the request through. The
// store falls back to a fresh bucket (so the request is evaluated
// against a zero TAT, which is correct GCRA semantics for "no prior
// state") while emitting a structured WARN so operators see the drift.
func TestNATSKVStore_CorruptTATRecoversAndLogs(t *testing.T) {
	kv := newFakeKV()
	// Seed an entry whose version byte does not match tatVersion1 —
	// decodeTAT rejects it and the store must fall back to fresh.
	kv.setInitial("k", []byte{0xFF, 0, 0, 0, 0, 0, 0, 0, 0})

	var logBuf bytes.Buffer
	sink := zerolog.New(&logBuf)

	sut := testNATSKVStore(t, kv, withLogger(sink))

	d, err := sut.Allow(context.Background(), "k", 100, 5)

	require.NoError(t, err)
	assert.True(t, d.Allowed, "fresh bucket allows the first request")
	assert.Equal(t, int64(1), sut.counters.corruptTAT.Load(),
		"corrupt-TAT counter must tick so operators see the recovery")
	assert.Contains(t, sut.Counters(), "ratelimit_natskv_corrupt_tat_total",
		"corrupt-TAT counter must appear in the Counters snapshot for OTel export")

	// Walk the JSON-structured log lines and assert the warn record
	// carries enough context (key, event, decoder error) for an
	// operator to locate the corrupt entry.
	var sawCorruptWarn bool
	for _, line := range bytes.Split(bytes.TrimSpace(logBuf.Bytes()), []byte("\n")) {
		if len(line) == 0 {
			continue
		}

		var record map[string]any
		require.NoError(t, json.Unmarshal(line, &record), "log sink must emit valid JSON")

		if record["event"] == "ratelimit.kv.corrupt_tat" {
			sawCorruptWarn = true
			assert.Equal(t, "warn", record["level"])
			assert.Equal(t, "k", record["key"])
			assert.NotEmpty(t, record["error"])
		}
	}

	assert.True(t, sawCorruptWarn,
		"a WARN record with event=ratelimit.kv.corrupt_tat must be emitted")
}

// TestIsBucketAlreadyExistsErr covers the TOCTOU recovery helper. A
// second replica that raced through js.KeyValue → ErrBucketNotFound →
// js.CreateKeyValue must classify the resulting ErrBucketExists (or
// the joined ErrStreamNameAlreadyInUse) as recoverable so it can
// re-open the bucket the winning pod just created. Unrecognised errors
// propagate as creation failures.
func TestIsBucketAlreadyExistsErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil is not a conflict", nil, false},
		{"ErrBucketExists sentinel", jetstream.ErrBucketExists, true},
		{"ErrStreamNameAlreadyInUse sentinel", jetstream.ErrStreamNameAlreadyInUse, true},
		{
			"ErrBucketExists wrapped via fmt.Errorf",
			fmt.Errorf("create bucket %q: %w", "rl", jetstream.ErrBucketExists),
			true,
		},
		{
			"nats.go-style joined ErrBucketExists + ErrStreamNameAlreadyInUse",
			errors.Join(
				fmt.Errorf("%w: %s", jetstream.ErrBucketExists, "rl"),
				jetstream.ErrStreamNameAlreadyInUse,
			),
			true,
		},
		{"unrelated error", errors.New("network down"), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isBucketAlreadyExistsErr(tc.err))
		})
	}
}

// TestNATSKVStore_CASMaxAttemptsDistinctFromBudget pins the
// observability split between time-based exhaustion and the hard
// per-call retry cap. A KV that conflicts on every CAS attempt drives
// the loop into the maxCASAttempts guard; the resulting error MUST
// surface as ErrCASMaxAttempts and bump a dedicated counter so
// operators can distinguish "hot key under contention" (budget)
// from "broken KV that always conflicts" (attempt cap).
func TestNATSKVStore_CASMaxAttemptsDistinctFromBudget(t *testing.T) {
	kv := newFakeKV()
	kv.setInitial("k", encodeTAT(time.Now().Add(-time.Second)))
	// Force every Update to conflict; with a generous budget the loop
	// hits maxCASAttempts before the wall-clock deadline.
	kv.setCASConflictCount(1_000_000)

	sut := testNATSKVStore(t, kv, withCASBudget(time.Hour))

	_, err := sut.Allow(context.Background(), "k", 100, 5)

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrCASMaxAttempts)
	assert.Equal(t, int64(1), sut.counters.casAttemptsExceeded.Load(),
		"hitting the attempt cap must bump the dedicated counter, not budgetExhausted")
	assert.Equal(t, int64(0), sut.counters.budgetExhausted.Load(),
		"attempt-cap exhaustion must not pollute the time-budget counter")
	assert.Contains(t, sut.Counters(), "ratelimit_natskv_cas_attempts_exceeded_total",
		"the counter must surface in the snapshot for OTel export")
}

// TestNATSKVStore_BudgetClampedToCtxDeadline pins the wall-clock
// hierarchy: when the caller's context carries a tighter deadline
// than the store's CAS budget, Allow MUST return within the smaller
// of the two. Without the clamp a 10ms store budget could keep
// retrying past a 2ms request budget and starve the caller's
// downstream timeout chain.
func TestNATSKVStore_BudgetClampedToCtxDeadline(t *testing.T) {
	kv := newFakeKV()
	kv.setInitial("k", encodeTAT(time.Now().Add(-time.Second)))
	kv.setCASConflictCount(1_000_000)

	// Store budget is generous — the caller's ctx is the actual cap.
	sut := testNATSKVStore(t, kv, withCASBudget(5*time.Second))

	deadline := 50 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	start := time.Now()
	_, err := sut.Allow(ctx, "k", 100, 5)
	elapsed := time.Since(start)

	require.Error(t, err)
	// Tolerance covers backoff jitter and CI scheduling slop. The
	// invariant is "did not blow past the ctx deadline by an order of
	// magnitude", not a tight upper bound.
	assert.Less(t, elapsed, 5*deadline,
		"Allow must return within the ctx deadline, not the store budget")
}

// blockingKV is a kvAPI fake that holds Get until the per-call ctx
// is cancelled, then returns the ctx error. Used to prove that
// allowInternal clamps each Get's effective deadline to the store
// budget so a misbehaving backend cannot hold Allow open past it.
type blockingKV struct{}

func (blockingKV) Get(ctx context.Context, _ string) (kvEntry, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (blockingKV) Create(ctx context.Context, _ string, _ []byte) (uint64, error) {
	<-ctx.Done()
	return 0, ctx.Err()
}
func (blockingKV) Update(ctx context.Context, _ string, _ []byte, _ uint64) (uint64, error) {
	<-ctx.Done()
	return 0, ctx.Err()
}

// TestNATSKVStore_BudgetCapsBackendCallsWhenCtxHasNoDeadline pins
// the per-attempt deadline derivation. When the caller passes a
// context without a deadline (e.g., a long-running admin task) the
// store MUST still bound each backend call to the remaining CAS
// budget so a hung KV cannot stall Allow past s.budget.
func TestNATSKVStore_BudgetCapsBackendCallsWhenCtxHasNoDeadline(t *testing.T) {
	const budget = 30 * time.Millisecond
	sut := testNATSKVStore(t, blockingKV{}, withCASBudget(budget))

	start := time.Now()
	_, err := sut.Allow(context.Background(), "k", 100, 5)
	elapsed := time.Since(start)

	require.Error(t, err)
	// 10x slack absorbs CI jitter without admitting a runaway loop.
	assert.Less(t, elapsed, 10*budget,
		"Allow must enforce its own wall-clock bound when ctx has no deadline")
}

// TestNATSKVStore_HungKVTripsBreaker pins the other half of the
// failure-classification contract: when the STORE's own CAS budget
// expires because the backend hangs (the canonical dead-JetStream
// outage the breaker exists for), the resulting error MUST count as a
// breaker failure even though it surfaces as a context deadline. The
// caller's context is healthy here — only the store-derived per-call
// deadline fired — so the "benign caller cancellation" whitelist must
// not swallow it. Without the distinction the breaker never opens on a
// hung KV and every request eats the full budget stall forever.
func TestNATSKVStore_HungKVTripsBreaker(t *testing.T) {
	const failures = 3
	sut := newNATSKVStoreFromKV(blockingKV{},
		withCASBudget(5*time.Millisecond),
		withBreakerFailures(failures),
	)

	for i := 0; i < failures; i++ {
		_, err := sut.Allow(context.Background(), "k", 100, 5)
		require.Error(t, err, "iteration %d: hung KV must surface an error", i)
		require.NotErrorIs(t, err, ErrCircuitOpen,
			"iteration %d: breaker must still be closed while failures accumulate", i)
	}

	_, err := sut.Allow(context.Background(), "k", 100, 5)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrCircuitOpen,
		"store-budget expiry on a hung KV must accumulate as breaker failures and open the circuit")
	assert.Equal(t, int64(1), sut.counters.circuitRejected.Load())
}

// TestNATSKVStore_CancelledCtxDoesNotTripBreaker pins the
// fail-classification fix: a cancelled or deadline-exceeded request
// context MUST NOT count as a backend failure for the circuit breaker.
// Otherwise an upstream handler timeout cascade would hammer the
// breaker open against a perfectly healthy KV. The Allow caller still
// sees the propagated ctx error so its FailPolicy can decide allow vs
// reject.
func TestNATSKVStore_CancelledCtxDoesNotTripBreaker(t *testing.T) {
	kv := newFakeKV()
	kv.setInitial("k", encodeTAT(time.Now().Add(-time.Second)))
	// Force every CAS attempt to conflict so the loop reaches the
	// backoff sleepCtx, where a cancelled context surfaces.
	kv.setCASConflictCount(1_000_000)

	// Tight breaker so a single misclassified failure flips state.
	sut := testNATSKVStore(t, kv, withBreakerFailures(2), withCASBudget(time.Hour))

	for i := 0; i < 50; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, err := sut.Allow(ctx, "k", 100, 5)
		require.Error(t, err, "iteration %d: cancelled ctx must surface an error", i)
		// The breaker MUST still be closed: no cancellation should be
		// counted as a real failure.
		require.NotErrorIs(t, err, ErrCircuitOpen,
			"iteration %d: cancelled ctx must not trip the breaker open", i)
	}
}

// TestNATSKVStore_CountersIncludeMinimumSchema enforces metric
// parity with the other Store backends: decisions_allowed,
// decisions_rejected, and backend_errors must all be present in the
// snapshot so a dashboard plotting them across backends does not go
// dark when the gateway swaps backends.
func TestNATSKVStore_CountersIncludeMinimumSchema(t *testing.T) {
	kv := newFakeKV()
	sut := testNATSKVStore(t, kv)

	c := sut.Counters()
	assert.Contains(t, c, "ratelimit_natskv_decisions_allowed_total")
	assert.Contains(t, c, "ratelimit_natskv_decisions_rejected_total")
	assert.Contains(t, c, "ratelimit_natskv_backend_errors_total")
}

// TestStateToInt pins the gobreaker.State → int mapping that backs
// the circuit_state gauge. The dashboard reads the gauge to render
// breaker status; a swapped mapping (open=0 instead of open=2) would
// silently invert the alert. Defensive default is closed: an unknown
// state value MUST NOT surface as "open" and falsely page on a
// healthy backend.
func TestStateToInt(t *testing.T) {
	cases := []struct {
		name  string
		state gobreaker.State
		want  int64
	}{
		{"closed maps to 0", gobreaker.StateClosed, breakerStateClosed},
		{"half-open maps to 1", gobreaker.StateHalfOpen, breakerStateHalfOpen},
		{"open maps to 2", gobreaker.StateOpen, breakerStateOpen},
		{
			"unknown state defaults to closed",
			gobreaker.State(99),
			breakerStateClosed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, stateToInt(tc.state))
		})
	}
}

// TestIsJSRevisionConflict pins the error-discrimination contract on
// jsKVAdapter Create/Update. A revision conflict (KeyExists, KeyDeleted,
// raw APIError carrying JSErrCodeStreamWrongLastSequence) MUST translate
// to errCASConflict so the CAS retry loop gets a chance to read-modify-
// write again. Any other error MUST surface as a real backend fault so
// the breaker accounts for it.
func TestIsJSRevisionConflict(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil is never a conflict", nil, false},
		{"ErrKeyExists sentinel", jetstream.ErrKeyExists, true},
		{"ErrKeyDeleted sentinel", jetstream.ErrKeyDeleted, true},
		{
			"ErrKeyExists wrapped via fmt.Errorf",
			fmt.Errorf("kv create %q: %w", "k", jetstream.ErrKeyExists),
			true,
		},
		{
			"ErrKeyDeleted wrapped via fmt.Errorf",
			fmt.Errorf("kv update %q: %w", "k", jetstream.ErrKeyDeleted),
			true,
		},
		{
			"raw APIError with JSErrCodeStreamWrongLastSequence",
			&jetstream.APIError{ErrorCode: jetstream.JSErrCodeStreamWrongLastSequence},
			true,
		},
		{
			"APIError with unrelated code is not a conflict",
			&jetstream.APIError{ErrorCode: jetstream.JSErrCodeStreamNotFound},
			false,
		},
		{"plain unrelated error", errors.New("network down"), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isJSRevisionConflict(tc.err))
		})
	}
}

// TestNATSKVStore_CloseIsIdempotentNoOp pins the documented Close
// contract: the production NATSKVStore Close is a no-op (the JS handle
// is owned by the caller, not the store) and MUST be safely callable
// multiple times so deferred shutdown chains do not surface a spurious
// error on the second invocation.
func TestNATSKVStore_CloseIsIdempotentNoOp(t *testing.T) {
	sut := testNATSKVStore(t, newFakeKV())

	require.NoError(t, sut.Close(), "first Close must succeed")
	require.NoError(t, sut.Close(), "second Close must remain a no-op")
}

func TestNATSKVStore_BreakerOpensAfterFailures(t *testing.T) {
	kv := newFakeKV()
	kv.setWriteError(errors.New("nats down"))

	sut := testNATSKVStore(t, kv, withBreakerFailures(3))
	ctx := context.Background()

	// Three consecutive failures trip the breaker.
	for i := 0; i < 3; i++ {
		_, err := sut.Allow(ctx, "k", 100, 5)
		require.Error(t, err)
	}

	getsBefore := kv.getCalls

	_, err := sut.Allow(ctx, "k", 100, 5)

	require.Error(t, err)
	assert.ErrorIs(t, err, ErrCircuitOpen)
	assert.Equal(t, getsBefore, kv.getCalls, "breaker short-circuits backend calls")
	assert.Equal(t, int64(1), sut.counters.circuitRejected.Load(), "circuit-open rejection increments counter")
}
