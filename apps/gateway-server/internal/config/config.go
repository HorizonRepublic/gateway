// Package config loads and validates the gateway-server's
// operator-facing configuration from environment variables.
//
// All settings are read at process startup via
// github.com/caarlos0/env/v11. Missing required fields cause Load to return
// an error, which the caller MUST treat as fatal — starting the gateway
// with partial config would be far more dangerous than refusing to start.
// Hot-reload is not supported in the MVP; config changes require a pod
// restart, which is the expected operational model in Kubernetes rolling
// deployments.
//
// Per-endpoint configuration (HTTP method, path, statusCode) is NOT defined
// here. It lives in the handler_registry NATS KV bucket and is controlled
// by Nest services via the @GatewayRoute decorator from gateway-sdk. The
// split keeps infrastructure concerns (how the gateway talks to NATS,
// where it listens, how it logs) separate from application concerns
// (which HTTP routes map to which RPC subjects), letting platform teams
// and feature teams own each side independently.
package config

import (
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/trustedproxy"
)

// allowedTrustedProxyHeaders is the set of HTTP header names the
// trusted-proxy middleware accepts as the source of the client IP.
//
// Keys are lowercase MIME canonical-fold form because operator-supplied
// values are normalised to lowercase before lookup. The mapped value
// is the canonical capitalised spelling preserved on the parsed
// Config.TrustedProxyHeader so log lines and downstream consumers see
// the conventional form regardless of how the operator typed it.
//
// Adding a new vendor header (e.g. Fly-Client-IP) requires inserting
// it here AND extending the trusted-proxy middleware to know how to
// read its value (single-value verbatim vs comma-walk). Operators who
// configure an unknown header MUST get a startup error rather than a
// silent fallback to the default — a typo in production should fail
// closed.
var allowedTrustedProxyHeaders = map[string]string{
	"x-forwarded-for":  "X-Forwarded-For",
	"x-real-ip":        "X-Real-IP",
	"cf-connecting-ip": "CF-Connecting-IP",
	"true-client-ip":   "True-Client-IP",
}

// Config is the complete set of operator-controlled gateway parameters.
//
// Fields are grouped by concern (HTTP, NATS, registry, request lifecycle,
// logging, observability, health, runtime). The groupings match the
// environment-variable prefixes documented on each field and are the
// contract operators use to configure a running gateway pod.
//
// The struct is loaded once at startup by Load. After that it is treated
// as effectively immutable — no code should mutate fields on a live
// Config instance, because doing so would create data races with the
// many components that hold references to it.
type Config struct {
	// HTTPAddr is the TCP listen address for the public HTTP server,
	// in Go's standard host:port form (empty host binds all interfaces).
	HTTPAddr string `env:"HTTP_ADDR"       envDefault:":8080"`
	// ReadTimeout bounds how long the server will wait for a full
	// request (headers + body) to arrive from the client.
	ReadTimeout time.Duration `env:"HTTP_READ_TIMEOUT"  envDefault:"10s"`
	// WriteTimeout bounds how long the server will take to write the
	// full response back to the client before forcibly closing.
	//
	// INVARIANT: WriteTimeout MUST be strictly greater than
	// RequestTimeout. When a request hits the RequestTimeout deadline
	// the handler writes a 504 response; if the underlying HTTP write
	// deadline has already expired, the 504 is truncated or dropped on
	// the wire. Operators should keep several seconds of slack between
	// the two (the defaults are 35s vs 30s).
	WriteTimeout time.Duration `env:"HTTP_WRITE_TIMEOUT" envDefault:"35s"`
	// IdleTimeout bounds how long a keep-alive connection may sit
	// between requests before the server closes it.
	IdleTimeout time.Duration `env:"HTTP_IDLE_TIMEOUT"  envDefault:"120s"`
	// MaxBodyBytes is the maximum accepted request body size in bytes.
	// Requests exceeding this are rejected with 413 Payload Too Large.
	MaxBodyBytes int64 `env:"HTTP_MAX_BODY_BYTES"   envDefault:"1048576"`
	// MaxHeaderBytes is the maximum accepted request header size in
	// bytes, summed across all headers.
	MaxHeaderBytes int `env:"HTTP_MAX_HEADER_BYTES" envDefault:"16384"`

	// HTTPMaxConcurrentRequests caps the number of HTTP requests
	// the gateway processes simultaneously. When the cap is reached
	// the concurrency-limit middleware short-circuits new requests
	// with 503 Service Unavailable + Retry-After: 1 — they never
	// reach the trusted-proxy chain, the rate-limit gate, or the
	// proxy handler.
	//
	// Defends against slowloris-style attacks that hold connections
	// open without sending bytes (rate-limit cannot fire because no
	// request body has been parsed yet) and against thundering-herd
	// retries during upstream incidents (each in-flight request
	// holds a goroutine + ~8 KB stack).
	//
	// Recommended production default: 10000. Zero disables the
	// middleware (legacy unbounded behaviour). Negative values are
	// rejected at Load().
	HTTPMaxConcurrentRequests int `env:"HTTP_MAX_CONCURRENT_REQUESTS" envDefault:"10000"`

	// TrustedProxiesRaw is the operator-facing `TRUSTED_PROXIES` env
	// value kept verbatim for diagnostics (log dumps). Parsed into
	// TrustedProxies by Load(). Supported forms: "" (trust nothing),
	// "private" (the 7-range private-network sentinel), or a literal
	// comma-separated CIDR list (`"10.0.0.0/8,172.16.0.0/12"`).
	TrustedProxiesRaw string `env:"TRUSTED_PROXIES"`

	// TrustedProxies is the parsed CIDR list consumed by the HTTP
	// trusted-proxy middleware. Populated by Load() at startup; not
	// an env field (derived from TrustedProxiesRaw).
	TrustedProxies []*net.IPNet `env:"-"`

	// TrustedProxyHeader names the HTTP header the trusted-proxy
	// middleware reads to recover the client IP when the peer is in
	// TrustedProxies. Defaults to `X-Forwarded-For`, the de-facto
	// L7-forwarded standard.
	//
	// Accepted values (case-insensitive on input; canonicalised to the
	// conventional capitalisation at Load): `X-Forwarded-For`,
	// `X-Real-IP`, `CF-Connecting-IP`, `True-Client-IP`. Any other
	// value fails Load() so a typo in production aborts startup
	// instead of silently demoting the trust resolution.
	//
	// Single-value headers (`X-Real-IP`, `CF-Connecting-IP`,
	// `True-Client-IP`) are used verbatim. `X-Forwarded-For` performs
	// the rightmost-untrusted walk per RFC 7239 §7.1; multi-hop
	// forwarders MUST use it because the single-value alternatives
	// preserve only the immediate predecessor.
	TrustedProxyHeader string `env:"TRUSTED_PROXY_HEADER" envDefault:"X-Forwarded-For"`

	// NATSUrls is the comma-separated list of NATS server URLs to
	// connect to. Supports a single URL, a static cluster list, or a
	// DNS-resolvable hostname (the nats.go client resolves A/SRV
	// records transparently). This is the only required field.
	NATSUrls []string `env:"NATS_URLS,required" envSeparator:","`
	// NATSRandomizeUrls shuffles NATSUrls before dialing to spread
	// initial connections across cluster nodes. Disable for
	// deterministic testing.
	NATSRandomizeUrls bool `env:"NATS_RANDOMIZE_URLS"    envDefault:"true"`
	// NATSDiscoverServers enables the client-side server-discovery
	// protocol so new cluster nodes are picked up without restart.
	NATSDiscoverServers bool `env:"NATS_DISCOVER_SERVERS"  envDefault:"true"`
	// NATSUser is the NATS username for password auth. Leave empty if
	// using creds-file or no auth.
	NATSUser string `env:"NATS_USER"`
	// NATSPassword is the NATS password for password auth. Leave empty
	// if using creds-file or no auth.
	NATSPassword string `env:"NATS_PASSWORD"`
	// NATSCredsFile is the filesystem path to an NKey credentials file,
	// used for NGS / decentralised JWT auth.
	NATSCredsFile string `env:"NATS_CREDS_FILE"`
	// NATSReconnectWait is the delay between reconnection attempts
	// after the NATS connection drops.
	NATSReconnectWait time.Duration `env:"NATS_RECONNECT_WAIT"    envDefault:"2s"`
	// NATSMaxReconnects is the cap on reconnection attempts before the
	// client gives up. A value of -1 means retry forever, which is the
	// right default for a gateway that must survive cluster restarts.
	NATSMaxReconnects int `env:"NATS_MAX_RECONNECTS"    envDefault:"-1"`
	// NATSReconnectBufSize is the in-memory buffer size (bytes) for
	// messages published while the connection is temporarily down.
	NATSReconnectBufSize int `env:"NATS_RECONNECT_BUFSIZE" envDefault:"8388608"`

	// KVBucket is the NATS KV bucket name the gateway watches for
	// handler registry entries.
	//
	// REQUIRED: must be set explicitly. There is no compiled-in default
	// because a typical NATS account is shared across environments
	// (dev/staging/prod) and silently falling back to a generic name
	// risks cross-env data leakage if an operator's deploy template
	// clears the variable. Set the bucket name to a value that matches
	// the NATS account isolation policy (e.g. `gateway_routes_prod`).
	KVBucket string `env:"KV_BUCKET"`

	// RequestTimeout is the per-request hard deadline applied to the
	// full handler pipeline (RPC round-trip included).
	RequestTimeout time.Duration `env:"REQUEST_TIMEOUT"  envDefault:"30s"`
	// ShutdownTimeout bounds how long the graceful-shutdown sequence
	// waits for in-flight requests to finish before force-closing.
	ShutdownTimeout time.Duration `env:"SHUTDOWN_TIMEOUT" envDefault:"30s"`

	// RateLimitFailPolicy selects behavior when the distributed
	// rate-limit store fails (network error, circuit breaker open,
	// CAS budget exhausted). "open" (default) favors availability
	// over strict RL enforcement; "closed" rejects with 503 for
	// compliance-critical deployments where the RL contract must
	// hold even under backend outage.
	//
	// Normal rate-limit rejections (bucket empty under a healthy
	// backend) always return 429 regardless of this setting.
	RateLimitFailPolicy string `env:"RATELIMIT_FAIL_POLICY" envDefault:"open"`

	// RateLimitKeyTTL is the stale-key cleanup threshold. NATS KV
	// backends apply it as bucket MaxAge; MemoryStore uses it as
	// the idle-entry sweeper interval. 10 minutes covers all
	// realistic rps profiles without penalizing infrequent clients.
	//
	// Semantics differ between backends and operators MUST account for
	// this when sizing the value:
	//   - memory  — idle-entry sweep interval; active keys retained
	//               indefinitely. The value only controls how often
	//               the sweeper scans for inactive entries.
	//   - nats-kv — bucket MaxAge; keys are reaped after this duration
	//               regardless of activity. Raise the value to preserve
	//               state longer across traffic gaps.
	RateLimitKeyTTL time.Duration `env:"RATELIMIT_KEY_TTL" envDefault:"10m"`

	// RateLimitTimeout bounds the wall-clock budget the rate-limit
	// gate may consume per request. It is intentionally separate from
	// RequestTimeout so a flaky distributed store cannot starve handler
	// latency under retry pressure (CAS contention, breaker probes).
	//
	// Recommended: 50 ms. Hard bounds: must be > 0 and ≤ 1s. Values
	// larger than 1s are rejected at Load() because they defeat the
	// purpose of having a separate budget — the route timeout should
	// cover that range. Values ≤ 0 are rejected because they would
	// trigger immediate timeouts in every gate evaluation.
	RateLimitTimeout time.Duration `env:"RATELIMIT_TIMEOUT" envDefault:"50ms"`

	// RateLimitMemoryMaxEntries caps how many distinct keys the
	// in-process MemoryStore admits before refusing to grow further.
	// Once the cap is reached, brand-new keys produce an
	// ErrMemoryStoreSaturated error that flows through the FailPolicy
	// (closed → 503, open → request passes); existing keys keep
	// resolving normally.
	//
	// The cap defends against a cardinality-spike DoS where an
	// attacker rotates source IP every request — without a cap the
	// store would hold all of them in RAM until the sweeper's TTL
	// pass dropped them. At 64-byte keys + 64-byte memoryEntry,
	// 1_000_000 entries is roughly 122 MiB (128 MB decimal).
	//
	// Zero disables the cap (legacy unbounded behaviour). Negative
	// values are rejected at Load().
	RateLimitMemoryMaxEntries int64 `env:"RATELIMIT_MEMORY_MAX_ENTRIES" envDefault:"1000000"`

	// LogLevel is the minimum zerolog level to emit. Valid values:
	// trace, debug, info, warn, error, fatal, panic, disabled.
	LogLevel string `env:"LOG_LEVEL"          envDefault:"info"`
	// LogFormat is the log output encoding: "json" for production or
	// "console" for human-friendly colored output in local dev.
	LogFormat string `env:"LOG_FORMAT"         envDefault:"json"`

	// Environment is a free-form deployment-tier label ("production",
	// "staging", "development", ...). The gateway treats "production"
	// specially to redact sensitive error details from HTTP responses.
	Environment string `env:"ENVIRONMENT" envDefault:"production"`
}

// Load reads the configuration from environment variables, applying
// envDefault tags for optional fields and returning an error if any
// required field is missing or malformed.
//
// The only currently required field is NATS_URLS; every other knob has a
// sensible default suitable for local development. Callers are expected
// to treat any returned error as fatal and exit the process immediately
// rather than attempt partial startup.
//
// TrustedProxies are parsed from TRUSTED_PROXIES at startup; invalid
// CIDR input fails Load() with an error naming the offending value so
// startup aborts fail-closed. If TRUSTED_PROXIES is unset, it defaults
// to "private".
func Load() (*Config, error) {
	cfg := &Config{}
	if err := env.Parse(cfg); err != nil {
		return nil, fmt.Errorf("parse gateway config: %w", err)
	}

	// caarlos0/env v11 treats an explicitly empty env value the same
	// as unset and applies envDefault, which would silently turn
	// TRUSTED_PROXIES="" into "private" and break the contract that
	// "" means "trust nothing". LookupEnv distinguishes the two cases
	// so only a truly absent variable gets the default.
	if _, ok := os.LookupEnv("TRUSTED_PROXIES"); !ok {
		cfg.TrustedProxiesRaw = "private"
	}

	trusted, err := trustedproxy.ParseCIDRList(cfg.TrustedProxiesRaw)
	if err != nil {
		return nil, fmt.Errorf("parse TRUSTED_PROXIES=%q: %w",
			cfg.TrustedProxiesRaw, err)
	}
	cfg.TrustedProxies = trusted

	canonical, ok := allowedTrustedProxyHeaders[strings.ToLower(strings.TrimSpace(cfg.TrustedProxyHeader))]
	if !ok {
		return nil, fmt.Errorf("TRUSTED_PROXY_HEADER=%q is not one of "+
			"X-Forwarded-For, X-Real-IP, CF-Connecting-IP, True-Client-IP "+
			"(unknown header)", cfg.TrustedProxyHeader)
	}
	cfg.TrustedProxyHeader = canonical

	switch cfg.RateLimitFailPolicy {
	case "open", "closed":
		// ok
	default:
		return nil, fmt.Errorf("RATELIMIT_FAIL_POLICY must be \"open\" or \"closed\", got %q", cfg.RateLimitFailPolicy)
	}

	// caarlos0/env v11 collapses unset and empty into the same code
	// path and would silently apply any envDefault. KV_BUCKET is
	// deliberately defaulted-by-validation rather than by tag — the
	// NATS account is typically shared across deploy stages, so a
	// silent fallback to a generic bucket name risks reading or
	// writing the wrong environment's routing metadata.
	if cfg.KVBucket == "" {
		return nil, fmt.Errorf("KV_BUCKET must be set explicitly (no default — value selects which NATS KV bucket holds the handler registry)")
	}

	if cfg.RateLimitTimeout <= 0 || cfg.RateLimitTimeout > time.Second {
		return nil, fmt.Errorf("RATELIMIT_TIMEOUT must be > 0 and ≤ 1s, got %s", cfg.RateLimitTimeout)
	}

	if cfg.RateLimitMemoryMaxEntries < 0 {
		return nil, fmt.Errorf("RATELIMIT_MEMORY_MAX_ENTRIES must be ≥ 0 (0 disables the cap), got %d", cfg.RateLimitMemoryMaxEntries)
	}

	if cfg.HTTPMaxConcurrentRequests < 0 {
		return nil, fmt.Errorf("HTTP_MAX_CONCURRENT_REQUESTS must be ≥ 0 (0 disables the cap), got %d", cfg.HTTPMaxConcurrentRequests)
	}

	return cfg, nil
}

// IsProduction reports whether the gateway is running with Environment
// set to "production".
//
// Components that need to redact or hide sensitive data (stack traces,
// internal error messages, registry lookup failures) from HTTP responses
// consult this method rather than reading Environment directly, so the
// policy stays centralised and is easy to audit.
func (c *Config) IsProduction() bool {
	return c.Environment == "production"
}
