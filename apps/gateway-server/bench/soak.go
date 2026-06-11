//go:build bench

package bench

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	vegeta "github.com/tsenart/vegeta/v12/lib"
)

// SoakResult captures memory stability across the soak window.
type SoakResult struct {
	Rate       int
	Duration   time.Duration
	RSSStart   uint64 // bytes
	RSSEnd     uint64 // bytes
	RSSPeak    uint64 // bytes
	Throughput float64
	Success    float64
}

// dockerRSSBytes returns the current resident memory of a container by
// parsing `docker stats --no-stream`. The MemUsage column looks like
// "12.3MiB / 1.94GiB"; we parse the first operand.
func dockerRSSBytes(containerID string) (uint64, error) {
	out, err := exec.Command(
		"docker", "stats", "--no-stream",
		"--format", "{{.MemUsage}}", containerID,
	).Output()
	if err != nil {
		return 0, fmt.Errorf("docker stats: %w", err)
	}
	field := strings.TrimSpace(string(out))
	used, _, ok := strings.Cut(field, "/")
	if !ok {
		return 0, fmt.Errorf("unexpected MemUsage format: %q", field)
	}
	return parseSize(strings.TrimSpace(used))
}

// parseSize converts a docker size string (e.g. "12.3MiB") to bytes.
func parseSize(s string) (uint64, error) {
	units := []struct {
		suffix string
		mult   float64
	}{
		{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10},
		{"GB", 1e9}, {"MB", 1e6}, {"kB", 1e3}, {"B", 1},
	}
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			n, err := strconv.ParseFloat(strings.TrimSuffix(s, u.suffix), 64)
			if err != nil {
				return 0, fmt.Errorf("parse size %q: %w", s, err)
			}
			return uint64(n * u.mult), nil
		}
	}
	return 0, fmt.Errorf("unknown size unit: %q", s)
}

// runSoak drives a scenario at a fixed rate for the given duration,
// sampling RSS every 30s. Latency is collected but the focus is RSS
// stability; a steady-or-plateauing RSSEnd vs RSSPeak means no leak.
func runSoak(sc Scenario, containerID string, rate int, dur time.Duration) (SoakResult, error) {
	start, err := dockerRSSBytes(containerID)
	if err != nil {
		return SoakResult{}, err
	}
	res := SoakResult{Rate: rate, Duration: dur, RSSStart: start, RSSPeak: start}

	done := make(chan struct{})
	peak := make(chan uint64, 1)
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		max := start
		for {
			select {
			case <-done:
				peak <- max
				return
			case <-ticker.C:
				if rss, e := dockerRSSBytes(containerID); e == nil && rss > max {
					max = rss
				}
			}
		}
	}()

	atk := vegeta.NewAttacker()
	var m vegeta.Metrics
	for r := range atk.Attack(sc.Targeter, vegeta.Rate{Freq: rate, Per: time.Second}, dur, "soak") {
		m.Add(r)
	}
	m.Close()
	atk.Stop()
	close(done)
	res.RSSPeak = <-peak

	end, err := dockerRSSBytes(containerID)
	if err != nil {
		return SoakResult{}, err
	}
	res.RSSEnd = end
	if end > res.RSSPeak {
		res.RSSPeak = end
	}
	res.Throughput = m.Throughput
	res.Success = m.Success
	return res, nil
}
