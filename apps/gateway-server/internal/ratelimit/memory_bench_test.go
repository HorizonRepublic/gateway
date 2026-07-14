package ratelimit

import (
	"context"
	"testing"
	"time"
)

// BenchmarkMemoryStoreAllow_Hit measures the steady-state Allow path
// for a key that already exists in the store — the dominant case for
// every active bucket in production. The rate-limit gate sits on the
// request hot path, so this benchmark guards the allocation budget of
// the per-request check.
func BenchmarkMemoryStoreAllow_Hit(b *testing.B) {
	s := NewMemoryStore(time.Hour)
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	// Prime the entry so every benchmark iteration takes the hit path.
	if _, err := s.Allow(ctx, "bench-key", 1_000_000, 1_000_000); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Allow(ctx, "bench-key", 1_000_000, 1_000_000); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkMemoryStoreAllow_HitWithCap mirrors the production wiring
// (RATELIMIT_MEMORY_MAX_ENTRIES > 0) where the cardinality-cap probe
// runs before the entry lookup.
func BenchmarkMemoryStoreAllow_HitWithCap(b *testing.B) {
	s := NewMemoryStoreWithCap(time.Hour, 1_000_000)
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	if _, err := s.Allow(ctx, "bench-key", 1_000_000, 1_000_000); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Allow(ctx, "bench-key", 1_000_000, 1_000_000); err != nil {
			b.Fatal(err)
		}
	}
}
