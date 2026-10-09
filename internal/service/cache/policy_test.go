package cache_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/cache"
	"github.com/r3n3r0/nexo/internal/service/distribution"
)

func TestParseDurationAndSize(t *testing.T) {
	durations := []struct {
		in   string
		want time.Duration
		err  bool
	}{
		{in: "1h", want: time.Hour},
		{in: "365d", want: 365 * 24 * time.Hour},
		{in: "1.5d", want: 36 * time.Hour},
		{in: "90s", want: 90 * time.Second},
		{in: "", err: true},
		{in: "soon", err: true},
	}
	for _, tc := range durations {
		t.Run("duration "+tc.in, func(t *testing.T) {
			d, err := cache.ParseDuration(tc.in)
			if tc.err {
				require.ErrorIs(t, err, domain.ErrInvalidArgument)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, d.D())
		})
	}
	sizes := []struct {
		in   string
		want int64
		err  bool
	}{
		{in: "20GiB", want: 20 << 30},
		{in: "512MiB", want: 512 << 20},
		{in: "8MB", want: 8e6},
		{in: "1K", want: 1024},
		{in: "100", want: 100},
		{in: "-1", err: true},
		{in: "big", err: true},
	}
	for _, tc := range sizes {
		t.Run("size "+tc.in, func(t *testing.T) {
			s, err := cache.ParseSize(tc.in)
			if tc.err {
				require.ErrorIs(t, err, domain.ErrInvalidArgument)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, int64(s))
		})
	}
	// The JSON forms: strings with units, numbers.
	var v struct {
		D cache.Duration `json:"d"`
		S cache.Size     `json:"s"`
		Q cache.QueryString
	}
	require.NoError(t, json.Unmarshal([]byte(`{"d":"365d","s":"20GiB","Q":"allowlist:v,w"}`), &v))
	assert.Equal(t, 365*24*time.Hour, v.D.D())
	assert.Equal(t, cache.Size(20<<30), v.S)
	assert.Equal(t, cache.QueryString{Mode: cache.QueryAllowlist, Params: []string{"v", "w"}}, v.Q)
	require.NoError(t, json.Unmarshal([]byte(`{"d":60,"s":1024,"Q":{"denylist":["utm"]}}`), &v))
	assert.Equal(t, time.Minute, v.D.D())
	assert.Equal(t, cache.Size(1024), v.S)
	assert.Equal(t, cache.QueryString{Mode: cache.QueryDenylist, Params: []string{"utm"}}, v.Q)
	out, err := json.Marshal(struct {
		D cache.Duration
		S cache.Size
	}{cache.Duration(365 * 24 * time.Hour), cache.Size(20 << 30)})
	require.NoError(t, err)
	assert.JSONEq(t, `{"D":"365d","S":"20GiB"}`, string(out))
}

// TestEffectivePolicyInheritance is TCC-01: the levels apply from the
// global settings to the behavior, a policy named at a level replaces the
// previous settings, the inline fields override it, and every setting
// reports the level it comes from.
func TestEffectivePolicyInheritance(t *testing.T) {
	custom := map[string]cache.Policy{"short": {Name: "short", PolicySpec: cache.PolicySpec{TTL: &cache.TTL{Default: dur(time.Minute), Max: dur(time.Hour)}}}}
	tests := []struct {
		name    string
		levels  []cache.LevelSettings
		enabled bool
		policy  string
		polLvl  cache.Level
		checks  func(t *testing.T, e cache.EffectivePolicy)
	}{
		{
			name: "defaults", enabled: true, policy: cache.PolicyStandard, polLvl: cache.LevelDefault,
			checks: func(t *testing.T, e cache.EffectivePolicy) {
				assert.Equal(t, time.Hour, e.Policy.TTL.Default.D())
				assert.Equal(t, cache.LevelDefault, e.Fields["ttl.default"])
				assert.True(t, e.AutoInvalidate)
			},
		},
		{
			name: "global policy, distribution override, domain policy, behavior ttl",
			levels: []cache.LevelSettings{
				cache.NewLevel(cache.LevelGlobal, &cache.Settings{Policy: cache.PolicyAggressive}),
				cache.NewLevel(cache.LevelDistribution, &cache.Settings{PolicySpec: cache.PolicySpec{StaleIfError: dur(time.Hour)}}),
				cache.NewLevel(cache.LevelDomain, &cache.Settings{Policy: "short"}),
				cache.NewLevel(cache.LevelBehavior, &cache.Settings{PolicySpec: cache.PolicySpec{TTL: &cache.TTL{Default: dur(30 * time.Second)}}}),
			},
			enabled: true, policy: "short", polLvl: cache.LevelDomain,
			checks: func(t *testing.T, e cache.EffectivePolicy) {
				assert.Equal(t, 30*time.Second, e.Policy.TTL.Default.D())
				assert.Equal(t, cache.LevelBehavior, e.Fields["ttl.default"])
				assert.Equal(t, time.Hour, e.Policy.TTL.Max.D())
				assert.Equal(t, cache.LevelDomain, e.Fields["ttl.max"])
				// The domain named a policy: the override of the
				// distribution does not survive it.
				assert.Equal(t, 24*time.Hour, e.Policy.StaleIfError.D())
				assert.Equal(t, cache.LevelDomain, e.Fields["staleIfError"])
			},
		},
		{
			name: "domain disables",
			levels: []cache.LevelSettings{
				cache.NewLevel(cache.LevelDistribution, &cache.Settings{Enabled: ptr(true), Policy: cache.PolicyStandard}),
				cache.NewLevel(cache.LevelDomain, &cache.Settings{Enabled: ptr(false)}),
			},
			enabled: false, policy: cache.PolicyStandard, polLvl: cache.LevelDistribution,
			checks: func(t *testing.T, e cache.EffectivePolicy) {
				assert.Equal(t, cache.LevelDomain, e.EnabledLevel)
			},
		},
		{
			name: "disabled policy at a behavior",
			levels: []cache.LevelSettings{
				cache.NewLevel(cache.LevelDistribution, &cache.Settings{Policy: cache.PolicyAggressive}),
				cache.NewLevel(cache.LevelBehavior, &cache.Settings{Policy: cache.PolicyDisabled}),
			},
			enabled: false, policy: cache.PolicyDisabled, polLvl: cache.LevelBehavior,
		},
		{
			name: "distribution turns the automatic invalidation off",
			levels: []cache.LevelSettings{
				cache.NewLevel(cache.LevelGlobal, &cache.Settings{AutoInvalidate: ptr(true)}),
				cache.NewLevel(cache.LevelDistribution, &cache.Settings{AutoInvalidate: ptr(false)}),
			},
			enabled: true, policy: cache.PolicyStandard, polLvl: cache.LevelDefault,
			checks: func(t *testing.T, e cache.EffectivePolicy) { assert.False(t, e.AutoInvalidate) },
		},
		{
			name: "immutable assets behavior sets the browser cache control",
			levels: []cache.LevelSettings{
				cache.NewLevel(cache.LevelBehavior, &cache.Settings{Policy: cache.PolicyImmutableAssets}),
			},
			enabled: true, policy: cache.PolicyImmutableAssets, polLvl: cache.LevelBehavior,
			checks: func(t *testing.T, e cache.EffectivePolicy) {
				assert.Equal(t, "public, max-age=31536000, immutable", e.CacheControl)
				assert.False(t, *e.Policy.RespectObjectCacheControl)
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := cache.Resolve(custom, tc.levels...)
			assert.Equal(t, tc.enabled, e.Enabled)
			assert.Equal(t, tc.policy, e.PolicyName)
			assert.Equal(t, tc.polLvl, e.PolicyLevel)
			if tc.checks != nil {
				tc.checks(t, e)
			}
		})
	}
}

// TestEffectivePolicyThroughTheService checks the resolution of the
// stored settings: a domain and a behavior of a distribution override its
// defaults (TCC-01, TCC-02).
func TestEffectivePolicyThroughTheService(t *testing.T) {
	e := newEnv(t, envOptions{})
	ctx := context.Background()
	e.setDist(cache.DistributionConfig{
		Distribution: "web",
		Cache:        cache.Settings{Enabled: ptr(true), Policy: cache.PolicyStandard},
		Domains: []cache.DomainSettings{
			{Host: "www.example.com", Cache: cache.Settings{Policy: cache.PolicyAggressive, Quota: size(20 << 30)}},
			{Host: "*.preview.example.com", Cache: cache.Settings{Enabled: ptr(false)}},
		},
		Behaviors: []cache.BehaviorSettings{{
			PathPattern: distribution.PathPattern{Type: distribution.PatternGlob, Pattern: "/assets/**"},
			Cache: cache.Settings{PolicySpec: cache.PolicySpec{TTL: &cache.TTL{Default: dur(365 * 24 * time.Hour), Min: dur(24 * time.Hour),
				Max: dur(365 * 24 * time.Hour)}, BrowserCacheControl: ptr("public, max-age=31536000, immutable")}},
		}},
	})
	www, err := e.svc.Effective(ctx, cache.EffectiveInput{Distribution: "web", Host: "www.example.com", Path: "/index.html"})
	require.NoError(t, err)
	assert.True(t, www.Enabled)
	assert.Equal(t, cache.PolicyAggressive, www.PolicyName)
	assert.Equal(t, cache.LevelDomain, www.PolicyLevel)
	assert.Equal(t, cache.Size(20<<30), www.DomainQuota)
	asset, err := e.svc.Effective(ctx, cache.EffectiveInput{Distribution: "web", Host: "www.example.com", Path: "/assets/app.js"})
	require.NoError(t, err)
	assert.Equal(t, 365*24*time.Hour, asset.Policy.TTL.Default.D())
	assert.Equal(t, cache.LevelBehavior, asset.Fields["ttl.default"])
	assert.Equal(t, 0, asset.Behavior)
	assert.Equal(t, "public, max-age=31536000, immutable", asset.CacheControl)
	preview, err := e.svc.Effective(ctx, cache.EffectiveInput{Distribution: "web", Host: "pr-1.preview.example.com", Path: "/"})
	require.NoError(t, err)
	assert.False(t, preview.Enabled)
	assert.Equal(t, cache.LevelDomain, preview.EnabledLevel)
	plain, err := e.svc.Effective(ctx, cache.EffectiveInput{Distribution: "web", Host: "example.com", Path: "/"})
	require.NoError(t, err)
	assert.Equal(t, cache.PolicyStandard, plain.PolicyName)
	assert.Equal(t, cache.LevelDistribution, plain.PolicyLevel)
	_, err = e.svc.Effective(ctx, cache.EffectiveInput{Distribution: "nope"})
	require.ErrorIs(t, err, domain.ErrNotFound)
}

func TestPolicyValidationAndPreview(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, envOptions{})
	tests := []struct {
		name string
		p    cache.Policy
		err  bool
	}{
		{name: "valid", p: cache.Policy{Name: "assets", PolicySpec: cache.PolicySpec{TTL: &cache.TTL{Default: dur(time.Hour), Min: dur(time.Minute), Max: dur(24 * time.Hour)}}}},
		{name: "min above default", p: cache.Policy{Name: "bad", PolicySpec: cache.PolicySpec{TTL: &cache.TTL{Default: dur(time.Minute), Min: dur(time.Hour)}}}, err: true},
		{name: "default above max", p: cache.Policy{Name: "bad", PolicySpec: cache.PolicySpec{TTL: &cache.TTL{Default: dur(30 * 24 * time.Hour)}}}, err: true},
		{name: "invalid name", p: cache.Policy{Name: "Bad Name"}, err: true},
		{name: "unknown encoding", p: cache.Policy{Name: "enc", PolicySpec: cache.PolicySpec{Compression: &cache.Compression{Algorithms: []string{"lzma"}}}}, err: true},
		{name: "authorization in the key", p: cache.Policy{Name: "auth", PolicySpec: cache.PolicySpec{CacheKey: &cache.KeyPolicy{Headers: []string{"Authorization"}}}}, err: true},
		{name: "block too small", p: cache.Policy{Name: "blk", PolicySpec: cache.PolicySpec{RangeCaching: &cache.RangeCaching{BlockSize: size(1024)}}}, err: true},
		{name: "query mode", p: cache.Policy{Name: "q", PolicySpec: cache.PolicySpec{CacheKey: &cache.KeyPolicy{QueryString: &cache.QueryString{Mode: "some"}}}}, err: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pv, err := e.svc.PreviewPolicy(ctx, tc.p)
			if tc.err {
				require.ErrorIs(t, err, domain.ErrInvalidArgument)
				return
			}
			require.NoError(t, err)
			assert.True(t, pv.Inherited)
			assert.NotEmpty(t, pv.Summary)
		})
	}
	pv, err := e.svc.PreviewPolicy(ctx, cache.Policy{Name: "x", PolicySpec: cache.PolicySpec{BrowserCacheControl: ptr("public, max-age=60")}})
	require.NoError(t, err)
	assert.Equal(t, "public, max-age=60", pv.CacheControl)
	assert.False(t, pv.Inherited)
}

func TestPolicyCRUD(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, envOptions{})
	list, err := e.svc.ListPolicies(ctx)
	require.NoError(t, err)
	require.Len(t, list, 5, "the built-in policies")
	for _, p := range list {
		assert.True(t, p.Builtin)
	}
	_, err = e.svc.CreatePolicy(ctx, cache.Policy{Name: cache.PolicyStandard})
	require.ErrorIs(t, err, domain.ErrConflict)
	p, err := e.svc.CreatePolicy(ctx, cache.Policy{Name: "assets", Comment: "long", PolicySpec: cache.PolicySpec{TTL: &cache.TTL{Default: dur(2 * time.Hour)}}})
	require.NoError(t, err)
	assert.NotEmpty(t, p.ETag)
	assert.Equal(t, 7*24*time.Hour, p.TTL.Max.D(), "completed with the standard policy")
	_, err = e.svc.CreatePolicy(ctx, cache.Policy{Name: "assets"})
	require.ErrorIs(t, err, domain.ErrConflict)
	_, err = e.svc.UpdatePolicy(ctx, cache.Policy{Name: "assets"}, "stale")
	require.ErrorIs(t, err, domain.ErrPreconditionFailed)
	up, err := e.svc.UpdatePolicy(ctx, cache.Policy{Name: "assets", PolicySpec: cache.PolicySpec{TTL: &cache.TTL{Default: dur(3 * time.Hour)}}}, p.ETag)
	require.NoError(t, err)
	got, err := e.svc.GetPolicy(ctx, "assets")
	require.NoError(t, err)
	assert.Equal(t, up.ETag, got.ETag)
	assert.Equal(t, 3*time.Hour, got.TTL.Default.D())
	// A policy in use cannot be removed.
	e.setDist(cache.DistributionConfig{Distribution: "web", Cache: cache.Settings{Policy: "assets"}})
	require.ErrorIs(t, e.svc.DeletePolicy(ctx, "assets"), cache.ErrPolicyInUse)
	e.setDist(cache.DistributionConfig{Distribution: "web"})
	require.NoError(t, e.svc.DeletePolicy(ctx, "assets"))
	_, err = e.svc.GetPolicy(ctx, "assets")
	require.ErrorIs(t, err, cache.ErrPolicyNotFound)
	require.ErrorIs(t, e.svc.DeletePolicy(ctx, cache.PolicyStandard), cache.ErrBuiltinPolicy)
}

func TestDistributionConfigValidation(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, envOptions{})
	tests := []struct {
		name string
		cfg  cache.DistributionConfig
	}{
		{name: "unknown policy", cfg: cache.DistributionConfig{Distribution: "web", Cache: cache.Settings{Policy: "nope"}}},
		{name: "foreign domain", cfg: cache.DistributionConfig{Distribution: "web", Domains: []cache.DomainSettings{{Host: "evil.example.org"}}}},
		{name: "duplicate domain", cfg: cache.DistributionConfig{Distribution: "web", Domains: []cache.DomainSettings{{Host: "www.example.com"}, {Host: "WWW.example.com"}}}},
		{name: "unknown webhook", cfg: cache.DistributionConfig{Distribution: "web", Domains: []cache.DomainSettings{{Host: "www.example.com", Cache: cache.Settings{PurgeWebhooks: []string{"cf"}}}}}},
		{name: "invalid pattern", cfg: cache.DistributionConfig{Distribution: "web", Behaviors: []cache.BehaviorSettings{{PathPattern: distribution.PathPattern{Type: distribution.PatternRegex, Pattern: "("}}}}},
		{name: "quota on a behavior", cfg: cache.DistributionConfig{Distribution: "web", Behaviors: []cache.BehaviorSettings{{PathPattern: distribution.PathPattern{Type: distribution.PatternPrefix, Pattern: "/a"}, Cache: cache.Settings{Quota: size(1)}}}}},
		{name: "webhook without zone", cfg: cache.DistributionConfig{Distribution: "web", Webhooks: []cache.Webhook{{ID: "cf", Type: cache.WebhookCloudflare, Token: "t"}}}},
		{name: "webhook template", cfg: cache.DistributionConfig{Distribution: "web", Webhooks: []cache.Webhook{{ID: "g", Type: cache.WebhookGeneric, URL: "http://x/{{.Nope}}"}}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := e.svc.UpdateDistribution(ctx, tc.cfg, "")
			require.ErrorIs(t, err, domain.ErrInvalidArgument)
		})
	}
	_, err := e.svc.UpdateDistribution(ctx, cache.DistributionConfig{Distribution: "nope"}, "")
	require.True(t, errors.Is(err, domain.ErrNotFound))
	// The secrets of the webhooks are never returned, and kept when an
	// update omits them.
	cfg := e.setDist(cache.DistributionConfig{Distribution: "web", Webhooks: []cache.Webhook{
		{ID: "cf", Type: cache.WebhookCloudflare, Zone: "z", Token: "secret-token"},
		{ID: "ref", Type: cache.WebhookCloudflare, Zone: "z", Token: "${secret:cf}"},
	}})
	assert.Empty(t, cfg.Webhooks[0].Token)
	assert.Equal(t, "${secret:cf}", cfg.Webhooks[1].Token)
	cfg.Webhooks[0].Domains = []string{"www.example.com"}
	e.setDist(cfg)
	e.docs.mu.Lock()
	raw := string(e.docs.docs["cache/distributions/web.json"])
	e.docs.mu.Unlock()
	assert.Contains(t, raw, "secret-token")
}
