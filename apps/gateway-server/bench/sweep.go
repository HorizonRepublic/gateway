//go:build bench

package bench

import (
	"time"

	vegeta "github.com/tsenart/vegeta/v12/lib"
)

// StepResult is one rung of the rate ladder: what we asked for and what
// the stack actually delivered, plus the latency distribution at that load.
type StepResult struct {
	RequestedRate int           // arrival rate we drove (req/s)
	Requests      uint64        // total requests issued
	Throughput    float64       // achieved successful req/s
	Success       float64       // success ratio in [0,1]
	P50           time.Duration // median latency
	P99           time.Duration // 99th percentile
	P999          time.Duration // 99.9th percentile (tail)
	Max           time.Duration // worst observed latency
}

// Summarize flattens a CLOSED vegeta.Metrics into a StepResult. The
// caller must have invoked metrics.Close() first; percentile fields are
// only populated after Close.
func Summarize(m *vegeta.Metrics, requestedRate int) StepResult {
	return StepResult{
		RequestedRate: requestedRate,
		Requests:      m.Requests,
		Throughput:    m.Throughput,
		Success:       m.Success,
		P50:           m.Latencies.P50,
		P99:           m.Latencies.P99,
		P999:          m.Latencies.Quantile(0.999),
		Max:           m.Latencies.Max,
	}
}

// sustained reports whether a rung kept up: throughput within 5% of the
// requested rate and a success ratio at or above 0.99.
func sustained(s StepResult) bool {
	return s.Success >= 0.99 && s.Throughput >= 0.95*float64(s.RequestedRate)
}

// DetectCeiling returns the highest rung that sustained the load. Steps
// are assumed ordered by ascending RequestedRate. If no rung sustained,
// the first rung is returned as a degenerate floor.
func DetectCeiling(steps []StepResult) StepResult {
	ceiling := steps[0]
	for _, s := range steps {
		if !sustained(s) {
			break
		}
		ceiling = s
	}
	return ceiling
}
