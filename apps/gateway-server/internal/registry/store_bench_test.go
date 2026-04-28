package registry

import "testing"

// BenchmarkStore_Get measures the cost of a snapshot read. The
// store is warmed with a single-entry snapshot so the returned
// pointer is stable across iterations; Get is expected to compile
// to an atomic.Pointer.Load plus a cast and nothing else.
func BenchmarkStore_Get(b *testing.B) {
	s := NewStore()
	s.Replace(&Snapshot{Entries: map[string]HandlerEntry{
		"svc.cmd.a": {HTTP: &HTTPMeta{Method: "GET", Path: "/a"}},
	}})

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = s.Get()
	}
}
