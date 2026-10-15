package cache_test

import (
	"bytes"
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/cache"
)

// TestCacheKeyUnambiguous is TCC-03/TCC-11: the separators of the key
// cannot be forged by an object key, a header value or a cookie value.
func TestCacheKeyUnambiguous(t *testing.T) {
	all := cache.ParamsOf(cache.PolicySpec{CacheKey: &cache.KeyPolicy{QueryString: &cache.QueryString{Mode: cache.QueryAll}}})
	keyed := cache.ParamsOf(cache.PolicySpec{CacheKey: &cache.KeyPolicy{Headers: []string{"accept-language"}, Cookies: []string{"variant", "other"}}})
	key := func(p cache.Params, object, query string, hdr http.Header, cred string) string {
		if hdr == nil {
			hdr = http.Header{}
		}
		req := &cache.Request{Host: "h", RawQuery: query, Header: hdr}
		return cache.KeyOf(&p, cache.ScopeOf(&p, "web", "h"), "web", object, req, cred)
	}
	tests := []struct {
		name string
		a, b string
	}{
		{name: "? in the object vs a query", a: key(all, "a.js?v=1", "", nil, ""), b: key(all, "a.js", "v=1", nil, "")},
		{name: "%3F in the object vs a query", a: key(all, "a.js%3Fv=1", "", nil, ""), b: key(all, "a.js", "v=1", nil, "")},
		{name: "| in the object vs a credential", a: key(all, "x|u:AK", "", nil, ""), b: key(all, "x", "", nil, "AK")},
		{name: "# in the object vs a block", a: key(all, "a#b0", "", nil, "") + "", b: key(all, "a", "", nil, "") + "#b0"},
		{name: "# in the object vs a variant", a: key(all, "a#e:gzip", "", nil, ""), b: key(all, "a", "", nil, "") + "#e:gzip"},
		{name: "| in a header vs a cookie", a: key(keyed, "a", "", http.Header{"Accept-Language": {"it|c:variant=a"}}, ""),
			b: key(keyed, "a", "", http.Header{"Accept-Language": {"it"}, "Cookie": {"variant=a"}}, "")},
		{name: "| in a cookie vs another cookie", a: key(keyed, "a", "", http.Header{"Cookie": {"variant=x|c:other=y"}}, ""),
			b: key(keyed, "a", "", http.Header{"Cookie": {"variant=x; other=y"}}, "")},
		{name: "| in a cookie vs a credential", a: key(keyed, "a", "", http.Header{"Cookie": {"other=y|u:AK"}}, ""),
			b: key(keyed, "a", "", http.Header{"Cookie": {"other=y"}}, "AK")},
		{name: "& in a query value", a: key(all, "a", "v=1%26w%3D2", nil, ""), b: key(all, "a", "v=1&w=2", nil, "")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.NotEqual(t, tc.a, tc.b)
		})
	}
}

// TestCacheKeyCollisionsNotServed checks end to end that an object whose
// key holds the separators of another request never serves (or poisons)
// the entry of that request.
func TestCacheKeyCollisionsNotServed(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.setDist(cache.DistributionConfig{Distribution: "web", Cache: cache.Settings{PolicySpec: cache.PolicySpec{
		CacheKey: &cache.KeyPolicy{QueryString: &cache.QueryString{Mode: cache.QueryAllowlist, Params: []string{"v"}}},
		Bypass:   &cache.Bypass{Authorization: ptr(false)},
	}}})
	e.origin.put("web", "app.js", []byte("APP"))
	// An anonymous read of the missing object "app.js?v=2" caches a 404:
	// it must not answer the reads of app.js with ?v=2.
	miss := e.get("www.example.com", "/app.js?v=2")
	require.Error(t, miss.err)
	r := e.get("www.example.com", "/app.js", withQuery("v=2"))
	require.NoError(t, r.err)
	assert.Equal(t, "APP", string(r.body))
	assert.Equal(t, "MISS", r.xcache())

	// Premium content read with a credential is never served to an
	// anonymous read of an object named after the credential part.
	e.origin.put("web", "premium.bin", []byte("PREMIUM"))
	e.origin.put("web", "premium.bin|u:AKIDPAYING", []byte("decoy"))
	paid := e.get("www.example.com", "/premium.bin", withCredentials("AKIDPAYING"))
	require.NoError(t, paid.err)
	require.Equal(t, "PREMIUM", string(paid.body))
	anon := e.get("www.example.com", "/premium.bin|u:AKIDPAYING")
	require.NoError(t, anon.err)
	assert.Equal(t, "decoy", string(anon.body))
	assert.Equal(t, "MISS", anon.xcache())
}

// TestVaryForKeyedHeaders checks that the responses keyed by request
// headers or cookies say so to the caches in front of Nexo.
func TestVaryForKeyedHeaders(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.setDist(cache.DistributionConfig{Distribution: "web", Cache: cache.Settings{PolicySpec: cache.PolicySpec{
		CacheKey: &cache.KeyPolicy{Headers: []string{"accept-language"}, Cookies: []string{"variant"}},
	}}})
	e.origin.put("web", "page.bin", []byte("page"))
	for _, x := range []string{"MISS", "HIT"} {
		r := e.get("www.example.com", "/page.bin", withHeader("Accept-Language", "it"))
		require.Equal(t, x, r.xcache())
		vary := r.header.Values("Vary")
		assert.Contains(t, vary, "Accept-Language")
		assert.Contains(t, vary, "Cookie")
	}
	e.setDist(cache.DistributionConfig{Distribution: "web"})
	r := e.get("www.example.com", "/page.bin")
	assert.Empty(t, r.header.Values("Vary"), "nothing keyed, nothing to vary on")
}

// TestCollapsingNeverServesAReadBeforeTheWrite is TCC-05 with request
// collapsing: a request that begins after the write (its invalidation
// ran) never joins a fill that read the origin before it, for the
// objects, the blocks and the variants.
func TestCollapsingNeverServesAReadBeforeTheWrite(t *testing.T) {
	const block = 64 << 10
	tests := []struct {
		name string
		// prepare stores v1 and primes the cache; hold selects the reads
		// to park; read is the request (A before the write, B after it);
		// check verifies what B got for v2.
		prepare func(e *env)
		hold    func(readKind) bool
		read    func(e *env) result
		check   func(t *testing.T, r result, v2 []byte)
		v1, v2  []byte
	}{
		{
			name:    "object",
			v1:      []byte("version-1"),
			v2:      []byte("version-2"),
			prepare: func(e *env) {},
			hold:    nil,
			read:    func(e *env) result { return e.get("www.example.com", "/a.bin") },
			check: func(t *testing.T, r result, v2 []byte) {
				assert.Equal(t, v2, r.body)
			},
		},
		{
			name: "block",
			v1:   payload(3*block, 1),
			v2:   payload(3*block, 2),
			prepare: func(e *env) {
				e.setDist(cache.DistributionConfig{Distribution: "web", Cache: cache.Settings{PolicySpec: cache.PolicySpec{
					RangeCaching: &cache.RangeCaching{BlockSize: size(block)}}}})
				// The description and the first block of v1.
				require.NoError(e.t, e.get("www.example.com", "/a.bin", withRange(0, 9)).err)
			},
			hold: func(k readKind) bool { return k == readRange },
			read: func(e *env) result { return e.get("www.example.com", "/a.bin", withRange(block+10, block+29)) },
			check: func(t *testing.T, r result, v2 []byte) {
				assert.Equal(t, v2[block+10:block+30], r.body, "never a block of the old version")
			},
		},
		{
			name: "variant",
			v1:   bytes.Repeat([]byte("one one one "), 512),
			v2:   bytes.Repeat([]byte("two two two two "), 512),
			prepare: func(e *env) {
				// Only the description is cached: the variant of a HEAD is
				// compressed from the origin.
				require.NoError(e.t, e.head("www.example.com", "/a.bin").err)
			},
			hold: func(k readKind) bool { return k == readObject },
			read: func(e *env) result {
				return e.head("www.example.com", "/a.bin", withHeader("Accept-Encoding", "gzip"))
			},
			check: func(t *testing.T, r result, v2 []byte) {
				assert.Equal(t, "gzip", r.info.ContentEncoding)
				assert.Contains(t, r.info.ETag, md5hex(v2), "the variant of the new version")
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, envOptions{})
			e.origin.put("web", "a.bin", tc.v1, textInfo("text/plain"))
			tc.prepare(e)
			release := e.origin.hold(tc.hold)
			defer release()
			var (
				wg   sync.WaitGroup
				a, b result
			)
			wg.Add(1)
			go func() {
				defer wg.Done()
				a = tc.read(e)
			}()
			require.Eventually(t, func() bool { return e.origin.parked.Load() == 1 }, timeout, time.Millisecond, "A reads v1")
			// The write: the new content, then its invalidation (before
			// the 200 of the PUT).
			e.origin.put("web", "a.bin", tc.v2, textInfo("text/plain"))
			require.NoError(t, e.svc.ObjectChanged(context.Background(), cache.ObjectChange{Bucket: "web", Key: "a.bin"}))
			wg.Add(1)
			go func() {
				defer wg.Done()
				b = tc.read(e)
			}()
			joined := !assert.Eventually(t, func() bool { return e.origin.parked.Load() == 2 }, time.Second, time.Millisecond,
				"B, begun after the write, reads the origin itself")
			release()
			wg.Wait()
			require.NoError(t, a.err)
			require.NoError(t, b.err)
			if !joined {
				tc.check(t, b, tc.v2)
			}
		})
	}
}

// TestRevalidationNeverDressesANewerEntry is TCC-04/TCC-05: a
// revalidation that confirmed the old version while a write replaced
// the entry never stores the old description over the new entry.
func TestRevalidationNeverDressesANewerEntry(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.setDist(cache.DistributionConfig{Distribution: "web", Cache: cache.Settings{PolicySpec: cache.PolicySpec{StaleWhileRevalidate: dur(0)}}})
	v1, v2 := payload(1000, 1), payload(2000, 2)
	info1 := e.origin.put("web", "a.bin", v1, cacheControl("max-age=60"))
	require.Equal(t, "MISS", e.get("www.example.com", "/a.bin").xcache())
	e.clk.Advance(2 * time.Minute)
	release := e.origin.hold(func(k readKind) bool { return k == readInfo })
	defer release()
	var (
		wg sync.WaitGroup
		a  result
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		a = e.get("www.example.com", "/a.bin")
	}()
	require.Eventually(t, func() bool { return e.origin.parked.Load() == 1 }, timeout, time.Millisecond)
	info2 := e.origin.put("web", "a.bin", v2, cacheControl("max-age=60"))
	require.NoError(t, e.svc.ObjectChanged(context.Background(), cache.ObjectChange{Bucket: "web", Key: "a.bin"}))
	b := e.get("www.example.com", "/a.bin")
	require.Equal(t, v2, b.body)
	release()
	wg.Wait()
	require.NoError(t, a.err)
	c := e.get("www.example.com", "/a.bin")
	assert.Equal(t, "HIT", c.xcache())
	assert.Equal(t, info2.ETag, c.info.ETag)
	assert.Equal(t, int64(len(v2)), c.info.Size)
	assert.Equal(t, v2, c.body)
	cond := e.get("www.example.com", "/a.bin", withConditions(domain.Conditions{IfNoneMatch: `"` + info1.ETag + `"`}))
	assert.NoError(t, cond.err, "the old validator no longer matches: no 304")
	assert.Equal(t, v2, cond.body)
}

// TestNegativeRevalidationSeesACreation is TCC-12: a revalidation that
// found the object missing never caches the 404 after a creation
// acknowledged meanwhile.
func TestNegativeRevalidationSeesACreation(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.setDist(cache.DistributionConfig{Distribution: "web", Cache: cache.Settings{PolicySpec: cache.PolicySpec{StaleWhileRevalidate: dur(0)}}})
	e.origin.put("web", "a.html", []byte("v1"), cacheControl("max-age=60"))
	require.Equal(t, "MISS", e.get("www.example.com", "/a.html").xcache())
	// Removed without an invalidation reaching this node (a distribution
	// that disabled it), then expired.
	e.origin.remove("web", "a.html")
	e.clk.Advance(2 * time.Minute)
	release := e.origin.hold(func(k readKind) bool { return k == readInfo })
	defer release()
	var (
		wg sync.WaitGroup
		a  result
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		a = e.get("www.example.com", "/a.html")
	}()
	require.Eventually(t, func() bool { return e.origin.parked.Load() == 1 }, timeout, time.Millisecond)
	e.origin.put("web", "a.html", []byte("v2"))
	require.NoError(t, e.svc.ObjectChanged(context.Background(), cache.ObjectChange{Bucket: "web", Key: "a.html"}))
	release()
	wg.Wait()
	require.Error(t, a.err, "A began before the creation")
	r := e.get("www.example.com", "/a.html")
	require.NoError(t, r.err, "created and acknowledged: visible at once")
	assert.Equal(t, "v2", string(r.body))
}

// TestHeadAndGetNegotiateTheSameVariant is TCC-10: a HEAD answers the
// validators of the variant a GET serves, also when only the description
// of the object is cached.
func TestHeadAndGetNegotiateTheSameVariant(t *testing.T) {
	e := newEnv(t, envOptions{})
	text := bytes.Repeat([]byte("same variant for head and get "), 300)
	e.origin.put("web", "big.html", text, textInfo("text/html"))
	gz := withHeader("Accept-Encoding", "gzip")
	head := e.head("www.example.com", "/big.html", gz)
	require.NoError(t, head.err)
	get := e.get("www.example.com", "/big.html", gz)
	require.NoError(t, get.err)
	head2 := e.head("www.example.com", "/big.html", gz)
	for _, r := range []result{head, get, head2} {
		assert.Equal(t, "gzip", r.info.ContentEncoding)
		assert.Equal(t, get.info.ETag, r.info.ETag)
		assert.Equal(t, get.info.Size, r.info.Size)
	}
	plain := e.head("www.example.com", "/big.html")
	assert.Empty(t, plain.info.ContentEncoding)
	assert.Equal(t, int64(len(text)), plain.info.Size)
}
