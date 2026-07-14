package http

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gerrors "github.com/HorizonRepublic/gateway/apps/gateway-server/internal/errors"
	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/observability"
)

// runPanickingChain drives one request through the recovery middleware
// followed by a handler that panics with the supplied value, capturing
// every log event into the returned buffer.
func runPanickingChain(t *testing.T, panicValue interface{}, prep func(*app.RequestContext)) (*app.RequestContext, *bytes.Buffer) {
	t.Helper()

	var logBuf bytes.Buffer
	logger := zerolog.New(&logBuf)

	ctx := app.NewContext(0)
	if prep != nil {
		prep(ctx)
	}
	ctx.SetHandlers([]app.HandlerFunc{
		newRecoveryMiddleware(logger),
		func(_ context.Context, _ *app.RequestContext) {
			panic(panicValue)
		},
	})
	ctx.Next(context.Background())

	return ctx, &logBuf
}

// TestRecovery_PanicYields500WithSharedErrorBody pins the client-facing
// contract: a panicking handler produces exactly the pre-encoded
// InternalError JSON body with the same header discipline as every
// other gateway-owned error (JSON content type, Cache-Control:
// no-store, X-Request-Id present) — not Hertz's stock empty 500.
func TestRecovery_PanicYields500WithSharedErrorBody(t *testing.T) {
	ctx, _ := runPanickingChain(t, "kaboom", func(ctx *app.RequestContext) {
		ctx.Request.Header.SetMethod("POST")
		ctx.Request.SetRequestURI("/v1/patients")
		ctx.Response.Header.Set("X-Request-Id", "01TESTULIDTESTULIDTESTULID")
	})

	assert.Equal(t, gerrors.InternalError.Status, ctx.Response.StatusCode())
	assert.Equal(t, string(gerrors.InternalError.Body), string(ctx.Response.Body()))
	assert.Equal(t, consts.MIMEApplicationJSON, string(ctx.Response.Header.ContentType()))
	assert.Equal(t, "no-store", string(ctx.Response.Header.Peek("Cache-Control")))
	assert.Equal(t, "01TESTULIDTESTULIDTESTULID",
		string(ctx.Response.Header.Peek("X-Request-Id")),
		"adapter-stamped correlator must survive onto the panic response")
}

// TestRecovery_LogsPanicWithRequestContextAndStack pins the operator-
// facing contract: one structured ERROR event carrying the request
// correlator, method, path, panic value, and a non-empty stack trace.
func TestRecovery_LogsPanicWithRequestContextAndStack(t *testing.T) {
	_, logBuf := runPanickingChain(t, "kaboom", func(ctx *app.RequestContext) {
		ctx.Request.Header.SetMethod("POST")
		ctx.Request.SetRequestURI("/v1/patients")
		ctx.Response.Header.Set("X-Request-Id", "01TESTULIDTESTULIDTESTULID")
	})

	var event map[string]interface{}
	require.NoError(t, json.Unmarshal(logBuf.Bytes(), &event),
		"recovery must emit exactly one JSON log event, got: %s", logBuf.String())

	assert.Equal(t, "error", event["level"])
	assert.Equal(t, "panic recovered", event["message"])
	assert.Equal(t, "01TESTULIDTESTULIDTESTULID", event["request_id"])
	assert.Equal(t, "POST", event["method"])
	assert.Equal(t, "/v1/patients", event["path"])
	assert.Equal(t, "kaboom", event["panic"])
	assert.Contains(t, event["stack"], "recovery_middleware_test",
		"stack trace must point at the panicking frame")
}

// TestRecovery_GeneratesRequestIDWhenPanicPrecedesAdapter covers a
// panic upstream of buildServeInput (e.g. inside an earlier
// middleware): no X-Request-Id has been stamped yet, so the recovery
// handler must mint one and use the SAME id on the response header and
// in the log event — a 500 without a correlator is undebuggable.
func TestRecovery_GeneratesRequestIDWhenPanicPrecedesAdapter(t *testing.T) {
	ctx, logBuf := runPanickingChain(t, "early", nil)

	responseID := string(ctx.Response.Header.Peek("X-Request-Id"))
	assert.Len(t, responseID, 26, "generated correlator must be a 26-char ULID")

	var event map[string]interface{}
	require.NoError(t, json.Unmarshal(logBuf.Bytes(), &event))
	assert.Equal(t, responseID, event["request_id"],
		"log event and response must share the generated correlator")
}

// TestRecovery_IncrementsPanicCounter pins the metrics hook: every
// recovered panic moves the observability counter by exactly one, so a
// future metrics registry (or an operator with a debugger) can see
// panic frequency even when log lines are sampled away.
func TestRecovery_IncrementsPanicCounter(t *testing.T) {
	before := observability.PanicsRecovered()

	runPanickingChain(t, "counted", nil)

	assert.Equal(t, before+1, observability.PanicsRecovered())
}

// TestRecovery_NonStringPanicValueIsStringified pins that panics with
// non-string values (errors, structs) land in the log as their
// fmt.Sprint rendering instead of zerolog's lossy "{}" interface dump.
func TestRecovery_NonStringPanicValueIsStringified(t *testing.T) {
	_, logBuf := runPanickingChain(t, assert.AnError, nil)

	var event map[string]interface{}
	require.NoError(t, json.Unmarshal(logBuf.Bytes(), &event))
	assert.Equal(t, assert.AnError.Error(), event["panic"])
}
