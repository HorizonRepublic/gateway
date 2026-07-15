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
//
// requestIDSource is reseeded under the same mutex whenever a read
// fails: same-millisecond monotonic overflow leaves the source's
// internal counter wrapped (oklog/ulid v2 MonotonicRead mutates the
// entropy even on ErrMonotonicOverflow), so the exhausted source
// cannot be reused for the rest of that millisecond.
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
// The monotonic source can overflow: within one millisecond each ID
// increments the 80-bit entropy by a random value in [1, 2^32), and
// an increment past the maximum makes oklog/ulid return
// ErrMonotonicOverflow. The probability is on the order of 2^-49 per
// same-millisecond pair — vanishingly rare per request, but nonzero
// integrated over fleet-years, and a panic here would turn one
// unlucky request into a recovery-path 500. On any read failure the
// ID for that call degrades to fresh non-monotonic random entropy
// (strict same-millisecond ordering yields to availability) and the
// shared source is reseeded so subsequent calls regain monotonicity.
// This function never panics.
//
// The returned string is always exactly 26 ASCII characters and is
// safe to use as an HTTP header value, log field, or database primary
// key without any additional escaping.
func NewRequestID() string {
	now := ulid.Timestamp(time.Now())

	requestIDMu.Lock()
	id, err := ulid.New(now, requestIDSource)
	if err != nil {
		// The overflowed source's counter has wrapped and cannot be
		// reused this millisecond — replace it for subsequent calls.
		requestIDSource = ulid.Monotonic(rand.Reader, 0)
	}
	requestIDMu.Unlock()

	if err != nil {
		// Degrade this one ID to fresh non-monotonic entropy. The
		// timestamp bytes are already set: ulid.New writes them before
		// touching entropy, and its SetTime error path is unreachable
		// for wall-clock input (ErrBigTime starts at year 10889).
		// crypto/rand.Read never returns an error (Go ≥ 1.24 crashes
		// the program irrecoverably if OS entropy fails), and
		// SetEntropy only rejects slices that are not 10 bytes.
		var entropy [10]byte
		_, _ = rand.Read(entropy[:])
		_ = id.SetEntropy(entropy[:])
	}

	return id.String()
}
