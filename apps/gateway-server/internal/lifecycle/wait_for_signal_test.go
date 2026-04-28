//go:build !windows

package lifecycle

import (
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWaitForSignal_ReturnsOnSIGTERM exercises the signal-driven
// shutdown trigger. A goroutine fires SIGTERM at the current
// process; WaitForSignal must observe it and return the signal
// value. The test is bounded by a generous timeout so a regression
// that leaves WaitForSignal blocking forever fails loudly instead
// of hanging the suite.
//
// SIGTERM is the production shutdown signal documented on
// DefaultSignals; SIGINT would also work but SIGTERM mirrors what
// orchestrators (systemd, Kubernetes, Docker) actually send when a
// pod is asked to drain.
//
// Build constraint excludes Windows because POSIX signal semantics
// do not apply there; the gateway only ships on Linux/macOS so the
// constraint matches the deployment surface.
func TestWaitForSignal_ReturnsOnSIGTERM(t *testing.T) {
	// Given: a separate goroutine that fires SIGTERM after the
	// receiver has had a chance to register its signal handler.
	signalErr := make(chan error, 1)
	go func() {
		// A small delay ensures WaitForSignal has called signal.Notify
		// before the SIGTERM lands. The kernel queues the signal until
		// a handler exists, so the delay is a sequencing aid rather
		// than a hard requirement.
		time.Sleep(50 * time.Millisecond)
		signalErr <- syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
	}()

	// When: the test process waits for the signal in a goroutine
	// bounded by a select-driven timeout.
	got := make(chan any, 1)
	go func() {
		got <- WaitForSignal()
	}()

	// Then: WaitForSignal returns SIGTERM within the budget. The
	// generous 1s timeout exists so a hung WaitForSignal surfaces as
	// a test failure rather than a hung suite.
	select {
	case sig := <-got:
		require.NoError(t, <-signalErr, "syscall.Kill must succeed")
		assert.Equal(t, syscall.SIGTERM, sig,
			"WaitForSignal must surface the exact signal that fired")
	case <-time.After(1 * time.Second):
		t.Fatal("WaitForSignal did not return within 1s of receiving SIGTERM")
	}
}
