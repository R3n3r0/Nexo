package clitest

import (
	"context"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/stretchr/testify/mock"

	"github.com/r3n3r0/nexo/internal/cli"
	climock "github.com/r3n3r0/nexo/internal/cli/mock"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// memObsState is the observability of MemAdmin: the requests and the log
// entries waiting for the live streams (a queue: an item is read by one
// stream), the audit records and the series of metrics.
type memObsState struct {
	traces []nexoadmin.TraceEvent
	logs   []nexoadmin.LogEntry
	// wake is closed (and replaced) when an item is published.
	wake chan struct{}
	// endStreams is the number of the next streams that end (io.EOF) once
	// the queue is empty, as when the server restarts.
	endStreams int
	// dropped is reported by the streams opened afterwards.
	dropped uint64
	// opened counts the streams opened.
	opened int

	traceOpts    []nexoadmin.TraceOptions
	logOpts      []nexoadmin.LogOptions
	auditQueries []nexoadmin.AuditQuery
	audit        []nexoadmin.AuditEntry
	metrics      []nexoadmin.MetricInfo
	series       map[string]nexoadmin.MetricSeries
	metricQuery  []nexoadmin.MetricQuery
}

// wakeLocked returns the channel closed by the next publication.
func (o *memObsState) wakeLocked() chan struct{} {
	if o.wake == nil {
		o.wake = make(chan struct{})
	}
	return o.wake
}

// notifyLocked wakes the streams waiting for an item.
func (o *memObsState) notifyLocked() {
	if o.wake != nil {
		close(o.wake)
		o.wake = nil
	}
}

// PublishTrace queues requests for the live trace streams.
func (m *MemAdmin) PublishTrace(events ...nexoadmin.TraceEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.obs.traces = append(m.obs.traces, events...)
	m.obs.notifyLocked()
}

// PublishLogs queues entries for the live log streams.
func (m *MemAdmin) PublishLogs(entries ...nexoadmin.LogEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.obs.logs = append(m.obs.logs, entries...)
	m.obs.notifyLocked()
}

// EndStreams makes the next n live streams end (io.EOF) once they have
// delivered the queued items, as when the server restarts.
func (m *MemAdmin) EndStreams(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.obs.endStreams = n
}

// SetDropped sets the items the streams report as dropped.
func (m *MemAdmin) SetDropped(n uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.obs.dropped = n
}

// StreamsOpened returns how many live streams were opened.
func (m *MemAdmin) StreamsOpened() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.obs.opened
}

// TraceOptions returns the options of the trace streams opened so far.
func (m *MemAdmin) TraceOptions() []nexoadmin.TraceOptions {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.obs.traceOpts)
}

// LogOptions returns the options of the log streams opened so far.
func (m *MemAdmin) LogOptions() []nexoadmin.LogOptions {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.obs.logOpts)
}

// AddAudit adds audit records (searched newest first).
func (m *MemAdmin) AddAudit(entries ...nexoadmin.AuditEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.obs.audit = append(m.obs.audit, entries...)
}

// AuditQueries returns the audit searches made so far.
func (m *MemAdmin) AuditQueries() []nexoadmin.AuditQuery {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.obs.auditQueries)
}

// SetMetricSeries sets a series of metrics (listed in the order they are
// set) and the points QueryMetric returns for it.
func (m *MemAdmin) SetMetricSeries(s nexoadmin.MetricSeries) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.obs.series == nil {
		m.obs.series = map[string]nexoadmin.MetricSeries{}
	}
	if _, ok := m.obs.series[s.Metric.Name]; !ok {
		m.obs.metrics = append(m.obs.metrics, s.Metric)
	}
	m.obs.series[s.Metric.Name] = s
}

// MetricQueries returns the metric queries made so far.
func (m *MemAdmin) MetricQueries() []nexoadmin.MetricQuery {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.obs.metricQuery)
}

// registerObservability configures the observability methods of a client.
func (s *memSession) registerObservability(t TestingT, c *climock.AdminClient) {
	x := mock.Anything
	c.EXPECT().Trace(x, x).RunAndReturn(func(ctx context.Context, opts nexoadmin.TraceOptions) (cli.EventStream[nexoadmin.TraceEvent], error) {
		return s.trace(ctx, t, opts)
	}).Maybe()
	c.EXPECT().Logs(x, x).RunAndReturn(func(ctx context.Context, opts nexoadmin.LogOptions) (cli.EventStream[nexoadmin.LogEntry], error) {
		return s.logs(ctx, t, opts)
	}).Maybe()
	c.EXPECT().SearchAudit(x, x).RunAndReturn(s.searchAudit).Maybe()
	c.EXPECT().ListMetrics(x).RunAndReturn(s.listMetrics).Maybe()
	c.EXPECT().QueryMetric(x, x).RunAndReturn(s.queryMetric).Maybe()
}

// allowedObservabilityLocked reports whether the caller may call an observability
// endpoint: an administrator or a user of the diagnostics policy (which
// allows admin:ServerTrace, admin:ConsoleLog and admin:Prometheus).
func (s *memSession) allowedObservabilityLocked() bool {
	return s.isAdminLocked() || slices.Contains(s.effectiveLocked(s.caller), "diagnostics")
}

// denied is the error of an observability call without permission.
func denied(action string) error {
	return apiError(http.StatusForbidden, nexoadmin.CodeAccessDenied, action+": no policy allows "+action)
}

// memStream is the state of a live stream of MemAdmin: it reads the queue
// of take until ctx is done or the stream closed.
type memStream[T any] struct {
	m       *MemAdmin
	ctx     context.Context
	cancel  context.CancelFunc
	end     bool
	dropped uint64
	// take pops the next queued item matching the filter (locked).
	take func() (T, bool)
}

// next waits for the next item.
func (st *memStream[T]) next() (*T, error) {
	for {
		st.m.mu.Lock()
		if item, ok := st.take(); ok {
			st.m.mu.Unlock()
			return &item, nil
		}
		if st.end {
			st.m.mu.Unlock()
			return nil, io.EOF
		}
		wake := st.m.obs.wakeLocked()
		st.m.mu.Unlock()
		select {
		case <-st.ctx.Done():
			return nil, st.ctx.Err()
		case <-wake:
		}
	}
}

// openStream returns a mock EventStream reading the queue of take.
func openStream[T any](ctx context.Context, t TestingT, m *MemAdmin, take func() (T, bool)) cli.EventStream[T] {
	ctx, cancel := context.WithCancel(ctx)
	st := &memStream[T]{m: m, ctx: ctx, cancel: cancel, take: take}
	// Called with the lock held by the caller.
	m.obs.opened++
	st.dropped = m.obs.dropped
	if m.obs.endStreams > 0 {
		m.obs.endStreams--
		st.end = true
	}
	stream := climock.NewEventStream[T](t)
	stream.EXPECT().Next().RunAndReturn(st.next).Maybe()
	stream.EXPECT().Dropped().RunAndReturn(func() uint64 { return st.dropped }).Maybe()
	var once sync.Once
	stream.EXPECT().Close().RunAndReturn(func() error {
		once.Do(cancel)
		return nil
	}).Maybe()
	return stream
}

// trace opens a live trace stream filtered like the server does.
func (s *memSession) trace(ctx context.Context, t TestingT, opts nexoadmin.TraceOptions) (cli.EventStream[nexoadmin.TraceEvent], error) {
	if err := s.begin("Trace"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	if !s.allowedObservabilityLocked() {
		return nil, denied("admin:ServerTrace")
	}
	for _, st := range opts.Statuses {
		if _, _, ok := parseStatus(st); !ok {
			return nil, invalid("status %q is neither a status code nor a class (4xx)", st)
		}
	}
	s.m.obs.traceOpts = append(s.m.obs.traceOpts, opts)
	return openStream(ctx, t, s.m, func() (nexoadmin.TraceEvent, bool) {
		for len(s.m.obs.traces) > 0 {
			ev := s.m.obs.traces[0]
			s.m.obs.traces = s.m.obs.traces[1:]
			if traceMatches(opts, ev) {
				return ev, true
			}
		}
		return nexoadmin.TraceEvent{}, false
	}), nil
}

// traceMatches applies the filter of the live trace.
func traceMatches(o nexoadmin.TraceOptions, ev nexoadmin.TraceEvent) bool {
	if len(o.Types) == 0 && ev.Type == "rpc" {
		return false
	}
	if len(o.Types) > 0 && !slices.Contains(o.Types, ev.Type) {
		return false
	}
	if len(o.Nodes) > 0 && !slices.Contains(o.Nodes, ev.Node) {
		return false
	}
	if len(o.APIs) > 0 && !slices.Contains(o.APIs, ev.API) {
		return false
	}
	if len(o.Buckets) > 0 && !slices.Contains(o.Buckets, ev.Bucket) {
		return false
	}
	if o.ErrorsOnly && ev.Status < 400 {
		return false
	}
	if o.Threshold > 0 && time.Duration(ev.DurationMs*float64(time.Millisecond)) < o.Threshold {
		return false
	}
	return statusMatches(o.Statuses, ev.Status)
}

// parseStatus parses a status code (404) or class (4xx).
func parseStatus(v string) (code, class int, ok bool) {
	if c, isClass := strings.CutSuffix(strings.ToLower(v), "xx"); isClass && len(c) == 1 && c[0] >= '1' && c[0] <= '5' {
		return 0, int(c[0] - '0'), true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 100 || n > 599 {
		return 0, 0, false
	}
	return n, 0, true
}

// statusMatches reports whether status is one of the codes or classes
// (true without any).
func statusMatches(filter []string, status int) bool {
	if len(filter) == 0 {
		return true
	}
	for _, v := range filter {
		code, class, ok := parseStatus(v)
		if ok && (code == status || (class > 0 && status/100 == class)) {
			return true
		}
	}
	return false
}

// logRank orders the log levels.
func logRank(level string) int {
	switch strings.ToLower(level) {
	case "debug":
		return 0
	case "warn", "warning":
		return 2
	case "error":
		return 3
	default:
		return 1
	}
}

// logs opens a live log stream.
func (s *memSession) logs(ctx context.Context, t TestingT, opts nexoadmin.LogOptions) (cli.EventStream[nexoadmin.LogEntry], error) {
	if err := s.begin("Logs"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	if !s.allowedObservabilityLocked() {
		return nil, denied("admin:ConsoleLog")
	}
	if opts.Last < 0 || opts.Last > 10000 {
		return nil, invalid("last must be between 0 and 10000")
	}
	s.m.obs.logOpts = append(s.m.obs.logOpts, opts)
	return openStream(ctx, t, s.m, func() (nexoadmin.LogEntry, bool) {
		for len(s.m.obs.logs) > 0 {
			e := s.m.obs.logs[0]
			s.m.obs.logs = s.m.obs.logs[1:]
			if (opts.Level == "" || logRank(e.Level) >= logRank(opts.Level)) && (len(opts.Nodes) == 0 || slices.Contains(opts.Nodes, e.Node)) {
				return e, true
			}
		}
		return nexoadmin.LogEntry{}, false
	}), nil
}

// searchAudit searches the audit records, newest first.
func (s *memSession) searchAudit(_ context.Context, q nexoadmin.AuditQuery) (*nexoadmin.AuditList, error) {
	if err := s.begin("SearchAudit"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	if !s.allowedObservabilityLocked() {
		return nil, denied("admin:ConsoleLog")
	}
	for _, st := range q.Statuses {
		if _, _, ok := parseStatus(st); !ok {
			return nil, invalid("status %q is neither a status code nor a class (4xx)", st)
		}
	}
	s.m.obs.auditQueries = append(s.m.obs.auditQueries, q)
	limit := q.Limit
	if limit == 0 {
		limit = 100
	}
	out := &nexoadmin.AuditList{Items: []nexoadmin.AuditEntry{}}
	for i := len(s.m.obs.audit) - 1; i >= 0 && len(out.Items) < limit; i-- {
		a := s.m.obs.audit[i]
		switch {
		case q.User != "" && a.AccessKey != q.User && a.ParentUser != q.User,
			q.API != "" && a.API != q.API,
			q.Bucket != "" && a.Bucket != q.Bucket,
			q.Type != "" && a.Type != q.Type,
			q.RequestID != "" && a.RequestID != q.RequestID,
			len(q.Nodes) > 0 && !slices.Contains(q.Nodes, a.Node),
			q.ErrorsOnly && a.StatusCode < 400,
			!q.Since.IsZero() && a.Time.Before(q.Since),
			!q.Until.IsZero() && a.Time.After(q.Until),
			!statusMatches(q.Statuses, a.StatusCode):
			continue
		}
		out.Items = append(out.Items, a)
	}
	return out, nil
}

// listMetrics lists the series of metrics.
func (s *memSession) listMetrics(context.Context) (*nexoadmin.MetricList, error) {
	if err := s.begin("ListMetrics"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	if !s.allowedObservabilityLocked() {
		return nil, denied("admin:Prometheus")
	}
	return &nexoadmin.MetricList{Items: slices.Clone(s.m.obs.metrics)}, nil
}

// queryMetric returns the points of a series.
func (s *memSession) queryMetric(_ context.Context, q nexoadmin.MetricQuery) (*nexoadmin.MetricSeries, error) {
	if err := s.begin("QueryMetric"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	if !s.allowedObservabilityLocked() {
		return nil, denied("admin:Prometheus")
	}
	s.m.obs.metricQuery = append(s.m.obs.metricQuery, q)
	if q.Range > 24*time.Hour {
		return nil, invalid("range must be at most 24h0m0s")
	}
	series, ok := s.m.obs.series[q.Metric]
	if !ok {
		return nil, notFound("not_found", "metric %q not found", q.Metric)
	}
	if len(q.Nodes) > 0 {
		nodes := series.Nodes[:0:0]
		for _, n := range series.Nodes {
			if slices.Contains(q.Nodes, n.Node) {
				nodes = append(nodes, n)
			}
		}
		series.Nodes = nodes
	}
	return &series, nil
}
