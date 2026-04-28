package lifecycle

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// logCapture is a thread-safe in-memory io.Writer for zerolog,
// used to inspect drain step output during shutdown tests.
type logCapture struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *logCapture) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logCapture) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// CountField returns how many log lines contain the named JSON field.
// The match is naive substring (`"<name>":`) — sufficient for
// asserting that every drain step emitted an `elapsed` field.
func (l *logCapture) CountField(name string) int {
	return strings.Count(l.String(), `"`+name+`":`)
}

// stepRecorder is a shared monotonic counter the fake collaborators
// use to stamp the order in which Drain invokes them. The sequence
// numbers expose the strict HTTP→watcher→router→NATS contract that
// the shutdown sequence promises.
type stepRecorder struct {
	counter atomic.Int64
}

func (r *stepRecorder) next() int64 { return r.counter.Add(1) }

// fakeHTTPServer captures Shutdown invocations for test assertions.
// A pre-seeded err (nil by default) is returned from Shutdown so the
// test can exercise both the happy and error branches of the drain
// sequence.
type fakeHTTPServer struct {
	mu          sync.Mutex
	called      bool
	receivedCtx context.Context
	err         error
	recorder    *stepRecorder
	order       int64
}

func (f *fakeHTTPServer) Shutdown(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called = true
	f.receivedCtx = ctx
	if f.recorder != nil {
		f.order = f.recorder.next()
	}
	return f.err
}

// fakeNATSConn captures Drain invocations for test assertions.
type fakeNATSConn struct {
	mu       sync.Mutex
	called   bool
	err      error
	recorder *stepRecorder
	order    int64
	// blockUntil, when non-nil, holds the goroutine inside Drain until
	// the channel is closed or the receive races a return. Used by the
	// drain-timeout test to simulate a hung NATS connection.
	blockUntil chan struct{}
}

func (f *fakeNATSConn) Drain() error {
	if f.blockUntil != nil {
		<-f.blockUntil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.called = true
	if f.recorder != nil {
		f.order = f.recorder.next()
	}
	return f.err
}

// recordingWatcher is a WatcherStopper used by every test that needs
// a `WatcherStopper` value. The shared instance avoids depending on
// `internal/registry` from this package's test surface — the real
// `*registry.Watcher` will be wired into Drain at the bootstrap
// site once the registry package lands.
type recordingWatcher struct {
	mu       sync.Mutex
	called   bool
	recorder *stepRecorder
	order    int64
}

func (r *recordingWatcher) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.called = true
	if r.recorder != nil {
		r.order = r.recorder.next()
	}
}

// recordingRouter is a RouterCloser used by ordering and error tests
// to stamp the drain step's sequence number and to optionally
// surface a configured Close error.
type recordingRouter struct {
	mu       sync.Mutex
	called   bool
	err      error
	recorder *stepRecorder
	order    int64
}

func (r *recordingRouter) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.called = true
	if r.recorder != nil {
		r.order = r.recorder.next()
	}
	return r.err
}

func TestDrain_CallsAllFourStepsInOrder(t *testing.T) {
	recorder := &stepRecorder{}
	http := &fakeHTTPServer{recorder: recorder}
	nats := &fakeNATSConn{recorder: recorder}
	watcher := &recordingWatcher{recorder: recorder}
	router := &recordingRouter{recorder: recorder}

	Drain(Options{
		HTTP:      http,
		Watcher:   watcher,
		RateLimit: router,
		NATS:      nats,
		Timeout:   1 * time.Second,
		Logger:    zerolog.Nop(),
	})

	http.mu.Lock()
	require.True(t, http.called, "HTTP.Shutdown must be called")
	httpOrder := http.order
	http.mu.Unlock()

	watcher.mu.Lock()
	require.True(t, watcher.called, "Watcher.Stop must be called")
	watcherOrder := watcher.order
	watcher.mu.Unlock()

	router.mu.Lock()
	require.True(t, router.called, "RateLimit.Close must be called")
	routerOrder := router.order
	router.mu.Unlock()

	nats.mu.Lock()
	require.True(t, nats.called, "NATS.Drain must be called")
	natsOrder := nats.order
	nats.mu.Unlock()

	// Drain MUST quiesce HTTP first (stop accepting new work), watcher
	// second (so a late KV delta cannot mutate the routing table after
	// we have stopped serving), the rate-limit router third (so
	// in-flight Allow calls observe ratelimit.ErrStoreClosed instead
	// of a raw NATS-draining error from the KV store), and NATS last
	// (so any in-flight upstream replies — including the router's
	// close-time RPCs — have a chance to land before we close the
	// socket). Reordering breaks the no-request-left-behind guarantee
	// during rolling deployments and surfaces transient backend
	// errors as user-visible 500s during the drain window.
	assert.Less(t, httpOrder, watcherOrder, "HTTP.Shutdown must run before Watcher.Stop")
	assert.Less(t, watcherOrder, routerOrder, "Watcher.Stop must run before RateLimit.Close")
	assert.Less(t, routerOrder, natsOrder, "RateLimit.Close must run before NATS.Drain")
}

func TestDrain_NoRouterSkipsCloseStep(t *testing.T) {
	// Drain must remain usable when no rate-limit router is wired
	// (legacy boot paths, tests, or feature-flag-disabled deployments).
	// Skipping the step instead of crashing keeps Drain a uniform API
	// across configurations.
	http := &fakeHTTPServer{}
	nats := &fakeNATSConn{}
	watcher := &recordingWatcher{}

	assert.NotPanics(t, func() {
		Drain(Options{
			HTTP:    http,
			Watcher: watcher,
			NATS:    nats,
			Timeout: 1 * time.Second,
			Logger:  zerolog.Nop(),
		})
	})

	nats.mu.Lock()
	assert.True(t, nats.called, "NATS drain must still run when RateLimit is nil")
	nats.mu.Unlock()
}

func TestDrain_LogsElapsedPerStep(t *testing.T) {
	// Each step's completion (or failure) log line MUST carry an
	// `elapsed` field so a slow-drain postmortem can pin which
	// resource burned the budget without correlating timestamps
	// across separate log lines.
	http := &fakeHTTPServer{}
	nats := &fakeNATSConn{}
	watcher := &recordingWatcher{}
	router := &recordingRouter{}

	var buf logCapture
	logger := zerolog.New(&buf)

	Drain(Options{
		HTTP:      http,
		Watcher:   watcher,
		RateLimit: router,
		NATS:      nats,
		Timeout:   1 * time.Second,
		Logger:    logger,
	})

	output := buf.String()
	assert.Contains(t, output, `"message":"shutdown step: http complete"`)
	assert.Contains(t, output, `"message":"shutdown step: registry watcher complete"`)
	assert.Contains(t, output, `"message":"shutdown step: ratelimit router complete"`)
	assert.Contains(t, output, `"message":"shutdown step: nats drain complete"`)
	assert.Contains(t, output, `"message":"gateway shutdown: drain complete"`)
	// Every completion log line carries `elapsed` — the field is what
	// makes the per-step duration grep-able.
	elapsedCount := buf.CountField("elapsed")
	assert.GreaterOrEqual(t, elapsedCount, 5,
		"4 step-completion lines + 1 overall-complete line must each carry elapsed")
}

func TestDrain_RateLimitCloseErrorDoesNotAbortSequence(t *testing.T) {
	// A failing rate-limit Close (e.g., a backend Store that errors
	// during its own teardown) must NOT prevent the NATS connection
	// from draining. The process is about to exit; leaving the NATS
	// socket open is strictly worse than leaking a half-closed Store.
	http := &fakeHTTPServer{}
	nats := &fakeNATSConn{}
	watcher := &recordingWatcher{}
	router := &recordingRouter{err: errors.New("ratelimit close boom")}

	Drain(Options{
		HTTP:      http,
		Watcher:   watcher,
		RateLimit: router,
		NATS:      nats,
		Timeout:   1 * time.Second,
		Logger:    zerolog.Nop(),
	})

	router.mu.Lock()
	assert.True(t, router.called, "RateLimit.Close must run even if it errors")
	router.mu.Unlock()

	nats.mu.Lock()
	assert.True(t, nats.called, "NATS drain must still run after RateLimit close failure")
	nats.mu.Unlock()
}

func TestDrain_HTTPErrorDoesNotAbortSequence(t *testing.T) {
	http := &fakeHTTPServer{err: errors.New("http shutdown boom")}
	nats := &fakeNATSConn{}
	watcher := &recordingWatcher{}

	Drain(Options{
		HTTP:    http,
		Watcher: watcher,
		NATS:    nats,
		Timeout: 1 * time.Second,
		Logger:  zerolog.Nop(),
	})

	nats.mu.Lock()
	assert.True(t, nats.called, "NATS drain must still run even after HTTP.Shutdown failure")
	nats.mu.Unlock()
}

func TestDrain_NATSErrorDoesNotPanic(t *testing.T) {
	http := &fakeHTTPServer{}
	nats := &fakeNATSConn{err: errors.New("nats drain boom")}
	watcher := &recordingWatcher{}

	assert.NotPanics(t, func() {
		Drain(Options{
			HTTP:    http,
			Watcher: watcher,
			NATS:    nats,
			Timeout: 1 * time.Second,
			Logger:  zerolog.Nop(),
		})
	})
}

func TestDrain_AppliesTimeoutToHTTPShutdown(t *testing.T) {
	// Verify the context passed into HTTP.Shutdown carries the
	// configured timeout as its deadline. This is the only step
	// that actually consumes the context; confirming it here keeps
	// the timeout wiring honest.
	http := &fakeHTTPServer{}
	nats := &fakeNATSConn{}
	watcher := &recordingWatcher{}

	const timeout = 500 * time.Millisecond
	Drain(Options{
		HTTP:    http,
		Watcher: watcher,
		NATS:    nats,
		Timeout: timeout,
		Logger:  zerolog.Nop(),
	})

	http.mu.Lock()
	defer http.mu.Unlock()
	require.NotNil(t, http.receivedCtx)
	deadline, ok := http.receivedCtx.Deadline()
	require.True(t, ok, "HTTP.Shutdown must receive a context with a deadline")
	remaining := time.Until(deadline)
	assert.Greater(t, remaining, time.Duration(0))
	assert.LessOrEqual(t, remaining, timeout)
}

func TestDrain_NATSDrainTimeoutDoesNotBlockShutdown(t *testing.T) {
	// Verify a hung NATS Drain cannot stall the gateway past the
	// configured timeout. The fake's Drain blocks indefinitely on
	// blockUntil so the only way Drain returns is via the goroutine
	// timeout branch in drainNATS.
	http := &fakeHTTPServer{}
	nats := &fakeNATSConn{blockUntil: make(chan struct{})}
	watcher := &recordingWatcher{}

	const timeout = 50 * time.Millisecond
	const slack = 250 * time.Millisecond

	done := make(chan struct{})
	go func() {
		Drain(Options{
			HTTP:    http,
			Watcher: watcher,
			NATS:    nats,
			Timeout: timeout,
			Logger:  zerolog.Nop(),
		})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(timeout + slack):
		// Unblock the goroutine so we do not leak it past the test.
		close(nats.blockUntil)
		t.Fatalf("Drain did not return within timeout+slack (%v)", timeout+slack)
	}

	// Release the orphan goroutine so the test process does not retain
	// it — drain returned via the timeout branch but the worker is
	// still parked on the channel send.
	close(nats.blockUntil)
}

func TestDrain_StopsWatcherIdempotently(t *testing.T) {
	// Calling Drain twice against the same watcher must not panic.
	// The recordingWatcher is permissive (Stop is a plain method
	// without a sync.Once guard); the production *registry.Watcher
	// adds a sync.Once and the second Drain is a no-op there. This
	// test pins that Drain itself does not blow up when invoked more
	// than once.
	http := &fakeHTTPServer{}
	nats := &fakeNATSConn{}
	watcher := &recordingWatcher{}

	opts := Options{
		HTTP:    http,
		Watcher: watcher,
		NATS:    nats,
		Timeout: 1 * time.Second,
		Logger:  zerolog.Nop(),
	}

	assert.NotPanics(t, func() {
		Drain(opts)
		Drain(opts)
	})
}
