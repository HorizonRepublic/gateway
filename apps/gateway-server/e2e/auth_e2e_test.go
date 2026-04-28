//go:build e2e

package e2e

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// claimsShape mirrors the IClaims wire shape the verifier emits in
// the envelope.auth field. The handler echoes it via @GatewayUser.
type claimsShape struct {
	Sub   string   `json:"sub"`
	Roles []string `json:"roles"`
}

// meResponse is the body shape for GET /me.
type meResponse struct {
	RequestID string      `json:"requestId"`
	You       claimsShape `json:"you"`
}

// meSessionResponse is the body shape for GET /me-session.
type meSessionResponse struct {
	You claimsShape `json:"you"`
}

// articleResponse is the body shape for GET /articles/:id. viewer is
// null when the request is unauthenticated and the route is optional.
type articleResponse struct {
	ID     string       `json:"id"`
	Viewer *claimsShape `json:"viewer"`
}

// authedGet issues a GET against the gateway with an Authorization
// Bearer header. Tests that need an unauthenticated request use
// http.Get directly.
func authedGet(t *testing.T, path, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, GatewayURL(t)+path, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

func TestE2E_Auth_RequiredAuth_ValidToken(t *testing.T) {
	WaitReady(t)

	resp := authedGet(t, "/me", "demo-alice")
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var got meResponse
	readJSON(t, resp, &got)
	assert.Equal(t, "alice", got.You.Sub)
	assert.Equal(t, []string{"user"}, got.You.Roles)
}

func TestE2E_Auth_RequiredAuth_AdminToken(t *testing.T) {
	WaitReady(t)

	resp := authedGet(t, "/me", "demo-admin")
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var got meResponse
	readJSON(t, resp, &got)
	assert.Equal(t, "admin", got.You.Sub)
	assert.ElementsMatch(t, []string{"admin", "user"}, got.You.Roles,
		"admin token must surface both admin and user roles")
}

func TestE2E_Auth_RequiredAuth_MissingHeader(t *testing.T) {
	WaitReady(t)

	resp, err := http.Get(GatewayURL(t) + "/me")
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode,
		"missing Authorization header must surface as 401 from the verifier")
}

func TestE2E_Auth_RequiredAuth_UnknownToken(t *testing.T) {
	WaitReady(t)

	resp := authedGet(t, "/me", "demo-bad-jwt")
	t.Cleanup(func() { _ = resp.Body.Close() })

	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode,
		"token not in the lookup map must surface as 401")
}

func TestE2E_Auth_RequiredAuth_BannedToken(t *testing.T) {
	WaitReady(t)

	resp := authedGet(t, "/me", "demo-banned")
	t.Cleanup(func() { _ = resp.Body.Close() })

	assert.Equal(t, http.StatusForbidden, resp.StatusCode,
		"banned token must surface as 403 from the verifier")
}

func TestE2E_Auth_OptionalAuth_NoToken(t *testing.T) {
	WaitReady(t)

	resp, err := http.Get(GatewayURL(t) + "/articles/42")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode,
		"optional auth: missing token must NOT short-circuit, gateway proceeds")

	var got articleResponse
	readJSON(t, resp, &got)
	assert.Equal(t, "42", got.ID)
	assert.Nil(t, got.Viewer, "viewer must be null when no token present")
}

func TestE2E_Auth_OptionalAuth_ValidToken(t *testing.T) {
	WaitReady(t)

	resp := authedGet(t, "/articles/42", "demo-alice")
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var got articleResponse
	readJSON(t, resp, &got)
	assert.Equal(t, "42", got.ID)
	require.NotNil(t, got.Viewer, "viewer must be populated when valid token provided")
	assert.Equal(t, "alice", got.Viewer.Sub)
}

func TestE2E_Auth_OptionalAuth_BannedToken_StillForbidden(t *testing.T) {
	WaitReady(t)

	resp := authedGet(t, "/articles/42", "demo-banned")
	t.Cleanup(func() { _ = resp.Body.Close() })

	assert.Equal(t, http.StatusForbidden, resp.StatusCode,
		"optional:true must NOT swallow 403; banned tokens still short-circuit")
}

func TestE2E_Auth_ExplicitVerifierID_Session(t *testing.T) {
	WaitReady(t)

	resp := authedGet(t, "/me-session", "demo-alice")
	assert.Equal(t, http.StatusOK, resp.StatusCode,
		"explicit verifier:'session' must resolve to the session verifier")

	var got meSessionResponse
	readJSON(t, resp, &got)
	assert.Equal(t, "alice", got.You.Sub,
		"session verifier accepts the same token and returns the same claims as jwt")
}

func TestE2E_Auth_RequestIdRoundTrip(t *testing.T) {
	WaitReady(t)

	resp := authedGet(t, "/me", "demo-alice")
	headerRequestID := resp.Header.Get("X-Request-Id")
	require.NotEmpty(t, headerRequestID, "X-Request-Id must be on the response header")

	var got meResponse
	readJSON(t, resp, &got)
	assert.Equal(t, headerRequestID, got.RequestID,
		"@GatewayRequestId must surface the same id the gateway stamps on X-Request-Id")
}
