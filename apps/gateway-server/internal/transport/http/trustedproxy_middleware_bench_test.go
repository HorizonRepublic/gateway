package http

import (
	"context"
	"net"
	"testing"

	"github.com/cloudwego/hertz/pkg/common/ut"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/trustedproxy"
)

// BenchmarkTrustedProxyMiddleware_SingleXFFLine measures the
// per-request cost of the trusted-proxy middleware on its dominant
// path: one X-Forwarded-For field line carrying a short chain. The
// middleware runs on every public request, so allocations here are
// multiplied by the full request rate.
func BenchmarkTrustedProxyMiddleware_SingleXFFLine(b *testing.B) {
	trusted, err := trustedproxy.ParseCIDRList("private")
	if err != nil {
		b.Fatal(err)
	}
	middleware := newTrustedProxyMiddleware(trusted, xForwardedForHeader)

	ctx := ut.CreateUtRequestContext("GET", "https://gateway.test/x", nil,
		ut.Header{Key: "X-Forwarded-For", Value: "1.2.3.4, 10.0.0.5"},
	)
	attachRemote(ctx, &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 54321})

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		middleware(context.Background(), ctx)
	}
}

// BenchmarkTrustedProxyMiddleware_RepeatedXFFLines measures the
// multi-line join path so a regression in the rare-path cost is
// visible next to the dominant single-line benchmark.
func BenchmarkTrustedProxyMiddleware_RepeatedXFFLines(b *testing.B) {
	trusted, err := trustedproxy.ParseCIDRList("private")
	if err != nil {
		b.Fatal(err)
	}
	middleware := newTrustedProxyMiddleware(trusted, xForwardedForHeader)

	ctx := ut.CreateUtRequestContext("GET", "https://gateway.test/x", nil,
		ut.Header{Key: "X-Forwarded-For", Value: "6.6.6.6"},
		ut.Header{Key: "X-Forwarded-For", Value: "203.0.113.9"},
	)
	attachRemote(ctx, &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 54321})

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		middleware(context.Background(), ctx)
	}
}
