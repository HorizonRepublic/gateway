package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQueryValue_MarshalScalar(t *testing.T) {
	value := NewQueryValueString("alice")

	encoded, err := json.Marshal(value)

	require.NoError(t, err)
	assert.Equal(t, `"alice"`, string(encoded))
}

func TestQueryValue_MarshalEmptyScalar(t *testing.T) {
	value := NewQueryValueString("")

	encoded, err := json.Marshal(value)

	require.NoError(t, err)
	assert.Equal(t, `""`, string(encoded))
}

func TestQueryValue_MarshalMultiPreservesArrayShape(t *testing.T) {
	value := NewQueryValueStrings([]string{"a", "b", "c"})

	encoded, err := json.Marshal(value)

	require.NoError(t, err)
	assert.Equal(t, `["a","b","c"]`, string(encoded))
}

func TestQueryValue_MarshalSingleElementMultiStaysArray(t *testing.T) {
	// A repeated key that happens to have arrived with one observation
	// MUST still marshal as an array, so the NestJS handler's
	// Array.isArray() discriminator preserves the "repeated" semantics.
	value := NewQueryValueStrings([]string{"only"})

	encoded, err := json.Marshal(value)

	require.NoError(t, err)
	assert.Equal(t, `["only"]`, string(encoded))
}

func TestNewQueryValueStrings_NormalizesNilToEmptySlice(t *testing.T) {
	value := NewQueryValueStrings(nil)

	encoded, err := json.Marshal(value)

	require.NoError(t, err)
	assert.Equal(t, `[]`, string(encoded), "nil slice must normalize to empty array variant")
}

func TestQueryValue_UnmarshalScalar(t *testing.T) {
	var value QueryValue

	require.NoError(t, json.Unmarshal([]byte(`"alice"`), &value))
	assert.Equal(t, "alice", value.Single)
	assert.Nil(t, value.Multi)
}

func TestQueryValue_UnmarshalArray(t *testing.T) {
	var value QueryValue

	require.NoError(t, json.Unmarshal([]byte(`["a","b"]`), &value))
	assert.Equal(t, []string{"a", "b"}, value.Multi)
	assert.Equal(t, "", value.Single)
}

func TestQueryValue_UnmarshalRejectsNumber(t *testing.T) {
	var value QueryValue

	err := json.Unmarshal([]byte(`42`), &value)

	assert.Error(t, err, "non-string, non-array values must be rejected")
}

func TestQueryValue_UnmarshalRejectsObject(t *testing.T) {
	var value QueryValue

	err := json.Unmarshal([]byte(`{"k":"v"}`), &value)

	assert.Error(t, err)
}

// TestQueryValue_UnmarshalRejectedShapesProduceFormattableErrors guards
// against a regression where the default branch returned a
// *json.UnmarshalTypeError with Type: nil. That value panics with a
// nil pointer dereference the first time anyone calls its Error()
// method — which is exactly what happens in log lines, fmt.Errorf %w
// wrapping, or test assertions. Walk every rejected shape, format the
// error to force Error() to run, and assert the message carries
// context an operator can act on.
func TestQueryValue_UnmarshalRejectedShapesProduceFormattableErrors(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"number", `42`},
		{"boolean_true", `true`},
		{"boolean_false", `false`},
		{"null", `null`},
		{"object", `{"k":"v"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var value QueryValue

			err := json.Unmarshal([]byte(tc.input), &value)
			require.Error(t, err)

			msg := err.Error()
			assert.NotEmpty(t, msg,
				"error must carry a non-empty message so logs and %%w wrapping stay useful")
			assert.Contains(t, msg, "query value",
				"error must identify the failing site so operators can trace source")
			assert.Contains(t, msg, tc.input,
				"error must include the offending JSON payload for diagnostics")

			wrapped := fmt.Errorf("decode envelope: %w", err)
			assert.Contains(t, wrapped.Error(), "query value",
				"wrapping with %%w must not panic or drop the underlying message")
		})
	}
}

func TestQueryValue_UnmarshalEmptyInputReturnsSentinel(t *testing.T) {
	// Empty input used to surface as json.SyntaxError{} whose Error()
	// returns a blank string; callers got no signal to diagnose with.
	// The sentinel error carries a descriptive message and is
	// matchable via errors.Is so it can be recognised downstream.
	var value QueryValue

	err := value.UnmarshalJSON(nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, errQueryValueEmptyJSON),
		"empty input must surface the sentinel so callers can branch on it")
	assert.NotEmpty(t, err.Error())
}

func TestQueryValue_RoundTripInsideMap(t *testing.T) {
	original := map[string]QueryValue{
		"include": NewQueryValueString("profile"),
		"tags":    NewQueryValueStrings([]string{"go", "nats"}),
	}

	encoded, err := json.Marshal(original)
	require.NoError(t, err)

	var decoded map[string]QueryValue
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	assert.Equal(t, original, decoded)
}
