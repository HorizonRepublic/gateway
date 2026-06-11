//go:build bench

package bench

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	vegeta "github.com/tsenart/vegeta/v12/lib"
)

func TestSummarize_ExtractsCorePercentiles(t *testing.T) {
	var m vegeta.Metrics
	// 1000 results: 999 fast (1ms), 1 slow (500ms) — exercises the p999 tail.
	for range 999 {
		m.Add(&vegeta.Result{Code: 200, Latency: time.Millisecond})
	}
	m.Add(&vegeta.Result{Code: 200, Latency: 500 * time.Millisecond})
	m.Close()

	got := Summarize(&m, 5000)

	assert.Equal(t, 5000, got.RequestedRate)
	assert.Equal(t, uint64(1000), got.Requests)
	assert.InDelta(t, 1.0, got.Success, 0.0001, "all 200s must yield success ratio 1.0")
	assert.Equal(t, time.Millisecond, got.P50)
	assert.GreaterOrEqual(t, got.P999, 100*time.Millisecond, "p999 must reflect the slow tail")
	assert.Greater(t, got.Throughput, 0.0)
}

func TestDetectCeiling_PicksLastSustainedRung(t *testing.T) {
	steps := []StepResult{
		{RequestedRate: 1000, Throughput: 998, Success: 1.0},
		{RequestedRate: 2000, Throughput: 1995, Success: 1.0},
		{RequestedRate: 5000, Throughput: 4980, Success: 0.999},
		{RequestedRate: 10000, Throughput: 6200, Success: 0.94}, // knee: throughput stalls, errors climb
	}
	got := DetectCeiling(steps)
	assert.Equal(t, 5000, got.RequestedRate, "ceiling is the last rung that kept up")
}

func TestDetectCeiling_FirstRungAlreadyBroken(t *testing.T) {
	steps := []StepResult{
		{RequestedRate: 1000, Throughput: 300, Success: 0.5},
	}
	got := DetectCeiling(steps)
	assert.Equal(t, 1000, got.RequestedRate, "degenerate: return the only rung")
}
