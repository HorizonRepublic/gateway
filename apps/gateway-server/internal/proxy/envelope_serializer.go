package proxy

// envelopeSerializer appends the wire representation of a
// GatewayRequest onto a caller-owned scratch buffer.
//
// This is the codec seam for the encoder's hot path: swapping the
// serializer implementation changes the wire format (JSON, msgpack,
// protobuf, …) without touching the proxy Handler, the envelope
// pool, or the HTTP transport adapter. The interface exists so that
// swapping the codec is a localised change, even though the default
// JSON path bypasses the codec package for performance reasons
// (sonic's map-iteration cost violates the zero-allocation budget).
//
// The contract is kept deliberately narrow:
//
//   - Implementations MUST be safe for concurrent use because a
//     single instance is shared across every request goroutine via
//     DefaultEncoder.
//   - Append takes buf by value and returns the grown slice, the
//     same convention stdlib's strconv.AppendInt et al. use.
//     Implementations MUST NOT retain buf after Append returns;
//     the buffer is owned by the caller's pool.
//   - Append MUST be deterministic and side-effect-free at the
//     process level. No logging, no metrics, no shared mutable
//     state — every piece of observability belongs at the layer
//     above (proxy Handler, transport, HTTP middleware).
//
// Package-private on purpose: until there is a second production
// implementation there is no external consumer with a justified
// reason to plug their own serializer in, and exporting the
// contract prematurely would commit the SDK to a stable public
// interface before any real-world codec-swap experience has
// informed its shape.
type envelopeSerializer interface {
	Append(buf []byte, env *GatewayRequest) []byte
}

// jsonEnvelopeSerializer is the default codec: the hand-rolled
// zero-allocation JSON emitter defined in envelope_encode.go.
//
// It is a zero-sized value type (empty struct), so storing it
// inside DefaultEncoder adds no struct padding, no heap allocation,
// and no per-request indirection beyond the single interface
// method dispatch that Go inlines into a direct call when the
// compiler can prove the concrete type at the call site.
type jsonEnvelopeSerializer struct{}

// Append delegates to appendEnvelopeJSON. Kept as a one-line
// forward so the serializer interface and the hand-rolled
// append function remain independently testable: the benchmarks
// in encoder_bench_test.go measure the path that goes through
// DefaultEncoder (and therefore this method), while
// envelope_encode_test.go calls appendEnvelopeJSON directly.
func (jsonEnvelopeSerializer) Append(buf []byte, env *GatewayRequest) []byte {
	return appendEnvelopeJSON(buf, env)
}

// Compile-time assertion that jsonEnvelopeSerializer satisfies
// the envelopeSerializer contract. Adding a new method to the
// interface fails the build here before any downstream encoder
// is affected.
var _ envelopeSerializer = jsonEnvelopeSerializer{}
