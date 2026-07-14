package proxy

import (
	"encoding/json"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

// FuzzDefaultDecoderDecode drives arbitrary bytes through the reply
// decoder. Reply envelopes are semi-trusted: they normally come from
// the SDK, but any NATS publisher on the reply subject could send
// garbage, and a buggy SDK service could emit malformed envelopes.
//
// Invariants:
//
//  1. Decode never panics.
//  2. Failure is a typed error with a nil reply — never a partial one.
//  3. On success the status is inside the RFC 9110 range [100, 599].
//  4. On success the body is either empty or a valid-UTF-8 JSON
//     document that encoding/json accepts — the gateway writes it
//     verbatim to the HTTP response under application/json.
//  5. On success every header key and value is valid UTF-8.
func FuzzDefaultDecoderDecode(f *testing.F) {
	seeds := [][]byte{
		[]byte(`{"status":201,"headers":{"x-foo":["bar"],"set-cookie":["a=1","b=2"]},"body":{"id":"42"}}`),
		[]byte(`{"status":200,"headers":{},"body":null}`),
		[]byte(`{"status":100,"headers":{},"body":null}`),
		[]byte(`{"status":599,"headers":{},"body":null}`),
		[]byte(`{"status":0,"headers":{},"body":null}`),
		[]byte(`{"status":99,"headers":{},"body":null}`),
		[]byte(`{"status":600,"headers":{},"body":null}`),
		[]byte(`{"status":999,"headers":{},"body":null}`),
		[]byte(`{"status":-200,"headers":{},"body":null}`),
		[]byte(`not json`),
		[]byte(``),
		[]byte(`null`),
		[]byte(`{}`),
		// Fractional and overflow statuses.
		[]byte(`{"status":200.5,"headers":{},"body":null}`),
		[]byte(`{"status":1e100000000,"headers":{},"body":null}`),
		[]byte(`{"status":18446744073709551616,"headers":{},"body":null}`),
		// Invalid UTF-8 inside header value and body string.
		[]byte("{\"status\":200,\"headers\":{\"x\":[\"a\x80b\"]},\"body\":null}"),
		[]byte("{\"status\":200,\"headers\":{},\"body\":\"a\x80b\"}"),
		// Invalid UTF-8 inside a header KEY.
		[]byte("{\"status\":200,\"headers\":{\"a\x80b\":[\"v\"]},\"body\":null}"),
		// NUL byte inside a header value.
		[]byte("{\"status\":200,\"headers\":{\"x\":[\"a\x00b\"]},\"body\":null}"),
		// Lone surrogate escape in body.
		[]byte(`{"status":200,"headers":{},"body":"\ud800"}`),
		// BOM prefix.
		[]byte("\xef\xbb\xbf{\"status\":200,\"headers\":{},\"body\":null}"),
		// Wrong shapes for each field.
		[]byte(`{"status":"200","headers":{},"body":null}`),
		[]byte(`{"status":200,"headers":{"x":"scalar"},"body":null}`),
		[]byte(`{"status":200,"headers":[],"body":null}`),
		// Duplicate status keys.
		[]byte(`{"status":200,"status":700,"headers":{},"body":null}`),
		// Nested body document.
		[]byte(`{"status":200,"headers":{},"body":{"a":[1,2,{"b":"c"}]}}`),
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	dec := NewDefaultDecoder()

	f.Fuzz(func(t *testing.T, data []byte) {
		reply, err := dec.Decode(data)
		if err != nil {
			require.Nil(t, reply, "failed Decode must not return a partial reply")

			return
		}

		require.NotNil(t, reply)
		require.GreaterOrEqual(t, reply.Status, minValidHTTPStatus)
		require.LessOrEqual(t, reply.Status, maxValidHTTPStatus)

		if len(reply.Body) > 0 {
			require.True(t, utf8.Valid(reply.Body),
				"decoded body must be valid UTF-8 before it hits an HTTP client: %q", reply.Body)
			require.True(t, json.Valid(reply.Body),
				"decoded body must remain a valid JSON document: %q", reply.Body)
		}

		for key, values := range reply.Headers {
			require.True(t, utf8.ValidString(key), "header key must be valid UTF-8: %q", key)
			for _, value := range values {
				require.True(t, utf8.ValidString(value), "header value must be valid UTF-8: %q", value)
			}
		}
	})
}
