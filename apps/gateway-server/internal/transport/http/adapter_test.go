package http

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/proxy"
	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/routing"
)

// ---------- buildServeInput tests ----------

func TestBuildServeInput_ExtractsMethodAndPath(t *testing.T) {
	ctx := ut.CreateUtRequestContext("GET", "https://gateway.test/users/42", nil)

	input := buildServeInput(ctx)

	assert.Equal(t, "GET", input.Method)
	assert.Equal(t, "/users/42", input.Path)
}

func TestBuildServeInput_LowercasesHeaderKeys(t *testing.T) {
	ctx := ut.CreateUtRequestContext("GET", "https://gateway.test/x", nil,
		ut.Header{Key: "Authorization", Value: "Bearer token"},
		ut.Header{Key: "X-Custom", Value: "value"},
	)

	input := buildServeInput(ctx)

	assert.Equal(t, "Bearer token", input.Headers["authorization"])
	assert.Equal(t, "value", input.Headers["x-custom"])
	assert.NotContains(t, input.Headers, "Authorization")
}

func TestBuildServeInput_StripsHopByHopHeaders(t *testing.T) {
	ctx := ut.CreateUtRequestContext("GET", "https://gateway.test/x", nil,
		ut.Header{Key: "Connection", Value: "keep-alive"},
		ut.Header{Key: "Transfer-Encoding", Value: "chunked"},
		ut.Header{Key: "Upgrade", Value: "h2c"},
		ut.Header{Key: "Trailer", Value: "Expires"},
		ut.Header{Key: "Trailers", Value: "Expires"},
	)

	input := buildServeInput(ctx)

	assert.NotContains(t, input.Headers, "connection")
	assert.NotContains(t, input.Headers, "transfer-encoding")
	assert.NotContains(t, input.Headers, "upgrade")
	assert.NotContains(t, input.Headers, "trailer",
		"the canonical singular `Trailer` header is hop-by-hop and must be stripped")
	assert.NotContains(t, input.Headers, "trailers",
		"the legacy plural `Trailers` spelling is also hop-by-hop")
}

// TestBuildServeInput_MergesRepeatedHeaderValues pins the RFC 7230
// §3.2.2 combining contract: a request that carries the same header
// twice must surface to upstream handlers as a single ", "-joined
// value rather than whichever value happened to arrive last. The
// previous implementation overwrote on every VisitAll callback, so
// multi-value headers silently lost observations.
//
// Cookie is a special case — Hertz itself merges repeated Cookie lines
// with "; " per RFC 6265 §5.4 before VisitAll fires — so this test
// exercises generic multi-value headers (Accept-Encoding and a custom
// X-Forward-Chain) that Hertz surfaces as separate callbacks.
func TestBuildServeInput_MergesRepeatedHeaderValues(t *testing.T) {
	ctx := ut.CreateUtRequestContext("GET", "https://gateway.test/x", nil,
		ut.Header{Key: "Accept-Encoding", Value: "gzip"},
		ut.Header{Key: "Accept-Encoding", Value: "br"},
		ut.Header{Key: "X-Forward-Chain", Value: "edge-1"},
		ut.Header{Key: "X-Forward-Chain", Value: "edge-2"},
		ut.Header{Key: "X-Forward-Chain", Value: "edge-3"},
	)

	input := buildServeInput(ctx)

	assert.Equal(t, "gzip, br", input.Headers["accept-encoding"],
		"repeated Accept-Encoding headers must merge per RFC 7230 §3.2.2")
	assert.Equal(t, "edge-1, edge-2, edge-3", input.Headers["x-forward-chain"],
		"three or more observations must all survive the merge in VisitAll order")
}

// TestBuildServeInput_CookieHeaderArrivesPreMerged documents the
// Hertz behaviour around Cookie: RFC 6265 §5.4 already collapses
// repeated Cookie lines with "; " before VisitAll fires, so the
// adapter sees a single callback and the merge path is a no-op.
// Guard against a future Hertz behavioural drift that would surface
// multi-callback cookies and force a different join separator.
func TestBuildServeInput_CookieHeaderArrivesPreMerged(t *testing.T) {
	ctx := ut.CreateUtRequestContext("GET", "https://gateway.test/x", nil,
		ut.Header{Key: "Cookie", Value: "a=1"},
		ut.Header{Key: "Cookie", Value: "b=2"},
	)

	input := buildServeInput(ctx)

	assert.Equal(t, "a=1; b=2", input.Headers["cookie"],
		"Hertz is expected to join Cookie lines with \"; \" before VisitAll fires")
}

// TestHeaderJoinSeparator pins the per-header delimiter contract:
// Cookie joins with "; " (RFC 6265 §5.4), every other repeated header
// joins with ", " (RFC 7230 §3.2.2). Any future Hertz behavioural
// drift that surfaces multiple Cookie callbacks would land in the
// merge path and rely on this helper picking the right separator.
func TestHeaderJoinSeparator(t *testing.T) {
	cases := []struct {
		key      string
		expected string
	}{
		{"cookie", "; "},
		{"accept-encoding", ", "},
		{"x-forward-chain", ", "},
		{"set-cookie", ", "}, // request-side header, response-side never reaches this path.
	}

	for _, c := range cases {
		t.Run(c.key, func(t *testing.T) {
			assert.Equal(t, c.expected, headerJoinSeparator(c.key))
		})
	}
}

// TestLowerHeaderKey pins the single-alloc lowercase helper to the
// exact output string(bytes.ToLower(key)) produced for ASCII inputs,
// including the invalid-but-possible non-ASCII bytes an adversarial
// client can put on the wire (those pass through untouched — RFC 9110
// §5.1 field names are ASCII tokens, so they match nothing downstream
// either way).
func TestLowerHeaderKey(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"canonical cased", "Content-Type", "content-type"},
		{"all uppercase", "AUTHORIZATION", "authorization"},
		{"already lowercase", "x-request-id", "x-request-id"},
		{"empty", "", ""},
		{"digits and symbols untouched", "X-Retry-2!", "x-retry-2!"},
		{"non-ascii bytes pass through", "X-\xc3\x89tag", "x-\xc3\x89tag"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, lowerHeaderKey([]byte(c.in)))
		})
	}
}

// TestLowerHeaderKey_AllocationProfile pins the allocation contract
// the helper exists for: exactly one allocation per call (the
// resulting string) regardless of input casing. The generic
// string(bytes.ToLower(key)) idiom it replaced costs two.
func TestLowerHeaderKey_AllocationProfile(t *testing.T) {
	var sink string
	for _, in := range [][]byte{[]byte("Content-Type"), []byte("x-request-id")} {
		key := in
		allocs := testing.AllocsPerRun(100, func() {
			sink = lowerHeaderKey(key)
		})
		assert.Equal(t, 1.0, allocs, "input %q", key)
	}
	_ = sink
}

func TestBuildServeInput_CollectsSingleValueQuery(t *testing.T) {
	ctx := ut.CreateUtRequestContext("GET", "https://gateway.test/x?include=profile", nil)

	input := buildServeInput(ctx)

	require.Contains(t, input.Query, "include")
	assert.Equal(t, proxy.NewQueryValueString("profile"), input.Query["include"])
}

func TestBuildServeInput_CollectsRepeatedQueryKey(t *testing.T) {
	ctx := ut.CreateUtRequestContext("GET", "https://gateway.test/x?tag=a&tag=b", nil)

	input := buildServeInput(ctx)

	require.Contains(t, input.Query, "tag")
	assert.Equal(t, proxy.NewQueryValueStrings([]string{"a", "b"}), input.Query["tag"])
}

// TestBuildServeInput_NoQueryStringYieldsNilQuery pins the
// zero-alloc contract for the GET-heavy common case: a request
// without a query string must surface Query as a nil map, not an
// empty one. Downstream the envelope encoder ranges over the map
// (nil-safe) into its pooled non-nil Query map, so the wire shape
// stays `"query":{}` — TestAppendEnvelopeJSON_NilQueryEncodesAsEmptyObject
// in the proxy package pins that half of the contract.
func TestBuildServeInput_NoQueryStringYieldsNilQuery(t *testing.T) {
	ctx := ut.CreateUtRequestContext("GET", "https://gateway.test/x", nil)

	input := buildServeInput(ctx)

	assert.Nil(t, input.Query,
		"a query-less request must not allocate an empty Query map")
}

func TestBuildServeInput_CapturesTraceparentHeader(t *testing.T) {
	traceValue := "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	ctx := ut.CreateUtRequestContext("GET", "https://gateway.test/x", nil,
		ut.Header{Key: "Traceparent", Value: traceValue},
	)

	input := buildServeInput(ctx)

	assert.Equal(t, traceValue, input.Traceparent)
	assert.Equal(t, traceValue, input.Headers["traceparent"])
}

func TestBuildServeInput_GeneratesRequestIDWhenAbsent(t *testing.T) {
	ctx := ut.CreateUtRequestContext("GET", "https://gateway.test/x", nil)

	input := buildServeInput(ctx)

	assert.Len(t, input.RequestID, 26, "monotonic ULID is exactly 26 base32 chars")
}

func TestBuildServeInput_StampsXRequestIdResponseHeader(t *testing.T) {
	ctx := ut.CreateUtRequestContext("GET", "https://gateway.test/x", nil)

	input := buildServeInput(ctx)

	assert.Equal(t, input.RequestID, string(ctx.Response.Header.Peek("X-Request-Id")))
}

// ---------- writeServeResult tests ----------

func TestWriteServeResult_SetsStatusHeadersAndBody(t *testing.T) {
	ctx := ut.CreateUtRequestContext("GET", "https://gateway.test/x", nil)
	result := &proxy.ServeResult{
		Status: 201,
		Headers: map[string][]string{
			"x-custom":     {"yes"},
			"x-request-id": {"forged-by-upstream"},
		},
		Body: []byte(`{"ok":true}`),
	}

	writeServeResult(ctx, result, "adapter-truth")

	assert.Equal(t, 201, ctx.Response.StatusCode())
	assert.Equal(t, "yes", string(ctx.Response.Header.Peek("x-custom")))
	// The adapter's requestID always wins — a compromised upstream
	// cannot forge correlator ids by stuffing X-Request-Id into the
	// reply.
	assert.Equal(t, "adapter-truth", string(ctx.Response.Header.Peek("x-request-id")))
	assert.Equal(t, `{"ok":true}`, string(ctx.Response.Body()))
}

func TestWriteServeResult_ForcesApplicationJSONContentType(t *testing.T) {
	// Upstream services MUST NOT be able to change the wire content-
	// type. The gateway overrides whatever they sent to application/
	// json as the last write before status, which is the invariant
	// downstream clients rely on to parse the body without sniffing.
	ctx := ut.CreateUtRequestContext("GET", "https://gateway.test/x", nil)
	result := &proxy.ServeResult{
		Status:  200,
		Headers: map[string][]string{"content-type": {"text/plain"}},
		Body:    []byte(`"ok"`),
	}

	writeServeResult(ctx, result, "test-req-id")

	assert.Contains(t, string(ctx.Response.Header.Peek("Content-Type")), "application/json")
}

// TestWriteServeResult_EmitsMultipleSetCookieLines is the load-bearing
// test for Phase E.1: a handler that returns two Set-Cookie values in
// the envelope MUST land on the wire as two distinct header lines so
// RFC 6265 §3 parsers (every browser, curl -v, Node's http module)
// recognize both cookies. If this assertion ever breaks it means the
// adapter is joining multi-value headers, which would silently drop
// the second cookie from the client-visible jar.
func TestWriteServeResult_EmitsMultipleSetCookieLines(t *testing.T) {
	ctx := ut.CreateUtRequestContext("GET", "https://gateway.test/x", nil)
	result := &proxy.ServeResult{
		Status: 200,
		Headers: map[string][]string{
			"set-cookie": {
				"sid=abc; Path=/; HttpOnly",
				"theme=dark; Path=/",
			},
		},
		Body: []byte(`{}`),
	}

	writeServeResult(ctx, result, "test-req-id")

	var lines []string
	ctx.Response.Header.VisitAllCookie(func(_, value []byte) {
		lines = append(lines, string(value))
	})

	assert.ElementsMatch(t, []string{
		"sid=abc; Path=/; HttpOnly",
		"theme=dark; Path=/",
	}, lines)
}

// TestWriteServeResult_DeduplicatesXRequestIdAcrossBuildAndWrite
// pins the regression fix for a double X-Request-Id header on
// every response. `buildServeInput` stamps the correlator on
// the Hertz response up front so panic-recovery middleware
// still sees it; `writeServeResult` later re-emits it from the
// proxy's ServeResult headers. Without the Del in
// writeServeResult, both writes landed and every HTTP response
// carried two identical `X-Request-Id:` header lines. The
// assertion walks every header line (not just Peek, which
// returns the first value) so a future regression producing
// duplicates is caught loudly instead of sneaking past
// single-value introspection.
func TestWriteServeResult_DeduplicatesXRequestIdAcrossBuildAndWrite(t *testing.T) {
	ctx := ut.CreateUtRequestContext("GET", "https://gateway.test/x", nil)

	// Simulate the real request flow: buildServeInput stamps
	// X-Request-Id on Response.Header early, then writeServeResult
	// runs with a ServeResult whose Headers include x-request-id
	// (as mergeHeaders in the proxy layer produces on every
	// request).
	input := buildServeInput(ctx)
	result := &proxy.ServeResult{
		Status: 200,
		Headers: map[string][]string{
			"x-request-id": {input.RequestID},
		},
		Body: []byte(`{"ok":true}`),
	}

	writeServeResult(ctx, result, input.RequestID)

	var xRequestIDLines []string
	ctx.Response.Header.VisitAll(func(key, value []byte) {
		if bytes.EqualFold(key, []byte("X-Request-Id")) {
			xRequestIDLines = append(xRequestIDLines, string(value))
		}
	})

	// Exactly one X-Request-Id header, with the generated ID.
	assert.Len(t, xRequestIDLines, 1, "expected exactly one X-Request-Id header, got %v", xRequestIDLines)
	assert.Equal(t, input.RequestID, xRequestIDLines[0])
}

// ---------- full adapter integration tests ----------

// fakeTable is a routing.Table double whose behaviour is determined
// by the hit flag and the canned route. Lives in the test file so it
// is not exported beyond the http package.
type fakeTable struct {
	route routing.Route
	hit   bool
}

func (f *fakeTable) Lookup(_, _ string) (routing.Route, map[string]string, bool) {
	if !f.hit {
		return routing.Route{}, nil, false
	}
	return f.route, map[string]string{}, true
}

func (f *fakeTable) Methods(string) []string { return nil }

func (f *fakeTable) Routes() []routing.Route { return nil }

// fakeRequester is a proxy.NatsRequester double that returns a fixed
// reply payload regardless of subject or timeout.
type fakeRequester struct {
	reply []byte
}

func (f *fakeRequester) Request(context.Context, string, []byte, time.Duration) ([]byte, error) {
	return f.reply, nil
}

func TestAdapter_ForwardsResponseThroughProxyHandler(t *testing.T) {
	table := &fakeTable{
		route: routing.Route{
			Subject:      "svc.cmd.users.list",
			Method:       "GET",
			PathTemplate: "/users",
		},
		hit: true,
	}
	requester := &fakeRequester{
		reply: []byte(`{"status":200,"headers":{"x-custom":["yes"]},"body":{"ok":true}}`),
	}
	handler := proxy.NewHandler(proxy.HandlerConfig{
		Table:   func() routing.Table { return table },
		Nats:    requester,
		Encoder: proxy.NewDefaultEncoder(),
		Decoder: proxy.NewDefaultDecoder(),
		Timeout: 5 * time.Second,
		Logger:  zerolog.Nop(),
	})

	adapter := NewHertzAdapter(handler)
	ctx := ut.CreateUtRequestContext("GET", "https://gateway.test/users", nil)

	adapter(nil, ctx)

	assert.Equal(t, 200, ctx.Response.StatusCode())
	assert.Equal(t, "yes", string(ctx.Response.Header.Peek("x-custom")))
	assert.Contains(t, string(ctx.Response.Header.Peek("Content-Type")), "application/json")
	assert.Equal(t, `{"ok":true}`, string(ctx.Response.Body()))
}

func TestAdapter_Returns404WhenRouteNotFound(t *testing.T) {
	table := &fakeTable{hit: false}
	handler := proxy.NewHandler(proxy.HandlerConfig{
		Table:   func() routing.Table { return table },
		Nats:    &fakeRequester{},
		Encoder: proxy.NewDefaultEncoder(),
		Decoder: proxy.NewDefaultDecoder(),
		Timeout: 5 * time.Second,
		Logger:  zerolog.Nop(),
	})

	adapter := NewHertzAdapter(handler)
	ctx := ut.CreateUtRequestContext("GET", "https://gateway.test/unknown", nil)

	adapter(nil, ctx)

	assert.Equal(t, 404, ctx.Response.StatusCode())
}

func TestWriteServeResult_StampsCacheControlNoStoreOnGatewayOwnedBody(t *testing.T) {
	// Gateway-produced error bodies (404/502/503/504/...) are transient
	// infrastructure failures. Intermediate caches must not memoize
	// them; `Cache-Control: no-store` is the only wire contract that
	// guarantees every cache hop re-fetches the origin's answer.
	ctx := ut.CreateUtRequestContext("GET", "https://gateway.test/x", nil)
	result := &proxy.ServeResult{
		Status:           504,
		Headers:          map[string][]string{},
		Body:             []byte(`{"error":"Gateway Timeout"}`),
		GatewayOwnedBody: true,
	}

	writeServeResult(ctx, result, "test-req-id")

	assert.Equal(t, "no-store", string(ctx.Response.Header.Peek("Cache-Control")))
}

func TestWriteServeResult_LeavesHandlerThrownErrorsUntouched(t *testing.T) {
	// When an upstream handler replied with a 500 (or any other error)
	// through its own exception filter, the gateway must NOT override
	// the application's cache policy — the handler-thrown body is
	// part of the app contract.
	ctx := ut.CreateUtRequestContext("GET", "https://gateway.test/x", nil)
	result := &proxy.ServeResult{
		Status:           500,
		Headers:          map[string][]string{"cache-control": {"private, max-age=60"}},
		Body:             []byte(`{"statusCode":500,"message":"handler went boom"}`),
		GatewayOwnedBody: false,
	}

	writeServeResult(ctx, result, "test-req-id")

	// Handler-owned Cache-Control stays intact.
	assert.Equal(t, "private, max-age=60", string(ctx.Response.Header.Peek("Cache-Control")))
}

// TestBuildServeInput_StripsSpoofableInboundRequestID pins the
// single-correlator-plane contract: the gateway NEVER honours an
// inbound X-Request-Id — `meta.requestId` (gateway-generated ULID) is
// the only trusted correlator. Forwarding the client-supplied header
// alongside it would hand upstream handlers a spoofable id one key
// away from the authoritative one.
func TestBuildServeInput_StripsSpoofableInboundRequestID(t *testing.T) {
	ctx := ut.CreateUtRequestContext("GET", "https://gateway.test/x", nil,
		ut.Header{Key: "X-Request-Id", Value: "client-forged-id"},
		ut.Header{Key: "X-Custom", Value: "kept"},
	)

	input := buildServeInput(ctx)

	assert.NotContains(t, input.Headers, "x-request-id",
		"client-supplied X-Request-Id must not reach the upstream header map")
	assert.Equal(t, "kept", input.Headers["x-custom"])
	assert.NotEqual(t, "client-forged-id", input.RequestID,
		"meta correlator stays gateway-generated")
	assert.NotEmpty(t, input.RequestID)
}

// TestBuildServeInput_TracestatePassesThrough turns the previously
// ACCIDENTAL tracestate propagation into a pinned contract: W3C Trace
// Context §3.4 requires a pass-through participant to forward
// traceparent AND tracestate together unchanged. traceparent is
// first-class in the envelope meta; tracestate rides the generic
// header map — this test is what keeps a future header-filtering
// change from silently dropping vendor sampling state.
func TestBuildServeInput_TracestatePassesThrough(t *testing.T) {
	ctx := ut.CreateUtRequestContext("GET", "https://gateway.test/x", nil,
		ut.Header{Key: "traceparent", Value: "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"},
		ut.Header{Key: "tracestate", Value: "vendor=opaque,other=value"},
	)

	input := buildServeInput(ctx)

	assert.Equal(t, "vendor=opaque,other=value", input.Headers["tracestate"],
		"tracestate must survive into the forwarded header map unchanged")
	assert.Equal(t, "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01", input.Traceparent)
}
