package admin_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	adminsvc "github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/cache"
	"github.com/r3n3r0/nexo/internal/service/distribution"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/admin"
)

// TestCacheAuthorizationDenied: every cache route asks its action first
// and answers 403 access_denied when it is denied, the service never
// called. The configuration and the diagnostics need admin:ManageCache,
// the operations on the content admin:InvalidateCache alone (TD-C-06).
func TestCacheAuthorizationDenied(t *testing.T) {
	manage, invalidate := policy.ActionAdminManageCache, policy.ActionAdminInvalidateCache
	tests := []struct {
		method, path, body string
		action             policy.Action
	}{
		{http.MethodGet, "/cache/policies", "", manage},
		{http.MethodPost, "/cache/policies", `{"name":"p1"}`, manage},
		{http.MethodGet, "/cache/policies/p1", "", manage},
		{http.MethodPut, "/cache/policies/p1", `{"name":"p1"}`, manage},
		{http.MethodDelete, "/cache/policies/p1", "", manage},
		{http.MethodPost, "/cache/preview", `{"name":"p1"}`, manage},
		{http.MethodGet, "/cache/settings", "", manage},
		{http.MethodPut, "/cache/settings", `{"cache":{}}`, manage},
		{http.MethodGet, "/cache/inspect?url=https://a.test/", "", manage},
		{http.MethodGet, "/cache/stats", "", manage},
		{http.MethodGet, "/distributions/d1/cache", "", manage},
		{http.MethodPut, "/distributions/d1/cache", `{"distribution":"d1","cache":{}}`, manage},
		{http.MethodGet, "/distributions/d1/cache/effective", "", manage},
		{http.MethodGet, "/distributions/d1/cache/webhooks", "", manage},
		{http.MethodPost, "/distributions/d1/cache/webhooks", `{"id":"w","type":"generic"}`, manage},
		{http.MethodDelete, "/distributions/d1/cache/webhooks/w", "", manage},
		{http.MethodPost, "/distributions/d1/cache/webhooks/w/test", "", manage},
		{http.MethodPost, "/cache/invalidations", `{"all":true}`, invalidate},
		{http.MethodGet, "/cache/invalidations", "", invalidate},
		{http.MethodGet, "/cache/invalidations/inv-1", "", invalidate},
		{http.MethodPost, "/distributions/d1/invalidations", `{"paths":["/a"]}`, invalidate},
		{http.MethodGet, "/distributions/d1/invalidations", "", invalidate},
		{http.MethodGet, "/distributions/d1/invalidations/inv-1", "", invalidate},
		{http.MethodPost, "/distributions/d1/cache/warm", `{"prefix":"/"}`, invalidate},
		{http.MethodGet, "/distributions/d1/cache/warm/warm-1", "", invalidate},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			f := newFixture(t)
			c := f.session(t, bobKey, bobSecret)
			f.denyAll()
			var body any
			if tt.body != "" {
				body = tt.body
			}
			rec := c.do(tt.method, tt.path, body)
			require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
			p := problemOf(t, rec)
			assert.Equal(t, "access_denied", p.Code)
			assert.Contains(t, p.Detail, string(tt.action))
			in := f.lastCheck()
			assert.Equal(t, []policy.Action{tt.action}, in.Actions)
			assert.Empty(t, in.Bucket)
		})
	}
}

func TestCachePolicyRoutes(t *testing.T) {
	ttl := cache.Duration(24 * time.Hour)
	p := cache.Policy{Name: "long", Comment: "one day", ETag: "e1", PolicySpec: cache.PolicySpec{TTL: &cache.TTL{Default: &ttl}}}
	t.Run("list", func(t *testing.T) {
		f := newFixture(t)
		f.cache.EXPECT().ListPolicies(mock.Anything).Return(nil, nil).Once()
		rec := f.root(t).do(http.MethodGet, "/cache/policies", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.JSONEq(t, `{"items":[]}`, rec.Body.String())
	})
	t.Run("create with the ETag, durations readable", func(t *testing.T) {
		f := newFixture(t)
		f.cache.EXPECT().CreatePolicy(mock.Anything, mock.MatchedBy(func(in cache.Policy) bool {
			return in.Name == "long" && in.TTL != nil && *in.TTL.Default == ttl && in.StaleIfError != nil && in.StaleIfError.D() == 90*time.Second
		})).Return(p, nil).Once()
		rec := f.root(t).do(http.MethodPost, "/cache/policies", `{"name":"long","ttl":{"default":"1d"},"staleIfError":90}`)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		assert.Equal(t, `"e1"`, rec.Header().Get("ETag"))
		assert.Contains(t, rec.Body.String(), `"default":"1d"`)
	})
	t.Run("an invalid duration", func(t *testing.T) {
		f := newFixture(t)
		rec := f.root(t).do(http.MethodPost, "/cache/policies", `{"name":"long","ttl":{"default":"forever"}}`)
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	})
	t.Run("update with If-Match, the name of the path", func(t *testing.T) {
		f := newFixture(t)
		f.cache.EXPECT().UpdatePolicy(mock.Anything, mock.MatchedBy(func(in cache.Policy) bool { return in.Name == "long" }), "e1").Return(p, nil).Once()
		req := f.root(t)
		r := req.newRequest(http.MethodPut, "/cache/policies/long", `{"comment":"one day"}`)
		r.Header.Set("If-Match", `"e1"`)
		rec := req.serve(r)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		rec = req.do(http.MethodPut, "/cache/policies/long", `{"name":"other"}`)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})
	t.Run("errors", func(t *testing.T) {
		f := newFixture(t)
		c := f.root(t)
		f.cache.EXPECT().GetPolicy(mock.Anything, "nope").Return(cache.Policy{}, fmt.Errorf("x: %w", cache.ErrPolicyNotFound)).Once()
		rec := c.do(http.MethodGet, "/cache/policies/nope", nil)
		assert.Equal(t, http.StatusNotFound, rec.Code)
		assert.Equal(t, "cache_policy_not_found", problemOf(t, rec).Code)
		f.cache.EXPECT().DeletePolicy(mock.Anything, "standard").Return(fmt.Errorf("x: %w", cache.ErrBuiltinPolicy)).Once()
		rec = c.do(http.MethodDelete, "/cache/policies/standard", nil)
		assert.Equal(t, http.StatusConflict, rec.Code)
		assert.Equal(t, "builtin_cache_policy", problemOf(t, rec).Code)
		f.cache.EXPECT().DeletePolicy(mock.Anything, "used").Return(fmt.Errorf("x: %w", cache.ErrPolicyInUse)).Once()
		rec = c.do(http.MethodDelete, "/cache/policies/used", nil)
		assert.Equal(t, "cache_policy_in_use", problemOf(t, rec).Code)
		f.cache.EXPECT().CreatePolicy(mock.Anything, mock.Anything).Return(cache.Policy{}, fmt.Errorf("x: %w", cache.ErrAlreadyExists)).Once()
		rec = c.do(http.MethodPost, "/cache/policies", `{"name":"long"}`)
		assert.Equal(t, http.StatusConflict, rec.Code)
		assert.Equal(t, "already_exists", problemOf(t, rec).Code)
		f.cache.EXPECT().PreviewPolicy(mock.Anything, mock.Anything).Return(cache.PolicyPreview{}, fmt.Errorf("%w: ttl.min > ttl.default", cache.ErrInvalid)).Once()
		rec = c.do(http.MethodPost, "/cache/preview", `{"name":"x"}`)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		pd := problemOf(t, rec)
		assert.Equal(t, "invalid_cache_request", pd.Code)
		assert.Contains(t, pd.Detail, "ttl.min")
		f.cache.EXPECT().DeletePolicy(mock.Anything, "long").Return(nil).Once()
		rec = c.do(http.MethodDelete, "/cache/policies/long", nil)
		assert.Equal(t, http.StatusNoContent, rec.Code)
	})
}

func TestCacheDistributionRoutes(t *testing.T) {
	quota := cache.Size(20 << 30)
	cfg := cache.DistributionConfig{Distribution: "site", ETag: "d1", Domains: []cache.DomainSettings{{Host: "www.site.test", Cache: cache.Settings{Policy: "aggressive", Quota: &quota}}}}
	t.Run("get and update", func(t *testing.T) {
		f := newFixture(t)
		c := f.root(t)
		f.cache.EXPECT().GetDistribution(mock.Anything, "site").Return(cfg, nil).Once()
		rec := c.do(http.MethodGet, "/distributions/site/cache", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, `"d1"`, rec.Header().Get("ETag"))
		assert.Contains(t, rec.Body.String(), `"quota":"20GiB"`)
		f.cache.EXPECT().UpdateDistribution(mock.Anything, mock.MatchedBy(func(in cache.DistributionConfig) bool {
			return in.Distribution == "site" && len(in.Domains) == 1 && *in.Domains[0].Cache.Quota == quota
		}), "d1").Return(cfg, nil).Once()
		r := c.newRequest(http.MethodPut, "/distributions/site/cache", `{"cache":{},"domains":[{"host":"www.site.test","cache":{"policy":"aggressive","quota":"20GiB"}}]}`)
		r.Header.Set("If-Match", "d1")
		rec = c.serve(r)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		rec = c.do(http.MethodPut, "/distributions/site/cache", `{"distribution":"other","cache":{}}`)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
		f.cache.EXPECT().GetDistribution(mock.Anything, "nope").Return(cache.DistributionConfig{}, fmt.Errorf("x: %w", distribution.ErrDistributionNotFound)).Once()
		rec = c.do(http.MethodGet, "/distributions/nope/cache", nil)
		assert.Equal(t, "distribution_not_found", problemOf(t, rec).Code)
	})
	t.Run("effective", func(t *testing.T) {
		f := newFixture(t)
		f.cache.EXPECT().Effective(mock.Anything, cache.EffectiveInput{Distribution: "site", Host: "www.site.test", Path: "/assets/a.js"}).
			Return(cache.EffectivePolicy{Enabled: true, PolicyName: "aggressive", PolicyLevel: cache.LevelDomain, Behavior: -1}, nil).Once()
		rec := f.root(t).do(http.MethodGet, "/distributions/site/cache/effective?host=www.site.test&path=/assets/a.js", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		eff := decode[cache.EffectivePolicy](t, rec)
		assert.Equal(t, cache.LevelDomain, eff.PolicyLevel)
		assert.Contains(t, rec.Body.String(), `"fields":{}`)
	})
	t.Run("webhooks", func(t *testing.T) {
		f := newFixture(t)
		c := f.root(t)
		w := cache.Webhook{ID: "cf", Type: cache.WebhookCloudflare, Zone: "z1", Token: "********"}
		f.cache.EXPECT().AddWebhook(mock.Anything, "site", mock.MatchedBy(func(in cache.Webhook) bool { return in.ID == "cf" && in.Token == "tok" })).Return(w, nil).Once()
		rec := c.do(http.MethodPost, "/distributions/site/cache/webhooks", `{"id":"cf","type":"cloudflare","zone":"z1","token":"tok"}`)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		assert.NotContains(t, rec.Body.String(), "tok\"")
		f.cache.EXPECT().ListWebhooks(mock.Anything, "site").Return([]cache.Webhook{w}, nil).Once()
		rec = c.do(http.MethodGet, "/distributions/site/cache/webhooks", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Len(t, decode[admin.CacheWebhookListResponse](t, rec).Items, 1)
		f.cache.EXPECT().TestWebhook(mock.Anything, "site", "cf").Return(cache.WebhookTestResult{OK: false, Status: 502, Error: "bad gateway", Duration: 1500 * time.Millisecond}, nil).Once()
		rec = c.do(http.MethodPost, "/distributions/site/cache/webhooks/cf/test", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, admin.CacheWebhookTestResponse{Status: 502, Error: "bad gateway", DurationMs: 1500}, decode[admin.CacheWebhookTestResponse](t, rec))
		f.cache.EXPECT().DeleteWebhook(mock.Anything, "site", "nope").Return(fmt.Errorf("x: %w", cache.ErrWebhookNotFound)).Once()
		rec = c.do(http.MethodDelete, "/distributions/site/cache/webhooks/nope", nil)
		assert.Equal(t, "webhook_not_found", problemOf(t, rec).Code)
	})
}

func TestCacheInvalidationRoutes(t *testing.T) {
	created := time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC)
	inv := cache.Invalidation{ID: "inv-1", CreatedAt: created, State: cache.InvalidationInProgress,
		Request: cache.InvalidationRequest{Distribution: "site", Paths: []string{"/a"}},
		Nodes:   []cache.NodeStatus{{Node: "n1", State: cache.NodeCompleted, Removed: 2}, {Node: "n2", State: cache.NodePending, Error: "offline"}}}
	t.Run("invalidate a distribution", func(t *testing.T) {
		f := newFixture(t)
		f.cache.EXPECT().Invalidate(mock.Anything, mock.MatchedBy(func(in adminsvc.InvalidateCacheInput) bool {
			r := in.Request
			return in.Requester.Identity.AccessKey == rootKey && r.Distribution == "site" && r.Domain == "www.site.test" &&
				len(r.Patterns) == 1 && r.Patterns[0].Type == distribution.PatternGlob && r.Soft && len(r.Tags) == 1
		})).Return(inv, nil).Once()
		rec := f.root(t).do(http.MethodPost, "/distributions/site/invalidations",
			`{"domain":"www.site.test","patterns":[{"type":"glob","pattern":"/assets/**"}],"tags":["p42"],"soft":true}`)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		got := decode[cache.Invalidation](t, rec)
		assert.Equal(t, "inv-1", got.ID)
		require.Len(t, got.Nodes, 2)
		assert.Equal(t, 2, got.Nodes[0].Removed)
	})
	t.Run("the actor is not taken from the body", func(t *testing.T) {
		f := newFixture(t)
		rec := f.root(t).do(http.MethodPost, "/distributions/site/invalidations", `{"all":true,"actor":"someone"}`)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "unknown member")
	})
	t.Run("global purge, history and lookup", func(t *testing.T) {
		f := newFixture(t)
		c := f.root(t)
		f.cache.EXPECT().Invalidate(mock.Anything, mock.MatchedBy(func(in adminsvc.InvalidateCacheInput) bool {
			return in.Request.Distribution == "" && in.Request.All
		})).Return(cache.Invalidation{ID: "inv-g", State: cache.InvalidationCompleted}, nil).Once()
		rec := c.do(http.MethodPost, "/cache/invalidations", `{"all":true}`)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		f.cache.EXPECT().ListInvalidations(mock.Anything, "").Return(nil, nil).Once()
		rec = c.do(http.MethodGet, "/cache/invalidations", nil)
		assert.JSONEq(t, `{"items":[]}`, rec.Body.String())
		f.cache.EXPECT().ListInvalidations(mock.Anything, "site").Return([]cache.Invalidation{inv}, nil).Once()
		rec = c.do(http.MethodGet, "/distributions/site/invalidations", nil)
		assert.Len(t, decode[admin.CacheInvalidationListResponse](t, rec).Items, 1)
		f.cache.EXPECT().FindInvalidation(mock.Anything, "inv-1").Return(inv, nil).Once()
		rec = c.do(http.MethodGet, "/cache/invalidations/inv-1", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		f.cache.EXPECT().GetInvalidation(mock.Anything, "site", "inv-9").Return(cache.Invalidation{}, fmt.Errorf("x: %w", cache.ErrInvalidationNotFound)).Once()
		rec = c.do(http.MethodGet, "/distributions/site/invalidations/inv-9", nil)
		assert.Equal(t, http.StatusNotFound, rec.Code)
		assert.Equal(t, "invalidation_not_found", problemOf(t, rec).Code)
	})
	t.Run("warm", func(t *testing.T) {
		f := newFixture(t)
		c := f.root(t)
		f.cache.EXPECT().Warm(mock.Anything, mock.MatchedBy(func(in adminsvc.WarmCacheInput) bool {
			r := in.Request
			return r.Distribution == "site" && r.Prefix == "/assets/" && r.Bandwidth == 10<<20 && r.Concurrency == 2 && in.Requester.Identity.AccessKey == rootKey
		})).Return(cache.WarmJob{ID: "warm-1", State: cache.WarmRunning}, nil).Once()
		rec := c.do(http.MethodPost, "/distributions/site/cache/warm", `{"prefix":"/assets/","bandwidth":"10MiB","concurrency":2}`)
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
		f.cache.EXPECT().GetWarmJob(mock.Anything, "site", "warm-x").Return(cache.WarmJob{}, fmt.Errorf("x: %w", cache.ErrWarmJobNotFound)).Once()
		rec = c.do(http.MethodGet, "/distributions/site/cache/warm/warm-x", nil)
		assert.Equal(t, "warm_job_not_found", problemOf(t, rec).Code)
	})
}

func TestCacheReportRoutes(t *testing.T) {
	t.Run("stats in bytes", func(t *testing.T) {
		f := newFixture(t)
		f.cache.EXPECT().Stats(mock.Anything, cache.StatsQuery{Distribution: "site", Host: "www.site.test", Top: 5}).Return(cache.StatsReport{
			Counters: cache.Counters{Requests: map[cache.Result]int64{cache.ResultHit: 3, cache.ResultMiss: 1}, BytesCache: 30, HitRatio: 0.75},
			Entries:  2, MemorySize: 1 << 20,
			Hosts: []cache.HostStats{{Distribution: "site", Host: "www.site.test", MemorySize: 1 << 20}},
			Nodes: []cache.NodeStats{{Node: "local", Online: true, Mode: cache.ModeLocal, MemoryLimit: 64 << 20, Synced: true}},
		}, nil).Once()
		rec := f.root(t).do(http.MethodGet, "/cache/stats?distribution=site&host=www.site.test&top=5", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		st := decode[admin.CacheStatsResponse](t, rec)
		assert.Equal(t, int64(3), st.Requests["HIT"])
		assert.Equal(t, int64(1<<20), st.MemoryBytes)
		require.Len(t, st.Hosts, 1)
		assert.Equal(t, int64(1<<20), st.Hosts[0].MemoryBytes)
		require.Len(t, st.Nodes, 1)
		assert.Equal(t, int64(64<<20), st.Nodes[0].MemoryLimitBytes)
		assert.NotNil(t, st.Nodes[0].TopObjects, "lists, not null")
		assert.Contains(t, rec.Body.String(), `"hitRatio":0.75`)
		rec = f.root(t).do(http.MethodGet, "/cache/stats?top=-1", nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})
	t.Run("inspect", func(t *testing.T) {
		f := newFixture(t)
		f.cache.EXPECT().Inspect(mock.Anything, mock.MatchedBy(func(in adminsvc.InspectCacheInput) bool {
			return in.URL == "/a.js" && in.Distribution == "site" && in.Header.Get("Accept-Encoding") == "br"
		})).Return(cache.InspectResult{URL: "https://www.site.test/a.js", Host: "www.site.test", Path: "/a.js", CacheKey: "k1", Mode: cache.ModeLocal,
			Nodes: []cache.NodePresence{{Node: "local", Online: true, Entry: cache.Presence{Key: "k1", Present: true, Tier: "memory",
				Age: cache.Duration(90 * time.Second), ExpiresIn: cache.Duration(-30 * time.Second), Stale: true, Size: 2048}}}}, nil).Once()
		rec := f.root(t).do(http.MethodGet, "/cache/inspect?url=/a.js&distribution=site&header=Accept-Encoding:%20br", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		res := decode[admin.CacheInspectResponse](t, rec)
		require.Len(t, res.Nodes, 1)
		e := res.Nodes[0].Entry
		assert.Equal(t, int64(90), e.AgeSeconds)
		assert.Equal(t, int64(-30), e.ExpiresInSeconds)
		assert.Equal(t, int64(2048), e.SizeBytes)
		assert.NotNil(t, e.Encodings)
		rec = f.root(t).do(http.MethodGet, "/cache/inspect?url=/a.js&header=nocolon", nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})
	t.Run("a cache not ready", func(t *testing.T) {
		f := newFixture(t)
		f.cache.EXPECT().GetSettings(mock.Anything).Return(cache.GlobalConfig{}, fmt.Errorf("x: %w", cache.ErrNotReady)).Once()
		rec := f.root(t).do(http.MethodGet, "/cache/settings", nil)
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	})
	t.Run("settings", func(t *testing.T) {
		f := newFixture(t)
		f.cache.EXPECT().UpdateSettings(mock.Anything, mock.MatchedBy(func(g cache.GlobalConfig) bool {
			return g.Cache.Policy == "standard" && g.NodeHeader && g.Debug != nil && g.Debug.Secret == "s3cret"
		}), "g1").Return(cache.GlobalConfig{ETag: "g2", NodeHeader: true}, nil).Once()
		c := f.root(t)
		r := c.newRequest(http.MethodPut, "/cache/settings", `{"cache":{"policy":"standard"},"nodeHeader":true,"debug":{"secret":"s3cret"}}`)
		r.Header.Set("If-Match", `"g1"`)
		rec := c.serve(r)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, `"g2"`, rec.Header().Get("ETag"))
	})
}

func TestCacheConstructorRefusesMissingServices(t *testing.T) {
	_, err := admin.NewCacheRoutes(admin.CacheDeps{})
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
}
