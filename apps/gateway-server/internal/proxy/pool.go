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
// an initial grow; the serializer's appends reallocate automatically
// if a specific request exceeds this, and the grown capacity is
// preserved across pool cycles (up to maxRetainedPayloadCap — see
// releasePayload) by storing a pointer to the slice.
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

// releaseEnvelope resets an envelope and returns it to the pool. It
// is safe to call with a nil receiver to simplify defer statements.
//
// The reset happens on release, not only on acquire, because a
// pooled envelope otherwise retains live references to the last
// request's Body bytes, Auth claims, and header strings — including
// raw Authorization bearer tokens on the verifier path — for an
// unbounded idle period between requests. Credentials pinned in
// pooled memory outlive the request they belong to and surface in
// heap dumps or through any memory-disclosure bug. Acquire still
// resets defensively (double reset of an already-clean envelope is
// map iteration over empty maps — effectively free) so a stray Put
// of a dirty envelope from future code cannot leak state into a
// request.
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

// maxRetainedPayloadCap bounds the capacity of payload buffers the
// pool retains. One multi-megabyte request body would otherwise grow
// a pooled buffer permanently — N pool entries each pinning the
// largest body they ever saw inflates steady-state RSS long after
// the burst that caused it. 64 KiB comfortably covers the typical
// envelope (headers + small JSON body, see initialPayloadCap) while
// letting rare oversized buffers fall to the GC.
const maxRetainedPayloadCap = 64 * 1024

// releasePayload returns a payload buffer to the pool, dropping
// buffers grown beyond maxRetainedPayloadCap so a burst of large
// bodies cannot permanently inflate the pool's memory footprint.
// The retained buffer is truncated to zero length on release; the
// backing array is NOT zeroed because the payload is re-sliced to
// [:0] and fully overwritten by the next encode before any read —
// unlike the envelope's reference fields, stale payload bytes are
// unreachable through the pool's API.
//
// Safe with a nil pointer so defer statements can unconditionally
// release.
func releasePayload(buf *[]byte) {
	if buf == nil || !shouldRetainPayload(cap(*buf)) {
		return
	}
	*buf = (*buf)[:0]
	payloadPool.Put(buf)
}

// shouldRetainPayload decides whether a released payload buffer of
// the given capacity goes back into the pool. Split out from
// releasePayload so the retention policy is unit-testable without
// poking at sync.Pool internals.
func shouldRetainPayload(capacity int) bool {
	return capacity <= maxRetainedPayloadCap
}
