//go:build integration

package registry

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"
)

// TestWatcher_ReflectsKVChangesIntoStore spins up a real NATS JetStream
// container via testcontainers, wires a Watcher against an empty
// handler_registry bucket, and asserts that a KV Put propagates into the
// Store and fires the registered change callback. This exercises the
// full JetStream KV watch path end-to-end — initial load, watch loop,
// delta application, and callback dispatch.
func TestWatcher_ReflectsKVChangesIntoStore(t *testing.T) {
	ctx := context.Background()
	// The testcontainers nats module passes "-DV -js" as the default command
	// line, so JetStream is already enabled without any extra WithArgument
	// call. Pinning the image tag keeps the test reproducible across CI
	// runs even if the module's default image floats forward.
	container, err := tcnats.Run(ctx, "nats:2.11.7")
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	url, err := container.ConnectionString(ctx)
	require.NoError(t, err)

	nc, err := nats.Connect(url)
	require.NoError(t, err)
	t.Cleanup(nc.Close)

	js, err := jetstream.New(nc)
	require.NoError(t, err)

	kv, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:  "handler_registry",
		History: 1,
	})
	require.NoError(t, err)

	store := NewStore()
	logger := zerolog.Nop()
	watcher := NewWatcher(kv, store, logger)

	changeCh := make(chan struct{}, 16)
	watcher.OnChange(func() {
		select {
		case changeCh <- struct{}{}:
		default:
			// Channel full — the test already observed the signal, so we
			// intentionally drop further notifications to avoid blocking
			// the watcher goroutine.
		}
	})

	require.NoError(t, watcher.Start(ctx))
	t.Cleanup(watcher.Stop)

	// Drain the initial-load callback fired by Start. The bucket is empty
	// at this point, so the initial snapshot is an empty map — draining
	// the signal here ensures the subsequent wait observes only callbacks
	// triggered by the live watch path.
	select {
	case <-changeCh:
	case <-time.After(2 * time.Second):
		t.Fatal("expected change callback after initial load")
	}

	entryJSON, err := json.Marshal(HandlerEntry{
		HTTP: &HTTPMeta{Method: "GET", Path: "/users/:id"},
	})
	require.NoError(t, err)

	_, err = kv.Put(ctx, "users-svc.cmd.users.get", entryJSON)
	require.NoError(t, err)

	// Wait for the Put to propagate. Retry the snapshot read under a
	// bounded deadline because the watch loop and the test goroutine race
	// between the callback firing and the snapshot becoming visible.
	deadline := time.Now().Add(5 * time.Second)
	var entry HandlerEntry
	var ok bool
	for time.Now().Before(deadline) {
		select {
		case <-changeCh:
		case <-time.After(200 * time.Millisecond):
		}
		snap := store.Get()
		if entry, ok = snap.Entries["users-svc.cmd.users.get"]; ok {
			break
		}
	}
	require.True(t, ok, "entry should be in snapshot after Put")
	require.NotNil(t, entry.HTTP)
	assert.Equal(t, "GET", entry.HTTP.Method)
	assert.Equal(t, "/users/:id", entry.HTTP.Path)

	// Delete-path verification. nestjs-jetstream's handler-metadata
	// cleanup relies on TTL + heartbeat expiry, and the watcher MUST
	// observe those as Purge events so the routing table stops
	// exposing stale handlers after a controller rewrite or pod
	// restart. Explicit Delete on the bucket is the deterministic
	// equivalent we can exercise without waiting out a real TTL
	// window in the test — the watcher's applyDelta handles Delete
	// and Purge through the same code path.
	require.NoError(t, kv.Delete(ctx, "users-svc.cmd.users.get"))

	deadline = time.Now().Add(5 * time.Second)
	var removed bool
	for time.Now().Before(deadline) {
		select {
		case <-changeCh:
		case <-time.After(200 * time.Millisecond):
		}

		snap := store.Get()
		if _, stillPresent := snap.Entries["users-svc.cmd.users.get"]; !stillPresent {
			removed = true

			break
		}
	}
	assert.True(t, removed, "entry should be dropped from snapshot after Delete")
}
