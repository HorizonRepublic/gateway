package http

import (
	"context"
	"sync/atomic"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
)

// concurrencyLimiter is the closure shape returned by
// newConcurrencyLimitMiddleware. The Rejected accessor lets tests
// (and a future metrics surface) read the counter without exposing
// the underlying atomic.Int64.
type concurrencyLimiter struct {
	handler  app.HandlerFunc
	rejected *atomic.Int64
}

// Rejected returns the number of requests refused with 503 since
// process start because the in-flight cap was saturated. The counter
// is monotonic across the gateway's lifetime; resetting it requires a
// process restart.
func (c *concurrencyLimiter) Rejected() int64 {
	if c.rejected == nil {
		return 0
	}
	return c.rejected.Load()
}

// newConcurrencyLimitMiddleware bounds the number of HTTP requests
// the gateway processes simultaneously. When the cap is reached the
// middleware short-circuits the request with 503 Service Unavailable
// and a 1-second Retry-After hint; the request never reaches the
// trusted-proxy chain, the rate-limit gate, or the proxy handler.
//
// The semaphore is a bounded chan struct{} of length max. Acquire is
// non-blocking — a saturated semaphore short-circuits immediately
// rather than queuing, because slowloris-style clients deliberately
// hold connections open and queuing would simply trade one
// memory-pressure attack for another. Operators preferring a queuing
// shape can run a reverse proxy (nginx, envoy) in front of the
// gateway with its own request-queue settings.
//
// limit <= 0 disables the middleware: the handler is a no-op pass-
// through. Production deployments MUST set a positive value via
// HTTP_MAX_CONCURRENT_REQUESTS.
//
// The middleware uses Hertz's ctx.Next pattern: the slot is acquired
// before forwarding the chain and released after the chain returns.
// This wraps around even if a downstream handler panics because
// Hertz recovers panics into the deferred unwind.
func newConcurrencyLimitMiddleware(limit int) *concurrencyLimiter {
	rejected := &atomic.Int64{}

	if limit <= 0 {
		// Explicit ctx.Next(c) instead of a bare no-op return — both
		// shapes propagate the chain correctly under Hertz semantics
		// (RequestContext.Next is an outer loop that auto-advances
		// ctx.index after each handler returns), but the explicit
		// shape is friendlier to static analysis tools that flag
		// no-op middleware as a chain-break risk. The behavioural
		// equivalence is pinned by
		// TestConcurrencyLimit_ZeroPropagatesToDownstream.
		return &concurrencyLimiter{
			handler:  func(c context.Context, ctx *app.RequestContext) { ctx.Next(c) },
			rejected: rejected,
		}
	}

	sem := make(chan struct{}, limit)

	handler := func(c context.Context, ctx *app.RequestContext) {
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
			ctx.Next(c)
		default:
			rejected.Add(1)
			ctx.Header("Retry-After", "1")
			ctx.AbortWithStatus(consts.StatusServiceUnavailable)
		}
	}

	return &concurrencyLimiter{handler: handler, rejected: rejected}
}
