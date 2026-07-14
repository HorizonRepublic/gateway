package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/observability"
	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/registry"
	"github.com/HorizonRepublic/gateway/apps/gateway-server/internal/routing"
)

// accessLogEvents extracts every http.access event from a zerolog
// buffer so tests can assert exactly-once emission and field content
// without string-scanning the raw output.
func accessLogEvents(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()

	var events []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &entry), "log line must be JSON: %s", line)
		if entry["event"] == "http.access" {
			events = append(events, entry)
		}
	}

	return events
}

// observedHandler builds a handler with the access log enabled and the
// log stream captured in buf.
func observedHandler(table routing.Table, nats *fakeRequester, buf *bytes.Buffer, metrics *observability.Metrics) *Handler {
	return NewHandler(HandlerConfig{
		Table:     func() routing.Table { return table },
		Nats:      nats,
		Encoder:   NewDefaultEncoder(),
		Decoder:   NewDefaultDecoder(),
		Timeout:   time.Second,
		Logger:    zerolog.New(buf),
		Metrics:   metrics,
		AccessLog: buf != nil,
	})
}

// TestHandler_AccessLogEmitsSingleEventWithAllFields pins the access
// log contract: exactly one http.access event per request carrying
// the full field set — method, route template, status, duration,
// bytes out, client IP, request id, NATS subject, and both gate
// outcomes.
func TestHandler_AccessLogEmitsSingleEventWithAllFields(t *testing.T) {
	table := &fakeTable{routes: map[string]routing.Route{
		"GET /users/:id": {Subject: "svc.cmd.users.get", PathTemplate: "/users/:id", Method: "GET"},
	}}
	nats := newFakeNats()
	nats.reply = []byte(`{"status":200,"headers":{},"body":{"ok":true}}`)

	var buf bytes.Buffer
	h := observedHandler(table, nats, &buf, nil)

	in := emptyServeInput("GET", "/users/:id")
	in.RequestID = "req-access-1"
	in.RemoteAddr = "203.0.113.9"

	result := h.Handle(context.Background(), in)
	require.Equal(t, 200, result.Status)

	events := accessLogEvents(t, &buf)
	require.Len(t, events, 1, "exactly one access event per request")

	event := events[0]
	assert.Equal(t, "GET", event["method"])
	assert.Equal(t, "/users/:id", event["route"])
	assert.InDelta(t, 200, event["status"], 0)
	assert.Equal(t, "203.0.113.9", event["client_ip"])
	assert.Equal(t, "req-access-1", event["request_id"])
	assert.Equal(t, "svc.cmd.users.get", event["subject"])
	assert.Equal(t, "none", event["auth"])
	assert.Equal(t, "none", event["ratelimit"])
	assert.Equal(t, "request completed", event["message"])

	duration, ok := event["duration_ms"].(float64)
	require.True(t, ok, "duration_ms must be numeric")
	assert.GreaterOrEqual(t, duration, 0.0)

	bytesOut, ok := event["bytes_out"].(float64)
	require.True(t, ok, "bytes_out must be numeric")
	assert.InDelta(t, float64(len(result.Body)), bytesOut, 0)
}

// TestHandler_AccessLogRouteMissUsesUnmatchedSentinel pins the
// cardinality discipline on the log side too: a 404 carries the
// "unmatched" sentinel, never the attacker-controlled raw path, so
// log-derived dashboards stay bounded.
func TestHandler_AccessLogRouteMissUsesUnmatchedSentinel(t *testing.T) {
	table := &fakeTable{routes: map[string]routing.Route{}}
	var buf bytes.Buffer
	h := observedHandler(table, newFakeNats(), &buf, nil)

	result := h.Handle(context.Background(), emptyServeInput("GET", "/no-such-path"))
	require.Equal(t, 404, result.Status)

	events := accessLogEvents(t, &buf)
	require.Len(t, events, 1)
	assert.Equal(t, observability.RouteUnmatched, events[0]["route"])
	assert.Empty(t, events[0]["subject"])
	assert.InDelta(t, 404, events[0]["status"], 0)
}

// TestHandler_AccessLogDisabledEmitsNoEvent pins the ACCESS_LOG_ENABLED
// off switch: with the knob down, the happy path writes nothing at all
// to the log stream.
func TestHandler_AccessLogDisabledEmitsNoEvent(t *testing.T) {
	table := &fakeTable{routes: map[string]routing.Route{
		"GET /users": {Subject: "svc.cmd.users.list", PathTemplate: "/users", Method: "GET"},
	}}
	nats := newFakeNats()
	nats.reply = []byte(`{"status":200,"headers":{},"body":null}`)

	var buf bytes.Buffer
	h := NewHandler(HandlerConfig{
		Table:     func() routing.Table { return table },
		Nats:      nats,
		Encoder:   NewDefaultEncoder(),
		Decoder:   NewDefaultDecoder(),
		Timeout:   time.Second,
		Logger:    zerolog.New(&buf),
		AccessLog: false,
	})

	result := h.Handle(context.Background(), emptyServeInput("GET", "/users"))
	require.Equal(t, 200, result.Status)

	assert.Empty(t, buf.String(), "no access event and no other log output on the happy path")
}

// TestHandler_AccessLogRecordsAuthAndRateLimitOutcomes pins the gate
// outcome vocabulary on a protected, rate-limited route: verifier 200
// maps to auth=ok, a healthy bucket pass maps to ratelimit=allowed.
func TestHandler_AccessLogRecordsAuthAndRateLimitOutcomes(t *testing.T) {
	verifierSubject := "svc.cmd.auth.verifier.jwt"
	table := &fakeTable{routes: map[string]routing.Route{
		"GET /me": {
			Subject:      "svc.cmd.users.me",
			PathTemplate: "/me",
			Method:       "GET",
			Auth:         &routing.RouteAuth{VerifierSubject: verifierSubject},
			RateLimit:    &registry.RateLimitMeta{RPS: 10, Burst: 20},
		},
	}}
	nats := newFakeNats()
	nats.program(verifierSubject, []byte(`{"status":200,"headers":{},"body":{"sub":"u1"}}`), nil)
	nats.reply = []byte(`{"status":200,"headers":{},"body":null}`)

	var buf bytes.Buffer
	rl := &fakeRateLimiter{allowed: true}
	h := NewHandler(HandlerConfig{
		Table:       func() routing.Table { return table },
		Nats:        nats,
		Encoder:     NewDefaultEncoder(),
		Decoder:     NewDefaultDecoder(),
		Timeout:     time.Second,
		Logger:      zerolog.New(&buf),
		RateLimiter: routerWithStore(t, rl),
		AccessLog:   true,
	})

	in := emptyServeInput("GET", "/me")
	in.RemoteAddr = "198.51.100.7"

	result := h.Handle(context.Background(), in)
	require.Equal(t, 200, result.Status)

	events := accessLogEvents(t, &buf)
	require.Len(t, events, 1)
	assert.Equal(t, "ok", events[0]["auth"])
	assert.Equal(t, "allowed", events[0]["ratelimit"])
	assert.Equal(t, "svc.cmd.users.me", events[0]["subject"])
}

// TestHandler_AccessLogRateLimitRejectedOutcome pins the 429 shape:
// the event carries ratelimit=rejected and the 429 status.
func TestHandler_AccessLogRateLimitRejectedOutcome(t *testing.T) {
	table := &fakeTable{routes: map[string]routing.Route{
		"GET /users": {
			Subject: "svc.cmd.users.list", PathTemplate: "/users", Method: "GET",
			RateLimit: &registry.RateLimitMeta{RPS: 10, Burst: 20},
		},
	}}
	nats := newFakeNats()

	var buf bytes.Buffer
	rl := &fakeRateLimiter{allowed: false}
	h := NewHandler(HandlerConfig{
		Table:       func() routing.Table { return table },
		Nats:        nats,
		Encoder:     NewDefaultEncoder(),
		Decoder:     NewDefaultDecoder(),
		Timeout:     time.Second,
		Logger:      zerolog.New(&buf),
		RateLimiter: routerWithStore(t, rl),
		AccessLog:   true,
	})

	in := emptyServeInput("GET", "/users")
	in.RemoteAddr = "198.51.100.7"

	result := h.Handle(context.Background(), in)
	require.Equal(t, 429, result.Status)

	events := accessLogEvents(t, &buf)
	require.Len(t, events, 1)
	assert.Equal(t, "rejected", events[0]["ratelimit"])
	assert.InDelta(t, 429, events[0]["status"], 0)
}

// TestHandler_MetricsRecordREDAndInflight drives real traffic through
// the handler with a live Metrics instance and asserts the scrape
// output: the RED counter accumulates under the route-template label
// and the in-flight gauge returns to zero once requests complete.
func TestHandler_MetricsRecordREDAndInflight(t *testing.T) {
	table := &fakeTable{routes: map[string]routing.Route{
		"GET /users/:id": {Subject: "svc.cmd.users.get", PathTemplate: "/users/:id", Method: "GET"},
	}}
	nats := newFakeNats()
	nats.reply = []byte(`{"status":200,"headers":{},"body":null}`)

	metrics := observability.NewMetrics()
	h := NewHandler(HandlerConfig{
		Table:   func() routing.Table { return table },
		Nats:    nats,
		Encoder: NewDefaultEncoder(),
		Decoder: NewDefaultDecoder(),
		Timeout: time.Second,
		Logger:  zerolog.Nop(),
		Metrics: metrics,
	})

	for range 3 {
		result := h.Handle(context.Background(), emptyServeInput("GET", "/users/:id"))
		require.Equal(t, 200, result.Status)
	}
	// One 404 lands on the unmatched sentinel.
	h.Handle(context.Background(), emptyServeInput("GET", "/missing"))

	rec := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	exposition := rec.Body.String()

	assert.Contains(t, exposition,
		`gateway_http_requests_total{method="GET",route="/users/:id",status="2xx"} 3`)
	assert.Contains(t, exposition,
		`gateway_http_requests_total{method="GET",route="unmatched",status="4xx"} 1`)
	assert.Contains(t, exposition,
		`gateway_http_request_duration_seconds_count{method="GET",route="/users/:id",status="2xx"} 3`)
	assert.Contains(t, exposition, "gateway_http_inflight_requests 0",
		"in-flight gauge must return to zero after requests complete")
}
