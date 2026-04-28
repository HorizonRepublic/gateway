package nats

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestJoinNATSURLs_HappyPath pins the canonical comma-joined output
// nats.go expects for clustered URL lists.
func TestJoinNATSURLs_HappyPath(t *testing.T) {
	got, err := joinNATSURLs([]string{"nats://a:4222", "nats://b:4222"})
	require.NoError(t, err)
	assert.Equal(t, "nats://a:4222,nats://b:4222", got)
}

// TestJoinNATSURLs_DropsBlankEntries verifies a stray blank entry in
// the operator-supplied list does not propagate to nats.go as a
// malformed URL. Cleaning happens before the empty-list check so a
// list of all blanks falls through to ErrNoNATSURLs.
func TestJoinNATSURLs_DropsBlankEntries(t *testing.T) {
	got, err := joinNATSURLs([]string{"", "nats://primary:4222", "  "})
	require.NoError(t, err)
	assert.Equal(t, "nats://primary:4222", got)
}

// TestJoinNATSURLs_EmptyListReturnsSentinel pins the defense-in-depth
// guard at the entry of Connect. nats.go silently falls back to
// localhost when given an empty URL string, which is a startup-time
// misconfiguration trap. The sentinel error keeps the bootstrap
// honest.
func TestJoinNATSURLs_EmptyListReturnsSentinel(t *testing.T) {
	cases := [][]string{
		nil,
		{},
		{""},
		{"   ", "\t"},
	}

	for _, urls := range cases {
		got, err := joinNATSURLs(urls)
		require.ErrorIs(t, err, ErrNoNATSURLs, "empty URL list must surface ErrNoNATSURLs")
		assert.Empty(t, got)
	}
}
