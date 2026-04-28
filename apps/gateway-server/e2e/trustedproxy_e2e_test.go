//go:build e2e

package e2e

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ipShape mirrors the body shape of GET /__tp/ip in example-app's
// trustedproxy module: the route returns { ip: meta.remoteAddr }.
type ipShape struct {
	IP string `json:"ip"`
}

// echoIP issues a GET /__tp/ip against baseURL with the supplied
// header, decodes the body, and returns the resolved IP. The header
// argument is the one the gateway under test trusts (X-Forwarded-For
// for the primary, X-Real-IP for the realip replica). Unset headers
// are skipped.
func echoIP(t *testing.T, baseURL, headerName, headerValue string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, baseURL+"/__tp/ip", nil)
	require.NoError(t, err)
	if headerName != "" {
		req.Header.Set(headerName, headerValue)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusOK, resp.StatusCode, "/__tp/ip must serve")

	var got ipShape
	readJSON(t, resp, &got)
	return got.IP
}

func TestE2E_TrustedProxy_XFFRightmostUntrusted(t *testing.T) {
	WaitReady(t)

	// XFF carries 9.9.9.9 (untrusted public-shaped IP) followed by
	// 172.31.0.5 (in trusted CIDR). The rightmost-untrusted walk
	// scans right-to-left, skips the trusted entry, and returns the
	// next one — 9.9.9.9.
	got := echoIP(t, GatewayURL(t), "X-Forwarded-For", "9.9.9.9, 172.31.0.5")
	assert.Equal(t, "9.9.9.9", got,
		"rightmost-untrusted XFF walk must return the first non-trusted entry")
}

func TestE2E_TrustedProxy_XRealIPHonoursAltHeader(t *testing.T) {
	WaitReadyAt(t, GatewayURLRealIP(t))

	// Replica with TRUSTED_PROXY_HEADER=X-Real-IP. The peer (docker
	// bridge) is in the trusted set, so the resolver takes the
	// header value verbatim.
	got := echoIP(t, GatewayURLRealIP(t), "X-Real-IP", "8.8.8.8")
	assert.Equal(t, "8.8.8.8", got,
		"X-Real-IP single-value form must resolve verbatim when peer is trusted")

	// Sanity check the header knob: the same X-Real-IP request
	// against the primary (which trusts X-Forwarded-For) must NOT
	// resolve to 8.8.8.8 — the primary ignores X-Real-IP entirely.
	WaitReady(t)
	gotPrimary := echoIP(t, GatewayURL(t), "X-Real-IP", "8.8.8.8")
	assert.NotEqual(t, "8.8.8.8", gotPrimary,
		"primary gateway trusts X-Forwarded-For; X-Real-IP must be invisible to it")
}

func TestE2E_TrustedProxy_EmptyTrustSetIgnoresXFF(t *testing.T) {
	WaitReadyAt(t, GatewayURLNoTrust(t))

	// Empty trust set ⇒ peer is never trusted ⇒ XFF is always
	// ignored (the spoofing defence). The resolver returns the peer
	// IP. We assert non-equality with the spoof rather than equality
	// with a docker-bridge address because that address varies
	// across hosts (Linux vs Docker Desktop on macOS).
	got := echoIP(t, GatewayURLNoTrust(t), "X-Forwarded-For", "1.2.3.4")
	assert.NotEqual(t, "1.2.3.4", got,
		"empty TRUSTED_PROXIES must reject XFF spoof; resolver returns peer IP")
	assert.NotEmpty(t, got, "resolver must still emit something — the peer IP")
}
