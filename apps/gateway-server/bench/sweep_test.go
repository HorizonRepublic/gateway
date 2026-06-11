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
