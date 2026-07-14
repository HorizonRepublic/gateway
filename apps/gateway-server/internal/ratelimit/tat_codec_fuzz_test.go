package ratelimit

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// FuzzDecodeTAT drives the KV wire decoder with arbitrary bytes. TAT
// values live in a NATS KV bucket that other processes can write to,
// so the decoder must treat every byte sequence as potentially
// corrupt.
//
// Invariants:
//
//  1. decodeTAT never panics.
//  2. Decode succeeds exactly when the input is 9 bytes long and
//     carries the version-1 prefix; anything else is a typed error
//     (the caller's "bucket data is corrupt" signal).
//  3. encode(decode(b)) == b byte-for-byte: a decoded value re-encodes
//     to the identical wire form, so a read-modify-write cycle never
//     silently rewrites stored state.
func FuzzDecodeTAT(f *testing.F) {
	seeds := [][]byte{
		{},
		{0x01},
		// Valid version-1 encodings: zero, positive, negative payloads.
		{0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
		{0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01},
		{0x01, 0x7f, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		{0x01, 0x80, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
		{0x01, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		// Unknown version bytes with otherwise plausible payloads.
		{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
		{0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01},
		{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		// Wrong lengths around the valid one.
		{0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
		{0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		tat, err := decodeTAT(data)

		wellFormed := len(data) == tatEncodedLength && data[0] == tatVersion1
		if !wellFormed {
			require.Error(t, err, "malformed input %x must be rejected", data)

			return
		}

		require.NoError(t, err, "well-formed input %x must decode", data)
		require.Equal(t, data, encodeTAT(tat),
			"encode(decode(x)) must reproduce the wire bytes exactly")
	})
}

// FuzzEncodeTATRoundTrip drives the encode direction across the full
// int64 nanosecond domain. time.Unix(0, ns) is a fully defined
// constructor for any int64, so the encoded form must decode back to
// the identical instant with no precision loss anywhere in the range.
func FuzzEncodeTATRoundTrip(f *testing.F) {
	seeds := []int64{
		0,
		1,
		-1,
		time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC).UnixNano(),
		9223372036854775807,
		-9223372036854775808,
		999999999,
		-999999999,
		1000000000,
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, nanos int64) {
		encoded := encodeTAT(time.Unix(0, nanos))
		require.Len(t, encoded, tatEncodedLength)
		require.Equal(t, tatVersion1, encoded[0])

		decoded, err := decodeTAT(encoded)
		require.NoError(t, err)
		require.Equal(t, nanos, decoded.UnixNano(),
			"decode(encode(t)) must preserve the instant exactly")
	})
}
