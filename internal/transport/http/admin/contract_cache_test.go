package admin_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/service/cache"
	"github.com/r3n3r0/nexo/internal/testutil"
	"github.com/r3n3r0/nexo/internal/transport/http/admin"
)

// hostGet sends an anonymous GET to the S3 listener with the Host header
// host and returns the response with its body read.
func hostGet(t *testing.T, s3URL, host, path string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, s3URL+path, nil)
	require.NoError(t, err)
	req.Host = host
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, string(data)
}

// cacheProblem is a problem document.
type cacheProblem struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// TestCacheContract exercises the cache endpoints against a real server,
// every exchange validated against api/openapi.yaml (docs/11 §11.11): a
// policy defined and named by a domain, a domain with the cache off, the
// effective policy with its levels, the answers of the cache (TCC-02), the
// inspector agreeing with them, the statistics, an invalidation completed
// on the node then a MISS, the global purge, a warm-up then a HIT, the
// purge webhooks with their test call, and a user allowed to invalidate
// but not to change the policies (TD-C-06).
func TestCacheContract(t *testing.T) {
	srv := testutil.StartServer(t)
	c := newConsole(t, srv, srv.RootUser, srv.RootPassword)
	c.call(http.MethodPost, "/buckets", map[string]any{"name": "web"}, http.StatusCreated)
	c.call(http.MethodPost, "/buckets/web/objects/upload"+q("index.html"), strings.NewReader("<h1>home</h1>"), http.StatusCreated)
	c.call(http.MethodPost, "/buckets/web/objects/upload"+q("assets/app.js"), strings.NewReader("console.log(1)"), http.StatusCreated)
	c.call(http.MethodPost, "/distributions", map[string]any{
		"id": "site", "enabled": true, "aliases": []string{"www.site.test", "preview.site.test"}, "origin": map[string]any{"bucket": "web"},
		"defaultBehavior": map[string]any{"access": "public", "indexDocument": "index.html"},
	}, http.StatusCreated)

	// Policies: the built-in ones, the preview, a defined one.
	policies := decodeAs[admin.CachePolicyListResponse](t, c.call(http.MethodGet, "/cache/policies", nil, http.StatusOK))
	var names []string
	for _, p := range policies.Items {
		names = append(names, p.Name)
		assert.True(t, p.Builtin, p.Name)
	}
	assert.Subset(t, names, []string{"disabled", "standard", "aggressive", "immutable-assets", "respect-origin"})
	bad := decodeAs[cacheProblem](t, c.call(http.MethodPost, "/cache/preview",
		map[string]any{"name": "x", "ttl": map[string]any{"default": "1h", "min": "2h"}}, http.StatusBadRequest))
	assert.Equal(t, "invalid_cache_request", bad.Code)
	preview := decodeAs[cache.PolicyPreview](t, c.call(http.MethodPost, "/cache/preview",
		map[string]any{"name": "short", "browserCacheControl": "public, max-age=600"}, http.StatusOK))
	assert.Equal(t, "public, max-age=600", preview.CacheControl)
	assert.False(t, preview.Inherited)
	created := decodeAs[cache.Policy](t, c.call(http.MethodPost, "/cache/policies", map[string]any{
		"name": "short", "comment": "ten minutes", "ttl": map[string]any{"default": "10m"}, "browserCacheControl": "public, max-age=600",
	}, http.StatusCreated))
	require.NotEmpty(t, created.ETag)
	assert.Equal(t, "10m0s", created.TTL.Default.String())
	c.call(http.MethodPost, "/cache/policies", map[string]any{"name": "short"}, http.StatusConflict)
	c.call(http.MethodPut, "/cache/policies/standard", map[string]any{"name": "standard"}, http.StatusConflict)
	c.callIfMatch(http.MethodPut, "/cache/policies/short", `"stale"`, map[string]any{"name": "short", "comment": "x"}, http.StatusPreconditionFailed)
	updated := decodeAs[cache.Policy](t, c.callIfMatch(http.MethodPut, "/cache/policies/short", `"`+created.ETag+`"`, map[string]any{
		"name": "short", "comment": "ten minutes, stale 1 min", "ttl": map[string]any{"default": "10m"}, "browserCacheControl": "public, max-age=600",
		"staleWhileRevalidate": "1m",
	}, http.StatusOK))
	assert.NotEqual(t, created.ETag, updated.ETag)
	got := decodeAs[cache.Policy](t, c.call(http.MethodGet, "/cache/policies/short", nil, http.StatusOK))
	assert.Equal(t, "ten minutes, stale 1 min", got.Comment)
	c.call(http.MethodGet, "/cache/policies/nope", nil, http.StatusNotFound)

	// Settings of the cluster.
	global := decodeAs[cache.GlobalConfig](t, c.call(http.MethodGet, "/cache/settings", nil, http.StatusOK))
	assert.Empty(t, global.ETag, "never stored")
	global = decodeAs[cache.GlobalConfig](t, c.call(http.MethodPut, "/cache/settings",
		map[string]any{"cache": map[string]any{"policy": "standard"}}, http.StatusOK))
	global = decodeAs[cache.GlobalConfig](t, c.callIfMatch(http.MethodPut, "/cache/settings", global.ETag,
		map[string]any{"cache": map[string]any{"policy": "standard"}, "nodeHeader": true}, http.StatusOK))
	assert.True(t, global.NodeHeader)
	c.callIfMatch(http.MethodPut, "/cache/settings", "stale", map[string]any{"cache": map[string]any{}}, http.StatusPreconditionFailed)

	// Settings of the distribution: a policy for www, no cache for the
	// previews, a year for the assets.
	cur := decodeAs[cache.DistributionConfig](t, c.call(http.MethodGet, "/distributions/site/cache", nil, http.StatusOK))
	assert.Equal(t, "site", cur.Distribution)
	assert.Empty(t, cur.ETag, "no settings yet")
	cfg := decodeAs[cache.DistributionConfig](t, c.call(http.MethodPut, "/distributions/site/cache", map[string]any{
		"cache": map[string]any{"enabled": true},
		"domains": []map[string]any{
			{"host": "www.site.test", "cache": map[string]any{"policy": "short", "quota": "64MiB"}},
			{"host": "preview.site.test", "cache": map[string]any{"enabled": false}},
		},
		"behaviors": []map[string]any{
			{"pathPattern": map[string]any{"type": "glob", "pattern": "/assets/**"}, "cache": map[string]any{"ttl": map[string]any{"default": "365d", "max": "365d"}}},
		},
	}, http.StatusOK))
	require.Len(t, cfg.Domains, 2)
	require.NotEmpty(t, cfg.ETag)
	c.callIfMatch(http.MethodPut, "/distributions/site/cache", "stale", map[string]any{"cache": map[string]any{}}, http.StatusPreconditionFailed)
	c.call(http.MethodPut, "/distributions/site/cache", map[string]any{"cache": map[string]any{},
		"domains": []map[string]any{{"host": "other.test", "cache": map[string]any{}}}}, http.StatusBadRequest)
	c.call(http.MethodGet, "/distributions/nope/cache", nil, http.StatusNotFound)
	c.call(http.MethodDelete, "/cache/policies/short", nil, http.StatusConflict)

	// TCC-01: the effective policy and its levels.
	eff := decodeAs[cache.EffectivePolicy](t, c.call(http.MethodGet, "/distributions/site/cache/effective?host=www.site.test&path=/assets/app.js", nil, http.StatusOK))
	assert.True(t, eff.Enabled)
	assert.Equal(t, "short", eff.PolicyName)
	assert.Equal(t, cache.LevelDomain, eff.PolicyLevel)
	assert.Equal(t, 0, eff.Behavior)
	assert.Equal(t, cache.LevelBehavior, eff.Fields["ttl.default"])
	assert.Equal(t, "365d", eff.Policy.TTL.Default.String())
	off := decodeAs[cache.EffectivePolicy](t, c.call(http.MethodGet, "/distributions/site/cache/effective?host=preview.site.test", nil, http.StatusOK))
	assert.False(t, off.Enabled)
	assert.Equal(t, cache.LevelDomain, off.EnabledLevel)

	// TCC-02: the same object on both domains.
	resp, body := hostGet(t, srv.S3URL, "www.site.test", "/")
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	assert.Equal(t, "MISS", resp.Header.Get(cache.HeaderXCache))
	resp, body = hostGet(t, srv.S3URL, "www.site.test", "/")
	assert.Equal(t, "<h1>home</h1>", body)
	assert.Equal(t, "HIT", resp.Header.Get(cache.HeaderXCache))
	assert.Equal(t, "public, max-age=600", resp.Header.Get("Cache-Control"))
	assert.NotEmpty(t, resp.Header.Get(cache.HeaderXCacheNode), "nodeHeader of the cluster settings")
	for range 2 {
		resp, _ = hostGet(t, srv.S3URL, "preview.site.test", "/")
		assert.Equal(t, "BYPASS", resp.Header.Get(cache.HeaderXCache))
	}

	// The inspector agrees: the entry is on the node.
	ins := decodeAs[admin.CacheInspectResponse](t, c.call(http.MethodGet, "/cache/inspect?url="+url.QueryEscape("https://www.site.test/"), nil, http.StatusOK))
	assert.Equal(t, "site", ins.Distribution)
	assert.Equal(t, "index.html", ins.Key)
	assert.False(t, ins.Bypass)
	assert.NotEmpty(t, ins.CacheKey)
	require.Len(t, ins.Nodes, 1)
	assert.True(t, ins.Nodes[0].Entry.Present)
	assert.Positive(t, ins.Nodes[0].Entry.SizeBytes)
	insPath := decodeAs[admin.CacheInspectResponse](t, c.call(http.MethodGet, "/cache/inspect?distribution=site&url=/assets/app.js&header="+
		url.QueryEscape("Accept-Encoding: gzip"), nil, http.StatusOK))
	assert.Equal(t, "www.site.test", insPath.Host)
	assert.False(t, insPath.Nodes[0].Entry.Present)
	insOff := decodeAs[admin.CacheInspectResponse](t, c.call(http.MethodGet, "/cache/inspect?url="+url.QueryEscape("http://preview.site.test/"), nil, http.StatusOK))
	assert.True(t, insOff.Bypass)
	c.call(http.MethodGet, "/cache/inspect?url=/a.js", nil, http.StatusBadRequest)

	// Statistics of the domains.
	// A request is counted once its response is sent: the client may read
	// it first.
	var st admin.CacheStatsResponse
	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		st = decodeAs[admin.CacheStatsResponse](t, c.call(http.MethodGet, "/cache/stats?distribution=site", nil, http.StatusOK))
		assert.GreaterOrEqual(ct, st.Requests["HIT"], int64(1))
		assert.GreaterOrEqual(ct, st.Requests["BYPASS"], int64(2))
	}, 5*time.Second, 10*time.Millisecond)
	require.NotEmpty(t, st.Nodes)
	assert.True(t, st.Nodes[0].Synced)
	hostStats := decodeAs[admin.CacheStatsResponse](t, c.call(http.MethodGet, "/cache/stats?host=www.site.test&top=3", nil, http.StatusOK))
	for _, h := range hostStats.Hosts {
		assert.Equal(t, "www.site.test", h.Host)
	}
	c.call(http.MethodGet, "/cache/stats?distribution=nope", nil, http.StatusNotFound)

	// An invalidation by URL: completed on the node, then a MISS.
	inv := decodeAs[cache.Invalidation](t, c.call(http.MethodPost, "/distributions/site/invalidations",
		map[string]any{"paths": []string{"https://www.site.test/"}}, http.StatusCreated))
	assert.Equal(t, cache.InvalidationCompleted, inv.State)
	assert.Equal(t, "www.site.test", inv.Request.Domain)
	assert.Equal(t, srv.RootUser, inv.Request.Actor)
	byDist := decodeAs[cache.Invalidation](t, c.call(http.MethodGet, "/distributions/site/invalidations/"+inv.ID, nil, http.StatusOK))
	assert.Equal(t, inv.ID, byDist.ID)
	found := decodeAs[cache.Invalidation](t, c.call(http.MethodGet, "/cache/invalidations/"+inv.ID, nil, http.StatusOK))
	assert.Equal(t, "site", found.Request.Distribution)
	history := decodeAs[admin.CacheInvalidationListResponse](t, c.call(http.MethodGet, "/distributions/site/invalidations", nil, http.StatusOK))
	require.NotEmpty(t, history.Items)
	assert.Equal(t, inv.ID, history.Items[0].ID)
	resp, _ = hostGet(t, srv.S3URL, "www.site.test", "/")
	assert.Equal(t, "MISS", resp.Header.Get(cache.HeaderXCache))
	c.call(http.MethodGet, "/cache/invalidations/inv-nope", nil, http.StatusNotFound)
	c.call(http.MethodPost, "/distributions/site/invalidations", map[string]any{}, http.StatusBadRequest)

	// The global purge.
	all := decodeAs[cache.Invalidation](t, c.call(http.MethodPost, "/cache/invalidations", map[string]any{"all": true}, http.StatusCreated))
	assert.Empty(t, all.Request.Distribution)
	globals := decodeAs[admin.CacheInvalidationListResponse](t, c.call(http.MethodGet, "/cache/invalidations", nil, http.StatusOK))
	require.NotEmpty(t, globals.Items)
	c.call(http.MethodPost, "/cache/invalidations", map[string]any{"paths": []string{"/"}}, http.StatusBadRequest)

	// A warm-up, then a HIT.
	job := decodeAs[cache.WarmJob](t, c.call(http.MethodPost, "/distributions/site/cache/warm",
		map[string]any{"domain": "www.site.test", "paths": []string{"/assets/app.js"}, "concurrency": 2}, http.StatusAccepted))
	require.Eventually(t, func() bool {
		job = decodeAs[cache.WarmJob](t, c.call(http.MethodGet, "/distributions/site/cache/warm/"+job.ID, nil, http.StatusOK))
		return job.State == cache.WarmCompleted
	}, 10*time.Second, 20*time.Millisecond)
	require.NotEmpty(t, job.Nodes)
	assert.Equal(t, 1, job.Nodes[0].Done)
	resp, body = hostGet(t, srv.S3URL, "www.site.test", "/assets/app.js")
	assert.Equal(t, "console.log(1)", body)
	assert.Equal(t, "HIT", resp.Header.Get(cache.HeaderXCache))
	c.call(http.MethodGet, "/distributions/site/cache/warm/warm-nope", nil, http.StatusNotFound)

	// Purge webhooks: a generic one receiving the purges.
	var (
		mu     sync.Mutex
		purges []map[string]any
	)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var v map[string]any
		_ = json.NewDecoder(r.Body).Decode(&v)
		mu.Lock()
		purges = append(purges, v)
		mu.Unlock()
	}))
	defer hook.Close()
	added := decodeAs[cache.Webhook](t, c.call(http.MethodPost, "/distributions/site/cache/webhooks",
		map[string]any{"id": "edge", "type": "generic", "url": hook.URL + "/purge", "headers": map[string]string{"X-Token": "abc"}}, http.StatusCreated))
	assert.Equal(t, "edge", added.ID)
	c.call(http.MethodPost, "/distributions/site/cache/webhooks", map[string]any{"id": "edge", "type": "generic", "url": hook.URL}, http.StatusConflict)
	hooks := decodeAs[admin.CacheWebhookListResponse](t, c.call(http.MethodGet, "/distributions/site/cache/webhooks", nil, http.StatusOK))
	require.Len(t, hooks.Items, 1)
	test := decodeAs[admin.CacheWebhookTestResponse](t, c.call(http.MethodPost, "/distributions/site/cache/webhooks/edge/test", nil, http.StatusOK))
	assert.True(t, test.OK, test.Error)
	assert.Equal(t, http.StatusOK, test.Status)
	mu.Lock()
	assert.NotEmpty(t, purges, "the test call reached the endpoint")
	mu.Unlock()
	c.call(http.MethodDelete, "/distributions/site/cache/webhooks/edge", nil, http.StatusNoContent)
	c.call(http.MethodPost, "/distributions/site/cache/webhooks/edge/test", nil, http.StatusNotFound)

	// TD-C-06: a deploy pipeline may invalidate and warm, not change the
	// policies nor read the configuration.
	c.call(http.MethodPost, "/policies", map[string]any{"name": "cache-purge", "document": map[string]any{
		"Version": "2012-10-17", "Statement": []map[string]any{{"Effect": "Allow", "Action": []string{"admin:InvalidateCache"}, "Resource": []string{"arn:aws:s3:::*"}}},
	}}, http.StatusCreated)
	c.call(http.MethodPost, "/users", map[string]any{"accessKey": "deployer", "secretKey": "deployer-secret", "policyNames": []string{"cache-purge"}}, http.StatusCreated)
	deployer := newConsole(t, srv, "deployer", "deployer-secret")
	deployer.call(http.MethodPost, "/distributions/site/invalidations", map[string]any{"patterns": []map[string]any{{"type": "glob", "pattern": "/assets/**"}}}, http.StatusCreated)
	deployer.call(http.MethodPost, "/distributions/site/cache/warm", map[string]any{"prefix": "/assets/"}, http.StatusAccepted)
	deployer.call(http.MethodPut, "/cache/policies/short", map[string]any{"name": "short"}, http.StatusForbidden)
	deployer.call(http.MethodGet, "/cache/policies", nil, http.StatusForbidden)
	deployer.call(http.MethodPut, "/distributions/site/cache", map[string]any{"cache": map[string]any{}}, http.StatusForbidden)

	// A removed distribution takes its cache settings with it.
	c.call(http.MethodDelete, "/distributions/site", nil, http.StatusNoContent)
	c.call(http.MethodDelete, "/cache/policies/short", nil, http.StatusNoContent)
}
