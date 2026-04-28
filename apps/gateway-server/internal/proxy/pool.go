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

// releaseEnvelope returns an envelope to the pool. It is safe to call
// with a nil receiver to simplify defer statements.
func releaseEnvelope(envelope *GatewayRequest) {
	if envelope == nil {
		return
	}
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
func releasePayload(buf *[]byte) {
	if buf == nil {
		return
	}
	payloadPool.Put(buf)
}
