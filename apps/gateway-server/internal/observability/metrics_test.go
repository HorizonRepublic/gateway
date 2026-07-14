package observability

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gatherNames collects the metric family names currently exposed by
// the private registry so tests can assert schema presence without
// string-matching the full exposition format.
func gatherNames(t *testing.T, m *Metrics) map[string]struct{} {
	t.Helper()

	families, err := m.registry.Gather()
	require.NoError(t, err)

	names := make(map[string]struct{}, len(families))
	for _, mf := range families {
		names[mf.GetName()] = struct{}{}
	}

	return names
}

// TestNewMetrics_RegistersCoreSchema pins the scrape schema the
// dashboards are built on: the RED vectors, in-flight gauge, registry
// reload pair, and the Go runtime collector. RED vectors surface only
// after the first observation (vectors with zero children emit no
// family), so one request is observed first.
func TestNewMetrics_RegistersCoreSchema(t *testing.T) {
	m := NewMetrics()
	m.ObserveHTTPRequest("GET", "/users/:id", 200, 0.01)
	m.RecordRegistryReload()

	names := gatherNames(t, m)

	for _, want := range []string{
		"gateway_http_requests_total",
		"gateway_http_request_duration_seconds",
		"gateway_http_inflight_requests",
		"gateway_registry_reloads_total",
		"gateway_registry_last_reload_timestamp_seconds",
		"go_goroutines",
	} {
		assert.Contains(t, names, want)
	}
}

// TestObserveHTTPRequest_CountsByLabelTuple pins the label shape:
// method, route template, status class — and that repeated
// observations accumulate on the same child (the cache returns the
// bound sample, not a new one).
func TestObserveHTTPRequest_CountsByLabelTuple(t *testing.T) {
	m := NewMetrics()

	m.ObserveHTTPRequest("GET", "/users/:id", 200, 0.01)
	m.ObserveHTTPRequest("GET", "/users/:id", 204, 0.02)
	m.ObserveHTTPRequest("GET", "/users/:id", 502, 0.03)

	counter, err := m.httpRequests.GetMetricWithLabelValues("GET", "/users/:id", "2xx")
	require.NoError(t, err)
	assert.InDelta(t, 2.0, testutil.ToFloat64(counter), 0)

	counter5xx, err := m.httpRequests.GetMetricWithLabelValues("GET", "/users/:id", "5xx")
	require.NoError(t, err)
	assert.InDelta(t, 1.0, testutil.ToFloat64(counter5xx), 0)
}

// TestObserveHTTPRequest_FoldsUnknownMethods pins the cardinality
// guard: a client minting garbage extension methods lands on the
// single OTHER label value instead of growing the method label set.
func TestObserveHTTPRequest_FoldsUnknownMethods(t *testing.T) {
	m := NewMetrics()

	m.ObserveHTTPRequest("YOLO", RouteUnmatched, 404, 0.001)
	m.ObserveHTTPRequest("PROPFIND", RouteUnmatched, 404, 0.001)

	counter, err := m.httpRequests.GetMetricWithLabelValues("OTHER", RouteUnmatched, "4xx")
	require.NoError(t, err)
	assert.InDelta(t, 2.0, testutil.ToFloat64(counter), 0)
}

// TestStatusClass_MapsFullRange pins the class mapping including the
// out-of-range fold so a buggy upstream status (0, 999) cannot mint
// new label values.
func TestStatusClass_MapsFullRange(t *testing.T) {
	cases := map[int]string{
		100: "1xx", 204: "2xx", 301: "3xx", 404: "4xx", 503: "5xx",
		0: "other", 99: "other", 600: "other", 999: "other",
	}
	for status, want := range cases {
		assert.Equal(t, want, statusClass(status), "status %d", status)
	}
}

// TestInflightGauge_TracksStartFinish pins the gauge pairing the
// proxy handler relies on around each request.
func TestInflightGauge_TracksStartFinish(t *testing.T) {
	m := NewMetrics()

	m.HTTPRequestStarted()
	m.HTTPRequestStarted()
	assert.InDelta(t, 2.0, testutil.ToFloat64(m.httpInflight), 0)

	m.HTTPRequestFinished()
	assert.InDelta(t, 1.0, testutil.ToFloat64(m.httpInflight), 0)
}

// TestRecordRegistryReload_BumpsCounterAndTimestamp pins the reload
// pair: monotonic count plus a wall-clock timestamp operators alert
// on ("no reload observed in N minutes" during registry incidents).
func TestRecordRegistryReload_BumpsCounterAndTimestamp(t *testing.T) {
	m := NewMetrics()

	m.RecordRegistryReload()
	m.RecordRegistryReload()

	assert.InDelta(t, 2.0, testutil.ToFloat64(m.registryReloads), 0)
	assert.Greater(t, testutil.ToFloat64(m.registryLastReload), 1.0e9,
		"last-reload gauge must carry a plausible unix timestamp")
}

// fakeNATSStats is a canned NATSRequesterStats source.
type fakeNATSStats struct {
	inflight, queueTimeouts, trips, state int64
}

func (f *fakeNATSStats) Inflight() int64      { return f.inflight }
func (f *fakeNATSStats) QueueTimeouts() int64 { return f.queueTimeouts }
func (f *fakeNATSStats) BreakerTrips() int64  { return f.trips }
func (f *fakeNATSStats) BreakerState() int64  { return f.state }

// TestRegisterNATS_ExportsAdmissionMetrics pins the func-backed NATS
// metric surface: values are sampled from the requester at gather
// time, so the exported numbers always match the accessors.
func TestRegisterNATS_ExportsAdmissionMetrics(t *testing.T) {
	m := NewMetrics()
	stats := &fakeNATSStats{inflight: 7, queueTimeouts: 3, trips: 2, state: 1}

	m.RegisterNATS(stats)

	names := gatherNames(t, m)
	for _, want := range []string{
		"gateway_nats_inflight_requests",
		"gateway_nats_inflight_queue_timeouts_total",
		"gateway_nats_breaker_state",
		"gateway_nats_breaker_trips_total",
	} {
		assert.Contains(t, names, want)
	}

	families, err := m.registry.Gather()
	require.NoError(t, err)
	values := map[string]float64{}
	for _, mf := range families {
		if len(mf.GetMetric()) != 1 {
			continue
		}
		metric := mf.GetMetric()[0]
		switch {
		case metric.GetGauge() != nil:
			values[mf.GetName()] = metric.GetGauge().GetValue()
		case metric.GetCounter() != nil:
			values[mf.GetName()] = metric.GetCounter().GetValue()
		}
	}
	assert.InDelta(t, 7.0, values["gateway_nats_inflight_requests"], 0)
	assert.InDelta(t, 3.0, values["gateway_nats_inflight_queue_timeouts_total"], 0)
	assert.InDelta(t, 1.0, values["gateway_nats_breaker_state"], 0)
	assert.InDelta(t, 2.0, values["gateway_nats_breaker_trips_total"], 0)
}

// TestRegisterRateLimit_ExportsSnapshotKeys pins the pass-through of
// the reserved ratelimit_* schema: keys ending _total surface as
// counters, state keys as gauges, and values track the snapshot on
// every gather.
func TestRegisterRateLimit_ExportsSnapshotKeys(t *testing.T) {
	m := NewMetrics()
	snapshot := map[string]map[string]int64{
		"memory": {
			"ratelimit_memory_decisions_allowed_total": 41,
		},
		"router": {
			"ratelimit_store_fallback_total": 5,
			"ratelimit_natskv_circuit_state": 2,
		},
	}

	m.RegisterRateLimit(func() map[string]map[string]int64 { return snapshot })

	families, err := m.registry.Gather()
	require.NoError(t, err)

	byName := map[string]float64{}
	counters := map[string]bool{}
	for _, mf := range families {
		if len(mf.GetMetric()) != 1 {
			continue
		}
		metric := mf.GetMetric()[0]
		if metric.GetCounter() != nil {
			byName[mf.GetName()] = metric.GetCounter().GetValue()
			counters[mf.GetName()] = true
		}
		if metric.GetGauge() != nil {
			byName[mf.GetName()] = metric.GetGauge().GetValue()
		}
	}

	assert.InDelta(t, 41.0, byName["ratelimit_memory_decisions_allowed_total"], 0)
	assert.True(t, counters["ratelimit_memory_decisions_allowed_total"],
		"_total keys must export as counters")
	assert.InDelta(t, 5.0, byName["ratelimit_store_fallback_total"], 0)
	assert.InDelta(t, 2.0, byName["ratelimit_natskv_circuit_state"], 0)
	assert.False(t, counters["ratelimit_natskv_circuit_state"],
		"state keys must export as gauges")
}
