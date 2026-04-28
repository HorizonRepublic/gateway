package proxy

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/registry"
)

func TestMatchOrigin_ExactMatch(t *testing.T) {
	cors := &registry.CORSMeta{Origins: []string{"https://app.example.com", "https://admin.example.com"}}

	assert.Equal(t, "https://app.example.com", MatchOrigin(cors, "https://app.example.com"))
}

func TestMatchOrigin_NoMatch(t *testing.T) {
	cors := &registry.CORSMeta{Origins: []string{"https://app.example.com"}}

	assert.Equal(t, "", MatchOrigin(cors, "https://evil.com"))
}

func TestMatchOrigin_Wildcard(t *testing.T) {
	cors := &registry.CORSMeta{Origins: []string{"*"}}

	assert.Equal(t, "*", MatchOrigin(cors, "https://anything.com"))
}

func TestMatchOrigin_EmptyOriginHeader(t *testing.T) {
	cors := &registry.CORSMeta{Origins: []string{"https://app.example.com"}}

	assert.Equal(t, "", MatchOrigin(cors, ""))
}

func TestMatchOrigin_NilCORSMeta_ReturnsEmpty(t *testing.T) {
	// Routes without CORS configured give handlers a nil meta pointer;
	// MatchOrigin must treat that as "no match" rather than panicking
	// on the loop over cors.Origins.
	assert.Equal(t, "", MatchOrigin(nil, "https://app.example.com"))
}

func TestBuildPreflightHeaders_FullConfig(t *testing.T) {
	cors := &registry.CORSMeta{
		Origins:     []string{"https://app.example.com"},
		Methods:     []string{"POST", "PUT"},
		Headers:     []string{"Content-Type", "Authorization"},
		Credentials: true,
		MaxAge:      3600,
	}

	h := BuildPreflightHeaders(cors, "https://app.example.com")

	assert.Equal(t, "https://app.example.com", h["Access-Control-Allow-Origin"])
	assert.Equal(t, "POST, PUT", h["Access-Control-Allow-Methods"])
	assert.Equal(t, "Content-Type, Authorization", h["Access-Control-Allow-Headers"])
	assert.Equal(t, "true", h["Access-Control-Allow-Credentials"])
	assert.Equal(t, "3600", h["Access-Control-Max-Age"])
	assert.Equal(t, "Origin", h["Vary"])
}

// TestBuildPreflightHeaders_WildcardEmitsVary pins the always-emit
// contract: even for wildcard CORS, Vary: Origin must reach the wire.
// In a mixed deployment where one route serves "*" and another serves
// an allowlist on the same path under different conditions, an
// intermediate CDN that does not see Vary will key on (URL, Method)
// alone and may serve the wrong preflight to the wrong origin.
// Always-emit removes the entire class of CDN-cache-confusion bugs.
func TestBuildPreflightHeaders_WildcardEmitsVary(t *testing.T) {
	cors := &registry.CORSMeta{Origins: []string{"*"}}

	h := BuildPreflightHeaders(cors, "*")

	assert.Equal(t, "*", h["Access-Control-Allow-Origin"])
	assert.Equal(t, "Origin", h["Vary"],
		"Vary: Origin MUST reach the wire on wildcard preflights for CDN cache correctness")
}

func TestBuildPreflightHeaders_WildcardWithCredentials_SkipsCredentialsHeader(t *testing.T) {
	// Browsers reject a preflight that pairs "*" with
	// Access-Control-Allow-Credentials: true. The SDK validator blocks
	// this combo at registration, but the emission path must stay
	// defensive against hand-crafted or legacy KV entries.
	cors := &registry.CORSMeta{
		Origins:     []string{"*"},
		Credentials: true,
	}

	h := BuildPreflightHeaders(cors, "*")

	assert.Equal(t, "*", h["Access-Control-Allow-Origin"])
	_, hasCreds := h["Access-Control-Allow-Credentials"]
	assert.False(t, hasCreds,
		"credentials header must be suppressed when echoing wildcard origin")
}

func TestBuildPreflightHeaders_MinimalConfig(t *testing.T) {
	cors := &registry.CORSMeta{Origins: []string{"https://app.example.com"}}

	h := BuildPreflightHeaders(cors, "https://app.example.com")

	assert.Equal(t, "https://app.example.com", h["Access-Control-Allow-Origin"])
	_, hasMethods := h["Access-Control-Allow-Methods"]
	assert.False(t, hasMethods)
	_, hasHeaders := h["Access-Control-Allow-Headers"]
	assert.False(t, hasHeaders)
	_, hasCreds := h["Access-Control-Allow-Credentials"]
	assert.False(t, hasCreds)
}

func TestBuildResponseCORSHeaders_IncludesOnlyResponseFields(t *testing.T) {
	cors := &registry.CORSMeta{
		Origins:     []string{"https://app.example.com"},
		Methods:     []string{"POST"},
		Headers:     []string{"Authorization"},
		Credentials: true,
		MaxAge:      3600,
	}

	h := BuildResponseCORSHeaders(cors, "https://app.example.com")

	assert.Equal(t, "https://app.example.com", h["Access-Control-Allow-Origin"])
	assert.Equal(t, "true", h["Access-Control-Allow-Credentials"])
	assert.Equal(t, "Origin", h["Vary"])

	_, hasMethods := h["Access-Control-Allow-Methods"]
	assert.False(t, hasMethods, "response should not include Allow-Methods")
	_, hasHeaders := h["Access-Control-Allow-Headers"]
	assert.False(t, hasHeaders, "response should not include Allow-Headers")
	_, hasMaxAge := h["Access-Control-Max-Age"]
	assert.False(t, hasMaxAge, "response should not include Max-Age")
}

func TestBuildResponseCORSHeaders_WildcardWithCredentials_SkipsCredentialsHeader(t *testing.T) {
	// Mirror of the preflight guard: the credentials header must not
	// accompany a wildcard origin on the main response either.
	cors := &registry.CORSMeta{
		Origins:     []string{"*"},
		Credentials: true,
	}

	h := BuildResponseCORSHeaders(cors, "*")

	assert.Equal(t, "*", h["Access-Control-Allow-Origin"])
	_, hasCreds := h["Access-Control-Allow-Credentials"]
	assert.False(t, hasCreds,
		"credentials header must be suppressed when echoing wildcard origin")
}

func TestBuildResponseCORSHeaders_EmitsDefaultExposeHeaderList(t *testing.T) {
	cors := &registry.CORSMeta{Origins: []string{"https://app.example.com"}}

	h := BuildResponseCORSHeaders(cors, "https://app.example.com")

	assert.Equal(t,
		"X-Request-Id, X-RateLimit-Limit, X-RateLimit-Remaining, X-RateLimit-Reset, Retry-After",
		h["Access-Control-Expose-Headers"],
	)
}

func TestBuildResponseCORSHeaders_EmitsCustomExposeHeaderList(t *testing.T) {
	cors := &registry.CORSMeta{
		Origins:       []string{"https://app.example.com"},
		ExposeHeaders: []string{"X-Trace-Id", "X-Server-Version"},
	}

	h := BuildResponseCORSHeaders(cors, "https://app.example.com")

	assert.Equal(t, "X-Trace-Id, X-Server-Version", h["Access-Control-Expose-Headers"])
}

func TestBuildResponseCORSHeaders_EmptyExposeHeadersFallsBackToDefault(t *testing.T) {
	cors := &registry.CORSMeta{
		Origins:       []string{"https://app.example.com"},
		ExposeHeaders: []string{},
	}

	h := BuildResponseCORSHeaders(cors, "https://app.example.com")

	assert.Equal(t,
		"X-Request-Id, X-RateLimit-Limit, X-RateLimit-Remaining, X-RateLimit-Reset, Retry-After",
		h["Access-Control-Expose-Headers"],
	)
}

// TestBuildResponseCORSHeaders_WildcardEmitsVary mirrors the wildcard
// always-Vary contract for the response-side builder. Same
// CDN-cache-correctness rationale.
func TestBuildResponseCORSHeaders_WildcardEmitsVary(t *testing.T) {
	cors := &registry.CORSMeta{Origins: []string{"*"}}

	h := BuildResponseCORSHeaders(cors, "*")

	assert.Equal(t, "*", h["Access-Control-Allow-Origin"])
	assert.Equal(t, "Origin", h["Vary"],
		"Vary: Origin MUST reach the wire on wildcard responses for CDN cache correctness")
}

// TestBuildResponseCORSHeaders_AllowlistEmitsVary pins the allowlist
// path: an exact-origin match also emits Vary: Origin so a CDN keying
// on (URL, Method, Origin) caches correctly when the same path is
// requested from a different allowlisted origin later.
func TestBuildResponseCORSHeaders_AllowlistEmitsVary(t *testing.T) {
	cors := &registry.CORSMeta{Origins: []string{"https://app.example.com", "https://admin.example.com"}}

	h := BuildResponseCORSHeaders(cors, "https://app.example.com")

	assert.Equal(t, "https://app.example.com", h["Access-Control-Allow-Origin"])
	assert.Equal(t, "Origin", h["Vary"])
}

// TestBuildPreflightHeaders_NilCORSReturnsNil pins the defensive
// guard at the top of the function. Existing callers (handler.go's
// preflight path) check cors != nil before invoking; the nil return
// is the second line of defense for future refactors that drop the
// outer check, ensuring the helper short-circuits with nil instead
// of panicking on cors.Methods et al.
func TestBuildPreflightHeaders_NilCORSReturnsNil(t *testing.T) {
	assert.NotPanics(t, func() {
		got := BuildPreflightHeaders(nil, "*")
		assert.Nil(t, got)
	})
}

// TestBuildResponseCORSHeaders_NilCORSReturnsNil mirrors the nil
// guard for the response-side CORS builder.
func TestBuildResponseCORSHeaders_NilCORSReturnsNil(t *testing.T) {
	assert.NotPanics(t, func() {
		got := BuildResponseCORSHeaders(nil, "*")
		assert.Nil(t, got)
	})
}
