package proxy

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGatewayRequest_ResetClearsAllFields(t *testing.T) {
	// Populate every field with a non-zero sentinel.
	envelope := &GatewayRequest{
		Route: RouteContext{Method: "POST", Path: "/users/:id", MatchedPath: "/users/42"},
		Params: map[string]string{
			"id": "42",
		},
		Query: map[string]QueryValue{
			"include": NewQueryValueString("profile"),
			"tags":    NewQueryValueStrings([]string{"a", "b"}),
		},
		Headers: map[string]string{
			"content-type": "application/json",
			"x-trace":      "abc",
		},
		Body: json.RawMessage(`{"name":"alice"}`),
		Meta: RequestMeta{
			RequestID:   "req-1",
			Traceparent: "00-trace",
			RemoteAddr:  "1.2.3.4",
			ReceivedAt:  100,
			TimeoutMs:   5000,
		},
	}

	envelope.reset()

	assert.Equal(t, RouteContext{}, envelope.Route)
	assert.Empty(t, envelope.Params)
	assert.Empty(t, envelope.Query)
	assert.Empty(t, envelope.Headers)
	assert.Nil(t, envelope.Body)
	assert.Equal(t, RequestMeta{}, envelope.Meta)
}

func TestGatewayRequest_ResetRetainsMapBackingCapacity(t *testing.T) {
	// The whole point of pooling is that the backing arrays survive
	// reset. The resulting maps are empty-but-not-nil so the next
	// acquirer can insert without a realloc for the common small-map
	// case.
	envelope := &GatewayRequest{
		Params:  map[string]string{"a": "1"},
		Query:   map[string]QueryValue{"q": NewQueryValueString("v")},
		Headers: map[string]string{"h": "v"},
	}

	envelope.reset()

	assert.NotNil(t, envelope.Params, "params map retained for reuse")
	assert.NotNil(t, envelope.Query, "query map retained for reuse")
	assert.NotNil(t, envelope.Headers, "headers map retained for reuse")
}
