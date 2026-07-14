package proxy

import (
	"sync"
)

// Initial map capacities for pooled GatewayRequest instances. These
// numbers track the 95th percentile of typical requests observed in
// load-test fixtures and are tuned to avoid a realloc in the common
// case while keeping the steady-state pool footprint small.
const (
	initialParamsCap  = 4
	initialQueryCap   = 4
	initialHeadersCap = 16
)

// initialPayloadCap is the pre-allocated capacity of pooled scratch
// []byte slices used for envelope marshalling. 1 KiB covers the
// typical envelope footprint (route + small body + headers) without
// an initial grow; sonic's EncodeInto reallocates automatically if a
// specific request exceeds this, and the grown capacity is preserved
// across pool cycles by storing a pointer to the slice.
const initialPayloadCap = 1024

// maxPooledPayloadCap bounds the capacity of buffers returned to the
// pool. Growth up to this cap is preserved across cycles so the
// steady-state working set pays zero allocations; beyond it the
// buffer is dropped on release and reclaimed by GC. Without the cap a
// single multi-megabyte request body pins its backing array in the
// pool indefinitely — N pool entries times the largest body ever seen
// is unbounded RSS for no steady-state benefit. 64 KiB clears the
// typical envelope P99 by well over an order of magnitude.
const maxPooledPayloadCap = 64 << 10

// envelopePool reuses GatewayRequest instances across requests. Every
// acquired envelope is reset before use by acquireEnvelope and must be
// returned via releaseEnvelope once the NATS reply has been processed.
var envelopePool = sync.Pool{
	New: func() any {
		return &GatewayRequest{
			Params:  make(map[string]string, initialParamsCap),
			Query:   make(map[string]QueryValue, initialQueryCap),
			Headers: make(map[string]string, initialHeadersCap),
		}
	},
}

// acquireEnvelope fetches a pooled GatewayRequest and resets it so
// callers observe a zero-valued struct regardless of its prior history.
// The returned pointer MUST be released via releaseEnvelope on every
// code path — including error paths — or the pool footprint grows
// without bound.
func acquireEnvelope() *GatewayRequest {
	envelope, _ := envelopePool.Get().(*GatewayRequest)
	envelope.reset()
	return envelope
}

// releaseEnvelope resets an envelope and returns it to the pool. It
// is safe to call with a nil receiver to simplify defer statements.
//
// The reset happens on RELEASE (in addition to the acquire-side reset
// that guards correctness) so an idle pooled envelope never retains
// references to the last request's body, verifier claims, or header
// strings — the verifier-path Headers map carries the raw
// Authorization bearer token, and pinning credentials in pool memory
// between requests widens the blast radius of any heap-disclosure
// bug. The double reset is nearly free: after the release-side clear,
// the acquire-side reset iterates empty maps.
func releaseEnvelope(envelope *GatewayRequest) {
	if envelope == nil {
		return
	}
	envelope.reset()
	envelopePool.Put(envelope)
}

// payloadPool reuses append-style scratch buffers across requests.
// Stores a pointer to a bare []byte slice so sonic.EncodeInto can
// append directly into the pooled backing array.
//
// Using a pointer is mandatory: storing a bare []byte in sync.Pool
// would lose any capacity grow that happened during encoding because
// the returned slice header is not the same value that was Put into
// the pool. Storing *[]byte preserves the grown capacity across
// acquire/release cycles — the pointer value is stable even when the
// slice header it points at has been reallocated.
var payloadPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 0, initialPayloadCap)
		return &buf
	},
}

// acquirePayload fetches a pooled scratch buffer reset to zero
// length. The underlying backing array retains whatever capacity
// previous users grew it to, so steady-state encodes pay zero
// allocations once the pool has warmed up to the typical envelope
// size.
//
// The returned pointer MUST be released via releasePayload on every
// code path — including error paths — or the pool footprint grows
// without bound.
func acquirePayload() *[]byte {
	ptr, _ := payloadPool.Get().(*[]byte)
	*ptr = (*ptr)[:0]
	return ptr
}

// releasePayload returns a payload buffer to the pool. Safe with a
// nil pointer so defer statements can unconditionally release.
// Buffers grown beyond maxPooledPayloadCap are dropped instead of
// pooled — see the constant's documentation for the retention
// rationale.
func releasePayload(buf *[]byte) {
	if buf == nil {
		return
	}
	if cap(*buf) > maxPooledPayloadCap {
		return
	}
	payloadPool.Put(buf)
}
