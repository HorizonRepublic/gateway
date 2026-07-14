package observability

import (
	"net/http"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Route label values for requests that never resolve to a registry
// route. Fixed strings keep the route label cardinality bounded by
// the registry's route-template set plus these two sentinels — a
// path-scanning client must never be able to mint new label values.
const (
	// RouteUnmatched labels requests that missed the routing table
	// (404) or matched a path with the wrong method (405).
	RouteUnmatched = "unmatched"
	// RoutePreflight labels CORS OPTIONS preflight requests, which
	// resolve through the Access-Control-Request-Method header rather
	// than the request's own method and are answered by the gateway
	// itself.
	RoutePreflight = "preflight"
)

// methodLabels is the closed set of HTTP method label values. Any
// method outside this set is folded into methodOther so a client
// sending garbage extension methods cannot inflate the method label
// cardinality. Lookup is a single map read — no per-request
// allocation.
var methodLabels = map[string]string{
	"GET":     "GET",
	"POST":    "POST",
	"PUT":     "PUT",
	"PATCH":   "PATCH",
	"DELETE":  "DELETE",
	"HEAD":    "HEAD",
	"OPTIONS": "OPTIONS",
}

// methodOther is the fold target for unknown HTTP methods.
const methodOther = "OTHER"

// statusClasses maps status/100 to the class label. Index 0 covers
// malformed statuses outside 100–599 so the label set is total.
var statusClasses = [...]string{"other", "1xx", "2xx", "3xx", "4xx", "5xx"}

// durationBuckets extends the default Prometheus buckets downward
// into the sub-millisecond range the gateway's hot path lives in.
// The upper tail matches the 30s default request timeout so slow
// upstream calls remain distinguishable from the timeout ceiling.
var durationBuckets = []float64{
	.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30,
}

// NATSRequesterStats is the narrow contract Metrics needs from the
// resilient NATS requester. Declared here instead of importing the
// transport package so the dependency points transport→observability,
// matching every other consumer of this package.
type NATSRequesterStats interface {
	// Inflight returns the number of NATS requests currently in
	// flight (admitted past the semaphore, reply not yet received).
	Inflight() int64
	// QueueTimeouts returns the monotonic count of requests shed
	// because no in-flight slot freed up within the queue timeout.
	QueueTimeouts() int64
	// BreakerTrips returns the monotonic count of circuit-breaker
	// transitions into the open state.
	BreakerTrips() int64
	// BreakerState returns the current breaker state using the
	// gobreaker encoding: 0 closed, 1 half-open, 2 open. A disabled
	// breaker reports 0 — it never opens, so "closed" is truthful.
	BreakerState() int64
}

// redKey identifies one (method, route, status-class) label tuple on
// the HTTP RED vectors. A comparable struct key makes the hot-path
// cache lookup allocation-free — composing a joined string key would
// allocate on every request.
type redKey struct {
	method string
	route  string
	status string
}

// redSample holds the pre-resolved child metrics for one label tuple
// so the hot path skips the label hashing inside
// CounterVec.WithLabelValues after the first request per tuple.
type redSample struct {
	requests prometheus.Counter
	duration prometheus.Observer
}

// Metrics owns the gateway's Prometheus registry and every metric the
// process exports. One instance is created at bootstrap and shared by
// the proxy handler (RED + in-flight), the registry watcher (reload
// counter), and the operator listener (scrape handler).
//
// Hot-path discipline: ObserveHTTPRequest resolves its label tuple
// through an RWMutex-guarded cache of pre-bound child metrics, so the
// steady state per request is one map read under RLock plus two
// atomic bumps — no label-slice allocation, no fmt, no string
// concatenation. Cache size is bounded by (methods ≤ 8) × (route
// templates + 2 sentinels) × (status classes ≤ 6).
type Metrics struct {
	registry *prometheus.Registry

	httpRequests *prometheus.CounterVec
	httpDuration *prometheus.HistogramVec
	httpInflight prometheus.Gauge

	registryReloads    prometheus.Counter
	registryLastReload prometheus.Gauge

	mu    sync.RWMutex
	cache map[redKey]redSample
}

// NewMetrics constructs the gateway metric surface on a fresh private
// registry: HTTP RED vectors, the in-flight gauge, registry reload
// counter/timestamp, and the standard Go runtime + process
// collectors. NATS and rate-limit metrics attach later via
// RegisterNATS / RegisterRateLimit because their sources are built
// after the metrics object during bootstrap.
//
// A private registry (rather than prometheus.DefaultRegisterer) keeps
// the scrape surface exactly what the gateway declares — third-party
// libraries that self-register on the global registry cannot leak
// metrics onto the operator listener.
func NewMetrics() *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		cache:    make(map[redKey]redSample),
	}

	m.httpRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_http_requests_total",
		Help: "HTTP requests processed by the proxy handler, by method, route template, and status class.",
	}, []string{"method", "route", "status"})

	m.httpDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "gateway_http_request_duration_seconds",
		Help:    "End-to-end proxy handler latency, by method, route template, and status class.",
		Buckets: durationBuckets,
	}, []string{"method", "route", "status"})

	m.httpInflight = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "gateway_http_inflight_requests",
		Help: "HTTP requests currently inside the proxy handler.",
	})

	m.registryReloads = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "gateway_registry_reloads_total",
		Help: "Route registry snapshot replacements, including the initial load.",
	})

	m.registryLastReload = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "gateway_registry_last_reload_timestamp_seconds",
		Help: "Unix time of the most recent route registry snapshot replacement.",
	})

	m.registry.MustRegister(
		m.httpRequests,
		m.httpDuration,
		m.httpInflight,
		m.registryReloads,
		m.registryLastReload,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	return m
}

// Handler returns the scrape endpoint for the operator listener. The
// handler serialises the private registry only — nothing registered
// on the global default registry is exposed.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// HTTPRequestStarted bumps the in-flight gauge. Paired with
// HTTPRequestFinished by the proxy handler around the request
// lifecycle; a single atomic add per call.
func (m *Metrics) HTTPRequestStarted() { m.httpInflight.Inc() }

// HTTPRequestFinished decrements the in-flight gauge.
func (m *Metrics) HTTPRequestFinished() { m.httpInflight.Dec() }

// ObserveHTTPRequest records one completed request on the RED
// vectors. method is folded into the closed method-label set and
// status into its class ("2xx"), so callers may pass raw values;
// route MUST be a route template or one of the Route* sentinels —
// never a raw request path.
func (m *Metrics) ObserveHTTPRequest(method, route string, status int, seconds float64) {
	key := redKey{
		method: normalizeMethod(method),
		route:  route,
		status: statusClass(status),
	}

	m.mu.RLock()
	sample, ok := m.cache[key]
	m.mu.RUnlock()

	if !ok {
		sample = m.bindSample(key)
	}

	sample.requests.Inc()
	sample.duration.Observe(seconds)
}

// bindSample resolves and caches the child metrics for a label tuple
// seen for the first time. Double-checked under the write lock so two
// racing first requests bind the same children.
func (m *Metrics) bindSample(key redKey) redSample {
	m.mu.Lock()
	defer m.mu.Unlock()

	if sample, ok := m.cache[key]; ok {
		return sample
	}

	sample := redSample{
		requests: m.httpRequests.WithLabelValues(key.method, key.route, key.status),
		duration: m.httpDuration.WithLabelValues(key.method, key.route, key.status),
	}
	m.cache[key] = sample

	return sample
}

// RecordRegistryReload bumps the reload counter and refreshes the
// last-reload timestamp. Wired as a registry watcher OnChange
// callback, so it fires for the initial snapshot and for every
// subsequent KV delta or reconcile that replaces the snapshot.
func (m *Metrics) RecordRegistryReload() {
	m.registryReloads.Inc()
	m.registryLastReload.SetToCurrentTime()
}

// RegisterNATS attaches gauges and counters that read the resilient
// requester's admission-control counters at scrape time. Func-backed
// metrics keep the requester's hot path free of any Prometheus
// coupling — it maintains plain atomics and this package samples them.
func (m *Metrics) RegisterNATS(stats NATSRequesterStats) {
	m.registry.MustRegister(
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "gateway_nats_inflight_requests",
			Help: "NATS requests currently in flight (admitted past the semaphore).",
		}, func() float64 { return float64(stats.Inflight()) }),
		prometheus.NewCounterFunc(prometheus.CounterOpts{
			Name: "gateway_nats_inflight_queue_timeouts_total",
			Help: "Requests shed because no in-flight slot freed within NATS_INFLIGHT_QUEUE_TIMEOUT.",
		}, func() float64 { return float64(stats.QueueTimeouts()) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "gateway_nats_breaker_state",
			Help: "Circuit breaker state: 0 closed, 1 half-open, 2 open.",
		}, func() float64 { return float64(stats.BreakerState()) }),
		prometheus.NewCounterFunc(prometheus.CounterOpts{
			Name: "gateway_nats_breaker_trips_total",
			Help: "Circuit breaker transitions into the open state.",
		}, func() float64 { return float64(stats.BreakerTrips()) }),
	)
}

// RegisterRateLimit attaches an unchecked collector that exports the
// rate-limit Router's Counters snapshots. snapshot is called once per
// scrape; the outer map key is the backend id and the inner keys are
// the stable metric names each Store already emits (the reserved
// `ratelimit_*` schema), so dashboards read the same names whether
// they arrived via this exporter or a future OTel pipeline.
func (m *Metrics) RegisterRateLimit(snapshot func() map[string]map[string]int64) {
	m.registry.MustRegister(&rateLimitCollector{snapshot: snapshot})
}

// rateLimitCollector adapts the ratelimit Counters snapshot contract
// to prometheus.Collector. It is an unchecked collector (Describe
// sends nothing) because the metric set depends on which backends the
// registry's routes have instantiated — unknown at registration time.
type rateLimitCollector struct {
	snapshot func() map[string]map[string]int64
}

var _ prometheus.Collector = (*rateLimitCollector)(nil)

// Describe intentionally sends nothing, making this an unchecked
// collector: the registry skips descriptor consistency checks and
// accepts whatever Collect produces at scrape time.
func (c *rateLimitCollector) Describe(chan<- *prometheus.Desc) {}

// Collect materialises one const metric per snapshot key. Keys ending
// in `_total` are monotonic counters per the Store.Counters naming
// contract; everything else (state gauges) is exported as a gauge.
// Inner keys already embed the backend id, so names are unique across
// the outer map and no backend label is needed.
func (c *rateLimitCollector) Collect(ch chan<- prometheus.Metric) {
	for _, counters := range c.snapshot() {
		for name, value := range counters {
			valueType := prometheus.GaugeValue
			if isCounterName(name) {
				valueType = prometheus.CounterValue
			}
			ch <- prometheus.MustNewConstMetric(
				prometheus.NewDesc(name, "Rate-limit counter exported from Store.Counters.", nil, nil),
				valueType,
				float64(value),
			)
		}
	}
}

// isCounterName reports whether a snapshot key follows the `_total`
// monotonic-counter naming contract.
func isCounterName(name string) bool {
	const suffix = "_total"
	return len(name) >= len(suffix) && name[len(name)-len(suffix):] == suffix
}

// normalizeMethod folds an HTTP method into the closed label set.
func normalizeMethod(method string) string {
	if v, ok := methodLabels[method]; ok {
		return v
	}

	return methodOther
}

// statusClass maps an HTTP status code to its class label.
func statusClass(status int) string {
	idx := status / 100
	if idx < 1 || idx > 5 {
		idx = 0
	}

	return statusClasses[idx]
}
