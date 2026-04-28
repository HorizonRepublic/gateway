// Package trustedproxy resolves the real client IP from an incoming
// request given a list of trusted proxy CIDR ranges. It is a
// framework-agnostic pure-Go package: callers hand in the peer IP
// and the raw X-Forwarded-For header string; the package walks the
// chain per RFC 7239 §7.1 (rightmost untrusted) and returns the IP
// that should be used for rate limiting, access logging, and
// auth-adjacent identity decisions.
//
// The package deliberately accepts primitive types only (net.IP,
// string, []*net.IPNet) so reuse across non-HTTP transports is
// trivial. HTTP-specific wiring (Hertz middleware, adapter glue)
// lives in internal/transport/http.
package trustedproxy

import (
	"fmt"
	"net"
	"strings"
)

// MaxHops bounds the number of X-Forwarded-For hops the resolver
// will walk before giving up and falling back to the peer IP.
//
// 32 L7 hops is well above any realistic production topology
// (typical chains are 1-3 hops: client → CDN → ingress → gateway).
// The cap exists as a defence-in-depth measure against XFF
// inflation attacks — an attacker cannot make the resolver walk an
// arbitrarily large chain by stuffing the header with fake entries.
const MaxHops = 32

// privateSentinel is the operator-facing magic string that expands
// to the conventional "private network" CIDR set covering k8s,
// EC2/GCE VPCs, loopback, and IPv6 ULA.
const privateSentinel = "private"

// privateCIDRs is the expansion of the `"private"` sentinel. The
// list intentionally mixes IPv4 and IPv6 because a dual-stack
// deployment may have private peers on either family.
//
// Contents:
//   - 10.0.0.0/8      RFC 1918
//   - 172.16.0.0/12   RFC 1918
//   - 192.168.0.0/16  RFC 1918
//   - 100.64.0.0/10   RFC 6598 (carrier-grade NAT)
//   - 127.0.0.0/8     loopback
//   - ::1/128         loopback (IPv6)
//   - fd00::/8        RFC 4193 unique-local
var privateCIDRs = []string{
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"100.64.0.0/10",
	"127.0.0.0/8",
	"::1/128",
	"fd00::/8",
}

// ParseCIDRList parses operator-facing configuration input into a
// slice of trusted CIDR networks. Supported forms:
//
//   - ""           → empty slice (trust nothing; always use peer IP).
//   - "private"    → expand to the privateCIDRs list.
//   - "a/b,c/d"    → literal comma-separated CIDR list, whitespace
//     around entries is tolerated.
//
// A single malformed entry fails the whole parse — callers
// (config.Load()) MUST treat any error as fatal to preserve
// fail-closed startup behaviour.
func ParseCIDRList(input string) ([]*net.IPNet, error) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return nil, nil
	}

	if trimmed == privateSentinel {
		return parseCIDRs(privateCIDRs)
	}

	parts := strings.Split(trimmed, ",")

	return parseCIDRs(parts)
}

// parseCIDRs converts a slice of textual CIDRs into parsed
// *net.IPNet values. Whitespace around each entry is trimmed. The
// first invalid entry aborts the whole parse with an error naming
// the offending input so operators can locate the typo.
func parseCIDRs(entries []string) ([]*net.IPNet, error) {
	out := make([]*net.IPNet, 0, len(entries))
	for _, raw := range entries {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}

		_, network, err := net.ParseCIDR(entry)
		if err != nil {
			return nil, fmt.Errorf("trustedproxy: invalid CIDR %q: %w", entry, err)
		}

		out = append(out, network)
	}

	return out, nil
}

// ResolveClientIP returns the client IP after X-Forwarded-For trust
// resolution per RFC 7239 §7.1 (rightmost untrusted).
//
// Algorithm:
//  1. If peerIP is nil or not in trusted → return peerIP.String()
//     (or "" if peerIP was nil). XFF is ignored — an untrusted peer
//     cannot vouch for headers it attached. This is the spoofing
//     defence.
//  2. If peerIP is trusted → walk XFF right-to-left. Skip trusted
//     IPs and malformed entries. Return the first untrusted IP.
//  3. Fallback (empty XFF, all-trusted chain, all entries malformed,
//     chain exceeds MaxHops) → return peerIP.String().
//
// IPv4-mapped IPv6 peer addresses (e.g. ::ffff:10.0.0.1) are
// normalised to IPv4 before CIDR matching so dual-stack hosts with
// IPv4 peers get matched against IPv4 CIDRs correctly.
func ResolveClientIP(peerIP net.IP, xff string, trusted []*net.IPNet) string {
	peerStr := ipString(peerIP)

	if !ipIn(peerIP, trusted) {
		return peerStr
	}

	entries := splitXFF(xff)
	if len(entries) > MaxHops {
		return peerStr
	}

	// Walk right-to-left: the rightmost entry is the IP closest to
	// the gateway, and we want the first untrusted entry going
	// backwards through the chain.
	for i := len(entries) - 1; i >= 0; i-- {
		candidate := net.ParseIP(entries[i])
		if candidate == nil {
			continue
		}
		if !ipIn(candidate, trusted) {
			return candidate.String()
		}
	}

	return peerStr
}

// ResolveClientIPSingle returns the client IP for a single-value
// forwarded-IP header (X-Real-IP, CF-Connecting-IP, True-Client-IP).
//
// Algorithm:
//  1. If peerIP is nil or not in trusted → return peerIP.String() and
//     ignore the header verbatim. An untrusted peer cannot vouch for
//     a single-value header any more than it can vouch for XFF.
//  2. If peerIP is trusted AND headerValue parses as a valid IP →
//     return that IP. The single-value contract has no chain to walk;
//     a trusted predecessor is asserting the value as-is.
//  3. Fallback (empty header, malformed value) → return peerIP.String().
//
// Trust testing for the supplied IP is intentionally NOT applied — a
// single-value header carries the originating client by definition,
// and stripping a "trusted-looking" value would defeat its purpose.
// The XFF rightmost-untrusted walk exists because XFF carries an
// arbitrary chain; single-value headers carry exactly one entry.
func ResolveClientIPSingle(peerIP net.IP, headerValue string, trusted []*net.IPNet) string {
	peerStr := ipString(peerIP)

	if !ipIn(peerIP, trusted) {
		return peerStr
	}

	candidate := net.ParseIP(strings.TrimSpace(headerValue))
	if candidate == nil {
		return peerStr
	}

	return candidate.String()
}

// ipIn reports whether ip falls inside any network in the trusted
// list. Nil IP is never trusted. IPv4-mapped IPv6 addresses are
// converted to IPv4 before matching so operators who configure an
// IPv4 CIDR still see matches when the peer arrived as IPv4-mapped
// IPv6 (a common situation on dual-stack Linux hosts).
func ipIn(ip net.IP, trusted []*net.IPNet) bool {
	if ip == nil {
		return false
	}

	normalized := ip
	if v4 := ip.To4(); v4 != nil {
		normalized = v4
	}

	for _, network := range trusted {
		if network.Contains(normalized) {
			return true
		}
	}

	return false
}

// ipString returns ip.String() with nil handling so callers can
// treat the result as a plain string without panicking.
func ipString(ip net.IP) string {
	if ip == nil {
		return ""
	}

	return ip.String()
}

// splitXFF parses an X-Forwarded-For header value into a slice of
// trimmed IP strings. Empty and whitespace-only entries are dropped
// up front so the caller can use len() as a hop count.
func splitXFF(xff string) []string {
	if xff == "" {
		return nil
	}

	raw := strings.Split(xff, ",")
	out := make([]string, 0, len(raw))
	for _, entry := range raw {
		trimmed := strings.TrimSpace(entry)
		if trimmed == "" {
			continue
		}
		out = append(out, trimmed)
	}

	return out
}
