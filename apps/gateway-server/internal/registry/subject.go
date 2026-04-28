package registry

import (
	"fmt"
	"strings"
)

// kvKeySeparator is the nestjs-jetstream convention for separating the
// service name, stream kind, and pattern within a handler_registry KV key.
//
// Example key: "users-svc.cmd.users.create"
//
//	serviceName = "users-svc"
//	streamKind  = "cmd"
//	pattern     = "users.create"
//
// The pattern may itself contain dots (as shown above), which is why the
// parser looks for the ".cmd." infix rather than splitting on "." directly.
const kvKeySeparator = ".cmd."

// subjectSuffix is the nestjs-jetstream convention for building the full
// NATS subject from a service name and pattern. The "__microservice" infix
// is how nestjs-jetstream namespaces its Core RPC subjects so they do not
// collide with event or broadcast subjects in the same cluster.
//
// COMPATIBILITY NOTE: if nestjs-jetstream ever changes this convention,
// this file MUST be updated in lockstep and the gateway's major version
// MUST be bumped. Subject naming is the single desync risk in the
// gateway integration — every other cross-repo contract is driven by
// data (KV entries, envelope JSON), but this one is a hardcoded
// convention shared between both sides.
const subjectSuffix = "__microservice.cmd."

// SubjectFromKey reconstructs the full NATS RPC subject from a KV key in
// the handler_registry bucket. Returns an error if the key is malformed
// (missing the ".cmd." infix, empty service name, or empty pattern).
//
// Example:
//
//	SubjectFromKey("users-svc.cmd.users.create")
//	// returns: "users-svc__microservice.cmd.users.create", nil
//
// Malformed keys are skipped during routing-table construction rather than
// failing the whole rebuild, because a single bad entry in KV should not
// take the entire gateway offline. The caller is expected to log the
// returned error and move on.
func SubjectFromKey(key string) (string, error) {
	idx := strings.Index(key, kvKeySeparator)
	if idx == -1 {
		return "", fmt.Errorf("registry: malformed KV key %q (missing %q marker)", key, kvKeySeparator)
	}
	serviceName := key[:idx]
	pattern := key[idx+len(kvKeySeparator):]
	if serviceName == "" || pattern == "" {
		return "", fmt.Errorf("registry: empty service or pattern in KV key %q", key)
	}
	return serviceName + subjectSuffix + pattern, nil
}
