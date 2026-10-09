package admin_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/admin"
	adminmock "github.com/r3n3r0/nexo/internal/service/admin/mock"
	"github.com/r3n3r0/nexo/internal/service/cache"
	cachemock "github.com/r3n3r0/nexo/internal/service/cache/mock"
	"github.com/r3n3r0/nexo/internal/service/distribution"
	distmock "github.com/r3n3r0/nexo/internal/service/distribution/mock"
)

// cacheFixture is the cache manager over mocked services: the
// distribution "site" serves www.site.test, site.test and
// *.preview.site.test.
type cacheFixture struct {
	mgr   *cachemock.Manager
	ops   *cachemock.Operations
	dists *distmock.Service
	m     admin.CacheManager
}

func newCacheFixture(t *testing.T, opts ...admin.CacheManagerOption) *cacheFixture {
	t.Helper()
	f := &cacheFixture{mgr: cachemock.NewManager(t), ops: cachemock.NewOperations(t), dists: distmock.NewService(t)}
	site := distribution.Distribution{ID: "site", Enabled: true, Aliases: []string{"*.preview.site.test", "www.site.test", "site.test"}}
	f.dists.EXPECT().Get(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, id string) (distribution.Distribution, error) {
		if id == site.ID {
			return site, nil
		}
		return distribution.Distribution{}, fmt.Errorf("%w: %s", distribution.ErrDistributionNotFound, id)
	}).Maybe()
	f.dists.EXPECT().List(mock.Anything).Return([]distribution.Distribution{{ID: "blog"}, site}, nil).Maybe()
	f.dists.EXPECT().Resolve(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, host string) distribution.Route {
		switch host {
		case "www.site.test", "site.test", "a.preview.site.test":
			return distribution.Route{Kind: distribution.RouteDistribution, Host: host, Distribution: &site}
		case "blog.test":
			return distribution.Route{Kind: distribution.RouteDistribution, Host: host, Distribution: &distribution.Distribution{ID: "blog"}}
		}
		return distribution.Route{Kind: distribution.RoutePathStyle, Host: host}
	}).Maybe()
	var err error
	f.m, err = admin.NewCacheManager(admin.CacheManagerDeps{Manager: f.mgr, Operations: f.ops, Distributions: f.dists}, opts...)
	require.NoError(t, err)
	return f
}

func TestNewCacheManagerRequiresDeps(t *testing.T) {
	for _, deps := range []admin.CacheManagerDeps{
		{},
		{Manager: cachemock.NewManager(t), Operations: cachemock.NewOperations(t)},
		{Manager: cachemock.NewManager(t), Distributions: distmock.NewService(t)},
		{Operations: cachemock.NewOperations(t), Distributions: distmock.NewService(t)},
	} {
		_, err := admin.NewCacheManager(deps)
		require.ErrorIs(t, err, domain.ErrInvalidArgument)
	}
}

func TestCacheManagerInvalidate(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name    string
		in      cache.InvalidationRequest
		want    cache.InvalidationRequest
		wantErr error
	}{
		{
			name: "paths and patterns as given",
			in:   cache.InvalidationRequest{Distribution: "site", Domain: "www.site.test", Paths: []string{"/index.html"}, Soft: true},
			want: cache.InvalidationRequest{Distribution: "site", Domain: "www.site.test", Paths: []string{"/index.html"}, Soft: true, Actor: "mario"},
		},
		{
			name: "absolute URLs set the domain",
			in:   cache.InvalidationRequest{Distribution: "site", Paths: []string{"https://site.test/a%20b.js", "http://site.test", "/c.css"}},
			want: cache.InvalidationRequest{Distribution: "site", Domain: "site.test", Paths: []string{"/a b.js", "/", "/c.css"}, Actor: "mario"},
		},
		{
			name: "a URL under the wildcard domain",
			in:   cache.InvalidationRequest{Distribution: "site", Domain: "*.preview.site.test", Paths: []string{"https://a.preview.site.test/x"}},
			want: cache.InvalidationRequest{Distribution: "site", Domain: "*.preview.site.test", Paths: []string{"/x"}, Actor: "mario"},
		},
		{
			name:    "URLs of two domains",
			in:      cache.InvalidationRequest{Distribution: "site", Paths: []string{"https://site.test/a", "https://www.site.test/b"}},
			wantErr: domain.ErrInvalidArgument,
		},
		{
			name:    "a URL of another distribution",
			in:      cache.InvalidationRequest{Distribution: "site", Paths: []string{"https://blog.test/a"}},
			wantErr: domain.ErrInvalidArgument,
		},
		{
			name:    "a malformed URL",
			in:      cache.InvalidationRequest{Distribution: "site", Paths: []string{"https:// bad"}},
			wantErr: domain.ErrInvalidArgument,
		},
		{
			name:    "an unknown distribution",
			in:      cache.InvalidationRequest{Distribution: "nope", All: true},
			wantErr: distribution.ErrDistributionNotFound,
		},
		{
			name: "the global purge",
			in:   cache.InvalidationRequest{All: true},
			want: cache.InvalidationRequest{All: true, Actor: "mario"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newCacheFixture(t)
			if tc.wantErr == nil {
				f.ops.EXPECT().Invalidate(mock.Anything, tc.want).Return(cache.Invalidation{ID: "inv-1", Request: tc.want}, nil).Once()
			}
			inv, err := f.m.Invalidate(ctx, admin.InvalidateCacheInput{Requester: requesterOf("mario"), Request: tc.in})
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "inv-1", inv.ID)
		})
	}
}

func TestCacheManagerFindInvalidation(t *testing.T) {
	ctx := context.Background()
	notFound := fmt.Errorf("x: %w", cache.ErrInvalidationNotFound)
	t.Run("global", func(t *testing.T) {
		f := newCacheFixture(t)
		f.ops.EXPECT().GetInvalidation(mock.Anything, "", "inv-g").Return(cache.Invalidation{ID: "inv-g"}, nil).Once()
		inv, err := f.m.FindInvalidation(ctx, "inv-g")
		require.NoError(t, err)
		assert.Equal(t, "inv-g", inv.ID)
	})
	t.Run("of a distribution", func(t *testing.T) {
		f := newCacheFixture(t)
		f.ops.EXPECT().GetInvalidation(mock.Anything, "", "inv-s").Return(cache.Invalidation{}, notFound).Once()
		f.ops.EXPECT().GetInvalidation(mock.Anything, "blog", "inv-s").Return(cache.Invalidation{}, notFound).Once()
		f.ops.EXPECT().GetInvalidation(mock.Anything, "site", "inv-s").Return(cache.Invalidation{ID: "inv-s", Request: cache.InvalidationRequest{Distribution: "site"}}, nil).Once()
		inv, err := f.m.FindInvalidation(ctx, "inv-s")
		require.NoError(t, err)
		assert.Equal(t, "site", inv.Request.Distribution)
	})
	t.Run("nowhere", func(t *testing.T) {
		f := newCacheFixture(t)
		f.ops.EXPECT().GetInvalidation(mock.Anything, mock.Anything, "inv-x").Return(cache.Invalidation{}, notFound).Times(3)
		_, err := f.m.FindInvalidation(ctx, "inv-x")
		require.ErrorIs(t, err, cache.ErrInvalidationNotFound)
		require.ErrorIs(t, err, domain.ErrNotFound)
	})
	t.Run("a failure stops the search", func(t *testing.T) {
		f := newCacheFixture(t)
		f.ops.EXPECT().GetInvalidation(mock.Anything, "", "inv-y").Return(cache.Invalidation{}, domain.ErrNodeOffline).Once()
		_, err := f.m.FindInvalidation(ctx, "inv-y")
		require.ErrorIs(t, err, domain.ErrNodeOffline)
	})
}

func TestCacheManagerHistoryChecksTheDistribution(t *testing.T) {
	ctx := context.Background()
	f := newCacheFixture(t)
	_, err := f.m.ListInvalidations(ctx, "nope")
	require.ErrorIs(t, err, distribution.ErrDistributionNotFound)
	_, err = f.m.GetInvalidation(ctx, "nope", "inv-1")
	require.ErrorIs(t, err, distribution.ErrDistributionNotFound)
	_, err = f.m.GetWarmJob(ctx, "nope", "warm-1")
	require.ErrorIs(t, err, distribution.ErrDistributionNotFound)

	f.ops.EXPECT().ListInvalidations(mock.Anything, "site").Return(nil, nil).Once()
	list, err := f.m.ListInvalidations(ctx, "site")
	require.NoError(t, err)
	assert.NotNil(t, list, "an empty history, not nil")
	f.ops.EXPECT().ListInvalidations(mock.Anything, "").Return([]cache.Invalidation{{ID: "inv-g"}}, nil).Once()
	list, err = f.m.ListInvalidations(ctx, "")
	require.NoError(t, err)
	assert.Len(t, list, 1)
	f.ops.EXPECT().GetWarmJob(mock.Anything, "site", "warm-1").Return(cache.WarmJob{ID: "warm-1"}, nil).Once()
	job, err := f.m.GetWarmJob(ctx, "site", "warm-1")
	require.NoError(t, err)
	assert.Equal(t, "warm-1", job.ID)
}

func TestCacheManagerWarm(t *testing.T) {
	ctx := context.Background()
	f := newCacheFixture(t)
	req := cache.WarmRequest{Distribution: "site", Prefix: "/assets/", Concurrency: 2}
	want := req
	want.Actor = "mario/AK1"
	f.ops.EXPECT().Warm(mock.Anything, want).Return(cache.WarmJob{ID: "warm-1", Request: want}, nil).Once()
	job, err := f.m.Warm(ctx, admin.WarmCacheInput{Requester: admin.Requester{Identity: domain.Identity{AccessKey: "AK1", Parent: "mario"}}, Request: req})
	require.NoError(t, err)
	assert.Equal(t, "mario/AK1", job.Request.Actor)
	_, err = f.m.Warm(ctx, admin.WarmCacheInput{Request: cache.WarmRequest{Distribution: "site", Prefix: "/", Concurrency: -1}})
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
}

func TestCacheManagerInspect(t *testing.T) {
	ctx := context.Background()
	header := http.Header{"Accept-Encoding": {"gzip"}}
	tests := []struct {
		name    string
		in      admin.InspectCacheInput
		url     string
		wantErr error
	}{
		{name: "absolute URL", in: admin.InspectCacheInput{URL: " https://site.test/a.js "}, url: "https://site.test/a.js"},
		{name: "a path of a distribution", in: admin.InspectCacheInput{URL: "/a.js", Distribution: "site"}, url: "https://www.site.test/a.js"},
		{name: "a URL of the distribution", in: admin.InspectCacheInput{URL: "http://site.test/a.js", Distribution: "site"}, url: "http://site.test/a.js"},
		{name: "a URL of another distribution", in: admin.InspectCacheInput{URL: "http://blog.test/a.js", Distribution: "site"}, wantErr: domain.ErrInvalidArgument},
		{name: "a path alone", in: admin.InspectCacheInput{URL: "/a.js"}, wantErr: domain.ErrInvalidArgument},
		{name: "nothing", in: admin.InspectCacheInput{}, wantErr: domain.ErrInvalidArgument},
		{name: "an unknown distribution", in: admin.InspectCacheInput{URL: "/a.js", Distribution: "nope"}, wantErr: distribution.ErrDistributionNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newCacheFixture(t)
			tc.in.Header = header
			if tc.wantErr == nil {
				f.ops.EXPECT().Inspect(mock.Anything, cache.InspectInput{URL: tc.url, Header: header}).Return(cache.InspectResult{URL: tc.url}, nil).Once()
			}
			res, err := f.m.Inspect(ctx, tc.in)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.url, res.URL)
		})
	}
}

func TestCacheManagerStats(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name    string
		q       cache.StatsQuery
		want    cache.StatsQuery
		wantErr error
	}{
		{name: "cluster", q: cache.StatsQuery{}, want: cache.StatsQuery{}},
		{name: "distribution", q: cache.StatsQuery{Distribution: "site", Top: 5}, want: cache.StatsQuery{Distribution: "site", Top: 5}},
		{name: "host alone", q: cache.StatsQuery{Host: "WWW.site.test"}, want: cache.StatsQuery{Distribution: "site", Host: "www.site.test"}},
		{name: "host of no distribution", q: cache.StatsQuery{Host: "nowhere.test"}, wantErr: domain.ErrInvalidArgument},
		{name: "unknown distribution", q: cache.StatsQuery{Distribution: "nope"}, wantErr: distribution.ErrDistributionNotFound},
		{name: "top too large", q: cache.StatsQuery{Top: 11}, wantErr: domain.ErrInvalidArgument},
		{name: "negative top", q: cache.StatsQuery{Top: -1}, wantErr: domain.ErrInvalidArgument},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newCacheFixture(t, admin.WithMaxStatsTop(10))
			if tc.wantErr == nil {
				f.ops.EXPECT().Stats(mock.Anything, tc.want).Return(cache.StatsReport{Entries: 3}, nil).Once()
			}
			rep, err := f.m.Stats(ctx, tc.q)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, 3, rep.Entries)
		})
	}
}

func TestCacheManagerEffectiveCompletesThePath(t *testing.T) {
	ctx := context.Background()
	f := newCacheFixture(t)
	f.mgr.EXPECT().Effective(mock.Anything, cache.EffectiveInput{Distribution: "site", Host: "www.site.test", Path: "/"}).
		Return(cache.EffectivePolicy{PolicyName: "standard"}, nil).Once()
	f.mgr.EXPECT().Effective(mock.Anything, cache.EffectiveInput{Distribution: "site", Path: "/assets/a.js"}).
		Return(cache.EffectivePolicy{PolicyName: "aggressive"}, nil).Once()
	eff, err := f.m.Effective(ctx, cache.EffectiveInput{Distribution: "site", Host: " www.site.test"})
	require.NoError(t, err)
	assert.Equal(t, "standard", eff.PolicyName)
	eff, err = f.m.Effective(ctx, cache.EffectiveInput{Distribution: "site", Path: "assets/a.js"})
	require.NoError(t, err)
	assert.Equal(t, "aggressive", eff.PolicyName)
}

// TestCacheManagerDelegates checks that the configuration and the
// webhooks reach the cache unchanged.
func TestCacheManagerDelegates(t *testing.T) {
	ctx := context.Background()
	f := newCacheFixture(t)
	p := cache.Policy{Name: "long"}
	f.mgr.EXPECT().ListPolicies(mock.Anything).Return([]cache.Policy{p}, nil).Once()
	f.mgr.EXPECT().GetPolicy(mock.Anything, "long").Return(p, nil).Once()
	f.mgr.EXPECT().CreatePolicy(mock.Anything, p).Return(p, nil).Once()
	f.mgr.EXPECT().UpdatePolicy(mock.Anything, p, "e1").Return(p, nil).Once()
	f.mgr.EXPECT().DeletePolicy(mock.Anything, "long").Return(nil).Once()
	f.mgr.EXPECT().PreviewPolicy(mock.Anything, p).Return(cache.PolicyPreview{CacheControl: "max-age=60"}, nil).Once()
	f.mgr.EXPECT().GetGlobal(mock.Anything).Return(cache.GlobalConfig{ETag: "g1"}, nil).Once()
	f.mgr.EXPECT().UpdateGlobal(mock.Anything, cache.GlobalConfig{NodeHeader: true}, "g1").Return(cache.GlobalConfig{ETag: "g2"}, nil).Once()
	cfg := cache.DistributionConfig{Distribution: "site"}
	f.mgr.EXPECT().GetDistribution(mock.Anything, "site").Return(cfg, nil).Once()
	f.mgr.EXPECT().UpdateDistribution(mock.Anything, cfg, "d1").Return(cfg, nil).Once()
	w := cache.Webhook{ID: "cf", Type: cache.WebhookCloudflare}
	f.mgr.EXPECT().ListWebhooks(mock.Anything, "site").Return(nil, nil).Once()
	f.mgr.EXPECT().AddWebhook(mock.Anything, "site", w).Return(w, nil).Once()
	f.mgr.EXPECT().DeleteWebhook(mock.Anything, "site", "cf").Return(nil).Once()
	f.mgr.EXPECT().TestWebhook(mock.Anything, "site", "cf").Return(cache.WebhookTestResult{OK: true}, nil).Once()

	_, err := f.m.ListPolicies(ctx)
	require.NoError(t, err)
	_, err = f.m.GetPolicy(ctx, "long")
	require.NoError(t, err)
	_, err = f.m.CreatePolicy(ctx, p)
	require.NoError(t, err)
	_, err = f.m.UpdatePolicy(ctx, p, "e1")
	require.NoError(t, err)
	require.NoError(t, f.m.DeletePolicy(ctx, "long"))
	prev, err := f.m.PreviewPolicy(ctx, p)
	require.NoError(t, err)
	assert.Equal(t, "max-age=60", prev.CacheControl)
	g, err := f.m.GetSettings(ctx)
	require.NoError(t, err)
	g, err = f.m.UpdateSettings(ctx, cache.GlobalConfig{NodeHeader: true}, g.ETag)
	require.NoError(t, err)
	assert.Equal(t, "g2", g.ETag)
	_, err = f.m.GetDistribution(ctx, "site")
	require.NoError(t, err)
	_, err = f.m.UpdateDistribution(ctx, cfg, "d1")
	require.NoError(t, err)
	hooks, err := f.m.ListWebhooks(ctx, "site")
	require.NoError(t, err)
	assert.NotNil(t, hooks)
	_, err = f.m.AddWebhook(ctx, "site", w)
	require.NoError(t, err)
	require.NoError(t, f.m.DeleteWebhook(ctx, "site", "cf"))
	res, err := f.m.TestWebhook(ctx, "site", "cf")
	require.NoError(t, err)
	assert.True(t, res.OK)
}

// TestDistributionManagerDeleteDropsCache: a removed distribution takes
// its cache settings with it; a failure there does not fail the removal.
func TestDistributionManagerDeleteDropsCache(t *testing.T) {
	ctx := context.Background()
	dists := distmock.NewService(t)
	caches := cachemock.NewManager(t)
	m, err := admin.NewDistributionManager(admin.DistributionManagerDeps{
		Distributions: dists, Signer: distmock.NewSigner(t), Authz: adminmock.NewAuthorizer(t), Cache: caches,
	})
	require.NoError(t, err)
	dists.EXPECT().Delete(mock.Anything, "site").Return(nil).Twice()
	caches.EXPECT().DeleteDistribution(mock.Anything, "site").Return(nil).Once()
	require.NoError(t, m.Delete(ctx, "site"))
	caches.EXPECT().DeleteDistribution(mock.Anything, "site").Return(domain.ErrNodeOffline).Once()
	require.NoError(t, m.Delete(ctx, "site"))
	dists.EXPECT().Delete(mock.Anything, "nope").Return(distribution.ErrDistributionNotFound).Once()
	require.ErrorIs(t, m.Delete(ctx, "nope"), distribution.ErrDistributionNotFound)
}
