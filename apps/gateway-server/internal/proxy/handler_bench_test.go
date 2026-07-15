package proxy

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/ratelimit"
	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/registry"
	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/routing"
)

// benchRequester is a minimal NatsRequester for handler benchmarks.
// Unlike fakeRequester it records nothing, so the benchmark numbers
// reflect the handler's own per-request cost instead of the fixture's
// append-and-copy bookkeeping.
type benchRequester struct {
	replies map[string][]byte
	reply   []byte
}

func (b *benchRequester) Request(
	_ context.Context,
	subject string,
	_ []byte,
	_ time.Duration,
) ([]byte, error) {
	if r, ok := b.replies[subject]; ok {
		return r, nil
	}

	return b.reply, nil
}

// allowAllStore is a ratelimit.Store that admits every request with a
// populated Decision and never errors. Keeps the benchmark on the
// handler's happy path.
type allowAllStore struct{}

func (allowAllStore) Allow(_ context.Context, _ string, _, _ int) (ratelimit.Decision, error) {
	return ratelimit.Decision{Allowed: true, Remaining: 1, ResetAt: time.Unix(1_700_000_000, 0)}, nil
}

func (allowAllStore) FlushPrefix(_ context.Context, _ string) error { return nil }

func (allowAllStore) Close() error { return nil }

func (allowAllStore) Counters() map[string]int64 { return nil }

func benchRouter(b *testing.B) *ratelimit.Router {
	b.Helper()
	router := ratelimit.NewRouter(ratelimit.FailPolicyOpen.Resolve(), zerolog.Nop())
	if err := router.EnsureBackend("memory", func() (ratelimit.Store, error) {
		return allowAllStore{}, nil
	}); err != nil {
		b.Fatalf("ensure backend: %v", err)
	}

	return router
}

// BenchmarkHandler_PublicRoute measures the full Handle cycle for a
// public route (no auth, no rate limit) with a canned 200 reply.
// Primary regression target: per-request logger construction — the
// request-scoped zerolog child logger must not be built on the happy
// path, where nothing is ever emitted.
func BenchmarkHandler_PublicRoute(b *testing.B) {
	table := &fakeTable{routes: map[string]routing.Route{
		"GET /users": {Subject: "svc.cmd.users.list", PathTemplate: "/users", Method: "GET"},
	}}
	h := NewHandler(HandlerConfig{
		Table:   func() routing.Table { return table },
		Nats:    &benchRequester{reply: []byte(`{"status":200,"headers":{},"body":{"ok":true}}`)},
		Encoder: NewDefaultEncoder(),
		Decoder: NewDefaultDecoder(),
		Timeout: 30 * time.Second,
		Logger:  zerolog.Nop(),
	})
	in := &ServeInput{
		Method:      "GET",
		Path:        "/users",
		Query:       map[string]QueryValue{},
		Headers:     map[string]string{"accept": "application/json"},
		RequestID:   "bench-req-1",
		Traceparent: "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
		RemoteAddr:  "10.0.0.1",
	}
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		_ = h.Handle(ctx, in)
	}
}

// BenchmarkHandler_AuthRateLimitedIPKeyed measures a protected route
// with an IP-keyed rate limit and a ~1 KiB verifier claims payload.
// Primary regression target: the claims JSON parse — an IP-keyed
// keyBy chain never reads claims, so the per-request unmarshal into
// map[string]any must not run.
func BenchmarkHandler_AuthRateLimitedIPKeyed(b *testing.B) {
	verifierSubject := "auth-svc.cmd.auth.verifier.jwt"
	routeSubject := "users-svc.cmd.users.me"

	claims := `{"sub":"user-123","scope":["read","write","admin"],"pad":"` +
		strings.Repeat("x", 900) + `"}`
	route := routing.Route{
		Subject:      routeSubject,
		Method:       "GET",
		PathTemplate: "/users/me",
		Auth:         &routing.RouteAuth{VerifierSubject: verifierSubject},
		RateLimit:    &registry.RateLimitMeta{RPS: 1000, Burst: 2000, KeyBy: []string{"ip"}},
	}

	h := NewHandler(HandlerConfig{
		Table: func() routing.Table { return stubTable(route) },
		Nats: &benchRequester{replies: map[string][]byte{
			verifierSubject: []byte(`{"status":200,"headers":{},"body":` + claims + `}`),
			routeSubject:    []byte(`{"status":200,"headers":{},"body":{"ok":true}}`),
		}},
		Encoder:          NewDefaultEncoder(),
		Decoder:          NewDefaultDecoder(),
		Timeout:          30 * time.Second,
		Logger:           zerolog.Nop(),
		RateLimiter:      benchRouter(b),
		RateLimitTimeout: 50 * time.Millisecond,
	})
	in := &ServeInput{
		Method:      "GET",
		Path:        "/users/me",
		Query:       map[string]QueryValue{},
		Headers:     map[string]string{"authorization": "Bearer tok"},
		RequestID:   "bench-req-2",
		Traceparent: "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
		RemoteAddr:  "10.0.0.1",
	}
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		_ = h.Handle(ctx, in)
	}
}

// BenchmarkPreviewClaimsForLog_4KB pins the slice-before-string
// optimisation in previewClaimsForLog. The pre-fix shape allocated
// proportional to input size (a 4 KiB claim → ~4 KiB string), which
// the redaction step then operated on after truncating to 256 bytes.
// The fix slices the byte view to the preview cap first, so the
// string allocation is bounded by maxPreview regardless of the input.
//
// Expected: bytes-per-op ≈ maxPreview (256) regardless of input size.
func BenchmarkPreviewClaimsForLog_4KB(b *testing.B) {
	payload := json.RawMessage([]byte(strings.Repeat("a", 4096)))

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		_ = previewClaimsForLog(payload)
	}
}

func BenchmarkPreviewClaimsForLog_Small(b *testing.B) {
	payload := json.RawMessage([]byte(`{"sub":"u1","scope":["read","write"]}`))

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		_ = previewClaimsForLog(payload)
	}
}
