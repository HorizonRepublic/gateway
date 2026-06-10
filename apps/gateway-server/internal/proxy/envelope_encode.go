package proxy

// This file houses a hand-rolled, zero-allocation JSON encoder for
// GatewayRequest envelopes. It exists to bypass sonic on the hot
// encode path: sonic's map-iteration code path allocates five objects
// per encode when reflecting over the three map fields of
// GatewayRequest, and no amount of output-slice pooling can eliminate
// those. The decode path (GatewayReply) stays on sonic because it is
// off the gateway's hot path and sonic's reflection-free SIMD decoder
// is already faster than anything hand-rolled here would be.
//
// The helpers follow the idiomatic Go append pattern: each takes a
// []byte by value, appends its contribution, and returns the grown
// slice. Top-level entry point is appendEnvelopeJSON. All helpers are
// package-private: they are a strict implementation detail of
// DefaultEncoder and must not become part of the proxy package's
// public surface.
//
// Wire-format invariants that callers rely on:
//
//   - Field names match the struct tags on GatewayRequest exactly.
//   - Map object key order follows Go's randomized map iteration —
//     JSON object key ordering is unspecified per RFC 8259 §4 so this
//     is safe.
//   - An empty Body (nil OR zero-length slice) serializes to the
//     literal "null"; a non-empty json.RawMessage is appended verbatim
//     with no re-validation. The proxy handler enforces this upstream:
//     Handle rejects any non-empty body that fails codec.Valid with
//     400 before the encoder runs, so a non-JSON body can never reach
//     this append path.
//     Treating an empty slice the same as nil matters because the HTTP
//     adapter populates Body from the framework's Request.Body(), which
//     returns []byte{} (not nil) when the client sent no body — and
//     emitting `"body":` with nothing after it would produce invalid
//     JSON that the upstream decoder rejects.
//   - Meta.Traceparent is emitted only when non-empty, mirroring the
//     `traceparent,omitempty` struct tag that sonic honors.
//   - QueryValue.Single variant → JSON string; Multi variant → JSON
//     array of strings, always, even for single-element slices.

import "strconv"

// appendEnvelopeJSON appends the JSON-encoded form of envelope onto
// buf and returns the grown slice. Cannot fail: every field of
// GatewayRequest has a total JSON serialization, so there is no error
// return. Keeping the fire-and-forget signature lets the caller wire
// the result back into *out with a single assignment.
func appendEnvelopeJSON(buf []byte, envelope *GatewayRequest) []byte {
	buf = append(buf, '{')

	buf = append(buf, `"route":`...)
	buf = appendRouteContext(buf, envelope.Route)

	buf = append(buf, `,"params":`...)
	buf = appendStringMap(buf, envelope.Params)

	buf = append(buf, `,"query":`...)
	buf = appendQueryMap(buf, envelope.Query)

	buf = append(buf, `,"headers":`...)
	buf = appendStringMap(buf, envelope.Headers)

	buf = append(buf, `,"body":`...)
	if len(envelope.Body) == 0 {
		buf = append(buf, `null`...)
	} else {
		buf = append(buf, envelope.Body...)
	}

	// Auth is emitted only when claims are present. Public routes and
	// optional-auth anonymous requests carry a nil Auth slice and must
	// not leak a `"auth":null` field — that would confuse consumers
	// running a pre-auth SDK build.
	if len(envelope.Auth) > 0 {
		buf = append(buf, `,"auth":`...)
		buf = append(buf, envelope.Auth...)
	}

	buf = append(buf, `,"meta":`...)
	buf = appendRequestMeta(buf, envelope.Meta)

	buf = append(buf, '}')
	return buf
}

// appendRouteContext emits a RouteContext as a JSON object with the
// three string fields in declaration order. Kept standalone so a
// future caller that needs just a RouteContext fragment can reuse it
// without pulling in the full envelope emitter.
func appendRouteContext(buf []byte, r RouteContext) []byte {
	buf = append(buf, `{"method":`...)
	buf = appendJSONString(buf, r.Method)
	buf = append(buf, `,"path":`...)
	buf = appendJSONString(buf, r.Path)
	buf = append(buf, `,"matchedPath":`...)
	buf = appendJSONString(buf, r.MatchedPath)
	buf = append(buf, '}')
	return buf
}

// appendRequestMeta emits a RequestMeta as a JSON object. The
// Traceparent field is elided when empty to match the
// `traceparent,omitempty` struct tag; every other field is unconditionally
// present because the wire contract treats them as required.
func appendRequestMeta(buf []byte, m RequestMeta) []byte {
	buf = append(buf, `{"requestId":`...)
	buf = appendJSONString(buf, m.RequestID)
	if m.Traceparent != "" {
		buf = append(buf, `,"traceparent":`...)
		buf = appendJSONString(buf, m.Traceparent)
	}
	buf = append(buf, `,"remoteAddr":`...)
	buf = appendJSONString(buf, m.RemoteAddr)
	buf = append(buf, `,"receivedAt":`...)
	buf = strconv.AppendInt(buf, m.ReceivedAt, 10)
	buf = append(buf, `,"timeoutMs":`...)
	buf = strconv.AppendInt(buf, m.TimeoutMs, 10)
	buf = append(buf, '}')
	return buf
}

// appendStringMap emits a map[string]string as a JSON object. An
// empty or nil map yields `{}`. Map iteration order is randomized by
// the Go runtime, which is spec-compliant for JSON objects but means
// tests must not rely on byte-level output equality.
func appendStringMap(buf []byte, m map[string]string) []byte {
	buf = append(buf, '{')
	first := true
	for k, v := range m {
		if !first {
			buf = append(buf, ',')
		}
		first = false
		buf = appendJSONString(buf, k)
		buf = append(buf, ':')
		buf = appendJSONString(buf, v)
	}
	buf = append(buf, '}')
	return buf
}

// appendQueryMap emits a map[string]QueryValue as a JSON object where
// each value is serialized per QueryValue's union semantics. Iteration
// order is randomized for the same reason as appendStringMap.
func appendQueryMap(buf []byte, m map[string]QueryValue) []byte {
	buf = append(buf, '{')
	first := true
	for k, v := range m {
		if !first {
			buf = append(buf, ',')
		}
		first = false
		buf = appendJSONString(buf, k)
		buf = append(buf, ':')
		buf = appendQueryValue(buf, v)
	}
	buf = append(buf, '}')
	return buf
}

// appendQueryValue emits a QueryValue per its union discriminator.
// The Multi variant (non-nil slice) is always serialized as a JSON
// array, even for single-element slices, to preserve the "repeated
// key" semantics that the Nest side relies on when disambiguating
// `?a=1` from `?a=1&a=1`. The Single variant emits a plain JSON
// string.
func appendQueryValue(buf []byte, v QueryValue) []byte {
	if v.Multi != nil {
		buf = append(buf, '[')
		for i, s := range v.Multi {
			if i > 0 {
				buf = append(buf, ',')
			}
			buf = appendJSONString(buf, s)
		}
		buf = append(buf, ']')
		return buf
	}
	return appendJSONString(buf, v.Single)
}

// appendJSONString appends a JSON string literal (including the
// surrounding quotes) onto buf per RFC 8259 §7. Control characters
// below 0x20, the backslash, and the double-quote are escaped; every
// other byte — including DEL (0x7F) and multi-byte UTF-8 sequences
// — passes through verbatim. This exactly mirrors encoding/json's
// behavior and the test suite certifies the byte-for-byte parity.
//
// The inner loop walks a run of "safe" bytes and flushes them with a
// single append — mirroring encoding/json.encodeString — so the
// steady-state cost on ASCII-only input is one append per string.
//
// Not exported because it is an implementation detail of the proxy
// encoder; callers outside this package that need a JSON string
// helper should reach for codec.Marshal (sonic) or encoding/json
// instead.
func appendJSONString(buf []byte, s string) []byte {
	buf = append(buf, '"')
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 0x20 && c != '"' && c != '\\' {
			continue
		}
		buf = append(buf, s[start:i]...)
		switch c {
		case '"':
			buf = append(buf, '\\', '"')
		case '\\':
			buf = append(buf, '\\', '\\')
		case '\n':
			buf = append(buf, '\\', 'n')
		case '\r':
			buf = append(buf, '\\', 'r')
		case '\t':
			buf = append(buf, '\\', 't')
		case '\b':
			buf = append(buf, '\\', 'b')
		case '\f':
			buf = append(buf, '\\', 'f')
		default:
			const hex = "0123456789abcdef"
			buf = append(buf, '\\', 'u', '0', '0', hex[c>>4], hex[c&0x0F])
		}
		start = i + 1
	}
	buf = append(buf, s[start:]...)
	buf = append(buf, '"')
	return buf
}
