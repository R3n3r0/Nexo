package nexoadmin_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

func boolPtr(b bool) *bool { return &b }

func strPtr(s string) *string { return &s }

// TestCachePolicyJSON: the inline settings of a policy and of a level
// are flat members, the unset ones absent.
func TestCachePolicyJSON(t *testing.T) {
	p := nexoadmin.CachePolicy{Name: "long", CachePolicySpec: nexoadmin.CachePolicySpec{
		TTL: &nexoadmin.CacheTTL{Default: "1d"}, BrowserCacheControl: strPtr(""), StaleIfError: "90s",
	}}
	data, err := json.Marshal(p)
	require.NoError(t, err)
	assert.JSONEq(t, `{"name":"long","ttl":{"default":"1d"},"browserCacheControl":"","staleIfError":"90s"}`, string(data))
	var s nexoadmin.CacheSettings
	require.NoError(t, json.Unmarshal([]byte(`{"enabled":false,"policy":"aggressive","quota":"20GiB","ttl":{"max":"30d"}}`), &s))
	assert.False(t, *s.Enabled)
	assert.Equal(t, "aggressive", s.Policy)
	assert.Equal(t, "20GiB", s.Quota)
	assert.Equal(t, "30d", s.TTL.Max)
}

// TestCacheAgainstServer drives the cache methods against a real server,
// every exchange validated against api/openapi.yaml.
func TestCacheAgainstServer(t *testing.T) {
	h := newHarness(t)
	ctx, c := h.ctx, h.root
	_, err := c.CreateBucket(ctx, nexoadmin.CreateBucketRequest{Name: "web"})
	require.NoError(t, err)
	h.put("web", "index.html", "home")
	h.put("web", "assets/app.js", "js")
	_, err = c.CreateDistribution(ctx, nexoadmin.Distribution{
		ID: "site", Enabled: true, Aliases: []string{"www.site.test", "preview.site.test"}, Origin: nexoadmin.Origin{Bucket: "web"},
		DefaultBehavior: nexoadmin.Behavior{Access: nexoadmin.AccessPublic, IndexDocument: "index.html"},
	})
	require.NoError(t, err)

	// Policies.
	list, err := c.ListCachePolicies(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(list), 5)
	std, err := c.GetCachePolicy(ctx, nexoadmin.CachePolicyStandard)
	require.NoError(t, err)
	assert.True(t, std.Builtin)
	assert.Equal(t, "1h0m0s", std.TTL.Default)
	preview, err := c.PreviewCachePolicy(ctx, nexoadmin.CachePolicy{Name: "x", CachePolicySpec: nexoadmin.CachePolicySpec{BrowserCacheControl: strPtr("public, max-age=60")}})
	require.NoError(t, err)
	assert.Equal(t, "public, max-age=60", preview.CacheControl)
	_, err = c.PreviewCachePolicy(ctx, nexoadmin.CachePolicy{Name: "x", CachePolicySpec: nexoadmin.CachePolicySpec{TTL: &nexoadmin.CacheTTL{Default: "1h", Max: "1m"}}})
	assert.Equal(t, nexoadmin.CodeInvalidCacheRequest, apiError(t, err).Code)
	p, err := c.CreateCachePolicy(ctx, nexoadmin.CachePolicy{Name: "short", CachePolicySpec: nexoadmin.CachePolicySpec{
		TTL: &nexoadmin.CacheTTL{Default: "10m"}, BrowserCacheControl: strPtr("public, max-age=600"),
		CacheKey: &nexoadmin.CacheKeyPolicy{QueryString: &nexoadmin.CacheQueryString{Mode: "allowlist", Params: []string{"v"}}},
	}})
	require.NoError(t, err)
	require.NotEmpty(t, p.ETag)
	assert.Equal(t, []string{"v"}, p.CacheKey.QueryString.Params)
	p.Comment = "ten minutes"
	updated, err := c.UpdateCachePolicy(ctx, *p, p.ETag)
	require.NoError(t, err)
	_, err = c.UpdateCachePolicy(ctx, *p, p.ETag)
	assert.True(t, nexoadmin.IsPreconditionFailed(err))
	_, err = c.CreateCachePolicy(ctx, nexoadmin.CachePolicy{Name: "short"})
	assert.Equal(t, nexoadmin.CodeAlreadyExists, apiError(t, err).Code)
	assert.Equal(t, "ten minutes", updated.Comment)

	// Settings of the cluster and of the distribution.
	g, err := c.GetCacheSettings(ctx)
	require.NoError(t, err)
	g.NodeHeader = true
	_, err = c.SetCacheSettings(ctx, *g, g.ETag)
	require.NoError(t, err)
	cfg, err := c.GetDistributionCache(ctx, "site")
	require.NoError(t, err)
	cfg.Domains = []nexoadmin.CacheDomain{
		{Host: "www.site.test", Cache: nexoadmin.CacheSettings{Policy: "short", Quota: "32MiB"}},
		{Host: "preview.site.test", Cache: nexoadmin.CacheSettings{Enabled: boolPtr(false)}},
	}
	cfg.Behaviors = []nexoadmin.CacheBehavior{{PathPattern: nexoadmin.KeyPattern{Type: nexoadmin.PatternGlob, Pattern: "/assets/**"},
		Cache: nexoadmin.CacheSettings{CachePolicySpec: nexoadmin.CachePolicySpec{TTL: &nexoadmin.CacheTTL{Default: "365d", Max: "365d"}}}}}
	cfg, err = c.SetDistributionCache(ctx, *cfg, cfg.ETag)
	require.NoError(t, err)
	assert.Equal(t, "32MiB", cfg.Domains[0].Cache.Quota)
	_, err = c.GetDistributionCache(ctx, "nope")
	assert.Equal(t, nexoadmin.CodeDistributionNotFound, apiError(t, err).Code)
	eff, err := c.EffectiveCachePolicy(ctx, "site", "www.site.test", "/assets/app.js")
	require.NoError(t, err)
	assert.Equal(t, "short", eff.PolicyName)
	assert.Equal(t, "behavior", eff.Fields["ttl.default"])
	assert.Equal(t, "365d", eff.Policy.TTL.Default)

	// Answers, inspector, statistics.
	status, _ := h.get("www.site.test", "/")
	require.Equal(t, http.StatusOK, status)
	ins, err := c.InspectCache(ctx, nexoadmin.CacheInspectRequest{URL: "/", Distribution: "site", Headers: []string{"Accept-Encoding: gzip"}})
	require.NoError(t, err)
	require.Len(t, ins.Nodes, 1)
	assert.True(t, ins.Nodes[0].Entry.Present)
	st, err := c.CacheStats(ctx, nexoadmin.CacheStatsQuery{Distribution: "site", Top: 5})
	require.NoError(t, err)
	assert.Equal(t, int64(1), st.Requests[nexoadmin.XCacheMiss])
	require.NotEmpty(t, st.Nodes)

	// Invalidations and warm-ups.
	inv, err := c.InvalidateCache(ctx, "site", nexoadmin.CacheInvalidationRequest{Domain: "www.site.test",
		Patterns: []nexoadmin.KeyPattern{{Type: nexoadmin.PatternGlob, Pattern: "/**"}}})
	require.NoError(t, err)
	assert.Equal(t, nexoadmin.InvalidationCompleted, inv.State)
	assert.Equal(t, h.srv.RootUser, inv.Request.Actor)
	found, err := c.GetCacheInvalidation(ctx, "", inv.ID)
	require.NoError(t, err)
	assert.Equal(t, "site", found.Request.Distribution)
	byDist, err := c.GetCacheInvalidation(ctx, "site", inv.ID)
	require.NoError(t, err)
	assert.Equal(t, inv.ID, byDist.ID)
	history, err := c.ListCacheInvalidations(ctx, "site")
	require.NoError(t, err)
	require.NotEmpty(t, history)
	all, err := c.InvalidateCache(ctx, "", nexoadmin.CacheInvalidationRequest{All: true, Soft: true})
	require.NoError(t, err)
	assert.Empty(t, all.Request.Distribution)
	globals, err := c.ListCacheInvalidations(ctx, "")
	require.NoError(t, err)
	assert.NotEmpty(t, globals)
	_, err = c.GetCacheInvalidation(ctx, "", "inv-nope")
	assert.Equal(t, nexoadmin.CodeInvalidationNotFound, apiError(t, err).Code)
	job, err := c.WarmCache(ctx, "site", nexoadmin.CacheWarmRequest{Prefix: "/assets/", Bandwidth: "1MiB"})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		job, err = c.GetCacheWarmJob(ctx, "site", job.ID)
		require.NoError(t, err)
		return job.State == nexoadmin.WarmCompleted
	}, 10*time.Second, 20*time.Millisecond)
	assert.Equal(t, "1MiB", job.Request.Bandwidth)
	assert.Equal(t, 1, job.Nodes[0].Done)

	// Webhooks.
	hook := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer hook.Close()
	w, err := c.AddCacheWebhook(ctx, "site", nexoadmin.CacheWebhook{ID: "edge", Type: nexoadmin.WebhookNginx, URL: hook.URL})
	require.NoError(t, err)
	assert.Equal(t, "edge", w.ID)
	hooks, err := c.ListCacheWebhooks(ctx, "site")
	require.NoError(t, err)
	require.Len(t, hooks, 1)
	res, err := c.TestCacheWebhook(ctx, "site", "edge")
	require.NoError(t, err)
	assert.True(t, res.OK, res.Error)
	require.NoError(t, c.DeleteCacheWebhook(ctx, "site", "edge"))
	_, err = c.TestCacheWebhook(ctx, "site", "edge")
	assert.Equal(t, nexoadmin.CodeWebhookNotFound, apiError(t, err).Code)

	// A policy in use cannot be removed.
	err = c.DeleteCachePolicy(ctx, "short")
	assert.Equal(t, nexoadmin.CodeCachePolicyInUse, apiError(t, err).Code)
	cfg, err = c.GetDistributionCache(ctx, "site")
	require.NoError(t, err)
	cfg.Domains = nil
	_, err = c.SetDistributionCache(ctx, *cfg, cfg.ETag)
	require.NoError(t, err)
	require.NoError(t, c.DeleteCachePolicy(ctx, "short"))
	t.Logf("operations exercised: %v", h.contract.operations())
}
