package ratelimit

import (
	"errors"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/routing"
)

func TestFailPolicyOpen_Allows(t *testing.T) {
	p := FailPolicy("open").Resolve()
	assert.True(t, p.Apply(errors.New("x"), routing.Route{}, "k", zerolog.Nop()))
}

func TestFailPolicyClosed_Rejects(t *testing.T) {
	p := FailPolicy("closed").Resolve()
	assert.False(t, p.Apply(errors.New("x"), routing.Route{}, "k", zerolog.Nop()))
}

func TestFailPolicyUnknown_FallsBackToOpen(t *testing.T) {
	p := FailPolicy("garbage").Resolve()
	assert.True(t, p.Apply(errors.New("x"), routing.Route{}, "k", zerolog.Nop()))
}

func TestFailPolicyEmpty_FallsBackToOpen(t *testing.T) {
	// Back-compat: uninitialised string defaults to open.
	p := FailPolicy("").Resolve()
	assert.True(t, p.Apply(errors.New("x"), routing.Route{}, "k", zerolog.Nop()))
}
