package ratelimit

import "testing"

// BenchmarkBuildBucketKey measures the per-request cost of composing
// the bucket key when the whole key (method + path-template hash +
// resolved-key hash) is computed from scratch — the pre-optimisation
// hot-path shape where the static route half is re-hashed every call.
func BenchmarkBuildBucketKey(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = BuildBucketKey("GET", "/users/:id/orders/:orderId", "203.0.113.7")
	}
}
