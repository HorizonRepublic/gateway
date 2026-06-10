//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// readSessionCookie probes GET /cookies/session with a raw Cookie
// header and returns the value the `@GatewayCookie('session')`
// decorator extracted (null ⇒ empty string plus ok=false).
func readSessionCookie(t *testing.T, rawCookie string) (string, bool) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, GatewayURL(t)+"/cookies/session", nil)
	require.NoError(t, err)
	if rawCookie != "" {
		req.Header.Set("Cookie", rawCookie)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var got struct {
		Value *string `json:"value"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	if got.Value == nil {
		return "", false
	}
	return *got.Value, true
}

// TestE2E_Cookie_SingleCookieReadByName pins the basic read path: a
// single `Cookie: session=...` pair survives the gateway's header
// forwarding and lands in `@GatewayCookie('session')` verbatim.
func TestE2E_Cookie_SingleCookieReadByName(t *testing.T) {
	WaitReady(t)

	value, ok := readSessionCookie(t, "session=abc123")
	require.True(t, ok)
	assert.Equal(t, "abc123", value)
}

// TestE2E_Cookie_MultiCookieSemicolonSplit pins RFC 6265 §5.4
// multi-pair parsing on the wire: the named cookie is extracted from
// the middle of a semicolon-separated header, neighbours ignored.
func TestE2E_Cookie_MultiCookieSemicolonSplit(t *testing.T) {
	WaitReady(t)

	value, ok := readSessionCookie(t, "a=1; session=xyz-789; b=2")
	require.True(t, ok)
	assert.Equal(t, "xyz-789", value)
}

// TestE2E_Cookie_DuplicateNameFirstWins pins the documented
// duplicate-name rule: when a client sends the same cookie name
// twice, the FIRST occurrence wins. Browsers order cookies by
// path-specificity, so first-wins matches the most-specific cookie —
// and a deterministic rule here is what the rate-limit cookie
// collision counter relies on.
func TestE2E_Cookie_DuplicateNameFirstWins(t *testing.T) {
	WaitReady(t)

	value, ok := readSessionCookie(t, "session=first; session=second")
	require.True(t, ok)
	assert.Equal(t, "first", value)
}

// TestE2E_Cookie_AbsentNameReadsNull pins the absent branch: no
// Cookie header at all ⇒ the decorator yields undefined ⇒ the route
// echoes null. Guards against a parser regression that would turn
// "absent" into an empty string or a throw.
func TestE2E_Cookie_AbsentNameReadsNull(t *testing.T) {
	WaitReady(t)

	_, ok := readSessionCookie(t, "")
	assert.False(t, ok, "absent cookie must surface as null, not a value")
}
