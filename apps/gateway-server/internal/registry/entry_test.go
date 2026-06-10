package registry

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHandlerEntry_ParseLegacyRoute pins backward compatibility: a
// route entry written by the previous SDK version (no auth, no
// verifier field) must still parse cleanly so rolling deploys can
// introduce the new gateway-server alongside existing services
// without breaking their registrations.
func TestHandlerEntry_ParseLegacyRoute(t *testing.T) {
	raw := []byte(`{"http":{"method":"GET","path":"/users/:id"}}`)

	var sut HandlerEntry
	require.NoError(t, json.Unmarshal(raw, &sut))

	require.NotNil(t, sut.HTTP)
	assert.Equal(t, "GET", sut.HTTP.Method)
	assert.Equal(t, "/users/:id", sut.HTTP.Path)
	assert.Nil(t, sut.Auth)
	assert.Nil(t, sut.Verifier)
}

func TestHandlerEntry_ParseRouteWithRequiredAuth(t *testing.T) {
	raw := []byte(`{"http":{"method":"POST","path":"/users"},"auth":{"verifier":"jwt","optional":false}}`)

	var sut HandlerEntry
	require.NoError(t, json.Unmarshal(raw, &sut))

	require.NotNil(t, sut.HTTP)
	require.NotNil(t, sut.Auth)
	assert.Equal(t, "jwt", sut.Auth.Verifier)
	assert.False(t, sut.Auth.Optional)
	assert.Nil(t, sut.Verifier)
}

// TestHandlerEntry_ParseRouteWithOptionalAuth covers the optional-auth
// case. An empty verifier id means "use the default verifier", which
// the routing builder resolves at table build time.
func TestHandlerEntry_ParseRouteWithOptionalAuth(t *testing.T) {
	raw := []byte(`{"http":{"method":"GET","path":"/articles/:id"},"auth":{"optional":true}}`)

	var sut HandlerEntry
	require.NoError(t, json.Unmarshal(raw, &sut))

	require.NotNil(t, sut.Auth)
	assert.Equal(t, "", sut.Auth.Verifier, "empty verifier id means default")
	assert.True(t, sut.Auth.Optional)
}

func TestHandlerEntry_ParseVerifierWithDefault(t *testing.T) {
	raw := []byte(`{"verifier":{"id":"jwt","default":true}}`)

	var sut HandlerEntry
	require.NoError(t, json.Unmarshal(raw, &sut))

	assert.Nil(t, sut.HTTP)
	assert.Nil(t, sut.Auth)
	require.NotNil(t, sut.Verifier)
	assert.Equal(t, "jwt", sut.Verifier.ID)
	assert.True(t, sut.Verifier.Default)
}

func TestHandlerEntry_ParseVerifierWithoutDefault(t *testing.T) {
	raw := []byte(`{"verifier":{"id":"session"}}`)

	var sut HandlerEntry
	require.NoError(t, json.Unmarshal(raw, &sut))

	require.NotNil(t, sut.Verifier)
	assert.Equal(t, "session", sut.Verifier.ID)
	assert.False(t, sut.Verifier.Default)
}

// TestHandlerEntry_ParsePureRpc covers handlers that are registered
// in KV (because they use @MessagePattern) but expose no gateway
// surface. They survive the parse and are later silently skipped by
// both the routing builder and the verifier registry.
func TestHandlerEntry_ParsePureRpc(t *testing.T) {
	raw := []byte(`{}`)

	var sut HandlerEntry
	require.NoError(t, json.Unmarshal(raw, &sut))

	assert.Nil(t, sut.HTTP)
	assert.Nil(t, sut.Auth)
	assert.Nil(t, sut.Verifier)
}

func TestHandlerEntry_DeserializeExtendedFields(t *testing.T) {
	raw := `{
		"http":{"method":"POST","path":"/users"},
		"cors":{"origins":["https://app.example.com"],"methods":["POST"],"credentials":true,"maxAge":3600},
		"rateLimit":{"rps":10,"burst":20,"keyBy":["user:id","ip"]},
		"headers":{"x-frame-options":"DENY","cache-control":"no-store"},
		"timeout":5000
	}`

	var entry HandlerEntry
	err := json.Unmarshal([]byte(raw), &entry)
	require.NoError(t, err)

	require.NotNil(t, entry.CORS)
	assert.Equal(t, []string{"https://app.example.com"}, entry.CORS.Origins)
	assert.Equal(t, []string{"POST"}, entry.CORS.Methods)
	assert.True(t, entry.CORS.Credentials)
	assert.Equal(t, 3600, entry.CORS.MaxAge)

	require.NotNil(t, entry.RateLimit)
	assert.Equal(t, 10, entry.RateLimit.RPS)
	assert.Equal(t, 20, entry.RateLimit.Burst)
	assert.Equal(t, []string{"user:id", "ip"}, entry.RateLimit.KeyBy)

	assert.Equal(t, map[string]string{"x-frame-options": "DENY", "cache-control": "no-store"}, entry.Headers)

	require.NotNil(t, entry.Timeout)
	assert.Equal(t, 5000, *entry.Timeout)
}

func TestHandlerEntry_OmitsExtendedFieldsWhenAbsent(t *testing.T) {
	raw := `{"http":{"method":"GET","path":"/hello"}}`

	var entry HandlerEntry
	err := json.Unmarshal([]byte(raw), &entry)
	require.NoError(t, err)

	assert.Nil(t, entry.CORS)
	assert.Nil(t, entry.RateLimit)
	assert.Nil(t, entry.Headers)
	assert.Nil(t, entry.Timeout)
}

// TestHandlerEntry_SDKWireFormatContract pins the exact JSON shape
// emitted by the gateway-sdk metadata enricher after merging
// forRoot defaults with per-route @GatewayRoute options. If the SDK
// changes any field name or nesting, this test MUST be updated in
// lockstep — it is the cross-boundary contract guard.
func TestHandlerEntry_SDKWireFormatContract(t *testing.T) {
	raw := `{
		"http": {
			"method": "POST",
			"path": "/users",
			"statusCode": 201
		},
		"auth": {
			"verifier": "jwt-default",
			"optional": false
		},
		"cors": {
			"origins": ["https://app.example.com", "https://admin.example.com"],
			"methods": ["POST", "PUT"],
			"headers": ["Content-Type", "Authorization", "X-Request-Id"],
			"credentials": true,
			"maxAge": 3600
		},
		"rateLimit": {
			"rps": 50,
			"burst": 100,
			"keyBy": ["user:id", "header:x-api-key", "ip"]
		},
		"headers": {
			"x-frame-options": "DENY",
			"x-content-type-options": "nosniff",
			"strict-transport-security": "max-age=31536000; includeSubDomains",
			"cache-control": "no-store"
		},
		"timeout": 15000
	}`

	var entry HandlerEntry
	require.NoError(t, json.Unmarshal([]byte(raw), &entry))

	// HTTP
	require.NotNil(t, entry.HTTP)
	assert.Equal(t, "POST", entry.HTTP.Method)
	assert.Equal(t, "/users", entry.HTTP.Path)
	require.NotNil(t, entry.HTTP.StatusCode)
	assert.Equal(t, 201, *entry.HTTP.StatusCode)

	// Auth
	require.NotNil(t, entry.Auth)
	assert.Equal(t, "jwt-default", entry.Auth.Verifier)
	assert.False(t, entry.Auth.Optional)

	// CORS
	require.NotNil(t, entry.CORS)
	assert.Equal(t, []string{"https://app.example.com", "https://admin.example.com"}, entry.CORS.Origins)
	assert.Equal(t, []string{"POST", "PUT"}, entry.CORS.Methods)
	assert.Equal(t, []string{"Content-Type", "Authorization", "X-Request-Id"}, entry.CORS.Headers)
	assert.True(t, entry.CORS.Credentials)
	assert.Equal(t, 3600, entry.CORS.MaxAge)

	// Rate limit
	require.NotNil(t, entry.RateLimit)
	assert.Equal(t, 50, entry.RateLimit.RPS)
	assert.Equal(t, 100, entry.RateLimit.Burst)
	assert.Equal(t, []string{"user:id", "header:x-api-key", "ip"}, entry.RateLimit.KeyBy)

	// Headers
	assert.Len(t, entry.Headers, 4)
	assert.Equal(t, "DENY", entry.Headers["x-frame-options"])
	assert.Equal(t, "nosniff", entry.Headers["x-content-type-options"])
	assert.Contains(t, entry.Headers["strict-transport-security"], "max-age=31536000")
	assert.Equal(t, "no-store", entry.Headers["cache-control"])

	// Timeout
	require.NotNil(t, entry.Timeout)
	assert.Equal(t, 15000, *entry.Timeout)
}

func TestRateLimitMeta_JSONRoundTripWithStore(t *testing.T) {
	in := RateLimitMeta{RPS: 10, Burst: 20, Store: "nats-kv"}
	raw, err := json.Marshal(in)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"store":"nats-kv"`)

	var out RateLimitMeta
	require.NoError(t, json.Unmarshal(raw, &out))
	assert.Equal(t, in, out)
}

func TestRateLimitMeta_JSONRoundTripWithFailPolicy(t *testing.T) {
	in := RateLimitMeta{RPS: 10, FailPolicy: "closed"}
	raw, err := json.Marshal(in)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"failPolicy":"closed"`)

	var out RateLimitMeta
	require.NoError(t, json.Unmarshal(raw, &out))
	assert.Equal(t, in, out)
}

func TestRateLimitMeta_JSONRoundTripWithoutFailPolicy(t *testing.T) {
	in := RateLimitMeta{RPS: 10}
	raw, err := json.Marshal(in)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), `"failPolicy"`)

	var out RateLimitMeta
	require.NoError(t, json.Unmarshal(raw, &out))
	assert.Equal(t, "", out.FailPolicy)
}

func TestRateLimitMeta_JSONRoundTripWithoutStore(t *testing.T) {
	in := RateLimitMeta{RPS: 10}
	raw, err := json.Marshal(in)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), `"store"`)

	var out RateLimitMeta
	require.NoError(t, json.Unmarshal(raw, &out))
	assert.Equal(t, "", out.Store)
}
