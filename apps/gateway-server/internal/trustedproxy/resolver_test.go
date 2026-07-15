package trustedproxy_test

import (
	"net"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/trustedproxy"
)

func TestParseCIDRList_EmptyStringReturnsEmptySlice(t *testing.T) {
	out, err := trustedproxy.ParseCIDRList("")
	require.NoError(t, err)
	assert.Empty(t, out, "empty input means trust nothing")
}

func TestParseCIDRList_PrivateSentinelExpandsToSevenRanges(t *testing.T) {
	out, err := trustedproxy.ParseCIDRList("private")
	require.NoError(t, err)
	require.Len(t, out, 7, "private sentinel expands to exactly 7 CIDR blocks")

	// Collect textual forms so the test is order-independent and
	// reads like a security audit.
	got := make(map[string]bool, len(out))
	for _, n := range out {
		got[n.String()] = true
	}
	want := []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"100.64.0.0/10",
		"127.0.0.0/8",
		"::1/128",
		"fd00::/8",
	}
	for _, cidr := range want {
		assert.True(t, got[cidr], "private sentinel must include %s", cidr)
	}
}

func TestParseCIDRList_LiteralSingleCIDR(t *testing.T) {
	out, err := trustedproxy.ParseCIDRList("10.0.0.0/8")
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, "10.0.0.0/8", out[0].String())
}

func TestParseCIDRList_LiteralCommaSeparatedWithWhitespace(t *testing.T) {
	out, err := trustedproxy.ParseCIDRList("10.0.0.0/8,  172.16.0.0/12 , 192.168.0.0/16")
	require.NoError(t, err)
	require.Len(t, out, 3, "literal list must tolerate whitespace around commas")

	got := make([]string, 0, 3)
	for _, n := range out {
		got = append(got, n.String())
	}
	assert.ElementsMatch(t, []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}, got)
}

func TestParseCIDRList_InvalidCIDRReturnsError(t *testing.T) {
	_, err := trustedproxy.ParseCIDRList("not-a-cidr")
	require.Error(t, err, "invalid input must fail parse so Load() aborts startup")
	assert.Contains(t, err.Error(), "not-a-cidr",
		"error must name the offending entry so operators can fix it")
}

func TestParseCIDRList_BadMaskReturnsError(t *testing.T) {
	_, err := trustedproxy.ParseCIDRList("10.0.0.0/99")
	require.Error(t, err, "mask out of range must fail parse")
}

func TestParseCIDRList_OneValidOneInvalidReturnsError(t *testing.T) {
	_, err := trustedproxy.ParseCIDRList("10.0.0.0/8,garbage")
	require.Error(t, err,
		"any single invalid entry must fail the whole parse (fail-closed)")
}

// ---------- ResolveClientIP ----------

// resolverFixture bundles a private-CIDR parse so every resolver
// test doesn't repeat the parse inline.
func resolverFixture(t *testing.T) []*net.IPNet {
	t.Helper()
	out, err := trustedproxy.ParseCIDRList("private")
	require.NoError(t, err)

	return out
}

func TestResolveClientIP_NoXFF_PeerTrusted_ReturnsPeer(t *testing.T) {
	trusted := resolverFixture(t)
	got := trustedproxy.ResolveClientIP(net.ParseIP("10.0.0.1"), "", trusted)
	assert.Equal(t, "10.0.0.1", got, "peer trusted + empty XFF → peer")
}

func TestResolveClientIP_NoXFF_PeerUntrusted_ReturnsPeer(t *testing.T) {
	trusted := resolverFixture(t)
	got := trustedproxy.ResolveClientIP(net.ParseIP("5.5.5.5"), "", trusted)
	assert.Equal(t, "5.5.5.5", got, "peer untrusted + empty XFF → peer")
}

func TestResolveClientIP_XFFSpoofAttempt_PeerUntrusted_IgnoresXFF(t *testing.T) {
	trusted := resolverFixture(t)
	got := trustedproxy.ResolveClientIP(net.ParseIP("5.5.5.5"), "1.2.3.4", trusted)
	assert.Equal(t, "5.5.5.5", got,
		"untrusted peer must NOT honour XFF — this is the spoofing defence")
}

func TestResolveClientIP_XFFSingleClient_PeerTrusted_ReturnsClient(t *testing.T) {
	trusted := resolverFixture(t)
	got := trustedproxy.ResolveClientIP(net.ParseIP("10.0.0.1"), "1.2.3.4", trusted)
	assert.Equal(t, "1.2.3.4", got, "trusted peer + single-client XFF → client")
}

func TestResolveClientIP_XFFChain_PeerTrusted_ReturnsRightmostUntrusted(t *testing.T) {
	trusted := resolverFixture(t)
	got := trustedproxy.ResolveClientIP(
		net.ParseIP("10.0.0.1"),
		"1.2.3.4, 10.0.0.5",
		trusted,
	)
	assert.Equal(t, "1.2.3.4", got,
		"rightmost-untrusted walk: 10.0.0.5 trusted → skip → 1.2.3.4 untrusted → return")
}

func TestResolveClientIP_XFFAllTrusted_ReturnsPeer(t *testing.T) {
	trusted := resolverFixture(t)
	got := trustedproxy.ResolveClientIP(
		net.ParseIP("10.0.0.1"),
		"10.0.0.5, 172.16.0.1",
		trusted,
	)
	assert.Equal(t, "10.0.0.1", got,
		"all-trusted chain exhausts without finding an untrusted entry → peer fallback")
}

func TestResolveClientIP_IPv6_PeerTrusted_ReturnsIPv6Client(t *testing.T) {
	trusted := resolverFixture(t)
	got := trustedproxy.ResolveClientIP(net.ParseIP("::1"), "2001:db8::1", trusted)
	assert.Equal(t, "2001:db8::1", got, "IPv6 chain resolves symmetrically")
}

func TestResolveClientIP_IPv4MappedIPv6_PeerTrusted_ReturnsClient(t *testing.T) {
	trusted := resolverFixture(t)
	// ::ffff:10.0.0.1 is the IPv4-mapped form of 10.0.0.1 which IS
	// in the private sentinel. Normalisation must strip the v6 prefix
	// so the IPv4-private CIDR match succeeds.
	got := trustedproxy.ResolveClientIP(
		net.ParseIP("::ffff:10.0.0.1"),
		"1.2.3.4",
		trusted,
	)
	assert.Equal(t, "1.2.3.4", got,
		"IPv4-mapped IPv6 peer must normalise to IPv4 for CIDR match")
}

// TestResolveClientIP_MalformedEntry_TerminatesWalkToPeer pins the
// fail-closed walk contract: an entry the resolver cannot parse marks
// the boundary of trustworthy data. Everything further left was
// written by less-trusted parties, so the walk MUST stop and fall
// back to the peer instead of continuing into attacker-controlled
// territory. Skipping the malformed hop here would hand the
// attacker-prepended 6.6.6.6 to rate limiting and audit logs.
func TestResolveClientIP_MalformedEntry_TerminatesWalkToPeer(t *testing.T) {
	trusted := resolverFixture(t)
	got := trustedproxy.ResolveClientIP(
		net.ParseIP("10.0.0.1"),
		"6.6.6.6, garbage",
		trusted,
	)
	assert.Equal(t, "10.0.0.1", got,
		"unparseable hop terminates the walk fail-closed — entries left of it are never consulted")
}

// TestResolveClientIP_MalformedEntryLeftOfUntrusted_NotReached pins
// that the fail-closed termination does not regress the happy path:
// the walk returns at the rightmost untrusted entry before ever
// touching a malformed hop further left.
func TestResolveClientIP_MalformedEntryLeftOfUntrusted_NotReached(t *testing.T) {
	trusted := resolverFixture(t)
	got := trustedproxy.ResolveClientIP(
		net.ParseIP("10.0.0.1"),
		"garbage, 1.2.3.4",
		trusted,
	)
	assert.Equal(t, "1.2.3.4", got,
		"walk resolves at the rightmost untrusted entry; hops left of it are irrelevant")
}

// TestResolveClientIP_PortSuffixedEntry_ParsesHost pins the ip:port
// entry form. Some trusted L7 proxies (Azure Application Gateway,
// IIS/ARR) record the peer socket rather than the bare address, so
// "203.0.113.9:51234" is a legitimate genuine-client entry. Treating
// it as malformed would terminate the walk and collapse every client
// behind that proxy onto the proxy's identity.
func TestResolveClientIP_PortSuffixedEntry_ParsesHost(t *testing.T) {
	trusted := resolverFixture(t)
	got := trustedproxy.ResolveClientIP(
		net.ParseIP("10.0.0.1"),
		"6.6.6.6, 203.0.113.9:51234",
		trusted,
	)
	assert.Equal(t, "203.0.113.9", got,
		"ip:port entry parses to its host — attacker-prepended 6.6.6.6 is never reached")
}

// TestResolveClientIP_PortSuffixedTrustedEntry_SkippedAsTrusted pins
// that port-suffixed entries participate in trust matching like bare
// ones: a trusted ip:port hop is skipped and the walk continues left.
func TestResolveClientIP_PortSuffixedTrustedEntry_SkippedAsTrusted(t *testing.T) {
	trusted := resolverFixture(t)
	got := trustedproxy.ResolveClientIP(
		net.ParseIP("10.0.0.1"),
		"1.2.3.4, 10.0.0.5:443",
		trusted,
	)
	assert.Equal(t, "1.2.3.4", got,
		"trusted ip:port hop is skipped exactly like a bare trusted IP")
}

// TestResolveClientIP_BracketedIPv6WithPort_ParsesHost pins the
// RFC 7239 §6 node form for IPv6: "[2001:db8::1]:443" resolves to
// the bracketed host.
func TestResolveClientIP_BracketedIPv6WithPort_ParsesHost(t *testing.T) {
	trusted := resolverFixture(t)
	got := trustedproxy.ResolveClientIP(
		net.ParseIP("::1"),
		"[2001:db8::1]:443",
		trusted,
	)
	assert.Equal(t, "2001:db8::1", got,
		"bracketed IPv6 with port parses to the enclosed address")
}

// TestResolveClientIP_BracketedIPv6WithoutPort_Parses covers the
// bracket-only variant some forwarders emit for IPv6 entries.
func TestResolveClientIP_BracketedIPv6WithoutPort_Parses(t *testing.T) {
	trusted := resolverFixture(t)
	got := trustedproxy.ResolveClientIP(
		net.ParseIP("::1"),
		"[2001:db8::1]",
		trusted,
	)
	assert.Equal(t, "2001:db8::1", got,
		"bracketed IPv6 without port parses to the enclosed address")
}

// TestResolveClientIP_RFC7239UnknownIdentifier_TerminatesWalk pins
// fail-closed handling of the RFC 7239 §6 "unknown" identifier: it
// carries no IP, so the walk cannot continue past it safely.
func TestResolveClientIP_RFC7239UnknownIdentifier_TerminatesWalk(t *testing.T) {
	trusted := resolverFixture(t)
	got := trustedproxy.ResolveClientIP(
		net.ParseIP("10.0.0.1"),
		"6.6.6.6, unknown",
		trusted,
	)
	assert.Equal(t, "10.0.0.1", got,
		"RFC 7239 \"unknown\" identifier terminates the walk to the peer")
}

// TestResolveClientIP_ObfuscatedIdentifier_TerminatesWalk pins the
// same fail-closed handling for RFC 7239 §6.3 obfuscated identifiers
// ("_hidden") — non-IP node forms never extend the walk.
func TestResolveClientIP_ObfuscatedIdentifier_TerminatesWalk(t *testing.T) {
	trusted := resolverFixture(t)
	got := trustedproxy.ResolveClientIP(
		net.ParseIP("10.0.0.1"),
		"6.6.6.6, _hidden",
		trusted,
	)
	assert.Equal(t, "10.0.0.1", got,
		"obfuscated identifier terminates the walk to the peer")
}

func TestResolveClientIP_WhitespaceAndEmptyEntries_Tolerated(t *testing.T) {
	trusted := resolverFixture(t)
	got := trustedproxy.ResolveClientIP(
		net.ParseIP("10.0.0.1"),
		"1.2.3.4,  ,  10.0.0.5",
		trusted,
	)
	assert.Equal(t, "1.2.3.4", got,
		"extra whitespace and empty entries between commas must not derail the walk")
}

func TestResolveClientIP_ChainExceedsMaxHops_ReturnsPeer(t *testing.T) {
	trusted := resolverFixture(t)
	// Build a chain with MaxHops+1 entries, each untrusted. The
	// resolver must refuse to walk past MaxHops and fall back to
	// peer rather than spend unbounded CPU on the header.
	entries := make([]string, 0, trustedproxy.MaxHops+1)
	for i := 0; i < trustedproxy.MaxHops+1; i++ {
		entries = append(entries, "1.2.3.4")
	}
	xff := strings.Join(entries, ", ")

	got := trustedproxy.ResolveClientIP(net.ParseIP("10.0.0.1"), xff, trusted)
	assert.Equal(t, "10.0.0.1", got,
		"chain > MaxHops must trigger peer fallback (DoS inflation guard)")
}

func TestResolveClientIP_EmptyTrustedList_AlwaysReturnsPeer(t *testing.T) {
	got := trustedproxy.ResolveClientIP(
		net.ParseIP("10.0.0.1"),
		"1.2.3.4",
		nil,
	)
	assert.Equal(t, "10.0.0.1", got,
		"empty trusted list → peer is never trusted → XFF always ignored")
}

func TestResolveClientIP_NilPeerIP_FallbackEmptyString(t *testing.T) {
	trusted := resolverFixture(t)
	// A nil peer (non-TCP connection in exotic test setup) is
	// treated as untrusted; XFF is ignored; resolver returns the
	// empty string rather than panic. Middleware wrapper handles
	// the empty string defensively.
	got := trustedproxy.ResolveClientIP(nil, "1.2.3.4", trusted)
	assert.Empty(t, got, "nil peer IP → empty string (non-panic safe degradation)")
}

// ---------- ResolveClientIPSingle ----------

// TestResolveClientIPSingle_PeerTrusted_ReturnsHeaderValue pins the
// canonical happy path: a trusted predecessor's single-value forwarded
// header (X-Real-IP / CF-Connecting-IP / True-Client-IP) is honoured
// verbatim once the IP parses as valid.
func TestResolveClientIPSingle_PeerTrusted_ReturnsHeaderValue(t *testing.T) {
	trusted := resolverFixture(t)
	got := trustedproxy.ResolveClientIPSingle(net.ParseIP("10.0.0.1"), "203.0.113.7", trusted)
	assert.Equal(t, "203.0.113.7", got,
		"trusted peer + valid single-value header → header IP")
}

// TestResolveClientIPSingle_PeerUntrusted_IgnoresHeader is the
// spoofing-defence pin: an untrusted peer cannot vouch for a
// single-value forwarded header any more than it can vouch for XFF.
// The resolver MUST drop the header and fall back to the peer.
func TestResolveClientIPSingle_PeerUntrusted_IgnoresHeader(t *testing.T) {
	trusted := resolverFixture(t)
	got := trustedproxy.ResolveClientIPSingle(net.ParseIP("5.5.5.5"), "203.0.113.7", trusted)
	assert.Equal(t, "5.5.5.5", got,
		"untrusted peer must NOT honour single-value header — spoofing defence")
}

// TestResolveClientIPSingle_PeerTrusted_EmptyHeader_FallsBackToPeer
// covers the no-header path: a trusted peer with no header carries no
// upstream identity to forward, so the peer IP is the correct identity.
func TestResolveClientIPSingle_PeerTrusted_EmptyHeader_FallsBackToPeer(t *testing.T) {
	trusted := resolverFixture(t)
	got := trustedproxy.ResolveClientIPSingle(net.ParseIP("10.0.0.1"), "", trusted)
	assert.Equal(t, "10.0.0.1", got,
		"trusted peer + empty header → peer (no upstream identity to honour)")
}

// TestResolveClientIPSingle_PeerTrusted_MalformedHeader_FallsBackToPeer
// pins the parse-failure recovery: a trusted predecessor that ships
// junk in the header MUST NOT be treated as authoritative — without
// a valid IP the resolver cannot return a meaningful identity, so it
// degrades to the peer rather than emitting an unparseable string.
func TestResolveClientIPSingle_PeerTrusted_MalformedHeader_FallsBackToPeer(t *testing.T) {
	trusted := resolverFixture(t)
	got := trustedproxy.ResolveClientIPSingle(net.ParseIP("10.0.0.1"), "not-an-ip", trusted)
	assert.Equal(t, "10.0.0.1", got,
		"malformed single-value header must fall through to peer rather than propagate junk")
}

// TestResolveClientIPSingle_PeerTrusted_IPv6_ReturnsVerbatim covers
// the IPv6 path. net.ParseIP returns a canonicalised form, so the
// assertion uses the parsed string to absorb any whitespace stripping
// or zero compression normalisation the stdlib applies.
func TestResolveClientIPSingle_PeerTrusted_IPv6_ReturnsVerbatim(t *testing.T) {
	trusted := resolverFixture(t)
	got := trustedproxy.ResolveClientIPSingle(net.ParseIP("::1"), "2001:db8::1", trusted)
	assert.Equal(t, "2001:db8::1", got,
		"trusted peer + IPv6 single-value header → IPv6 client IP")
}

// TestResolveClientIPSingle_NilPeerIP_ReturnsEmptyString matches the
// nil-peer behaviour of ResolveClientIP — a non-TCP connection (exotic
// test or transport surface) MUST degrade to the empty string rather
// than panic.
func TestResolveClientIPSingle_NilPeerIP_ReturnsEmptyString(t *testing.T) {
	trusted := resolverFixture(t)
	got := trustedproxy.ResolveClientIPSingle(nil, "203.0.113.7", trusted)
	assert.Empty(t, got, "nil peer IP → empty string (non-panic safe degradation)")
}
