package metrics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
)

// HistoryDeps are the dependencies of the ring buffer.
type HistoryDeps struct {
	// Registry is sampled at every resolution (required).
	Registry Registry
	// NodeID names this node in the results.
	NodeID string
	// Peers are the other nodes of a cluster, queried for their points.
	Peers  []Peer
	Clock  clock.Clock
	Logger *slog.Logger
}

// HistoryOption configures the ring buffer.
type HistoryOption func(*historyOptions)

type historyOptions struct {
	resolution  time.Duration
	retention   time.Duration
	peerTimeout time.Duration
	maxPoints   int
}

// WithResolution sets the distance between two samples (default 10s,
// at least one second).
func WithResolution(d time.Duration) HistoryOption {
	return func(o *historyOptions) {
		if d >= time.Second {
			o.resolution = d.Truncate(time.Second)
		}
	}
}

// WithRetention sets how far back the buffer goes (default 24h).
func WithRetention(d time.Duration) HistoryOption {
	return func(o *historyOptions) {
		if d > 0 {
			o.retention = d
		}
	}
}

// WithHistoryPeerTimeout bounds the query of the points of a peer
// (default 5s).
func WithHistoryPeerTimeout(d time.Duration) HistoryOption {
	return func(o *historyOptions) {
		if d > 0 {
			o.peerTimeout = d
		}
	}
}

// sampleKind tells how a series is computed from the families.
type sampleKind int

const (
	// sampleGauge sums the values of every series of the family.
	sampleGauge sampleKind = iota
	// sampleZeros counts the series of the family whose value is 0.
	sampleZeros
	// sampleRate is the per-second increase of the sum of a counter
	// family since the previous sample.
	sampleRate
	// sampleAvg is the average of the observations of a histogram family
	// since the previous sample.
	sampleAvg
	// sampleQuantile is a quantile of the observations of a histogram
	// family since the previous sample.
	sampleQuantile
)

// seriesDef describes how a series of the buffer is sampled.
type seriesDef struct {
	info     SeriesInfo
	family   string
	kind     sampleKind
	quantile float64
}

// Aggregations of the cluster series.
const (
	aggSum = "sum"
	aggAvg = "avg"
	aggMax = "max"
)

// defaultSeries are the key series of the console charts (D-MON-01).
func defaultSeries() []seriesDef {
	return []seriesDef{
		{SeriesInfo{"s3_requests", "S3 requests per second", "req/s", aggSum}, "nexo_s3_requests_total", sampleRate, 0},
		{SeriesInfo{"s3_errors_4xx", "S3 requests answered with a 4xx status per second", "req/s", aggSum}, "nexo_s3_requests_4xx_errors_total", sampleRate, 0},
		{SeriesInfo{"s3_errors_5xx", "S3 requests answered with a 5xx status per second", "req/s", aggSum}, "nexo_s3_requests_5xx_errors_total", sampleRate, 0},
		{SeriesInfo{"s3_latency_avg", "Average latency of the S3 requests", "seconds", aggAvg}, "nexo_s3_request_duration_seconds", sampleAvg, 0},
		{SeriesInfo{"s3_latency_p50", "Median latency of the S3 requests", "seconds", aggMax}, "nexo_s3_request_duration_seconds", sampleQuantile, 0.5},
		{SeriesInfo{"s3_latency_p95", "95th percentile of the latency of the S3 requests", "seconds", aggMax}, "nexo_s3_request_duration_seconds", sampleQuantile, 0.95},
		{SeriesInfo{"s3_latency_p99", "99th percentile of the latency of the S3 requests", "seconds", aggMax}, "nexo_s3_request_duration_seconds", sampleQuantile, 0.99},
		{SeriesInfo{"s3_rx_bytes", "Bytes received by the S3 API per second", "bytes/s", aggSum}, "nexo_s3_traffic_received_bytes_total", sampleRate, 0},
		{SeriesInfo{"s3_tx_bytes", "Bytes sent by the S3 API per second", "bytes/s", aggSum}, "nexo_s3_traffic_sent_bytes_total", sampleRate, 0},
		{SeriesInfo{"s3_inflight", "S3 requests being served", "requests", aggSum}, "nexo_s3_requests_inflight", sampleGauge, 0},
		{SeriesInfo{"http_requests", "HTTP requests of every listener per second", "req/s", aggSum}, "nexo_http_requests_total", sampleRate, 0},
		{SeriesInfo{"drive_used_bytes", "Used space of the local drives", "bytes", aggSum}, "nexo_drive_used_bytes", sampleGauge, 0},
		{SeriesInfo{"drive_free_bytes", "Free space of the local drives", "bytes", aggSum}, "nexo_drive_free_bytes", sampleGauge, 0},
		{SeriesInfo{"drive_total_bytes", "Capacity of the local drives", "bytes", aggSum}, "nexo_drive_total_bytes", sampleGauge, 0},
		{SeriesInfo{"drives_online", "Local drives online", "drives", aggSum}, "nexo_drive_online", sampleGauge, 0},
		{SeriesInfo{"drives_offline", "Local drives offline", "drives", aggSum}, "nexo_drive_online", sampleZeros, 0},
		{SeriesInfo{"drive_errors", "Errors of the local drives per second", "errors/s", aggSum}, "nexo_drive_errors_total", sampleRate, 0},
		{SeriesInfo{"mrf_pending", "Objects waiting in the healing queue", "objects", aggSum}, "nexo_mrf_queue_pending", sampleGauge, 0},
		{SeriesInfo{"mrf_healed", "Objects of the healing queue healed per second", "objects/s", aggSum}, "nexo_mrf_queue_healed_total", sampleRate, 0},
		{SeriesInfo{"heal_scanned", "Items examined by the running healing jobs", "objects", aggSum}, "nexo_heal_running_objects_scanned", sampleGauge, 0},
		{SeriesInfo{"scanner_scanning", "Whether a scanner cycle is in progress", "boolean", aggMax}, "nexo_scanner_scanning", sampleGauge, 0},
		{SeriesInfo{"scanner_objects", "Objects scanned by the last scanner cycle", "objects", aggMax}, "nexo_scanner_last_cycle_objects", sampleGauge, 0},
		{SeriesInfo{"events_pending", "Bucket notifications waiting for their targets", "events", aggSum}, "nexo_events_pending", sampleGauge, 0},
		{SeriesInfo{"rpc_requests", "Internode calls per second", "req/s", aggSum}, "nexo_rpc_requests_total", sampleRate, 0},
		{SeriesInfo{"rpc_errors", "Failed internode calls per second", "errors/s", aggSum}, "nexo_rpc_errors_total", sampleRate, 0},
		{SeriesInfo{"cpu_usage", "CPU used by the server process", "cores", aggSum}, "process_cpu_seconds_total", sampleRate, 0},
		{SeriesInfo{"memory_bytes", "Resident memory of the server process", "bytes", aggSum}, "process_resident_memory_bytes", sampleGauge, 0},
		{SeriesInfo{"goroutines", "Goroutines of the server process", "goroutines", aggSum}, "go_goroutines", sampleGauge, 0},
	}
}

// ring is the circular buffer of one series: the samples in the order
// they were taken, each with its sample number (unix time / resolution).
// It grows up to its size, then overwrites the oldest sample: a node keeps
// memory for the samples it took only.
type ring struct {
	stamps []int64
	values []float64
	next   int
	size   int
}

func newRing(n int) *ring { return &ring{size: n} }

// add records a sample.
func (r *ring) add(stamp int64, v float64) {
	if len(r.stamps) < r.size {
		r.stamps = append(r.stamps, stamp)
		r.values = append(r.values, v)
		return
	}
	r.stamps[r.next], r.values[r.next] = stamp, v
	r.next = (r.next + 1) % r.size
}

// previous are the totals of the last sample, for the rates.
type previous struct {
	at      time.Time
	total   float64
	count   uint64
	sum     float64
	buckets []Bucket
}

type history struct {
	deps   HistoryDeps
	opts   historyOptions
	series []seriesDef

	mu    sync.Mutex
	rings map[string]*ring
	prev  map[string]previous

	runMu  sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

var _ History = (*history)(nil)

// NewHistory returns the ring buffer of the key series of the node. A
// missing registry is a wiring error (domain.ErrInvalidArgument).
func NewHistory(deps HistoryDeps, opts ...HistoryOption) (History, error) {
	if deps.Registry == nil {
		return nil, fmt.Errorf("%w: metrics: the history needs the registry", domain.ErrInvalidArgument)
	}
	if deps.NodeID == "" {
		deps.NodeID = "local"
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	o := historyOptions{resolution: 10 * time.Second, retention: 24 * time.Hour, peerTimeout: 5 * time.Second, maxPoints: 360}
	for _, opt := range opts {
		opt(&o)
	}
	if o.retention < o.resolution {
		o.retention = o.resolution
	}
	h := &history{deps: deps, opts: o, series: defaultSeries(), rings: map[string]*ring{}, prev: map[string]previous{}}
	n := int(o.retention / o.resolution)
	for _, s := range h.series {
		h.rings[s.info.Name] = newRing(n)
	}
	return h, nil
}

// Start implements lifecycle.Runner: a sample every resolution.
func (h *history) Start(ctx context.Context) error {
	h.runMu.Lock()
	defer h.runMu.Unlock()
	if h.cancel != nil {
		return nil
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	h.cancel, h.done = cancel, make(chan struct{})
	go h.run(runCtx, h.done)
	return nil
}

// Stop implements lifecycle.Runner.
func (h *history) Stop(ctx context.Context) error {
	h.runMu.Lock()
	cancel, done := h.cancel, h.done
	h.cancel, h.done = nil, nil
	h.runMu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// run samples every resolution of wall time (the samples are stamped
// with the clock of the service).
func (h *history) run(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(h.opts.resolution)
	defer ticker.Stop()
	for {
		if err := h.Sample(ctx); err != nil && ctx.Err() == nil {
			h.deps.Logger.DebugContext(ctx, "metrics: history sample failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Metrics implements History.
func (h *history) Metrics(context.Context) []SeriesInfo {
	out := make([]SeriesInfo, 0, len(h.series))
	for _, s := range h.series {
		out = append(out, s.info)
	}
	return out
}

// totals sums the families met in a snapshot.
type totals struct {
	found   bool
	total   float64
	zeros   int
	count   uint64
	sum     float64
	buckets []Bucket
}

func totalOf(f Family) totals {
	t := totals{found: true}
	for _, s := range f.Samples {
		t.total += s.Value
		if s.Value == 0 {
			t.zeros++
		}
		t.count += s.Count
		t.sum += s.Sum
		for _, b := range s.Buckets {
			i := slices.IndexFunc(t.buckets, func(x Bucket) bool { return x.UpperBound == b.UpperBound })
			if i < 0 {
				t.buckets = append(t.buckets, b)
				continue
			}
			t.buckets[i].Count += b.Count
		}
	}
	sort.Slice(t.buckets, func(i, j int) bool { return t.buckets[i].UpperBound < t.buckets[j].UpperBound })
	return t
}

// Sample implements History.
func (h *history) Sample(ctx context.Context) error {
	fams, err := h.deps.Registry.Snapshot(ctx)
	if err != nil {
		return err
	}
	now := h.deps.Clock.Now()
	byName := make(map[string]totals, len(fams))
	for _, f := range fams {
		byName[f.Name] = totalOf(f)
	}
	slot := now.Unix() / int64(h.opts.resolution/time.Second)
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.series {
		v, ok := h.valueOf(s, byName[s.family], now)
		if ok {
			h.rings[s.info.Name].add(slot, v)
		}
	}
	return nil
}

// valueOf computes the value of a series from the totals of its family
// (h.mu held); ok is false when there is no value (family absent, first
// sample of a rate, no observation since the previous sample).
func (h *history) valueOf(s seriesDef, t totals, now time.Time) (float64, bool) {
	switch s.kind {
	case sampleGauge:
		return t.total, t.found
	case sampleZeros:
		return float64(t.zeros), t.found
	}
	// A counter or histogram family appears with its first series: until
	// then its totals are zero (they start at zero with the process), so
	// that the first increase is a rate too.
	prev, had := h.prev[s.info.Name]
	h.prev[s.info.Name] = previous{at: now, total: t.total, count: t.count, sum: t.sum, buckets: t.buckets}
	if !had {
		return 0, false
	}
	dt := now.Sub(prev.at).Seconds()
	if dt <= 0 {
		return 0, false
	}
	switch s.kind {
	case sampleRate:
		delta := t.total - prev.total
		if delta < 0 { // counter reset (restart)
			delta = t.total
		}
		return delta / dt, true
	case sampleAvg:
		if t.count <= prev.count {
			return 0, false
		}
		return (t.sum - prev.sum) / float64(t.count-prev.count), true
	default:
		if t.count <= prev.count {
			return 0, false
		}
		return quantileOf(s.quantile, t.buckets, prev.buckets, t.count-prev.count), true
	}
}

// quantileOf estimates the quantile q of the observations between two
// cumulative bucket sets (linear interpolation inside the bucket, the
// bound of the last finite bucket beyond it).
func quantileOf(q float64, cur, prev []Bucket, total uint64) float64 {
	rank := q * float64(total)
	var lower float64
	var lowerCount uint64
	for _, b := range cur {
		c := b.Count
		if i := slices.IndexFunc(prev, func(x Bucket) bool { return x.UpperBound == b.UpperBound }); i >= 0 && prev[i].Count <= c {
			c -= prev[i].Count
		}
		if float64(c) >= rank {
			if math.IsInf(b.UpperBound, 1) {
				return lower
			}
			if c == lowerCount {
				return b.UpperBound
			}
			return lower + (b.UpperBound-lower)*(rank-float64(lowerCount))/float64(c-lowerCount)
		}
		lower, lowerCount = b.UpperBound, c
	}
	return lower
}

// Query implements History.
func (h *history) Query(ctx context.Context, q HistoryQuery) (*HistoryResult, error) {
	def, ok := h.def(q.Metric)
	if !ok {
		return nil, fmt.Errorf("%w: unknown metric %q", domain.ErrNotFound, q.Metric)
	}
	q, err := h.normalize(q)
	if err != nil {
		return nil, err
	}
	res := &HistoryResult{Metric: def.info, Start: q.Start, End: q.End, Step: q.Step}
	if wanted(q.Nodes, h.deps.NodeID) {
		res.Nodes = append(res.Nodes, NodeSeries{Node: h.deps.NodeID, Values: h.points(def.info.Name, q)})
	}
	if !q.Local {
		res.Nodes = append(res.Nodes, h.peerSeries(ctx, q)...)
	}
	res.Cluster = aggregate(def.info.Aggregation, res.Nodes, pointCount(q))
	return res, nil
}

func (h *history) def(name string) (seriesDef, bool) {
	for _, s := range h.series {
		if s.info.Name == name {
			return s, true
		}
	}
	return seriesDef{}, false
}

// normalize applies the defaults and bounds of a query. The query of a
// peer (Local) is already normalized by the node that asks: its bounds are
// kept as they are, so that the clocks of the nodes, never exactly equal,
// do not change the number of points.
func (h *history) normalize(q HistoryQuery) (HistoryQuery, error) {
	now := h.deps.Clock.Now()
	if q.Local && !q.Start.IsZero() && q.Start.Before(q.End) && q.Step >= h.opts.resolution && q.Step%h.opts.resolution == 0 {
		if q.End.Sub(q.Start)/q.Step > time.Duration(10*h.opts.maxPoints) {
			return q, fmt.Errorf("%w: too many points: use a larger step", domain.ErrInvalidArgument)
		}
		return q, nil
	}
	if q.End.IsZero() || q.End.After(now) {
		q.End = now
	}
	if q.Start.IsZero() {
		span := q.Range
		if span <= 0 {
			span = time.Hour
		}
		q.Start = q.End.Add(-span)
	}
	if !q.Start.Before(q.End) {
		return q, fmt.Errorf("%w: the start of the range must precede its end", domain.ErrInvalidArgument)
	}
	if oldest := now.Add(-h.opts.retention); q.Start.Before(oldest) {
		q.Start = oldest
		if !q.Start.Before(q.End) {
			return q, fmt.Errorf("%w: the range is older than the retention (%s)", domain.ErrInvalidArgument, h.opts.retention)
		}
	}
	res := h.opts.resolution
	span := q.End.Sub(q.Start)
	if q.Step <= 0 {
		q.Step = res
		if n := int(span / q.Step); n > h.opts.maxPoints {
			q.Step = span / time.Duration(h.opts.maxPoints)
		}
	}
	if q.Step < res {
		q.Step = res
	}
	// A multiple of the resolution, aligned on it.
	q.Step = ((q.Step + res - 1) / res) * res
	if span/q.Step > time.Duration(10*h.opts.maxPoints) {
		return q, fmt.Errorf("%w: too many points: use a larger step", domain.ErrInvalidArgument)
	}
	q.Start = q.Start.Truncate(q.Step)
	return q, nil
}

// pointCount is the number of points of a normalized query.
func pointCount(q HistoryQuery) int {
	return int(q.End.Sub(q.Start)/q.Step) + 1
}

// points returns the points of a series of this node: the average of the
// samples of every step.
func (h *history) points(name string, q HistoryQuery) []float64 {
	n := pointCount(q)
	out := make([]float64, n)
	sums := make([]float64, n)
	counts := make([]int, n)
	res := int64(h.opts.resolution / time.Second)
	step := int64(q.Step / time.Second)
	start := q.Start.Unix()
	h.mu.Lock()
	r := h.rings[name]
	for i, stamp := range r.stamps {
		at := stamp * res
		if at < start || at > q.End.Unix() {
			continue
		}
		k := int((at - start) / step)
		if k >= n || math.IsNaN(r.values[i]) {
			continue
		}
		sums[k] += r.values[i]
		counts[k]++
	}
	h.mu.Unlock()
	for k := range out {
		if counts[k] == 0 {
			out[k] = math.NaN()
			continue
		}
		out[k] = sums[k] / float64(counts[k])
	}
	return out
}

// peerSeries queries the peers selected by q.Nodes.
func (h *history) peerSeries(ctx context.Context, q HistoryQuery) []NodeSeries {
	local := q
	local.Local, local.Nodes = true, nil
	out := make([]NodeSeries, 0, len(h.deps.Peers))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, p := range h.deps.Peers {
		if !wanted(q.Nodes, p.NodeID()) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			callCtx, cancel := context.WithTimeout(ctx, h.opts.peerTimeout)
			defer cancel()
			ns, err := p.History(callCtx, local)
			if err == nil && len(ns.Values) != pointCount(q) {
				err = errors.New("unexpected number of points")
			}
			if err != nil {
				ns = NodeSeries{Node: p.NodeID(), Error: err.Error()}
			}
			ns.Node = p.NodeID()
			mu.Lock()
			out = append(out, ns)
			mu.Unlock()
		}()
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].Node < out[j].Node })
	return out
}

// wanted reports whether node is selected by nodes (empty: every node).
func wanted(nodes []string, node string) bool {
	return len(nodes) == 0 || slices.Contains(nodes, node)
}

// aggregate combines the points of the nodes.
func aggregate(how string, nodes []NodeSeries, n int) []float64 {
	out := make([]float64, n)
	for k := range out {
		var acc float64
		count := 0
		for _, ns := range nodes {
			if k >= len(ns.Values) || math.IsNaN(ns.Values[k]) {
				continue
			}
			v := ns.Values[k]
			switch {
			case count == 0:
				acc = v
			case how == aggMax:
				acc = max(acc, v)
			default:
				acc += v
			}
			count++
		}
		switch {
		case count == 0:
			out[k] = math.NaN()
		case how == aggAvg:
			out[k] = acc / float64(count)
		default:
			out[k] = acc
		}
	}
	return out
}
