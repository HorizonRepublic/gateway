//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const userResetSubpath = "/__e2e/reset"

// withCleanState issues POST /__e2e/reset against example-app's
// host-exposed health port BEFORE fn runs. Tests that mutate users
// state (POST/PATCH/DELETE) wrap their body in withCleanState so the
// seeded fixture set is restored between runs.
func withCleanState(t *testing.T, fn func()) {
	t.Helper()
	resetURL := exampleAppHealthURL(t) + userResetSubpath
	req, err := http.NewRequest(http.MethodPost, resetURL, nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode, "reset must succeed before test body")
	fn()
}

// userShape mirrors the User wire shape the controller emits.
// Optional fields surface only when the relevant decorator is exercised.
type userShape struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Role   string `json:"role"`
	Trace  string `json:"trace,omitempty"`
	Reason string `json:"reason,omitempty"`
}

func readJSON(t *testing.T, resp *http.Response, target any) {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	require.NoError(t, json.NewDecoder(resp.Body).Decode(target))
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(b)
}

func TestE2E_Core_GetUserByID(t *testing.T) {
	WaitReady(t)

	resp, err := http.Get(GatewayURL(t) + "/users/alice")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.NotEmpty(t, resp.Header.Get("X-Request-Id"), "X-Request-Id must be stamped on every response")

	var got userShape
	readJSON(t, resp, &got)
	assert.Equal(t, "alice", got.ID)
	assert.Equal(t, "Alice", got.Name)
	assert.Equal(t, "admin", got.Role)
}

func TestE2E_Core_GetUnknownUser(t *testing.T) {
	WaitReady(t)

	resp, err := http.Get(GatewayURL(t) + "/users/ghost")
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode,
		"NotFoundException from handler must surface as 404 via GatewayExceptionFilter")
	body := readBody(t, resp)
	assert.Contains(t, body, "ghost", "error body should reference the missing id")
}

func TestE2E_Core_CreateUserReturns201(t *testing.T) {
	WaitReady(t)
	withCleanState(t, func() {
		body := bytes.NewBufferString(`{"name":"E2E"}`)
		req, err := http.NewRequest(http.MethodPost, GatewayURL(t)+"/users", body)
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		assert.Equal(t, http.StatusCreated, resp.StatusCode,
			"explicit statusCode:201 on @GatewayRoute must surface")

		var got userShape
		readJSON(t, resp, &got)
		assert.Equal(t, "E2E", got.Name)
		assert.Equal(t, "user", got.Role, "role defaults to 'user' when omitted from body")
		assert.NotEmpty(t, got.ID, "service must assign an id")
	})
}

func TestE2E_Core_DeleteUserReturns204(t *testing.T) {
	WaitReady(t)
	withCleanState(t, func() {
		req, err := http.NewRequest(http.MethodDelete, GatewayURL(t)+"/users/alice", nil)
		require.NoError(t, err)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		assert.Equal(t, http.StatusNoContent, resp.StatusCode,
			"void return on @GatewayRoute defaults to 204")

		body := readBody(t, resp)
		assert.Empty(t, body, "204 must carry an empty body")
	})
}

func TestE2E_Core_ListUsers(t *testing.T) {
	WaitReady(t)
	withCleanState(t, func() {
		resp, err := http.Get(GatewayURL(t) + "/users")
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var users []userShape
		readJSON(t, resp, &users)
		require.GreaterOrEqual(t, len(users), 3, "seeded fixture set must surface")

		ids := make([]string, 0, len(users))
		for _, u := range users {
			ids = append(ids, u.ID)
		}
		sort.Strings(ids)
		assert.Contains(t, ids, "alice")
		assert.Contains(t, ids, "bob")
		assert.Contains(t, ids, "charlie")
	})
}

func TestE2E_Core_ListUsersWithSingleQuery(t *testing.T) {
	WaitReady(t)
	withCleanState(t, func() {
		resp, err := http.Get(GatewayURL(t) + "/users?role=admin")
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var users []userShape
		readJSON(t, resp, &users)
		require.NotEmpty(t, users)
		for _, u := range users {
			assert.Equal(t, "admin", u.Role, "single-value query filter must apply")
		}
	})
}

func TestE2E_Core_ListUsersWithMultiValueQuery(t *testing.T) {
	WaitReady(t)
	withCleanState(t, func() {
		resp, err := http.Get(GatewayURL(t) + "/users?role=admin&role=user")
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var users []userShape
		readJSON(t, resp, &users)
		require.GreaterOrEqual(t, len(users), 3,
			"multi-value query filter must include all matching roles")

		roles := make(map[string]struct{})
		for _, u := range users {
			roles[u.Role] = struct{}{}
		}
		assert.Contains(t, roles, "admin")
		assert.Contains(t, roles, "user")
	})
}

func TestE2E_Core_PatchUserMixedDecorators(t *testing.T) {
	WaitReady(t)
	withCleanState(t, func() {
		body := bytes.NewBufferString(`{"role":"user"}`)
		req, err := http.NewRequest(http.MethodPatch,
			GatewayURL(t)+"/users/alice?reason=audit", body)
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var got userShape
		readJSON(t, resp, &got)
		assert.Equal(t, "alice", got.ID)
		assert.Equal(t, "user", got.Role, "PATCH body must apply role change")
		assert.Equal(t, "audit", got.Reason,
			"@GatewayQuery('reason') must surface in the response body")
	})
}

func TestE2E_Core_HeaderDecorator(t *testing.T) {
	WaitReady(t)
	req, err := http.NewRequest(http.MethodGet, GatewayURL(t)+"/users/alice", nil)
	require.NoError(t, err)
	req.Header.Set("X-Trace-Id", "t-123")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var got userShape
	readJSON(t, resp, &got)
	assert.Equal(t, "t-123", got.Trace,
		"@GatewayHeader('x-trace-id') must surface the inbound header in the response")
}

func TestE2E_Core_UnknownRouteReturns404(t *testing.T) {
	WaitReady(t)
	resp, err := http.Get(GatewayURL(t) + "/nothing/here")
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode,
		"gateway-server emits 404 for routing.Table miss with pre-encoded body")
	assert.Contains(t, resp.Header.Get("Content-Type"), "application/json",
		"pre-encoded error bodies are JSON-shaped")

	body := readBody(t, resp)
	assert.Contains(t, body, "Not Found", "RFC 9110 reason phrase pinned")
}

func TestE2E_Core_MethodNotAllowedReturns405(t *testing.T) {
	WaitReady(t)
	req, err := http.NewRequest(http.MethodPut, GatewayURL(t)+"/users", nil)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode,
		"path /users matches but PUT is not registered")

	allow := resp.Header.Get("Allow")
	assert.NotEmpty(t, allow, "Allow header must list registered methods")
	parts := strings.Split(allow, ",")
	registered := make(map[string]struct{}, len(parts))
	for _, p := range parts {
		registered[strings.TrimSpace(p)] = struct{}{}
	}
	assert.Contains(t, registered, http.MethodGet)
	assert.Contains(t, registered, http.MethodPost)
	_ = readBody(t, resp)
}

func TestE2E_Core_RequestIdAlwaysPresent(t *testing.T) {
	WaitReady(t)
	cases := []struct {
		name string
		req  func() (*http.Request, error)
	}{
		{
			name: "200",
			req: func() (*http.Request, error) {
				return http.NewRequest(http.MethodGet, GatewayURL(t)+"/users/alice", nil)
			},
		},
		{
			name: "404",
			req: func() (*http.Request, error) {
				return http.NewRequest(http.MethodGet, GatewayURL(t)+"/nothing/here", nil)
			},
		},
		{
			name: "405",
			req: func() (*http.Request, error) {
				return http.NewRequest(http.MethodPut, GatewayURL(t)+"/users", nil)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := tc.req()
			require.NoError(t, err)
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			t.Cleanup(func() { _ = resp.Body.Close() })
			assert.NotEmpty(t, resp.Header.Get("X-Request-Id"),
				"X-Request-Id must surface on every response")
		})
	}
}
