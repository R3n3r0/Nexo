package integration_test

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/config"
	"github.com/r3n3r0/nexo/internal/service/cache"
	"github.com/r3n3r0/nexo/internal/service/distribution"
	"github.com/r3n3r0/nexo/internal/testutil"
)

// cacheSite creates a public website distribution of bucket on aliases.
func (e *env) cacheSite(id, bucket string, aliases ...string) {
	e.t.Helper()
	_, err := e.srv.Nodes[0].App.Distributions().Create(e.ctx, distribution.Distribution{
		ID: id, Enabled: true, Aliases: aliases, Mode: distribution.ModeWebsite, Origin: distribution.Origin{Bucket: bucket},
		DefaultBehavior: distribution.Behavior{Access: distribution.AccessPublic, IndexDocument: "index.html"},
	})
	require.NoError(e.t, err)
	e.cacheSynced()
}

// cacheSynced waits for the caches of the nodes to receive the
// invalidations of every peer (a node that just started compares the
// sequences of its peers first): their entries are served as HITs.
func (e *env) cacheSynced() {
	e.t.Helper()
	require.Eventually(e.t, func() bool {
		for _, n := range e.srv.Nodes {
			st, err := n.App.Cache().NodeStats(e.ctx, cache.StatsQuery{})
			if err != nil || !st.Synced {
				return false
			}
		}
		return true
	}, 10*time.Second, 10*time.Millisecond)
}

// setCache stores the cache configuration of a distribution (on the last
// node: every node applies it at once).
func (e *env) setCache(cfg cache.DistributionConfig) {
	e.t.Helper()
	svc := e.srv.Nodes[len(e.srv.Nodes)-1].App.Cache()
	cur, err := svc.GetDistribution(e.ctx, cfg.Distribution)
	require.NoError(e.t, err)
	_, err = svc.UpdateDistribution(e.ctx, cfg, cur.ETag)
	require.NoError(e.t, err)
}

// xcache returns the X-Cache of an answer.
func xcache(r response) string { return r.Header.Get(cache.HeaderXCache) }

// TestCacheServesAndInvalidates is TCC-03 and TCC-05: the answers of a
// distribution come from the cache of each node (MISS, then HIT with its
// Age, 304 from the cache), and a write acknowledged by any node is seen
// at once on every domain of every node: never the old content.
//
// covers: S-CCH-01, S-CCH-03
func TestCacheServesAndInvalidates(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.mkBucket("cached")
	e.putTyped("cached", "index.html", "<h1>v1</h1>", "text/html")
	e.putTyped("cached", "app.js", "console.log(1)", "application/javascript")
	e.cacheSite("cached-site", "cached", "www.cached.test", "cached.test")
	hosts := []string{"www.cached.test", "cached.test"}

	for _, base := range e.nodeURLs() {
		for _, h := range hosts {
			first := hostRequest(t, http.MethodGet, base, h, "/", nil)
			require.Equal(t, http.StatusOK, first.StatusCode)
			assert.Equal(t, "<h1>v1</h1>", string(first.Body))
			assert.Equal(t, "MISS", xcache(first), "%s %s", base, h)
			second := hostRequest(t, http.MethodGet, base, h, "/", nil)
			assert.Equal(t, "<h1>v1</h1>", string(second.Body))
			assert.Equal(t, "HIT", xcache(second), "%s %s", base, h)
			assert.NotEmpty(t, second.Header.Get("Age"))
			assert.Equal(t, first.Header.Get("ETag"), second.Header.Get("ETag"))
			// A conditional request is answered from the cache.
			notMod := hostRequest(t, http.MethodGet, base, h, "/", http.Header{"If-None-Match": {second.Header.Get("ETag")}})
			assert.Equal(t, http.StatusNotModified, notMod.StatusCode)
			assert.Equal(t, "HIT", xcache(notMod))
		}
	}

	// Writes through the S3 API, on whatever node, are seen at once.
	for i, body := range []string{"<h1>v2</h1>", "<h1>v3</h1>"} {
		e.putTyped("cached", "index.html", body, "text/html")
		for _, base := range e.nodeURLs() {
			for _, h := range hosts {
				r := hostRequest(t, http.MethodGet, base, h, "/", nil)
				assert.Equal(t, body, string(r.Body), "write %d on %s %s", i, base, h)
				assert.NotEqual(t, "HIT", xcache(r), "the entry was dropped on %s", base)
			}
		}
	}
	// A copy, a delete, a re-creation.
	_, err := e.root.CopyObject(e.ctx, &s3.CopyObjectInput{Bucket: aws.String("cached"), Key: aws.String("index.html"),
		CopySource: aws.String("cached/app.js"), MetadataDirective: "REPLACE", ContentType: aws.String("text/html")})
	require.NoError(t, err)
	for _, base := range e.nodeURLs() {
		assert.Equal(t, "console.log(1)", string(hostRequest(t, http.MethodGet, base, "www.cached.test", "/index.html", nil).Body), base)
	}
	_, err = e.root.DeleteObject(e.ctx, &s3.DeleteObjectInput{Bucket: aws.String("cached"), Key: aws.String("index.html")})
	require.NoError(t, err)
	for _, base := range e.nodeURLs() {
		r := hostRequest(t, http.MethodGet, base, "www.cached.test", "/index.html", nil)
		assert.Equal(t, http.StatusNotFound, r.StatusCode, base)
		// The 404 is cached (negative caching)...
		r = hostRequest(t, http.MethodGet, base, "www.cached.test", "/index.html", nil)
		assert.Equal(t, http.StatusNotFound, r.StatusCode, base)
	}
	// ...and dropped by the creation of the object.
	e.putTyped("cached", "index.html", "<h1>back</h1>", "text/html")
	for _, base := range e.nodeURLs() {
		r := hostRequest(t, http.MethodGet, base, "www.cached.test", "/index.html", nil)
		assert.Equal(t, http.StatusOK, r.StatusCode, base)
		assert.Equal(t, "<h1>back</h1>", string(r.Body), base)
	}
	// A multipart upload completed.
	up, err := e.root.CreateMultipartUpload(e.ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String("cached"), Key: aws.String("app.js")})
	require.NoError(t, err)
	part, err := e.root.UploadPart(e.ctx, &s3.UploadPartInput{Bucket: aws.String("cached"), Key: aws.String("app.js"), UploadId: up.UploadId,
		PartNumber: aws.Int32(1), Body: strings.NewReader("console.log(2)")})
	require.NoError(t, err)
	for _, base := range e.nodeURLs() {
		hostRequest(t, http.MethodGet, base, "www.cached.test", "/app.js", nil)
	}
	_, err = e.root.CompleteMultipartUpload(e.ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String("cached"), Key: aws.String("app.js"),
		UploadId: up.UploadId, MultipartUpload: &s3types.CompletedMultipartUpload{Parts: []s3types.CompletedPart{{ETag: part.ETag, PartNumber: aws.Int32(1)}}}})
	require.NoError(t, err)
	for _, base := range e.nodeURLs() {
		assert.Equal(t, "console.log(2)", string(hostRequest(t, http.MethodGet, base, "www.cached.test", "/app.js", nil).Body), base)
	}

	// The inspector sees the entry of every node.
	for _, base := range e.nodeURLs() {
		hostRequest(t, http.MethodGet, base, "www.cached.test", "/app.js", nil)
	}
	res, err := e.srv.Nodes[0].App.Cache().Inspect(e.ctx, cache.InspectInput{URL: "http://www.cached.test/app.js"})
	require.NoError(t, err)
	assert.Equal(t, "cached-site", res.Distribution)
	require.Len(t, res.Nodes, len(e.srv.Nodes))
	for _, n := range res.Nodes {
		assert.True(t, n.Entry.Present, n.Node)
	}
}

// TestCacheManualInvalidation covers the invalidations of the API (by
// path, tag, domain, everything) with their state on every node, and the
// purge webhooks (TCC-04, TCC-06, TCC-15).
//
// covers: S-CCH-03, S-CCH-05
func TestCacheManualInvalidation(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.mkBucket("purge")
	_, err := e.root.PutObject(e.ctx, &s3.PutObjectInput{Bucket: aws.String("purge"), Key: aws.String("p/1.html"),
		Body: strings.NewReader("product 1"), ContentType: aws.String("text/html"), Metadata: map[string]string{"cache-tag": "product-1, catalog"}})
	require.NoError(t, err)
	e.putTyped("purge", "p/2.html", "product 2", "text/html")
	e.putTyped("purge", "index.html", "home", "text/html")
	e.cacheSite("purge-site", "purge", "www.purge.test", "purge.test")

	var (
		mu    sync.Mutex
		calls []cache.PurgeTemplateData
	)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data cache.PurgeTemplateData
		body, _ := io.ReadAll(r.Body)
		if json.Unmarshal(body, &data) == nil {
			mu.Lock()
			calls = append(calls, data)
			mu.Unlock()
		}
	}))
	t.Cleanup(hook.Close)
	e.setCache(cache.DistributionConfig{Distribution: "purge-site", Webhooks: []cache.Webhook{
		{ID: "edge", Type: cache.WebhookGeneric, URL: hook.URL + "/purge", Domains: []string{"www.purge.test"}},
	}})

	warm := func() {
		for _, base := range e.nodeURLs() {
			for _, h := range []string{"www.purge.test", "purge.test"} {
				for _, p := range []string{"/p/1.html", "/p/2.html", "/"} {
					hostRequest(t, http.MethodGet, base, h, p, nil)
					require.Equal(t, "HIT", xcache(hostRequest(t, http.MethodGet, base, h, p, nil)), "%s%s on %s", h, p, base)
				}
			}
		}
	}
	state := func(h, p string) []string {
		var out []string
		for _, base := range e.nodeURLs() {
			out = append(out, xcache(hostRequest(t, http.MethodGet, base, h, p, nil)))
		}
		return out
	}
	all := func(v string) []string {
		out := make([]string, len(e.srv.Nodes))
		for i := range out {
			out[i] = v
		}
		return out
	}
	svc := e.srv.Nodes[len(e.srv.Nodes)-1].App.Cache()

	warm()
	inv, err := svc.Invalidate(e.ctx, cache.InvalidationRequest{Distribution: "purge-site", Paths: []string{"/p/2.html"}})
	require.NoError(t, err)
	assert.Equal(t, cache.InvalidationCompleted, inv.State)
	require.Len(t, inv.Nodes, len(e.srv.Nodes))
	for _, n := range inv.Nodes {
		assert.Equal(t, cache.NodeCompleted, n.State, n.Node)
	}
	assert.Equal(t, all("MISS"), state("www.purge.test", "/p/2.html"))
	assert.Equal(t, all("MISS"), state("purge.test", "/p/2.html"))
	assert.Equal(t, all("HIT"), state("www.purge.test", "/p/1.html"))
	got, err := e.srv.Nodes[0].App.Cache().GetInvalidation(e.ctx, "purge-site", inv.ID)
	require.NoError(t, err)
	assert.Equal(t, cache.InvalidationCompleted, got.State, "the history is shared")

	// By tag (x-amz-meta-cache-tag).
	warm()
	_, err = svc.Invalidate(e.ctx, cache.InvalidationRequest{Distribution: "purge-site", Tags: []string{"product-1"}})
	require.NoError(t, err)
	assert.Equal(t, all("MISS"), state("www.purge.test", "/p/1.html"))
	assert.Equal(t, all("HIT"), state("www.purge.test", "/p/2.html"))

	// One domain only.
	warm()
	_, err = svc.Invalidate(e.ctx, cache.InvalidationRequest{Distribution: "purge-site", Domain: "purge.test", All: true})
	require.NoError(t, err)
	assert.Equal(t, all("MISS"), state("purge.test", "/"))
	assert.Equal(t, all("HIT"), state("www.purge.test", "/"))

	// Soft: served stale while revalidated.
	warm()
	_, err = svc.Invalidate(e.ctx, cache.InvalidationRequest{Distribution: "purge-site", Patterns: []distribution.PathPattern{
		{Type: distribution.PatternGlob, Pattern: "/p/*.html"}}, Soft: true})
	require.NoError(t, err)
	for _, x := range state("www.purge.test", "/p/1.html") {
		assert.Contains(t, []string{"STALE", "REVALIDATED"}, x)
	}

	list, err := svc.ListInvalidations(e.ctx, "purge-site")
	require.NoError(t, err)
	assert.Len(t, list, 4)

	// The webhook of www.purge.test was called for the invalidations of
	// its domain (by the node that received them) and for the writes.
	e.putTyped("purge", "p/2.html", "product 2 bis", "text/html")
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range calls {
			if c.Automatic && len(c.Paths) > 0 && c.Paths[0] == "/p/2.html" {
				return true
			}
		}
		return false
	}, 10*time.Second, 20*time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	manual := 0
	for _, c := range calls {
		assert.Equal(t, []string{"www.purge.test"}, c.Hosts)
		if !c.Automatic {
			manual++
		}
	}
	assert.Equal(t, 3, manual, "every invalidation but the one of purge.test")
}

// TestCacheSecurity is TCC-11: a cached answer is never served to a
// request the distribution or the bucket refuses: the signature of the
// distribution is verified before the cache (and is not in the key), an
// expired one is refused while the object is cached, a change of the
// access of a path applies at once, and the answers to authenticated
// requests are not served to others.
//
// covers: S-CCH-04
func TestCacheSecurity(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.mkBucket("secure")
	e.putTyped("secure", "downloads/a.zip", "zipdata", "application/zip")
	e.putTyped("secure", "private/p.txt", "private", "text/plain")
	e.putTyped("secure", "index.html", "home", "text/html")
	dists := e.srv.Nodes[0].App.Distributions()
	_, err := dists.CreateKeyGroup(e.ctx, "secure-keys", "downloads")
	require.NoError(t, err)
	k, err := dists.GenerateKey(e.ctx, distribution.GenerateKeyInput{KeyGroup: "secure-keys"})
	require.NoError(t, err)
	_, err = dists.Create(e.ctx, distribution.Distribution{
		ID: "secure-site", Enabled: true, Aliases: []string{"dl.secure.test"}, Mode: distribution.ModeWebsite,
		Origin:          distribution.Origin{Bucket: "secure"},
		DefaultBehavior: distribution.Behavior{Access: distribution.AccessPublic, IndexDocument: "index.html"},
		Behaviors: []distribution.Behavior{{
			PathPattern: &distribution.PathPattern{Type: distribution.PatternPrefix, Pattern: "/downloads/"},
			Access:      distribution.AccessSigned, Signing: &distribution.Signing{KeyGroup: "secure-keys"},
		}},
	})
	require.NoError(t, err)
	e.cacheSynced()
	signer := e.srv.Nodes[0].App.DistributionSigner()
	sign := func(expires time.Time) string {
		u, err := signer.SignURL(e.ctx, distribution.SignInput{URL: "http://dl.secure.test/downloads/a.zip", KeyPairID: k.PublicKey.ID,
			PrivateKeyPEM: k.PrivateKeyPEM, Expires: expires})
		require.NoError(t, err)
		parsed, err := url.Parse(u)
		require.NoError(t, err)
		return parsed.RequestURI()
	}
	for _, base := range e.nodeURLs() {
		get := func(path string, hdr http.Header) response {
			return hostRequest(t, http.MethodGet, base, "dl.secure.test", path, hdr)
		}
		r := get(sign(time.Now().Add(time.Hour)), nil)
		require.Equal(t, http.StatusOK, r.StatusCode, "%s", r.Body)
		// Another signature of the same object hits the same entry.
		r = get(sign(time.Now().Add(2*time.Hour)), nil)
		assert.Equal(t, http.StatusOK, r.StatusCode)
		assert.Equal(t, "HIT", xcache(r), "the signature is not in the key")
		// Unsigned, tampered: refused while cached.
		assert.Equal(t, http.StatusForbidden, get("/downloads/a.zip", nil).StatusCode)
		assert.Equal(t, http.StatusForbidden, get(strings.Replace(sign(time.Now().Add(time.Hour)), "Signature=", "Signature=AA", 1), nil).StatusCode)
	}
	// Expired while cached: a signer whose clock is two hours behind.
	past := distribution.NewSigner(distribution.SignerDeps{Clock: clock.NewFake(time.Now().Add(-2 * time.Hour))})
	expiredURL, err := past.SignURL(e.ctx, distribution.SignInput{URL: "http://dl.secure.test/downloads/a.zip", KeyPairID: k.PublicKey.ID,
		PrivateKeyPEM: k.PrivateKeyPEM, Expires: time.Now().Add(-time.Hour)})
	require.NoError(t, err)
	parsed, err := url.Parse(expiredURL)
	require.NoError(t, err)
	expired := parsed.RequestURI()
	for _, base := range e.nodeURLs() {
		assert.Equal(t, http.StatusForbidden, hostRequest(t, http.MethodGet, base, "dl.secure.test", expired, nil).StatusCode, base)
	}

	// The access of a cached path changes: refused at once on every node.
	for _, base := range e.nodeURLs() {
		hostRequest(t, http.MethodGet, base, "dl.secure.test", "/private/p.txt", nil)
		require.Equal(t, "HIT", xcache(hostRequest(t, http.MethodGet, base, "dl.secure.test", "/private/p.txt", nil)))
	}
	d, err := dists.Get(e.ctx, "secure-site")
	require.NoError(t, err)
	d.Behaviors = append(d.Behaviors, distribution.Behavior{
		PathPattern: &distribution.PathPattern{Type: distribution.PatternPrefix, Pattern: "/private/"}, Access: distribution.AccessDeny})
	_, err = dists.Update(e.ctx, d, d.ETag)
	require.NoError(t, err)
	for _, base := range e.nodeURLs() {
		r := hostRequest(t, http.MethodGet, base, "dl.secure.test", "/private/p.txt", nil)
		assert.NotEqual(t, http.StatusOK, r.StatusCode, base)
		assert.NotEqual(t, "private", string(r.Body), base)
	}

	// Authenticated requests of an s3 distribution: never served to the
	// anonymous ones (nor to another user).
	e.mkBucket("s3secure")
	e.put("s3secure", "doc.txt", []byte("for alice"))
	_, err = dists.Create(e.ctx, distribution.Distribution{ID: "s3-secure", Enabled: true, Aliases: []string{"s3.secure.test"},
		Mode: distribution.ModeS3, Origin: distribution.Origin{Bucket: "s3secure"}})
	require.NoError(t, err)
	_, port, err := net.SplitHostPort(e.hostPort())
	require.NoError(t, err)
	endpoint := url.URL{Scheme: "http", Host: "s3.secure.test:" + port}
	cfg := e.sdkConfig(e.rootCreds())
	cfg.HTTPClient = e.loopbackClient()
	client := s3.NewFromConfig(cfg, func(o *s3.Options) { o.EndpointResolverV2 = fixedEndpoint{u: endpoint} })
	for range 2 {
		out, err := client.GetObject(e.ctx, &s3.GetObjectInput{Bucket: aws.String("s3secure"), Key: aws.String("doc.txt")})
		require.NoError(t, err)
		body, _ := io.ReadAll(out.Body)
		_ = out.Body.Close()
		assert.Equal(t, "for alice", string(body))
	}
	anon := doWith(t, e.loopbackClient(), newRequest(t, http.MethodGet, endpoint.String()+"/doc.txt", nil))
	assert.Equal(t, http.StatusForbidden, anon.StatusCode)
	assert.NotEqual(t, "for alice", string(anon.Body))
	bob := e.createUser("bobcache", "bobcache-secret")
	bobCfg := e.sdkConfig(bob)
	bobCfg.HTTPClient = e.loopbackClient()
	bobClient := s3.NewFromConfig(bobCfg, func(o *s3.Options) { o.EndpointResolverV2 = fixedEndpoint{u: endpoint} })
	_, err = bobClient.GetObject(e.ctx, &s3.GetObjectInput{Bucket: aws.String("s3secure"), Key: aws.String("doc.txt")})
	assert.Equal(t, http.StatusForbidden, httpStatus(err), "another user without the permission")
}

// TestCacheDomainsAndConfig covers the settings per domain applied by
// every node without restart (TCC-01, TCC-02) and two proxies in front of
// the cluster with different domains: the purge of one domain does not
// touch the other (TCC-C-05).
//
// covers: S-CCH-02
func TestCacheDomainsAndConfig(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.mkBucket("multi")
	e.putTyped("multi", "index.html", "home", "text/html")
	e.cacheSite("multi-site", "multi", "a.multi.test", "b.multi.test", "nocache.multi.test")

	// Two proxies in front of the first node, one per domain.
	proxy := func(host string) string {
		target, err := url.Parse(e.srv.Nodes[0].S3URL)
		require.NoError(t, err)
		rp := httputil.NewSingleHostReverseProxy(target)
		director := rp.Director
		rp.Director = func(r *http.Request) {
			director(r)
			r.Host = host
		}
		srv := httptest.NewServer(rp)
		t.Cleanup(srv.Close)
		return srv.URL
	}
	pa, pb := proxy("a.multi.test"), proxy("b.multi.test")
	viaProxy := func(base string) string {
		r := hostRequest(t, http.MethodGet, base, "ignored.test", "/", nil)
		require.Equal(t, http.StatusOK, r.StatusCode)
		return xcache(r)
	}
	viaProxy(pa)
	viaProxy(pb)
	require.Equal(t, "HIT", viaProxy(pa))
	require.Equal(t, "HIT", viaProxy(pb))
	_, err := e.srv.Nodes[0].App.Cache().Invalidate(e.ctx, cache.InvalidationRequest{Distribution: "multi-site", Domain: "a.multi.test", All: true})
	require.NoError(t, err)
	assert.Equal(t, "MISS", viaProxy(pa))
	assert.Equal(t, "HIT", viaProxy(pb), "the other domain kept its entries")

	// The cache disabled on one domain, the TTL of another: every node
	// applies them at once.
	e.setCache(cache.DistributionConfig{Distribution: "multi-site", Domains: []cache.DomainSettings{
		{Host: "nocache.multi.test", Cache: cache.Settings{Enabled: aws.Bool(false)}},
		{Host: "b.multi.test", Cache: cache.Settings{Policy: cache.PolicyAggressive}},
	}})
	for _, base := range e.nodeURLs() {
		for range 2 {
			r := hostRequest(t, http.MethodGet, base, "nocache.multi.test", "/", nil)
			assert.Equal(t, "home", string(r.Body))
			assert.Equal(t, "BYPASS", xcache(r), base)
		}
	}
	eff, err := e.srv.Nodes[0].App.Cache().Effective(e.ctx, cache.EffectiveInput{Distribution: "multi-site", Host: "b.multi.test", Path: "/"})
	require.NoError(t, err)
	assert.Equal(t, cache.PolicyAggressive, eff.PolicyName)
	assert.Equal(t, cache.LevelDomain, eff.PolicyLevel)
	// The stats per domain.
	rep, err := e.srv.Nodes[0].App.Cache().Stats(e.ctx, cache.StatsQuery{Distribution: "multi-site"})
	require.NoError(t, err)
	require.Len(t, rep.Nodes, len(e.srv.Nodes))
	hosts := map[string]bool{}
	for _, h := range rep.Hosts {
		hosts[h.Host] = true
	}
	assert.True(t, hosts["a.multi.test"])
	assert.True(t, hosts["nocache.multi.test"])
	assert.Positive(t, rep.Requests[cache.ResultHit])
}

// TestCacheSharedMode is TCC-C-01 and TCC-C-03 with the shared cache of a
// cluster: a key has one owner, the first request fills it and every other
// node is served from it (one copy, one read of the origin); the writes
// still invalidate it at once. On one node the mode is the local cache.
//
// covers: S-CCH-06
func TestCacheSharedMode(t *testing.T) {
	t.Parallel()
	e := newEnv(t, testutil.WithConfig(func(c *config.Static) { c.CacheMode = config.CacheModeShared }))
	e.mkBucket("shared")
	e.putTyped("shared", "video.mp4", strings.Repeat("v", 64<<10), "video/mp4")
	e.cacheSite("shared-site", "shared", "media.shared.test")
	results := map[string]int{}
	for _, base := range e.nodeURLs() {
		r := hostRequest(t, http.MethodGet, base, "media.shared.test", "/video.mp4", nil)
		require.Equal(t, http.StatusOK, r.StatusCode)
		require.Len(t, r.Body, 64<<10)
		results[xcache(r)]++
	}
	assert.Equal(t, 1, results["MISS"], "%v", results)
	assert.Equal(t, len(e.srv.Nodes)-1, results["HIT"], "%v", results)
	rep, err := e.srv.Nodes[0].App.Cache().Stats(e.ctx, cache.StatsQuery{Distribution: "shared-site"})
	require.NoError(t, err)
	assert.Equal(t, 1, rep.Entries, "one copy in the cluster")
	res, err := e.srv.Nodes[0].App.Cache().Inspect(e.ctx, cache.InspectInput{URL: "http://media.shared.test/video.mp4"})
	require.NoError(t, err)
	if len(e.srv.Nodes) > 1 {
		assert.Equal(t, cache.ModeShared, res.Mode)
		owners := 0
		for _, n := range res.Nodes {
			if n.Owner {
				owners++
				assert.True(t, n.Entry.Present, n.Node)
			}
		}
		assert.Equal(t, 1, owners)
	}
	e.putTyped("shared", "video.mp4", strings.Repeat("w", 64<<10), "video/mp4")
	for _, base := range e.nodeURLs() {
		r := hostRequest(t, http.MethodGet, base, "media.shared.test", "/video.mp4", nil)
		assert.Equal(t, strings.Repeat("w", 64<<10), string(r.Body), base)
	}
}
