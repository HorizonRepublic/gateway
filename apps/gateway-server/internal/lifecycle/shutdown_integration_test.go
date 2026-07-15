//go:build integration

package lifecycle

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	tcnats "github.com/testcontainers/testcontainers-go/modules/nats"
)

// TestDrain_RealNATSDrainCompletesInFlightWork is the empirical proof
// behind the drain step's contract, pinned against a real NATS server.
//
// nats.go's Conn.Drain returns immediately after INITIATING the drain
// (a background goroutine finishes in-flight subscription callbacks
// and publishes, then closes the connection). The lifecycle drain
// step therefore may not treat Drain's return as completion — it has
// to wait until the connection reports closed. This test drives a
// message into a deliberately slow subscription handler and asserts
// two things about the moment Drain (the lifecycle sequence) returns:
//
//  1. The slow handler has finished — no in-flight work was cut off
//     mid-callback (the "no request left behind" rolling-deploy
//     guarantee).
//  2. The connection is actually closed — the process may exit
//     without killing a socket that is still draining.
//
// A regression back to "Drain() returned, therefore drained" fails
// both assertions deterministically: the step would return while the
// handler is still sleeping and the connection still draining.
func TestDrain_RealNATSDrainCompletesInFlightWork(t *testing.T) {
	ctx := context.Background()
	container, err := tcnats.Run(ctx, "nats:2.11.7")
	require.NoError(t, err)
	t.Cleanup(func() { _ = container.Terminate(ctx) })

	url, err := container.ConnectionString(ctx)
	require.NoError(t, err)

	// drainConn plays the gateway connection: it owns the slow
	// subscription and is the connection the lifecycle step drains.
	// The publisher rides a separate connection, mirroring the
	// production topology where the gateway and its upstream peers
	// sit on different NATS clients.
	drainConn, err := natsgo.Connect(url)
	require.NoError(t, err)
	t.Cleanup(func() {
		if !drainConn.IsClosed() {
			drainConn.Close()
		}
	})

	pubConn, err := natsgo.Connect(url)
	require.NoError(t, err)
	t.Cleanup(pubConn.Close)

	const handlerWorkTime = 300 * time.Millisecond
	started := make(chan struct{})
	var handlerFinished atomic.Bool

	_, err = drainConn.Subscribe("drain.slow", func(_ *natsgo.Msg) {
		close(started)
		time.Sleep(handlerWorkTime)
		handlerFinished.Store(true)
	})
	require.NoError(t, err)
	require.NoError(t, drainConn.Flush())

	require.NoError(t, pubConn.Publish("drain.slow", []byte("work")))
	require.NoError(t, pubConn.Flush())

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("subscription handler never received the message")
	}

	var buf logCapture
	Drain(Options{
		HTTP:    &fakeHTTPServer{},
		Watcher: &recordingWatcher{},
		NATS:    drainConn,
		Timeout: 10 * time.Second,
		Logger:  zerolog.New(&buf),
	})

	assert.True(t, handlerFinished.Load(),
		"the in-flight subscription callback must complete before the drain step returns — "+
			"returning earlier means main exits and kills work mid-callback")
	assert.True(t, drainConn.IsClosed(),
		"the connection must actually be closed when the drain step returns")
	assert.Contains(t, buf.String(), `"message":"shutdown step: nats drain complete"`)
}
