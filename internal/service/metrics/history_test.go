package metrics_test

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/metrics"
	metricsmock "github.com/r3n3r0/nexo/internal/service/metrics/mock"
)

// historyFixture is a registry with the families of the default series
// and a history on a fake clock.
type historyFixture struct {
	ctx      context.Context
	clk      *clock.Fake
	reg      metrics.Registry
	requests metrics.Counter
	latency  metrics.Histogram
	online   metrics.Gauge
	hist     metrics.History
}

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func newHistoryFixture(t *testing.T, peers []metrics.Peer, opts ...metrics.HistoryOption) *historyFixture {
	t.Helper()
	ctx := context.Background()
	f := &historyFixture{ctx: ctx, clk: clock.NewFake(t0), reg: metrics.New(metrics.Deps{}, metrics.WithoutRuntimeCollectors())}
	var err error
	f.requests, err = f.reg.Counter(ctx, metrics.Definition{Subsystem: "s3", Name: "requests_total", Help: "R.", Labels: []string{"api"}})
	require.NoError(t, err)
	f.latency, err = f.reg.Histogram(ctx, metrics.Definition{Subsystem: "s3", Name: "request_duration_seconds", Help: "L.", Labels: []string{"api"},
		Buckets: []float64{0.1, 0.5, 1}})
	require.NoError(t, err)
	f.online, err = f.reg.Gauge(ctx, metrics.Definition{Subsystem: "drive", Name: "online", Help: "O.", Labels: []string{"drive"}})
	require.NoError(t, err)
	f.hist, err = metrics.NewHistory(metrics.HistoryDeps{Registry: f.reg, NodeID: "n1", Peers: peers, Clock: f.clk}, opts...)
	require.NoError(t, err)
	return f
}

// tick advances the clock by d and samples.
func (f *historyFixture) tick(t *testing.T, d time.Duration) {
	t.Helper()
	f.clk.Advance(d)
	require.NoError(t, f.hist.Sample(f.ctx))
}

func (f *historyFixture) query(t *testing.T, q metrics.HistoryQuery) *metrics.HistoryResult {
	t.Helper()
	res, err := f.hist.Query(f.ctx, q)
	require.NoError(t, err)
	return res
}

func last(values []float64) float64 { return values[len(values)-1] }

func TestHistoryRatesAndGauges(t *testing.T) {
	f := newHistoryFixture(t, nil)
	f.online.Set(f.ctx, 1, "/d1")
	f.online.Set(f.ctx, 0, "/d2")
	f.online.Set(f.ctx, 1, "/d3")
	require.NoError(t, f.hist.Sample(f.ctx))

	f.requests.Add(f.ctx, 50, "PutObject")
	for i := 0; i < 98; i++ {
		f.latency.Observe(f.ctx, 0.05, "PutObject")
	}
	f.latency.Observe(f.ctx, 0.7, "PutObject")
	f.latency.Observe(f.ctx, 0.7, "GetObject")
	f.tick(t, 10*time.Second)

	q := metrics.HistoryQuery{Range: time.Minute, Step: 10 * time.Second}
	for name, want := range map[string]float64{
		"s3_requests":    5,
		"s3_latency_avg": (98*0.05 + 2*0.7) / 100,
		"s3_latency_p50": 0.1 * 50 / 98,
		"s3_latency_p95": 0.1 * 95 / 98,
		"s3_latency_p99": 0.75,
		"drives_online":  2,
		"drives_offline": 1,
	} {
		q.Metric = name
		res := f.query(t, q)
		require.Len(t, res.Nodes, 1, name)
		assert.Equal(t, "n1", res.Nodes[0].Node)
		assert.Equal(t, 7, len(res.Nodes[0].Values), "%s: a minute of 10s points, both ends included", name)
		assert.InDelta(t, want, last(res.Nodes[0].Values), 1e-9, name)
		assert.InDelta(t, want, last(res.Cluster), 1e-9, name)
		assert.True(t, math.IsNaN(res.Nodes[0].Values[0]), "%s: no sample a minute ago", name)
	}
	q.Metric = "s3_requests"
	res := f.query(t, q)
	assert.Equal(t, metrics.SeriesInfo{Name: "s3_requests", Description: "S3 requests per second", Unit: "req/s", Aggregation: "sum"}, res.Metric)
	assert.Equal(t, t0.Add(10*time.Second-time.Minute), res.Start)
	assert.Equal(t, 10*time.Second, res.Step)
	assert.True(t, math.IsNaN(res.Nodes[0].Values[5]), "the first sample of a counter has no rate")

	// No observation in a step: no latency point; a restart resets the
	// counters: the rate counts from zero.
	f.tick(t, 10*time.Second)
	res = f.query(t, metrics.HistoryQuery{Metric: "s3_latency_avg", Range: time.Minute})
	assert.True(t, math.IsNaN(last(res.Nodes[0].Values)))
	res = f.query(t, metrics.HistoryQuery{Metric: "s3_requests", Range: time.Minute})
	assert.InDelta(t, 0, last(res.Nodes[0].Values), 1e-9)

	// A gauge whose family is not registered has no point; a counter
	// without series yet counts from zero.
	res = f.query(t, metrics.HistoryQuery{Metric: "mrf_pending"})
	assert.True(t, math.IsNaN(last(res.Nodes[0].Values)))
	res = f.query(t, metrics.HistoryQuery{Metric: "rpc_requests"})
	assert.InDelta(t, 0, last(res.Nodes[0].Values), 1e-9)
}

func TestHistoryCounterReset(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(t0)
	reg := metricsmock.NewRegistry(t)
	values := []float64{100, 160, 30}
	i := 0
	reg.EXPECT().Snapshot(mock.Anything).RunAndReturn(func(context.Context) ([]metrics.Family, error) {
		v := values[i]
		i++
		return []metrics.Family{{Name: "nexo_s3_requests_total", Kind: metrics.KindCounter, Samples: []metrics.Sample{{Value: v}}}}, nil
	})
	h, err := metrics.NewHistory(metrics.HistoryDeps{Registry: reg, Clock: clk})
	require.NoError(t, err)
	for range values {
		require.NoError(t, h.Sample(ctx))
		clk.Advance(10 * time.Second)
	}
	res, err := h.Query(ctx, metrics.HistoryQuery{Metric: "s3_requests", Range: 30 * time.Second, Step: 10 * time.Second})
	require.NoError(t, err)
	got := res.Nodes[0].Values
	assert.InDelta(t, 6, got[len(got)-3], 1e-9)
	assert.InDelta(t, 3, got[len(got)-2], 1e-9, "a counter that went down restarted from zero")

	reg2 := metricsmock.NewRegistry(t)
	reg2.EXPECT().Snapshot(mock.Anything).Return(nil, errors.New("closed"))
	h2, err := metrics.NewHistory(metrics.HistoryDeps{Registry: reg2})
	require.NoError(t, err)
	require.Error(t, h2.Sample(ctx))
}

func TestHistoryRingAndStep(t *testing.T) {
	f := newHistoryFixture(t, nil, metrics.WithRetention(time.Minute))
	for i := 0; i < 12; i++ {
		f.online.Set(f.ctx, float64(i), "/d1")
		f.tick(t, 10*time.Second)
	}
	// The ring keeps 6 samples: the older slots were overwritten.
	res := f.query(t, metrics.HistoryQuery{Metric: "drives_online", Range: time.Hour, Step: 10 * time.Second})
	assert.Equal(t, f.clk.Now().Add(-time.Minute), res.Start, "the range is bounded by the retention")
	vals := res.Nodes[0].Values
	assert.True(t, math.IsNaN(vals[0]), "older than the ring")
	assert.InDelta(t, 11, last(vals), 1e-9)
	assert.InDelta(t, 6, vals[1], 1e-9)

	// A coarser step averages the samples of each step.
	res = f.query(t, metrics.HistoryQuery{Metric: "drives_online", Range: time.Minute, Step: 25 * time.Second})
	assert.Equal(t, 30*time.Second, res.Step, "a multiple of the resolution")
	assert.InDelta(t, 6.5, res.Nodes[0].Values[0], 1e-9, "average of 6 and 7 (the sample of 5 left the ring)")

	_, err := f.hist.Query(f.ctx, metrics.HistoryQuery{Metric: "nope"})
	require.ErrorIs(t, err, domain.ErrNotFound)
	_, err = f.hist.Query(f.ctx, metrics.HistoryQuery{Metric: "drives_online", Start: f.clk.Now(), End: f.clk.Now().Add(-time.Minute)})
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = f.hist.Query(f.ctx, metrics.HistoryQuery{Metric: "drives_online", End: f.clk.Now().Add(-2 * time.Hour), Range: time.Minute})
	require.ErrorIs(t, err, domain.ErrInvalidArgument, "older than the retention")
}

func TestHistoryDefaultStep(t *testing.T) {
	f := newHistoryFixture(t, nil)
	res := f.query(t, metrics.HistoryQuery{Metric: "drives_online", Range: 24 * time.Hour})
	assert.Equal(t, 4*time.Minute, res.Step, "at most 360 points")
	assert.LessOrEqual(t, len(res.Nodes[0].Values), 361)
	res = f.query(t, metrics.HistoryQuery{Metric: "drives_online"})
	assert.Equal(t, 10*time.Second, res.Step)
	assert.Equal(t, time.Hour, res.End.Sub(res.Start), "one hour by default")
}

func TestHistoryCluster(t *testing.T) {
	up := metricsmock.NewPeer(t)
	up.EXPECT().NodeID().Return("n2")
	up.EXPECT().History(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, q metrics.HistoryQuery) (metrics.NodeSeries, error) {
		assert.True(t, q.Local, "a peer answers with its own points")
		assert.Empty(t, q.Nodes)
		n := int(q.End.Sub(q.Start)/q.Step) + 1
		vals := make([]float64, n)
		for i := range vals {
			vals[i] = math.NaN()
		}
		vals[n-1] = 3
		return metrics.NodeSeries{Node: "ignored", Values: vals}, nil
	})
	down := metricsmock.NewPeer(t)
	down.EXPECT().NodeID().Return("n3")
	down.EXPECT().History(mock.Anything, mock.Anything).Return(metrics.NodeSeries{}, errors.New("unreachable"))
	short := metricsmock.NewPeer(t)
	short.EXPECT().NodeID().Return("n4")
	short.EXPECT().History(mock.Anything, mock.Anything).Return(metrics.NodeSeries{Values: []float64{1}}, nil)

	f := newHistoryFixture(t, []metrics.Peer{up, down, short})
	f.online.Set(f.ctx, 2, "/d1")
	require.NoError(t, f.hist.Sample(f.ctx))
	res := f.query(t, metrics.HistoryQuery{Metric: "drives_online", Range: time.Minute})
	require.Len(t, res.Nodes, 4)
	assert.Equal(t, "n1", res.Nodes[0].Node)
	assert.Equal(t, "n2", res.Nodes[1].Node)
	assert.InDelta(t, 3, last(res.Nodes[1].Values), 1e-9)
	assert.Equal(t, "n3", res.Nodes[2].Node)
	assert.Equal(t, "unreachable", res.Nodes[2].Error)
	assert.Equal(t, "n4", res.Nodes[3].Node)
	assert.NotEmpty(t, res.Nodes[3].Error, "a wrong number of points is an error")
	assert.InDelta(t, 5, last(res.Cluster), 1e-9, "drives_online sums the nodes")
	assert.True(t, math.IsNaN(res.Cluster[0]))

	// The nodes filter selects the peers asked.
	res = f.query(t, metrics.HistoryQuery{Metric: "drives_online", Range: time.Minute, Nodes: []string{"n2"}})
	require.Len(t, res.Nodes, 1)
	assert.Equal(t, "n2", res.Nodes[0].Node)
	// A local query asks no peer and keeps its bounds.
	start := t0.Add(-30 * time.Second)
	res = f.query(t, metrics.HistoryQuery{Metric: "drives_online", Start: start, End: t0.Add(time.Hour), Step: 10 * time.Second, Local: true})
	require.Len(t, res.Nodes, 1)
	assert.Equal(t, start, res.Start)
	assert.Equal(t, t0.Add(time.Hour), res.End)
}

func TestHistoryAggregations(t *testing.T) {
	peer := metricsmock.NewPeer(t)
	peer.EXPECT().NodeID().Return("n2")
	peer.EXPECT().History(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, q metrics.HistoryQuery) (metrics.NodeSeries, error) {
		n := int(q.End.Sub(q.Start)/q.Step) + 1
		vals := make([]float64, n)
		for i := range vals {
			vals[i] = 0.5
		}
		return metrics.NodeSeries{Values: vals}, nil
	})
	f := newHistoryFixture(t, []metrics.Peer{peer})
	require.NoError(t, f.hist.Sample(f.ctx))
	for i := 0; i < 10; i++ {
		f.latency.Observe(f.ctx, 0.9, "GetObject")
	}
	f.tick(t, 10*time.Second)
	avg := f.query(t, metrics.HistoryQuery{Metric: "s3_latency_avg", Range: time.Minute})
	assert.InDelta(t, 0.7, last(avg.Cluster), 1e-9, "avg of 0.9 and 0.5")
	p99 := f.query(t, metrics.HistoryQuery{Metric: "s3_latency_p99", Range: time.Minute})
	assert.InDelta(t, 0.995, last(p99.Nodes[0].Values), 1e-9, "interpolated in the bucket (0.5, 1]")
	assert.InDelta(t, 0.995, last(p99.Cluster), 1e-9, "max of the nodes")
}

func TestHistoryRunner(t *testing.T) {
	ctx := context.Background()
	reg := metrics.New(metrics.Deps{}, metrics.WithoutRuntimeCollectors())
	h, err := metrics.NewHistory(metrics.HistoryDeps{Registry: reg}, metrics.WithResolution(time.Second))
	require.NoError(t, err)
	assert.NotEmpty(t, h.Metrics(ctx))
	require.NoError(t, h.Start(ctx))
	require.NoError(t, h.Start(ctx), "idempotent")
	require.NoError(t, h.Stop(ctx))
	require.NoError(t, h.Stop(ctx), "idempotent")
	_, err = metrics.NewHistory(metrics.HistoryDeps{})
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
}
