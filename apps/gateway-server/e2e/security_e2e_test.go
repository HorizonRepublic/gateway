//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// echoHeadersResponse mirrors example-app's /__sec/headers body shape.
type echoHeadersResponse struct {
	Headers map[string]string `json:"headers"`
}

func TestE2E_Security_HopByHopHeadersStripped(t *testing.T) {
	WaitReady(t)

	req, err := http.NewRequest(http.MethodGet, GatewayURL(t)+"/__sec/headers", nil)
	require.NoError(t, err)
	// Hop-by-hop headers per RFC 9110 §7.6.1; the gateway adapter
	// strips these before building the envelope. Forwarding them
	// would let an upstream re-frame the request body (smuggling)
	// or hold the connection open in unexpected ways.
	req.Header.Set("Connection", "close")
	req.Header.Set("Te", "trailers")
	req.Header.Set("Trailer", "foo")
	req.Header.Set("Upgrade", "websocket")
	// A benign custom header that MUST round-trip — the test would
	// pass trivially if the adapter dropped everything.
	req.Header.Set("X-Custom", "keep")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var got echoHeadersResponse
	readJSON(t, resp, &got)

	assert.Equal(t, "keep", got.Headers["x-custom"],
		"benign custom header must reach the upstream so the test isn't trivial")

	// Strip set: every entry MUST be absent from the upstream's view.
	for _, hop := range []string{"connection", "te", "trailer", "upgrade", "transfer-encoding"} {
		_, present := got.Headers[hop]
		assert.False(t, present,
			"hop-by-hop header %q must be stripped at the adapter, but upstream observed it", hop)
	}
}

func TestE2E_Security_MalformedKVEntryDoesNotCrashGateway(t *testing.T) {
	WaitReady(t)

	bucket := HandlerBucket(t)
	const poisonKey = "example-app.cmd.malformed.entry"
	t.Cleanup(func() { deleteHandlerEntry(t, poisonKey) })

	// Truncated JSON — every parser fails this on the first
	// closing brace it never finds.
	garbage := []byte(`{"http":{"method":"GET",`)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := bucket.Put(ctx, poisonKey, garbage)
	require.NoError(t, err)

	// Give the watcher a moment to swallow and ignore the bad
	// entry. The watcher's per-key isolation is the contract: one
	// bad value MUST NOT take other routes down.
	time.Sleep(150 * time.Millisecond)

	respUsers, err := http.Get(GatewayURL(t) + "/users/alice")
	require.NoError(t, err)
	t.Cleanup(func() { _ = respUsers.Body.Close() })
	assert.Equal(t, http.StatusOK, respUsers.StatusCode,
		"malformed KV entry MUST NOT affect other routes")

	respReady, err := http.Get(OperatorURL(t, "gateway-server") + "/readyz")
	require.NoError(t, err)
	t.Cleanup(func() { _ = respReady.Body.Close() })
	assert.Equal(t, http.StatusOK, respReady.StatusCode,
		"malformed KV entry MUST NOT take the gateway out of /readyz")
}

func TestE2E_Security_KVEntryWithCRLFInCORSOriginDropsCORSBlock(t *testing.T) {
	WaitReady(t)

	bucket := HandlerBucket(t)
	t.Cleanup(func() { deleteHandlerEntry(t, echoHandlerKey) })

	// Route entry pointing at PR 8's reload.echo subject (so the
	// upstream actually responds) with a CORS block whose origin
	// carries a CRLF byte. The routing builder fail-closes the
	// CORS block: the route still serves, but the gateway emits
	// no CORS response headers.
	value, err := json.Marshal(map[string]any{
		"http": map[string]any{"method": "GET", "path": "/__sec/cors"},
		"cors": map[string]any{
			"origins": []string{"https://attacker.example\r\n"},
		},
	})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = bucket.Put(ctx, echoHandlerKey, value)
	require.NoError(t, err)

	respLive := WaitForRoute(t, http.MethodGet, "/__sec/cors", http.StatusOK)
	t.Cleanup(func() { _ = respLive.Body.Close() })

	// Send the would-be-allowed origin. With CORS dropped, the
	// gateway emits NO Access-Control-Allow-Origin. A leak here
	// would mean an attacker who poisons KV can opt themselves into
	// CORS — exactly the privilege escalation the fail-closed
	// drop is designed to prevent.
	reqProbe, err := http.NewRequest(http.MethodGet, GatewayURL(t)+"/__sec/cors", nil)
	require.NoError(t, err)
	reqProbe.Header.Set("Origin", "https://attacker.example")
	respProbe, err := http.DefaultClient.Do(reqProbe)
	require.NoError(t, err)
	t.Cleanup(func() { _ = respProbe.Body.Close() })
	assert.Equal(t, http.StatusOK, respProbe.StatusCode,
		"poisoned CORS block drops the block, not the route — request still serves")
	assert.Empty(t, respProbe.Header.Get("Access-Control-Allow-Origin"),
		"CORS block was dropped fail-closed: NO ACAO must reach the wire")
}

func TestE2E_Security_KVEntryWithCRLFInStaticHeadersDropsThatKey(t *testing.T) {
	WaitReady(t)

	bucket := HandlerBucket(t)
	t.Cleanup(func() { deleteHandlerEntry(t, echoHandlerKey) })

	// Route entry with two static headers: a clean one and a
	// poisoned one. The routing builder MUST drop the poisoned
	// entry per-key, leaving the clean one on the wire.
	value, err := json.Marshal(map[string]any{
		"http": map[string]any{"method": "GET", "path": "/__sec/hdr"},
		"headers": map[string]string{
			"x-good": "ok",
			"x-bad":  "bad\r\nset-cookie: hijack=1",
		},
	})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = bucket.Put(ctx, echoHandlerKey, value)
	require.NoError(t, err)

	respLive := WaitForRoute(t, http.MethodGet, "/__sec/hdr", http.StatusOK)
	t.Cleanup(func() { _ = respLive.Body.Close() })

	assert.Equal(t, "ok", respLive.Header.Get("X-Good"),
		"clean static header must round-trip on a route whose other entry is poisoned")
	assert.Empty(t, respLive.Header.Get("X-Bad"),
		"poisoned X-Bad header must be dropped per-key")
	assert.Empty(t, respLive.Header.Get("Set-Cookie"),
		"smuggled Set-Cookie line must NEVER reach the wire — that is the entire point of CRLF sanitisation")

	// Defence-in-depth check: scan all response headers for the
	// smuggled-cookie value verbatim, in case some future framework
	// upgrade lower-cases or normalises differently.
	for k, vs := range respLive.Header {
		for _, v := range vs {
			assert.False(t, strings.Contains(strings.ToLower(v), "hijack"),
				"smuggled value must not appear anywhere on the wire (header %q)", k)
		}
	}
}
