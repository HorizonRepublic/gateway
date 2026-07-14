package ratelimit

import (
	"testing"
	"time"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/registry"
)

// BenchmarkBuildHeaders_Stamp measures the full per-request header
// flow the proxy handler executes on the rate-limit happy path: render
// the X-RateLimit-* values from a Decision and merge them into a
// response header map (map[string][]string), mirroring the merge loop
// in the proxy handler.
func BenchmarkBuildHeaders_Stamp(b *testing.B) {
	rl := &registry.RateLimitMeta{RPS: 100, Burst: 200}
	d := Decision{Allowed: true, Remaining: 87, ResetAt: time.Unix(1_735_837_293, 0)}

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		out := make(map[string][]string, 4)
		h := BuildHeaders(rl, d)
		for k, v := range h {
			if _, exists := out[k]; !exists {
				out[k] = []string{v}
			}
		}
	}
}
