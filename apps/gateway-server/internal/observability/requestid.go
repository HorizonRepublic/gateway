package observability

import (
	"crypto/rand"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

// requestIDMu guards the shared monotonic ULID entropy source. The
// source itself is not goroutine-safe, so every call to NewRequestID
// funnels through this mutex. At realistic request rates the critical
// section is on the order of tens of nanoseconds and the mutex is
// essentially uncontended — this does NOT become a bottleneck on the
// hot path, even at the target 100k RPS.
var (
	requestIDMu     sync.Mutex
	requestIDSource = ulid.Monotonic(rand.Reader, 0)
)

// NewRequestID returns a newly generated monotonic ULID formatted as
// its 26-character canonical Crockford base32 string.
//
// ULIDs are used instead of UUIDs because they sort lexicographically
// by creation time, which makes grep-ing logs for "all requests after
// 01HXY..." trivial and lets downstream systems (Loki, Elasticsearch)
// exploit the ordering for efficient range queries. Monotonic entropy
// within the same millisecond guarantees strictly-increasing IDs for
// rapid-fire request bursts, even when the wall clock has not
// advanced — so sorting by ID and sorting by true arrival order agree.
//
// The returned string is always exactly 26 ASCII characters and is
// safe to use as an HTTP header value, log field, or database primary
// key without any additional escaping.
func NewRequestID() string {
	requestIDMu.Lock()
	defer requestIDMu.Unlock()
	return ulid.MustNew(ulid.Timestamp(time.Now()), requestIDSource).String()
}
