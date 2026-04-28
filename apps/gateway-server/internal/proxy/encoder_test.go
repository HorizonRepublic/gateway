package proxy

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/codec"
	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/routing"
)

func TestDefaultEncoder_BuildsValidEnvelope(t *testing.T) {
	enc := NewDefaultEncoder()
	buf := make([]byte, 0, 512)

	err := enc.Encode(&buf, &EncodeInput{
		Method: "GET",
		Path:   "/users/42",
		Body:   []byte(`{"name":"Alice"}`),
		Query: map[string]QueryValue{
			"dry": NewQueryValueString("1"),
		},
		Headers: map[string]string{
			"authorization": "Bearer token",
		},
		Route: routing.Route{
			Subject:      "svc.cmd.users.get",
			Method:       "GET",
			PathTemplate: "/users/:id",
		},
		PathParams:  map[string]string{"id": "42"},
		RequestID:   "req-123",
		Traceparent: "00-trace-span-01",
		RemoteAddr:  "127.0.0.1:51000",
		ReceivedAt:  1700000000000,
		TimeoutMs:   30000,
	})
	require.NoError(t, err)

	var decoded GatewayRequest
	require.NoError(t, codec.Unmarshal(buf, &decoded))

	assert.Equal(t, "GET", decoded.Route.Method)
	assert.Equal(t, "/users/:id", decoded.Route.Path)
	assert.Equal(t, "/users/42", decoded.Route.MatchedPath)
	assert.Equal(t, "42", decoded.Params["id"])
	assert.Equal(t, NewQueryValueString("1"), decoded.Query["dry"])
	assert.Equal(t, "Bearer token", decoded.Headers["authorization"])
	assert.JSONEq(t, `{"name":"Alice"}`, string(decoded.Body))
	assert.Equal(t, "req-123", decoded.Meta.RequestID)
	assert.Equal(t, "00-trace-span-01", decoded.Meta.Traceparent)
	assert.Equal(t, "127.0.0.1:51000", decoded.Meta.RemoteAddr)
	assert.Equal(t, int64(1700000000000), decoded.Meta.ReceivedAt)
	assert.Equal(t, int64(30000), decoded.Meta.TimeoutMs)
}

func TestDefaultEncoder_HandlesMultiValueQuery(t *testing.T) {
	enc := NewDefaultEncoder()
	buf := make([]byte, 0, 512)

	err := enc.Encode(&buf, &EncodeInput{
		Method: "GET",
		Path:   "/items",
		Query: map[string]QueryValue{
			"tag": NewQueryValueStrings([]string{"a", "b"}),
		},
		Route: routing.Route{
			Subject:      "svc.cmd.items.list",
			Method:       "GET",
			PathTemplate: "/items",
		},
		RequestID: "req-1",
		TimeoutMs: 1000,
	})
	require.NoError(t, err)

	var decoded GatewayRequest
	require.NoError(t, codec.Unmarshal(buf, &decoded))

	assert.Equal(t, []string{"a", "b"}, decoded.Query["tag"].Multi)
}

func TestDefaultEncoder_EmptyBodyIsNullJSON(t *testing.T) {
	enc := NewDefaultEncoder()
	buf := make([]byte, 0, 512)

	err := enc.Encode(&buf, &EncodeInput{
		Method: "GET",
		Path:   "/ping",
		Body:   nil,
		Route: routing.Route{
			Subject:      "svc.cmd.ping",
			Method:       "GET",
			PathTemplate: "/ping",
		},
		RequestID: "req-1",
		TimeoutMs: 1000,
	})
	require.NoError(t, err)

	assert.True(t, strings.Contains(string(buf), `"body":null`),
		"expected body to marshal as null, got: %s", string(buf))
}
