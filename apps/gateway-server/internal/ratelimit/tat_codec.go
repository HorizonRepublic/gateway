package ratelimit

import (
	"encoding/binary"
	"fmt"
	"time"
)

const (
	// tatVersion1 is the version byte prefix in the current TAT
	// encoding. Future versions MUST bump this constant and the
	// decoder MUST dispatch on the byte. Callers MUST NOT peek past
	// the version byte without checking it first.
	tatVersion1 byte = 0x01

	// tatEncodedLength is the fixed wire length of a version-1
	// encoded TAT value: 1 version byte + 8 bytes of
	// big-endian int64 nanoseconds.
	tatEncodedLength = 9
)

// encodeTAT serializes a TAT timestamp for storage in a NATS KV
// bucket as [version(1 byte)][UnixNano int64 big-endian(8 bytes)].
//
// The version byte reserves forward-compatibility room for future
// state expansion (e.g., adding createdAt or a config hash without
// breaking existing buckets). Callers MUST use decodeTAT to read
// values back — never reach past the version byte directly.
//
// Requires a real, post-1970 timestamp. Calling with time.Time{}
// produces an undefined round-trip (time.Unix cannot reconstruct
// year-0001 from its UnixNano representation); callers MUST NOT do so.
func encodeTAT(tat time.Time) []byte {
	out := make([]byte, tatEncodedLength)
	out[0] = tatVersion1
	binary.BigEndian.PutUint64(out[1:], uint64(tat.UnixNano()))

	return out
}

// decodeTAT parses a TAT byte sequence produced by encodeTAT. Returns
// an error for wrong length or unknown version byte — callers MUST
// treat a decode error as "bucket data is corrupt" and fall back to
// the fresh-bucket path (currentTAT = zero time).
func decodeTAT(b []byte) (time.Time, error) {
	if len(b) != tatEncodedLength {
		return time.Time{}, fmt.Errorf("ratelimit: TAT length got %d want %d", len(b), tatEncodedLength)
	}

	if b[0] != tatVersion1 {
		return time.Time{}, fmt.Errorf("ratelimit: TAT unknown version 0x%02x", b[0])
	}

	ns := int64(binary.BigEndian.Uint64(b[1:]))

	return time.Unix(0, ns), nil
}
