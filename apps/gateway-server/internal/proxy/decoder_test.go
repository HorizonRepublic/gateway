package proxy

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultDecoder_ValidReply(t *testing.T) {
	dec := NewDefaultDecoder()

	reply, err := dec.Decode(
		[]byte(`{"status":201,"headers":{"x-foo":["bar"],"set-cookie":["a=1","b=2"]},"body":{"id":"42"}}`),
	)
	require.NoError(t, err)

	assert.Equal(t, 201, reply.Status)
	assert.Equal(t, []string{"bar"}, reply.Headers["x-foo"])
	// Multi-value headers must decode verbatim so Set-Cookie lines
	// preserve order as emitted by the handler — order matters for
	// cookies that set the same name with different paths.
	assert.Equal(t, []string{"a=1", "b=2"}, reply.Headers["set-cookie"])
	assert.JSONEq(t, `{"id":"42"}`, string(reply.Body))
}

func TestDefaultDecoder_RejectsInvalidJSON(t *testing.T) {
	dec := NewDefaultDecoder()

	_, err := dec.Decode([]byte(`not json`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "proxy decoder unmarshal")
}

func TestDefaultDecoder_RejectsOutOfRangeStatus(t *testing.T) {
	dec := NewDefaultDecoder()

	// 1xx statuses are interim responses (RFC 9110 §15.2) — never a
	// valid FINAL status for a proxied exchange. Writing one as the
	// sole response makes standards-compliant clients wait for the
	// real response until their own timeout, so the decoder rejects
	// them into the same fail-closed 502 path as other garbage.
	for _, status := range []int{0, 99, 100, 101, 199, 600, 999} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			payload := []byte(`{"status":` + strconv.Itoa(status) + `,"headers":{},"body":null}`)
			_, err := dec.Decode(payload)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "invalid status")
		})
	}
}

func TestDefaultDecoder_AcceptsStatusBoundaries(t *testing.T) {
	dec := NewDefaultDecoder()

	for _, status := range []int{minValidHTTPStatus, maxValidHTTPStatus} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			payload := []byte(`{"status":` + strconv.Itoa(status) + `,"headers":{},"body":null}`)
			reply, err := dec.Decode(payload)
			require.NoError(t, err)
			assert.Equal(t, status, reply.Status)
		})
	}
}
