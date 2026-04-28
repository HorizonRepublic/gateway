package http

import (
	"bytes"
	"math"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestResolveMaxBodyBytes_NormalRangeReturnsCast pins the happy path:
// any value at or below math.MaxInt32 is forwarded to Hertz unchanged.
func TestResolveMaxBodyBytes_NormalRangeReturnsCast(t *testing.T) {
	got, err := resolveMaxBodyBytes(1<<20, zerolog.Nop())
	require.NoError(t, err)
	assert.Equal(t, 1<<20, got)
}

// TestResolveMaxBodyBytes_ZeroIsAccepted documents that an explicit
// zero limit propagates as zero — Hertz interprets that as
// "unlimited" or "no body" depending on its own defaults; the helper
// stays out of the policy decision.
func TestResolveMaxBodyBytes_ZeroIsAccepted(t *testing.T) {
	got, err := resolveMaxBodyBytes(0, zerolog.Nop())
	require.NoError(t, err)
	assert.Equal(t, 0, got)
}

// TestResolveMaxBodyBytes_NegativeFails verifies the startup-fast
// guard. A negative cap has no sensible interpretation; returning an
// error stops the bootstrap before traffic hits the pod.
func TestResolveMaxBodyBytes_NegativeFails(t *testing.T) {
	got, err := resolveMaxBodyBytes(-1, zerolog.Nop())
	require.Error(t, err)
	assert.ErrorContains(t, err, "non-negative")
	assert.Equal(t, 0, got)
}

// TestResolveMaxBodyBytes_OverflowClampsToInt32 pins the 32-bit
// overflow guard. Operators who request a very large body cap on a
// 32-bit build (or a future int width change) get the maximum the
// platform can deliver, plus a WARN log line so the clamp is visible.
func TestResolveMaxBodyBytes_OverflowClampsToInt32(t *testing.T) {
	var buf bytes.Buffer
	logger := zerolog.New(&buf)

	got, err := resolveMaxBodyBytes(int64(math.MaxInt32)+1, logger)
	require.NoError(t, err)
	assert.Equal(t, math.MaxInt32, got, "values past int32 must clamp, not overflow")

	logged := buf.String()
	assert.Contains(t, logged, "clamping to MaxInt32")
	assert.Contains(t, logged, `"clamped":2147483647`)
}

// TestResolveMaxBodyBytes_WayPastInt32StillClamps exercises a
// pathologically large input (1 << 40) to verify the upper-bound
// branch handles values much larger than the cutoff.
func TestResolveMaxBodyBytes_WayPastInt32StillClamps(t *testing.T) {
	got, err := resolveMaxBodyBytes(int64(1)<<40, zerolog.Nop())
	require.NoError(t, err)
	assert.Equal(t, math.MaxInt32, got)
}
