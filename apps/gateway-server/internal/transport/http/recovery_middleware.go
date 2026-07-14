package http

import (
	"context"
	"fmt"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/middlewares/server/recovery"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/rs/zerolog"

	gerrors "github.com/HorizonRepublic/gateway/apps/gateway-server/internal/errors"
	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/observability"
)

// newRecoveryMiddleware returns the panic-recovery middleware for the
// public listener. It wraps Hertz's recovery.Recovery with a custom
// handler because the built-in one (installed by server.Default) logs
// through hlog in plain text and answers with an empty 500 body —
// invisible to the gateway's structured-log pipeline and inconsistent
// with the JSON error contract every other gateway-owned error obeys.
//
// The custom handler, in order:
//
//  1. Increments the observability panic counter so the event is
//     countable even if the log line is sampled or dropped.
//  2. Emits one ERROR event on the gateway's zerolog logger carrying
//     the request correlator (request_id), route context (method,
//     path), the panic value, and the formatted stack trace.
//  3. Writes the shared pre-encoded 500 body (internal/errors
//     InternalError) with the same header discipline as
//     writeServeResult: JSON content type, Cache-Control: no-store
//     (gateway-owned transient failure), X-Request-Id stamped last.
//
// The request id is read back from the X-Request-Id response header
// that buildServeInput stamps before the proxy handler runs. A panic
// upstream of that stamp (concurrency limiter, trusted-proxy
// middleware) finds the header empty; the handler then generates a
// fresh id so the client response and the log line still share a
// correlator.
//
// The middleware MUST be registered first on the engine so its
// deferred recover encloses every later middleware and the adapter.
func newRecoveryMiddleware(logger zerolog.Logger) app.HandlerFunc {
	return recovery.Recovery(recovery.WithRecoveryHandler(
		func(c context.Context, ctx *app.RequestContext, err interface{}, stack []byte) {
			observability.IncPanicRecovered()

			requestID := string(ctx.Response.Header.Peek("X-Request-Id"))
			if requestID == "" {
				requestID = observability.NewRequestID()
			}

			// fmt.Sprint instead of Interface(): the panic value is
			// an arbitrary interface{} and error values marshal to
			// "{}" under zerolog's JSON dump, losing the message.
			logger.Error().
				Str("request_id", requestID).
				Str("method", string(ctx.Method())).
				Str("path", string(ctx.Path())).
				Str("panic", fmt.Sprint(err)).
				Bytes("stack", stack).
				Msg("panic recovered")

			ctx.Response.Header.SetContentType(consts.MIMEApplicationJSON)
			ctx.Response.Header.Set("Cache-Control", "no-store")
			ctx.Response.Header.Set("X-Request-Id", requestID)
			ctx.Response.SetBody(gerrors.InternalError.Body)
			ctx.AbortWithStatus(gerrors.InternalError.Status)
		},
	))
}
