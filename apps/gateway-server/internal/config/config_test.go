package config

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setRequiredEnv populates the minimum env contract — both required
// variables — so individual tests focus on the field under assertion
// without each repeating the boilerplate.
func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("NATS_URLS", "nats://localhost:4222")
	t.Setenv("KV_BUCKET", "handler_registry")
}

func TestLoad_AppliesDefaultsWhenOnlyRequiredSet(t *testing.T) {
	setRequiredEnv(t)

	cfg, err := Load()
	require.NoError(t, err)

	assert.Equal(t, ":8080", cfg.HTTPAddr)
	assert.Equal(t, 10*time.Second, cfg.ReadTimeout)
	assert.Equal(t, 35*time.Second, cfg.WriteTimeout)
	assert.Equal(t, 120*time.Second, cfg.IdleTimeout)
	assert.Equal(t, int64(1048576), cfg.MaxBodyBytes)
	assert.Equal(t, 16384, cfg.MaxHeaderBytes)

	assert.True(t, cfg.NATSRandomizeUrls)
	assert.True(t, cfg.NATSDiscoverServers)
	assert.Equal(t, 2*time.Second, cfg.NATSReconnectWait)
	assert.Equal(t, -1, cfg.NATSMaxReconnects)

	assert.Equal(t, "handler_registry", cfg.KVBucket)

	assert.Equal(t, 30*time.Second, cfg.RequestTimeout)
	assert.Equal(t, 30*time.Second, cfg.ShutdownTimeout)

	assert.Equal(t, "open", cfg.RateLimitFailPolicy)
	assert.Equal(t, 10*time.Minute, cfg.RateLimitKeyTTL)
	assert.Equal(t, 50*time.Millisecond, cfg.RateLimitTimeout)

	assert.Equal(t, "info", cfg.LogLevel)
	assert.Equal(t, "json", cfg.LogFormat)

	assert.Equal(t, "production", cfg.Environment)
	assert.True(t, cfg.IsProduction())
}

func TestLoad_ParsesMultipleNATSUrls(t *testing.T) {
	t.Setenv("KV_BUCKET", "handler_registry")
	t.Setenv("NATS_URLS", "nats://n1:4222,nats://n2:4222,nats://n3:4222")

	cfg, err := Load()
	require.NoError(t, err)

	assert.Equal(t, []string{
		"nats://n1:4222",
		"nats://n2:4222",
		"nats://n3:4222",
	}, cfg.NATSUrls)
}

func TestLoad_FailsWithoutRequiredNATSUrls(t *testing.T) {
	t.Setenv("KV_BUCKET", "handler_registry")
	original, wasSet := os.LookupEnv("NATS_URLS")
	require.NoError(t, os.Unsetenv("NATS_URLS"))
	t.Cleanup(func() {
		if wasSet {
			_ = os.Setenv("NATS_URLS", original)
		}
	})

	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "NATS_URLS")
}

func TestIsProduction_FalseForNonProductionEnv(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("ENVIRONMENT", "staging")

	cfg, err := Load()
	require.NoError(t, err)
	assert.False(t, cfg.IsProduction())
}

func TestLoad_HonorsCustomHTTPAddr(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("HTTP_ADDR", ":9000")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, ":9000", cfg.HTTPAddr)
}

func TestLoad_TrustedProxies_DefaultsToPrivateSentinel(t *testing.T) {
	setRequiredEnv(t)

	cfg, err := Load()
	require.NoError(t, err)

	assert.Equal(t, "private", cfg.TrustedProxiesRaw,
		"raw env value preserved for diagnostics")
	require.Len(t, cfg.TrustedProxies, 7,
		"private sentinel expands to 7 CIDRs at Load() time")
}

func TestLoad_TrustedProxies_EmptyString_TrustsNothing(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("TRUSTED_PROXIES", "")

	cfg, err := Load()
	require.NoError(t, err)

	assert.Empty(t, cfg.TrustedProxies,
		"empty string means trust nothing — always use peer IP")
}

func TestLoad_TrustedProxies_LiteralCIDRList(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("TRUSTED_PROXIES", "10.0.0.0/8,192.168.0.0/16")

	cfg, err := Load()
	require.NoError(t, err)

	require.Len(t, cfg.TrustedProxies, 2)
	assert.Equal(t, "10.0.0.0/8", cfg.TrustedProxies[0].String())
	assert.Equal(t, "192.168.0.0/16", cfg.TrustedProxies[1].String())
}

func TestLoad_TrustedProxies_InvalidCIDR_FailsStartupClosed(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("TRUSTED_PROXIES", "garbage")

	_, err := Load()
	require.Error(t, err,
		"invalid CIDR must fail Load() so main.go aborts startup rather than running in an unsafe state")
	assert.Contains(t, err.Error(), "TRUSTED_PROXIES",
		"error must name the offending env var for operator diagnosis")
	assert.Contains(t, err.Error(), "garbage",
		"error must include the invalid value")
}

// TestLoad_TrustedProxiesMalformed pins the operator-facing error
// surface when a CIDR list mixes a malformed entry with valid ones.
// resolver_test.go covers ParseCIDRList directly; this test asserts
// the wrapping at Load() preserves the offending substring so an
// operator scanning a startup log finds the bad token without
// needing to grep the source.
func TestLoad_TrustedProxiesMalformed(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("TRUSTED_PROXIES", "not-a-cidr,10.0.0.0/8")

	_, err := Load()
	require.Error(t, err,
		"any single malformed CIDR must fail Load() — fail-closed startup")
	assert.Contains(t, err.Error(), "TRUSTED_PROXIES",
		"error must name the offending env var for operator diagnosis")
	assert.Contains(t, err.Error(), "not-a-cidr",
		"error must include the malformed substring so the operator can spot it in the env value")
}

func TestLoad_RateLimitDefaults(t *testing.T) {
	setRequiredEnv(t)

	cfg, err := Load()
	require.NoError(t, err)

	assert.Equal(t, "open", cfg.RateLimitFailPolicy)
	assert.Equal(t, 10*time.Minute, cfg.RateLimitKeyTTL)
	assert.Equal(t, 50*time.Millisecond, cfg.RateLimitTimeout)
}

// TestLoad_RateLimitTimeout pins the per-request rate-limit gate
// budget knob. The default keeps the gate well below typical route
// timeouts so a flaky distributed store cannot starve handler latency
// under retry pressure (CAS contention, breaker probes).
func TestLoad_RateLimitTimeout(t *testing.T) {
	t.Run("custom value within bounds", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("RATELIMIT_TIMEOUT", "100ms")

		cfg, err := Load()
		require.NoError(t, err)
		assert.Equal(t, 100*time.Millisecond, cfg.RateLimitTimeout)
	})

	t.Run("zero is rejected", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("RATELIMIT_TIMEOUT", "0s")

		_, err := Load()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "RATELIMIT_TIMEOUT")
	})

	t.Run("above 1s upper bound is rejected", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("RATELIMIT_TIMEOUT", "2s")

		_, err := Load()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "RATELIMIT_TIMEOUT")
	})

	t.Run("exact 1s upper bound accepted", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("RATELIMIT_TIMEOUT", "1s")

		cfg, err := Load()
		require.NoError(t, err)
		assert.Equal(t, time.Second, cfg.RateLimitTimeout)
	})
}

func TestLoad_RateLimitValidFailPolicyClosed(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("RATELIMIT_FAIL_POLICY", "closed")

	cfg, err := Load()
	require.NoError(t, err)

	assert.Equal(t, "closed", cfg.RateLimitFailPolicy)
}

func TestLoad_RateLimitInvalidFailPolicy(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("RATELIMIT_FAIL_POLICY", "garbage")

	_, err := Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "RATELIMIT_FAIL_POLICY")
}

func TestLoad_RateLimitCustomKeyTTL(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("RATELIMIT_KEY_TTL", "2m")

	cfg, err := Load()
	require.NoError(t, err)

	assert.Equal(t, 2*time.Minute, cfg.RateLimitKeyTTL)
}

// TestLoad_KVBucketRequired guards the production-safety contract that
// KV_BUCKET MUST be set explicitly. caarlos0/env treats explicit-empty
// as unset and would silently apply a default — risking cross-env data
// leakage if an operator clears the var in a deploy template.
func TestLoad_KVBucketRequired(t *testing.T) {
	t.Run("unset returns error", func(t *testing.T) {
		t.Setenv("NATS_URLS", "nats://localhost:4222")
		require.NoError(t, os.Unsetenv("KV_BUCKET"))

		_, err := Load()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "KV_BUCKET",
			"error must name the offending env var for operator diagnosis")
	})

	t.Run("empty string returns error", func(t *testing.T) {
		t.Setenv("NATS_URLS", "nats://localhost:4222")
		t.Setenv("KV_BUCKET", "")

		_, err := Load()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "KV_BUCKET")
	})

	t.Run("explicit value loads successfully", func(t *testing.T) {
		t.Setenv("NATS_URLS", "nats://localhost:4222")
		t.Setenv("KV_BUCKET", "my_handler_registry")

		cfg, err := Load()
		require.NoError(t, err)
		assert.Equal(t, "my_handler_registry", cfg.KVBucket)
	})
}

// TestLoad_TrustedProxyHeader_DefaultsToXForwardedFor pins the
// default header source: operators who do not set TRUSTED_PROXY_HEADER
// continue to see X-Forwarded-For — preserving the historical contract.
func TestLoad_TrustedProxyHeader_DefaultsToXForwardedFor(t *testing.T) {
	setRequiredEnv(t)

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "X-Forwarded-For", cfg.TrustedProxyHeader)
}

// TestLoad_TrustedProxyHeader_AcceptsAllowedHeaders pins the
// allowed-set contract: every supported alternative parses cleanly
// and is canonicalised to its conventional capitalisation.
func TestLoad_TrustedProxyHeader_AcceptsAllowedHeaders(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"X-Forwarded-For", "X-Forwarded-For"},
		{"x-forwarded-for", "X-Forwarded-For"},
		{"X-Real-IP", "X-Real-IP"},
		{"x-real-ip", "X-Real-IP"},
		{"X-REAL-IP", "X-Real-IP"},
		{"CF-Connecting-IP", "CF-Connecting-IP"},
		{"cf-connecting-ip", "CF-Connecting-IP"},
		{"True-Client-IP", "True-Client-IP"},
		{"true-client-ip", "True-Client-IP"},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv("TRUSTED_PROXY_HEADER", tc.input)

			cfg, err := Load()
			require.NoError(t, err)
			assert.Equal(t, tc.want, cfg.TrustedProxyHeader,
				"operator-supplied header must canonicalise to the conventional capitalisation")
		})
	}
}

// TestLoad_TrustedProxyHeader_RejectsUnknown pins the fail-closed
// behaviour: an unknown header name aborts startup so a typo in
// production cannot silently degrade the trust resolution.
func TestLoad_TrustedProxyHeader_RejectsUnknown(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{"typo", "X-Forwarded-Fro"},
		{"random-header", "X-Forwarded-By"},
		{"vendor-not-in-allowlist", "Fly-Client-IP"},
		{"x-amzn-trace-id-not-allowlisted", "X-Amzn-Trace-Id"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv("TRUSTED_PROXY_HEADER", tc.value)

			_, err := Load()
			require.Error(t, err,
				"unknown TRUSTED_PROXY_HEADER must fail Load() — startup aborts rather than running with an unsafe trust source")
			assert.Contains(t, err.Error(), "TRUSTED_PROXY_HEADER",
				"error must name the offending env var for operator diagnosis")
			assert.Contains(t, err.Error(), "unknown header",
				"error must name the failure mode")
		})
	}
}

// TestLoad_WriteTimeoutStrictlyExceedsRequestTimeout guards the
// invariant documented on Config.WriteTimeout: the HTTP write deadline
// must leave enough budget for the handler to emit a 504 after the
// request deadline fires. Shipping defaults where the two are equal
// would truncate the timeout response on the wire.
func TestLoad_WriteTimeoutStrictlyExceedsRequestTimeout(t *testing.T) {
	setRequiredEnv(t)

	cfg, err := Load()
	require.NoError(t, err)

	assert.Greater(t, cfg.WriteTimeout, cfg.RequestTimeout,
		"WriteTimeout must leave slack over RequestTimeout so a 504 can be written before the HTTP write deadline")
}

func TestLoad_ResilienceDefaults(t *testing.T) {
	t.Setenv("NATS_URLS", "nats://localhost:4222")
	t.Setenv("KV_BUCKET", "handler_registry")

	cfg, err := Load()
	require.NoError(t, err)

	assert.Equal(t, 0, cfg.NATSMaxInflight, "in-flight cap defaults to disabled")
	assert.Equal(t, 100*time.Millisecond, cfg.NATSInflightQueueTimeout)
	assert.True(t, cfg.CircuitBreakerEnabled, "breaker defaults ON — fail-closed protection for the 3am operator")
	assert.Equal(t, uint32(10), cfg.CircuitBreakerFailureThreshold)
	assert.Equal(t, 10*time.Second, cfg.CircuitBreakerRecoveryTimeout)
	assert.Equal(t, uint32(1), cfg.CircuitBreakerHalfOpenProbes)
	assert.Equal(t, 1024, cfg.CircuitBreakerMaxSubjects,
		"per-service breaker map defaults to a bounded cardinality cap")
}

func TestLoad_ResilienceValidation(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"negative inflight", map[string]string{"NATS_MAX_INFLIGHT": "-1"}, "NATS_MAX_INFLIGHT"},
		{"zero queue timeout", map[string]string{"NATS_INFLIGHT_QUEUE_TIMEOUT": "0"}, "NATS_INFLIGHT_QUEUE_TIMEOUT"},
		{"oversized queue timeout", map[string]string{"NATS_INFLIGHT_QUEUE_TIMEOUT": "11s"}, "NATS_INFLIGHT_QUEUE_TIMEOUT"},
		{"zero failure threshold", map[string]string{"CIRCUIT_BREAKER_FAILURE_THRESHOLD": "0"}, "CIRCUIT_BREAKER_FAILURE_THRESHOLD"},
		{"zero recovery timeout", map[string]string{"CIRCUIT_BREAKER_RECOVERY_TIMEOUT": "0"}, "CIRCUIT_BREAKER_RECOVERY_TIMEOUT"},
		{"zero half-open probes", map[string]string{"CIRCUIT_BREAKER_HALF_OPEN_PROBES": "0"}, "CIRCUIT_BREAKER_HALF_OPEN_PROBES"},
		{"zero breaker subject cap", map[string]string{"CIRCUIT_BREAKER_MAX_SUBJECTS": "0"}, "CIRCUIT_BREAKER_MAX_SUBJECTS"},
		{"negative breaker subject cap", map[string]string{"CIRCUIT_BREAKER_MAX_SUBJECTS": "-5"}, "CIRCUIT_BREAKER_MAX_SUBJECTS"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("NATS_URLS", "nats://localhost:4222")
			t.Setenv("KV_BUCKET", "handler_registry")
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			_, err := Load()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestLoad_BreakerDisabledSkipsBreakerValidation(t *testing.T) {
	t.Setenv("NATS_URLS", "nats://localhost:4222")
	t.Setenv("KV_BUCKET", "handler_registry")
	t.Setenv("CIRCUIT_BREAKER_ENABLED", "false")
	t.Setenv("CIRCUIT_BREAKER_FAILURE_THRESHOLD", "0")

	_, err := Load()
	require.NoError(t, err, "breaker knobs are not validated when the breaker is off")
}

func TestLoad_OperatorAddrDefaultsAndValidation(t *testing.T) {
	t.Setenv("NATS_URLS", "nats://localhost:4222")
	t.Setenv("KV_BUCKET", "handler_registry")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, ":8081", cfg.OperatorHTTPAddr, "operator listener defaults to :8081")

	t.Setenv("OPERATOR_HTTP_ADDR", ":8080")
	_, err = Load()
	require.Error(t, err, "operator port must never equal the public port")
	assert.Contains(t, err.Error(), "OPERATOR_HTTP_ADDR")
}

// TestLoad_AccessLogDefaultsToEnabled pins the production default:
// operators get the request trail out of the box, no config change
// required during an incident.
func TestLoad_AccessLogDefaultsToEnabled(t *testing.T) {
	setRequiredEnv(t)

	cfg, err := Load()

	require.NoError(t, err)
	assert.True(t, cfg.AccessLogEnabled)
}

// TestLoad_AccessLogCanBeDisabled pins the off switch for
// deployments whose edge already captures equivalent access logs.
func TestLoad_AccessLogCanBeDisabled(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("ACCESS_LOG_ENABLED", "false")

	cfg, err := Load()

	require.NoError(t, err)
	assert.False(t, cfg.AccessLogEnabled)
}
