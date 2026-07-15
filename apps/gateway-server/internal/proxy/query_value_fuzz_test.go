package proxy

import (
	"encoding/json"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

// FuzzQueryValueUnmarshalJSON drives raw value bytes through
// QueryValue's union decoder the same way the upstream JSON decoder
// does: UnmarshalJSON receives the value slice directly.
//
// Invariants:
//
//  1. UnmarshalJSON never panics and every returned error survives
//     Error() formatting (guards the *json.UnmarshalTypeError
//     regression documented in query_value.go).
//  2. Success implies the input was the string or array variant —
//     numbers, booleans, null, and objects must be rejected.
//  3. Exactly one variant is active after a successful decode.
//  4. decode → encode → decode is stable: the re-decoded value equals
//     the first decode, and the encoded form is a valid UTF-8 JSON
//     value.
func FuzzQueryValueUnmarshalJSON(f *testing.F) {
	seeds := [][]byte{
		[]byte(`"alice"`),
		[]byte(`""`),
		[]byte(`["a","b"]`),
		[]byte(`[]`),
		[]byte(`["only"]`),
		[]byte(`42`),
		[]byte(`true`),
		[]byte(`false`),
		[]byte(`null`),
		[]byte(`{"k":"v"}`),
		[]byte(``),
		// Invalid UTF-8 raw bytes inside the JSON string.
		[]byte("\"a\x80b\""),
		// Lone surrogate escape.
		[]byte(`"\ud800"`),
		// Array with non-string members.
		[]byte(`["a",42]`),
		[]byte(`[null]`),
		[]byte(`[["nested"]]`),
		// Truncated documents.
		[]byte(`"unterminated`),
		[]byte(`["a"`),
		// Trailing garbage after a valid value.
		[]byte(`"a" trailing`),
		[]byte(`[] []`),
		// Whitespace-led value: first byte drives the union dispatch.
		[]byte(` "padded"`),
		// Escaped control characters and BOM content.
		[]byte("\"\\u0000\\t\""),
		[]byte("\"\xef\xbb\xbf\""),
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		var q QueryValue
		if err := q.UnmarshalJSON(data); err != nil {
			require.NotPanics(t, func() { _ = err.Error() },
				"decode errors must survive formatting")

			return
		}

		require.True(t, data[0] == '"' || data[0] == '[',
			"only string and array variants may decode successfully, got %q", data)
		if q.Multi != nil {
			require.Empty(t, q.Single, "variants must be mutually exclusive")
		}

		encoded, err := q.MarshalJSON()
		require.NoError(t, err, "a decoded QueryValue must re-encode")
		require.True(t, utf8.Valid(encoded), "encoded form must be valid UTF-8: %q", encoded)
		require.True(t, json.Valid(encoded), "encoded form must be valid JSON: %q", encoded)

		var again QueryValue
		require.NoError(t, again.UnmarshalJSON(encoded))
		require.Equal(t, q, again, "decode→encode→decode must be stable")
	})
}
