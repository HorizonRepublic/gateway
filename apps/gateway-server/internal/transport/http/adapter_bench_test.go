package http

import (
	"testing"

	"github.com/cloudwego/hertz/pkg/common/ut"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/proxy"
)

// Sinks defeat dead-code elimination in the benchmark loops.
var (
	benchInput *proxy.ServeInput
	benchQuery map[string]proxy.QueryValue
)

// BenchmarkBuildServeInput_TypicalHeaders measures the full request
// translation with the twelve canonical-cased headers a browser-
// originated API request typically carries and no query string. The
// per-iteration cost includes the ULID request-id generation, which is
// inherent to the translation and identical across implementations.
func BenchmarkBuildServeInput_TypicalHeaders(b *testing.B) {
	ctx := ut.CreateUtRequestContext("GET", "https://gateway.test/users/42", nil,
		ut.Header{Key: "Accept", Value: "application/json"},
		ut.Header{Key: "Accept-Encoding", Value: "gzip, br"},
		ut.Header{Key: "Accept-Language", Value: "en-US,en;q=0.9"},
		ut.Header{Key: "Authorization", Value: "Bearer 0123456789abcdef"},
		ut.Header{Key: "Cache-Control", Value: "no-cache"},
		ut.Header{Key: "Content-Type", Value: "application/json"},
		ut.Header{Key: "Cookie", Value: "sid=abc123; theme=dark"},
		ut.Header{Key: "Origin", Value: "https://app.example.com"},
		ut.Header{Key: "Referer", Value: "https://app.example.com/dashboard"},
		ut.Header{Key: "User-Agent", Value: "Mozilla/5.0 (Macintosh) Gecko/20100101"},
		ut.Header{Key: "X-Forwarded-For", Value: "203.0.113.7"},
		ut.Header{Key: "Traceparent", Value: "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"},
	)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		benchInput = buildServeInput(ctx)
	}
}

// BenchmarkCollectQueryValues covers the three per-request query
// shapes: no query string at all (the GET-heavy common case), a few
// scalar keys, and a repeated key that must surface as the Multi
// variant.
func BenchmarkCollectQueryValues(b *testing.B) {
	cases := []struct {
		name string
		url  string
	}{
		{"empty", "https://gateway.test/x"},
		{"three-scalar-keys", "https://gateway.test/x?a=1&b=2&c=3"},
		{"repeated-key", "https://gateway.test/x?tag=a&tag=b&tag=c"},
	}

	for _, c := range cases {
		b.Run(c.name, func(b *testing.B) {
			ctx := ut.CreateUtRequestContext("GET", c.url, nil)
			b.ResetTimer()
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				benchQuery = collectQueryValues(ctx)
			}
		})
	}
}
