//go:build e2e

package e2e

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// findCookie returns the first Set-Cookie header value whose first
// segment ("name=value") is the named cookie, or "" if not present.
func findCookie(resp *http.Response, name string) string {
	for _, raw := range resp.Header.Values("Set-Cookie") {
		head := raw
		if semi := strings.IndexByte(raw, ';'); semi >= 0 {
			head = raw[:semi]
		}
		if eq := strings.IndexByte(head, '='); eq >= 0 && head[:eq] == name {
			return raw
		}
	}
	return ""
}

// hasCookieAttr reports whether the Set-Cookie line carries the given
// attribute (case-insensitive substring match against `; <attr>` or
// `; <attr>=`).
func hasCookieAttr(setCookie, attr string) bool {
	return strings.Contains(strings.ToLower(setCookie), strings.ToLower(attr))
}

// postJSON issues a POST with a JSON body. Tests that need a payload
// pass it; logout passes nil.
func postJSON(t *testing.T, path, jsonBody string) *http.Response {
	t.Helper()
	var body *bytes.Buffer
	if jsonBody != "" {
		body = bytes.NewBufferString(jsonBody)
	} else {
		body = bytes.NewBufferString("{}")
	}
	req, err := http.NewRequest(http.MethodPost, GatewayURL(t)+path, body)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

func TestE2E_Response_LoginSetsCookieWithFlags(t *testing.T) {
	WaitReady(t)

	resp := postJSON(t, "/auth/login", `{"name":"alice"}`)
	t.Cleanup(func() { _ = resp.Body.Close() })
	assert.Equal(t, http.StatusCreated, resp.StatusCode)

	sid := findCookie(resp, "sid")
	require.NotEmpty(t, sid, "Set-Cookie for sid must be present")
	assert.True(t, strings.HasPrefix(sid, "sid=alice-token"), "cookie value pinned (got %q)", sid)
	assert.True(t, hasCookieAttr(sid, "HttpOnly"), "HttpOnly missing: %q", sid)
	assert.True(t, hasCookieAttr(sid, "Secure"), "Secure missing: %q", sid)
	assert.True(t, hasCookieAttr(sid, "SameSite=Lax"), "SameSite=Lax missing: %q", sid)
	assert.True(t, hasCookieAttr(sid, "Path=/"), "Path=/ missing: %q", sid)
	assert.True(t, hasCookieAttr(sid, "Max-Age=3600"), "Max-Age=3600 missing: %q", sid)
}

func TestE2E_Response_LoginCookieMergesForRootDefaults(t *testing.T) {
	WaitReady(t)

	resp := postJSON(t, "/auth/login", `{"name":"alice"}`)
	t.Cleanup(func() { _ = resp.Body.Close() })

	sid := findCookie(resp, "sid")
	require.NotEmpty(t, sid)
	// Route-set field
	assert.True(t, hasCookieAttr(sid, "Max-Age=3600"),
		"route-set Max-Age must land on the wire")
	// forRoot defaults
	for _, want := range []string{"HttpOnly", "Secure", "SameSite=Lax", "Path=/"} {
		assert.True(t, hasCookieAttr(sid, want),
			"forRoot default %q must merge into route cookie: got %q", want, sid)
	}
}

func TestE2E_Response_LoginStrictOverridesSameSite(t *testing.T) {
	WaitReady(t)

	resp := postJSON(t, "/auth/login-strict", `{"name":"alice"}`)
	t.Cleanup(func() { _ = resp.Body.Close() })
	assert.Equal(t, http.StatusCreated, resp.StatusCode)

	sid := findCookie(resp, "sid")
	require.NotEmpty(t, sid)
	assert.True(t, hasCookieAttr(sid, "SameSite=Strict"),
		"per-route SameSite=Strict must override forRoot SameSite=Lax: %q", sid)
	assert.False(t, hasCookieAttr(sid, "SameSite=Lax"),
		"forRoot Lax must NOT survive when route specifies Strict: %q", sid)
	// Other defaults unchanged
	for _, want := range []string{"HttpOnly", "Secure", "Path=/"} {
		assert.True(t, hasCookieAttr(sid, want),
			"non-overridden forRoot default %q must still apply: %q", want, sid)
	}
}

func TestE2E_Response_LogoutDeletesCookie(t *testing.T) {
	WaitReady(t)

	resp := postJSON(t, "/auth/logout", "")
	t.Cleanup(func() { _ = resp.Body.Close() })
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	sid := findCookie(resp, "sid")
	require.NotEmpty(t, sid, "Set-Cookie for sid must be present on logout")
	assert.True(t, hasCookieAttr(sid, "Max-Age=0"),
		"deletion uses Max-Age=0: %q", sid)
	assert.True(t, hasCookieAttr(sid, "Expires="),
		"deletion uses Expires in the past: %q", sid)
}

func TestE2E_Response_RedirectReturns302WithLocation(t *testing.T) {
	WaitReady(t)

	// Disable automatic redirect-following so we can assert the 302.
	client := &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, err := http.NewRequest(http.MethodGet, GatewayURL(t)+"/auth/google/start", nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	assert.Equal(t, http.StatusFound, resp.StatusCode, "302 Found expected")
	assert.Contains(t, resp.Header.Get("Location"), "accounts.google.com/o/oauth2/v2/auth",
		"Location header must point at the OAuth provider")
}

func TestE2E_Response_CustomHeadersSurface(t *testing.T) {
	WaitReady(t)

	req, err := http.NewRequest(http.MethodGet, GatewayURL(t)+"/auth/echo-headers", nil)
	require.NoError(t, err)
	req.Header.Set("X-Trace-Id", "t-42")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "horizon", resp.Header.Get("X-Custom"),
		"@GatewayResponse.header() must surface a custom response header")
	assert.Equal(t, "t-42", resp.Header.Get("X-Trace"),
		"X-Trace must echo the inbound X-Trace-Id read by @GatewayHeader")
}

func TestE2E_Response_CustomStatusViaResponseBuilder(t *testing.T) {
	WaitReady(t)

	resp := postJSON(t, "/auth/login", `{"name":"bob"}`)
	t.Cleanup(func() { _ = resp.Body.Close() })

	// /auth/login has no statusCode set on @GatewayRoute — the 201
	// comes from the in-handler `res.status(201)` call. Pinning it
	// here proves the accumulator-driven status wins over the SDK's
	// default status resolver for non-null body.
	assert.Equal(t, http.StatusCreated, resp.StatusCode,
		"res.status(201) on the accumulator must surface as the wire status")
}
