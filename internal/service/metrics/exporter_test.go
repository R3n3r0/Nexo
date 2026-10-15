package metrics_test

import (
	"bytes"
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/common/expfmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/metrics"
	metricsmock "github.com/r3n3r0/nexo/internal/service/metrics/mock"
	"github.com/r3n3r0/nexo/internal/service/scanner"
	scannermock "github.com/r3n3r0/nexo/internal/service/scanner/mock"
)

// family finds a family by name.
func family(fams []metrics.Family, name string) (metrics.Family, bool) {
	for _, f := range fams {
		if f.Name == name {
			return f, true
		}
	}
	return metrics.Family{}, false
}

// valueOf returns the value of the sample of name with exactly labels.
func valueOf(t *testing.T, fams []metrics.Family, name string, labels map[string]string) float64 {
	t.Helper()
	f, ok := family(fams, name)
	require.True(t, ok, "family %s missing", name)
	for _, s := range f.Samples {
		if assert.ObjectsAreEqual(labels, s.Labels) || (len(labels) == 0 && len(s.Labels) == 0) {
			return s.Value
		}
	}
	require.Failf(t, "sample missing", "%s%v in %+v", name, labels, f.Samples)
	return math.NaN()
}

func TestObserveAndSnapshot(t *testing.T) {
	ctx := context.Background()
	reg := metrics.New(metrics.Deps{}, metrics.WithoutRuntimeCollectors())
	calls := 0
	require.NoError(t, reg.Observe(ctx, metrics.Definition{Subsystem: "drive", Name: "online", Help: "Drives online.", Labels: []string{"drive"}},
		metrics.KindGauge, func(context.Context) []metrics.Observation {
			calls++
			return []metrics.Observation{
				{LabelValues: []string{"/d1"}, Value: 1},
				{LabelValues: []string{"/d2"}, Value: 0},
				{LabelValues: []string{"/d2"}, Value: 1},          // the last value wins
				{LabelValues: []string{"/d3", "extra"}, Value: 1}, // wrong arity: dropped
			}
		}))
	require.NoError(t, reg.Observe(ctx, metrics.Definition{Subsystem: "mrf", Name: "added_total", Help: "Added."},
		metrics.KindCounter, func(context.Context) []metrics.Observation { return []metrics.Observation{{Value: 7}} }))
	c, err := reg.Counter(ctx, metrics.Definition{Subsystem: "s3", Name: "requests_total", Help: "Requests.", Labels: []string{"api"}})
	require.NoError(t, err)
	c.Add(ctx, 3, "GetObject")
	h, err := reg.Histogram(ctx, metrics.Definition{Subsystem: "s3", Name: "request_duration_seconds", Help: "Latency."})
	require.NoError(t, err)
	h.Observe(ctx, 0.2)

	fams, err := reg.Snapshot(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, calls)
	online, ok := family(fams, "nexo_drive_online")
	require.True(t, ok)
	assert.Equal(t, metrics.KindGauge, online.Kind)
	assert.Equal(t, "Drives online.", online.Help)
	require.Len(t, online.Samples, 2)
	assert.InDelta(t, 1, valueOf(t, fams, "nexo_drive_online", map[string]string{"drive": "/d2"}), 0)
	added, _ := family(fams, "nexo_mrf_added_total")
	assert.Equal(t, metrics.KindCounter, added.Kind)
	assert.InDelta(t, 7, valueOf(t, fams, "nexo_mrf_added_total", nil), 0)
	assert.InDelta(t, 3, valueOf(t, fams, "nexo_s3_requests_total", map[string]string{"api": "GetObject"}), 0)
	hist, _ := family(fams, "nexo_s3_request_duration_seconds")
	assert.Equal(t, metrics.KindHistogram, hist.Kind)
	require.Len(t, hist.Samples, 1)
	assert.Equal(t, uint64(1), hist.Samples[0].Count)
	assert.InDelta(t, 0.2, hist.Samples[0].Sum, 1e-9)
	assert.NotEmpty(t, hist.Samples[0].Buckets)

	// The registry scrape serves the observed families too.
	out := scrape(t, reg)
	assert.Contains(t, out, `nexo_drive_online{drive="/d1"} 1`)
	assert.Contains(t, out, `nexo_mrf_added_total 7`)

	for name, tc := range map[string]struct {
		def  metrics.Definition
		kind metrics.Kind
		fn   metrics.ObserveFunc
	}{
		"histogram": {metrics.Definition{Subsystem: "x", Name: "h", Help: "h"}, metrics.KindHistogram, func(context.Context) []metrics.Observation { return nil }},
		"no source": {metrics.Definition{Subsystem: "x", Name: "g", Help: "g"}, metrics.KindGauge, nil},
		"no help":   {metrics.Definition{Subsystem: "x", Name: "g"}, metrics.KindGauge, func(context.Context) []metrics.Observation { return nil }},
		"duplicate": {metrics.Definition{Subsystem: "drive", Name: "online", Help: "again", Labels: []string{"drive"}}, metrics.KindGauge,
			func(context.Context) []metrics.Observation { return nil }},
	} {
		t.Run(name, func(t *testing.T) {
			require.ErrorIs(t, reg.Observe(ctx, tc.def, tc.kind, tc.fn), domain.ErrInvalidArgument)
		})
	}
}

func TestObserveTimeout(t *testing.T) {
	ctx := context.Background()
	reg := metrics.New(metrics.Deps{}, metrics.WithoutRuntimeCollectors(), metrics.WithObserveTimeout(10*time.Millisecond))
	require.NoError(t, reg.Observe(ctx, metrics.Definition{Subsystem: "slow", Name: "value", Help: "Slow."}, metrics.KindGauge,
		func(ctx context.Context) []metrics.Observation {
			<-ctx.Done()
			return nil
		}))
	start := time.Now()
	_, err := reg.Snapshot(ctx)
	require.NoError(t, err)
	assert.Less(t, time.Since(start), 5*time.Second, "a slow source is bounded")
}

// newNodeRegistry returns a registry with one node family and one bucket
// family.
func newNodeRegistry(t *testing.T, requests, bucketBytes float64) metrics.Registry {
	t.Helper()
	ctx := context.Background()
	reg := metrics.New(metrics.Deps{}, metrics.WithoutRuntimeCollectors())
	c, err := reg.Counter(ctx, metrics.Definition{Subsystem: "s3", Name: "requests_total", Help: "Requests.", Labels: []string{"api"}})
	require.NoError(t, err)
	c.Add(ctx, requests, "PutObject")
	b, err := reg.Counter(ctx, metrics.Definition{Subsystem: "bucket", Name: "traffic_received_bytes_total", Help: "Bytes in.", Labels: []string{"bucket"}})
	require.NoError(t, err)
	b.Add(ctx, bucketBytes, "photos")
	return reg
}

func TestExporterLocalAndNode(t *testing.T) {
	ctx := context.Background()
	exp, err := metrics.NewExporter(metrics.ExporterDeps{Registry: newNodeRegistry(t, 2, 100), NodeID: "n1:9000"})
	require.NoError(t, err)

	node, err := exp.Local(ctx, metrics.ScopeNode)
	require.NoError(t, err)
	_, hasBucket := family(node, "nexo_bucket_traffic_received_bytes_total")
	assert.False(t, hasBucket, "the bucket families are on the bucket endpoint only")
	bucket, err := exp.Local(ctx, metrics.ScopeBucket)
	require.NoError(t, err)
	require.Len(t, bucket, 1)

	gathered, err := exp.Gather(ctx, metrics.ScopeNode)
	require.NoError(t, err)
	assert.InDelta(t, 2, valueOf(t, gathered, "nexo_s3_requests_total", map[string]string{"api": "PutObject", "server": "n1:9000"}), 0)

	var buf bytes.Buffer
	require.NoError(t, exp.WriteText(ctx, &buf, gathered))
	text := buf.String()
	assert.Contains(t, text, "# TYPE nexo_s3_requests_total counter")
	assert.Contains(t, text, `nexo_s3_requests_total{api="PutObject",server="n1:9000"} 2`)
	var p expfmt.TextParser
	_, err = p.TextToMetricFamilies(strings.NewReader(text))
	require.NoError(t, err, "valid text exposition")

	_, err = exp.Gather(ctx, "galaxy")
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = metrics.NewExporter(metrics.ExporterDeps{})
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	require.Error(t, exp.WriteText(cancelled, &buf, gathered))
}

func TestExporterCluster(t *testing.T) {
	ctx := context.Background()
	remote := newNodeRegistry(t, 5, 0)
	remoteFams, err := remote.Snapshot(ctx)
	require.NoError(t, err)
	up := metricsmock.NewPeer(t)
	up.EXPECT().NodeID().Return("n2:9000")
	up.EXPECT().Families(mock.Anything, metrics.ScopeNode).Return(remoteFams, nil)
	down := metricsmock.NewPeer(t)
	down.EXPECT().NodeID().Return("n3:9000").Maybe()
	down.EXPECT().Families(mock.Anything, metrics.ScopeNode).Return(nil, errors.New("unreachable"))

	disk := func(pool int, state string, total, free uint64) domain.DiskInfo {
		return domain.DiskInfo{PoolIdx: pool, State: state, Total: total, Free: free, Used: total - free}
	}
	info := &domain.ServerInfo{
		Nodes: []domain.NodeInfo{
			{Endpoint: "n1:9000", State: domain.StateOnline, Disks: []domain.DiskInfo{disk(0, domain.StateOnline, 100, 40), disk(0, domain.StateHealing, 100, 90)}},
			{Endpoint: "n2:9000", State: domain.StateOnline, Disks: []domain.DiskInfo{disk(0, domain.StateOnline, 100, 50), disk(0, domain.StateOffline, 100, 100)}},
			{Endpoint: "n3:9000", State: domain.StateOffline},
		},
		Pools: []domain.PoolInfo{{Index: 0, Sets: 2, DrivesPerSet: 4, Parity: 2, ReadQuorum: 2, WriteQuorum: 3,
			ErasureSets: []domain.ErasureSetInfo{{State: domain.SetStateOK}, {State: domain.SetStateDegraded}, {State: domain.SetStateReadOnly}, {State: domain.SetStateUnavailable}}}},
		Buckets: 3, Objects: 10, Usage: 1234,
	}
	clusterInfo := metricsmock.NewClusterInfo(t)
	clusterInfo.EXPECT().ServerInfo(mock.Anything).Return(info, nil)

	exp, err := metrics.NewExporter(metrics.ExporterDeps{Registry: newNodeRegistry(t, 2, 0), NodeID: "n1:9000",
		Peers: []metrics.Peer{up, down}, Cluster: clusterInfo}, metrics.WithPeerTimeout(time.Second))
	require.NoError(t, err)
	fams, err := exp.Gather(ctx, metrics.ScopeCluster)
	require.NoError(t, err)

	assert.InDelta(t, 2, valueOf(t, fams, "nexo_s3_requests_total", map[string]string{"api": "PutObject", "server": "n1:9000"}), 0)
	assert.InDelta(t, 5, valueOf(t, fams, "nexo_s3_requests_total", map[string]string{"api": "PutObject", "server": "n2:9000"}), 0)
	reqs, _ := family(fams, "nexo_s3_requests_total")
	assert.Len(t, reqs.Samples, 2, "an unreachable peer is left out")

	for name, want := range map[string]float64{
		"nexo_cluster_nodes_online": 2, "nexo_cluster_nodes_offline": 1,
		"nexo_cluster_drives_online": 3, "nexo_cluster_drives_offline": 1, "nexo_cluster_drives_healing": 1, "nexo_cluster_drives_total": 4,
		"nexo_cluster_capacity_raw_total_bytes": 300, "nexo_cluster_capacity_raw_free_bytes": 180, "nexo_cluster_capacity_raw_used_bytes": 120,
		"nexo_cluster_capacity_usable_total_bytes": 150, "nexo_cluster_capacity_usable_free_bytes": 90,
		"nexo_cluster_buckets": 3, "nexo_cluster_objects": 10, "nexo_cluster_usage_bytes": 1234,
	} {
		assert.InDelta(t, want, valueOf(t, fams, name, nil), 1e-9, name)
	}
	pool := map[string]string{"pool": "0"}
	for name, want := range map[string]float64{
		"nexo_cluster_sets_healthy": 1, "nexo_cluster_sets_degraded": 1, "nexo_cluster_sets_read_only": 1, "nexo_cluster_sets_unavailable": 1,
		"nexo_cluster_write_quorum": 3, "nexo_cluster_read_quorum": 2,
	} {
		assert.InDelta(t, want, valueOf(t, fams, name, pool), 1e-9, name)
	}

	var buf bytes.Buffer
	require.NoError(t, exp.WriteText(ctx, &buf, fams))
	var p expfmt.TextParser
	parsed, err := p.TextToMetricFamilies(strings.NewReader(buf.String()))
	require.NoError(t, err)
	assert.Contains(t, parsed, "nexo_cluster_sets_degraded")
}

func TestExporterClusterInfoUnavailable(t *testing.T) {
	ctx := context.Background()
	clusterInfo := metricsmock.NewClusterInfo(t)
	clusterInfo.EXPECT().ServerInfo(mock.Anything).Return(nil, errors.New("no quorum"))
	exp, err := metrics.NewExporter(metrics.ExporterDeps{Registry: newNodeRegistry(t, 1, 0), Cluster: clusterInfo})
	require.NoError(t, err)
	fams, err := exp.Gather(ctx, metrics.ScopeCluster)
	require.NoError(t, err, "the node families are served without the cluster ones")
	_, ok := family(fams, "nexo_cluster_nodes_online")
	assert.False(t, ok)
	assert.InDelta(t, 1, valueOf(t, fams, "nexo_s3_requests_total", map[string]string{"api": "PutObject", "server": "local"}), 0)
}

func TestExporterBucket(t *testing.T) {
	ctx := context.Background()
	remote := newNodeRegistry(t, 0, 50)
	remoteBucket, err := metrics.NewExporter(metrics.ExporterDeps{Registry: remote})
	require.NoError(t, err)
	remoteFams, err := remoteBucket.Local(ctx, metrics.ScopeBucket)
	require.NoError(t, err)
	peer := metricsmock.NewPeer(t)
	peer.EXPECT().Families(mock.Anything, metrics.ScopeBucket).Return(remoteFams, nil)
	usage := scannermock.NewService(t)
	last := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	usage.EXPECT().Usage(mock.Anything).Return(scanner.Usage{LastUpdate: last, Buckets: map[string]scanner.BucketUsage{
		"photos": {Counts: scanner.Counts{Objects: 4, Versions: 2, DeleteMarkers: 1, Size: 4096}},
	}}, nil)

	exp, err := metrics.NewExporter(metrics.ExporterDeps{Registry: newNodeRegistry(t, 0, 100), Peers: []metrics.Peer{peer}, Usage: usage})
	require.NoError(t, err)
	fams, err := exp.Gather(ctx, metrics.ScopeBucket)
	require.NoError(t, err)
	photos := map[string]string{"bucket": "photos"}
	assert.InDelta(t, 150, valueOf(t, fams, "nexo_bucket_traffic_received_bytes_total", photos), 0, "summed over the nodes, no server label")
	assert.InDelta(t, 4096, valueOf(t, fams, "nexo_bucket_usage_total_bytes", photos), 0)
	assert.InDelta(t, 4, valueOf(t, fams, "nexo_bucket_objects", photos), 0)
	assert.InDelta(t, 2, valueOf(t, fams, "nexo_bucket_versions", photos), 0)
	assert.InDelta(t, 1, valueOf(t, fams, "nexo_bucket_delete_markers", photos), 0)
	assert.InDelta(t, float64(last.Unix()), valueOf(t, fams, "nexo_bucket_usage_last_update_timestamp_seconds", nil), 0)
	_, nodeFamily := family(fams, "nexo_s3_requests_total")
	assert.False(t, nodeFamily)
}

// TestHistogramMerge checks that the buckets of histograms add up in the
// bucket scope.
func TestHistogramMerge(t *testing.T) {
	ctx := context.Background()
	mk := func(v float64) []metrics.Family {
		reg := metrics.New(metrics.Deps{}, metrics.WithoutRuntimeCollectors())
		h, err := reg.Histogram(ctx, metrics.Definition{Subsystem: "bucket", Name: "latency_seconds", Help: "L.", Labels: []string{"bucket"},
			Buckets: []float64{0.1, 1}})
		require.NoError(t, err)
		h.Observe(ctx, v, "b")
		fams, err := reg.Snapshot(ctx)
		require.NoError(t, err)
		return fams
	}
	peer := metricsmock.NewPeer(t)
	peer.EXPECT().Families(mock.Anything, metrics.ScopeBucket).Return(mk(0.5), nil)
	reg := metrics.New(metrics.Deps{}, metrics.WithoutRuntimeCollectors())
	h, err := reg.Histogram(ctx, metrics.Definition{Subsystem: "bucket", Name: "latency_seconds", Help: "L.", Labels: []string{"bucket"},
		Buckets: []float64{0.1, 1}})
	require.NoError(t, err)
	h.Observe(ctx, 0.05, "b")
	exp, err := metrics.NewExporter(metrics.ExporterDeps{Registry: reg, Peers: []metrics.Peer{peer}})
	require.NoError(t, err)
	fams, err := exp.Gather(ctx, metrics.ScopeBucket)
	require.NoError(t, err)
	f, ok := family(fams, "nexo_bucket_latency_seconds")
	require.True(t, ok)
	require.Len(t, f.Samples, 1)
	s := f.Samples[0]
	assert.Equal(t, uint64(2), s.Count)
	assert.InDelta(t, 0.55, s.Sum, 1e-9)
	require.Len(t, s.Buckets, 2)
	assert.Equal(t, uint64(1), s.Buckets[0].Count, "le=0.1")
	assert.Equal(t, uint64(2), s.Buckets[1].Count, "le=1")
}
