package http

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConcurrencyLimit_SaturatesAt503 pins the back-pressure shape:
// once the in-flight count reaches the cap, the next request gets
// 503 + Retry-After: 1 instead of being queued. Queueing would
// trade memory pressure for tail-latency pressure, which is a worse
// trade against slowloris-style attackers.
func TestConcurrencyLimit_SaturatesAt503(t *testing.T) {
	const slots = 2
	limiter := newConcurrencyLimitMiddleware(slots)

	hold := make(chan struct{})

	// Two goroutines occupy the semaphore, blocking on `hold`.
	var blocked sync.WaitGroup
	blocked.Add(slots)
	for i := 0; i < slots; i++ {
		go func() {
			ctx := app.NewContext(0)
			next := func(c context.Context, _ *app.RequestContext) {
				blocked.Done()
				<-hold
			}
			ctx.SetHandlers([]app.HandlerFunc{limiter.handler, next})
			ctx.Next(context.Background())
		}()
	}
	blocked.Wait()

	// Third request hits the saturated semaphore.
	overflow := app.NewContext(0)
	called := atomic.Bool{}
	next := func(_ context.Context, _ *app.RequestContext) {
		called.Store(true)
	}
	overflow.SetHandlers([]app.HandlerFunc{limiter.handler, next})
	overflow.Next(context.Background())

	assert.Equal(t, consts.StatusServiceUnavailable, overflow.Response.StatusCode())
	assert.Equal(t, "1", string(overflow.Response.Header.Peek("Retry-After")))
	assert.False(t, called.Load(), "downstream handler must not run when the semaphore is saturated")
	assert.Equal(t, int64(1), limiter.Rejected())

	close(hold)
}

// TestConcurrencyLimit_ReleasesSlotAfterRequest pins that the
// semaphore is decremented once the chain unwinds, so a long sequence
// of fast requests sees the same N-slot capacity reused — not
// permanently held.
func TestConcurrencyLimit_ReleasesSlotAfterRequest(t *testing.T) {
	limiter := newConcurrencyLimitMiddleware(1)

	for i := 0; i < 5; i++ {
		ctx := app.NewContext(0)
		next := func(_ context.Context, c *app.RequestContext) {
			c.SetStatusCode(consts.StatusOK)
		}
		ctx.SetHandlers([]app.HandlerFunc{limiter.handler, next})

		done := make(chan struct{})
		go func() {
			ctx.Next(context.Background())
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("iteration %d: chain did not complete; slot was not released", i)
		}

		assert.Equal(t, consts.StatusOK, ctx.Response.StatusCode())
	}

	assert.Equal(t, int64(0), limiter.Rejected(),
		"sequential requests within the cap must never be rejected")
}

// TestConcurrencyLimit_ZeroDisablesCheck pins the legacy fallback:
// HTTP_MAX_CONCURRENT_REQUESTS=0 is a pass-through middleware so
// existing deployments that rely on unbounded concurrency keep
// working until they opt into the cap.
func TestConcurrencyLimit_ZeroDisablesCheck(t *testing.T) {
	limiter := newConcurrencyLimitMiddleware(0)
	require.NotNil(t, limiter.handler, "even a disabled limiter must return a non-nil handler")

	for i := 0; i < 100; i++ {
		ctx := app.NewContext(0)
		ctx.SetHandlers([]app.HandlerFunc{limiter.handler})
		ctx.Next(context.Background())
		assert.NotEqual(t, consts.StatusServiceUnavailable, ctx.Response.StatusCode(),
			"disabled limiter must never short-circuit")
	}
	assert.Equal(t, int64(0), limiter.Rejected())
}

// TestConcurrencyLimit_ZeroPropagatesToDownstream pins the
// no-short-circuit contract from the Hertz-middleware-chain side.
// Hertz's RequestContext.Next is a loop that advances ctx.index on
// every iteration: when the limit-disabled handler returns without
// calling ctx.Next(c) explicitly, the outer Next loop's `index++`
// step advances to the downstream handler regardless. Verifying the
// invariant in a test eliminates ambiguity around middleware
// semantics — a regression that mistakenly added an `Abort()` call
// or returned early in some refactor would surface here loudly.
func TestConcurrencyLimit_ZeroPropagatesToDownstream(t *testing.T) {
	limiter := newConcurrencyLimitMiddleware(0)

	var downstreamRan atomic.Bool
	downstream := func(_ context.Context, _ *app.RequestContext) {
		downstreamRan.Store(true)
	}

	ctx := app.NewContext(0)
	ctx.SetHandlers([]app.HandlerFunc{limiter.handler, downstream})
	ctx.Next(context.Background())

	assert.True(t, downstreamRan.Load(),
		"limit<=0 must NOT short-circuit; downstream handler must execute via Hertz's Next loop")
}
