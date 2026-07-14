package nats

import (
	"fmt"
)

// Envelope headroom model for the startup payload-budget check.
//
// The proxy embeds the raw HTTP body verbatim inside a JSON envelope
// that additionally carries the route context, path params, query,
// forwarded headers, and request meta. The NATS payload is therefore
// strictly larger than the HTTP body, and nats.go rejects any message
// larger than the server-advertised max_payload client-side with
// ErrMaxPayload before it touches the wire. A configuration where
// HTTP_MAX_BODY_BYTES plus the envelope overhead exceeds max_payload
// deterministically fails in-contract requests at publish time, so it
// is a startup error, not a runtime surprise.
//
// The overhead bound is derived from what the client can control:
//
//   - Headers, request line (path + query), and params are all read
//     from the header buffer that Hertz caps at HTTP_MAX_HEADER_BYTES.
//     JSON string escaping inflates a byte to at most two bytes for
//     the characters legal in header values and URLs (`"` and `\`),
//     and the matched path is emitted twice (route.path + params), so
//     3x the header cap bounds the client-controlled envelope text.
//   - Route subject, meta (request id, traceparent, remote addr,
//     timestamps), and the JSON scaffolding are gateway-produced and
//     small; envelopeFixedOverheadBytes covers them with margin.
//
// Verifier-supplied auth claims also ride the envelope and are not
// client-bounded; the runtime ErrMaxPayload → 413 mapping in the
// proxy handler remains the backstop for that residual case.
const (
	// envelopeHeaderInflationFactor bounds how much the header-buffer
	// bytes (headers + request line) can grow once JSON-escaped and
	// duplicated inside the envelope.
	envelopeHeaderInflationFactor = 3

	// envelopeFixedOverheadBytes covers the gateway-produced envelope
	// fields (route context, meta, JSON scaffolding) with margin.
	envelopeFixedOverheadBytes = 4096
)

// EnvelopeOverheadBudget returns the number of bytes the startup check
// reserves on top of HTTP_MAX_BODY_BYTES for the request envelope,
// given the configured HTTP_MAX_HEADER_BYTES.
func EnvelopeOverheadBudget(maxHeaderBytes int) int64 {
	return int64(maxHeaderBytes)*envelopeHeaderInflationFactor + envelopeFixedOverheadBytes
}

// ValidatePayloadBudget verifies that a maximally-sized HTTP request
// (body at HTTP_MAX_BODY_BYTES, headers at HTTP_MAX_HEADER_BYTES)
// still fits the server-advertised NATS max_payload once wrapped in
// the request envelope. maxPayload is the live value reported by
// Conn.MaxPayload() after connect — the server delivers it in the
// INFO handshake, so the check reflects the actual cluster the
// gateway just joined, not an assumed default.
//
// A misfit is a fatal misconfiguration: requests that pass the HTTP
// body guard would deterministically fail at NATS publish time with
// ErrMaxPayload. The error names every knob involved so the operator
// can either lower HTTP_MAX_BODY_BYTES / HTTP_MAX_HEADER_BYTES or
// raise the NATS server's max_payload.
func ValidatePayloadBudget(maxPayload, maxBodyBytes int64, maxHeaderBytes int) error {
	required := maxBodyBytes + EnvelopeOverheadBudget(maxHeaderBytes)
	if required > maxPayload {
		return fmt.Errorf(
			"nats payload budget: HTTP_MAX_BODY_BYTES (%d) + envelope headroom (%d, derived from HTTP_MAX_HEADER_BYTES=%d) = %d exceeds the NATS server max_payload (%d); "+
				"lower HTTP_MAX_BODY_BYTES / HTTP_MAX_HEADER_BYTES or raise max_payload in the NATS server configuration",
			maxBodyBytes, EnvelopeOverheadBudget(maxHeaderBytes), maxHeaderBytes, required, maxPayload,
		)
	}

	return nil
}
