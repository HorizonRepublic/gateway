package http

import (
	"context"
	"net"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
)

// newIPFilterMiddleware returns a Hertz handler that rejects a request
// whose RESOLVED client IP — the trusted-proxy walk's output stamped
// on the context by newTrustedProxyMiddleware, never the raw peer or a
// spoofable forwarded header — is outside the operator's IP policy:
//
//   - a denylist CIDR match, or
//   - when an allowlist is configured, any address NOT inside it.
//
// Rejections are 403 Forbidden and short-circuit before the request
// reaches the rate-limit gate or the proxy handler, so an abusive
// client can be shed at the edge without a config change in another
// team's load balancer.
//
// It MUST run AFTER the trusted-proxy middleware so it filters the
// genuine client. The caller only registers it when at least one list
// is non-empty, so the default deployment keeps a middleware-free hot
// path.
//
// Fail-closed on ambiguity: a request whose client IP cannot be
// resolved or parsed is served when only a denylist is set (nothing to
// match it against) but rejected when an allowlist is set — an
// unidentifiable client is by definition not on the allowlist.
func newIPFilterMiddleware(allow, deny []*net.IPNet) app.HandlerFunc {
	return func(_ context.Context, ctx *app.RequestContext) {
		ip := resolvedClientIP(ctx)
		if ip == nil {
			if len(allow) > 0 {
				ctx.AbortWithStatus(consts.StatusForbidden)
			}

			return
		}

		if ipInAny(ip, deny) {
			ctx.AbortWithStatus(consts.StatusForbidden)

			return
		}

		if len(allow) > 0 && !ipInAny(ip, allow) {
			ctx.AbortWithStatus(consts.StatusForbidden)

			return
		}
	}
}

// resolvedClientIP reads the trusted-proxy-stamped client IP off the
// request context and parses it. Returns nil when the slot is missing,
// empty, or not a valid textual IP.
func resolvedClientIP(ctx *app.RequestContext) net.IP {
	raw, ok := ctx.Get(clientIPUserKey)
	if !ok {
		return nil
	}

	s, ok := raw.(string)
	if !ok || s == "" {
		return nil
	}

	return net.ParseIP(s)
}

func ipInAny(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}

	return false
}
