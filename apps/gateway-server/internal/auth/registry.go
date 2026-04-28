// Package auth holds the gateway-side auth machinery that consumes
// the handler_registry KV snapshot: verifier lookup, auth flow
// orchestration, and (future) result caching.
//
// The package is structurally parallel to internal/routing: both
// project the same registry.Snapshot into a lookup-optimised data
// structure that the proxy handler consults on every request. Kept
// separate so auth semantics never leak into the routing layer and
// vice versa.
package auth

import (
	"sort"

	"github.com/rs/zerolog"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/registry"
)

// VerifierRegistry is an immutable id-keyed index of verifier NATS
// subjects built from a registry.Snapshot. Instances are produced by
// BuildVerifierRegistry and published atomically by the caller
// through the same rebuild callback that swaps the routing table.
//
// The zero value is NOT valid — callers must go through
// BuildVerifierRegistry, which always returns a non-nil pointer
// (possibly with an empty lookup map for empty snapshots).
type VerifierRegistry struct {
	idToSubject map[string]string
	defaultID   string
}

// Lookup returns the fully-qualified NATS subject for a given
// verifier id. The bool return discriminates "not found" from an
// empty string so callers never accidentally send to the zero
// subject.
func (r *VerifierRegistry) Lookup(id string) (string, bool) {
	subject, ok := r.idToSubject[id]

	return subject, ok
}

// LookupDefault returns the subject of the default verifier when
// exactly one verifier set `default: true` at build time. Routes
// that declare an auth block without naming a verifier explicitly
// resolve through this path.
func (r *VerifierRegistry) LookupDefault() (string, bool) {
	if r.defaultID == "" {
		return "", false
	}

	return r.Lookup(r.defaultID)
}

// BuildVerifierRegistry projects a registry.Snapshot into a
// VerifierRegistry.
//
// Non-verifier entries (routes, pure-RPC handlers) are silently
// ignored. Malformed KV keys that fail registry.SubjectFromKey are
// skipped with a WARN log. Id and default collisions resolve
// deterministically to the lexicographically-smallest KV key so
// gateway pods agree on the same mapping without coordination —
// this is important for rolling deploys and for reconciling after
// a pod crash.
//
// Callers publish the returned registry via atomic swap alongside
// the routing table; see cmd/gateway/main.go for the bootstrap
// wiring.
func BuildVerifierRegistry(snapshot *registry.Snapshot, logger zerolog.Logger) *VerifierRegistry {
	type candidate struct {
		key     string
		subject string
		meta    *registry.VerifierMeta
	}

	// Collect candidates first so we can sort by key before resolving
	// collisions. Go's map iteration order is randomized, which would
	// otherwise let two pods disagree on which collider wins.
	candidates := make([]candidate, 0, len(snapshot.Entries))

	for key, entry := range snapshot.Entries {
		if entry.Verifier == nil {
			continue
		}

		subject, err := registry.SubjectFromKey(key)
		if err != nil {
			logger.Warn().
				Err(err).
				Str("key", key).
				Msg("auth: skipping verifier entry with malformed KV key")

			continue
		}

		candidates = append(candidates, candidate{key: key, subject: subject, meta: entry.Verifier})
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].key < candidates[j].key
	})

	idToSubject := make(map[string]string, len(candidates))
	defaultID := ""

	for _, c := range candidates {
		if existing, ok := idToSubject[c.meta.ID]; ok {
			logger.Warn().
				Str("id", c.meta.ID).
				Str("chosen", existing).
				Str("ignored", c.subject).
				Msg("auth: verifier id collision — keeping first lexicographic key")

			continue
		}

		idToSubject[c.meta.ID] = c.subject

		if c.meta.Default {
			if defaultID != "" {
				logger.Error().
					Str("previous", defaultID).
					Str("ignored", c.meta.ID).
					Msg("auth: multiple verifiers claim default:true — keeping first lexicographic key")

				continue
			}

			defaultID = c.meta.ID
		}
	}

	return &VerifierRegistry{
		idToSubject: idToSubject,
		defaultID:   defaultID,
	}
}
