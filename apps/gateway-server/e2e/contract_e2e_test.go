//go:build e2e

package e2e

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// optionsRequest issues an OPTIONS preflight with the canonical
// Access-Control-Request-Method + Origin pair the browser sends.
func optionsRequest(t *testing.T, path, origin, method string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodOptions, GatewayURL(t)+path, nil)
	require.NoError(t, err)
	req.Header.Set("Origin", origin)
	req.Header.Set("Access-Control-Request-Method", method)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

// getWithOrigin issues a GET that includes an Origin header so the
// gateway evaluates CORS on the regular response path.
func getWithOrigin(t *testing.T, path, origin string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, GatewayURL(t)+path, nil)
	require.NoError(t, err)
	req.Header.Set("Origin", origin)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

func TestE2E_Contract_PreflightMatchedOrigin(t *testing.T) {
	WaitReady(t)

	resp := optionsRequest(t, "/products/123", "https://shop.example", http.MethodGet)
	t.Cleanup(func() { _ = resp.Body.Close() })

	assert.Equal(t, http.StatusNoContent, resp.StatusCode,
		"preflight short-circuits to 204 without touching NATS")
	assert.Equal(t, "https://shop.example", resp.Header.Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "Origin", resp.Header.Get("Vary"),
		"Vary: Origin must be emitted unconditionally for CDN-cache correctness")
	assert.Equal(t, "600", resp.Header.Get("Access-Control-Max-Age"),
		"per-route maxAge must surface")
}

func TestE2E_Contract_PreflightUnknownOrigin(t *testing.T) {
	WaitReady(t)

	resp := optionsRequest(t, "/products/123", "https://evil.example", http.MethodGet)
	t.Cleanup(func() { _ = resp.Body.Close() })

	assert.Equal(t, http.StatusNotFound, resp.StatusCode,
		"unknown origin must NOT leak the allowlist; gateway returns 404 verbatim")
	assert.Empty(t, resp.Header.Get("Access-Control-Allow-Origin"),
		"no ACAO must surface for an unmatched origin")
}

func TestE2E_Contract_GetWithCorsEmitsVary(t *testing.T) {
	WaitReady(t)

	resp := getWithOrigin(t, "/products/abc", "https://shop.example")
	t.Cleanup(func() { _ = resp.Body.Close() })

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "https://shop.example", resp.Header.Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "Origin", resp.Header.Get("Vary"),
		"Vary: Origin pinned on every CORS response, not just preflight")
	expose := resp.Header.Get("Access-Control-Expose-Headers")
	assert.Contains(t, expose, "X-Request-Id",
		"default expose list must include gateway correlator")
}

func TestE2E_Contract_CredentialsRoute(t *testing.T) {
	WaitReady(t)

	resp := getWithOrigin(t, "/tenants/t-1", "https://tenant.example")
	t.Cleanup(func() { _ = resp.Body.Close() })

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "https://tenant.example", resp.Header.Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "true", resp.Header.Get("Access-Control-Allow-Credentials"),
		"credentials:true on the route must surface as ACAC: true")
}

func TestE2E_Contract_ForRootCorsAppliesWhenRouteOmits(t *testing.T) {
	WaitReady(t)

	// /shared has no per-route cors — inherits forRoot's allowlist
	// ('https://default.example').
	respMatch := getWithOrigin(t, "/shared", "https://default.example")
	t.Cleanup(func() { _ = respMatch.Body.Close() })
	assert.Equal(t, http.StatusOK, respMatch.StatusCode)
	assert.Equal(t, "https://default.example", respMatch.Header.Get("Access-Control-Allow-Origin"),
		"forRoot CORS must apply when route omits its own block")

	// Same path, origin not in forRoot allowlist ⇒ no ACAO.
	respMiss := getWithOrigin(t, "/shared", "https://shop.example")
	t.Cleanup(func() { _ = respMiss.Body.Close() })
	assert.Equal(t, http.StatusOK, respMiss.StatusCode)
	assert.Empty(t, respMiss.Header.Get("Access-Control-Allow-Origin"),
		"forRoot allowlist is the only one that matters when route has no cors")

	// /products/123 has its own cors block — forRoot's origin must NOT
	// apply (shallow-replace semantics).
	respShallow := getWithOrigin(t, "/products/123", "https://default.example")
	t.Cleanup(func() { _ = respShallow.Body.Close() })
	assert.Equal(t, http.StatusOK, respShallow.StatusCode)
	assert.Empty(t, respShallow.Header.Get("Access-Control-Allow-Origin"),
		"per-route cors fully replaces forRoot — default.example is not in shop.example's allowlist")
}

func TestE2E_Contract_TimeoutSurfacesAs504(t *testing.T) {
	WaitReady(t)

	start := time.Now()
	resp, err := http.Get(GatewayURL(t) + "/slow")
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	elapsed := time.Since(start)
	assert.Equal(t, http.StatusGatewayTimeout, resp.StatusCode,
		"50ms route timeout against a 250ms handler must surface as 504")
	assert.Less(t, elapsed, 1*time.Second,
		"504 must arrive promptly; gateway must not wait for the handler to finish")

	body := readBody(t, resp)
	assert.Contains(t, body, "Gateway Timeout",
		"504 body carries RFC 9110 reason phrase")
}

func TestE2E_Contract_HeadersDeepMergeWithForRoot(t *testing.T) {
	WaitReady(t)

	resp, err := http.Get(GatewayURL(t) + "/headers/static")
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "forRoot", resp.Header.Get("X-Default-Header"),
		"forRoot-only header must reach the wire")
	assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"),
		"route-only header must reach the wire")
	assert.Equal(t, "route-wins", resp.Header.Get("X-Route"),
		"per-route header value must override forRoot value on key conflict")
}

func TestE2E_Contract_ExceptionFilterMapsHttpExceptionTypes(t *testing.T) {
	WaitReady(t)

	cases := []struct {
		kind   string
		status int
	}{
		{"badrequest", http.StatusBadRequest},
		{"forbidden", http.StatusForbidden},
		{"conflict", http.StatusConflict},
		{"internal", http.StatusInternalServerError},
	}

	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			resp, err := http.Get(GatewayURL(t) + "/errors/" + tc.kind)
			require.NoError(t, err)
			t.Cleanup(func() { _ = resp.Body.Close() })
			assert.Equal(t, tc.status, resp.StatusCode,
				"GatewayExceptionFilter must map %s to %d", tc.kind, tc.status)
			body := readBody(t, resp)
			assert.True(t, strings.Contains(body, tc.kind+"-kind") || strings.Contains(body, "kind"),
				"body should carry the thrown message: %q", body)
		})
	}
}
