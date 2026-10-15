package rpc_test

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/audit"
	"github.com/r3n3r0/nexo/internal/service/logs"
	"github.com/r3n3r0/nexo/internal/service/metrics"
	"github.com/r3n3r0/nexo/internal/service/trace"
	"github.com/r3n3r0/nexo/internal/transport/rpc"
)

// obsNode is the observability of a test node, served over the RPC.
type obsNode struct {
	reg     metrics.Registry
	clk     *clock.Fake
	history metrics.History
	trace   trace.Service
	logs    logs.Service
	audit   audit.Service
	peer    rpc.ObservabilityPeer
}

func newObsNode(t *testing.T) *obsNode {
	t.Helper()
	ctx := context.Background()
	n := &obsNode{reg: metrics.New(metrics.Deps{}, metrics.WithoutRuntimeCollectors()), clk: clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))}
	c, err := n.reg.Counter(ctx, metrics.Definition{Subsystem: "s3", Name: "requests_total", Help: "Requests.", Labels: []string{"api"}})
	require.NoError(t, err)
	c.Add(ctx, 4, "GetObject")
	b, err := n.reg.Counter(ctx, metrics.Definition{Subsystem: "bucket", Name: "traffic_sent_bytes_total", Help: "Bytes.", Labels: []string{"bucket"}})
	require.NoError(t, err)
	b.Add(ctx, 10, "photos")
	h, err := n.reg.Histogram(ctx, metrics.Definition{Subsystem: "s3", Name: "request_duration_seconds", Help: "Latency.", Labels: []string{"api"}})
	require.NoError(t, err)
	h.Observe(ctx, 0.3, "GetObject")
	g, err := n.reg.Gauge(ctx, metrics.Definition{Subsystem: "drive", Name: "online", Help: "Online.", Labels: []string{"drive"}})
	require.NoError(t, err)
	g.Set(ctx, 1, "/d1")
	exp, err := metrics.NewExporter(metrics.ExporterDeps{Registry: n.reg, NodeID: "remote"})
	require.NoError(t, err)
	n.history, err = metrics.NewHistory(metrics.HistoryDeps{Registry: n.reg, NodeID: "remote", Clock: n.clk})
	require.NoError(t, err)
	require.NoError(t, n.history.Sample(ctx))
	n.trace, err = trace.New(trace.Deps{NodeID: "remote"})
	require.NoError(t, err)
	n.logs, err = logs.New(logs.Deps{NodeID: "remote"})
	require.NoError(t, err)
	n.audit = audit.NewSlog(audit.Deps{Logger: slog.New(slog.DiscardHandler), NodeID: "remote"})

	routes, err := rpc.NewObservabilityRoutes(rpc.ObservabilityRoutesDeps{Metrics: exp, History: n.history, Trace: n.trace, Logs: n.logs, Audit: n.audit})
	require.NoError(t, err)
	addr, _ := startRPC(t, routes)
	n.peer, err = rpc.NewObservabilityClient(rpc.ObservabilityClientDeps{Conn: dial(t, newAuth(t, "client:9000"), addr)})
	require.NoError(t, err)
	assert.Equal(t, addr, n.peer.NodeID())
	return n
}

func TestObservabilityMetricsOverRPC(t *testing.T) {
	ctx := context.Background()
	n := newObsNode(t)

	node, err := n.peer.Families(ctx, metrics.ScopeNode)
	require.NoError(t, err)
	byName := map[string]metrics.Family{}
	for _, f := range node {
		byName[f.Name] = f
	}
	require.Contains(t, byName, "nexo_s3_requests_total")
	assert.NotContains(t, byName, "nexo_bucket_traffic_sent_bytes_total")
	req := byName["nexo_s3_requests_total"]
	assert.Equal(t, metrics.KindCounter, req.Kind)
	require.Len(t, req.Samples, 1)
	assert.Equal(t, map[string]string{"api": "GetObject"}, req.Samples[0].Labels, "the server label is added by the caller")
	assert.InDelta(t, 4, req.Samples[0].Value, 0)
	lat := byName["nexo_s3_request_duration_seconds"]
	require.Len(t, lat.Samples, 1)
	assert.Equal(t, uint64(1), lat.Samples[0].Count)
	assert.NotEmpty(t, lat.Samples[0].Buckets, "the histograms cross the wire whole")

	bucket, err := n.peer.Families(ctx, metrics.ScopeBucket)
	require.NoError(t, err)
	require.Len(t, bucket, 1)
	assert.Equal(t, "nexo_bucket_traffic_sent_bytes_total", bucket[0].Name)

	_, err = n.peer.Families(ctx, "galaxy")
	require.ErrorIs(t, err, domain.ErrInvalidArgument, "the domain errors cross the wire")
}

func TestObservabilityHistoryOverRPC(t *testing.T) {
	ctx := context.Background()
	n := newObsNode(t)
	end := n.clk.Now()
	start := end.Add(-time.Minute)
	ns, err := n.peer.History(ctx, metrics.HistoryQuery{Metric: "drives_online", Start: start, End: end, Step: 10 * time.Second, Local: true})
	require.NoError(t, err)
	require.Len(t, ns.Values, 7, "the bounds of the caller are kept")
	assert.True(t, math.IsNaN(ns.Values[0]), "NaN crosses the wire")
	assert.InDelta(t, 1, ns.Values[6], 1e-9)

	_, err = n.peer.History(ctx, metrics.HistoryQuery{Metric: "nope"})
	require.ErrorIs(t, err, domain.ErrNotFound)
}

func TestObservabilityAuditOverRPC(t *testing.T) {
	ctx := context.Background()
	n := newObsNode(t)
	e := audit.Entry{Time: time.Now().UTC(), Type: "s3", RequestID: "r1", AccessKey: "alice"}
	e.API.Name, e.API.Bucket, e.API.StatusCode, e.API.TimeToResponse = "PutObject", "photos", 200, 3*time.Millisecond
	n.audit.Log(ctx, e)
	n.audit.Log(ctx, audit.Entry{Time: time.Now().UTC(), Type: "admin", API: audit.API{Name: "GET /info", StatusCode: 200}})

	got, err := n.peer.SearchAudit(ctx, audit.Query{Bucket: "photos", Nodes: []string{"ignored"}})
	require.NoError(t, err)
	require.Len(t, got, 1, "the filter applies on the peer, its own entries only")
	assert.Equal(t, "remote", got[0].Node)
	assert.Equal(t, "PutObject", got[0].API.Name)
	assert.Equal(t, 3*time.Millisecond, got[0].API.TimeToResponse)
	assert.Equal(t, "alice", got[0].AccessKey)
}

func TestObservabilityTraceOverRPC(t *testing.T) {
	n := newObsNode(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan trace.Delivery, 4)
	done := make(chan error, 1)
	go func() {
		done <- n.peer.Trace(ctx, trace.Filter{APIs: []string{"PutObject"}, Nodes: []string{"elsewhere"}}, func(d trace.Delivery) error {
			got <- d
			return nil
		})
	}()
	// The stream subscribes on the peer: publish until it is seen.
	require.Eventually(t, n.trace.Enabled, 5*time.Second, 5*time.Millisecond, "the stream subscribes on the peer")
	n.trace.Publish(ctx, trace.Event{Type: trace.TypeS3, API: "GetObject"})
	n.trace.Publish(ctx, trace.Event{Type: trace.TypeS3, API: "PutObject", Status: 200, Duration: time.Second, Query: "X-Amz-Signature=abc"})
	select {
	case d := <-got:
		require.NotNil(t, d.Event)
		assert.Equal(t, "PutObject", d.Event.API, "filtered on the peer")
		assert.Equal(t, "remote", d.Event.Node)
		assert.Equal(t, time.Second, d.Event.Duration)
		assert.Equal(t, "X-Amz-Signature=REDACTED", d.Event.Query)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "no event over the stream")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			assert.True(t, errors.Is(err, context.Canceled) || ctx.Err() != nil, "%v", err)
		}
	case <-time.After(5 * time.Second):
		require.FailNow(t, "the stream does not end with its context")
	}
	assert.Eventually(t, func() bool { return !n.trace.Enabled() }, 5*time.Second, 5*time.Millisecond, "the peer unsubscribes")

	err := n.peer.Trace(context.Background(), trace.Filter{Types: []trace.Type{"ftp"}}, func(trace.Delivery) error { return nil })
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
}

func TestObservabilityLogsOverRPC(t *testing.T) {
	n := newObsNode(t)
	logger := slog.New(n.logs.Handler())
	logger.Info("before", "k", "v")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan logs.Delivery, 4)
	go func() {
		_ = n.peer.Logs(ctx, logs.Filter{Last: 1, Level: "info"}, func(d logs.Delivery) error {
			got <- d
			return nil
		})
	}()
	next := func() logs.Entry {
		select {
		case d := <-got:
			require.NotNil(t, d.Entry)
			return *d.Entry
		case <-time.After(5 * time.Second):
			require.FailNow(t, "no entry over the stream")
			return logs.Entry{}
		}
	}
	first := next()
	assert.Equal(t, "before", first.Message, "the replay crosses the wire")
	assert.Equal(t, map[string]string{"k": "v"}, first.Attrs)
	assert.Equal(t, "remote", first.Node)
	logger.Debug("filtered")
	logger.Warn("after")
	assert.Equal(t, "after", next().Message)
}

func TestObservabilityDeps(t *testing.T) {
	_, err := rpc.NewObservabilityRoutes(rpc.ObservabilityRoutesDeps{})
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = rpc.NewObservabilityClient(rpc.ObservabilityClientDeps{})
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
}
