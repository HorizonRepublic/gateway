package registry

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testSyncTimeout bounds every channel wait in this file. It is large
// enough to tolerate slow CI hosts yet short enough that an accidentally
// hung test fails the suite in well under a second.
const testSyncTimeout = 500 * time.Millisecond

// validEntryJSON is the on-wire KV value shape the watcher expects:
// a JSON object with an "http" sub-object carrying the routing metadata.
const validEntryJSON = `{"http":{"method":"GET","path":"/users/:id"}}`

// otherEntryJSON is a second valid payload used to distinguish keys in
// tests that load multiple entries.
const otherEntryJSON = `{"http":{"method":"POST","path":"/orders"}}`

// malformedEntryJSON is not valid JSON for HandlerEntry and must trigger
// the decodeEntry warning branches in initialLoad and applyDelta.
const malformedEntryJSON = `{not json`

// fakeKeyValue is a minimal in-memory implementation of jetstream.KeyValue
// that satisfies only the methods watcher.go calls: Keys, Get and
// WatchAll. Every other method panics via the embedded interface, which
// guarantees we notice immediately if watcher.go starts using a new KV
// method that we have not stubbed here.
//
// The fake is driven by function pointers rather than fixed state so each
// test can inject the exact error branch it wants to exercise without
// cross-test coupling.
type fakeKeyValue struct {
	// Embedding the interface gives us a zero-value type that compiles
	// but panics with a nil-deref on any method we do not explicitly
	// override. That is deliberate: unexpected KV calls should fail
	// loudly rather than silently return zero values.
	jetstream.KeyValue

	keysFunc     func(ctx context.Context) ([]string, error)
	getFunc      func(ctx context.Context, key string) (jetstream.KeyValueEntry, error)
	watchAllFunc func(ctx context.Context) (jetstream.KeyWatcher, error)
}

func (f *fakeKeyValue) Keys(ctx context.Context, _ ...jetstream.WatchOpt) ([]string, error) {
	return f.keysFunc(ctx)
}

func (f *fakeKeyValue) Get(ctx context.Context, key string) (jetstream.KeyValueEntry, error) {
	return f.getFunc(ctx, key)
}

// WatchAll wires context-driven teardown on top of the user-provided
// watchAllFunc. When the runWatch context is cancelled (which happens on
// Stop), a helper goroutine closes the returned watcher's updates
// channel, causing runWatch to observe a drop and return. Without this
// shim, runWatch would block indefinitely on <-watcher.Updates() because
// the test fakes never spontaneously publish the sentinel nil entry that
// a real jetstream watcher uses to signal shutdown.
func (f *fakeKeyValue) WatchAll(ctx context.Context, _ ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	kw, err := f.watchAllFunc(ctx)
	if err != nil {
		return nil, err
	}
	if closable, ok := kw.(*fakeKeyWatcher); ok {
		go func() {
			<-ctx.Done()
			_ = closable.Stop()
		}()
	}
	return kw, nil
}

// fakeKeyWatcher is a minimal jetstream.KeyWatcher controlled by the test
// via its updates channel. Callers push entries to simulate live deltas,
// or close the channel via Stop to simulate a server drop.
//
// The watcher is also linked to the runWatch context inside
// fakeKeyValue.WatchAll: when that context is cancelled, a helper
// goroutine closes the updates channel so runWatch observes a drop and
// returns. watchLoop then exits through the ctx.Err() guard. This mirrors
// the real jetstream KV watcher, which tears down its subscription when
// its creation context is cancelled.
type fakeKeyWatcher struct {
	updates chan jetstream.KeyValueEntry
	stopped atomic.Bool
}

func newFakeKeyWatcher() *fakeKeyWatcher {
	return &fakeKeyWatcher{updates: make(chan jetstream.KeyValueEntry, 8)}
}

func (w *fakeKeyWatcher) Updates() <-chan jetstream.KeyValueEntry { return w.updates }

func (w *fakeKeyWatcher) Stop() error {
	// Guard against double-close: watcher.go defers Stop, and some tests
	// may also close the channel directly to simulate a server drop.
	if w.stopped.CompareAndSwap(false, true) {
		close(w.updates)
	}
	return nil
}

// fakeKVEntry is a minimal jetstream.KeyValueEntry returning only the
// fields watcher.go inspects: Key, Value and Operation. The rest of the
// interface returns zero values, which is fine because the watcher never
// reads them.
type fakeKVEntry struct {
	key   string
	value []byte
	op    jetstream.KeyValueOp
}

func (e *fakeKVEntry) Bucket() string              { return "handler_registry" }
func (e *fakeKVEntry) Key() string                 { return e.key }
func (e *fakeKVEntry) Value() []byte               { return e.value }
func (e *fakeKVEntry) Revision() uint64            { return 0 }
func (e *fakeKVEntry) Created() time.Time          { return time.Time{} }
func (e *fakeKVEntry) Delta() uint64               { return 0 }
func (e *fakeKVEntry) Operation() jetstream.KeyValueOp { return e.op }

// newWatcherWithFake wires a Watcher against the given fake KV without
// starting it. Tests that need a running watcher call Start themselves.
func newWatcherWithFake(t *testing.T, kv *fakeKeyValue) (*Watcher, *Store) {
	t.Helper()
	store := NewStore()
	return NewWatcher(kv, store, zerolog.Nop()), store
}

// TestWatcher_InitialLoad_EmptyBucket exercises the ErrNoKeysFound branch
// of initialLoad: the watcher must publish an empty snapshot and still
// fire registered callbacks so downstream components can build their
// initial state from a known-empty registry.
func TestWatcher_InitialLoad_EmptyBucket(t *testing.T) {
	kv := &fakeKeyValue{
		keysFunc: func(context.Context) ([]string, error) {
			return nil, jetstream.ErrNoKeysFound
		},
		watchAllFunc: func(context.Context) (jetstream.KeyWatcher, error) {
			return newFakeKeyWatcher(), nil
		},
	}

	watcher, store := newWatcherWithFake(t, kv)

	var callbackCount atomic.Int32
	watcher.OnChange(func() { callbackCount.Add(1) })

	require.NoError(t, watcher.Start(context.Background()))
	t.Cleanup(watcher.Stop)

	assert.Empty(t, store.Get().Entries)
	assert.Equal(t, int32(1), callbackCount.Load(), "initial load must fire callbacks even on empty bucket")
}

// TestWatcher_InitialLoad_KeysError exercises the non-ErrNoKeysFound error
// branch of initialLoad: a failure listing keys must be wrapped and
// propagated through Start so the bootstrap fails fast.
func TestWatcher_InitialLoad_KeysError(t *testing.T) {
	sentinel := errors.New("keys boom")
	kv := &fakeKeyValue{
		keysFunc: func(context.Context) ([]string, error) { return nil, sentinel },
	}

	watcher, store := newWatcherWithFake(t, kv)

	err := watcher.Start(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel)
	assert.Contains(t, err.Error(), "initial load")
	assert.Empty(t, store.Get().Entries)
}

// TestWatcher_InitialLoad_GetErrorSkipsKey exercises the per-key recovery
// path: a transient Get failure on one key must not block the remaining
// keys from reaching the snapshot. The resulting snapshot contains only
// the successfully decoded entries.
func TestWatcher_InitialLoad_GetErrorSkipsKey(t *testing.T) {
	goodKey := "svc.cmd.users.get"
	badKey := "svc.cmd.users.broken"
	getErr := errors.New("get failed")

	kv := &fakeKeyValue{
		keysFunc: func(context.Context) ([]string, error) {
			return []string{goodKey, badKey}, nil
		},
		getFunc: func(_ context.Context, key string) (jetstream.KeyValueEntry, error) {
			if key == goodKey {
				return &fakeKVEntry{key: key, value: []byte(validEntryJSON), op: jetstream.KeyValuePut}, nil
			}
			return nil, getErr
		},
		watchAllFunc: func(context.Context) (jetstream.KeyWatcher, error) {
			return newFakeKeyWatcher(), nil
		},
	}

	watcher, store := newWatcherWithFake(t, kv)
	require.NoError(t, watcher.Start(context.Background()))
	t.Cleanup(watcher.Stop)

	snap := store.Get()
	assert.Len(t, snap.Entries, 1)
	_, ok := snap.Entries[goodKey]
	assert.True(t, ok, "good key must be present in snapshot")
	_, ok = snap.Entries[badKey]
	assert.False(t, ok, "errored key must be skipped")
}

// TestWatcher_InitialLoad_MalformedValueSkipsKey exercises the
// decodeEntry warning branch in initialLoad: malformed JSON for one key
// must not prevent the remaining keys from loading.
func TestWatcher_InitialLoad_MalformedValueSkipsKey(t *testing.T) {
	goodKey := "svc.cmd.users.get"
	badKey := "svc.cmd.users.corrupted"

	kv := &fakeKeyValue{
		keysFunc: func(context.Context) ([]string, error) {
			return []string{goodKey, badKey}, nil
		},
		getFunc: func(_ context.Context, key string) (jetstream.KeyValueEntry, error) {
			switch key {
			case goodKey:
				return &fakeKVEntry{key: key, value: []byte(validEntryJSON), op: jetstream.KeyValuePut}, nil
			default:
				return &fakeKVEntry{key: key, value: []byte(malformedEntryJSON), op: jetstream.KeyValuePut}, nil
			}
		},
		watchAllFunc: func(context.Context) (jetstream.KeyWatcher, error) {
			return newFakeKeyWatcher(), nil
		},
	}

	watcher, store := newWatcherWithFake(t, kv)
	require.NoError(t, watcher.Start(context.Background()))
	t.Cleanup(watcher.Stop)

	snap := store.Get()
	assert.Len(t, snap.Entries, 1)
	entry, ok := snap.Entries[goodKey]
	require.True(t, ok)
	require.NotNil(t, entry.HTTP)
	assert.Equal(t, "GET", entry.HTTP.Method)
}

// TestWatcher_Stop_CancelsWatchLoop verifies the watchLoop goroutine
// terminates cleanly when Stop is called. The test blocks until the
// watcher signals done via its internal channel (observed indirectly via
// Stop returning).
func TestWatcher_Stop_CancelsWatchLoop(t *testing.T) {
	kv := &fakeKeyValue{
		keysFunc: func(context.Context) ([]string, error) { return nil, jetstream.ErrNoKeysFound },
		watchAllFunc: func(context.Context) (jetstream.KeyWatcher, error) {
			return newFakeKeyWatcher(), nil
		},
	}

	watcher, _ := newWatcherWithFake(t, kv)
	require.NoError(t, watcher.Start(context.Background()))

	done := make(chan struct{})
	go func() {
		watcher.Stop()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(testSyncTimeout):
		t.Fatal("watcher.Stop did not return within timeout")
	}
}

// TestWatcher_Stop_IdempotentBeforeStart verifies Stop is safe to call on
// a watcher that was never started. This matches the documented contract
// in watcher.go and prevents panics in shutdown code paths that may
// unconditionally defer Stop.
func TestWatcher_Stop_IdempotentBeforeStart(t *testing.T) {
	kv := &fakeKeyValue{}
	watcher, _ := newWatcherWithFake(t, kv)

	assert.NotPanics(t, watcher.Stop)
	assert.NotPanics(t, watcher.Stop, "second Stop must also be a no-op")
}

// TestWatcher_Stop_IdempotentAfterStart verifies a second Stop after a
// successful Start is a no-op rather than a double-close panic.
func TestWatcher_Stop_IdempotentAfterStart(t *testing.T) {
	kv := &fakeKeyValue{
		keysFunc: func(context.Context) ([]string, error) { return nil, jetstream.ErrNoKeysFound },
		watchAllFunc: func(context.Context) (jetstream.KeyWatcher, error) {
			return newFakeKeyWatcher(), nil
		},
	}

	watcher, _ := newWatcherWithFake(t, kv)
	require.NoError(t, watcher.Start(context.Background()))

	watcher.Stop()
	assert.NotPanics(t, watcher.Stop, "second Stop must be a no-op")
}

// TestWatcher_Start_AfterStopReturnsErrWatcherStopped pins the
// lifecycle invariant that Start on a stopped watcher refuses to
// resurrect. Without the explicit guard, an accidental
// Start→Stop→Start sequence used to silently leak the second
// goroutine because sync.Once consumed its token on the first run
// and Stop became a no-op the second time around.
func TestWatcher_Start_AfterStopReturnsErrWatcherStopped(t *testing.T) {
	kv := &fakeKeyValue{
		keysFunc: func(context.Context) ([]string, error) { return nil, jetstream.ErrNoKeysFound },
		watchAllFunc: func(context.Context) (jetstream.KeyWatcher, error) {
			return newFakeKeyWatcher(), nil
		},
	}

	watcher, _ := newWatcherWithFake(t, kv)
	require.NoError(t, watcher.Start(context.Background()))

	watcher.Stop()

	err := watcher.Start(context.Background())
	require.ErrorIs(t, err, ErrWatcherStopped, "Start after Stop must surface the lifecycle violation")

	// A second Stop must still be a no-op rather than panic, since the
	// terminal state is the same regardless of how many times callers
	// announce it.
	assert.NotPanics(t, watcher.Stop, "Stop after refusal must remain a no-op")
}

// TestWatcher_StopBeforeStart_ThenStartIsRefused exercises the
// pre-Start Stop path: even when the watcher never ran, Stop flips
// the terminal flag so the first Start call after it returns
// ErrWatcherStopped instead of spawning a goroutine.
func TestWatcher_StopBeforeStart_ThenStartIsRefused(t *testing.T) {
	kv := &fakeKeyValue{
		keysFunc: func(context.Context) ([]string, error) { return nil, jetstream.ErrNoKeysFound },
		watchAllFunc: func(context.Context) (jetstream.KeyWatcher, error) {
			return newFakeKeyWatcher(), nil
		},
	}

	watcher, _ := newWatcherWithFake(t, kv)

	watcher.Stop() // Pre-Start Stop must be safe.

	err := watcher.Start(context.Background())
	require.ErrorIs(t, err, ErrWatcherStopped, "Start after a pre-Start Stop must be refused")
}

// TestWatcher_Start_Idempotent verifies the sync.Once guard on Start:
// repeated Start calls must return the same error observed on the first
// invocation without re-running initialLoad.
func TestWatcher_Start_Idempotent(t *testing.T) {
	var keysCalls atomic.Int32
	kv := &fakeKeyValue{
		keysFunc: func(context.Context) ([]string, error) {
			keysCalls.Add(1)
			return nil, jetstream.ErrNoKeysFound
		},
		watchAllFunc: func(context.Context) (jetstream.KeyWatcher, error) {
			return newFakeKeyWatcher(), nil
		},
	}

	watcher, _ := newWatcherWithFake(t, kv)
	require.NoError(t, watcher.Start(context.Background()))
	require.NoError(t, watcher.Start(context.Background()))
	t.Cleanup(watcher.Stop)

	assert.Equal(t, int32(1), keysCalls.Load(), "initial load must run exactly once")
}

// TestWatcher_Start_IdempotentError verifies the sync.Once guard also
// preserves the error outcome: a second Start must return the same
// wrapped error without re-invoking the KV layer.
func TestWatcher_Start_IdempotentError(t *testing.T) {
	sentinel := errors.New("first failure")
	var keysCalls atomic.Int32
	kv := &fakeKeyValue{
		keysFunc: func(context.Context) ([]string, error) {
			keysCalls.Add(1)
			return nil, sentinel
		},
	}

	watcher, _ := newWatcherWithFake(t, kv)
	first := watcher.Start(context.Background())
	second := watcher.Start(context.Background())

	require.Error(t, first)
	assert.ErrorIs(t, first, sentinel)
	assert.Equal(t, first, second, "Start must return the cached startErr")
	assert.Equal(t, int32(1), keysCalls.Load(), "initial load must not retry on repeat Start")
}

// TestWatcher_WatchLoop_BackoffRespectsCancel exercises the backoff path
// in watchLoop when WatchAll returns an error. The test cancels the
// watcher's internal context mid-backoff (via Stop) and asserts the
// goroutine exits promptly rather than sleeping for the full 2-second
// retry window.
func TestWatcher_WatchLoop_BackoffRespectsCancel(t *testing.T) {
	watchErr := errors.New("watch boom")
	var watchCalls atomic.Int32
	kv := &fakeKeyValue{
		keysFunc: func(context.Context) ([]string, error) { return nil, jetstream.ErrNoKeysFound },
		watchAllFunc: func(context.Context) (jetstream.KeyWatcher, error) {
			watchCalls.Add(1)
			return nil, watchErr
		},
	}

	watcher, _ := newWatcherWithFake(t, kv)
	require.NoError(t, watcher.Start(context.Background()))

	// Give the watch loop a moment to enter the backoff select. The
	// goroutine yields almost immediately after the WatchAll error, so
	// a tiny deterministic wait is enough to observe the failed call.
	deadline := time.Now().Add(testSyncTimeout)
	for watchCalls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	require.GreaterOrEqual(t, watchCalls.Load(), int32(1), "WatchAll must be invoked at least once")

	stopped := make(chan struct{})
	go func() {
		watcher.Stop()
		close(stopped)
	}()

	select {
	case <-stopped:
	case <-time.After(testSyncTimeout):
		t.Fatal("Stop did not interrupt the backoff select within timeout")
	}
}

// TestWatcher_WatchLoop_UpdatesChannelClosed exercises the "watch updates
// channel closed" branch. Closing the updates channel from the fake
// watcher causes runWatch to return an error; watchLoop then logs and
// retries. Stopping the watcher while it is in the retry loop must
// terminate it cleanly.
func TestWatcher_WatchLoop_UpdatesChannelClosed(t *testing.T) {
	var watchCalls atomic.Int32
	watchers := make(chan *fakeKeyWatcher, 4)

	kv := &fakeKeyValue{
		keysFunc: func(context.Context) ([]string, error) { return nil, jetstream.ErrNoKeysFound },
		watchAllFunc: func(context.Context) (jetstream.KeyWatcher, error) {
			watchCalls.Add(1)
			kw := newFakeKeyWatcher()
			select {
			case watchers <- kw:
			default:
			}
			return kw, nil
		},
	}

	watcher, _ := newWatcherWithFake(t, kv)
	require.NoError(t, watcher.Start(context.Background()))
	t.Cleanup(watcher.Stop)

	// Wait for the first WatchAll subscription, then close its updates
	// channel to simulate a server-side drop. runWatch must return the
	// "channel closed" error and watchLoop must enter the backoff path.
	select {
	case kw := <-watchers:
		_ = kw.Stop()
	case <-time.After(testSyncTimeout):
		t.Fatal("watcher did not subscribe within timeout")
	}

	deadline := time.Now().Add(testSyncTimeout)
	for watchCalls.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	assert.GreaterOrEqual(t, watchCalls.Load(), int32(1))
}

// TestWatcher_ApplyDelta_PutUpdatesSnapshot exercises the Put branch of
// applyDelta via a live watch update. A pre-registered callback must fire
// and the new entry must appear in the store's snapshot.
func TestWatcher_ApplyDelta_PutUpdatesSnapshot(t *testing.T) {
	kw := newFakeKeyWatcher()
	kv := &fakeKeyValue{
		keysFunc:     func(context.Context) ([]string, error) { return nil, jetstream.ErrNoKeysFound },
		watchAllFunc: func(context.Context) (jetstream.KeyWatcher, error) { return kw, nil },
	}

	watcher, store := newWatcherWithFake(t, kv)

	changeCh := make(chan struct{}, 4)
	watcher.OnChange(func() { changeCh <- struct{}{} })

	require.NoError(t, watcher.Start(context.Background()))
	t.Cleanup(watcher.Stop)

	// Drain the initial-load callback before pushing the live update so
	// the subsequent assertion observes only the Put-driven signal.
	select {
	case <-changeCh:
	case <-time.After(testSyncTimeout):
		t.Fatal("expected callback after initial load")
	}

	kw.updates <- &fakeKVEntry{
		key:   "svc.cmd.users.get",
		value: []byte(validEntryJSON),
		op:    jetstream.KeyValuePut,
	}

	select {
	case <-changeCh:
	case <-time.After(testSyncTimeout):
		t.Fatal("expected callback after Put delta")
	}

	snap := store.Get()
	entry, ok := snap.Entries["svc.cmd.users.get"]
	require.True(t, ok)
	require.NotNil(t, entry.HTTP)
	assert.Equal(t, "GET", entry.HTTP.Method)
}

// TestWatcher_ApplyDelta_MalformedUpdateIgnored exercises the decodeEntry
// warning branch inside applyDelta. A malformed Put must leave the prior
// snapshot untouched and must not fire any callback.
func TestWatcher_ApplyDelta_MalformedUpdateIgnored(t *testing.T) {
	kw := newFakeKeyWatcher()
	kv := &fakeKeyValue{
		keysFunc:     func(context.Context) ([]string, error) { return nil, jetstream.ErrNoKeysFound },
		watchAllFunc: func(context.Context) (jetstream.KeyWatcher, error) { return kw, nil },
	}

	watcher, store := newWatcherWithFake(t, kv)

	var callbackCount atomic.Int32
	changeCh := make(chan struct{}, 4)
	watcher.OnChange(func() {
		callbackCount.Add(1)
		changeCh <- struct{}{}
	})

	require.NoError(t, watcher.Start(context.Background()))
	t.Cleanup(watcher.Stop)

	// Drain initial-load callback.
	<-changeCh

	// First push a valid entry to establish a non-empty baseline.
	kw.updates <- &fakeKVEntry{
		key:   "svc.cmd.users.get",
		value: []byte(validEntryJSON),
		op:    jetstream.KeyValuePut,
	}
	<-changeCh

	baseline := callbackCount.Load()

	// Now push a malformed update. applyDelta must log and return
	// without swapping the snapshot or firing the callback.
	kw.updates <- &fakeKVEntry{
		key:   "svc.cmd.users.broken",
		value: []byte(malformedEntryJSON),
		op:    jetstream.KeyValuePut,
	}

	// And push a second valid entry under a different key so we have a
	// deterministic sync point: once the second valid callback fires,
	// the malformed one before it has definitely been processed.
	kw.updates <- &fakeKVEntry{
		key:   "svc.cmd.orders.create",
		value: []byte(otherEntryJSON),
		op:    jetstream.KeyValuePut,
	}
	<-changeCh

	// The malformed entry must never appear in the snapshot, and the
	// callback must have fired exactly twice after the baseline: once
	// for the broken entry (no, it must NOT fire) and once for the
	// second valid entry.
	assert.Equal(t, baseline+1, callbackCount.Load(), "malformed update must not fire callback")

	snap := store.Get()
	_, ok := snap.Entries["svc.cmd.users.broken"]
	assert.False(t, ok, "malformed entry must not reach snapshot")
	_, ok = snap.Entries["svc.cmd.users.get"]
	assert.True(t, ok, "prior valid entry must survive malformed update")
	_, ok = snap.Entries["svc.cmd.orders.create"]
	assert.True(t, ok, "subsequent valid entry must reach snapshot")
}

// TestWatcher_ApplyDelta_UnknownOperationIgnored exercises the default
// branch of the applyDelta switch: an operation value outside the known
// Put/Delete/Purge set must not swap the snapshot or fire callbacks.
// This guards the defense-in-depth invariant called out in watcher.go.
func TestWatcher_ApplyDelta_UnknownOperationIgnored(t *testing.T) {
	kw := newFakeKeyWatcher()
	kv := &fakeKeyValue{
		keysFunc:     func(context.Context) ([]string, error) { return nil, jetstream.ErrNoKeysFound },
		watchAllFunc: func(context.Context) (jetstream.KeyWatcher, error) { return kw, nil },
	}

	watcher, store := newWatcherWithFake(t, kv)

	var callbackCount atomic.Int32
	changeCh := make(chan struct{}, 4)
	watcher.OnChange(func() {
		callbackCount.Add(1)
		changeCh <- struct{}{}
	})

	require.NoError(t, watcher.Start(context.Background()))
	t.Cleanup(watcher.Stop)

	// Drain initial-load callback.
	<-changeCh
	baseline := callbackCount.Load()

	// Push an entry with a sentinel operation value that does not match
	// any of the known KeyValueOp constants. 255 is chosen because
	// KeyValueOp is uint8 and the defined constants occupy small values.
	kw.updates <- &fakeKVEntry{
		key:   "svc.cmd.users.mystery",
		value: []byte(validEntryJSON),
		op:    jetstream.KeyValueOp(255),
	}

	// Push a trailing valid Put as a sync point.
	kw.updates <- &fakeKVEntry{
		key:   "svc.cmd.orders.create",
		value: []byte(otherEntryJSON),
		op:    jetstream.KeyValuePut,
	}
	<-changeCh

	assert.Equal(t, baseline+1, callbackCount.Load(), "unknown op must not fire callback")

	snap := store.Get()
	_, ok := snap.Entries["svc.cmd.users.mystery"]
	assert.False(t, ok, "unknown-op entry must not reach snapshot")
}

// TestWatcher_OnChange_PostStartCallbackFires verifies that callbacks
// registered after a successful Start still receive subsequent delta
// notifications. The lock-then-snapshot pattern in fireCallbacks must
// pick up the newly appended slice entry.
func TestWatcher_OnChange_PostStartCallbackFires(t *testing.T) {
	kw := newFakeKeyWatcher()
	kv := &fakeKeyValue{
		keysFunc:     func(context.Context) ([]string, error) { return nil, jetstream.ErrNoKeysFound },
		watchAllFunc: func(context.Context) (jetstream.KeyWatcher, error) { return kw, nil },
	}

	watcher, _ := newWatcherWithFake(t, kv)
	require.NoError(t, watcher.Start(context.Background()))
	t.Cleanup(watcher.Stop)

	changeCh := make(chan struct{}, 4)
	watcher.OnChange(func() { changeCh <- struct{}{} })

	kw.updates <- &fakeKVEntry{
		key:   "svc.cmd.orders.create",
		value: []byte(otherEntryJSON),
		op:    jetstream.KeyValuePut,
	}

	select {
	case <-changeCh:
	case <-time.After(testSyncTimeout):
		t.Fatal("post-start callback did not fire on delta")
	}
}

// TestWatcher_OnChange_MultipleCallbacksInOrder verifies the registration
// order contract documented on OnChange: callbacks fire in the order they
// were registered, within a single invocation of fireCallbacks.
func TestWatcher_OnChange_MultipleCallbacksInOrder(t *testing.T) {
	kv := &fakeKeyValue{
		keysFunc:     func(context.Context) ([]string, error) { return nil, jetstream.ErrNoKeysFound },
		watchAllFunc: func(context.Context) (jetstream.KeyWatcher, error) { return newFakeKeyWatcher(), nil },
	}

	watcher, _ := newWatcherWithFake(t, kv)

	var (
		mu    sync.Mutex
		order []int
	)
	for i := 1; i <= 3; i++ {
		id := i
		watcher.OnChange(func() {
			mu.Lock()
			order = append(order, id)
			mu.Unlock()
		})
	}

	require.NoError(t, watcher.Start(context.Background()))
	t.Cleanup(watcher.Stop)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []int{1, 2, 3}, order)
}

// TestWatcher_Reconcile_DropsStaleEntries certifies the belt-and-
// suspenders path that compensates for silent JetStream KV TTL
// expirations. Bucket MaxAge evicts stream messages without writing
// a delete/purge tombstone, so the watch subscription never observes
// the removal. reconcile must detect the discrepancy on its own by
// listing live KV keys and dropping anything in the store that is
// no longer present.
func TestWatcher_Reconcile_DropsStaleEntries(t *testing.T) {
	liveKey := "svc.cmd.users.get"
	staleKey := "svc.cmd.users.old"

	kv := &fakeKeyValue{
		keysFunc: func(context.Context) ([]string, error) {
			// KV reports only the live key — the stale one has silently
			// aged out of the stream.
			return []string{liveKey}, nil
		},
	}

	watcher, store := newWatcherWithFake(t, kv)

	// Pre-populate the store with both keys as if the watcher had
	// seen Put events for each at some earlier point.
	store.Replace(&Snapshot{Entries: map[string]HandlerEntry{
		liveKey:  {HTTP: &HTTPMeta{Method: "GET", Path: "/users/:id"}},
		staleKey: {HTTP: &HTTPMeta{Method: "GET", Path: "/old/:id"}},
	}})

	var callbackCount atomic.Int32
	watcher.OnChange(func() { callbackCount.Add(1) })

	watcher.reconcile(context.Background())

	snap := store.Get()
	assert.Len(t, snap.Entries, 1, "only the live key must remain after reconcile")
	_, liveOK := snap.Entries[liveKey]
	_, staleOK := snap.Entries[staleKey]
	assert.True(t, liveOK, "live key must still be present")
	assert.False(t, staleOK, "stale key must be dropped")
	assert.Equal(t, int32(1), callbackCount.Load(), "callback must fire exactly once when state changes")
}

// TestWatcher_Reconcile_NoOpWhenAllKeysAlive certifies that reconcile
// does NOT touch the store or fire callbacks when every store entry
// is still present in KV. This is the 99% case in a healthy cluster —
// reconcile must be cheap when there is nothing to clean up, otherwise
// downstream rebuild callbacks fire on every tick and pollute logs.
func TestWatcher_Reconcile_NoOpWhenAllKeysAlive(t *testing.T) {
	key := "svc.cmd.users.get"

	kv := &fakeKeyValue{
		keysFunc: func(context.Context) ([]string, error) {
			return []string{key}, nil
		},
	}

	watcher, store := newWatcherWithFake(t, kv)
	store.Replace(&Snapshot{Entries: map[string]HandlerEntry{
		key: {HTTP: &HTTPMeta{Method: "GET", Path: "/users/:id"}},
	}})

	var callbackCount atomic.Int32
	watcher.OnChange(func() { callbackCount.Add(1) })

	watcher.reconcile(context.Background())

	assert.Len(t, store.Get().Entries, 1)
	assert.Equal(t, int32(0), callbackCount.Load(), "no callbacks on a clean reconcile")
}

// TestWatcher_Reconcile_EmptyBucketDropsEverything certifies that a
// KV bucket that has emptied out (every key TTL-expired) correctly
// drops every store entry. The jetstream.ErrNoKeysFound response is
// handled identically to an empty slice — both mean "nothing alive".
func TestWatcher_Reconcile_EmptyBucketDropsEverything(t *testing.T) {
	kv := &fakeKeyValue{
		keysFunc: func(context.Context) ([]string, error) {
			return nil, jetstream.ErrNoKeysFound
		},
	}

	watcher, store := newWatcherWithFake(t, kv)
	store.Replace(&Snapshot{Entries: map[string]HandlerEntry{
		"svc.cmd.a": {HTTP: &HTTPMeta{Method: "GET", Path: "/a"}},
		"svc.cmd.b": {HTTP: &HTTPMeta{Method: "GET", Path: "/b"}},
	}})

	var callbackCount atomic.Int32
	watcher.OnChange(func() { callbackCount.Add(1) })

	watcher.reconcile(context.Background())

	assert.Empty(t, store.Get().Entries)
	assert.Equal(t, int32(1), callbackCount.Load())
}

// TestWatcher_Reconcile_PreservesStoreOnListError certifies that a
// transient Keys() failure does NOT wipe the store. The watcher must
// treat list errors as "state unknown, do not drop" and wait for the
// next tick to retry.
func TestWatcher_Reconcile_PreservesStoreOnListError(t *testing.T) {
	kv := &fakeKeyValue{
		keysFunc: func(context.Context) ([]string, error) {
			return nil, errors.New("transient kv failure")
		},
	}

	watcher, store := newWatcherWithFake(t, kv)
	store.Replace(&Snapshot{Entries: map[string]HandlerEntry{
		"svc.cmd.a": {HTTP: &HTTPMeta{Method: "GET", Path: "/a"}},
	}})

	var callbackCount atomic.Int32
	watcher.OnChange(func() { callbackCount.Add(1) })

	watcher.reconcile(context.Background())

	assert.Len(t, store.Get().Entries, 1, "store must not be mutated on list error")
	assert.Equal(t, int32(0), callbackCount.Load())
}

// goroutineDelta is a coarse-grained leak detector that bounds the
// amount of goroutines created by the body. The runtime startup
// noise (timer goroutines, pollers) means runtime.NumGoroutine is
// not perfectly stable across runs, so the test asserts on a small
// slack window rather than exact equality.
func goroutineDelta(t *testing.T, body func()) int {
	t.Helper()
	runtime.GC()
	before := runtime.NumGoroutine()
	body()

	// Give scheduled goroutines a chance to wind down before we
	// snapshot. A short sleep avoids hard-coding internal scheduler
	// timing while still catching obvious leaks (a single leaked
	// goroutine survives many seconds, not microseconds).
	time.Sleep(50 * time.Millisecond)
	runtime.GC()

	return runtime.NumGoroutine() - before
}

// TestWatcher_StartStopRace_NoGoroutineLeak pins the lifecycle race
// fix: a Stop firing concurrently with Start must always tear down
// the watch goroutine instead of installing it after stopped=true is
// already set.
//
// Before the fix, Start released lifecycleMu after the stopped check,
// then re-acquired it later to install w.cancel. A Stop firing in
// that window saw cancel == nil, set stopped=true, and returned
// clean — but the watch goroutine was launched anyway because Start
// had already entered startOnce.Do. The leaked goroutine ran past
// the test's lifetime.
//
// The test launches Start and Stop concurrently many times against
// fresh watchers and asserts the goroutine count converges back to
// roughly its initial value. A real leak would accumulate goroutines
// and produce a delta proportional to the iteration count.
func TestWatcher_StartStopRace_NoGoroutineLeak(t *testing.T) {
	const iterations = 30

	delta := goroutineDelta(t, func() {
		for i := 0; i < iterations; i++ {
			kv := &fakeKeyValue{
				keysFunc: func(context.Context) ([]string, error) { return nil, jetstream.ErrNoKeysFound },
				watchAllFunc: func(context.Context) (jetstream.KeyWatcher, error) {
					return newFakeKeyWatcher(), nil
				},
			}
			watcher, _ := newWatcherWithFake(t, kv)

			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				_ = watcher.Start(context.Background())
			}()
			go func() {
				defer wg.Done()
				watcher.Stop()
			}()
			wg.Wait()

			// One last Stop ensures any goroutine that did get launched
			// is cancelled before the next iteration. Idempotent by
			// construction.
			watcher.Stop()
		}
	})

	// A few stragglers from the test runner are tolerable; a leak
	// would scale with iterations and easily exceed this slack.
	assert.LessOrEqual(t, delta, 4,
		"watch goroutines must not leak under Start/Stop racing (delta=%d)", delta)
}

// TestWatcher_InitialLoadCanceledByStop pins the cancellation
// invariant for the bootstrap path: a rapid Stop during a slow
// initialLoad must interrupt the in-flight KV calls rather than
// waiting for the bootstrap-time 10s timeout to expire.
//
// The fake's Keys callback blocks on its supplied ctx so the only
// way Start returns is via Stop firing initialLoad's cancel func.
// Without the fix, Stop has nothing to cancel until startOnce
// completes — meaning a 10s wait for initialLoad's inner timeout to
// pop, ten seconds during which the gateway shutdown sequence is
// stalled.
func TestWatcher_InitialLoadCanceledByStop(t *testing.T) {
	keysEntered := make(chan struct{})

	kv := &fakeKeyValue{
		keysFunc: func(ctx context.Context) ([]string, error) {
			close(keysEntered)
			<-ctx.Done()
			return nil, ctx.Err()
		},
		watchAllFunc: func(context.Context) (jetstream.KeyWatcher, error) {
			return newFakeKeyWatcher(), nil
		},
	}

	watcher, _ := newWatcherWithFake(t, kv)

	startDone := make(chan error, 1)
	go func() {
		startDone <- watcher.Start(context.Background())
	}()

	select {
	case <-keysEntered:
	case <-time.After(testSyncTimeout):
		t.Fatal("initialLoad never reached the kv.Keys call")
	}

	// Now race Stop against the blocked initialLoad. Without the
	// cancel-on-Stop hook, Stop has nothing to cancel and Start
	// blocks until the inner 10s timeout pops.
	stopDone := make(chan struct{})
	go func() {
		watcher.Stop()
		close(stopDone)
	}()

	select {
	case <-stopDone:
	case <-time.After(testSyncTimeout):
		t.Fatal("Stop did not interrupt initialLoad within timeout")
	}

	select {
	case <-startDone:
	case <-time.After(testSyncTimeout):
		t.Fatal("Start did not return after Stop cancelled initialLoad")
	}
}
