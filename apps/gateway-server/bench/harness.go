//go:build bench

package bench

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/testcontainers/testcontainers-go/modules/compose"
	vegeta "github.com/tsenart/vegeta/v12/lib"
)

// composeFile is the e2e compose stack, reused verbatim. Path is relative
// to the bench package directory (go test sets cwd to the package dir).
const composeFile = "../e2e/compose.yml"

// stepDuration is how long each rung of the ceiling ladder is driven.
const stepDuration = 30 * time.Second

// defaultLadder is the ascending arrival-rate ladder (req/s) the sweep
// climbs until the ceiling knee. Kept conservative for laptop runs.
var defaultLadder = []int{1000, 2000, 5000, 10000, 20000, 40000}

// liveStack holds the running compose stack and the resolved gateway
// coordinates the bench needs.
type liveStack struct {
	compose     compose.ComposeStack
	gatewayURL  string
	containerID string
}

// startStack brings up the compose stack and resolves the primary
// gateway-server's host URL and container id.
func startStack(ctx context.Context) (*liveStack, error) {
	c, err := compose.NewDockerCompose(composeFile)
	if err != nil {
		return nil, fmt.Errorf("compose new: %w", err)
	}
	upCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if err := c.Up(upCtx, compose.Wait(true)); err != nil {
		return nil, fmt.Errorf("compose up: %w", err)
	}

	gw, err := c.ServiceContainer(ctx, "gateway-server")
	if err != nil {
		return nil, fmt.Errorf("resolve gateway-server: %w", err)
	}
	host, err := gw.Host(ctx)
	if err != nil {
		return nil, fmt.Errorf("gateway host: %w", err)
	}
	port, err := gw.MappedPort(ctx, "8080/tcp")
	if err != nil {
		return nil, fmt.Errorf("gateway port: %w", err)
	}

	stack := &liveStack{
		compose:     c,
		gatewayURL:  fmt.Sprintf("http://%s:%s", host, port.Port()),
		containerID: gw.GetContainerID(),
	}
	if err := waitForRoute(ctx, stack.gatewayURL+"/users/alice"); err != nil {
		stack.stop(ctx)
		return nil, err
	}
	return stack, nil
}

// waitForRoute polls a known route until the gateway serves it with 200.
// compose Wait(true) only proves containers are healthy; the gateway is
// not servable until its routing snapshot lands and example-app has
// registered its handlers in the KV registry — same readiness gap the
// e2e harness closes with WaitReady.
func waitForRoute(ctx context.Context, url string) error {
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return fmt.Errorf("build readiness probe: %w", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("gateway never served %s within 2m", url)
}

// stop tears the stack down, removing orphans and volumes.
func (s *liveStack) stop(ctx context.Context) {
	if s == nil || s.compose == nil {
		return
	}
	_ = s.compose.Down(ctx, compose.RemoveOrphans(true), compose.RemoveVolumes(true))
}

// runLadder drives a scenario up the rate ladder, one rung at a time,
// returning a StepResult per rung. It stops early once a rung fails to
// sustain (so we do not pointlessly hammer past the knee).
func runLadder(sc Scenario, ladder []int) []StepResult {
	steps := make([]StepResult, 0, len(ladder))
	for _, freq := range ladder {
		rate := vegeta.Rate{Freq: freq, Per: time.Second}
		atk := vegeta.NewAttacker()
		var m vegeta.Metrics
		for res := range atk.Attack(sc.Targeter, rate, stepDuration, sc.Name) {
			m.Add(res)
		}
		m.Close()
		atk.Stop()

		step := Summarize(&m, freq)
		steps = append(steps, step)
		if !sustained(step) {
			break
		}
	}
	return steps
}
