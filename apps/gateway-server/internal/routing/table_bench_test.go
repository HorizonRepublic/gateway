package routing

import (
	"fmt"
	"testing"
)

// Sinks defeat dead-code elimination so the compiler cannot optimize
// the Lookup call away once the benchmark loop body is inlined.
var (
	benchRoute  Route
	benchParams map[string]string
	benchOK     bool
)

// buildBenchTable registers n routes alternating between a static
// template ("/resourceN/info") and a parameterized one
// ("/resourceN/:id") so every benchmark case exercises the same table
// shape a mixed real-world registry produces.
func buildBenchTable(n int) Table {
	routes := make([]Route, 0, n)
	for i := 0; i < n; i++ {
		template := fmt.Sprintf("/resource%d/:id", i)
		if i%2 == 0 {
			template = fmt.Sprintf("/resource%d/info", i)
		}
		routes = append(routes, Route{
			Subject:      fmt.Sprintf("svc.cmd.resource%d.get", i),
			Method:       "GET",
			PathTemplate: template,
		})
	}

	return BuildTableFromRoutes(routes)
}

// BenchmarkTableLookup measures Lookup across registry sizes of 10,
// 100, and 1000 routes for the three per-request outcomes: a static
// template hit, a parameterized template hit, and a full miss. The
// mid-table target indices keep the measurement representative of
// neither the best nor the worst insertion position.
func BenchmarkTableLookup(b *testing.B) {
	for _, n := range []int{10, 100, 1000} {
		table := buildBenchTable(n)

		staticIdx := n / 2
		if staticIdx%2 == 1 {
			staticIdx++
		}
		staticPath := fmt.Sprintf("/resource%d/info", staticIdx)
		paramPath := fmt.Sprintf("/resource%d/abc", staticIdx+1)

		b.Run(fmt.Sprintf("routes=%d/static-hit", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				benchRoute, benchParams, benchOK = table.Lookup("GET", staticPath)
			}
		})

		b.Run(fmt.Sprintf("routes=%d/param-hit", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				benchRoute, benchParams, benchOK = table.Lookup("GET", paramPath)
			}
		})

		b.Run(fmt.Sprintf("routes=%d/miss", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				benchRoute, benchParams, benchOK = table.Lookup("GET", "/absent/path")
			}
		})
	}
}
