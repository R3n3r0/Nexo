package cache_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/service/cache"
	"github.com/r3n3r0/nexo/internal/service/distribution"
	"github.com/r3n3r0/nexo/internal/service/metrics"
	"github.com/r3n3r0/nexo/internal/service/object"
)

// waitWarm waits for the end of a warm-up.
func waitWarm(t *testing.T, s cache.Service, id string) cache.WarmJob {
	t.Helper()
	var job cache.WarmJob
	require.Eventually(t, func() bool {
		var err error
		job, err = s.GetWarmJob(context.Background(), "web", id)
		require.NoError(t, err)
		return job.State != cache.WarmRunning
	}, timeout, tick)
	return job
}

// TestWarmPrefix warms every object below a prefix (docs/11 §11.6): the
// first requests are HITs, the origin is not read again.
func TestWarmPrefix(t *testing.T) {
	e := newEnv(t, envOptions{})
	var total int64
	for _, k := range []string{"assets/a.js", "assets/b.css", "assets/sub/c.png"} {
		data := payload(4<<10, byte(len(k)))
		total += int64(len(data))
		e.origin.put("web", k, data)
	}
	e.origin.put("web", "other.html", []byte("other"))
	job, err := e.svc.Warm(context.Background(), cache.WarmRequest{Distribution: "web", Prefix: "/assets/"})
	require.NoError(t, err)
	assert.Equal(t, "www.example.com", job.Request.Domain, "the first exact alias")
	job = waitWarm(t, e.svc, job.ID)
	assert.Equal(t, cache.WarmCompleted, job.State)
	require.Len(t, job.Nodes, 1)
	assert.Equal(t, 3, job.Nodes[0].Total)
	assert.Equal(t, 3, job.Nodes[0].Done)
	assert.Zero(t, job.Nodes[0].Failed)
	assert.Equal(t, total, job.Nodes[0].Bytes)
	reads := e.origin.gets.Load()
	for _, p := range []string{"/assets/a.js", "/assets/b.css", "/assets/sub/c.png"} {
		assert.Equal(t, "HIT", e.get("www.example.com", p).xcache(), p)
	}
	assert.Equal(t, reads, e.origin.gets.Load())
	assert.Equal(t, "MISS", e.get("www.example.com", "/other.html").xcache())
}

// TestWarmPathsAndPattern warms listed paths, URLs and the objects of a
// pattern; a missing one is reported.
func TestWarmPathsAndPattern(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.origin.put("web", "index.html", []byte("home"))
	e.origin.put("web", "a.html", []byte("a"))
	e.origin.put("web", "img/x.png", []byte("x"))
	e.origin.put("web", "img/y.jpg", []byte("y"))
	job, err := e.svc.Warm(context.Background(), cache.WarmRequest{Distribution: "web", Domain: "example.com",
		Paths: []string{"/", "/missing.html"}, URLs: []string{"https://example.com/a.html"},
		Pattern: &distribution.PathPattern{Type: distribution.PatternGlob, Pattern: "/img/*.png"}})
	require.NoError(t, err)
	job = waitWarm(t, e.svc, job.ID)
	assert.Equal(t, cache.WarmCompleted, job.State)
	assert.Equal(t, 4, job.Nodes[0].Total)
	assert.Equal(t, 1, job.Nodes[0].Failed)
	assert.Contains(t, job.Nodes[0].Error, "/missing.html")
	for _, p := range []string{"/", "/a.html", "/img/x.png"} {
		assert.Equal(t, "HIT", e.get("example.com", p).xcache(), p)
	}
	assert.Equal(t, "MISS", e.get("example.com", "/img/y.jpg").xcache())
}

// TestWarmValidation refuses the warm-ups that cannot run.
func TestWarmValidation(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	tests := []struct {
		name string
		req  cache.WarmRequest
	}{
		{"nothing", cache.WarmRequest{Distribution: "web"}},
		{"foreign domain", cache.WarmRequest{Distribution: "web", Domain: "evil.com", Paths: []string{"/"}}},
		{"wildcard domain", cache.WarmRequest{Distribution: "web", Domain: "*.preview.example.com", Paths: []string{"/"}}},
		{"url of another domain", cache.WarmRequest{Distribution: "web", Domain: "example.com", URLs: []string{"https://www.example.com/"}}},
		{"relative path", cache.WarmRequest{Distribution: "web", Paths: []string{"a.html"}}},
		{"bad pattern", cache.WarmRequest{Distribution: "web", Pattern: &distribution.PathPattern{Type: distribution.PatternRegex, Pattern: "("}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := e.svc.Warm(ctx, tt.req)
			require.ErrorIs(t, err, cache.ErrInvalid)
		})
	}
	_, err := e.svc.Warm(ctx, cache.WarmRequest{Distribution: "nope", Paths: []string{"/"}})
	require.Error(t, err)
	_, err = e.svc.GetWarmJob(ctx, "web", "warm-unknown")
	require.ErrorIs(t, err, cache.ErrWarmJobNotFound)
	_, err = e.svc.GetWarmJob(ctx, "web", "../../etc")
	require.ErrorIs(t, err, cache.ErrWarmJobNotFound)
	e.origin.put("web", "a.html", []byte("a"))
	job, err := e.svc.Warm(ctx, cache.WarmRequest{Distribution: "web", Paths: []string{"/a.html"}})
	require.NoError(t, err)
	_, err = e.svc.GetWarmJob(ctx, "other", job.ID)
	require.ErrorIs(t, err, cache.ErrWarmJobNotFound, "the job of another distribution")
	waitWarm(t, e.svc, job.ID)
}

// TestWarmBandwidth bounds the bytes a warm-up reads per second.
func TestWarmBandwidth(t *testing.T) {
	e := newEnv(t, envOptions{})
	for i := range 4 {
		e.origin.put("web", "big/"+strconv.Itoa(i), payload(32<<10, byte(i)))
	}
	start := time.Now()
	job, err := e.svc.Warm(context.Background(), cache.WarmRequest{Distribution: "web", Prefix: "/big/", Concurrency: 1, Bandwidth: 64 << 10})
	require.NoError(t, err)
	job = waitWarm(t, e.svc, job.ID)
	assert.Equal(t, cache.WarmCompleted, job.State)
	assert.Equal(t, int64(128<<10), job.Nodes[0].Bytes)
	assert.GreaterOrEqual(t, time.Since(start), time.Second, "128 KiB at 64 KiB/s")
}

// TestWarmOnWrite caches the objects of the warmOnWrite patterns as soon
// as they are written.
func TestWarmOnWrite(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.setDist(cache.DistributionConfig{Distribution: "web",
		WarmOnWrite: []distribution.PathPattern{{Type: distribution.PatternPrefix, Pattern: "/hot/"}}})
	e.origin.put("web", "hot/news.json", []byte(`{"v":1}`))
	e.origin.put("web", "cold.json", []byte(`{}`))
	require.NoError(t, e.svc.ObjectChanged(context.Background(), cache.ObjectChange{Bucket: "web", Key: "hot/news.json"}))
	require.NoError(t, e.svc.ObjectChanged(context.Background(), cache.ObjectChange{Bucket: "web", Key: "cold.json"}))
	require.Eventually(t, func() bool { return e.cached("www.example.com", "/hot/news.json") }, timeout, tick)
	require.Eventually(t, func() bool { return e.cached("example.com", "/hot/news.json") }, timeout, tick)
	assert.Equal(t, "HIT", e.get("www.example.com", "/hot/news.json").xcache())
	assert.False(t, e.cached("www.example.com", "/cold.json"))
	// A new version replaces the warmed one.
	e.origin.put("web", "hot/news.json", []byte(`{"v":2}`))
	require.NoError(t, e.svc.ObjectChanged(context.Background(), cache.ObjectChange{Bucket: "web", Key: "hot/news.json"}))
	require.Eventually(t, func() bool { return e.cached("www.example.com", "/hot/news.json") }, timeout, tick)
	r := e.get("www.example.com", "/hot/news.json")
	assert.Equal(t, `{"v":2}`, string(r.body))
}

// TestClusterWarm warms every node of a cluster in the local mode.
func TestClusterWarm(t *testing.T) {
	c := newCluster(t, 3, cache.ModeLocal)
	c.nodes[0].origin.put("web", "a.html", []byte("a"))
	job, err := c.nodes[1].svc.Warm(context.Background(), cache.WarmRequest{Distribution: "web", Paths: []string{"/a.html"}})
	require.NoError(t, err)
	job = waitWarm(t, c.nodes[0].svc, job.ID)
	assert.Equal(t, cache.WarmCompleted, job.State)
	require.Len(t, job.Nodes, 3)
	for _, n := range job.Nodes {
		assert.Equal(t, cache.WarmCompleted, n.State, n.Node)
		assert.Equal(t, 1, n.Done, n.Node)
	}
	for _, nd := range c.nodes {
		assert.Equal(t, "HIT", nd.get("www.example.com", "/a.html").xcache(), nd.node)
	}
	assert.Equal(t, int64(3), c.nodes[0].origin.gets.Load())
}

// TestInspector reports the key, the effective policy with its levels and
// the entry of every node (docs/11 §11.7).
func TestInspector(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	e.setDist(cache.DistributionConfig{Distribution: "web", Domains: []cache.DomainSettings{
		{Host: "www.example.com", Cache: cache.Settings{PolicySpec: cache.PolicySpec{TTL: &cache.TTL{Default: dur(time.Minute)}}}},
	}})
	e.origin.put("web", "index.html", []byte("home"))
	res, err := e.svc.Inspect(ctx, cache.InspectInput{URL: "https://www.example.com/"})
	require.NoError(t, err)
	assert.Equal(t, "web", res.Distribution)
	assert.Equal(t, "index.html", res.Key)
	assert.NotEmpty(t, res.CacheKey)
	assert.False(t, res.Bypass)
	assert.Equal(t, cache.ModeLocal, res.Mode)
	assert.Equal(t, cache.LevelDomain, res.Policy.Fields["ttl.default"])
	assert.Equal(t, time.Minute, res.Policy.Policy.TTL.Default.D())
	require.Len(t, res.Nodes, 1)
	assert.False(t, res.Nodes[0].Entry.Present)

	require.Equal(t, "MISS", e.get("www.example.com", "/").xcache())
	e.clk.Advance(20 * time.Second)
	res, err = e.svc.Inspect(ctx, cache.InspectInput{URL: "https://www.example.com/"})
	require.NoError(t, err)
	p := res.Nodes[0].Entry
	assert.True(t, p.Present)
	assert.Equal(t, "memory", p.Tier)
	assert.Equal(t, "object", p.Kind)
	assert.Equal(t, 20*time.Second, p.Age.D())
	assert.Equal(t, 40*time.Second, p.ExpiresIn.D())
	assert.Equal(t, cache.Size(4), p.Size)
	assert.NotEmpty(t, p.ETag)
	e.clk.Advance(time.Minute)
	res, err = e.svc.Inspect(ctx, cache.InspectInput{URL: "https://www.example.com/"})
	require.NoError(t, err)
	assert.True(t, res.Nodes[0].Entry.Stale)

	// The other domain keeps the policy of the distribution.
	res, err = e.svc.Inspect(ctx, cache.InspectInput{URL: "https://example.com/index.html"})
	require.NoError(t, err)
	assert.NotEqual(t, cache.LevelDomain, res.Policy.Fields["ttl.default"])
	// A request with credentials, a host of no distribution, a bad URL.
	res, err = e.svc.Inspect(ctx, cache.InspectInput{URL: "https://www.example.com/a?x=1",
		Header: http.Header{"Cookie": {"session=1"}}})
	require.NoError(t, err)
	assert.NotEmpty(t, res.CacheKey)
	res, err = e.svc.Inspect(ctx, cache.InspectInput{URL: "https://unknown.test/"})
	require.NoError(t, err)
	assert.True(t, res.Bypass)
	assert.Empty(t, res.Distribution)
	_, err = e.svc.Inspect(ctx, cache.InspectInput{URL: "ftp://x"})
	require.ErrorIs(t, err, cache.ErrInvalid)
}

// scrape returns the exposition of a registry.
func scrape(t *testing.T, r metrics.Registry) string {
	t.Helper()
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	return rec.Body.String()
}

// TestMetricsAndStats checks the metrics per domain of docs/11 §11.8 and
// the statistics of the node.
func TestMetricsAndStats(t *testing.T) {
	reg := metrics.New(metrics.Deps{Logger: slog.New(slog.DiscardHandler)}, metrics.WithoutRuntimeCollectors())
	e := newEnv(t, envOptions{reg: reg})
	e.origin.put("web", "a.html", payload(1000, 1))
	e.origin.put("web", "b.html", payload(500, 2))
	for range 3 {
		e.get("www.example.com", "/a.html")
	}
	e.get("example.com", "/b.html")
	_, err := e.svc.Invalidate(context.Background(), cache.InvalidationRequest{Distribution: "web", All: true})
	require.NoError(t, err)
	out := scrape(t, reg)
	for _, want := range []string{
		`nexo_cache_requests_total{distribution="web",host="www.example.com",result="HIT"} 2`,
		`nexo_cache_requests_total{distribution="web",host="www.example.com",result="MISS"} 1`,
		`nexo_cache_requests_total{distribution="web",host="example.com",result="MISS"} 1`,
		`nexo_cache_bytes_served_total{distribution="web",host="www.example.com",source="cache"} 2000`,
		`nexo_cache_origin_bytes_saved_total`,
		`nexo_cache_invalidations_total{type="manual"} 1`,
		`nexo_cache_origin_reads_total`,
		`nexo_cache_request_duration_seconds_bucket`,
		`nexo_cache_hit_ratio{distribution="web",host="www.example.com"}`,
	} {
		assert.Contains(t, out, want)
	}
	st, err := e.svc.NodeStats(context.Background(), cache.StatsQuery{Distribution: "web", Host: "www.example.com"})
	require.NoError(t, err)
	assert.Equal(t, int64(2), st.Requests[cache.ResultHit])
	assert.Equal(t, int64(1), st.Requests[cache.ResultMiss])
	assert.Equal(t, int64(2000), st.BytesCache)
	assert.Equal(t, int64(1000), st.BytesOrigin)
	assert.InDelta(t, 2.0/3, st.HitRatio, 0.001)
	require.NotEmpty(t, st.TopObjects)
	assert.Equal(t, "/a.html", st.TopObjects[0].Path)
	assert.Equal(t, int64(3), st.TopObjects[0].Requests)
	assert.Equal(t, int64(1), st.Invalidations)
	assert.Equal(t, cache.Size(cache.DefaultMemorySize), st.MemoryLimit)
}

// hitOnce serves a path from the cache as the website engine does and
// returns the body.
func hitOnce(ctx context.Context, s cache.Service, d *distribution.Distribution, path string, buf []byte) (int, string, error) {
	rctx, ex := s.Begin(ctx, cache.Request{Distribution: d, Host: "www.example.com", Method: http.MethodGet, Path: path, Header: http.Header{}})
	rd, _, err := s.GetObject(rctx, d.Origin.Bucket, strings.TrimPrefix(path, "/"), object.GetObjectOptions{})
	if err != nil {
		return 0, "", err
	}
	n, err := io.ReadFull(rd, buf)
	_ = rd.Close()
	h := http.Header{}
	ex.Decorate(http.StatusOK, h)
	ex.Finish(ctx, http.StatusOK, int64(n))
	return n, h.Get(cache.HeaderXCache), err
}

// TestHitLatency is TCC-16: a HIT of the memory tier on 64 KiB is served
// in less than 1 ms at the 99th percentile.
func TestHitLatency(t *testing.T) {
	if testing.Short() {
		t.Skip("latency measurement")
	}
	e := newEnv(t, envOptions{})
	e.origin.put("web", "obj.bin", payload(64<<10, 7))
	d := e.dists.resolve("www.example.com").Distribution
	buf := make([]byte, 64<<10)
	ctx := context.Background()
	_, _, err := hitOnce(ctx, e.svc, d, "/obj.bin", buf)
	require.NoError(t, err)
	const n = 2000
	lat := make([]time.Duration, 0, n)
	for range n {
		start := time.Now()
		got, xc, err := hitOnce(ctx, e.svc, d, "/obj.bin", buf)
		lat = append(lat, time.Since(start))
		require.NoError(t, err)
		require.Equal(t, 64<<10, got)
		require.Equal(t, "HIT", xc)
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	p99 := lat[n*99/100]
	t.Logf("HIT 64 KiB: p50 %v, p99 %v", lat[n/2], p99)
	bound := time.Millisecond
	if raceDetector {
		bound = 20 * time.Millisecond
	}
	assert.Less(t, p99, bound)
}

// BenchmarkHitMemory64KiB measures a HIT of the memory tier (TCC-16).
func BenchmarkHitMemory64KiB(b *testing.B) {
	e := newEnv(b, envOptions{})
	e.origin.put("web", "obj.bin", payload(64<<10, 7))
	d := e.dists.resolve("www.example.com").Distribution
	buf := make([]byte, 64<<10)
	ctx := context.Background()
	if _, _, err := hitOnce(ctx, e.svc, d, "/obj.bin", buf); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(64 << 10)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		local := make([]byte, 64<<10)
		for pb.Next() {
			if _, xc, err := hitOnce(ctx, e.svc, d, "/obj.bin", local); err != nil || xc != "HIT" {
				b.Errorf("hit: %v %s", err, xc)
				return
			}
		}
	})
}
