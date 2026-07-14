package http

import (
	"context"
	"net"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/trustedproxy"
)

// clientIPUserKey is the per-request context slot the trusted-proxy
// middleware writes and the adapter reads. Unexported constant so the
// middleware and adapter share the literal without any other code in
// the package (or outside) introducing a parallel key.
const clientIPUserKey = "client_ip"

// xForwardedForHeader names the multi-hop forwarded-IP standard. It
// is the default operator-facing knob value and the only header that
// triggers the rightmost-untrusted chain walk per RFC 7239 §7.1.
const xForwardedForHeader = "X-Forwarded-For"

// newTrustedProxyMiddleware returns a Hertz handler that resolves
// the client IP for every incoming request and stamps it on the
// request context via ctx.Set(clientIPUserKey, ip). The adapter
// reads the stamped value in buildServeInput via resolveRemoteAddr.
//
// headerName names the HTTP header the resolver consults to recover
// the client IP. Allowed values are validated by config.Load() to be
// one of `X-Forwarded-For`, `X-Real-IP`, `CF-Connecting-IP`, or
// `True-Client-IP`. Single-value headers (`X-Real-IP`,
// `CF-Connecting-IP`, `True-Client-IP`) are taken verbatim from a
// trusted peer; `X-Forwarded-For` walks the chain rightmost-untrusted.
// In all cases, an untrusted peer is never permitted to vouch for the
// header's contents — this is the spoofing defence.
//
// The middleware is small by design: all trust logic lives in the
// pure trustedproxy package. This wrapper extracts framework inputs
// (peer TCP address, configured header), calls the appropriate
// resolver, and writes the result back onto Hertz's context. The
// resolver's nil-safe behaviour means a non-TCP peer (exotic test
// setup, Unix socket) degrades gracefully — the resolver returns the
// empty string, the adapter's fallback path invokes ctx.ClientIP(),
// and the request still serves.
func newTrustedProxyMiddleware(trusted []*net.IPNet, headerName string) app.HandlerFunc {
	multiHop := headerName == xForwardedForHeader

	return func(_ context.Context, ctx *app.RequestContext) {
		peerIP := extractPeerIP(ctx.RemoteAddr())

		var resolved string
		if multiHop {
			xff := joinFieldLines(&ctx.Request.Header, headerName)
			resolved = trustedproxy.ResolveClientIP(peerIP, xff, trusted)
		} else {
			raw := string(ctx.Request.Header.Peek(headerName))
			resolved = trustedproxy.ResolveClientIPSingle(peerIP, raw, trusted)
		}

		ctx.Set(clientIPUserKey, resolved)
	}
}

// joinFieldLines returns the value of every field line named name,
// joined with ", " per RFC 9110 §5.3: repeated field lines are
// semantically one comma-separated list, and the trust walk MUST see
// the whole list. Reading only the first line would let an attacker
// smuggle a spoofed X-Forwarded-For line past a trusted proxy that
// records the genuine client on a separate line (HAProxy `option
// forwardfor` appends a new line when one is already present). The
// adapter joins repeated header lines the same way when building the
// upstream envelope, so the trust decision and the forwarded header
// agree on the same request.
//
// The zero- and one-line paths stay allocation-lean: no join buffer,
// exactly one string copy of the single value. The multi-line join is
// a rare path — one sized strings.Builder pass.
func joinFieldLines(h *protocol.RequestHeader, name string) string {
	lines := h.PeekAll(name)
	switch len(lines) {
	case 0:
		return ""
	case 1:
		return string(lines[0])
	}

	size := 0
	for _, line := range lines {
		size += len(line) + 2 // + ", " separator budget
	}

	var b strings.Builder
	b.Grow(size)
	for i, line := range lines {
		if i > 0 {
			b.WriteString(", ")
		}
		b.Write(line)
	}

	return b.String()
}

// extractPeerIP pulls the IP portion out of a net.Addr. For TCP
// connections (the only production transport) this is a simple
// type assertion. For anything else we return nil and the resolver
// handles it as an untrusted peer.
func extractPeerIP(addr net.Addr) net.IP {
	if tcp, ok := addr.(*net.TCPAddr); ok {
		return tcp.IP
	}

	return nil
}
