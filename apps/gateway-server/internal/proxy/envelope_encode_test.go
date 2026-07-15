package proxy

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/codec"
)

// mustMarshalString is a tiny wrapper around encoding/json.Marshal
// that panics on failure. Used only in tests as the source of truth
// for correct JSON string escaping.
func mustMarshalString(t *testing.T, s string) string {
	t.Helper()
	out, err := json.Marshal(s)
	require.NoError(t, err)
	return string(out)
}

func TestAppendJSONString_MatchesStdlib(t *testing.T) {
	longRun := strings.Repeat("x", 256)

	cases := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"plain ascii", "hello"},
		{"embedded quote", `he said "hi"`},
		{"embedded backslash", `a\b`},
		{"newline", "line1\nline2"},
		{"tab", "a\tb"},
		{"carriage return", "a\rb"},
		{"backspace", "a\bb"},
		{"form feed", "a\fb"},
		{"control 0x01", "a\x01b"},
		{"del 0x7f", "a\x7fb"},
		{"cyrillic", "привіт"},
		{"emoji", "hi 👋"},
		{"long run", longRun},
		{"invalid utf-8 lone continuation byte", "a\x80b"},
		{"invalid utf-8 truncated multibyte tail", "a\xc3"},
		{"invalid utf-8 0xff 0xfe pair", "\xff\xfe"},
		{"invalid utf-8 between valid runes", "ok\x80привіт\xc3ok"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := string(appendJSONString(nil, tc.input))
			want := mustMarshalString(t, tc.input)
			assert.Equal(t, want, got)
		})
	}
}

// fullEnvelope returns a populated envelope used by the round-trip
// and equivalence tests. Kept separate from the benchmark fixtures so
// test changes do not perturb steady-state benchmark numbers.
func fullEnvelope() *GatewayRequest {
	return &GatewayRequest{
		Route: RouteContext{
			Method:      "POST",
			Path:        "/users/:id",
			MatchedPath: "/users/42",
		},
		Params: map[string]string{
			"id": "42",
		},
		Query: map[string]QueryValue{
			"include": NewQueryValueString("profile"),
			"tags":    NewQueryValueStrings([]string{"a", "b"}),
		},
		Headers: map[string]string{
			"authorization": "Bearer xxx",
			"x-tenant-id":   "42",
		},
		Body: json.RawMessage(`{"email":"a@b.c","name":"Alice"}`),
		Meta: RequestMeta{
			RequestID:   "01HXY0000000000000000000",
			Traceparent: "00-trace",
			RemoteAddr:  "127.0.0.1",
			ReceivedAt:  1000,
			TimeoutMs:   30000,
		},
	}
}

func TestAppendEnvelopeJSON_RoundTripsThroughJSONUnmarshal(t *testing.T) {
	env := fullEnvelope()
	out := appendEnvelopeJSON(nil, env)

	var decoded GatewayRequest
	require.NoError(t, json.Unmarshal(out, &decoded))

	assert.Equal(t, env.Route, decoded.Route)
	assert.Equal(t, env.Params, decoded.Params)
	assert.Equal(t, env.Query, decoded.Query)
	assert.Equal(t, env.Headers, decoded.Headers)
	assert.JSONEq(t, string(env.Body), string(decoded.Body))
	assert.Equal(t, env.Meta, decoded.Meta)
}

func TestAppendEnvelopeJSON_MatchesSonicMarshal(t *testing.T) {
	env := fullEnvelope()

	handRolled := appendEnvelopeJSON(nil, env)
	sonicBytes, err := codec.Marshal(env)
	require.NoError(t, err)

	var handRolledMap map[string]any
	var sonicMap map[string]any
	require.NoError(t, json.Unmarshal(handRolled, &handRolledMap))
	require.NoError(t, json.Unmarshal(sonicBytes, &sonicMap))

	assert.True(t,
		reflect.DeepEqual(handRolledMap, sonicMap),
		"hand-rolled JSON diverges from sonic output\nhand-rolled: %s\nsonic:       %s",
		handRolled, sonicBytes,
	)
}

func TestAppendEnvelopeJSON_EmptyBodyIsNull(t *testing.T) {
	// Both forms of "empty" must serialize as the literal null.
	// The HTTP adapter sources Body from the framework's
	// Request.Body(), which may return either nil or []byte{}
	// depending on whether the client sent a Content-Length:0
	// header or no body at all. Treating the two differently once
	// broke the proxy path: []byte{} fell through to the
	// append-verbatim branch and emitted `"body":,"meta":...`,
	// producing invalid JSON that Nest's JsonCodec rejected with
	// a decode error.
	cases := []struct {
		name string
		body []byte
	}{
		{"nil slice", nil},
		{"zero-length slice", []byte{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := fullEnvelope()
			env.Body = c.body

			out := string(appendEnvelopeJSON(nil, env))

			assert.Contains(t, out, `"body":null`)
			assert.NotContains(t, out, `"body":""`)
			assert.NotContains(t, out, `"body":,`)

			// Strongest guarantee: the full envelope must still
			// round-trip through a standards-compliant JSON parser.
			var decoded map[string]any
			require.NoError(t, json.Unmarshal([]byte(out), &decoded))
			assert.Nil(t, decoded["body"])
		})
	}
}

func TestAppendEnvelopeJSON_OmitsEmptyTraceparent(t *testing.T) {
	env := fullEnvelope()
	env.Meta.Traceparent = ""

	out := string(appendEnvelopeJSON(nil, env))

	assert.NotContains(t, out, "traceparent")

	// The surrounding fields must still be present — guards against an
	// accidental slice of the meta object.
	assert.Contains(t, out, `"requestId":"01HXY0000000000000000000"`)
	assert.Contains(t, out, `"remoteAddr":"127.0.0.1"`)
}

// TestAppendEnvelopeJSON_NilQueryEncodesAsEmptyObject pins the
// nil-tolerance half of the query wire contract: an envelope whose
// Query map is nil must still emit `"query":{}` — never `null` and
// never an absent key. The HTTP adapter returns a nil Query map for
// requests without a query string, so this shape is what every
// query-less request rides on.
func TestAppendEnvelopeJSON_NilQueryEncodesAsEmptyObject(t *testing.T) {
	env := &GatewayRequest{}

	out := string(appendEnvelopeJSON(nil, env))

	assert.Contains(t, out, `"query":{}`)
	assert.NotContains(t, out, `"query":null`)
}

func TestAppendEnvelopeJSON_QueryMultiValueArrayShape(t *testing.T) {
	env := &GatewayRequest{
		Query: map[string]QueryValue{
			"tags": NewQueryValueStrings([]string{"a", "b"}),
		},
	}

	out := string(appendEnvelopeJSON(nil, env))

	assert.Contains(t, out, `"query":{"tags":["a","b"]}`)
}

func TestAppendEnvelopeJSON_QuerySingleValueStringShape(t *testing.T) {
	env := &GatewayRequest{
		Query: map[string]QueryValue{
			"include": NewQueryValueString("profile"),
		},
	}

	out := string(appendEnvelopeJSON(nil, env))

	assert.Contains(t, out, `"query":{"include":"profile"}`)
	assert.NotContains(t, out, `"query":{"include":["profile"]}`)
}

func TestAppendEnvelopeJSON_SingleElementMultiStillArray(t *testing.T) {
	// Guards the "repeated-key semantics even for one element" rule.
	env := &GatewayRequest{
		Query: map[string]QueryValue{
			"tag": NewQueryValueStrings([]string{"solo"}),
		},
	}

	out := string(appendEnvelopeJSON(nil, env))

	assert.Contains(t, out, `"query":{"tag":["solo"]}`)
}

// TestAppendEnvelopeJSON_OmitsAuthWhenNil pins the contract that the
// Auth field is only emitted when the gateway resolved claims for a
// protected route. Public routes must produce envelopes byte-identical
// to the pre-auth gateway build — otherwise consumers running a
// pre-auth SDK version see an unexpected `auth` key and potentially
// break on strict-parse JSON codecs.
func TestAppendEnvelopeJSON_OmitsAuthWhenNil(t *testing.T) {
	env := fullEnvelope()
	env.Auth = nil

	out := string(appendEnvelopeJSON(nil, env))

	assert.NotContains(t, out, `"auth"`)
}

func TestAppendEnvelopeJSON_EmitsAuthWhenPresent(t *testing.T) {
	env := fullEnvelope()
	env.Auth = json.RawMessage(`{"userId":"u1","roles":["admin"]}`)

	out := string(appendEnvelopeJSON(nil, env))

	assert.Contains(t, out, `"auth":{"userId":"u1","roles":["admin"]}`)

	// Strongest guarantee: the envelope must still round-trip through
	// a standards-compliant JSON parser after the auth slice is spliced
	// in — nothing about the raw append should break the surrounding
	// JSON structure.
	var decoded map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &decoded))

	authField, ok := decoded["auth"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "u1", authField["userId"])
}
