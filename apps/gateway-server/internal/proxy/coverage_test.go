package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/routing"
)

// TestPreviewClaimsForLog_EmptyInputReturnsEmptyString covers the
// guard at the top of previewClaimsForLog. An empty json.RawMessage
// must short-circuit to "" so the WARN log line does not emit a
// stray empty string for a verifier that legitimately returned no
// claims body.
func TestPreviewClaimsForLog_EmptyInputReturnsEmptyString(t *testing.T) {
	// Given: an empty raw message (length zero).
	var empty json.RawMessage

	// When: previewing for log emission.
	got := previewClaimsForLog(empty)

	// Then: the function returns the empty string verbatim.
	assert.Empty(t, got, "empty claims must not produce a preview")
}

// TestPreviewClaimsForLog_BoundaryAt256Bytes pins the exact
// boundary: an input of EXACTLY 256 bytes is preserved verbatim, no
// truncation. The cap is `n > maxPreview`, so the equal-length input
// MUST take the full-pass-through branch.
func TestPreviewClaimsForLog_BoundaryAt256Bytes(t *testing.T) {
	// Given: a payload whose length is exactly the maxPreview cap.
	exact := bytes.Repeat([]byte("a"), 256)

	// When: previewing for log emission.
	got := previewClaimsForLog(json.RawMessage(exact))

	// Then: the entire payload is returned, byte-for-byte.
	assert.Len(t, got, 256)
	assert.Equal(t, string(exact), got, "256-byte input must pass through verbatim")
}

// TestPreviewClaimsForLog_TruncatesAt257Bytes is the symmetric
// complement: one byte over the cap forces the truncation path. The
// existing 1024-byte test exercises a generic large input — this
// case pins the boundary at +1.
func TestPreviewClaimsForLog_TruncatesAt257Bytes(t *testing.T) {
	// Given: a payload whose length is one byte over the cap.
	oversize := bytes.Repeat([]byte("b"), 257)

	// When: previewing for log emission.
	got := previewClaimsForLog(json.RawMessage(oversize))

	// Then: only the first 256 bytes survive; the trailing byte is
	// dropped.
	assert.Len(t, got, 256, "input one byte over the cap must be truncated to maxPreview")
}

// TestPreviewClaimsForLog_MixedCaseSecretKeysAreRedacted exercises
// the case-insensitive flag of the redaction regex against keys
// whose casing does not match the canonical lowercase set. A
// case-sensitive matcher would let `"Token"` or `"PASSWORD"` slip
// past the redactor and surface a credential in the WARN log line.
func TestPreviewClaimsForLog_MixedCaseSecretKeysAreRedacted(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "TitleCase Token",
			in:   `{"Token":"abc.def.ghi","sub":"u1"}`,
			want: `{"Token":"***","sub":"u1"}`,
		},
		{
			name: "UPPERCASE PASSWORD",
			in:   `{"PASSWORD":"hunter2","id":"u1"}`,
			want: `{"PASSWORD":"***","id":"u1"}`,
		},
		{
			name: "MixedCase Authorization",
			in:   `{"Authorization":"Bearer xxx"}`,
			want: `{"Authorization":"***"}`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := previewClaimsForLog(json.RawMessage(c.in))
			assert.Equal(t, c.want, got)
		})
	}
}

// TestPreviewClaimsForLog_NestedObjectSecretValueRedacted pins that
// the redaction matcher walks string-valued secret keys regardless
// of nesting depth. The current pattern operates on the textual
// stream, so a `"token":"..."` pair survives even when wrapped in
// outer object literals — operators viewing a verifier reply WARN
// line never see a token leak just because the verifier nested its
// claim layout.
func TestPreviewClaimsForLog_NestedObjectSecretValueRedacted(t *testing.T) {
	// Given: a verifier-shaped payload with a token nested under an
	// outer claims object.
	in := `{"user":{"id":"u1","token":"jwt.value.sig"},"role":"admin"}`

	// When: previewing for log emission.
	got := previewClaimsForLog(json.RawMessage(in))

	// Then: the inner token value is replaced; the structural
	// scaffolding around it survives.
	assert.NotContains(t, got, "jwt.value.sig", "nested token value must not survive in preview")
	assert.Contains(t, got, `"token":"***"`, "nested token must redact to the masked form")
	assert.Contains(t, got, `"id":"u1"`, "non-secret nested fields are untouched")
	assert.Contains(t, got, `"role":"admin"`, "non-secret outer fields are untouched")
}

// TestHandler_VerifierForbiddenShortCircuitsWith403 covers the
// distinction between 401 (unauthenticated, default short-circuit)
// and 403 (authenticated but forbidden). On a non-optional route the
// gateway forwards both verbatim, but the 403 path differs from 401
// because it MUST NOT receive a stamped WWW-Authenticate header —
// 403 is a permissions failure, not a credentials failure, and
// stamping a Bearer challenge on a 403 would mislead clients into
// re-authenticating.
func TestHandler_VerifierForbiddenShortCircuitsWith403(t *testing.T) {
	// Given: a protected route whose verifier replies 403 with a
	// custom error body.
	routeSubject := "users-svc__microservice.cmd.users.me"
	verifierSubject := "users-svc__microservice.cmd.auth.verifier.jwt"
	route := routing.Route{
		Subject:      routeSubject,
		Method:       "GET",
		PathTemplate: "/users/me",
		Auth:         &routing.RouteAuth{VerifierSubject: verifierSubject},
	}
	nats := newFakeNats()
	nats.program(
		verifierSubject,
		[]byte(`{"status":403,"headers":{},"body":{"error":"Forbidden"}}`),
		nil,
	)

	sut := newAuthHandler(stubTable(route), nats)

	// When: handling the request.
	result := sut.Handle(context.Background(), authServeInput("GET", "/users/me"))

	// Then: gateway forwards the 403 verbatim and never invokes the
	// route subject. WWW-Authenticate is intentionally NOT stamped on
	// a 403 reply — the client is authenticated, just not authorised.
	assert.Equal(t, 403, result.Status)
	require.Len(t, nats.requests, 1, "route MUST NOT be called when verifier rejects with 403")
	assert.Empty(t, result.Headers["www-authenticate"],
		"403 must not carry a default Bearer challenge: it would suggest re-auth would help")
}

// TestHandler_VerifierServerErrorShortCircuitsVerbatim covers a 5xx
// reply from the verifier itself (not a transport failure). The
// verifier produced a structured reply, so the gateway forwards it
// verbatim instead of mapping to 502/503 — the verifier owns its
// own error contract on the protected route and a transport-grade
// remap would lose its custom body and headers.
func TestHandler_VerifierServerErrorShortCircuitsVerbatim(t *testing.T) {
	// Given: a verifier that replies 500 with a custom body.
	routeSubject := "users-svc__microservice.cmd.users.me"
	verifierSubject := "users-svc__microservice.cmd.auth.verifier.jwt"
	route := routing.Route{
		Subject:      routeSubject,
		Method:       "GET",
		PathTemplate: "/users/me",
		Auth:         &routing.RouteAuth{VerifierSubject: verifierSubject},
	}
	nats := newFakeNats()
	nats.program(
		verifierSubject,
		[]byte(`{"status":500,"headers":{"x-verifier-trace":["abc"]},"body":{"error":"verifier crashed"}}`),
		nil,
	)

	sut := newAuthHandler(stubTable(route), nats)

	// When: handling the request.
	result := sut.Handle(context.Background(), authServeInput("GET", "/users/me"))

	// Then: the 500 reply is forwarded verbatim, route is not called,
	// and verifier-set diagnostic headers reach the client.
	assert.Equal(t, 500, result.Status, "verifier 5xx forwards as-is, not remapped to 502/503")
	require.Len(t, nats.requests, 1, "route MUST NOT be called on verifier 5xx")
	assert.Equal(t, []string{"abc"}, result.Headers["x-verifier-trace"],
		"verifier diagnostic headers survive the short-circuit path")
}

// TestHandler_VerifierTimeoutReturns504 covers the verifier-side
// timeout branch in runAuthFlow. A NATS timeout on the auth
// sub-request must surface as 504 Gateway Timeout, mirroring the
// main-route timeout contract — the verifier shares the per-route
// budget, and a budget exhaustion on either leg is a timeout-class
// outcome from the client's perspective.
func TestHandler_VerifierTimeoutReturns504(t *testing.T) {
	// Given: a protected route whose verifier transport returns
	// context.DeadlineExceeded (the dominant timeout sentinel after
	// the requester moved onto RequestWithContext).
	routeSubject := "users-svc__microservice.cmd.users.me"
	verifierSubject := "users-svc__microservice.cmd.auth.verifier.jwt"
	route := routing.Route{
		Subject:      routeSubject,
		Method:       "GET",
		PathTemplate: "/users/me",
		Auth:         &routing.RouteAuth{VerifierSubject: verifierSubject},
	}
	nats := newFakeNats()
	nats.program(verifierSubject, nil, context.DeadlineExceeded)

	sut := newAuthHandler(stubTable(route), nats)

	// When: handling the request.
	result := sut.Handle(context.Background(), authServeInput("GET", "/users/me"))

	// Then: the gateway maps the timeout to 504 and never invokes
	// the route handler.
	assert.Equal(t, 504, result.Status,
		"verifier transport timeout must surface as 504, not 503")
	require.Len(t, nats.requests, 1, "route MUST NOT be called when verifier times out")
}

// TestHandler_VerifierMalformedReplyReturns502 covers the auth-flow
// decode-failure branch: a verifier that returned bytes the decoder
// cannot parse is upstream-protocol-broken, not transport-broken,
// and 502 Bad Gateway is the precise wire-level diagnosis.
func TestHandler_VerifierMalformedReplyReturns502(t *testing.T) {
	// Given: a verifier that replies with a payload that is not a
	// valid GatewayReply envelope.
	routeSubject := "users-svc__microservice.cmd.users.me"
	verifierSubject := "users-svc__microservice.cmd.auth.verifier.jwt"
	route := routing.Route{
		Subject:      routeSubject,
		Method:       "GET",
		PathTemplate: "/users/me",
		Auth:         &routing.RouteAuth{VerifierSubject: verifierSubject},
	}
	nats := newFakeNats()
	nats.program(verifierSubject, []byte(`not a json envelope`), nil)

	sut := newAuthHandler(stubTable(route), nats)

	// When: handling the request.
	result := sut.Handle(context.Background(), authServeInput("GET", "/users/me"))

	// Then: the gateway maps the malformed reply to 502 and never
	// calls the route subject.
	assert.Equal(t, 502, result.Status,
		"verifier reply that fails to decode must surface as 502")
	require.Len(t, nats.requests, 1,
		"route MUST NOT be called when verifier reply cannot be decoded")
}

// TestHandler_VerifierLogsErrorOnDecodeFailure pins the diagnostics
// contract for the decode-failure branch: an operator investigating
// a 502 spike on a protected route must see the verifier subject in
// the structured ERROR log so the failing service is identifiable
// at a glance.
func TestHandler_VerifierLogsErrorOnDecodeFailure(t *testing.T) {
	// Given: a buffer-backed logger and a verifier that returns
	// junk bytes.
	var buf bytes.Buffer
	routeSubject := "users-svc__microservice.cmd.users.me"
	verifierSubject := "users-svc__microservice.cmd.auth.verifier.jwt"
	route := routing.Route{
		Subject:      routeSubject,
		Method:       "GET",
		PathTemplate: "/users/me",
		Auth:         &routing.RouteAuth{VerifierSubject: verifierSubject},
	}
	nats := newFakeNats()
	nats.program(verifierSubject, []byte(`{not-json`), nil)

	sut := NewHandler(HandlerConfig{
		Table:   func() routing.Table { return stubTable(route) },
		Nats:    nats,
		Encoder: NewDefaultEncoder(),
		Decoder: NewDefaultDecoder(),
		Timeout: time.Second,
		Logger:  zerolog.New(&buf),
	})

	// When: handling the request.
	result := sut.Handle(context.Background(), authServeInput("GET", "/users/me"))

	// Then: the response is 502 and the structured log line carries
	// the auth-decode signal.
	require.Equal(t, 502, result.Status)
	assert.Contains(t, buf.String(), "auth: verifier reply decode failed",
		"decode failure must emit the auth-specific log message")
}

// TestHandler_OptionalAuth403StillShortCircuits is the symmetric
// complement of TestHandler_OptionalAuthContinuesOn401: optional
// auth swallows 401 (unauthenticated) but MUST NOT swallow 403
// (forbidden). 403 means the verifier authenticated the caller and
// concluded they lack permission — proceeding anonymously would
// silently downgrade an authorisation failure into a public-route
// invocation.
func TestHandler_OptionalAuth403StillShortCircuits(t *testing.T) {
	// Given: an optional-auth route whose verifier replies 403.
	routeSubject := "users-svc__microservice.cmd.articles.get"
	verifierSubject := "users-svc__microservice.cmd.auth.verifier.jwt"
	route := routing.Route{
		Subject:      routeSubject,
		Method:       "GET",
		PathTemplate: "/articles/:id",
		Auth:         &routing.RouteAuth{VerifierSubject: verifierSubject, Optional: true},
	}
	nats := newFakeNats()
	nats.program(
		verifierSubject,
		[]byte(`{"status":403,"headers":{},"body":{"error":"Forbidden"}}`),
		nil,
	)

	sut := newAuthHandler(stubTable(route), nats)

	// When: handling the request.
	result := sut.Handle(context.Background(), authServeInput("GET", "/articles/:id"))

	// Then: the 403 short-circuits even on the optional path; the
	// route handler is not invoked.
	assert.Equal(t, 403, result.Status,
		"optional-auth must NOT swallow 403 — only 401 falls through to the anonymous path")
	require.Len(t, nats.requests, 1,
		"route MUST NOT be called when an optional verifier rejects with 403")
}

// TestHandler_VerifierNonObjectClaimsBodyForwardsAsIs covers the
// shape-flexibility contract on the 200 happy path: the verifier
// reply Body field is forwarded as a json.RawMessage, so a non-
// object body (e.g., a bare string or number returned by a quirky
// custom verifier) must NOT crash the proxy. The bytes flow into
// the auth field of the route envelope verbatim.
//
// This pins forward compatibility with verifier implementations
// whose claim shape is not a JSON object (e.g., an opaque session
// id, a numeric tenant id, a plain string username).
func TestHandler_VerifierNonObjectClaimsBodyForwardsAsIs(t *testing.T) {
	// Given: a verifier that replies 200 with a bare-string body.
	routeSubject := "users-svc__microservice.cmd.users.me"
	verifierSubject := "users-svc__microservice.cmd.auth.verifier.jwt"
	route := routing.Route{
		Subject:      routeSubject,
		Method:       "GET",
		PathTemplate: "/users/me",
		Auth:         &routing.RouteAuth{VerifierSubject: verifierSubject},
	}
	nats := newFakeNats()
	// Body is a bare JSON string, not an object.
	nats.program(
		verifierSubject,
		[]byte(`{"status":200,"headers":{},"body":"opaque-session-id"}`),
		nil,
	)
	nats.program(
		routeSubject,
		[]byte(`{"status":200,"headers":{},"body":{"ok":true}}`),
		nil,
	)

	sut := newAuthHandler(stubTable(route), nats)

	// When: handling the request.
	result := sut.Handle(context.Background(), authServeInput("GET", "/users/me"))

	// Then: the route handler runs and the auth field on the route
	// envelope carries the bare-string claims verbatim.
	require.Equal(t, 200, result.Status)
	require.Len(t, nats.requests, 2)

	var routePayload map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(nats.requests[1].payload, &routePayload))
	assert.JSONEq(t, `"opaque-session-id"`, string(routePayload["auth"]),
		"non-object verifier claims body must flow into envelope.auth verbatim")
}

// TestReleasePayload_IsNilSafe pins the nil-receiver branch in
// releasePayload. Defer statements unconditionally release acquired
// payloads, so an early-return code path that never reached
// acquirePayload would otherwise panic on the deferred release. The
// guard exists for that reason and the test pins it explicitly.
func TestReleasePayload_IsNilSafe(t *testing.T) {
	// When: a nil pointer is released.
	// Then: the function does not panic.
	assert.NotPanics(t, func() {
		releasePayload(nil)
	})
}

// TestReleasePayload_ReturnsBufferToPool pins the behavioural
// contract that release path actually returns the buffer for reuse.
// A subsequent acquire MUST be able to write to the buffer without
// panicking and observe a zero-length view (with whatever capacity
// the prior caller grew to). The test therefore acquires, writes,
// releases, and re-acquires, asserting the slice is reset to len=0
// before the next caller appends.
func TestReleasePayload_ReturnsBufferToPool(t *testing.T) {
	// Given: a fresh payload buffer grown by a prior caller.
	first := acquirePayload()
	*first = append(*first, []byte(`{"k":"v"}`)...)
	require.NotEmpty(t, *first)
	releasePayload(first)

	// When: the next caller acquires a payload.
	next := acquirePayload()
	defer releasePayload(next)

	// Then: the slice header is reset to length zero so the next
	// EncodeInto starts from an empty buffer (sync.Pool may or may
	// not return the same backing array, but acquirePayload always
	// resets length).
	assert.Empty(t, *next, "acquirePayload must hand out a zero-length slice")
	// And: writing into the recycled buffer does not panic — the
	// slice header is valid.
	assert.NotPanics(t, func() {
		*next = append(*next, byte('x'))
	})
}

// TestQueryValue_UnmarshalMalformedStringReturnsError covers the
// JSON-parse error branch of the string-tagged path: input begins
// with `"` so the switch routes into the string case, but the
// content is invalid (unclosed string). The decoder must surface a
// formatted error that wraps the underlying json error and carries
// the diagnostic site name so an operator inspecting an envelope
// failure log sees "query value unmarshal string" alongside the
// raw payload.
func TestQueryValue_UnmarshalMalformedStringReturnsError(t *testing.T) {
	// Given: a payload that begins with " but is not a valid JSON
	// string (no terminating quote).
	var value QueryValue

	// When: unmarshalling the malformed input.
	err := value.UnmarshalJSON([]byte(`"unterminated`))

	// Then: the decoder surfaces an error with the diagnostic
	// "query value unmarshal string" prefix.
	require.Error(t, err)
	assert.True(t,
		strings.Contains(err.Error(), "query value unmarshal string"),
		"error must identify the failing site as the string variant: %q", err.Error())
}

// TestQueryValue_UnmarshalMalformedSliceReturnsError covers the
// JSON-parse error branch of the array-tagged path: input begins
// with `[` so the switch routes into the slice case, but the
// content is invalid for a string slice (numbers cannot fit
// []string). The decoder must surface a formatted error that wraps
// the underlying json error and carries the diagnostic site name
// so an operator distinguishes a malformed-slice failure from the
// malformed-string failure above.
func TestQueryValue_UnmarshalMalformedSliceReturnsError(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{
			name:  "numbers in string slice",
			input: `[1,2,3]`,
		},
		{
			name:  "unterminated array",
			input: `["a","b"`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Given: malformed array-shaped input.
			var value QueryValue

			// When: unmarshalling the malformed input.
			err := value.UnmarshalJSON([]byte(c.input))

			// Then: the decoder surfaces an error with the diagnostic
			// "query value unmarshal slice" prefix.
			require.Error(t, err)
			assert.True(t,
				strings.Contains(err.Error(), "query value unmarshal slice"),
				"error must identify the failing site as the slice variant: %q", err.Error())
		})
	}
}

// TestMergeAuthHeaders_SkipsEmptyVerifierValueSlice pins the
// well-known no-op branch in mergeAuthHeaders: when the verifier
// passes through a header key whose values slice is empty, the
// merger must skip it without crashing or stamping an empty slot
// in the merged map. Operators relying on header-presence checks
// must not see ghost entries from an empty-valued verifier reply.
func TestMergeAuthHeaders_SkipsEmptyVerifierValueSlice(t *testing.T) {
	// Given: a merged map with a baseline gateway-stamped key, and
	// auth headers that include an empty-value slice for a custom
	// header alongside a populated x-trace.
	merged := map[string][]string{
		"x-request-id": {"req-1"},
	}
	authHeaders := map[string][]string{
		"x-empty": {},
		"x-trace": {"verifier-trace-1"},
	}

	// When: merging the verifier headers.
	mergeAuthHeaders(merged, authHeaders)

	// Then: the empty-value key is skipped — never landing in the
	// merged map — while populated keys flow through.
	_, hasEmpty := merged["x-empty"]
	assert.False(t, hasEmpty, "header with empty value slice must not land in merged map")
	assert.Equal(t, []string{"verifier-trace-1"}, merged["x-trace"])
	assert.Equal(t, []string{"req-1"}, merged["x-request-id"])
}

// errorEncoder is a deliberately broken Encoder used to exercise
// the proxy's encode-failure branches (both the verifier-encode and
// the route-encode paths). The default DefaultEncoder cannot fail
// for any value the gateway constructs internally, so the only way
// to drive the error branch in tests is to inject a stub.
type errorEncoder struct {
	err error
}

func (e *errorEncoder) Encode(_ *[]byte, _ *EncodeInput) error {
	return e.err
}

// TestHandler_RouteEncodeFailureReturns500 covers the proxy
// encode-failure branch. The outbound encoder is the only stage
// the handler cannot decompose into a transport-grade outcome —
// an encode crash means the gateway cannot produce a wire-valid
// envelope at all, which is a 500 by definition (it is the
// gateway's own bug, not an upstream condition).
func TestHandler_RouteEncodeFailureReturns500(t *testing.T) {
	// Given: a public route whose encoder is wired to fail.
	table := &fakeTable{routes: map[string]routing.Route{
		"GET /users": {Subject: "svc.cmd.users.list", PathTemplate: "/users", Method: "GET"},
	}}
	nats := newFakeNats()

	sut := NewHandler(HandlerConfig{
		Table:   func() routing.Table { return table },
		Nats:    nats,
		Encoder: &errorEncoder{err: errors.New("encode boom")},
		Decoder: NewDefaultDecoder(),
		Timeout: time.Second,
		Logger:  zerolog.Nop(),
	})

	// When: handling the request.
	result := sut.Handle(context.Background(), emptyServeInput("GET", "/users"))

	// Then: the gateway emits 500 and never issues the NATS request.
	assert.Equal(t, 500, result.Status,
		"encode failure is a gateway-internal bug and must surface as 500")
	assert.Empty(t, nats.requests, "no NATS round trip on encode failure")
}

// TestHandler_VerifierEncodeFailureReturns500 covers the auth-flow
// encode-failure branch. The verifier sub-request shares the same
// outbound encoder; an encode failure at the auth stage must
// short-circuit with 500 and skip the verifier round trip entirely.
func TestHandler_VerifierEncodeFailureReturns500(t *testing.T) {
	// Given: a protected route whose encoder is wired to fail.
	routeSubject := "users-svc__microservice.cmd.users.me"
	verifierSubject := "users-svc__microservice.cmd.auth.verifier.jwt"
	route := routing.Route{
		Subject:      routeSubject,
		Method:       "GET",
		PathTemplate: "/users/me",
		Auth:         &routing.RouteAuth{VerifierSubject: verifierSubject},
	}
	nats := newFakeNats()

	sut := NewHandler(HandlerConfig{
		Table:   func() routing.Table { return stubTable(route) },
		Nats:    nats,
		Encoder: &errorEncoder{err: errors.New("verify encode boom")},
		Decoder: NewDefaultDecoder(),
		Timeout: time.Second,
		Logger:  zerolog.Nop(),
	})

	// When: handling the request.
	result := sut.Handle(context.Background(), authServeInput("GET", "/users/me"))

	// Then: the gateway emits 500 without issuing any NATS request.
	assert.Equal(t, 500, result.Status,
		"verifier encode failure is a gateway-internal bug and must surface as 500")
	assert.Empty(t, nats.requests,
		"no NATS round trip when the verifier envelope cannot be encoded")
}
