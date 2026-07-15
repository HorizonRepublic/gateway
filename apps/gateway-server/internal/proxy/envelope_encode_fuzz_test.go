package proxy

import (
	"bytes"
	"encoding/json"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/codec"
)

// stdlibNormalizeString is the reference sanitisation: what a string
// looks like after travelling through encoding/json (encode then
// decode). Invalid UTF-8 bytes come back as one U+FFFD per byte,
// valid text comes back unchanged. appendJSONString must agree with
// this normalisation for every possible input.
func stdlibNormalizeString(t *testing.T, s string) string {
	t.Helper()
	encoded, err := json.Marshal(s)
	require.NoError(t, err)

	var out string
	require.NoError(t, json.Unmarshal(encoded, &out))

	return out
}

// compactJSON compacts a JSON value so byte-level comparisons ignore
// insignificant whitespace differences between encoder and decoder.
func compactJSON(t *testing.T, raw []byte) string {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, json.Compact(&buf, raw))

	return buf.String()
}

// FuzzAppendJSONString differentially tests the hand-rolled JSON
// string emitter against encoding/json across arbitrary byte
// sequences (Go fuzz strings are not guaranteed to be valid UTF-8).
//
// Invariants:
//
//  1. appendJSONString never panics.
//  2. The output is valid UTF-8 — invalid input bytes must surface as
//     � escapes, never as raw bytes (RFC 8259 §8.1).
//  3. The output is a single JSON string literal that encoding/json
//     accepts.
//  4. Decoding the output yields exactly the string that decoding
//     encoding/json's own encoding yields (semantic parity; escaping
//     choices such as U+2028 handling may differ byte-wise, both are
//     legal JSON).
func FuzzAppendJSONString(f *testing.F) {
	seeds := []string{
		"",
		"hello",
		`he said "hi"`,
		`a\b`,
		"line1\nline2",
		"a\tb",
		"a\rb",
		"a\bb",
		"a\fb",
		"a\x01b",
		"a\x00b",
		"a\x7fb",
		"привіт",
		"hi 👋",
		"<script>&amp;</script>",
		"\u2028\u2029",
		// Invalid UTF-8: lone continuation, truncated multibyte tail,
		// 0xFF 0xFE pair, mixed with valid runes.
		"a\x80b",
		"a\xc3",
		"\xff\xfe",
		"ok\x80привіт\xc3ok",
		// Overlong encoding of '/'.
		"\xc0\xaf",
		// CESU-8 surrogate halves as raw bytes.
		"\xed\xa0\x80",
		"\xed\xa0\x80\xed\xbd\x9e",
		// UTF-8 BOM.
		"\xef\xbb\xbfx",
		// Maximal valid code point and boundary sequences.
		"\U0010FFFF",
		"\xf4\x90\x80\x80", // first byte sequence past U+10FFFF — invalid
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, s string) {
		out := appendJSONString(nil, s)

		require.True(t, utf8.Valid(out), "output must be valid UTF-8: %q", out)

		var got string
		require.NoError(t, json.Unmarshal(out, &got),
			"output must be a JSON string literal encoding/json accepts: %q", out)

		require.Equal(t, stdlibNormalizeString(t, s), got,
			"sanitisation must match encoding/json semantics for input %q", s)
	})
}

// FuzzAppendEnvelopeJSON drives the full hand-rolled envelope emitter
// with adversarial field values. Body and Auth are gated with
// codec.Valid before encoding, mirroring the production contract:
// Handle rejects non-JSON bodies with 400 before the encoder runs,
// and the verifier reply is a decoded JSON document.
//
// Invariants:
//
//  1. appendEnvelopeJSON never panics.
//  2. The output is one JSON document that encoding/json accepts.
//  3. When Body and Auth are valid UTF-8, the whole envelope is valid
//     UTF-8 (all other fields are sanitised by appendJSONString).
//  4. Decoding the output recovers every field after encoding/json
//     string normalisation — no field bleeds into another, no map
//     entry is lost, meta integers survive exactly.
//  5. The auth key is present if and only if Auth was non-empty.
func FuzzAppendEnvelopeJSON(f *testing.F) {
	f.Add(
		"POST", "/users/:id", "/users/42",
		"id", "42",
		"include", "profile", "a", "b",
		"authorization", "Bearer xxx",
		[]byte(`{"email":"a@b.c"}`), []byte(`{"userId":"u1"}`),
		"01HXY0000000000000000000", "00-trace", "127.0.0.1",
		int64(1000), int64(30000),
	)
	f.Add(
		"GET", "", "",
		"", "",
		"", "", "", "",
		"", "",
		[]byte(nil), []byte(nil),
		"", "", "",
		int64(0), int64(0),
	)
	f.Add(
		"GET\x80", "/p\x00ath", "/\xff\xfe",
		"k\x01", "v\\\"",
		"q\xed\xa0\x80", "s\nv", "m\t1", "m\r2",
		"h\xc0\xaf", "line1\nline2",
		[]byte("\"a\\u0001b\""), []byte(`[1,2,3]`),
		"привіт", "\u2028\u2029", "::ffff:10.0.0.1",
		int64(-9223372036854775808), int64(9223372036854775807),
	)

	f.Fuzz(func(t *testing.T,
		method, path, matchedPath string,
		paramKey, paramValue string,
		queryKey, querySingle, queryMultiA, queryMultiB string,
		headerKey, headerValue string,
		body, auth []byte,
		requestID, traceparent, remoteAddr string,
		receivedAt, timeoutMs int64,
	) {
		if len(body) > 0 && !codec.Valid(body) {
			body = nil
		}
		if len(auth) > 0 && !codec.Valid(auth) {
			auth = nil
		}

		multiKey := queryKey + "-multi"
		env := &GatewayRequest{
			Route: RouteContext{Method: method, Path: path, MatchedPath: matchedPath},
			Params: map[string]string{
				paramKey: paramValue,
			},
			Query: map[string]QueryValue{
				queryKey: NewQueryValueString(querySingle),
				multiKey: NewQueryValueStrings([]string{queryMultiA, queryMultiB}),
			},
			Headers: map[string]string{
				headerKey: headerValue,
			},
			Body: body,
			Auth: auth,
			Meta: RequestMeta{
				RequestID:   requestID,
				Traceparent: traceparent,
				RemoteAddr:  remoteAddr,
				ReceivedAt:  receivedAt,
				TimeoutMs:   timeoutMs,
			},
		}

		out := appendEnvelopeJSON(nil, env)

		require.True(t, json.Valid(out), "envelope must be a valid JSON document: %q", out)
		if utf8.Valid(body) && utf8.Valid(auth) {
			require.True(t, utf8.Valid(out), "envelope must be valid UTF-8: %q", out)
		}

		var decoded GatewayRequest
		require.NoError(t, json.Unmarshal(out, &decoded))

		norm := func(s string) string { return stdlibNormalizeString(t, s) }

		require.Equal(t, RouteContext{
			Method:      norm(method),
			Path:        norm(path),
			MatchedPath: norm(matchedPath),
		}, decoded.Route)

		require.Equal(t, map[string]string{norm(paramKey): norm(paramValue)}, decoded.Params)
		require.Equal(t, map[string]string{norm(headerKey): norm(headerValue)}, decoded.Headers)
		require.Equal(t, map[string]QueryValue{
			norm(queryKey): NewQueryValueString(norm(querySingle)),
			norm(multiKey): NewQueryValueStrings([]string{norm(queryMultiA), norm(queryMultiB)}),
		}, decoded.Query)

		require.Equal(t, RequestMeta{
			RequestID:   norm(requestID),
			Traceparent: norm(traceparent),
			RemoteAddr:  norm(remoteAddr),
			ReceivedAt:  receivedAt,
			TimeoutMs:   timeoutMs,
		}, decoded.Meta)

		if len(body) > 0 {
			require.Equal(t, compactJSON(t, body), compactJSON(t, decoded.Body),
				"body must be forwarded verbatim")
		} else {
			require.Equal(t, "null", string(decoded.Body), "empty body must serialize as null")
		}

		var asMap map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(out, &asMap))
		_, hasAuth := asMap["auth"]
		require.Equal(t, len(auth) > 0, hasAuth,
			"auth key must be present exactly when claims were attached")
		if len(auth) > 0 {
			require.Equal(t, compactJSON(t, auth), compactJSON(t, asMap["auth"]),
				"auth must be forwarded verbatim")
		}
	})
}
