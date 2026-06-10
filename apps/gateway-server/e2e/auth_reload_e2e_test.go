//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Synthetic KV keys for the verifier hot-reload pack. The keys encode
// the shadow handler patterns (ShadowAuthController in example-app),
// which publish NO registry entries of their own — every entry under
// these keys is test-owned and immune to the metadata heartbeat.
const (
	verifierA1Key   = "example-app.cmd.auth.verifier.a1"
	verifierA2Key   = "example-app.cmd.auth.verifier.a2"
	whoamiRouteKey  = "example-app.cmd.reload.whoami"
	whoamiRoutePath = "/__reload/whoami"
)

// putVerifierEntry PUTs a standalone verifier entry. Mirrors
// registry.VerifierMeta on the wire; hand-encoded so the test stays a
// black-box client of the public KV contract.
func putVerifierEntry(t *testing.T, key, id string, isDefault bool) {
	t.Helper()
	bucket := HandlerBucket(t)
	value, err := json.Marshal(map[string]any{
		"verifier": map[string]any{
			"id":      id,
			"default": isDefault,
		},
	})
	require.NoError(t, err)
	ctx, cancel := kvCtx()
	defer cancel()
	_, err = bucket.Put(ctx, key, value)
	require.NoError(t, err, "PUT %s", key)
}

// putAuthRouteEntry PUTs a route entry with an auth block. An empty
// verifierID means "use the default verifier" (the SDK's `auth: true`
// wire shape).
func putAuthRouteEntry(t *testing.T, key, method, path, verifierID string) {
	t.Helper()
	bucket := HandlerBucket(t)
	value, err := json.Marshal(map[string]any{
		"http": map[string]any{
			"method": method,
			"path":   path,
		},
		"auth": map[string]any{
			"verifier": verifierID,
			"optional": false,
		},
	})
	require.NoError(t, err)
	ctx, cancel := kvCtx()
	defer cancel()
	_, err = bucket.Put(ctx, key, value)
	require.NoError(t, err, "PUT %s", key)
}

// whoamiSub probes the whoami route once and returns the echoed sub
// claim. The shadow verifiers are allow-all, so any bearer works.
func whoamiSub(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	var got struct {
		Sub string `json:"sub"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	return got.Sub
}

// TestE2E_AuthReload_NewVerifierIdServesRoute pins verifier-id
// rotation without a gateway redeploy: a verifier id that did not
// exist at gateway boot is PUT into KV alongside a route that
// references it; the route must come up authenticated through the
// new verifier.
func TestE2E_AuthReload_NewVerifierIdServesRoute(t *testing.T) {
	WaitReady(t)

	t.Cleanup(func() {
		deleteHandlerEntry(t, whoamiRouteKey)
		deleteHandlerEntry(t, verifierA1Key)
	})

	putVerifierEntry(t, verifierA1Key, "rotated-1", false)
	putAuthRouteEntry(t, whoamiRouteKey, http.MethodGet, whoamiRoutePath, "rotated-1")

	resp := WaitForRoute(t, http.MethodGet, whoamiRoutePath, http.StatusOK)
	assert.Equal(t, "shadow-a1", whoamiSub(t, resp),
		"route must authenticate through the hot-loaded verifier id")
}

// TestE2E_AuthReload_DeletedVerifierDropsRouteAndSelfHeals pins the
// fail-closed posture and its inverse: deleting a verifier entry
// drops every route that references it from the table (404 — never
// an unauthenticated pass-through), and re-PUTting the verifier
// self-heals the route on the next rebuild without any gateway
// restart.
func TestE2E_AuthReload_DeletedVerifierDropsRouteAndSelfHeals(t *testing.T) {
	WaitReady(t)

	t.Cleanup(func() {
		deleteHandlerEntry(t, whoamiRouteKey)
		deleteHandlerEntry(t, verifierA1Key)
	})

	putVerifierEntry(t, verifierA1Key, "rotated-1", false)
	putAuthRouteEntry(t, whoamiRouteKey, http.MethodGet, whoamiRoutePath, "rotated-1")
	resp := WaitForRoute(t, http.MethodGet, whoamiRoutePath, http.StatusOK)
	_ = resp.Body.Close()

	// Verifier disappears; the route must go WITH it, fail-closed.
	deleteHandlerEntry(t, verifierA1Key)
	respGone := WaitForRoute(t, http.MethodGet, whoamiRoutePath, http.StatusNotFound)
	_ = respGone.Body.Close()

	// Verifier returns; the route self-heals.
	putVerifierEntry(t, verifierA1Key, "rotated-1", false)
	respBack := WaitForRoute(t, http.MethodGet, whoamiRoutePath, http.StatusOK)
	assert.Equal(t, "shadow-a1", whoamiSub(t, respBack),
		"route must self-heal once its verifier re-registers")
}

// TestE2E_AuthReload_DefaultFlipChangesResolution pins two contracts
// at once: a `default: true` flip across verifier entries propagates
// to live default-resolution, and a default collision resolves to the
// first lexicographic KV key. The shadow keys (`...verifier.a1`,
// `...verifier.a2`) sort before the real `...verifier.jwt` entry, so
// the synthetic default wins the slot while the heartbeat-owned jwt
// entry stays untouched.
func TestE2E_AuthReload_DefaultFlipChangesResolution(t *testing.T) {
	WaitReady(t)

	t.Cleanup(func() {
		deleteHandlerEntry(t, whoamiRouteKey)
		deleteHandlerEntry(t, verifierA1Key)
		deleteHandlerEntry(t, verifierA2Key)
	})

	// Stage 1: a1 holds default. Route declares no verifier id ⇒
	// default resolution.
	putVerifierEntry(t, verifierA1Key, "shadow-a1", true)
	putVerifierEntry(t, verifierA2Key, "shadow-a2", false)
	putAuthRouteEntry(t, whoamiRouteKey, http.MethodGet, whoamiRoutePath, "")

	resp := WaitForRoute(t, http.MethodGet, whoamiRoutePath, http.StatusOK)
	require.Equal(t, "shadow-a1", whoamiSub(t, resp),
		"default resolution must pick a1 (default:true, first lexicographic key)")

	// Stage 2: flip the default to a2. The route entry is untouched —
	// only the verifier entries change, so the observed sub flipping
	// proves the verifier registry rebuilt and the route re-resolved.
	putVerifierEntry(t, verifierA1Key, "shadow-a1", false)
	putVerifierEntry(t, verifierA2Key, "shadow-a2", true)

	deadline := waitForWhoamiSub(t, "shadow-a2")
	assert.True(t, deadline,
		"default flip must propagate to live default-resolution within the reload budget")
}

// waitForWhoamiSub polls the whoami route until the echoed sub equals
// want or the reload deadline elapses. The status stays 200 across the
// flip (both verifiers allow), so WaitForRoute's status polling cannot
// observe the transition — the body can.
func waitForWhoamiSub(t *testing.T, want string) bool {
	t.Helper()
	deadline := time.Now().Add(routeReloadDeadline)
	for {
		resp := WaitForRoute(t, http.MethodGet, whoamiRoutePath, http.StatusOK)
		if whoamiSub(t, resp) == want {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(routeReloadInterval)
	}
}
