//go:build bench

package bench

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	vegeta "github.com/tsenart/vegeta/v12/lib"
)

func TestScenarios_TargetShapes(t *testing.T) {
	base := "http://gateway.local:8080"
	byName := map[string]Scenario{}
	for _, s := range Scenarios(base) {
		byName[s.Name] = s
	}
	require.Len(t, byName, 3)

	var tgt vegeta.Target
	require.NoError(t, byName["proxy-echo"].Targeter(&tgt))
	assert.Equal(t, "GET", tgt.Method)
	assert.Equal(t, base+"/users/alice", tgt.URL)
	assert.Empty(t, tgt.Header.Get("Authorization"))

	require.NoError(t, byName["auth-verify"].Targeter(&tgt))
	assert.Equal(t, base+"/me", tgt.URL)
	assert.Equal(t, "Bearer demo-alice", tgt.Header.Get("Authorization"))

	require.NoError(t, byName["rate-limited"].Targeter(&tgt))
	assert.Equal(t, base+"/rl/bench", tgt.URL)
}
