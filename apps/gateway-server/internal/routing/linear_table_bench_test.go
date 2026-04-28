package routing

import (
	"fmt"
	"testing"
)

// BenchmarkLinearTable_Lookup_1000Routes measures routing Lookup
// cost under a realistic worst case: 1000 registered routes, all
// parameterized, with the target route sitting in the middle of the
// insertion order so neither the best nor the worst case skews the
// result. The linear scan is O(n) by construction; this baseline
// quantifies the slope so a future trie-backed implementation has
// a concrete number to beat.
func BenchmarkLinearTable_Lookup_1000Routes(b *testing.B) {
	table := newLinearTable()
	for i := 0; i < 1000; i++ {
		table.add(Route{
			Subject:      fmt.Sprintf("svc.cmd.resource%d.get", i),
			Method:       "GET",
			PathTemplate: fmt.Sprintf("/resource%d/:id", i),
		})
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _, _ = table.Lookup("GET", "/resource500/abc")
	}
}
