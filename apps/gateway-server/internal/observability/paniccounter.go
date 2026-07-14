package observability

import "sync/atomic"

// panicsRecovered counts handler-chain panics the HTTP recovery
// middleware has caught since process start. A package-level atomic is
// the deliberate minimal shape for the current phase: the gateway has
// no metrics registry yet, and a panic is rare enough that a single
// monotonic counter carries the full operational signal ("is this
// number moving?"). When a metrics registry lands, its collector reads
// PanicsRecovered() and exports the value as a monotonic counter —
// callers of IncPanicRecovered stay untouched.
var panicsRecovered atomic.Uint64

// IncPanicRecovered records one recovered panic. Called exclusively by
// the HTTP recovery middleware; safe from any goroutine.
func IncPanicRecovered() {
	panicsRecovered.Add(1)
}

// PanicsRecovered returns the number of panics recovered since process
// start. Monotonic, never reset; consumers derive rates by sampling.
func PanicsRecovered() uint64 {
	return panicsRecovered.Load()
}
