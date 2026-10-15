package cache_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/cache"
	"github.com/r3n3r0/nexo/internal/service/distribution"
	"github.com/r3n3r0/nexo/internal/service/object"
)

// TestCacheKey is TCC-03: what keys an entry (query, headers, cookies,
// host) and how the key is normalized.
func TestCacheKey(t *testing.T) {
	key := func(spec cache.PolicySpec, host, query string, hdr http.Header) string {
		p := cache.ParamsOf(spec)
		if hdr == nil {
			hdr = http.Header{}
		}
		req := &cache.Request{Host: host, RawQuery: query, Header: hdr}
		return cache.KeyOf(&p, cache.ScopeOf(&p, "web", host), "web", "a.js", req, "")
	}
	none := cache.PolicySpec{}
	all := cache.PolicySpec{CacheKey: &cache.KeyPolicy{QueryString: &cache.QueryString{Mode: cache.QueryAll}}}
	allRaw := cache.PolicySpec{CacheKey: &cache.KeyPolicy{QueryString: &cache.QueryString{Mode: cache.QueryAll}, Normalize: ptr(false)}}
	allow := cache.PolicySpec{CacheKey: &cache.KeyPolicy{QueryString: &cache.QueryString{Mode: cache.QueryAllowlist, Params: []string{"v"}}}}
	deny := cache.PolicySpec{CacheKey: &cache.KeyPolicy{QueryString: &cache.QueryString{Mode: cache.QueryDenylist, Params: []string{"session"}}}}
	lang := cache.PolicySpec{CacheKey: &cache.KeyPolicy{Headers: []string{"accept-language"}}}
	cookie := cache.PolicySpec{CacheKey: &cache.KeyPolicy{Cookies: []string{"variant"}}}
	shared := cache.PolicySpec{CacheKey: &cache.KeyPolicy{IncludeHost: ptr(false)}}
	tests := []struct {
		name  string
		a, b  string
		equal bool
	}{
		{name: "query none ignores the query", a: key(none, "h", "v=1", nil), b: key(none, "h", "v=2", nil), equal: true},
		{name: "query all keys it", a: key(all, "h", "v=1", nil), b: key(all, "h", "v=2", nil)},
		{name: "normalization sorts", a: key(all, "h", "b=2&a=1", nil), b: key(all, "h", "a=1&b=2", nil), equal: true},
		{name: "normalization lowercases the names", a: key(all, "h", "V=1", nil), b: key(all, "h", "v=1", nil), equal: true},
		{name: "normalization drops the tracking", a: key(all, "h", "v=1&utm_source=x&fbclid=y", nil), b: key(all, "h", "v=1", nil), equal: true},
		{name: "without normalization the order counts", a: key(allRaw, "h", "b=2&a=1", nil), b: key(allRaw, "h", "a=1&b=2", nil)},
		{name: "signature parameters never key", a: key(all, "h", "v=1&Expires=1&Signature=s&Key-Pair-Id=k", nil), b: key(all, "h", "v=1", nil), equal: true},
		{name: "allowlist keeps v", a: key(allow, "h", "v=1&x=1", nil), b: key(allow, "h", "v=1&x=2", nil), equal: true},
		{name: "allowlist keys v", a: key(allow, "h", "v=1", nil), b: key(allow, "h", "v=2", nil)},
		{name: "denylist drops session", a: key(deny, "h", "v=1&session=a", nil), b: key(deny, "h", "v=1&session=b", nil), equal: true},
		{name: "denylist keys the others", a: key(deny, "h", "v=1", nil), b: key(deny, "h", "v=2", nil)},
		{name: "header keys", a: key(lang, "h", "", http.Header{"Accept-Language": {"it"}}), b: key(lang, "h", "", http.Header{"Accept-Language": {"en"}})},
		{name: "header normalized", a: key(lang, "h", "", http.Header{"Accept-Language": {"IT-it"}}), b: key(lang, "h", "", http.Header{"Accept-Language": {"it-IT"}}), equal: true},
		{name: "other headers ignored", a: key(lang, "h", "", http.Header{"Accept-Language": {"it"}, "X-Other": {"1"}}), b: key(lang, "h", "", http.Header{"Accept-Language": {"it"}}), equal: true},
		{name: "cookie keys", a: key(cookie, "h", "", http.Header{"Cookie": {"variant=a; other=1"}}), b: key(cookie, "h", "", http.Header{"Cookie": {"variant=b"}})},
		{name: "other cookies ignored", a: key(cookie, "h", "", http.Header{"Cookie": {"variant=a; other=1"}}), b: key(cookie, "h", "", http.Header{"Cookie": {"variant=a; other=2"}}), equal: true},
		{name: "host keys", a: key(none, "www.example.com", "", nil), b: key(none, "example.com", "", nil)},
		{name: "includeHost false shares", a: key(shared, "www.example.com", "", nil), b: key(shared, "example.com", "", nil), equal: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.equal {
				assert.Equal(t, tc.a, tc.b)
			} else {
				assert.NotEqual(t, tc.a, tc.b)
			}
		})
	}
}

// TestCacheKeyHitMiss checks TCC-03 end to end: the requests whose keys
// match hit the entry of the first one.
func TestCacheKeyHitMiss(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.origin.put("web", "a.js", []byte("console.log(1)"), textInfo("application/javascript"))
	e.setDist(cache.DistributionConfig{Distribution: "web", Cache: cache.Settings{PolicySpec: cache.PolicySpec{
		CacheKey: &cache.KeyPolicy{QueryString: &cache.QueryString{Mode: cache.QueryAllowlist, Params: []string{"v"}}}}}})
	assert.Equal(t, "MISS", e.get("www.example.com", "/a.js", withQuery("v=1&utm_source=x")).xcache())
	assert.Equal(t, "HIT", e.get("www.example.com", "/a.js", withQuery("v=1")).xcache())
	assert.Equal(t, "HIT", e.get("www.example.com", "/a.js", withQuery("x=9&v=1")).xcache())
	assert.Equal(t, "MISS", e.get("www.example.com", "/a.js", withQuery("v=2")).xcache())
	assert.Equal(t, "MISS", e.get("example.com", "/a.js", withQuery("v=1")).xcache(), "another domain, another entry")
}

// TestFreshness is TCC-04: the lifetime of the objects under a policy.
func TestFreshness(t *testing.T) {
	std := cache.ParamsOf(cache.PolicySpec{})
	bounded := cache.ParamsOf(cache.PolicySpec{TTL: &cache.TTL{Default: dur(time.Hour), Min: dur(time.Minute), Max: dur(2 * time.Hour)}})
	ignore := cache.ParamsOf(cache.PolicySpec{RespectObjectCacheControl: ptr(false)})
	now := t0
	tests := []struct {
		name      string
		p         cache.Params
		info      domain.ObjectInfo
		ttl       time.Duration
		swr, sie  time.Duration
		cacheable bool
	}{
		{name: "default", p: std, ttl: time.Hour, swr: time.Minute, sie: 24 * time.Hour, cacheable: true},
		{name: "max-age", p: std, info: domain.ObjectInfo{CacheControl: "public, max-age=600"}, ttl: 10 * time.Minute, swr: time.Minute, sie: 24 * time.Hour, cacheable: true},
		{name: "s-maxage wins", p: std, info: domain.ObjectInfo{CacheControl: "max-age=600, s-maxage=60"}, ttl: time.Minute, swr: time.Minute, sie: 24 * time.Hour, cacheable: true},
		{name: "stale directives", p: std, info: domain.ObjectInfo{CacheControl: "max-age=60, stale-while-revalidate=5, stale-if-error=9"}, ttl: time.Minute, swr: 5 * time.Second, sie: 9 * time.Second, cacheable: true},
		{name: "expires", p: std, info: domain.ObjectInfo{Expires: now.Add(30 * time.Minute)}, ttl: 30 * time.Minute, swr: time.Minute, sie: 24 * time.Hour, cacheable: true},
		{name: "expired", p: std, info: domain.ObjectInfo{Expires: now.Add(-time.Minute)}, ttl: 0, swr: time.Minute, sie: 24 * time.Hour, cacheable: true},
		{name: "no-cache revalidates", p: std, info: domain.ObjectInfo{CacheControl: "no-cache"}, ttl: 0, swr: time.Minute, sie: 24 * time.Hour, cacheable: true},
		{name: "no-store", p: std, info: domain.ObjectInfo{CacheControl: "no-store"}},
		{name: "private", p: std, info: domain.ObjectInfo{CacheControl: "private, max-age=60"}},
		{name: "bounded below", p: bounded, info: domain.ObjectInfo{CacheControl: "max-age=1"}, ttl: time.Minute, swr: time.Minute, sie: 24 * time.Hour, cacheable: true},
		{name: "bounded above", p: bounded, info: domain.ObjectInfo{CacheControl: "max-age=86400"}, ttl: 2 * time.Hour, swr: time.Minute, sie: 24 * time.Hour, cacheable: true},
		{name: "not respected", p: ignore, info: domain.ObjectInfo{CacheControl: "no-store"}, ttl: time.Hour, swr: time.Minute, sie: 24 * time.Hour, cacheable: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ttl, swr, sie, ok := cache.FreshnessOf(&tc.p, tc.info, now)
			assert.Equal(t, tc.cacheable, ok)
			if !tc.cacheable {
				return
			}
			assert.Equal(t, tc.ttl, ttl)
			assert.Equal(t, tc.swr, swr)
			assert.Equal(t, tc.sie, sie)
		})
	}
}

// TestHitMissAgeAndExpiry checks the life of an entry with a fake clock:
// MISS, HIT with its Age, REVALIDATED when the origin confirms it,
// EXPIRED when it changed (TCC-04).
func TestHitMissAgeAndExpiry(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.origin.put("web", "page.html", []byte("v1"), textInfo("text/html"), cacheControl("max-age=60"))
	e.setDist(cache.DistributionConfig{Distribution: "web", Cache: cache.Settings{PolicySpec: cache.PolicySpec{StaleWhileRevalidate: dur(0)}}})
	r := e.get("www.example.com", "/page.html")
	assert.Equal(t, "MISS", r.xcache())
	assert.Equal(t, "v1", string(r.body))
	assert.Empty(t, r.header.Get("Age"))
	e.clk.Advance(30 * time.Second)
	r = e.get("www.example.com", "/page.html")
	assert.Equal(t, "HIT", r.xcache())
	assert.Equal(t, "30", r.header.Get("Age"))
	assert.Equal(t, int64(1), e.origin.gets.Load(), "one read of the origin")
	e.clk.Advance(time.Minute)
	r = e.get("www.example.com", "/page.html")
	assert.Equal(t, "REVALIDATED", r.xcache())
	assert.Equal(t, "0", r.header.Get("Age"))
	assert.Equal(t, int64(1), e.origin.gets.Load(), "revalidated without reading the content")
	// A change the cache did not hear of (no event): found at the expiry.
	e.origin.put("web", "page.html", []byte("v2"), textInfo("text/html"), cacheControl("max-age=60"))
	e.clk.Advance(2 * time.Minute)
	r = e.get("www.example.com", "/page.html")
	assert.Equal(t, "EXPIRED", r.xcache())
	assert.Equal(t, "v2", string(r.body))
	assert.Equal(t, "HIT", e.get("www.example.com", "/page.html").xcache())
}

// TestStaleWhileRevalidateAndIfError is TCC-07: an expired entry is served
// stale while it is refreshed, and when the origin has no quorum.
//
// covers: S-DST-07c
func TestStaleWhileRevalidateAndIfError(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.origin.put("web", "a.txt", []byte("v1"), textInfo("text/plain"), cacheControl("max-age=10"))
	e.setDist(cache.DistributionConfig{Distribution: "web", Cache: cache.Settings{PolicySpec: cache.PolicySpec{
		StaleWhileRevalidate: dur(time.Minute), StaleIfError: dur(time.Hour)}}})
	require.Equal(t, "MISS", e.get("www.example.com", "/a.txt").xcache())
	// Within stale-while-revalidate: served stale, refreshed in the
	// background.
	e.origin.put("web", "a.txt", []byte("v2"), textInfo("text/plain"), cacheControl("max-age=10"))
	e.clk.Advance(30 * time.Second)
	r := e.get("www.example.com", "/a.txt")
	assert.Equal(t, "STALE", r.xcache())
	assert.Equal(t, "v1", string(r.body))
	require.Eventually(t, func() bool {
		r := e.get("www.example.com", "/a.txt")
		return r.xcache() == "HIT" && string(r.body) == "v2"
	}, 5*time.Second, 10*time.Millisecond, "refreshed in the background")
	// Beyond it, the origin fails: stale-if-error.
	e.clk.Advance(10 * time.Minute)
	e.origin.setFail(domain.ErrReadQuorum)
	r = e.get("www.example.com", "/a.txt")
	require.NoError(t, r.err)
	assert.Equal(t, "STALE", r.xcache())
	assert.Equal(t, "v2", string(r.body))
	// Beyond stale-if-error, the error of the origin.
	e.clk.Advance(2 * time.Hour)
	r = e.get("www.example.com", "/a.txt")
	require.ErrorIs(t, r.err, domain.ErrReadQuorum)
	e.origin.setFail(nil)
	assert.Equal(t, "REVALIDATED", e.get("www.example.com", "/a.txt").xcache(), "unchanged at the origin")
}

// TestRequestCollapsing is TCC-08: concurrent misses read the origin once.
//
// covers: S-DST-07c
func TestRequestCollapsing(t *testing.T) {
	e := newEnv(t, envOptions{})
	data := payload(64<<10, 7)
	e.origin.put("web", "hot.bin", data)
	gate := make(chan struct{})
	e.origin.mu.Lock()
	e.origin.gate = gate
	e.origin.mu.Unlock()
	const n = 1000
	var wg sync.WaitGroup
	results := make([]result, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = e.get("www.example.com", "/hot.bin")
		}()
	}
	time.Sleep(100 * time.Millisecond)
	e.origin.mu.Lock()
	e.origin.gate = nil
	e.origin.mu.Unlock()
	close(gate)
	wg.Wait()
	assert.Equal(t, int64(1), e.origin.gets.Load(), "one read of the origin for %d concurrent misses", n)
	for _, r := range results {
		require.NoError(t, r.err)
		require.True(t, bytes.Equal(data, r.body))
	}
}

// TestRangeCaching is TCC-09: an object larger than a block is cached by
// blocks; the ranges are served byte-identical from them.
//
// covers: S-DST-07c
func TestRangeCaching(t *testing.T) {
	e := newEnv(t, envOptions{})
	const block = 64 << 10
	e.setDist(cache.DistributionConfig{Distribution: "web", Cache: cache.Settings{PolicySpec: cache.PolicySpec{
		RangeCaching: &cache.RangeCaching{BlockSize: size(block)}}}})
	video := payload(10*block+123, 3)
	e.origin.put("web", "video.mp4", video, textInfo("video/mp4"))
	ranges := []struct{ start, end int64 }{
		{0, 99}, {block - 10, block + 10}, {3*block + 5, 7*block + 17}, {int64(len(video)) - 200, int64(len(video)) - 1}, {0, int64(len(video)) - 1},
	}
	for _, rg := range ranges {
		r := e.get("www.example.com", "/video.mp4", withRange(rg.start, rg.end))
		require.NoError(t, r.err)
		require.True(t, bytes.Equal(video[rg.start:rg.end+1], r.body), "range %d-%d", rg.start, rg.end)
	}
	reads := e.origin.gets.Load()
	assert.LessOrEqual(t, reads, int64(12), "one read per block (and the description)")
	// Everything cached: no read of the origin.
	full := e.get("www.example.com", "/video.mp4")
	require.True(t, bytes.Equal(video, full.body))
	assert.Equal(t, "HIT", full.xcache())
	r := e.get("www.example.com", "/video.mp4", withRange(2*block+1, 5*block))
	require.True(t, bytes.Equal(video[2*block+1:5*block+1], r.body))
	assert.Equal(t, reads, e.origin.gets.Load())
	// A new version: the old blocks are never mixed in.
	video2 := payload(10*block+123, 9)
	e.origin.put("web", "video.mp4", video2, textInfo("video/mp4"))
	require.NoError(t, e.svc.ObjectChanged(context.Background(), cache.ObjectChange{Bucket: "web", Key: "video.mp4"}))
	full = e.get("www.example.com", "/video.mp4")
	require.True(t, bytes.Equal(video2, full.body))
	// With range caching off, the ranges of a large object go to the
	// origin.
	e.setDist(cache.DistributionConfig{Distribution: "web", Cache: cache.Settings{PolicySpec: cache.PolicySpec{
		RangeCaching: &cache.RangeCaching{Enabled: ptr(false), BlockSize: size(block)}}}})
	r = e.get("www.example.com", "/video.mp4", withRange(0, 9))
	assert.False(t, r.handled)
}

// TestCompressedVariants is TCC-10: the textual objects are served
// compressed to the clients that accept it, with Vary, and plain to the
// others.
func TestCompressedVariants(t *testing.T) {
	e := newEnv(t, envOptions{})
	text := bytes.Repeat([]byte("hello cache world "), 512)
	e.origin.put("web", "app.js", text, textInfo("application/javascript"))
	e.origin.put("web", "pic.jpg", payload(8<<10, 1), textInfo("image/jpeg"))
	plain := e.get("www.example.com", "/app.js")
	assert.Equal(t, text, plain.body)
	assert.Empty(t, plain.info.ContentEncoding)
	assert.Equal(t, "Accept-Encoding", plain.header.Get("Vary"))
	gz := e.get("www.example.com", "/app.js", withHeader("Accept-Encoding", "gzip, deflate"))
	assert.Equal(t, "gzip", gz.info.ContentEncoding)
	assert.Less(t, len(gz.body), len(text))
	zr, err := gzip.NewReader(bytes.NewReader(gz.body))
	require.NoError(t, err)
	got, err := io.ReadAll(zr)
	require.NoError(t, err)
	assert.Equal(t, text, got)
	assert.NotEqual(t, plain.info.ETag, gz.info.ETag)
	br := e.get("www.example.com", "/app.js", withHeader("Accept-Encoding", "br;q=1, gzip;q=0.5"))
	assert.Equal(t, "gzip", br.info.ContentEncoding, "the policy prefers gzip, the client accepts it")
	e.setDist(cache.DistributionConfig{Distribution: "web", Cache: cache.Settings{PolicySpec: cache.PolicySpec{
		Compression: &cache.Compression{Algorithms: []string{cache.EncodingBrotli, cache.EncodingZstd, cache.EncodingGzip}}}}})
	br = e.get("www.example.com", "/app.js", withHeader("Accept-Encoding", "br"))
	require.Equal(t, "br", br.info.ContentEncoding)
	got, err = io.ReadAll(brotli.NewReader(bytes.NewReader(br.body)))
	require.NoError(t, err)
	assert.Equal(t, text, got)
	zs := e.get("www.example.com", "/app.js", withHeader("Accept-Encoding", "zstd"))
	assert.Equal(t, "zstd", zs.info.ContentEncoding)
	none := e.get("www.example.com", "/app.js", withHeader("Accept-Encoding", "identity"))
	assert.Empty(t, none.info.ContentEncoding)
	assert.Equal(t, text, none.body)
	// A conditional request matches the ETag of its variant.
	nm := e.get("www.example.com", "/app.js", withHeader("Accept-Encoding", "br"), withConditions(domain.Conditions{IfNoneMatch: `"` + br.info.ETag + `"`}))
	require.ErrorIs(t, nm.err, domain.ErrNotModified)
	// HEAD reports the variant too.
	head := e.head("www.example.com", "/app.js", withHeader("Accept-Encoding", "br"))
	assert.Equal(t, "br", head.info.ContentEncoding)
	// Binary types are not compressed.
	jpg := e.get("www.example.com", "/pic.jpg", withHeader("Accept-Encoding", "gzip"))
	assert.Empty(t, jpg.info.ContentEncoding)
	assert.Empty(t, jpg.header.Get("Vary"))
	// Ranges are served from the identity.
	rg := e.get("www.example.com", "/app.js", withHeader("Accept-Encoding", "gzip"), withRange(0, 4))
	assert.Equal(t, "hello", string(rg.body))
}

// TestNegativeCaching is TCC-12: a 404 is cached for its short lifetime,
// and dropped at once when the object is created.
func TestNegativeCaching(t *testing.T) {
	e := newEnv(t, envOptions{})
	r := e.get("www.example.com", "/later.html")
	require.ErrorIs(t, r.err, domain.ErrObjectNotFound)
	assert.Equal(t, "MISS", r.xcache())
	r = e.get("www.example.com", "/later.html")
	require.ErrorIs(t, r.err, domain.ErrObjectNotFound)
	assert.Equal(t, "HIT", r.xcache())
	assert.Equal(t, int64(1), e.origin.gets.Load())
	// Created: the write invalidates the negative entry.
	e.origin.put("web", "later.html", []byte("here"), textInfo("text/html"))
	require.NoError(t, e.svc.ObjectChanged(context.Background(), cache.ObjectChange{Bucket: "web", Key: "later.html"}))
	r = e.get("www.example.com", "/later.html")
	require.NoError(t, r.err)
	assert.Equal(t, "here", string(r.body))
	// The negative lifetime ends by itself too.
	e.get("www.example.com", "/gone.html")
	e.origin.put("web", "gone.html", []byte("x"))
	e.clk.Advance(11 * time.Second)
	require.NoError(t, e.get("www.example.com", "/gone.html").err)
}

// TestBypassAndSecurity is TCC-11 at the level of the cache: the requests
// with credentials skip it (or get entries of their own), the other
// methods and the bypass conditions too; the signature parameters never
// key the entries.
func TestBypassAndSecurity(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.origin.put("web", "doc.pdf", []byte("pdf"), textInfo("application/pdf"))
	r := e.get("www.example.com", "/doc.pdf", withCredentials("alice"))
	assert.Equal(t, "BYPASS", r.xcache())
	assert.False(t, r.handled)
	assert.Equal(t, "BYPASS", e.get("www.example.com", "/doc.pdf", withMethod(http.MethodPost)).xcache())
	assert.Equal(t, "MISS", e.get("www.example.com", "/doc.pdf").xcache())
	// A policy that caches the credentials keys the entries by them.
	e.setDist(cache.DistributionConfig{Distribution: "web", Cache: cache.Settings{PolicySpec: cache.PolicySpec{
		Bypass: &cache.Bypass{Authorization: ptr(false), QueryParams: []string{"nocache"}, Cookies: []string{"session"}}}}})
	assert.Equal(t, "MISS", e.get("www.example.com", "/doc.pdf", withCredentials("alice")).xcache())
	assert.Equal(t, "HIT", e.get("www.example.com", "/doc.pdf", withCredentials("alice")).xcache())
	assert.Equal(t, "MISS", e.get("www.example.com", "/doc.pdf", withCredentials("bob")).xcache(), "never the entry of another credential")
	assert.Equal(t, "MISS", e.get("www.example.com", "/doc.pdf").xcache(), "nor the anonymous one")
	assert.Equal(t, "BYPASS", e.get("www.example.com", "/doc.pdf", withQuery("nocache=1")).xcache())
	assert.Equal(t, "BYPASS", e.get("www.example.com", "/doc.pdf", withHeader("Cookie", "session=x")).xcache())
	// Signed URLs: verified before the lookup by the distribution, they
	// share the entry.
	assert.Equal(t, "HIT", e.get("www.example.com", "/doc.pdf", withQuery("Expires=1&Signature=s&Key-Pair-Id=k")).xcache())
	// Versions, parts and SSE-C keys are never served from the cache.
	route := e.dists.resolve("www.example.com")
	ctx, _ := e.svc.Begin(context.Background(), cache.Request{Distribution: route.Distribution, Host: route.Host, Method: http.MethodGet, Path: "/doc.pdf", Header: http.Header{}})
	_, handled, _ := e.svc.GetObject(ctx, "web", "doc.pdf", object.GetObjectOptions{VersionID: "v"})
	assert.False(t, handled)
	// A read of another bucket than the origin of the distribution.
	_, handled, _ = e.svc.GetObject(ctx, "other", "doc.pdf", object.GetObjectOptions{})
	assert.False(t, handled)
	// No request begun: the reads are not the cache's.
	_, handled, _ = e.svc.GetObject(context.Background(), "web", "doc.pdf", object.GetObjectOptions{})
	assert.False(t, handled)
}

// TestDistributionChangeDropsEntries checks that a change of a
// distribution drops its entries on the next request (the settings it
// was cached under changed).
func TestDistributionChangeDropsEntries(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.origin.put("web", "a.txt", []byte("a"))
	e.get("www.example.com", "/a.txt")
	require.Equal(t, "HIT", e.get("www.example.com", "/a.txt").xcache())
	d := *e.dists.get("web")
	d.Aliases = append(d.Aliases, "static.example.com")
	e.dists.set(d)
	assert.Equal(t, "MISS", e.get("www.example.com", "/a.txt").xcache())
}

// TestDisabledDomain is TCC-02: the same bucket on two domains with
// different policies; the domain with the cache off always bypasses it.
func TestDisabledDomain(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.origin.put("web", "index.html", []byte("home"), textInfo("text/html"))
	e.setDist(cache.DistributionConfig{Distribution: "web", Domains: []cache.DomainSettings{
		{Host: "www.example.com", Cache: cache.Settings{Policy: cache.PolicyImmutableAssets}},
		{Host: "*.preview.example.com", Cache: cache.Settings{Enabled: ptr(false)}},
	}})
	e.get("www.example.com", "/index.html")
	www := e.get("www.example.com", "/index.html")
	assert.Equal(t, "HIT", www.xcache())
	assert.Equal(t, "public, max-age=31536000, immutable", www.header.Get("Cache-Control"))
	for range 3 {
		p := e.get("pr-7.preview.example.com", "/index.html")
		assert.Equal(t, "BYPASS", p.xcache())
		assert.Empty(t, p.header.Get("Cache-Control"))
	}
	e.get("example.com", "/index.html")
	other := e.get("example.com", "/index.html")
	assert.Equal(t, "HIT", other.xcache())
	assert.Empty(t, other.header.Get("Cache-Control"), "the standard policy keeps the Cache-Control of the object")
	// A behavior overrides the domain.
	e.setDist(cache.DistributionConfig{Distribution: "web", Behaviors: []cache.BehaviorSettings{{
		PathPattern: distribution.PathPattern{Type: distribution.PatternPrefix, Pattern: "/api/"}, Cache: cache.Settings{Enabled: ptr(false)}}}})
	e.origin.put("web", "api/x", []byte("1"))
	e.get("www.example.com", "/api/x")
	assert.Equal(t, "BYPASS", e.get("www.example.com", "/api/x").xcache())
}

// TestUncacheableObjects checks hit-for-pass: the objects outside the
// limits of the policy are read from the origin.
func TestUncacheableObjects(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.setDist(cache.DistributionConfig{Distribution: "web", Cache: cache.Settings{PolicySpec: cache.PolicySpec{
		MaxObjectSize: size(1024), MinSize: size(2), ContentTypes: &cache.ContentTypes{Deny: []string{"application/x-private"}}}}})
	e.origin.put("web", "big", payload(2048, 1))
	e.origin.put("web", "tiny", []byte("x"))
	e.origin.put("web", "priv", []byte("secret"), textInfo("application/x-private"))
	e.origin.put("web", "nostore", []byte("abc"), cacheControl("no-store"))
	for _, p := range []string{"/big", "/tiny", "/priv", "/nostore"} {
		r := e.get("www.example.com", p)
		assert.False(t, r.handled, p)
		assert.Equal(t, "BYPASS", r.xcache(), p)
	}
	assert.Equal(t, 0, cache.Entries(e.svc)-4, "only the pass markers are kept")
}

// TestConditionalAndHead checks the conditional requests served from the
// cache (ETag, Last-Modified, 304) and HEAD.
func TestConditionalAndHead(t *testing.T) {
	e := newEnv(t, envOptions{})
	info := e.origin.put("web", "a.css", []byte("body{}"), textInfo("image/png"))
	h := e.head("www.example.com", "/a.css")
	require.NoError(t, h.err)
	assert.Equal(t, info.ETag, h.info.ETag)
	assert.Equal(t, "MISS", h.xcache())
	assert.Equal(t, "HIT", e.head("www.example.com", "/a.css").xcache())
	r := e.get("www.example.com", "/a.css")
	assert.Equal(t, "MISS", r.xcache(), "a description does not hold the content")
	r = e.get("www.example.com", "/a.css", withConditions(domain.Conditions{IfNoneMatch: `"` + info.ETag + `"`}))
	assert.True(t, errors.Is(r.err, domain.ErrNotModified))
	r = e.get("www.example.com", "/a.css", withConditions(domain.Conditions{IfModifiedSince: info.ModTime.Add(time.Second)}))
	assert.True(t, errors.Is(r.err, domain.ErrNotModified))
	r = e.get("www.example.com", "/a.css", withConditions(domain.Conditions{IfMatch: `"other"`}))
	assert.True(t, errors.Is(r.err, domain.ErrPreconditionFailed))
	r = e.get("www.example.com", "/a.css", withRange(100, 200))
	assert.True(t, errors.Is(r.err, domain.ErrInvalidRange))
}
