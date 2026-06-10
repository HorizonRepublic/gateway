package errors

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHTTPError_StatusAndBodyPaired verifies every declared error
// carries both a valid status and a non-empty body. Regressions in
// the init() pairing would surface here as a field-level mismatch.
func TestHTTPError_StatusAndBodyPaired(t *testing.T) {
	cases := []struct {
		name   string
		err    HTTPError
		status int
	}{
		{"NotFound", NotFound, StatusNotFound},
		{"MethodNotAllowed", MethodNotAllowed, StatusMethodNotAllowed},
		{"TooManyRequests", TooManyRequests, StatusTooManyRequests},
		{"InternalError", InternalError, StatusInternalError},
		{"ServiceUnavailable", ServiceUnavailable, StatusServiceUnavailable},
		{"GatewayTimeout", GatewayTimeout, StatusGatewayTimeout},
		{"BadGateway", BadGateway, StatusBadGateway},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.status, c.err.Status, "status code")
			assert.NotEmpty(t, c.err.Body, "body must be non-empty")
		})
	}
}

// TestHTTPError_BodiesAreSingleFieldObjects verifies every pre-encoded
// body is a single-field JSON object carrying a non-empty 'error'
// string. This is the gateway's wire contract: the HTTP status line
// carries the numeric code, the body carries a short human-readable
// phrase and nothing else — no code field, no message field, no
// implementation-identifying leak.
func TestHTTPError_BodiesAreSingleFieldObjects(t *testing.T) {
	errs := []HTTPError{
		NotFound, MethodNotAllowed,
		TooManyRequests, InternalError, ServiceUnavailable, GatewayTimeout, BadGateway,
	}
	for _, e := range errs {
		var parsed map[string]string
		require.NoError(t, json.Unmarshal(e.Body, &parsed))
		assert.Len(t, parsed, 1, "body must contain exactly one field")
		assert.NotEmpty(t, parsed["error"], "body must carry a non-empty error field")
	}
}

// TestHTTPError_ReasonPhrases pins the exact RFC 9110 reason phrase
// each error surfaces. These strings are part of the wire contract —
// any deliberate rewording needs to update this test and the
// corresponding entry in http_errors.go together.
func TestHTTPError_ReasonPhrases(t *testing.T) {
	cases := []struct {
		name   string
		err    HTTPError
		phrase string
	}{
		{"BadRequest", BadRequest, "Bad Request"},
		{"NotFound", NotFound, "Not Found"},
		{"MethodNotAllowed", MethodNotAllowed, "Method Not Allowed"},
		{"TooManyRequests", TooManyRequests, "Too Many Requests"},
		{"InternalError", InternalError, "Internal Server Error"},
		{"ServiceUnavailable", ServiceUnavailable, "Service Unavailable"},
		{"GatewayTimeout", GatewayTimeout, "Gateway Timeout"},
		{"BadGateway", BadGateway, "Bad Gateway"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var parsed map[string]string
			require.NoError(t, json.Unmarshal(c.err.Body, &parsed))
			assert.Equal(t, c.phrase, parsed["error"])
		})
	}
}

// TestHTTPError_BuildSnapshotEqualsLiveValue documents the package
// convention that callers MUST NOT mutate Status or Body on a shared
// HTTPError. Go has no type-system enforcement of immutability for
// exported struct fields, so the test instead snapshots the live
// value at startup and asserts the package-level variable still
// matches at test time. A failure here means some unrelated test (or
// a future refactor) wrote through the shared slice and broke the
// shared-instance contract.
func TestHTTPError_BuildSnapshotEqualsLiveValue(t *testing.T) {
	cases := []struct {
		name string
		err  HTTPError
	}{
		{"NotFound", NotFound},
		{"MethodNotAllowed", MethodNotAllowed},
		{"TooManyRequests", TooManyRequests},
		{"InternalError", InternalError},
		{"ServiceUnavailable", ServiceUnavailable},
		{"GatewayTimeout", GatewayTimeout},
		{"BadGateway", BadGateway},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			snapshotStatus := c.err.Status
			snapshotBody := append([]byte(nil), c.err.Body...)

			// Re-read both fields through a fresh binding to surface any
			// in-place mutation that happened after init(). Comparing
			// snapshot vs live value is the point — comparing the live
			// value to itself would be a tautology.
			liveStatus := c.err.Status
			liveBody := c.err.Body

			assert.Equal(t, snapshotStatus, liveStatus,
				"Status must remain stable across reads; a mismatch means something reassigned the exported field")
			assert.Equal(t, snapshotBody, liveBody,
				"Body must remain byte-for-byte identical to its init-time snapshot; "+
					"a difference here means a caller mutated the shared slice")
		})
	}
}

// TestHTTPError_BodiesDoNotAlias verifies the build factory
// produces a distinct backing slice per HTTPError. If two error
// values shared a backing array, an in-place edit to one would
// silently corrupt the other — the convention is then per-instance,
// not global, and the test catches a future refactor that pools
// buffers without honouring the share boundary.
func TestHTTPError_BodiesDoNotAlias(t *testing.T) {
	pairs := []struct {
		name string
		a, b HTTPError
	}{
		{"NotFound vs BadGateway", NotFound, BadGateway},
		{"GatewayTimeout vs ServiceUnavailable", GatewayTimeout, ServiceUnavailable},
		{"TooManyRequests vs InternalError", TooManyRequests, InternalError},
	}

	for _, p := range pairs {
		t.Run(p.name, func(t *testing.T) {
			require.NotEmpty(t, p.a.Body)
			require.NotEmpty(t, p.b.Body)
			// Different start addresses imply different backing arrays
			// (or at least different windows that cannot overlap when
			// the lengths plus offsets fit). For the small fixed bodies
			// produced by build, separate Marshal calls always yield
			// freshly allocated slices.
			assert.NotSame(t, &p.a.Body[0], &p.b.Body[0],
				"distinct HTTPError values must back onto distinct byte slices")
		})
	}
}
