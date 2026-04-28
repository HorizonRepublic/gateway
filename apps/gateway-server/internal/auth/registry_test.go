package auth

import (
	"io"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/registry"
)

// silentLogger returns a zerolog.Logger that discards output, so tests
// can build verifier registries over scenarios that intentionally log
// WARN / ERROR without polluting test output.
func silentLogger() zerolog.Logger {
	return zerolog.New(io.Discard).Level(zerolog.Disabled)
}

func TestBuildVerifierRegistry_PopulatesIdToSubjectMap(t *testing.T) {
	snapshot := &registry.Snapshot{
		Entries: map[string]registry.HandlerEntry{
			"users-svc.cmd.auth.verifier.jwt": {
				Verifier: &registry.VerifierMeta{ID: "jwt", Default: true},
			},
			"users-svc.cmd.auth.verifier.session": {
				Verifier: &registry.VerifierMeta{ID: "session"},
			},
			// Route entry must be ignored by the verifier builder.
			"users-svc.cmd.users.get": {
				HTTP: &registry.HTTPMeta{Method: "GET", Path: "/users/:id"},
			},
		},
	}

	sut := BuildVerifierRegistry(snapshot, silentLogger())

	jwtSubject, ok := sut.Lookup("jwt")
	require.True(t, ok)
	assert.Equal(t, "users-svc__microservice.cmd.auth.verifier.jwt", jwtSubject)

	sessionSubject, ok := sut.Lookup("session")
	require.True(t, ok)
	assert.Equal(t, "users-svc__microservice.cmd.auth.verifier.session", sessionSubject)

	defaultSubject, ok := sut.LookupDefault()
	require.True(t, ok)
	assert.Equal(t, "users-svc__microservice.cmd.auth.verifier.jwt", defaultSubject)
}

func TestBuildVerifierRegistry_NoDefaultWhenAbsent(t *testing.T) {
	snapshot := &registry.Snapshot{
		Entries: map[string]registry.HandlerEntry{
			"users-svc.cmd.auth.verifier.session": {
				Verifier: &registry.VerifierMeta{ID: "session"},
			},
		},
	}

	sut := BuildVerifierRegistry(snapshot, silentLogger())

	_, ok := sut.LookupDefault()
	assert.False(t, ok)
}

func TestBuildVerifierRegistry_IDCollisionFirstLexicographicKey(t *testing.T) {
	// Two services both register an `id: 'jwt'` verifier. The registry
	// must pick the entry with the lexicographically-smallest KV key so
	// that gateway pods agree on the same subject without coordination.
	snapshot := &registry.Snapshot{
		Entries: map[string]registry.HandlerEntry{
			"z-svc.cmd.auth.verifier.jwt": {
				Verifier: &registry.VerifierMeta{ID: "jwt"},
			},
			"a-svc.cmd.auth.verifier.jwt": {
				Verifier: &registry.VerifierMeta{ID: "jwt"},
			},
		},
	}

	sut := BuildVerifierRegistry(snapshot, silentLogger())

	subject, ok := sut.Lookup("jwt")
	require.True(t, ok)
	assert.Equal(t, "a-svc__microservice.cmd.auth.verifier.jwt", subject,
		"lexicographically smallest key wins")
}

func TestBuildVerifierRegistry_DefaultCollisionFirstLexicographicKey(t *testing.T) {
	// Two verifiers both claim default:true with DIFFERENT ids. Only one
	// wins the default slot, deterministically the one with the smaller
	// KV key. The other verifier is still available via explicit lookup.
	snapshot := &registry.Snapshot{
		Entries: map[string]registry.HandlerEntry{
			"z-svc.cmd.auth.verifier.z-verifier": {
				Verifier: &registry.VerifierMeta{ID: "z-verifier", Default: true},
			},
			"a-svc.cmd.auth.verifier.a-verifier": {
				Verifier: &registry.VerifierMeta{ID: "a-verifier", Default: true},
			},
		},
	}

	sut := BuildVerifierRegistry(snapshot, silentLogger())

	defaultSubject, ok := sut.LookupDefault()
	require.True(t, ok)
	assert.Equal(t, "a-svc__microservice.cmd.auth.verifier.a-verifier", defaultSubject)

	// Both verifiers must remain looked-uppable by explicit id.
	_, okA := sut.Lookup("a-verifier")
	_, okZ := sut.Lookup("z-verifier")
	assert.True(t, okA)
	assert.True(t, okZ)
}

func TestBuildVerifierRegistry_SkipsMalformedKeys(t *testing.T) {
	snapshot := &registry.Snapshot{
		Entries: map[string]registry.HandlerEntry{
			"bogus-key-without-cmd-segment": {
				Verifier: &registry.VerifierMeta{ID: "ghost"},
			},
		},
	}

	sut := BuildVerifierRegistry(snapshot, silentLogger())

	_, ok := sut.Lookup("ghost")
	assert.False(t, ok)
}

func TestBuildVerifierRegistry_EmptySnapshot(t *testing.T) {
	snapshot := &registry.Snapshot{Entries: map[string]registry.HandlerEntry{}}

	sut := BuildVerifierRegistry(snapshot, silentLogger())

	_, ok := sut.Lookup("jwt")
	assert.False(t, ok)

	_, defaultOk := sut.LookupDefault()
	assert.False(t, defaultOk)
}
