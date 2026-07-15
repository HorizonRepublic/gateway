package ratelimit

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/cespare/xxhash/v2"
)

// claimNondeterministicCount tracks the number of times stringifyClaim
// fell through to the lossy fallback for an exotic claim shape (e.g.,
// map[any]any whose keys are not stringable). Surfaced via
// ClaimNondeterministicCount so the router's Counters export can
// expose it to OpenTelemetry without the key.go file holding a router
// reference.
var claimNondeterministicCount atomic.Uint64

// ClaimNondeterministicCount returns the running total of claim
// payloads that escaped deterministic stringification and landed in
// the lossy fallback path. JSON-shaped claims always pass the typed
// branches; reaching this counter implies an upstream verifier that
// produced a non-JSON shape (e.g. YAML) or a programmer-constructed
// payload with non-stringable keys. Operators should treat a non-zero
// reading as a misconfiguration signal.
func ClaimNondeterministicCount() uint64 {
	return claimNondeterministicCount.Load()
}

// cookieCollisionCount tracks the number of times the cookie keyBy
// strategy detected a duplicate cookie name in the inbound Cookie
// header. RFC 6265 allows multiple cookies with the same name; the
// rate-limit keyBy treats the situation as unresolvable rather than
// quietly picking one of the values. Exported through
// CookieCollisionCount so the router's Counters surface lifts it to
// OpenTelemetry without the key.go file holding a router reference.
var cookieCollisionCount atomic.Uint64

// CookieCollisionCount returns the running total of rate-limit
// resolutions that observed duplicate cookie names in a single
// request's Cookie header.
//
// A duplicate cookie name is RFC-permitted but rare in honest
// browser traffic — the standard cookie jar dedupes by (name, domain,
// path) before sending. Non-zero readings usually signal one of:
//
//   - An attacker injecting a Cookie header (e.g. via response
//     splitting or an upstream that forwards client-supplied raw
//     cookies) to defeat per-session rate limiting by sandwiching
//     the victim's session cookie next to the attacker's.
//   - A misbehaving client that retries with stale-and-fresh cookie
//     pairs without dedupe.
//   - Multiple Cookie headers folded by an HTTP/1.1 stack into one
//     comma-joined string (the gateway's adapter joins with "; " per
//     RFC 6265 §5.4 to avoid this; deployments with non-RFC fronts
//     may surface it).
//
// In every case the rate-limit keyBy treats the cookie strategy as
// unresolvable and falls through to the next candidate (typically IP).
// Tracking lets operators alert on surge.
func CookieCollisionCount() uint64 {
	return cookieCollisionCount.Load()
}

// base32Alphabet is the lowercase, unpadded, NATS-KV-safe base32
// alphabet used by encodeBase32. Chosen over stdlib encoding/base32
// because stdlib's alphabet mixes case and appends '=' padding, both
// of which would collide with NATS KV key charset restrictions.
const base32Alphabet = "abcdefghijklmnopqrstuvwxyz234567"

// hashKey returns a fixed-length 13-character lowercase base32 digest
// of input, backed by xxHash64.
//
// The hash is non-cryptographic and used purely to compress arbitrary
// user-supplied identifiers (path templates, IPs, header values,
// cookie values, JWT claim fragments) into a uniform, NATS-KV-safe
// token. xxHash64 is collision-safe at the cardinalities expected
// for rate-limit buckets (~10k-100k active keys; 64-bit birthday
// bound is ~4 billion keys) and ~5x faster than SHA-256 on the hot
// path — cryptographic strength is irrelevant because the output
// never travels to a trust boundary.
func hashKey(input string) string {
	return encodeBase32(xxhash.Sum64String(input))
}

// hashEncodedLength is the fixed output width of encodeBase32:
// ceil(64/5) = 13 characters for a uint64.
const hashEncodedLength = 13

// encodeBase32 renders a uint64 as exactly 13 lowercase-base32
// characters without padding.
func encodeBase32(h uint64) string {
	var buf [hashEncodedLength]byte
	encodeBase32Into(&buf, h)

	return string(buf[:])
}

// encodeBase32Into fills buf with the 13-character lowercase-base32
// rendering of h. Fills right-to-left because each iteration captures
// the low 5 bits of h before shifting. The caller-provided buffer
// variant lets hot-path composition append the digest without an
// intermediate string allocation.
func encodeBase32Into(buf *[hashEncodedLength]byte, h uint64) {
	for i := hashEncodedLength - 1; i >= 0; i-- {
		buf[i] = base32Alphabet[h&0x1f]
		h >>= 5
	}
}

// pathHashCacheMaxEntries bounds the pathTemplateHash cache. Route
// path templates come from the sanitized registry, so cardinality is
// the live route count (typically hundreds); the bound exists so that
// a misbehaving registry churning unique templates degrades to
// hash-per-call instead of unbounded memory growth.
const pathHashCacheMaxEntries = 4096

// pathHashCache memoizes hashKey(pathTemplate) keyed by the template
// string. The (method, pathTemplate) half of every bucket key is
// immutable once a routing table is built, yet BuildBucketKey runs on
// every rate-limited request — without the cache the same template is
// re-hashed and re-encoded ~100k times per second at target load.
var (
	pathHashCache     sync.Map // map[string]string
	pathHashCacheSize atomic.Int64
)

// pathTemplateHash returns hashKey(pathTemplate), memoized. Lookups
// key directly on the template string so a cache hit costs one
// sync.Map Load and zero allocations.
func pathTemplateHash(pathTemplate string) string {
	if v, ok := pathHashCache.Load(pathTemplate); ok {
		return v.(string)
	}
	h := hashKey(pathTemplate)
	if pathHashCacheSize.Load() < pathHashCacheMaxEntries {
		if _, loaded := pathHashCache.LoadOrStore(pathTemplate, h); !loaded {
			pathHashCacheSize.Add(1)
		}
	}

	return h
}

// BuildBucketKey composes a NATS-KV-safe rate-limit bucket key using
// the canonical schema shared by every Store backend:
//
//	{method}.{hashKey(pathTemplate)}.{hashKey(resolvedKey)}
//
// Both MemoryStore and NATSKVStore use this identical schema so that
// switching the store backend preserves bucket identity across a
// migration — a user rate-limited on one backend remains rate-limited
// after a hot-swap without losing their TAT. The charset is the
// lowercase base32 alphabet plus '.' as separator — all NATS KV key
// constraints (no ':', no whitespace, no wildcard characters) are
// satisfied by construction.
//
// method is emitted verbatim (e.g. "GET", "POST") — HTTP method names
// are already NATS-KV-safe. Both pathTemplate and resolvedKey are
// hashed so arbitrary user-supplied characters (':', '/', ' ', '>',
// '*', etc.) never reach the key.
//
// Hot-path shape: the pathTemplate digest is memoized (immutable per
// route) and the resolvedKey digest is encoded straight into the
// output buffer, so a call with a warm template costs one xxhash over
// the resolved key and a single string allocation.
func BuildBucketKey(method, pathTemplate, resolvedKey string) string {
	templateHash := pathTemplateHash(pathTemplate)

	var resolvedHash [hashEncodedLength]byte
	encodeBase32Into(&resolvedHash, xxhash.Sum64String(resolvedKey))

	out := make([]byte, 0, len(method)+1+hashEncodedLength+1+hashEncodedLength)
	out = append(out, method...)
	out = append(out, '.')
	out = append(out, templateHash...)
	out = append(out, '.')
	out = append(out, resolvedHash[:]...)

	return string(out)
}

// ResolveKey walks the keyBy chain and returns the first resolved value.
// Falls back to clientIP if nothing resolves.
//
// headerFn and cookieFn are injected to decouple from any HTTP framework.
//
// Header suffixes are folded to lowercase before headerFn lookup
// because every wire-level adapter (Hertz / net/http) lowercases
// header keys when building the inbound map. Operators can therefore
// write keyBy entries in any casing — `header:X-Api-Key`,
// `header:x-api-key`, `header:X-API-KEY` all resolve identically.
// Cookie suffixes are passed through verbatim because RFC 6265
// declares cookie names case-sensitive on the wire.
//
// User claim suffixes are also passed through verbatim — JWT claim
// names are case-sensitive (RFC 7519 §4) and a misspelling MUST fail
// the lookup rather than silently match a sibling claim.
//
// cookieFn returns (value, collided). When collided is true the
// inbound Cookie header carried two or more cookies with the requested
// name — RFC 6265 permits this but it is a strong signal of a Cookie-
// header injection attempt. The cookie strategy is treated as
// unresolvable on collision: ResolveKey skips this entry, bumps
// CookieCollisionCount for operator observability, and continues to
// the next keyBy candidate. Falling back to a partial match would
// either bucket two distinct identities under one quota (allowing
// quota-share or overflow attacks) or randomly flip-flop between
// them as cookie-parser implementations differ.
func ResolveKey(
	keyBy []string,
	clientIP string,
	headerFn func(name string) string,
	cookieFn func(name string) (string, bool),
	claims map[string]any,
) string {
	for _, key := range keyBy {
		switch {
		case key == "ip":
			return clientIP

		case strings.HasPrefix(key, "header:"):
			if v := headerFn(strings.ToLower(key[7:])); v != "" {
				return v
			}

		case strings.HasPrefix(key, "cookie:"):
			v, collided := cookieFn(key[7:])
			if collided {
				cookieCollisionCount.Add(1)
				continue
			}
			if v != "" {
				return v
			}

		case strings.HasPrefix(key, "user:"):
			if claims != nil {
				field := key[5:]
				if v, ok := claims[field]; ok {
					return stringifyClaim(v)
				}
			}
		}
	}

	return clientIP
}

// stringifyClaim renders a JWT claim value into a deterministic
// rate-limit key fragment.
//
// JSON marshalling sorts map keys lexicographically (per
// encoding/json since Go 1.12), so json.Marshal is the canonical way
// to derive a stable representation for object/array claims. Scalar
// primitives go through fmt.Sprint directly because their wire form
// is already deterministic.
//
// The map[interface{}]interface{} and []interface{} branches handle
// payloads from non-JSON verifiers (YAML, msgpack, hand-constructed
// claim objects) that escape json.Marshal — that encoder rejects
// map[any]any outright. The branches recursively rewrite to
// map[string]any so json.Marshal can sort and emit a stable shape
// matching what an equivalent map[string]any would produce. A claim
// that still cannot be normalised (non-stringable keys, etc.) bumps
// the claimNondeterministicCount counter and falls through to a
// lossy-but-deterministic %T:len descriptor — operators should treat
// a non-zero counter reading as a verifier misconfiguration signal.
func stringifyClaim(v any) string {
	switch val := v.(type) {
	case nil:
		return ""
	case string:
		return val
	case bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return fmt.Sprint(val)
	}

	normalised := normaliseForJSON(v)
	encoded, err := json.Marshal(normalised)
	if err != nil {
		claimNondeterministicCount.Add(1)
		return fallbackClaimDescriptor(v)
	}

	return string(encoded)
}

// normaliseForJSON rewrites a claim subtree so json.Marshal can encode
// it deterministically. The hot case is map[interface{}]interface{},
// which the stdlib encoder refuses; we copy it into map[string]any
// using fmt.Sprint on each key so JSON's lexicographic sort produces
// a stable shape. Slices are walked element-wise to handle nested
// objects. Other types pass through unchanged — json.Marshal already
// sorts map[string]any keys and preserves slice order.
func normaliseForJSON(v any) any {
	switch val := v.(type) {
	case map[interface{}]interface{}:
		converted := make(map[string]any, len(val))
		// Pre-collect string-form keys so a duplicate key from two
		// distinct any-typed originals is detected before silent
		// last-write-wins clobbering surfaces a misleading bucket.
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, fmt.Sprint(k))
		}
		sort.Strings(keys)
		// Re-walk in the original (any-typed) form to preserve
		// value-side recursion regardless of key order.
		for k, inner := range val {
			converted[fmt.Sprint(k)] = normaliseForJSON(inner)
		}

		return converted
	case []interface{}:
		out := make([]any, len(val))
		for i, inner := range val {
			out[i] = normaliseForJSON(inner)
		}

		return out
	default:
		return v
	}
}

// fallbackClaimDescriptor produces a deterministic, lossy descriptor
// for a claim that cannot be normalised into a JSON-serialisable
// shape. The descriptor commits to type + cardinality only — never
// to fmt.Sprintf("%v", ...) which would walk randomised map iteration
// for shapes outside the special cases above.
func fallbackClaimDescriptor(v any) string {
	switch val := v.(type) {
	case map[interface{}]interface{}:
		return fmt.Sprintf("%T:%d", v, len(val))
	case []interface{}:
		return fmt.Sprintf("%T:%d", v, len(val))
	default:
		return fmt.Sprintf("%T", v)
	}
}
