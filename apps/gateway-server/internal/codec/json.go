// Package codec is the gateway's single entry point for JSON marshalling.
//
// All JSON encoding and decoding in the gateway MUST go through this
// package so the underlying implementation (sonic today) can be swapped
// in a single place — for example, to plug in a protobuf codec later
// or to revert to encoding/json for debugging.
package codec

import (
	"encoding/json"
	"fmt"

	"github.com/bytedance/sonic"
)

// Marshal serializes v to JSON using sonic's optimized encoder.
//
// The returned slice is freshly allocated on every call. Hot-path
// callers that need zero-alloc encoding should use
// sonic/encoder.EncodeInto directly against a pooled *[]byte (see
// the envelope encoder in internal/proxy).
func Marshal(v any) ([]byte, error) {
	b, err := sonic.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("codec marshal: %w", err)
	}

	return b, nil
}

// decodeAPI is the frozen sonic configuration for the decode path.
// ValidateString makes sonic sanitise invalid UTF-8 inside JSON
// strings to U+FFFD exactly like encoding/json — without it, sonic's
// default passes raw invalid bytes through (including inside
// json.RawMessage), and the gateway would forward them from a
// non-SDK NATS publisher straight to public HTTP clients under
// application/json, violating RFC 8259 §8.1. The decode path is off
// the hot encode path, so the validation cost lands where the trust
// boundary is.
var decodeAPI = sonic.Config{ValidateString: true}.Froze()

// Unmarshal decodes JSON bytes into v. Wraps the underlying sonic
// error so callers get a uniform context prefix when logging.
// Invalid UTF-8 inside string values is sanitised to U+FFFD
// (encoding/json-equivalent behaviour — see decodeAPI).
func Unmarshal(data []byte, v any) error {
	if err := decodeAPI.Unmarshal(data, v); err != nil {
		return fmt.Errorf("codec unmarshal: %w", err)
	}

	return nil
}

// Valid reports whether data is a syntactically valid RFC 8259 JSON
// document. Used by the proxy intake guard: the request envelope is
// one JSON text, so embedding a non-JSON body would invalidate the
// whole document for every upstream consumer.
//
// The check runs in two phases: sonic's SIMD-accelerated validator
// rejects malformed input fast, and encoding/json's scanner confirms
// every acceptance. The confirmation pass exists because sonic's
// validator is laxer than RFC 8259 in ways fuzzing has demonstrated
// concretely: it accepts raw control bytes inside string values
// (§7 requires them escaped), invalid escape sequences such as `\0`,
// and truncated literals such as a bare `n`. Strict downstream
// parsers (encoding/json, the SDK side's JSON.parse, this package's
// own Unmarshal) reject all of those, so a body that passed a
// sonic-only guard could produce a request envelope the upstream
// cannot decode — surfacing as a 5xx from the handler instead of a
// clean 400 at the gate. FuzzValid holds Valid to the containment
// invariant: everything it accepts, the strict stdlib scanner
// accepts too.
func Valid(data []byte) bool {
	return sonic.Valid(data) && json.Valid(data)
}
