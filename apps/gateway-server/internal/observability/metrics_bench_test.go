package observability

import "testing"

// BenchmarkObserveHTTPRequest_SteadyState pins the hot-path claim the
// proxy handler relies on: once a label tuple is cached, recording a
// request is allocation-free (one RLock'd map read plus two atomic
// bumps). A regression here multiplies across every request at
// ~100k RPS.
func BenchmarkObserveHTTPRequest_SteadyState(b *testing.B) {
	m := NewMetrics()
	// Warm the cache so the loop measures the steady state.
	m.ObserveHTTPRequest("GET", "/users/:id", 200, 0.001)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.ObserveHTTPRequest("GET", "/users/:id", 200, 0.001)
	}
}
