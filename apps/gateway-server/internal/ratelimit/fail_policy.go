package ratelimit

import (
	"github.com/rs/zerolog"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/routing"
)

// FailPolicy is the operator-selected behaviour for backend failures.
// It determines whether requests should be allowed or rejected when the
// rate-limit store itself becomes unavailable (network error, circuit breaker
// open, CAS budget exhausted, etc.). This decouples rate-limit correctness
// from transport reliability.
//
// Implementations MUST log the decision with structured fields so operators
// can see failure-mode rejections/allows in aggregate across replicas.
type FailPolicy string

const (
	// FailPolicyOpen allows requests through on backend failure.
	// Default; favors availability over strict rate-limit enforcement
	// when the store is unavailable. Safe for high-traffic deployments
	// where temporary loss of rate-limiting is preferable to rejecting
	// valid requests.
	FailPolicyOpen FailPolicy = "open"

	// FailPolicyClosed rejects requests on backend failure.
	// For strict-compliance deployments that must never allow unbounded
	// traffic, even if the rate-limit store is temporarily offline.
	FailPolicyClosed FailPolicy = "closed"
)

// Policy is the runtime-resolved allow/reject decision surface for
// distributed store failures. Apply is called by the handler when
// Store.Allow returns a non-nil error; the implementation decides
// whether to allow or reject the request.
type Policy interface {
	// Apply is called when Store.Allow returned a non-nil error.
	// Returns whether the request should proceed.
	// The implementation MUST log the decision with structured fields
	// so operators can see failure-mode rejections/allows in aggregate.
	//
	// Parameters:
	// - err: the error returned by Store.Allow
	// - route: the routing.Route being processed (contains Method, PathTemplate, Subject)
	// - key: the rate-limit bucket key (e.g., user ID, API token, IP)
	// - logger: zerolog.Logger instance for structured logging
	Apply(err error, route routing.Route, key string, logger zerolog.Logger) bool
}

// Resolve returns the Policy implementation for the selected FailPolicy.
// Unknown or empty values fall back to open (safe default for availability-first).
// Empty string is treated the same as FailPolicyOpen for backwards compatibility.
func (fp FailPolicy) Resolve() Policy {
	switch fp {
	case FailPolicyClosed:
		return closedPolicy{}
	case FailPolicyOpen, "":
		return openPolicy{}
	default:
		// Unknown values default to open for maximum safety (availability first).
		return openPolicy{}
	}
}

type openPolicy struct{}

// Apply returns true (allow on failure). Logs the decision at warn level.
// The key parameter is part of the Policy contract but not read here —
// open-on-failure logs are route-scoped, not key-scoped, to keep error
// volume bounded when a backend outage sprays errors across every bucket.
func (openPolicy) Apply(err error, route routing.Route, _ string, logger zerolog.Logger) bool {
	logger.Warn().
		Err(err).
		Str("route", route.Method+":"+route.PathTemplate).
		Str("policy", "open").
		Msg("ratelimit.store.failure.allowed")
	return true
}

type closedPolicy struct{}

// Apply returns false (reject on failure). Logs the decision at warn level.
// The key parameter is part of the Policy contract but not read here —
// closed-on-failure logs are route-scoped so a backend outage does not
// fan out into per-key log lines across the fleet.
func (closedPolicy) Apply(err error, route routing.Route, _ string, logger zerolog.Logger) bool {
	logger.Warn().
		Err(err).
		Str("route", route.Method+":"+route.PathTemplate).
		Str("policy", "closed").
		Msg("ratelimit.store.failure.rejected")
	return false
}
