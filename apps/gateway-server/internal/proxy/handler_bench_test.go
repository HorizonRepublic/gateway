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

// benchAllowStore is an always-allow ratelimit.Store that records
// nothing, so Handler.Handle benchmarks measure only the handler's
// own per-request cost, not fixture bookkeeping.
type benchAllowStore struct{}

func (benchAllowStore) Allow(context.Context, string, int, int) (ratelimit.Decision, error) {
	return ratelimit.Decision{Allowed: true, Remaining: 10, ResetAt: time.Unix(1_700_000_000, 0)}, nil
}

func (benchAllowStore) FlushPrefix(context.Context, string) error { return nil }
func (benchAllowStore) Close() error                              { return nil }
func (benchAllowStore) Counters() map[string]int64                { return nil }

// BenchmarkHandler_Handle_PublicRoute measures the full happy-path
// request lifecycle for a public route (no auth, no rate limit)
// against an in-process fake NATS requester. This is the hottest
// production path; per-request allocations here multiply directly
// by gateway RPS.
func BenchmarkHandler_Handle_PublicRoute(b *testing.B) {
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
		RequestID:   "01HXY0000000000000000000",
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

// BenchmarkHandler_Handle_AuthRateLimitedIPKey measures a protected,
// rate-limited route whose bucket key is the client IP. Pins the
// lazy-claims contract: keyBy chains without a `user:` strategy must
// not pay a claims JSON parse per request.
func BenchmarkHandler_Handle_AuthRateLimitedIPKey(b *testing.B) {
	routeSubject := "svc.cmd.users.me"
	verifierSubject := "svc.cmd.auth.verifier.jwt"
	table := &fakeTable{routes: map[string]routing.Route{
		"GET /users/me": {
			Subject:      routeSubject,
			Method:       "GET",
			PathTemplate: "/users/me",
			Auth:         &routing.RouteAuth{VerifierSubject: verifierSubject},
			RateLimit:    &registry.RateLimitMeta{RPS: 1000, Burst: 2000, KeyBy: []string{"ip"}},
		},
	}}

	nats := benchSubjectRequester{
		verifierSubject: []byte(`{"status":200,"headers":{},"body":{"sub":"u1","tenant":"t1","scope":["read","write"],"roles":["admin","editor"],"iat":1700000000,"exp":1700003600}}`),
		routeSubject:    []byte(`{"status":200,"headers":{},"body":{"ok":true}}`),
	}

	router := ratelimit.NewRouter(ratelimit.FailPolicyOpen.Resolve(), zerolog.Nop())
	if err := router.EnsureBackend("memory", func() (ratelimit.Store, error) {
		return benchAllowStore{}, nil
	}); err != nil {
		b.Fatal(err)
	}

	h := NewHandler(HandlerConfig{
		Table:            func() routing.Table { return table },
		Nats:             nats,
		Encoder:          NewDefaultEncoder(),
		Decoder:          NewDefaultDecoder(),
		Timeout:          30 * time.Second,
		Logger:           zerolog.Nop(),
		RateLimiter:      router,
		RateLimitTimeout: 50 * time.Millisecond,
	})
	in := &ServeInput{
		Method:     "GET",
		Path:       "/users/me",
		Query:      map[string]QueryValue{},
		Headers:    map[string]string{"authorization": "Bearer xyz"},
		RequestID:  "01HXY0000000000000000000",
		RemoteAddr: "10.0.0.1",
	}
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = h.Handle(ctx, in)
	}
}

// benchRequester is a minimal NatsRequester that returns one canned
// reply without recording calls — fakeRequester's per-call append
// would dominate the allocation profile at benchmark iteration counts.
type benchRequester struct {
	reply []byte
}

func (r *benchRequester) Request(context.Context, string, []byte, time.Duration) ([]byte, error) {
	return r.reply, nil
}

// benchSubjectRequester maps subject → canned reply without recording
// calls, for benchmarks that exercise the verifier + route two-hop
// flow.
type benchSubjectRequester map[string][]byte

func (r benchSubjectRequester) Request(_ context.Context, subject string, _ []byte, _ time.Duration) ([]byte, error) {
	return r[subject], nil
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
