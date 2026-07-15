package codec

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

// FuzzUnmarshalAny drives arbitrary bytes through the gateway's decode
// path and pins the trust-boundary contract of the codec package:
// anything that decodes successfully must be safe to hand to public
// HTTP clients under application/json.
//
// Invariants:
//
//  1. Unmarshal never panics, whatever the input.
//  2. Every string reachable from a successfully decoded value is
//     valid UTF-8 (the ValidateString sanitisation contract — see
//     decodeAPI).
//  3. A successfully decoded value re-marshals without error, and the
//     re-marshalled bytes are valid UTF-8 and parseable by
//     encoding/json (RFC 8259 §8.1 interchange guarantee).
func FuzzUnmarshalAny(f *testing.F) {
	seeds := [][]byte{
		[]byte(`{"name":"alice","age":30}`),
		[]byte(`{"value": "not-a-number"}`),
		[]byte(`null`),
		[]byte(``),
		[]byte(`{}`),
		[]byte(`[]`),
		[]byte(`"plain"`),
		// Invalid UTF-8 inside a string value: must sanitise to U+FFFD.
		[]byte("{\"v\":\"a\x80b\"}"),
		// Truncated multibyte tail.
		[]byte("\"a\xc3\""),
		// Overlong encoding of '/' (0xC0 0xAF) — never valid UTF-8.
		[]byte("\"\xc0\xaf\""),
		// CESU-8 surrogate half encoded as raw bytes.
		[]byte("\"\xed\xa0\x80\""),
		// Lone surrogate as a \u escape.
		[]byte(`"\ud800"`),
		// Surrogate pair split across escapes (valid pair).
		[]byte(`"😀"`),
		// UTF-8 BOM before the document.
		[]byte("\xef\xbb\xbf{}"),
		// NUL byte inside a string.
		[]byte("\"a\x00b\""),
		// Huge numbers: overflow int64, overflow float64 exponent.
		[]byte(`18446744073709551616`),
		[]byte(`1e100000000`),
		[]byte(`-1e-100000000`),
		[]byte(`0.00000000000000000000000000000000000001`),
		// Deep nesting (sonic caps at 4096; 64 stays decodable).
		[]byte(strings.Repeat("[", 64) + "1" + strings.Repeat("]", 64)),
		[]byte(strings.Repeat(`{"a":`, 32) + "null" + strings.Repeat("}", 32)),
		// Duplicate keys.
		[]byte(`{"a":1,"a":2}`),
		// Escaped control characters and line separators.
		[]byte("\"\\u0000\\u2028\\u2029\""),
		// Trailing garbage after a valid document.
		[]byte(`{} extra`),
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		var v any
		if err := Unmarshal(data, &v); err != nil {
			return
		}

		requireUTF8Clean(t, v)

		out, err := Marshal(v)
		require.NoError(t, err, "value decoded by Unmarshal must re-marshal")
		require.True(t, utf8.Valid(out), "Marshal output must be valid UTF-8: %q", out)
		require.True(t, json.Valid(out), "Marshal output must be parseable by encoding/json: %q", out)
	})
}

// FuzzValid differentially tests the intake-guard validator against
// the strict stdlib scanner. Valid gates client bodies before they
// are spliced verbatim into the request envelope, so every document
// it accepts must also be accepted by strict RFC 8259 parsers —
// otherwise the gateway forwards an envelope the upstream decoder
// rejects and the client sees a 5xx instead of a clean 400.
//
// Invariants:
//
//  1. Valid never panics.
//  2. Valid(data) implies json.Valid(data) — the accepted set is
//     contained in the strict stdlib set. (The reverse is not
//     asserted: Valid may reject documents the stdlib tolerates,
//     e.g. nesting past sonic's 4096-level cap, which fails closed.)
func FuzzValid(f *testing.F) {
	seeds := [][]byte{
		[]byte(`{"a":1}`),
		[]byte("{\n\t\"a\": 1\r\n}"),
		[]byte(``),
		[]byte(`null`),
		[]byte(`"plain"`),
		// Raw control bytes inside strings: sonic-lax, RFC-invalid.
		[]byte("\"a\x01b\""),
		[]byte("\"a\nb\""),
		[]byte("\"a\x00b\""),
		// Invalid escape sequences: sonic-lax, RFC-invalid. The first
		// entry is the fuzzer-found envelope breaker (see the
		// FuzzAppendEnvelopeJSON corpus).
		[]byte(`"\0"`),
		[]byte(`"\x41"`),
		[]byte(`"\"`),
		// Truncated literals: sonic-lax, RFC-invalid. The bare `n` is
		// a fuzzer-found divergence (see this fuzzer's corpus).
		[]byte(`n`),
		[]byte(`tru`),
		[]byte(`[null,true,false]`),
		// Legal escapes and legal raw whitespace.
		[]byte(`"a\n\t\\\"éb"`),
		[]byte(`[1, 2,	3]`),
		// Invalid UTF-8 (accepted by both validators — the decode
		// path sanitises it, so containment still holds).
		[]byte("\"a\x80b\""),
		[]byte(`{} trailing`),
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		if Valid(data) {
			require.True(t, json.Valid(data),
				"Valid accepted a document strict parsers reject: %q", data)
		}
	})
}

// requireUTF8Clean walks a decoded JSON value and fails the test on
// the first string (value or object key) that is not valid UTF-8.
// Depth is bounded by sonic's own 4096-level decode cap, so plain
// recursion cannot exhaust the test goroutine stack.
func requireUTF8Clean(t *testing.T, v any) {
	t.Helper()
	switch typed := v.(type) {
	case string:
		require.True(t, utf8.ValidString(typed), "decoded string must be valid UTF-8: %q", typed)
	case []any:
		for _, item := range typed {
			requireUTF8Clean(t, item)
		}
	case map[string]any:
		for key, item := range typed {
			require.True(t, utf8.ValidString(key), "decoded object key must be valid UTF-8: %q", key)
			requireUTF8Clean(t, item)
		}
	}
}
