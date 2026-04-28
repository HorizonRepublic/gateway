package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/rs/zerolog"
)

// ErrWatcherStopped is returned by Start when invoked on a Watcher
// whose lifecycle has already terminated via Stop. Restarting a
// stopped watcher is a structural lifecycle violation: the start/stop
// pair is single-shot by design so resources held by the previous run
// are not silently revived. Callers that need a second life should
// construct a fresh Watcher.
var ErrWatcherStopped = errors.New("registry: watcher stopped")

// reconcileInterval is how often the watcher performs a full-bucket
// key scan and drops any store entries that no longer exist in KV.
// Paired with the nestjs-jetstream heartbeat TTL (default 30 s on the
// bucket) so silent TTL expirations are detected within a bounded
// window without being so aggressive that the reconcile itself
// becomes observable load on the cluster.
//
// Chosen as roughly half the typical bucket MaxAge so any stale
// entry is guaranteed to be dropped within one full TTL period —
// the worst-case staleness window is reconcileInterval + bucket
// MaxAge, which at 15s + 30s caps operator-visible lag at 45s.
const reconcileInterval = 15 * time.Second

// streamProbeInterval is how often the watcher pings the underlying
// JetStream KV bucket via Status() to detect stream-level outages
// that nats.go's WatchAll subscription does NOT surface as a closed
// Updates() channel.
//
// Empirically observed failure mode: when the stream backing the KV
// bucket is deleted (operator action, JetStream cluster failover, or
// retention reset), the WatchAll subscription stays open and silent —
// no error, no channel close, no notification. Without an explicit
// probe the watch goroutine hangs indefinitely on <-Updates(), gateway
// cannot pick up new routes, and the only operator-visible signal is
// "/readyz still returns 200 but no traffic flows".
//
// Status() is a lightweight metadata call against the KV stream and
// returns ErrStreamNotFound (or similar) when the stream is gone.
// We probe at a 3-second cadence so a stream-deletion event is
// detected within at most 3s, triggering watchLoop's restart-with-
// backoff path. The probe runs on the same goroutine as the watch
// loop's Updates consumer, so it is naturally serialised with delta
// application — no lock coordination needed.
const streamProbeInterval = 3 * time.Second

// ChangeCallback is invoked after every successful snapshot replacement.
// Downstream layers (notably the routing-table builder in `routing`)
// register a callback via Watcher.OnChange to rebuild their derived state
// whenever KV entries change.
type ChangeCallback func()

// Watcher mirrors a handler_registry JetStream KV bucket into a Store,
// keeping it up-to-date via jetstream.KeyValue.Watch().
//
// Lifecycle:
//  1. Start performs a full initial scan (blocking until the first
//     snapshot is loaded), then spawns a background goroutine that
//     consumes the watch channel and applies deltas.
//  2. OnChange registers a callback invoked after every successful
//     Store.Replace. Multiple callbacks may be registered; they are
//     invoked in registration order on the watcher goroutine.
//  3. Stop cancels the watch goroutine's context and waits for it to
//     exit cleanly.
//
// The Watcher is the ONLY writer to the Store. All other components are
// readers, which means the Store's atomic semantics are enforced by
// construction rather than by convention.
type Watcher struct {
	kv        jetstream.KeyValue
	store     *Store
	logger    zerolog.Logger
	callbacks []ChangeCallback
	cbMu      sync.RWMutex

	// lifecycleMu guards every field below it. Cancel installation
	// MUST happen while the lock is held so a Stop racing Start
	// observes either both cancel funcs or neither — never a partial
	// state that lets the watch goroutine survive past stopped=true.
	lifecycleMu      sync.Mutex
	initialCancel    context.CancelFunc
	cancel           context.CancelFunc
	goroutineStarted bool
	done             chan struct{}
	startOnce        sync.Once
	startErr         error
	stopped          bool
}

// NewWatcher constructs a Watcher for the given KV bucket and store.
// The provided logger is cloned with a "component" field so watcher logs
// can be filtered from the rest of the gateway output.
func NewWatcher(kv jetstream.KeyValue, store *Store, logger zerolog.Logger) *Watcher {
	return &Watcher{
		kv:     kv,
		store:  store,
		logger: logger.With().Str("component", "registry.Watcher").Logger(),
		done:   make(chan struct{}),
	}
}

// OnChange registers a callback invoked after every successful Store
// replacement. Multiple callbacks may be registered; they run on the
// watcher goroutine in registration order.
//
// Callbacks MUST NOT call OnChange or Stop on the same Watcher, directly
// or transitively. The implementation protects the callback slice with a
// snapshot-under-read-lock pattern so those calls would not deadlock, but
// mutating the subscription set from inside a callback risks subtle
// ordering bugs — keep callbacks side-effect-free with respect to the
// watcher that invokes them.
func (w *Watcher) OnChange(cb ChangeCallback) {
	w.cbMu.Lock()
	defer w.cbMu.Unlock()
	w.callbacks = append(w.callbacks, cb)
}

// Start performs the initial full-scan load and spawns the background
// watch loop. Returns once the initial snapshot has been published to the
// store — the gateway is ready to route requests by the time this call
// returns.
//
// The watch goroutine runs until Stop is called, automatically restarting
// the underlying JetStream watcher with a short backoff if the NATS
// connection drops or the watch channel closes unexpectedly.
//
// Start is single-use: calling it more than once is a no-op that returns
// the error (or nil) observed on the first invocation. This prevents
// goroutine leaks from an accidental double-Start during bootstrap
// refactors and makes the lifecycle symmetric with Stop.
//
// Calling Start on a watcher that has already been Stop()ped returns
// ErrWatcherStopped so accidental Start→Stop→Start sequences fail loud
// rather than silently leak the previous goroutine's resources or
// resurrect a torn-down lifecycle.
//
// Race-safety: cancel funcs are installed under lifecycleMu inside
// startOnce.Do so the first Start owns the lifecycle and subsequent
// Starts pass through with the cached startErr. A Stop racing the
// first Start either observes the cancel funcs (and tears down the
// in-flight initialLoad plus the about-to-spawn watch goroutine) or
// runs purely before any cancel is installed (and flips stopped=true,
// so the re-check inside startOnce.Do refuses to spawn the goroutine).
//
// The previous design released the lock after the stopped check and
// re-acquired it later to assign w.cancel — leaving a window where
// Stop saw cancel == nil and returned clean while the watch goroutine
// was launched anyway.
func (w *Watcher) Start(ctx context.Context) error {
	w.lifecycleMu.Lock()
	if w.stopped {
		w.lifecycleMu.Unlock()
		return ErrWatcherStopped
	}
	w.lifecycleMu.Unlock()

	w.startOnce.Do(func() {
		// Install both cancel funcs under the lock so a concurrent
		// Stop either sees both (and cancels both) or sees neither
		// (and is a no-op). The lock is released before initialLoad
		// blocks on KV calls — Stop concurrently flipping stopped=true
		// is what cancels initialLoadCtx, and the post-load re-check
		// of stopped inside the lock prevents the watch goroutine
		// from being launched after the lifecycle is terminal.
		w.lifecycleMu.Lock()
		if w.stopped {
			w.lifecycleMu.Unlock()
			w.startErr = ErrWatcherStopped
			return
		}
		initialLoadCtx, initialCancel := context.WithCancel(ctx)
		watchCtx, watchCancel := context.WithCancel(context.Background())
		w.initialCancel = initialCancel
		w.cancel = watchCancel
		w.lifecycleMu.Unlock()

		if err := w.initialLoad(initialLoadCtx); err != nil {
			// initialLoad failed (genuine KV error or Stop-driven
			// cancellation). Cancel the watch ctx defensively so its
			// resources are released — no goroutine ever consumed it.
			watchCancel()
			w.startErr = fmt.Errorf("registry watcher initial load: %w", err)
			return
		}

		// Re-check stopped under lock BEFORE spawning the goroutine.
		// A Stop firing between unlock and this point has already
		// cancelled watchCtx and is not waiting on w.done (goroutine
		// Started was still false when Stop sampled it). Launching
		// the goroutine anyway would leak it past the gateway
		// shutdown — runWatch would observe the cancel, close w.done,
		// but no caller is listening. Skip the launch entirely when
		// the lifecycle is already terminal.
		w.lifecycleMu.Lock()
		if w.stopped {
			w.lifecycleMu.Unlock()
			watchCancel()
			return
		}
		w.goroutineStarted = true
		w.lifecycleMu.Unlock()

		go w.watchLoop(watchCtx)
	})
	return w.startErr
}

// Stop cancels the watch loop's context and waits for the goroutine to
// exit. Safe to call multiple times and before Start; every later call
// is a no-op. After Stop returns, the watcher is in a terminal state —
// further Start calls return ErrWatcherStopped instead of resurrecting
// the lifecycle.
//
// Stop also cancels the initial-load context so a Stop racing a slow
// bootstrap (e.g., a flaky NATS handshake during shutdown) interrupts
// the in-flight kv.Keys / kv.Get calls instead of waiting for the
// inner 10s timeout to pop. Without that, the gateway shutdown
// sequence would stall for ten seconds every time a SIGTERM arrived
// before initialLoad completed.
func (w *Watcher) Stop() {
	w.lifecycleMu.Lock()
	alreadyStopped := w.stopped
	initialCancel := w.initialCancel
	cancel := w.cancel
	goroutineStarted := w.goroutineStarted
	w.stopped = true
	w.lifecycleMu.Unlock()

	if alreadyStopped {
		return
	}

	// Cancel the initial-load ctx first so an in-flight kv.Keys /
	// kv.Get unblocks. If Stop ran before Start ever installed cancels,
	// both pointers are nil — flipping stopped=true is enough to make
	// any subsequent Start return ErrWatcherStopped.
	if initialCancel != nil {
		initialCancel()
	}
	if cancel != nil {
		cancel()
	}

	if goroutineStarted {
		<-w.done
	}
}

func (w *Watcher) initialLoad(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	keys, err := w.kv.Keys(ctx)
	if err != nil {
		if errors.Is(err, jetstream.ErrNoKeysFound) {
			w.store.Replace(&Snapshot{Entries: map[string]HandlerEntry{}})
			w.fireCallbacks()
			return nil
		}
		return fmt.Errorf("list keys: %w", err)
	}

	entries := make(map[string]HandlerEntry, len(keys))
	for _, key := range keys {
		kve, err := w.kv.Get(ctx, key)
		if err != nil {
			w.logger.Warn().Err(err).Str("key", key).Msg("skipping key on initial load")
			continue
		}
		entry, err := decodeEntry(kve.Value())
		if err != nil {
			w.logger.Warn().Err(err).Str("key", key).Msg("skipping malformed KV value on initial load")
			continue
		}
		entries[key] = entry
	}

	w.store.Replace(&Snapshot{Entries: entries})
	w.fireCallbacks()
	return nil
}

func (w *Watcher) watchLoop(ctx context.Context) {
	defer close(w.done)

	for {
		if err := w.runWatch(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			w.logger.Error().Err(err).Msg("watch loop error; restarting after backoff")
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
	}
}

func (w *Watcher) runWatch(ctx context.Context) error {
	// WatchAll is intentionally invoked WITHOUT IgnoreDeletes — the
	// watcher must see KV deletes and purges so the Store reflects
	// route removals in real time. nestjs-jetstream's handler-metadata
	// KV uses TTL + heartbeat cleanup: stale entries expire as Purge
	// events on the watch channel, and failing to observe those would
	// let orphaned routes linger in the routing table across controller
	// rewrites and service restarts until the next gateway reboot.
	watcher, err := w.kv.WatchAll(ctx)
	if err != nil {
		return fmt.Errorf("kv WatchAll: %w", err)
	}
	defer func() { _ = watcher.Stop() }()

	// Reconciliation tick handles silent TTL expirations from the KV
	// bucket. nestjs-jetstream's handler-metadata cleanup relies on
	// bucket MaxAge, which drops stream messages without writing a
	// delete/purge tombstone — meaning the watch subscription never
	// observes the removal. A periodic full-bucket scan is the only
	// way to detect those vanished keys and evict them from the local
	// store. The ticker shares the watch goroutine so its execution is
	// serialized with applyDelta: no locks needed because only one
	// function mutates the Store at a time.
	reconcileTicker := time.NewTicker(reconcileInterval)
	defer reconcileTicker.Stop()

	// Stream-presence probe: nats.go's WatchAll does not signal stream
	// deletion on the Updates() channel. Without an explicit probe the
	// watcher silently hangs when the JetStream stream backing the KV
	// bucket disappears (deletion, cluster failover, retention reset).
	// The probe returns an error when the stream is gone, breaking the
	// inner loop so watchLoop's restart-with-backoff can re-acquire the
	// subscription against the recreated stream.
	streamProbeTicker := time.NewTicker(streamProbeInterval)
	defer streamProbeTicker.Stop()

	// nats.go's JetStream KV watcher sends a nil entry once the initial
	// replay of existing entries completes. We do not discard the replay
	// entries: initialLoad may have run at T0 while a concurrent writer
	// pushed a new value at T1, before WatchAll subscribed at T2. Letting
	// the replay go through applyDelta guarantees eventual consistency —
	// the worst case is a handful of redundant snapshot swaps on startup,
	// which is cheap and race-free because Store.Replace is atomic.
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-reconcileTicker.C:
			w.reconcile(ctx)
		case <-streamProbeTicker.C:
			if err := w.probeStream(ctx); err != nil {
				return err
			}
		case kve, ok := <-watcher.Updates():
			if !ok {
				return fmt.Errorf("watch updates channel closed")
			}
			if kve == nil {
				// End-of-initial-replay marker. Nothing to apply, and we
				// stay subscribed for subsequent live updates.
				continue
			}
			w.applyDelta(kve)
		}
	}
}

// probeStream calls KV Status to verify the underlying JetStream
// stream still exists. nats.go's WatchAll subscription does not
// surface stream-level outages — the channel stays open and silent
// even after the stream is deleted — so an explicit probe is the
// only reliable signal.
//
// Returns nil when the stream is healthy (or when the probe itself
// timed out, since a transient network blip is not a stream-deletion
// event and forcing a restart on every blip would amplify churn).
// Returns a wrapped error only when Status reports a genuine
// stream-not-found condition; the caller (runWatch) MUST surface that
// error to watchLoop so the restart-with-backoff path re-acquires
// the subscription.
func (w *Watcher) probeStream(ctx context.Context) error {
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	if _, err := w.kv.Status(probeCtx); err != nil {
		// Distinguish stream-not-found (genuine outage requiring a
		// restart) from transient probe failures (timeouts, peer
		// resets) where retrying the same subscription is correct.
		if errors.Is(err, jetstream.ErrStreamNotFound) ||
			errors.Is(err, jetstream.ErrBucketNotFound) {
			return fmt.Errorf("watch loop probe: bucket gone: %w", err)
		}
		// Probe-context timeout means the JS server did not respond
		// in 2s; that is a network condition, not a stream-deletion.
		// Don't trigger a restart — the next probe tick will retry,
		// and the existing watch subscription will continue receiving
		// updates if the network blip resolves.
		w.logger.Debug().Err(err).Msg("watch loop probe: transient KV Status error; not restarting")
	}

	return nil
}

// reconcile performs a full-bucket key scan and drops any store
// entries that no longer exist in KV. This is the safety net for
// silent TTL expirations: JetStream bucket MaxAge evicts stream
// messages without writing a delete/purge tombstone, so the watch
// subscription never observes the removal. A periodic scan is the
// only way to detect those vanished keys.
//
// Reconcile NEVER adds new entries — adds are handled deterministically
// by the watch subscription's Put events. This asymmetry is deliberate:
// adds must be observed exactly once with correct ordering (so
// applyDelta can decode and validate), while drops are a recovery
// operation that can be applied idempotently from any starting state.
//
// Called on the watchLoop goroutine, so its execution is serialized
// with applyDelta — no locks are required because only one function
// at a time is mutating the Store.
func (w *Watcher) reconcile(ctx context.Context) {
	scanCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	liveKeys, err := w.kv.Keys(scanCtx)
	if err != nil && !errors.Is(err, jetstream.ErrNoKeysFound) {
		w.logger.Warn().Err(err).Msg("reconcile: list keys failed")

		return
	}

	alive := make(map[string]struct{}, len(liveKeys))
	for _, key := range liveKeys {
		alive[key] = struct{}{}
	}

	current := w.store.Get()

	var stale []string
	for key := range current.Entries {
		if _, ok := alive[key]; !ok {
			stale = append(stale, key)
		}
	}

	if len(stale) == 0 {
		return
	}

	// Sort the stale-key slice so the log output is deterministic
	// across runs — Go map iteration order is randomized, and
	// operators reading incident logs appreciate stable diffs.
	sort.Strings(stale)

	next := make(map[string]HandlerEntry, len(current.Entries)-len(stale))
	for key, entry := range current.Entries {
		if _, ok := alive[key]; ok {
			next[key] = entry
		}
	}

	w.store.Replace(&Snapshot{Entries: next})
	w.logger.Info().
		Int("dropped", len(stale)).
		Strs("keys", stale).
		Msg("reconcile: dropped stale store entries")
	w.fireCallbacks()
}

func (w *Watcher) applyDelta(kve jetstream.KeyValueEntry) {
	current := w.store.Get()
	key := kve.Key()

	next := make(map[string]HandlerEntry, len(current.Entries)+1)
	for k, v := range current.Entries {
		next[k] = v
	}

	switch kve.Operation() {
	case jetstream.KeyValuePut:
		entry, err := decodeEntry(kve.Value())
		if err != nil {
			w.logger.Warn().Err(err).Str("key", key).Msg("skipping malformed KV update")
			return
		}
		next[key] = entry
	case jetstream.KeyValueDelete, jetstream.KeyValuePurge:
		// Delete and Purge are the two operations nats.go surfaces for
		// entry removal. Delete marks the key as deleted (soft); Purge
		// evicts every historical revision. Both must drop the entry
		// from the snapshot — the routing table cares about the
		// CURRENT set of live handlers, not the history of key states.
		delete(next, key)
	default:
		// Unknown operation — ignore it instead of swapping in an
		// unchanged snapshot and firing spurious callbacks.
		return
	}

	w.store.Replace(&Snapshot{Entries: next})
	w.fireCallbacks()
}

func (w *Watcher) fireCallbacks() {
	// Snapshot the callback slice under the read lock, then release before
	// invocation. Holding the lock across user code would turn any callback
	// that touches the watcher (for example to register another callback
	// or to call Stop during a shutdown cascade) into a deadlock, because
	// sync.RWMutex does not support recursive acquisition. Allocating a
	// small slice per delta is cheap relative to the JSON marshal and
	// atomic Replace that already happened upstream of this call.
	w.cbMu.RLock()
	cbs := make([]ChangeCallback, len(w.callbacks))
	copy(cbs, w.callbacks)
	w.cbMu.RUnlock()

	for _, cb := range cbs {
		cb()
	}
}

func decodeEntry(raw []byte) (HandlerEntry, error) {
	var entry HandlerEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return HandlerEntry{}, fmt.Errorf("decode handler entry: %w", err)
	}
	return entry, nil
}
