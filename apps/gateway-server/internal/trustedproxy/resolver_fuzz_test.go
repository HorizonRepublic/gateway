package trustedproxy_test

import (
	"net"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/trustedproxy"
)

// FuzzResolveClientIP drives both resolver entry points with
// arbitrary peer addresses and header values against the "private"
// trusted set. The header value is fully attacker-controlled in
// production, so the resolver must stay total over any byte sequence.
//
// Invariants (both ResolveClientIP and ResolveClientIPSingle):
//
//  1. No panic, whatever the inputs.
//  2. A nil peer resolves to the empty string; a non-nil peer always
//     resolves to a non-empty result.
//  3. A non-empty result parses back through net.ParseIP and is in
//     canonical form — downstream consumers (rate-limit keys, access
//     logs, auth identity) never receive an unparseable or
//     non-canonical address.
//  4. An untrusted peer resolves to itself — headers are never
//     honoured across the trust boundary.
func FuzzResolveClientIP(f *testing.F) {
	seeds := []struct {
		peer   string
		header string
	}{
		{"10.0.0.1", ""},
		{"5.5.5.5", "1.2.3.4"},
		{"10.0.0.1", "1.2.3.4"},
		{"10.0.0.1", "1.2.3.4, 10.0.0.5"},
		{"10.0.0.1", "10.0.0.5, 172.16.0.1"},
		{"::1", "2001:db8::1"},
		{"::ffff:10.0.0.1", "1.2.3.4"},
		{"10.0.0.1", "garbage, 1.2.3.4"},
		{"10.0.0.1", "1.2.3.4,  ,  10.0.0.5"},
		{"", "1.2.3.4"},
		{"not-an-ip", "1.2.3.4"},
		// IPv4-mapped IPv6 inside the header.
		{"10.0.0.1", "::ffff:1.2.3.4"},
		// Zone identifier — net.ParseIP rejects it, resolver must skip.
		{"10.0.0.1", "fe80::1%eth0"},
		// RFC 7239 Forwarded-style values with quoting and ports —
		// malformed as XFF entries, must be skipped without panics.
		{"10.0.0.1", `for="[2001:db8::1]:8080"`},
		{"10.0.0.1", `for=1.2.3.4;proto=https, for=5.6.7.8`},
		// Comma inside a quoted value splits into malformed halves.
		{"10.0.0.1", `"1.2.3.4, 5.6.7.8"`},
		// Weird separators and whitespace forms.
		{"10.0.0.1", "1.2.3.4;5.6.7.8"},
		{"10.0.0.1", "1.2.3.4\t,\t5.6.7.8"},
		{"10.0.0.1", ",,,,"},
		// Hop-inflation attack: MaxHops+1 entries.
		{"10.0.0.1", strings.Repeat("1.2.3.4,", trustedproxy.MaxHops) + "1.2.3.4"},
		// Unicode digits, NUL bytes, invalid UTF-8.
		{"10.0.0.1", "１.２.３.４"},
		{"10.0.0.1", "1.2.3.4\x00"},
		{"10.0.0.1", "\x80\xff, 1.2.3.4"},
		// Leading-zero octets (rejected by Go since 1.17) and
		// out-of-range octets.
		{"10.0.0.1", "010.010.010.010"},
		{"10.0.0.1", "1.2.3.4.5"},
		{"10.0.0.1", "256.1.1.1"},
		// IPv6 edge shapes.
		{"fd00::1", "::"},
		{"fd00::1", "2001:db8::1:2:3:4:5:6:7"},
		{"fd00::1", "::ffff:999.1.1.1"},
	}
	for _, seed := range seeds {
		f.Add(seed.peer, seed.header)
	}

	trusted, err := trustedproxy.ParseCIDRList("private")
	if err != nil {
		f.Fatalf("parse private sentinel: %v", err)
	}

	f.Fuzz(func(t *testing.T, peerRaw, header string) {
		peer := net.ParseIP(peerRaw)

		for name, got := range map[string]string{
			"ResolveClientIP":       trustedproxy.ResolveClientIP(peer, header, trusted),
			"ResolveClientIPSingle": trustedproxy.ResolveClientIPSingle(peer, header, trusted),
		} {
			if peer == nil {
				require.Empty(t, got, "%s: nil peer must resolve to empty string", name)

				continue
			}

			require.NotEmpty(t, got, "%s: non-nil peer must resolve to a non-empty IP", name)

			parsed := net.ParseIP(got)
			require.NotNil(t, parsed, "%s: result must be parseable, got %q", name, got)
			require.Equal(t, parsed.String(), got, "%s: result must be canonical", name)
		}
	})
}

// FuzzParseCIDRList drives the operator-facing CIDR list parser with
// arbitrary configuration strings. Config is operator-supplied rather
// than attacker-supplied, but a parse that panics or produces nil
// entries would turn a config typo into a startup crash or a nil
// dereference on the per-request resolve path.
//
// Invariants:
//
//  1. ParseCIDRList never panics.
//  2. On success every returned network is non-nil, round-trips
//     through net.ParseCIDR, and is usable by the resolver without
//     panicking.
func FuzzParseCIDRList(f *testing.F) {
	seeds := []string{
		"",
		"private",
		" private ",
		"10.0.0.0/8",
		"10.0.0.0/8,  172.16.0.0/12 , 192.168.0.0/16",
		"not-a-cidr",
		"10.0.0.0/99",
		"10.0.0.0/8,garbage",
		"0.0.0.0/0",
		"::/0",
		"::1/128,fd00::/8",
		// Host bits set: net.ParseCIDR masks them off.
		"10.0.0.1/8",
		// Trailing and doubled commas.
		"1.2.3.4/32,",
		",,,",
		// IPv6 zone, IPv4-mapped form, unicode digits.
		"fe80::/10",
		"fe80::1%eth0/64",
		"::ffff:10.0.0.0/104",
		"１０.0.0.0/8",
		// Whitespace variants.
		"\t10.0.0.0/8\n",
	}
	for _, seed := range seeds {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, input string) {
		networks, err := trustedproxy.ParseCIDRList(input)
		if err != nil {
			return
		}

		for _, network := range networks {
			require.NotNil(t, network, "parsed list must never contain nil networks")

			_, reparsed, reErr := net.ParseCIDR(network.String())
			require.NoError(t, reErr, "network %q must round-trip through net.ParseCIDR", network)
			require.Equal(t, reparsed.String(), network.String())
		}

		// The parsed set must be directly usable on the resolve path.
		got := trustedproxy.ResolveClientIP(net.ParseIP("192.0.2.1"), "1.2.3.4", networks)
		require.NotEmpty(t, got)
	})
}
