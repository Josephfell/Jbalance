// Package dataplane: metrics.go instruments the proxy request path (both
// L7 HTTP and L4 TCP) with two complementary outputs:
//
//   - A Prometheus registry exposed via /metrics, for anyone wiring this
//     tool into existing Grafana/Prometheus infrastructure — the
//     standard way a real monitoring stack would consume it.
//   - A small in-memory ring-buffer time series (Recent), independent of
//     Prometheus, that backs the admin web UI's own live charts without
//     requiring a separate Prometheus server just to see "what's
//     happening right now" from the dashboard.
//
// Both read from the same underlying counters — recording a request
// updates both outputs in one call, so they can never drift apart.
package dataplane

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics holds every counter/gauge/histogram this data plane instance
// exposes, plus the in-memory series the admin UI's charts read from.
// One Metrics is shared process-wide (created once in cmd/dataplane and
// threaded through Proxy/TCPProxy), not one per group — group is carried
// as a label instead, so a single /metrics scrape covers every group this
// instance proxies to.
type Metrics struct {
	requestsTotal   *prometheus.CounterVec
	requestDuration *prometheus.HistogramVec
	activeConns     *prometheus.GaugeVec
	tcpBytesTotal   *prometheus.CounterVec
	tcpConnsTotal   *prometheus.CounterVec
	tcpActiveConns  *prometheus.GaugeVec

	series   *seriesStore
	perGroup *perGroupStats
}

// NewMetrics creates a Metrics instance and registers all of its
// collectors with reg. Panics if registration fails (e.g. a duplicate
// metric name) — that's a programming error, not a runtime condition to
// handle gracefully, and would indicate this function was called twice
// against the same registry.
func NewMetrics(reg *prometheus.Registry) *Metrics {
	m := &Metrics{
		requestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "jbalance_http_requests_total",
			Help: "Total number of HTTP requests proxied, by group and response status class.",
		}, []string{"group", "status"}),
		requestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "jbalance_http_request_duration_seconds",
			Help:    "HTTP request duration as observed by the proxy (time to first byte of the upstream response), by group.",
			Buckets: prometheus.DefBuckets,
		}, []string{"group"}),
		activeConns: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "jbalance_active_connections",
			Help: "Current number of in-flight requests/connections being proxied, by group.",
		}, []string{"group"}),
		tcpBytesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "jbalance_tcp_bytes_total",
			Help: "Total bytes forwarded by the L4 TCP proxy, by group and direction.",
		}, []string{"group", "direction"}),
		tcpConnsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "jbalance_tcp_connections_total",
			Help: "Total TCP connections accepted by the L4 proxy, by group.",
		}, []string{"group"}),
		tcpActiveConns: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "jbalance_tcp_active_connections",
			Help: "Current number of open TCP connections being proxied, by group.",
		}, []string{"group"}),
		series:   newSeriesStore(),
		perGroup: newPerGroupStats(),
	}

	reg.MustRegister(
		m.requestsTotal,
		m.requestDuration,
		m.activeConns,
		m.tcpBytesTotal,
		m.tcpConnsTotal,
		m.tcpActiveConns,
	)
	return m
}

// RegisterBackendsCollector registers a live-reading collector for
// jbalance_backends_healthy/jbalance_backends_total against reg, backed
// by groups. Separate from NewMetrics because GroupManager isn't
// necessarily constructed yet at the point Metrics itself needs to
// exist (cmd/dataplane wires NewMetrics before NewGroupManager) — call
// this once GroupManager is available.
func RegisterBackendsCollector(reg *prometheus.Registry, groups *GroupManager) {
	reg.MustRegister(newBackendsCollector(groups))
}

// statusClass reduces an HTTP status code to Prometheus's conventional
// "Nxx" class label (e.g. 200 -> "2xx"), keeping the requestsTotal
// cardinality bounded regardless of how many distinct status codes a
// backend returns.
func statusClass(code int) string {
	switch {
	case code >= 500:
		return "5xx"
	case code >= 400:
		return "4xx"
	case code >= 300:
		return "3xx"
	case code >= 200:
		return "2xx"
	default:
		return "1xx"
	}
}

// ObserveHTTPRequest records one completed HTTP request: updates the
// Prometheus counters/histogram and appends to the in-memory series the
// admin UI charts read from. Called once per request, after the response
// has been written.
func (m *Metrics) ObserveHTTPRequest(group string, statusCode int, duration time.Duration) {
	class := statusClass(statusCode)
	m.requestsTotal.WithLabelValues(group, class).Inc()
	m.requestDuration.WithLabelValues(group).Observe(duration.Seconds())
	m.series.recordRequest(group, class, duration)
	m.perGroup.recordRequest(group, class == "5xx", duration)
}

// SetActiveConnections updates the current in-flight request/connection
// gauge for group — called from the proxy's request lifecycle (increment
// on start, decrement on completion) rather than derived from anything
// else, since "currently in flight" isn't retrievable after the fact.
func (m *Metrics) SetActiveConnections(group string, delta int) {
	if delta > 0 {
		m.activeConns.WithLabelValues(group).Add(float64(delta))
	} else if delta < 0 {
		m.activeConns.WithLabelValues(group).Sub(float64(-delta))
	}
	m.series.recordActiveConns(group, delta)
	m.perGroup.setActiveConns(group, delta)
}

// backendsCollector reports jbalance_backends_healthy/jbalance_backends_total
// by reading live from a GroupManager at scrape time, rather than being
// pushed to on every BackendList change — a pull-on-scrape collector
// can't drift out of sync with reality the way a cached gauge updated
// from scattered call sites could, and GroupManager.Groups()/Ensure()
// are already cheap, lock-protected reads.
type backendsCollector struct {
	groups  *GroupManager
	healthy *prometheus.Desc
	total   *prometheus.Desc
}

func newBackendsCollector(groups *GroupManager) *backendsCollector {
	return &backendsCollector{
		groups:  groups,
		healthy: prometheus.NewDesc("jbalance_backends_healthy", "Current number of healthy backends, by group.", []string{"group"}, nil),
		total:   prometheus.NewDesc("jbalance_backends_total", "Current total number of known backends (healthy or not), by group.", []string{"group"}, nil),
	}
}

func (c *backendsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.healthy
	ch <- c.total
}

func (c *backendsCollector) Collect(ch chan<- prometheus.Metric) {
	for _, group := range c.groups.Groups() {
		bl := c.groups.Ensure(group) // always already-created for a tracked group name — Ensure is idempotent, this never starts a new subscription
		ch <- prometheus.MustNewConstMetric(c.healthy, prometheus.GaugeValue, float64(bl.HealthyLen()), group)
		ch <- prometheus.MustNewConstMetric(c.total, prometheus.GaugeValue, float64(bl.Len()), group)
	}
}

// ObserveTCPConnection records one L4 TCP connection's lifecycle: called
// once when accepted (bytesIn/bytesOut both zero, just to count the
// connection) and doesn't need a separate "closed" call — bytes are
// reported via ObserveTCPBytes as they're copied, and the active-gauge
// delta is handled by the caller via SetTCPActiveConnections.
func (m *Metrics) ObserveTCPConnection(group string) {
	m.tcpConnsTotal.WithLabelValues(group).Inc()
	m.series.recordTCPConnection(group)
}

// ObserveTCPBytes records bytes forwarded in one direction ("in" from
// client to backend, "out" from backend to client) for group.
func (m *Metrics) ObserveTCPBytes(group, direction string, n int64) {
	if n <= 0 {
		return
	}
	m.tcpBytesTotal.WithLabelValues(group, direction).Add(float64(n))
	m.series.recordTCPBytes(group, direction, n)
}

// SetTCPActiveConnections updates the current open-TCP-connection gauge
// for group.
func (m *Metrics) SetTCPActiveConnections(group string, delta int) {
	if delta > 0 {
		m.tcpActiveConns.WithLabelValues(group).Add(float64(delta))
	} else if delta < 0 {
		m.tcpActiveConns.WithLabelValues(group).Sub(float64(-delta))
	}
}

// Series returns the in-memory time series backing the admin UI's charts.
func (m *Metrics) Series() *seriesStore {
	return m.series
}

// GroupSnapshot returns per-group summary stats suitable for pushing to
// the control plane via ReportMetrics (MetricsReporter uses this), and
// resets the interval-scoped fields (average duration) so each report
// reflects only traffic since the last one, not an all-time average that
// would smooth away a recent latency spike.
func (m *Metrics) GroupSnapshot() []GroupSnapshot {
	return m.perGroup.snapshotAndResetInterval()
}

// GroupSnapshot is one group's summary stats at report time.
type GroupSnapshot struct {
	Group          string
	RequestsTotal  int64
	Errors5xxTotal int64
	ActiveConns    int64
	AvgDurationMs  float64
}

// perGroupStats tracks the small set of cumulative counters/gauges
// needed for MetricsReport, independent of both the Prometheus registry
// (write-only from this process's perspective, not readable back for
// building a report) and seriesStore (which aggregates across all
// groups, whereas ReportMetrics needs one line per group).
type perGroupStats struct {
	mu     sync.Mutex
	groups map[string]*groupCounters
}

type groupCounters struct {
	requestsTotal  int64
	errors5xxTotal int64
	activeConns    int64
	// intervalDurationTotal/intervalDurationCount are reset every
	// snapshotAndResetInterval call, so AvgDurationMs in the resulting
	// GroupSnapshot always covers only "since the last report", not
	// all-time.
	intervalDurationTotal time.Duration
	intervalDurationCount int64
}

func newPerGroupStats() *perGroupStats {
	return &perGroupStats{groups: make(map[string]*groupCounters)}
}

func (p *perGroupStats) entry(group string) *groupCounters {
	p.mu.Lock()
	defer p.mu.Unlock()
	g, ok := p.groups[group]
	if !ok {
		g = &groupCounters{}
		p.groups[group] = g
	}
	return g
}

func (p *perGroupStats) recordRequest(group string, is5xx bool, d time.Duration) {
	g := p.entry(group)
	p.mu.Lock()
	defer p.mu.Unlock()
	g.requestsTotal++
	if is5xx {
		g.errors5xxTotal++
	}
	g.intervalDurationTotal += d
	g.intervalDurationCount++
}

func (p *perGroupStats) setActiveConns(group string, delta int) {
	g := p.entry(group)
	p.mu.Lock()
	defer p.mu.Unlock()
	g.activeConns += int64(delta)
	if g.activeConns < 0 {
		g.activeConns = 0
	}
}

// snapshotAndResetInterval returns a GroupSnapshot per known group and
// resets each group's interval-scoped duration accumulator. requestsTotal/
// errors5xxTotal/activeConns are NOT reset — those are meant to read as
// cumulative-since-process-start (matching Prometheus counter semantics),
// with rate-of-change left for the control plane/admin UI to derive from
// successive reports, rather than the data plane pre-computing a rate
// that would be sensitive to the exact reporting interval.
func (p *perGroupStats) snapshotAndResetInterval() []GroupSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()

	out := make([]GroupSnapshot, 0, len(p.groups))
	for group, g := range p.groups {
		avg := 0.0
		if g.intervalDurationCount > 0 {
			avg = float64(g.intervalDurationTotal.Milliseconds()) / float64(g.intervalDurationCount)
		}
		out = append(out, GroupSnapshot{
			Group:          group,
			RequestsTotal:  g.requestsTotal,
			Errors5xxTotal: g.errors5xxTotal,
			ActiveConns:    g.activeConns,
			AvgDurationMs:  avg,
		})
		g.intervalDurationTotal = 0
		g.intervalDurationCount = 0
	}
	return out
}

// ── in-memory series for the admin UI's own charts ──────────────────────
//
// Deliberately not Prometheus-backed: reading back a time range from a
// Prometheus CounterVec/HistogramVec isn't something client_golang
// supports (it's a write-only client library from this process's
// perspective) — actually querying historical data means either running
// a real Prometheus server or keeping a small independent buffer here.
// The latter is consistent with this project's "no external database"
// pattern used everywhere else (admin store, audit log, overrides,
// routes).

// seriesPoint is one bucketed sample of aggregate traffic across every
// group, for the admin dashboard's overview chart.
type seriesPoint struct {
	t             time.Time
	requests      int64
	errors5xx     int64
	durationTotal time.Duration // sum of request durations in this bucket, for computing an average
	durationCount int64
	activeConns   int64 // last-known value at the end of this bucket, not a sum
	tcpConns      int64
	tcpBytesIn    int64
	tcpBytesOut   int64
}

// maxSeriesPoints bounds memory use: at the default 5s bucket width (see
// seriesBucketWidth), this covers a bit over 20 minutes of history —
// enough for "what's happening right now and over the last while" without
// needing unbounded growth or a real time-series database.
const maxSeriesPoints = 250

// seriesBucketWidth is how much wall-clock time each seriesPoint covers.
const seriesBucketWidth = 5 * time.Second

// seriesStore is a small ring buffer of seriesPoint, plus current gauges,
// safe for concurrent use. Not per-group — the admin dashboard's overview
// chart is intentionally an aggregate across every group this instance
// serves, matching what operators actually want to glance at first.
type seriesStore struct {
	mu            sync.Mutex
	points        []seriesPoint
	currentActive int64
}

func newSeriesStore() *seriesStore {
	return &seriesStore{points: make([]seriesPoint, 0, maxSeriesPoints)}
}

// currentBucketLocked returns the bucket for "now", creating one if the
// most recent bucket has aged out. Callers must hold s.mu.
func (s *seriesStore) currentBucketLocked() *seriesPoint {
	now := time.Now()
	if len(s.points) > 0 {
		last := &s.points[len(s.points)-1]
		if now.Sub(last.t) < seriesBucketWidth {
			return last
		}
	}

	s.points = append(s.points, seriesPoint{t: now, activeConns: s.currentActive, tcpConns: 0})
	if len(s.points) > maxSeriesPoints {
		// Drop the oldest point — a plain slice re-slice here is fine at
		// this bound (250 elements, one append-and-trim every 5s at
		// most); no need for a more clever circular buffer.
		s.points = s.points[1:]
	}
	return &s.points[len(s.points)-1]
}

func (s *seriesStore) recordRequest(_ string, class string, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.currentBucketLocked()
	b.requests++
	b.durationTotal += d
	b.durationCount++
	if class == "5xx" {
		b.errors5xx++
	}
}

func (s *seriesStore) recordActiveConns(_ string, delta int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.currentActive += int64(delta)
	if s.currentActive < 0 {
		s.currentActive = 0 // defensive: a decrement should never outpace increments, but never show a negative gauge
	}
	b := s.currentBucketLocked()
	b.activeConns = s.currentActive
}

func (s *seriesStore) recordTCPConnection(_ string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.currentBucketLocked()
	b.tcpConns++
}

func (s *seriesStore) recordTCPBytes(_ string, direction string, n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.currentBucketLocked()
	if direction == "out" {
		b.tcpBytesOut += n
	} else {
		b.tcpBytesIn += n
	}
}

// SeriesPoint is the read-only, display-ready form of a bucketed sample,
// returned by Recent — AvgDurationMs is precomputed here so template code
// doesn't need to do duration math.
type SeriesPoint struct {
	Time          time.Time
	Requests      int64
	Errors5xx     int64
	AvgDurationMs float64
	ActiveConns   int64
	TCPConns      int64
	TCPBytesIn    int64
	TCPBytesOut   int64
}

// Recent returns up to the last n buckets, oldest first, ready for
// display/charting.
func (s *seriesStore) Recent(n int) []SeriesPoint {
	s.mu.Lock()
	defer s.mu.Unlock()

	start := 0
	if len(s.points) > n {
		start = len(s.points) - n
	}
	src := s.points[start:]

	out := make([]SeriesPoint, len(src))
	for i, p := range src {
		avg := 0.0
		if p.durationCount > 0 {
			avg = float64(p.durationTotal.Milliseconds()) / float64(p.durationCount)
		}
		out[i] = SeriesPoint{
			Time:          p.t,
			Requests:      p.requests,
			Errors5xx:     p.errors5xx,
			AvgDurationMs: avg,
			ActiveConns:   p.activeConns,
			TCPConns:      p.tcpConns,
			TCPBytesIn:    p.tcpBytesIn,
			TCPBytesOut:   p.tcpBytesOut,
		}
	}
	return out
}
