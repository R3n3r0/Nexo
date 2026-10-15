package admin_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/audit"
	"github.com/r3n3r0/nexo/internal/service/logs"
	logsmock "github.com/r3n3r0/nexo/internal/service/logs/mock"
	"github.com/r3n3r0/nexo/internal/service/metrics"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/service/trace"
	tracemock "github.com/r3n3r0/nexo/internal/service/trace/mock"
	"github.com/r3n3r0/nexo/internal/transport/http/admin"
)

// sseEvent is a parsed Server-Sent Event.
type sseEvent struct {
	name string
	data string
}

// parseSSE splits a stream into its events and comments.
func parseSSE(t *testing.T, body string) (events []sseEvent, comments []string) {
	t.Helper()
	sc := bufio.NewScanner(strings.NewReader(body))
	var cur sseEvent
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if cur.name != "" || cur.data != "" {
				events = append(events, cur)
			}
			cur = sseEvent{}
		case strings.HasPrefix(line, ":"):
			comments = append(comments, strings.TrimSpace(line[1:]))
		case strings.HasPrefix(line, "event: "):
			cur.name = line[len("event: "):]
		case strings.HasPrefix(line, "data: "):
			cur.data = line[len("data: "):]
		}
	}
	return events, comments
}

func TestTraceStream(t *testing.T) {
	f := newFixture(t)
	c := f.root(t)
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	ch := make(chan trace.Event, 2)
	ch <- trace.Event{Time: now, Node: "n1", Type: trace.TypeS3, API: "PutObject", Method: http.MethodPut, Path: "/b/k",
		Query: "X-Amz-Signature=REDACTED", Status: 200, Duration: 1500 * time.Microsecond, InputBytes: 5, RequestID: "r1",
		ClientIP: "10.0.0.1", User: "alice", Bucket: "b"}
	ch <- trace.Event{Time: now, Node: "n2", Type: trace.TypeAdmin, API: "GET /info", Method: http.MethodGet, Path: "/api/v1/info", Status: 500, Error: "boom"}
	close(ch)
	sub := tracemock.NewSubscription(t)
	sub.EXPECT().Events().Return(ch)
	sub.EXPECT().Dropped().Return(uint64(3))
	f.tracer.EXPECT().Subscribe(mock.Anything, mock.MatchedBy(func(fl trace.Filter) bool {
		return assert.ObjectsAreEqual([]string{"n1", "n2"}, fl.Nodes) &&
			assert.ObjectsAreEqual([]trace.Type{trace.TypeS3, trace.TypeAdmin}, fl.Types) &&
			assert.ObjectsAreEqual([]string{"PutObject"}, fl.APIs) && assert.ObjectsAreEqual([]string{"b"}, fl.Buckets) &&
			assert.ObjectsAreEqual([]int{404}, fl.StatusCodes) && assert.ObjectsAreEqual([]int{5}, fl.StatusClasses) &&
			fl.ErrorsOnly && fl.MinDuration == 250*time.Millisecond
	})).Return(sub, nil)

	rec := c.do(http.MethodGet, "/trace?node=n1,n2&type=S3&type=admin&api=PutObject&bucket=b&status=404,5xx&errors=true&threshold=250ms", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
	assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"))
	assert.True(t, strings.HasPrefix(rec.Body.String(), "retry: 3000\n\n"))
	assert.Equal(t, policy.ActionAdminServerTrace, f.lastCheck().Actions[0])

	events, _ := parseSSE(t, rec.Body.String())
	require.Len(t, events, 3, rec.Body.String())
	assert.Equal(t, "trace", events[0].name)
	var got admin.TraceEventResponse
	require.NoError(t, json.Unmarshal([]byte(events[0].data), &got))
	assert.Equal(t, admin.TraceEventResponse{Time: now, Node: "n1", Type: "s3", API: "PutObject", Method: http.MethodPut, Path: "/b/k",
		Query: "X-Amz-Signature=REDACTED", Status: 200, DurationMs: 1.5, InputBytes: 5, RequestID: "r1", ClientIP: "10.0.0.1",
		User: "alice", Bucket: "b"}, got)
	// The losses are reported once, after the event that revealed them.
	assert.Equal(t, sseEvent{name: "dropped", data: `{"dropped":3}`}, events[1])
	assert.Equal(t, "trace", events[2].name)
	assert.Contains(t, events[2].data, `"error":"boom"`)
}

func TestTraceStreamHeartbeat(t *testing.T) {
	f := newFixture(t)
	c := f.root(t)
	sub := tracemock.NewSubscription(t)
	sub.EXPECT().Events().Return(make(chan trace.Event))
	f.tracer.EXPECT().Subscribe(mock.Anything, trace.Filter{}).Return(sub, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	rec := c.serve(c.newRequest(http.MethodGet, "/trace", nil).WithContext(ctx))
	require.Equal(t, http.StatusOK, rec.Code)
	_, comments := parseSSE(t, rec.Body.String())
	assert.Contains(t, comments, "heartbeat", "an idle stream sends heartbeats")
}

func TestTraceBadFilters(t *testing.T) {
	f := newFixture(t)
	c := f.root(t)
	for _, q := range []string{"status=abc", "status=7xx", "errors=maybe", "threshold=soon"} {
		t.Run(q, func(t *testing.T) {
			rec := c.do(http.MethodGet, "/trace?"+q, nil)
			assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		})
	}
	f.tracer.EXPECT().Subscribe(mock.Anything, mock.Anything).Return(nil, fmt.Errorf("%w: unknown type", domain.ErrInvalidArgument)).Once()
	rec := c.do(http.MethodGet, "/trace?type=nope", nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestTraceDenied(t *testing.T) {
	f := newFixture(t)
	c := f.root(t)
	f.denyAll()
	rec := c.do(http.MethodGet, "/trace", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	rec = c.do(http.MethodGet, "/logs", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	rec = c.do(http.MethodGet, "/audit", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestLogsStream(t *testing.T) {
	f := newFixture(t)
	c := f.root(t)
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	ch := make(chan logs.Entry, 1)
	ch <- logs.Entry{Time: now, Node: "n1", Level: "WARN", Message: "drive slow", Attrs: map[string]string{"drive": "/d1"}}
	close(ch)
	sub := logsmock.NewSubscription(t)
	sub.EXPECT().Entries().Return(ch)
	sub.EXPECT().Dropped().Return(uint64(0))
	f.logs.EXPECT().Subscribe(mock.Anything, logs.Filter{Nodes: []string{"n1"}, Level: "warn", Last: 20}).Return(sub, nil)

	rec := c.do(http.MethodGet, "/logs?node=n1&level=warn&last=20", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, policy.ActionAdminConsoleLog, f.lastCheck().Actions[0])
	events, _ := parseSSE(t, rec.Body.String())
	require.Len(t, events, 1)
	assert.Equal(t, "log", events[0].name)
	var got admin.LogEntryResponse
	require.NoError(t, json.Unmarshal([]byte(events[0].data), &got))
	assert.Equal(t, admin.LogEntryResponse{Time: now, Node: "n1", Level: "WARN", Message: "drive slow", Attrs: map[string]string{"drive": "/d1"}}, got)

	rec = c.do(http.MethodGet, "/logs?last=-1", nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestAuditSearch(t *testing.T) {
	f := newFixture(t)
	c := f.root(t)
	since := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	until := since.Add(time.Hour)
	entry := audit.Entry{Time: since.Add(time.Minute), Node: "n1", Type: "s3", RequestID: "r1", RemoteHost: "10.0.0.1",
		AccessKey: "alice", ReqQuery: map[string]string{"versionId": "v1"}}
	entry.API.Name, entry.API.Method, entry.API.Path = "PutObject", http.MethodPut, "/b/k"
	entry.API.Bucket, entry.API.Object, entry.API.StatusCode = "b", "k", 200
	entry.API.TimeToResponse, entry.API.InputBytes, entry.API.OutputBytes = 2*time.Millisecond, 10, 0
	f.audit.EXPECT().Search(mock.Anything, audit.Query{Nodes: []string{"n1"}, API: "PutObject", Bucket: "b", User: "alice", Type: "s3",
		StatusClasses: []int{2}, Since: since, Until: until, RequestID: "r1", Limit: 5}).Return([]audit.Entry{entry}, nil)

	rec := c.do(http.MethodGet, "/audit?node=n1&api=PutObject&bucket=b&user=alice&type=s3&status=2xx&since="+since.Format(time.RFC3339)+
		"&until="+until.Format(time.RFC3339)+"&requestId=r1&limit=5", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, policy.ActionAdminConsoleLog, f.lastCheck().Actions[0])
	got := decode[admin.AuditListResponse](t, rec)
	require.Len(t, got.Items, 1)
	assert.Equal(t, admin.AuditEntryResponse{Time: entry.Time, Node: "n1", Type: "s3", RequestID: "r1", API: "PutObject", Method: http.MethodPut,
		Path: "/b/k", Bucket: "b", Object: "k", StatusCode: 200, DurationMs: 2, RX: 10, RemoteHost: "10.0.0.1", AccessKey: "alice",
		Query: map[string]string{"versionId": "v1"}}, got.Items[0])

	f.audit.EXPECT().Search(mock.Anything, audit.Query{}).Return(nil, nil).Once()
	rec = c.do(http.MethodGet, "/audit", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"items":[]}`, rec.Body.String(), "an empty list, not null")

	for _, q := range []string{"since=yesterday", "limit=x", "errors=2"} {
		rec = c.do(http.MethodGet, "/audit?"+q, nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code, q)
	}
}

func TestMetricRoutes(t *testing.T) {
	f := newFixture(t)
	c := f.root(t)
	info := metrics.SeriesInfo{Name: "s3_requests", Description: "S3 requests per second", Unit: "req/s", Aggregation: "sum"}
	f.history.EXPECT().Metrics(mock.Anything).Return([]metrics.SeriesInfo{info})
	rec := c.do(http.MethodGet, "/metrics", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, admin.MetricListResponse{Items: []admin.MetricInfoResponse{{Name: "s3_requests", Description: "S3 requests per second",
		Unit: "req/s", Aggregation: "sum"}}}, decode[admin.MetricListResponse](t, rec))

	start := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	end := start.Add(time.Minute)
	f.history.EXPECT().Query(mock.Anything, metrics.HistoryQuery{Metric: "s3_requests", Range: 30 * time.Minute, Step: time.Minute,
		End: end, Nodes: []string{"n1"}}).Return(&metrics.HistoryResult{Metric: info, Start: start, End: end, Step: 30 * time.Second,
		Nodes:   []metrics.NodeSeries{{Node: "n1", Values: []float64{1, math.NaN()}}, {Node: "n2", Error: "unreachable"}},
		Cluster: []float64{1, math.NaN()}}, nil)
	rec = c.do(http.MethodGet, "/metrics/query?metric=s3_requests&range=30m&step=1m&node=n1&end="+end.Format(time.RFC3339), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := decode[admin.MetricSeriesResponse](t, rec)
	assert.Equal(t, int64(30), got.StepSeconds)
	require.Len(t, got.Nodes, 2)
	require.Len(t, got.Nodes[0].Points, 2)
	assert.Equal(t, start.Unix(), got.Nodes[0].Points[0].T)
	assert.Equal(t, start.Add(30*time.Second).Unix(), got.Nodes[0].Points[1].T)
	require.NotNil(t, got.Nodes[0].Points[0].V)
	assert.InDelta(t, 1, *got.Nodes[0].Points[0].V, 1e-9)
	assert.Nil(t, got.Nodes[0].Points[1].V, "a missing sample is null")
	assert.Equal(t, "unreachable", got.Nodes[1].Error)
	assert.NotNil(t, got.Nodes[1].Points, "an empty list, not null")
	assert.Contains(t, rec.Body.String(), `"v":null`)

	rec = c.do(http.MethodGet, "/metrics/query", nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	rec = c.do(http.MethodGet, "/metrics/query?metric=x&range=forever", nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	f.history.EXPECT().Query(mock.Anything, metrics.HistoryQuery{Metric: "nope"}).Return(nil, fmt.Errorf("%w: unknown metric", domain.ErrNotFound))
	rec = c.do(http.MethodGet, "/metrics/query?metric=nope", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestMetricRoutesAnyOf(t *testing.T) {
	tests := []struct {
		name   string
		denied []policy.Action
		want   int
	}{
		{"both allowed", nil, http.StatusOK},
		{"prometheus only", []policy.Action{policy.ActionAdminServerInfo}, http.StatusOK},
		{"server info only", []policy.Action{policy.ActionAdminPrometheus}, http.StatusOK},
		{"neither", []policy.Action{policy.ActionAdminPrometheus, policy.ActionAdminServerInfo}, http.StatusForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			c := f.root(t)
			f.history.EXPECT().Metrics(mock.Anything).Return(nil).Maybe()
			f.denyActions(tc.denied...)
			rec := c.do(http.MethodGet, "/metrics", nil)
			require.Equal(t, tc.want, rec.Code, rec.Body.String())
			if tc.want == http.StatusForbidden {
				assert.Contains(t, problemOf(t, rec).Detail, string(policy.ActionAdminPrometheus))
			}
		})
	}
}

func TestObservabilityRoutesNotConfigured(t *testing.T) {
	f := newFixture(t)
	routes, err := admin.NewObservabilityRoutes(admin.ObservabilityDeps{Authz: f.authz})
	require.NoError(t, err)
	specs := admin.RouteSpecs(routes)
	require.Len(t, specs, 5)
	_, err = admin.NewObservabilityRoutes(admin.ObservabilityDeps{})
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
}
