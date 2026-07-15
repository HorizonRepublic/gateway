package http

import (
	"context"
	"net"
	"testing"

	"github.com/cloudwego/hertz/pkg/common/ut"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/trustedproxy"
)

func mustCIDRs(t *testing.T, raw string) []*net.IPNet {
	t.Helper()
	nets, err := trustedproxy.ParseCIDRList(raw)
	require.NoError(t, err)

	return nets
}

// runIPFilter drives the middleware against a request whose resolved
// client IP is clientIP (empty string ⇒ the slot is left unset, as if
// the peer was unresolvable) and reports whether the request was
// aborted and with what status.
func runIPFilter(allow, deny []*net.IPNet, clientIP string) (aborted bool, status int) {
	mw := newIPFilterMiddleware(allow, deny)
	ctx := ut.CreateUtRequestContext("GET", "https://gateway.test/x", nil)
	if clientIP != "" {
		ctx.Set(clientIPUserKey, clientIP)
	}

	mw(context.Background(), ctx)

	return ctx.IsAborted(), ctx.Response.StatusCode()
}

func TestIPFilterMiddleware(t *testing.T) {
	deny := mustCIDRs(t, "10.0.0.0/8,192.168.1.5/32")
	allow := mustCIDRs(t, "203.0.113.0/24")

	tests := []struct {
		name        string
		allow, deny []*net.IPNet
		clientIP    string
		wantAbort   bool
	}{
		{"denylist match is rejected", nil, deny, "10.1.2.3", true},
		{"denylist exact host is rejected", nil, deny, "192.168.1.5", true},
		{"outside denylist passes", nil, deny, "8.8.8.8", false},
		{"allowlist member passes", allow, nil, "203.0.113.7", false},
		{"outside allowlist is rejected", allow, nil, "8.8.8.8", true},
		{"deny wins even inside allowlist", allow, mustCIDRs(t, "203.0.113.7/32"), "203.0.113.7", true},
		{"unresolvable IP passes under denylist only", nil, deny, "", false},
		{"unresolvable IP rejected under allowlist", allow, nil, "", true},
		{"malformed IP rejected under allowlist", allow, nil, "not-an-ip", true},
		{"malformed IP passes under denylist only", nil, deny, "not-an-ip", false},
		{"IPv6 denylist match is rejected", nil, mustCIDRs(t, "2001:db8::/32"), "2001:db8::1", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			aborted, status := runIPFilter(tc.allow, tc.deny, tc.clientIP)
			assert.Equal(t, tc.wantAbort, aborted)
			if tc.wantAbort {
				assert.Equal(t, consts.StatusForbidden, status,
					"a rejected request must carry 403 Forbidden")
			}
		})
	}
}
