package commands_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// traceEvents are requests of two nodes: an upload, a failed download, a
// slow listing and an internode call.
func traceEvents() []nexoadmin.TraceEvent {
	at := t0.Add(time.Minute)
	return []nexoadmin.TraceEvent{
		{Time: at, Node: "n1:9000", Type: "s3", API: "PutObject", Method: http.MethodPut, Path: "/photos/a.jpg", Status: 200,
			DurationMs: 12.5, InputBytes: 2048, RequestID: "r1", ClientIP: "10.0.0.1", User: "alice", Bucket: "photos"},
		{Time: at.Add(time.Second), Node: "n2:9000", Type: "s3", API: "GetObject", Method: http.MethodGet, Path: "/photos/b.jpg",
			Query: "X-Amz-Signature=*REDACTED*", Status: 503, DurationMs: 0.42, RequestID: "r2", Bucket: "photos", Error: "SlowDown"},
		{Time: at.Add(2 * time.Second), Node: "n1:9000", Type: "admin", API: "GET /buckets", Method: http.MethodGet, Path: "/api/v1/buckets",
			Status: 200, DurationMs: 1500, RequestID: "r3", User: "root"},
		{Time: at.Add(3 * time.Second), Node: "n2:9000", Type: "rpc", API: "/storage/v1/read", Method: http.MethodPost, Path: "/rpc/storage/v1/read",
			Status: 200, DurationMs: 3},
	}
}

// covers: C-ADM-08, S-OPS-07
func TestAdminTrace(t *testing.T) {
	h := newHarness(t)
	h.admin.PublishTrace(traceEvents()...)

	s := h.session()
	s.run("admin", "trace", "--count", "3", "mem")
	golden(t, "admin/trace", s.b.String())

	// The internode calls are shown when asked; JSON wraps each request.
	h.admin.PublishTrace(traceEvents()...)
	r := h.mustRun("admin", "trace", "--type", "rpc", "--count", "1", "--json", "mem")
	recs := jsonLines(t, r.stdout)
	require.Len(t, recs, 1)
	assert.Equal(t, "mem", recs[0]["alias"])
	tr := recs[0]["trace"].(map[string]any)
	assert.Equal(t, "rpc", tr["type"])
	assert.Equal(t, "/storage/v1/read", tr["api"])
}

// covers: C-ADM-08
func TestAdminTraceFilters(t *testing.T) {
	h := newHarness(t)
	h.admin.PublishTrace(traceEvents()...)
	r := h.mustRun("admin", "trace", "--errors", "--status", "5XX,404", "--api", "GetObject,PutObject", "--bucket", "photos",
		"--node", "n1:9000,n2:9000", "--type", "S3", "--min-duration", "100us", "--count", "1", "mem")
	assert.Contains(t, r.stdout, "GetObject")
	assert.NotContains(t, r.stdout, "PutObject")
	opts := h.admin.TraceOptions()
	require.Len(t, opts, 1)
	assert.Equal(t, nexoadmin.TraceOptions{
		Nodes: []string{"n1:9000", "n2:9000"}, Types: []string{"s3"}, APIs: []string{"GetObject", "PutObject"},
		Buckets: []string{"photos"}, Statuses: []string{"5xx", "404"}, ErrorsOnly: true, Threshold: 100 * time.Microsecond,
	}, opts[0])

	tests := []struct {
		name string
		args []string
		code int
	}{
		{"invalid status", []string{"admin", "trace", "--status", "abc", "mem"}, cli.ExitUsage},
		{"status out of range", []string{"admin", "trace", "--status", "6xx", "mem"}, cli.ExitUsage},
		{"invalid type", []string{"admin", "trace", "--type", "ftp", "mem"}, cli.ExitUsage},
		{"negative duration", []string{"admin", "trace", "--min-duration", "-1s", "mem"}, cli.ExitUsage},
		{"negative count", []string{"admin", "trace", "--count", "-1", "mem"}, cli.ExitUsage},
		{"bucket instead of alias", []string{"admin", "trace", "mem/photos"}, cli.ExitUsage},
		{"unknown alias", []string{"admin", "trace", "nope"}, cli.ExitNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := h.run(tc.args...)
			assert.Equal(t, tc.code, r.code, "stdout: %s\nstderr: %s", r.stdout, r.stderr)
		})
	}
}

// covers: C-ADM-08, TCL-E-17
func TestAdminTraceReconnects(t *testing.T) {
	h := newHarness(t)
	h.deps.ReconnectDelay = 5 * time.Millisecond
	events := traceEvents()
	// The first stream ends after the first request, as when the server
	// restarts; the command opens it again and shows the next request.
	h.admin.EndStreams(1)
	h.admin.PublishTrace(events[0])

	done := make(chan result, 1)
	go func() { done <- h.run("admin", "trace", "--count", "2", "mem") }()
	require.Eventually(t, func() bool { return h.admin.StreamsOpened() == 2 }, 5*time.Second, 5*time.Millisecond)
	h.admin.PublishTrace(events[2])

	select {
	case r := <-done:
		require.Equal(t, 0, r.code, "stderr: %s", r.stderr)
		assert.Contains(t, r.stdout, "PutObject")
		assert.Contains(t, r.stdout, "GET /buckets")
		assert.Contains(t, r.stderr, "the trace stream ended (EOF): reconnecting in 5ms")
	case <-time.After(10 * time.Second):
		t.Fatal("admin trace did not stop after --count")
	}
}

// covers: C-ADM-08
func TestAdminTraceStops(t *testing.T) {
	h := newHarness(t)

	// --timeout ends an idle stream with success.
	r := h.mustRun("admin", "trace", "--timeout", "30ms", "mem")
	assert.Contains(t, r.stdout, `Tracing the requests of "mem" (Ctrl-C to stop).`)

	// Ctrl-C (the context canceled) too.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan result, 1)
	go func() { done <- h.runContext(ctx, "admin", "trace", "mem") }()
	require.Eventually(t, func() bool { return h.admin.StreamsOpened() == 2 }, 5*time.Second, 5*time.Millisecond)
	cancel()
	select {
	case r := <-done:
		assert.Equal(t, 0, r.code, "stderr: %s", r.stderr)
	case <-time.After(10 * time.Second):
		t.Fatal("admin trace did not stop on Ctrl-C")
	}

	// The server reports the requests the client did not keep up with.
	h.admin.SetDropped(7)
	h.admin.PublishTrace(traceEvents()[0])
	r = h.mustRun("admin", "trace", "--count", "1", "mem")
	assert.Contains(t, r.stderr, "the server dropped 7 trace item(s)")
}

// covers: C-ADM-08
func TestAdminTraceFailures(t *testing.T) {
	h := newHarness(t)
	h.admin.AddUser("alice", "alice-secret", "readwrite")
	h.admin.AddUser("diag", "diag-secret-1", "diagnostics")
	h.mustRun("alias", "set", "alice", "http://s3.mem.test", "alice", "alice-secret")
	h.mustRun("alias", "set", "diag", "http://s3.mem.test", "diag", "diag-secret-1")

	r := h.run("admin", "trace", "alice")
	assert.Equal(t, cli.ExitAccess, r.code, "stderr: %s", r.stderr)
	assert.Contains(t, r.stderr, "admin:ServerTrace")

	h.admin.PublishTrace(traceEvents()[0])
	h.mustRun("admin", "trace", "--count", "1", "diag")

	// A failure of the first open is reported, not retried.
	h.admin.FailOn("Trace", cli.MapAdminError(&nexoadmin.APIError{Status: http.StatusServiceUnavailable, Code: "unavailable", Detail: "down"}, "http://admin.mem.test"))
	before := h.admin.Calls("Trace")
	r = h.run("admin", "trace", "mem")
	assert.Equal(t, cli.ExitError, r.code)
	assert.Equal(t, before+1, h.admin.Calls("Trace"))
}

// covers: C-ADM-08
func TestAdminLogs(t *testing.T) {
	h := newHarness(t)
	h.deps.ReconnectDelay = 5 * time.Millisecond
	at := t0.Add(time.Minute)
	h.admin.PublishLogs(
		nexoadmin.LogEntry{Time: at, Node: "n1:9000", Level: "INFO", Message: "server started", Attrs: map[string]string{"addr": ":9000", "mode": "cluster"}},
		nexoadmin.LogEntry{Time: at.Add(time.Second), Node: "n2:9000", Level: "DEBUG", Message: "scanner cycle"},
		nexoadmin.LogEntry{Time: at.Add(2 * time.Second), Node: "n2:9000", Level: "ERROR", Message: "drive offline", Attrs: map[string]string{"drive": "/data 2", "err": ""}},
	)
	s := h.session()
	s.run("admin", "logs", "--level", "INFO", "--last", "50", "--count", "2", "mem")
	golden(t, "admin/logs", s.b.String())

	opts := h.admin.LogOptions()
	require.Len(t, opts, 1)
	assert.Equal(t, nexoadmin.LogOptions{Level: "info", Last: 50}, opts[0])

	// The replay is asked only once: a reconnection shows the new entries.
	h.admin.EndStreams(1)
	h.admin.PublishLogs(nexoadmin.LogEntry{Time: at, Node: "n1:9000", Level: "WARN", Message: "slow drive"})
	done := make(chan result, 1)
	go func() { done <- h.run("admin", "logs", "--node", "n1:9000", "--count", "2", "--json", "mem") }()
	require.Eventually(t, func() bool { return len(h.admin.LogOptions()) == 3 }, 5*time.Second, 5*time.Millisecond)
	h.admin.PublishLogs(nexoadmin.LogEntry{Time: at, Node: "n1:9000", Level: "INFO", Message: "back"})
	r := <-done
	require.Equal(t, 0, r.code, "stderr: %s", r.stderr)
	recs := jsonLines(t, r.stdout)
	require.Len(t, recs, 2)
	assert.Equal(t, "slow drive", recs[0]["log"].(map[string]any)["message"])
	assert.Equal(t, "back", recs[1]["log"].(map[string]any)["message"])
	opts = h.admin.LogOptions()
	assert.Equal(t, 10, opts[1].Last, "the default replay")
	assert.Equal(t, 0, opts[2].Last, "no replay after a reconnection")
	assert.Equal(t, []string{"n1:9000"}, opts[2].Nodes)

	for _, args := range [][]string{
		{"admin", "logs", "--level", "trace", "mem"},
		{"admin", "logs", "--last", "10001", "mem"},
		{"admin", "logs", "--timeout", "-1s", "mem"},
	} {
		r := h.run(args...)
		assert.Equal(t, cli.ExitUsage, r.code, "%v: %s", args, r.stderr)
	}
}

// auditEntries are records of two users, oldest first.
func auditEntries() []nexoadmin.AuditEntry {
	return []nexoadmin.AuditEntry{
		{Time: t0.Add(-3 * time.Hour), Node: "n1:9000", Type: "s3", RequestID: "a1", API: "PutObject", Bucket: "photos", Object: "a.jpg",
			StatusCode: 200, DurationMs: 8, AccessKey: "alice"},
		{Time: t0.Add(-30 * time.Minute), Node: "n2:9000", Type: "s3", RequestID: "a2", API: "DeleteObject", Bucket: "photos", Object: "b.jpg",
			StatusCode: 403, DurationMs: 0.5, AccessKey: "svc-1", ParentUser: "bob", Error: "AccessDenied"},
		{Time: t0.Add(-10 * time.Minute), Node: "n1:9000", Type: "admin", RequestID: "a3", API: "CreateUser", Path: "/api/v1/users",
			StatusCode: 201, DurationMs: 25, AccessKey: "root"},
	}
}

// covers: C-ADM-08, S-OPS-06
func TestAdminAudit(t *testing.T) {
	h := newHarness(t)
	h.admin.AddAudit(auditEntries()...)

	s := h.session()
	s.run("admin", "audit", "mem")
	s.run("admin", "audit", "--user", "bob", "--since", "1h", "mem")
	s.run("admin", "audit", "--action", "PutObject", "--since", "2026-01-01", "--until", "2026-01-02T01:00:00Z", "--json", "mem")
	s.run("admin", "audit", "--user", "nobody", "mem")
	golden(t, "admin/audit", s.b.String())

	q := h.admin.AuditQueries()
	require.Len(t, q, 4)
	assert.Equal(t, t0.Add(-time.Hour), q[1].Since, "a duration before now")
	assert.Equal(t, "bob", q[1].User)
	assert.Equal(t, "PutObject", q[2].API)
	assert.Equal(t, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), q[2].Since)

	h.mustRun("admin", "audit", "--errors", "--status", "4xx", "--bucket", "photos", "--type", "S3", "--node", "n2:9000",
		"--request-id", "a2", "--limit", "5", "mem")
	q = h.admin.AuditQueries()
	assert.Equal(t, nexoadmin.AuditQuery{Nodes: []string{"n2:9000"}, Bucket: "photos", Type: "s3", Statuses: []string{"4xx"},
		ErrorsOnly: true, RequestID: "a2", Limit: 5}, q[len(q)-1])

	tests := []struct {
		name string
		args []string
	}{
		{"invalid since", []string{"admin", "audit", "--since", "yesterday", "mem"}},
		{"until before since", []string{"admin", "audit", "--since", "1h", "--until", "2h", "mem"}},
		{"invalid type", []string{"admin", "audit", "--type", "rpc", "mem"}},
		{"limit too high", []string{"admin", "audit", "--limit", "5000", "mem"}},
		{"invalid status", []string{"admin", "audit", "--status", "2x", "mem"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := h.run(tc.args...)
			assert.Equal(t, cli.ExitUsage, r.code, "stderr: %s", r.stderr)
		})
	}

	h.admin.AddUser("alice", "alice-secret", "readwrite")
	h.mustRun("alias", "set", "alice", "http://s3.mem.test", "alice", "alice-secret")
	r := h.run("admin", "audit", "alice")
	assert.Equal(t, cli.ExitAccess, r.code)
	assert.Contains(t, r.stderr, "admin:ConsoleLog")
}

// covers: D-MON-01, C-ADM-08
func TestAdminMetrics(t *testing.T) {
	h := newHarness(t)
	f := func(v float64) *float64 { return &v }
	start := t0.Add(-20 * time.Second)
	h.admin.SetMetricSeries(nexoadmin.MetricSeries{
		Metric: nexoadmin.MetricInfo{Name: "s3_requests", Description: "S3 requests per second", Unit: "req/s", Aggregation: "sum"},
		Start:  start, End: t0, StepSeconds: 10,
		Nodes: []nexoadmin.MetricNodeSeries{
			{Node: "n1:9000", Points: []nexoadmin.MetricPoint{{T: start.Unix(), V: f(1.5)}, {T: start.Unix() + 10, V: f(2)}, {T: start.Unix() + 20}}},
			{Node: "n2:9000", Error: "context deadline exceeded"},
		},
		Cluster: []nexoadmin.MetricPoint{{T: start.Unix(), V: f(1.5)}, {T: start.Unix() + 10, V: f(2)}, {T: start.Unix() + 20}},
	})
	h.admin.SetMetricSeries(nexoadmin.MetricSeries{
		Metric: nexoadmin.MetricInfo{Name: "s3_latency_p99", Description: "99th percentile of the S3 latency", Unit: "seconds", Aggregation: "max"},
		Start:  start, End: t0, StepSeconds: 10,
		Nodes:   []nexoadmin.MetricNodeSeries{{Node: "n1:9000", Points: []nexoadmin.MetricPoint{{T: start.Unix(), V: f(0.25)}}}},
		Cluster: []nexoadmin.MetricPoint{{T: start.Unix(), V: f(0.25)}},
	})
	h.admin.SetMetricSeries(nexoadmin.MetricSeries{
		Metric: nexoadmin.MetricInfo{Name: "drives_used", Description: "Used capacity of the drives", Unit: "bytes", Aggregation: "sum"},
		Start:  start, End: t0, StepSeconds: 10,
		Cluster: []nexoadmin.MetricPoint{{T: start.Unix(), V: f(3 << 30)}},
	})

	s := h.session()
	s.run("admin", "metrics", "list", "mem")
	s.run("admin", "metrics", "query", "mem", "s3_requests")
	s.run("admin", "metrics", "query", "--range", "6h", "--step", "5m", "--end", "30m", "mem", "s3_latency_p99")
	s.run("admin", "metrics", "query", "mem", "drives_used")
	s.run("admin", "metrics", "query", "mem", "nope")
	golden(t, "admin/metrics", s.b.String())

	q := h.admin.MetricQueries()
	require.Len(t, q, 4)
	assert.Equal(t, nexoadmin.MetricQuery{Metric: "s3_latency_p99", Range: 6 * time.Hour, Step: 5 * time.Minute, End: t0.Add(-30 * time.Minute)}, q[1])

	r := h.mustRun("admin", "metrics", "query", "--node", "n1:9000", "--json", "mem", "s3_requests")
	recs := jsonLines(t, r.stdout)
	require.Len(t, recs, 1)
	assert.Equal(t, "s3_requests", recs[0]["metric"].(map[string]any)["name"])
	assert.Len(t, recs[0]["nodes"], 1)

	for _, args := range [][]string{
		{"admin", "metrics", "query", "--range", "-1h", "mem", "s3_requests"},
		{"admin", "metrics", "query", "--end", "soon", "mem", "s3_requests"},
		{"admin", "metrics", "query", "mem"},
	} {
		r := h.run(args...)
		assert.Equal(t, cli.ExitUsage, r.code, "%v: %s", args, r.stderr)
	}
	r = h.run("admin", "metrics", "query", "--range", "48h", "mem", "s3_requests")
	assert.Equal(t, cli.ExitError, r.code, "the server rejects the range: %s", r.stderr)
	assert.Contains(t, r.stderr, "range must be at most 24h0m0s")
}

// covers: C-ADM-12
func TestAdminPrometheusGenerate(t *testing.T) {
	h := newHarness(t)
	s := h.session()
	s.run("admin", "prometheus", "generate", "mem")
	s.run("admin", "prometheus", "generate", "--job", "nexo prod", "--expiry", "30d", "mem", "BUCKET")
	s.run("admin", "prometheus", "generate", "--public", "mem", "node")
	golden(t, "admin/prometheus", s.b.String())

	// A public endpoint takes no token.
	r := h.mustRun("admin", "prometheus", "generate", "--public", "--json", "mem")
	pub := jsonLines(t, r.stdout)
	require.Len(t, pub, 1)
	assert.NotContains(t, pub[0], "token")
	assert.NotContains(t, pub[0], "expiresAt")
	assert.NotContains(t, pub[0]["config"], "authorization")

	r = h.mustRun("admin", "prometheus", "generate", "--json", "mem", "node")
	recs := jsonLines(t, r.stdout)
	require.Len(t, recs, 1)
	rec := recs[0]
	assert.Equal(t, "node", rec["type"])
	assert.Equal(t, "/nexo/metrics/v1/node", rec["metricsPath"])
	assert.Equal(t, "s3.mem.test", rec["target"])
	assert.Equal(t, t0.AddDate(1, 0, 0).Format(time.RFC3339), rec["expiresAt"])

	// The token is the one the server verifies: HS512, signed with the
	// secret key, subject the access key, issuer prometheus.
	token, err := jwt.ParseWithClaims(rec["token"].(string), &jwt.RegisteredClaims{}, func(tok *jwt.Token) (any, error) {
		assert.Equal(t, jwt.SigningMethodHS512.Alg(), tok.Method.Alg())
		return []byte("MEMSECRET"), nil
	}, jwt.WithTimeFunc(func() time.Time { return t0 }), jwt.WithIssuer("prometheus"), jwt.WithSubject("MEMKEY"))
	require.NoError(t, err)
	assert.True(t, token.Valid)
	assert.Contains(t, rec["config"], "credentials: \""+rec["token"].(string)+"\"")

	tests := []struct {
		name string
		args []string
		code int
	}{
		{"unknown kind", []string{"admin", "prometheus", "generate", "mem", "drives"}, cli.ExitUsage},
		{"expired", []string{"admin", "prometheus", "generate", "--expiry", "2020-01-01", "mem"}, cli.ExitUsage},
		{"public with an expiry", []string{"admin", "prometheus", "generate", "--public", "--expiry", "30d", "mem"}, cli.ExitUsage},
		{"bucket instead of alias", []string{"admin", "prometheus", "generate", "mem/photos"}, cli.ExitUsage},
		{"unknown alias", []string{"admin", "prometheus", "generate", "nope"}, cli.ExitNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := h.run(tc.args...)
			assert.Equal(t, tc.code, r.code, "stdout: %s\nstderr: %s", r.stdout, r.stderr)
		})
	}
}

// covers: S-OPS-11, C-ADM-12
func TestAdminProfileSeconds(t *testing.T) {
	h := newHarness(t)
	r := h.mustRun("admin", "profile", "--type", "mutex", "--seconds", "3", "--output", "mutex.out", "mem")
	assert.Contains(t, r.stdout, "Sampling the mutex profile of \"mem\" for 3s")
	assert.Equal(t, "pprof:mutex", h.read("mutex.out"))

	for _, args := range [][]string{
		{"admin", "profile", "--seconds", "3", "--duration", "3s", "mem"},
		{"admin", "profile", "--seconds", "-1", "mem"},
	} {
		r := h.run(args...)
		assert.Equal(t, cli.ExitUsage, r.code, "%v: %s", args, r.stderr)
	}
}

// covers: C-ADM-08
func TestAdminTraceJSONIsJSONLines(t *testing.T) {
	h := newHarness(t)
	h.admin.PublishTrace(traceEvents()...)
	r := h.mustRun("admin", "trace", "--count", "3", "--json", "mem")
	for _, line := range strings.Split(strings.TrimSpace(r.stdout), "\n") {
		var v struct {
			Status string               `json:"status"`
			Alias  string               `json:"alias"`
			Trace  nexoadmin.TraceEvent `json:"trace"`
		}
		require.NoError(t, json.Unmarshal([]byte(line), &v))
		assert.Equal(t, "success", v.Status)
		assert.NotEmpty(t, v.Trace.RequestID)
	}
}
