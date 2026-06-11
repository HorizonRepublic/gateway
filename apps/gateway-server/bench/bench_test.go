//go:build bench

package bench

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	vegeta "github.com/tsenart/vegeta/v12/lib"
)

// TestBench_Smoke is the harness self-protection: a low-rate burst that
// asserts the stack comes up and serves successfully. NOT a perf claim.
func TestBench_Smoke(t *testing.T) {
	ctx := context.Background()
	stack, err := startStack(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { stack.stop(ctx) })

	sc := Scenarios(stack.gatewayURL)[0] // proxy-echo
	atk := vegeta.NewAttacker()
	var m vegeta.Metrics
	for r := range atk.Attack(sc.Targeter, vegeta.Rate{Freq: 50, Per: time.Second}, 3*time.Second, "smoke") {
		m.Add(r)
	}
	m.Close()
	atk.Stop()

	require.Greater(t, m.Throughput, 0.0, "harness must produce throughput")
	require.Equal(t, 1.0, m.Success, "smoke load must be served without errors")
}

// TestBench_Baseline climbs the ladder for each scenario and prints the
// ceiling. Long-running; gated behind -tags=bench so it never runs in the
// default suite. Numbers are recorded into the perf docs from this output.
func TestBench_Baseline(t *testing.T) {
	ctx := context.Background()
	stack, err := startStack(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { stack.stop(ctx) })

	for _, sc := range Scenarios(stack.gatewayURL) {
		steps := runLadder(sc, defaultLadder)
		for _, s := range steps {
			t.Logf("scenario=%s rate=%d throughput=%.0f p50=%s p99=%s p999=%s success=%.4f",
				sc.Name, s.RequestedRate, s.Throughput, s.P50, s.P99, s.P999, s.Success)
		}
		ceiling := DetectCeiling(steps)
		fmt.Printf("| %s | %d | %s | %s | %s | %.4f |\n",
			sc.Name, ceiling.RequestedRate, ceiling.P50, ceiling.P99, ceiling.P999, ceiling.Success)
	}
}

// TestBench_Soak is opt-in: set BENCH_SOAK=1h (any time.ParseDuration value).
func TestBench_Soak(t *testing.T) {
	raw := os.Getenv("BENCH_SOAK")
	if raw == "" {
		t.Skip("BENCH_SOAK unset — soak skipped")
	}
	dur, err := time.ParseDuration(raw)
	require.NoError(t, err, "BENCH_SOAK must be a Go duration, e.g. 1h")

	ctx := context.Background()
	stack, err := startStack(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { stack.stop(ctx) })

	sc := Scenarios(stack.gatewayURL)[0] // proxy-echo
	res, err := runSoak(sc, stack.containerID, 5000, dur)
	require.NoError(t, err)
	t.Logf("soak rate=%d dur=%s rss_start=%d rss_peak=%d rss_end=%d success=%.4f",
		res.Rate, res.Duration, res.RSSStart, res.RSSPeak, res.RSSEnd, res.Success)
	// Leak guard: end RSS must not exceed peak by more than 5% (i.e. it plateaued).
	require.LessOrEqual(t, res.RSSEnd, uint64(float64(res.RSSPeak)*1.05),
		"RSS at end must plateau, not keep climbing")
}
