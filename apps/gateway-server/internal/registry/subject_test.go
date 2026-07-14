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

func TestServiceFromSubject_WellFormedSubject(t *testing.T) {
	assert.Equal(t, "users-svc",
		ServiceFromSubject("users-svc__microservice.cmd.users.create"))
}

func TestServiceFromSubject_PatternWithDots(t *testing.T) {
	// Pattern dots must not confuse the extraction — the service name
	// is everything before the "__microservice.cmd." marker.
	assert.Equal(t, "orders",
		ServiceFromSubject("orders__microservice.cmd.orders.v2.update"))
}

func TestServiceFromSubject_RoundTripsWithSubjectFromKey(t *testing.T) {
	subject, err := SubjectFromKey("billing-svc.cmd.invoices.pay")
	require.NoError(t, err)
	assert.Equal(t, "billing-svc", ServiceFromSubject(subject))
}

func TestServiceFromSubject_MarkerAbsentFallsBackToWholeSubject(t *testing.T) {
	// A subject that does not follow the nestjs-jetstream convention
	// still gets a stable identity: itself. Callers grouping by service
	// then degrade to per-subject granularity instead of misgrouping.
	assert.Equal(t, "some.other.subject",
		ServiceFromSubject("some.other.subject"))
}

func TestServiceFromSubject_EmptyServicePrefixFallsBack(t *testing.T) {
	// Degenerate subject where the marker is present but the service
	// prefix is empty — fall back to the whole subject rather than
	// returning "" (which would collapse all such subjects into one
	// identity).
	assert.Equal(t, "__microservice.cmd.x",
		ServiceFromSubject("__microservice.cmd.x"))
}
