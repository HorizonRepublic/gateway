package nats

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/sony/gobreaker"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeInner is a programmable innerRequester. hold, when non-nil, is
// closed by the test to release in-flight calls; err, when non-nil, is
// returned for every call. errBySubject overrides err for individual
// subjects so per-subject breaker tests can fail one upstream while
// another keeps answering.
type fakeInner struct {
	mu             sync.Mutex
	calls          int
	callsBySubject map[string]int
	hold           chan struct{}
	err            error
	errBySubject   map[string]error
}

func (f *fakeInner) Request(ctx context.Context, subject string, _ []byte, _ time.Duration) ([]byte, error) {
	f.mu.Lock()
	f.calls++
	if f.callsBySubject == nil {
		f.callsBySubject = make(map[string]int)
	}
	f.callsBySubject[subject]++
	hold := f.hold
	err, overridden := f.errBySubject[subject]
	if !overridden {
		err = f.err
	}
	f.mu.Unlock()

	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	if err != nil {
		return nil, err
	}

	return []byte("ok"), nil
}

func (f *fakeInner) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.calls
}

func (f *fakeInner) callCountFor(subject string) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.callsBySubject[subject]
}

func (f *fakeInner) setSubjectErr(subject string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.errBySubject == nil {
		f.errBySubject = make(map[string]error)
	}
	f.errBySubject[subject] = err
}

func TestResilientRequester_PassthroughWhenDisabled(t *testing.T) {
	inner := &fakeInner{}
	r := NewResilientRequester(inner, ResilientConfig{}, zerolog.Nop())

	data, err := r.Request(context.Background(), "s", nil, time.Second)

	require.NoError(t, err)
	assert.Equal(t, []byte("ok"), data)
	assert.Equal(t, 1, inner.callCount())
}

func TestResilientRequester_QueueFullRejectsWith503Sentinel(t *testing.T) {
	hold := make(chan struct{})
	inner := &fakeInner{hold: hold}
	r := NewResilientRequester(inner, ResilientConfig{
		MaxInflight:  1,
		QueueTimeout: 30 * time.Millisecond,
	}, zerolog.Nop())

	// First request occupies the only slot.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = r.Request(context.Background(), "s", nil, time.Second)
	}()

	// Give the goroutine time to acquire the slot.
	require.Eventually(t, func() bool { return inner.callCount() == 1 },
		time.Second, time.Millisecond)

	// Second request must shed within ~QueueTimeout with the
	// dedicated sentinel — NOT a timeout-shaped error (504 class).
	_, err := r.Request(context.Background(), "s", nil, time.Second)
	require.ErrorIs(t, err, ErrInflightQueueFull)
	assert.NotErrorIs(t, err, context.DeadlineExceeded,
		"queue-full must not classify as a gateway timeout")

	close(hold)
	<-done

	// Slot released — admission works again.
	_, err = r.Request(context.Background(), "s", nil, time.Second)
	require.NoError(t, err)
}

func TestResilientRequester_BreakerOpensAfterConsecutiveFailures(t *testing.T) {
	inner := &fakeInner{err: errors.New("nats down")}
	r := NewResilientRequester(inner, ResilientConfig{
		BreakerEnabled:   true,
		FailureThreshold: 3,
		RecoveryTimeout:  time.Hour,
		HalfOpenProbes:   1,
	}, zerolog.Nop())

	for range 3 {
		_, err := r.Request(context.Background(), "s", nil, time.Second)
		require.Error(t, err)
		require.NotErrorIs(t, err, ErrCircuitOpen,
			"failures below the threshold must reach the inner requester")
	}

	require.Equal(t, 3, inner.callCount())

	// Breaker is now open: fast-fail without touching the inner.
	_, err := r.Request(context.Background(), "s", nil, time.Second)
	require.ErrorIs(t, err, ErrCircuitOpen)
	assert.Equal(t, 3, inner.callCount(),
		"open breaker must not consult the inner requester")
}

func TestResilientRequester_BreakerRecoversThroughHalfOpenProbe(t *testing.T) {
	inner := &fakeInner{err: errors.New("nats down")}
	r := NewResilientRequester(inner, ResilientConfig{
		BreakerEnabled:   true,
		FailureThreshold: 1,
		RecoveryTimeout:  20 * time.Millisecond,
		HalfOpenProbes:   1,
	}, zerolog.Nop())

	_, err := r.Request(context.Background(), "s", nil, time.Second)
	require.Error(t, err)

	_, err = r.Request(context.Background(), "s", nil, time.Second)
	require.ErrorIs(t, err, ErrCircuitOpen, "breaker tripped open after one failure")

	// Upstream heals; wait out the recovery timeout so the breaker
	// admits a half-open probe.
	inner.mu.Lock()
	inner.err = nil
	inner.mu.Unlock()
	time.Sleep(30 * time.Millisecond)

	data, err := r.Request(context.Background(), "s", nil, time.Second)
	require.NoError(t, err, "half-open probe must reach the healed upstream")
	assert.Equal(t, []byte("ok"), data)

	// Probe success closed the breaker — steady state restored.
	_, err = r.Request(context.Background(), "s", nil, time.Second)
	require.NoError(t, err)
}

func TestResilientRequester_BreakerIsolatesFailingService(t *testing.T) {
	const (
		subjectA      = "svc-a__microservice.cmd.users.create"
		subjectAOther = "svc-a__microservice.cmd.users.delete"
		subjectB      = "svc-b__microservice.cmd.orders.create"
	)

	inner := &fakeInner{}
	inner.setSubjectErr(subjectA, errors.New("svc-a down"))
	inner.setSubjectErr(subjectAOther, errors.New("svc-a down"))
	r := NewResilientRequester(inner, ResilientConfig{
		BreakerEnabled:   true,
		FailureThreshold: 3,
		RecoveryTimeout:  time.Hour,
		HalfOpenProbes:   1,
	}, zerolog.Nop())

	for range 3 {
		_, err := r.Request(context.Background(), subjectA, nil, time.Second)
		require.Error(t, err)
	}

	// Service A's breaker is open: fast-fail without touching the inner.
	_, err := r.Request(context.Background(), subjectA, nil, time.Second)
	require.ErrorIs(t, err, ErrCircuitOpen)
	assert.Equal(t, 3, inner.callCountFor(subjectA))

	// Another subject of the SAME service shares the breaker — the key
	// is the service prefix, not the full subject.
	_, err = r.Request(context.Background(), subjectAOther, nil, time.Second)
	require.ErrorIs(t, err, ErrCircuitOpen,
		"subjects of the same service must share one breaker")
	assert.Equal(t, 0, inner.callCountFor(subjectAOther))

	// Service B is unaffected — the blast radius stays per upstream.
	data, err := r.Request(context.Background(), subjectB, nil, time.Second)
	require.NoError(t, err, "healthy service must keep flowing while another is open")
	assert.Equal(t, []byte("ok"), data)
}

func TestResilientRequester_HalfOpenRecoveryIsPerService(t *testing.T) {
	const (
		subjectA = "svc-a__microservice.cmd.x"
		subjectB = "svc-b__microservice.cmd.y"
	)

	inner := &fakeInner{}
	inner.setSubjectErr(subjectA, errors.New("svc-a down"))
	r := NewResilientRequester(inner, ResilientConfig{
		BreakerEnabled:   true,
		FailureThreshold: 1,
		RecoveryTimeout:  20 * time.Millisecond,
		HalfOpenProbes:   1,
	}, zerolog.Nop())

	_, err := r.Request(context.Background(), subjectA, nil, time.Second)
	require.Error(t, err)
	_, err = r.Request(context.Background(), subjectA, nil, time.Second)
	require.ErrorIs(t, err, ErrCircuitOpen, "svc-a breaker tripped open after one failure")

	// Service B keeps flowing the whole time.
	_, err = r.Request(context.Background(), subjectB, nil, time.Second)
	require.NoError(t, err)

	// Service A heals; after the recovery timeout its breaker admits a
	// half-open probe and closes on success.
	inner.setSubjectErr(subjectA, nil)
	time.Sleep(30 * time.Millisecond)

	data, err := r.Request(context.Background(), subjectA, nil, time.Second)
	require.NoError(t, err, "half-open probe must reach the healed upstream")
	assert.Equal(t, []byte("ok"), data)

	_, err = r.Request(context.Background(), subjectA, nil, time.Second)
	require.NoError(t, err, "probe success must close svc-a's breaker")
}

func TestResilientRequester_CardinalityCapFallsBackToSharedBreaker(t *testing.T) {
	const (
		subjectA = "svc-a__microservice.cmd.x" // gets the only dedicated breaker
		subjectB = "svc-b__microservice.cmd.y" // overflow -> shared breaker
		subjectC = "svc-c__microservice.cmd.z" // overflow -> same shared breaker
	)

	inner := &fakeInner{}
	inner.setSubjectErr(subjectB, errors.New("svc-b down"))
	r := NewResilientRequester(inner, ResilientConfig{
		BreakerEnabled:     true,
		FailureThreshold:   2,
		RecoveryTimeout:    time.Hour,
		HalfOpenProbes:     1,
		MaxBreakerSubjects: 1,
	}, zerolog.Nop())

	// Claim the single dedicated breaker slot for service A.
	_, err := r.Request(context.Background(), subjectA, nil, time.Second)
	require.NoError(t, err)

	// Service B lands on the shared fallback breaker and trips it.
	for range 2 {
		_, err = r.Request(context.Background(), subjectB, nil, time.Second)
		require.Error(t, err)
	}
	_, err = r.Request(context.Background(), subjectB, nil, time.Second)
	require.ErrorIs(t, err, ErrCircuitOpen)

	// Service C shares the fallback breaker's fate — degraded blast
	// radius is the documented cost of exceeding the cap.
	_, err = r.Request(context.Background(), subjectC, nil, time.Second)
	require.ErrorIs(t, err, ErrCircuitOpen,
		"overflow services share one fallback breaker")
	assert.Equal(t, 0, inner.callCountFor(subjectC))

	// Service A's dedicated breaker is untouched.
	_, err = r.Request(context.Background(), subjectA, nil, time.Second)
	require.NoError(t, err,
		"dedicated breakers must be isolated from the shared fallback")
}

func TestResilientRequester_ClientCancellationDoesNotTripBreaker(t *testing.T) {
	const subject = "svc-a__microservice.cmd.x"

	inner := &fakeInner{}
	// nats.go surfaces caller-context cancellation as context.Canceled
	// (wrapped by the requester); simulate the same shape.
	inner.setSubjectErr(subject, fmt.Errorf("nats request %q: %w", subject, context.Canceled))
	r := NewResilientRequester(inner, ResilientConfig{
		BreakerEnabled:   true,
		FailureThreshold: 3,
		RecoveryTimeout:  time.Hour,
		HalfOpenProbes:   1,
	}, zerolog.Nop())

	// Well past the failure threshold: client disconnects say nothing
	// about upstream health and must not open the breaker.
	for range 10 {
		_, err := r.Request(context.Background(), subject, nil, time.Second)
		require.Error(t, err, "cancellation still propagates to the caller")
		require.ErrorIs(t, err, context.Canceled)
		require.NotErrorIs(t, err, ErrCircuitOpen,
			"client cancellations must not count as breaker failures")
	}
	assert.Equal(t, 10, inner.callCountFor(subject),
		"every request must have reached the inner requester")

	// Deadline expiry (upstream too slow) IS a failure and still trips.
	inner.setSubjectErr(subject, fmt.Errorf("nats request %q: %w", subject, context.DeadlineExceeded))
	for range 3 {
		_, err := r.Request(context.Background(), subject, nil, time.Second)
		require.Error(t, err)
	}
	_, err := r.Request(context.Background(), subject, nil, time.Second)
	require.ErrorIs(t, err, ErrCircuitOpen,
		"deadline expiry must still count toward the failure threshold")
}

func TestResilientRequester_BreakerSnapshots(t *testing.T) {
	const (
		subjectA = "svc-a__microservice.cmd.x"
		subjectB = "svc-b__microservice.cmd.y"
	)

	inner := &fakeInner{}
	inner.setSubjectErr(subjectA, errors.New("svc-a down"))
	r := NewResilientRequester(inner, ResilientConfig{
		BreakerEnabled:   true,
		FailureThreshold: 1,
		RecoveryTimeout:  time.Hour,
		HalfOpenProbes:   1,
	}, zerolog.Nop())

	_, _ = r.Request(context.Background(), subjectA, nil, time.Second)
	_, err := r.Request(context.Background(), subjectB, nil, time.Second)
	require.NoError(t, err)

	snapshots := r.BreakerSnapshots()
	byService := make(map[string]BreakerSnapshot, len(snapshots))
	for _, s := range snapshots {
		byService[s.Service] = s
	}

	require.Contains(t, byService, "svc-a")
	require.Contains(t, byService, "svc-b")
	assert.Equal(t, gobreaker.StateOpen, byService["svc-a"].State)
	assert.False(t, byService["svc-a"].Shared)
	assert.Equal(t, gobreaker.StateClosed, byService["svc-b"].State)
	// gobreaker zeroes Counts on every state transition (generation
	// change), so counters are only meaningful on the closed breaker.
	assert.Equal(t, uint32(1), byService["svc-b"].Counts.TotalSuccesses)
}

func TestResilientRequester_SnapshotsEmptyWhenBreakerDisabled(t *testing.T) {
	r := NewResilientRequester(&fakeInner{}, ResilientConfig{}, zerolog.Nop())

	_, err := r.Request(context.Background(), "s", nil, time.Second)
	require.NoError(t, err)
	assert.Empty(t, r.BreakerSnapshots())
}

func TestResilientRequester_ConcurrentMixedSubjects(t *testing.T) {
	// Race-detector workout: concurrent lazy breaker creation across
	// more services than the cap admits, with one service failing.
	// Correctness assertion: healthy dedicated services always succeed.
	inner := &fakeInner{}
	inner.setSubjectErr("bad__microservice.cmd.x", errors.New("down"))
	r := NewResilientRequester(inner, ResilientConfig{
		BreakerEnabled:     true,
		FailureThreshold:   2,
		RecoveryTimeout:    time.Hour,
		HalfOpenProbes:     1,
		MaxBreakerSubjects: 4,
	}, zerolog.Nop())

	subjects := []string{
		"good-0__microservice.cmd.x",
		"good-1__microservice.cmd.x",
		"good-2__microservice.cmd.x",
		"bad__microservice.cmd.x",
		"overflow-0__microservice.cmd.x",
		"overflow-1__microservice.cmd.x",
	}

	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := range 50 {
				subject := subjects[(i+j)%len(subjects)]
				data, err := r.Request(context.Background(), subject, nil, time.Second)
				if strings.HasPrefix(subject, "good-") {
					assert.NoError(t, err)
					assert.Equal(t, []byte("ok"), data)
				}
				_ = r.BreakerSnapshots()
			}
		}(i)
	}
	wg.Wait()

	// The cap held: at most MaxBreakerSubjects dedicated snapshots plus
	// one shared fallback.
	dedicated := 0
	for _, s := range r.BreakerSnapshots() {
		if !s.Shared {
			dedicated++
		}
	}
	assert.LessOrEqual(t, dedicated, 4)
}

func TestResilientRequester_InnerErrorsPropagateVerbatimWhileClosed(t *testing.T) {
	sentinel := errors.New("nats timeout lookalike")
	inner := &fakeInner{err: sentinel}
	r := NewResilientRequester(inner, ResilientConfig{
		BreakerEnabled:   true,
		FailureThreshold: 10,
		RecoveryTimeout:  time.Hour,
		HalfOpenProbes:   1,
	}, zerolog.Nop())

	_, err := r.Request(context.Background(), "s", nil, time.Second)

	require.ErrorIs(t, err, sentinel,
		"closed-state breaker must not rewrap inner errors — the proxy's 504 timeout classification depends on errors.Is reaching nats.ErrTimeout")
}
