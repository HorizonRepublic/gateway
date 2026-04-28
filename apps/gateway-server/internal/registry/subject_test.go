package registry

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSubjectFromKey_ValidKey(t *testing.T) {
	subject, err := SubjectFromKey("users-svc.cmd.users.create")
	require.NoError(t, err)
	assert.Equal(t, "users-svc__microservice.cmd.users.create", subject)
}

func TestSubjectFromKey_NestedPattern(t *testing.T) {
	// Pattern itself contains dots ("orders.v2.update") — parser must
	// split on the first ".cmd." occurrence, not on every dot.
	subject, err := SubjectFromKey("orders.cmd.orders.v2.update")
	require.NoError(t, err)
	assert.Equal(t, "orders__microservice.cmd.orders.v2.update", subject)
}

func TestSubjectFromKey_MissingSeparator(t *testing.T) {
	// Event keys use ".ev." instead of ".cmd." — this parser rejects them
	// because they do not map to an RPC subject.
	_, err := SubjectFromKey("users-svc.ev.users.created")
	assert.Error(t, err)
}

func TestSubjectFromKey_EmptyKey(t *testing.T) {
	_, err := SubjectFromKey("")
	assert.Error(t, err)
}

func TestSubjectFromKey_EmptyServiceName(t *testing.T) {
	_, err := SubjectFromKey(".cmd.users.create")
	assert.Error(t, err)
}

func TestSubjectFromKey_EmptyPattern(t *testing.T) {
	_, err := SubjectFromKey("users-svc.cmd.")
	assert.Error(t, err)
}
