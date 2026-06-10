package nats

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeInner is a programmable innerRequester. hold, when non-nil, is
// closed by the test to release in-flight calls; err, when non-nil, is
// returned for every call.
type fakeInner struct {
	mu    sync.Mutex
	calls int
	hold  chan struct{}
	err   error
}

func (f *fakeInner) Request(ctx context.Context, _ string, _ []byte, _ time.Duration) ([]byte, error) {
	f.mu.Lock()
	f.calls++
	hold := f.hold
	err := f.err
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
