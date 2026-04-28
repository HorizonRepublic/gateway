package codec

import (
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
