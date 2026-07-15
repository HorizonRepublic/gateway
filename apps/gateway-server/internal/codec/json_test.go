package codec

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMarshalUnmarshal_RoundTrip(t *testing.T) {
	type payload struct {
		Name string `json:"name"`
		Age  int    `json:"age"`
	}
	original := payload{Name: "alice", Age: 30}

	encoded, err := Marshal(original)
	require.NoError(t, err)
	assert.JSONEq(t, `{"name":"alice","age":30}`, string(encoded))

	var decoded payload
	require.NoError(t, Unmarshal(encoded, &decoded))
	assert.Equal(t, original, decoded)
}

func TestUnmarshal_PropagatesError(t *testing.T) {
	var target struct {
		Value int `json:"value"`
	}
	err := Unmarshal([]byte(`{"value": "not-a-number"}`), &target)
	assert.Error(t, err)
}

func TestMarshal_ReturnsFreshSliceEachCall(t *testing.T) {
	// Defensive: callers rely on the fact that Marshal does not hand
	// out a shared backing array. If sonic ever changed this, pooled
	// buffer reuse upstream would see cross-request corruption.
	value := map[string]int{"a": 1}

	first, err := Marshal(value)
	require.NoError(t, err)

	second, err := Marshal(value)
	require.NoError(t, err)

	assert.Equal(t, first, second)
	// Identity check: different underlying arrays.
	if len(first) > 0 && len(second) > 0 {
		firstHeader := &first[0]
		secondHeader := &second[0]
		assert.NotSame(t, firstHeader, secondHeader, "Marshal must return independently-allocated slices")
	}
}

// TestValid_MatchesStrictParsersOnControlChars pins the intake-guard
// contract: Valid must reject documents that strict RFC 8259 parsers
// reject, otherwise the proxy forwards a body that the upstream
// decoder cannot parse and the client sees a 5xx instead of a 400.
// sonic's SIMD validator alone accepts raw control characters inside
// string values; the two-phase Valid must catch them while still
// accepting legal inter-token whitespace.
func TestValid_MatchesStrictParsersOnControlChars(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  bool
	}{
		{"compact object", `{"a":1}`, true},
		{"pretty printed", "{\n\t\"a\": 1\r\n}", true},
		{"raw 0x01 in string", "\"a\x01b\"", false},
		{"raw newline in string", "\"a\nb\"", false},
		{"raw NUL in string", "\"a\x00b\"", false},
		{"raw 0x01 in object key", "{\"a\x01\":1}", false},
		{"escaped control char", "\"a\\u0001b\"", true},
		{"legal escapes", `"a\n\t\\\"\u00e9b"`, true},
		{"invalid escape backslash-zero", `"\0"`, false},
		{"invalid escape backslash-x", `"\x41"`, false},
		{"lone trailing backslash", `"\"`, false},
		{"raw DEL is legal", "\"a\x7fb\"", true},
		{"truncated null literal", "n", false},
		{"truncated true literal", "tru", false},
		{"complete literals", "[null,true,false]", true},
		{"not json at all", "control \x01 outside", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, Valid([]byte(tc.input)))
			if tc.want {
				assert.True(t, json.Valid([]byte(tc.input)),
					"accepted documents must also satisfy the strict stdlib validator")
			}
		})
	}
}

// TestUnmarshal_SanitisesInvalidUTF8 pins the trust-boundary contract:
// JSON arriving from NATS (upstream replies, KV entries) may come from
// non-SDK publishers. RFC 8259 §8.1 makes UTF-8 a MUST for
// inter-system exchange; a decoder that passes invalid bytes through
// verbatim would forward them to public HTTP clients under
// application/json. The decode path must sanitise invalid sequences
// to U+FFFD exactly like encoding/json does.
func TestUnmarshal_SanitisesInvalidUTF8(t *testing.T) {
	raw := []byte("{\"v\":\"a\x80b\"}")

	var stdGot map[string]string
	require.NoError(t, json.Unmarshal(raw, &stdGot), "encoding/json accepts and sanitises")

	var got map[string]string
	require.NoError(t, Unmarshal(raw, &got))

	assert.Equal(t, stdGot["v"], got["v"],
		"codec.Unmarshal must match encoding/json's U+FFFD sanitisation")
	assert.Equal(t, "a�b", got["v"])
}
