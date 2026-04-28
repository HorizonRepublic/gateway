//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// echoHandlerKey is the KV key the reload tests PUT/DELETE on. The
// value's `pattern` portion (`reload.echo`) maps to a real
// `@MessagePattern` handler in example-app's reload feature module
// that returns a manual `GatewayReply` envelope.
const echoHandlerKey = "example-app.cmd.reload.echo"

// echoAltHandlerKey points at example-app's second shadow handler.
// Used by the modified-entry test to swap subjects mid-flight.
const echoAltHandlerKey = "example-app.cmd.reload.echo.alt"

// kvCtx returns a 3s context for KV operations. Sufficient for any
// JetStream KV op against a local Compose nats; bounds a stuck call
// so the test fails loudly instead of hanging.
func kvCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 3*time.Second)
}

// putHandlerEntry serialises the supplied HTTP descriptor into a
// HandlerEntry-shaped value and PUTs it on the named key. Returns the
// raw revision for callers that want to assert versioning, but most
// reload tests ignore it.
//
// Mirrors registry.HTTPMeta + registry.HandlerEntry on the wire; we
// deliberately hand-encode here instead of importing those types so
// the test stays a black-box client of the public KV contract.
func putHandlerEntry(t *testing.T, key string, method, path string) {
	t.Helper()
	bucket := HandlerBucket(t)
	value, err := json.Marshal(map[string]any{
		"http": map[string]any{
			"method": method,
			"path":   path,
		},
	})
	require.NoError(t, err)
	ctx, cancel := kvCtx()
	defer cancel()
	_, err = bucket.Put(ctx, key, value)
	require.NoError(t, err, "PUT %s", key)
}

// deleteHandlerEntry removes the named key. Idempotent: a missing key
// is treated as success because t.Cleanup may run after a manual
// delete.
func deleteHandlerEntry(t *testing.T, key string) {
	t.Helper()
	bucket := HandlerBucket(t)
	ctx, cancel := kvCtx()
	defer cancel()
	if err := bucket.Delete(ctx, key); err != nil {
		// jetstream returns nats.ErrKeyNotFound when the key never
		// existed; tolerate that for cleanup ergonomics.
		t.Logf("KV delete %s: %v (tolerated)", key, err)
	}
}

// echoBodyShape mirrors the JSON shape ShadowController returns.
type echoBodyShape struct {
	OK   bool   `json:"ok"`
	Kind string `json:"kind"`
}

func TestE2E_Reload_NewKVEntryAddsLiveRoute(t *testing.T) {
	WaitReady(t)

	// Path is unique to this test so a stale entry from an earlier
	// failure does not pollute neighbours.
	const path = "/__reload/new"

	// Pre-condition: route does not exist.
	resp, err := http.Get(GatewayURL(t) + path)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode,
		"path must 404 before the test PUTs the entry")

	t.Cleanup(func() { deleteHandlerEntry(t, echoHandlerKey) })
	putHandlerEntry(t, echoHandlerKey, http.MethodGet, path)

	respLive := WaitForRoute(t, http.MethodGet, path, http.StatusOK)
	t.Cleanup(func() { _ = respLive.Body.Close() })

	var got echoBodyShape
	require.NoError(t, json.NewDecoder(respLive.Body).Decode(&got))
	assert.True(t, got.OK)
	assert.Equal(t, "echo", got.Kind,
		"body must come from ShadowController.echo, proving the new entry routes to the live handler")
}

func TestE2E_Reload_DeletedKVEntryReturns404(t *testing.T) {
	WaitReady(t)

	const path = "/__reload/deleted"

	t.Cleanup(func() { deleteHandlerEntry(t, echoHandlerKey) })
	putHandlerEntry(t, echoHandlerKey, http.MethodGet, path)

	respUp := WaitForRoute(t, http.MethodGet, path, http.StatusOK)
	_ = respUp.Body.Close()

	deleteHandlerEntry(t, echoHandlerKey)

	respDown := WaitForRoute(t, http.MethodGet, path, http.StatusNotFound)
	_ = respDown.Body.Close()
}

func TestE2E_Reload_ModifiedKVEntryChangesBehavior(t *testing.T) {
	WaitReady(t)

	const pathV1 = "/__reload/v1"
	const pathV2 = "/__reload/v2"

	t.Cleanup(func() {
		deleteHandlerEntry(t, echoHandlerKey)
		deleteHandlerEntry(t, echoAltHandlerKey)
	})

	// Stage 1: PUT echo at v1 path. Wait until live.
	putHandlerEntry(t, echoHandlerKey, http.MethodGet, pathV1)
	respV1 := WaitForRoute(t, http.MethodGet, pathV1, http.StatusOK)
	_ = respV1.Body.Close()

	// Stage 2: same key, mutated path ⇒ Modified delta. Watcher
	// rebuilds the table; v1 disappears, v2 appears.
	putHandlerEntry(t, echoHandlerKey, http.MethodGet, pathV2)

	// v2 must come up.
	respV2 := WaitForRoute(t, http.MethodGet, pathV2, http.StatusOK)
	t.Cleanup(func() { _ = respV2.Body.Close() })

	// v1 must drop. Same WaitForRoute helper but expecting 404.
	respV1Gone := WaitForRoute(t, http.MethodGet, pathV1, http.StatusNotFound)
	_ = respV1Gone.Body.Close()

	// Body assertion against the v2 response confirms the same
	// underlying handler is still reachable — it's the table that
	// changed, not the upstream.
	var got echoBodyShape
	require.NoError(t, json.NewDecoder(respV2.Body).Decode(&got))
	assert.Equal(t, "echo", got.Kind)
}
