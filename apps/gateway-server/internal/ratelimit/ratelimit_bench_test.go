package ratelimit

import (
	"context"
	"testing"
	"time"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/registry"
)

// BenchmarkMemoryStoreAllow_HotKey measures the steady-state Allow
// path for a key that already exists in the map — the dominant case
// on a production route with a stable client population. Guards the
// zero-allocation contract on the hot path.
func BenchmarkMemoryStoreAllow_HotKey(b *testing.B) {
	s := NewMemoryStore(time.Hour)
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	// Prime the entry so every benchmark iteration takes the hit path.
	_, _ = s.Allow(ctx, "bench-key", 1_000_000, 1_000_000)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = s.Allow(ctx, "bench-key", 1_000_000, 1_000_000)
	}
}

// BenchmarkBuildBucketKey measures the per-request key composition
// with a constant (method, pathTemplate) pair — the shape every
// rate-limited request produces once a route table is live.
func BenchmarkBuildBucketKey(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = BuildBucketKey("GET", "/users/:id/orders", "203.0.113.7")
	}
}

// BenchmarkBuildHeaders measures header-map construction on the
// allow path with a fully populated Decision.
func BenchmarkBuildHeaders(b *testing.B) {
	rl := &registry.RateLimitMeta{RPS: 100, Burst: 200}
	d := Decision{Allowed: true, Remaining: 87, ResetAt: time.Unix(1_735_837_293, 0)}

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = BuildHeaders(rl, d)
	}
}
