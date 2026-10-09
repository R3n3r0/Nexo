package clitest

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/stretchr/testify/mock"

	"github.com/r3n3r0/nexo/internal/cli"
	climock "github.com/r3n3r0/nexo/internal/cli/mock"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// The cache of the distributions of MemAdmin (nexoctl cache, dist cache,
// dist invalidate). The model is simplified: one node ("mem"), the
// entries are the paths warmed (or marked with MarkCached), the effective
// policy applies the policy and the TTL and Cache-Control of each level,
// and the invalidations and warm-ups complete at once.

// memCacheNode is the node of the cache of MemAdmin.
const memCacheNode = "mem"

// memCacheState is the state of the cache.
type memCacheState struct {
	policies map[string]*nexoadmin.CachePolicy
	global   nexoadmin.CacheGlobalSettings
	dists    map[string]*nexoadmin.DistributionCache
	invs     []nexoadmin.CacheInvalidation
	warms    map[string]*nexoadmin.CacheWarmJob
	// cached are the entries, "distribution host path".
	cached map[string]bool
}

// cacheLocked returns the state, initialized on first use (lock held).
func (s *memSession) cacheLocked() *memCacheState {
	c := &s.m.cache
	if c.policies == nil {
		c.policies, c.dists = map[string]*nexoadmin.CachePolicy{}, map[string]*nexoadmin.DistributionCache{}
		c.warms, c.cached = map[string]*nexoadmin.CacheWarmJob{}, map[string]bool{}
	}
	return c
}

// MarkCached records an entry of a distribution for a host and a path, as
// a request served by the cache would.
func (m *MemAdmin) MarkCached(dist, host, path string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	(&memSession{m: m}).cacheLocked().cached[dist+" "+host+" "+path] = true
}

// expectCache registers the cache methods of the store on c.
func (s *memSession) expectCache(c *climock.AdminClient) {
	x := mock.Anything
	c.EXPECT().ListCachePolicies(x).RunAndReturn(s.listCachePolicies).Maybe()
	c.EXPECT().GetCachePolicy(x, x).RunAndReturn(s.getCachePolicy).Maybe()
	c.EXPECT().CreateCachePolicy(x, x).RunAndReturn(func(ctx context.Context, p nexoadmin.CachePolicy) (*nexoadmin.CachePolicy, error) {
		return s.saveCachePolicy(ctx, "CreateCachePolicy", p, true, "")
	}).Maybe()
	c.EXPECT().UpdateCachePolicy(x, x, x).RunAndReturn(func(ctx context.Context, p nexoadmin.CachePolicy, ifMatch string) (*nexoadmin.CachePolicy, error) {
		return s.saveCachePolicy(ctx, "UpdateCachePolicy", p, false, ifMatch)
	}).Maybe()
	c.EXPECT().DeleteCachePolicy(x, x).RunAndReturn(s.deleteCachePolicy).Maybe()
	c.EXPECT().PreviewCachePolicy(x, x).RunAndReturn(s.previewCachePolicy).Maybe()
	c.EXPECT().GetCacheSettings(x).RunAndReturn(s.getCacheSettings).Maybe()
	c.EXPECT().SetCacheSettings(x, x, x).RunAndReturn(s.setCacheSettings).Maybe()
	c.EXPECT().GetDistributionCache(x, x).RunAndReturn(s.getDistributionCache).Maybe()
	c.EXPECT().SetDistributionCache(x, x, x).RunAndReturn(s.setDistributionCache).Maybe()
	c.EXPECT().EffectiveCachePolicy(x, x, x, x).RunAndReturn(s.effectiveCachePolicy).Maybe()
	c.EXPECT().InvalidateCache(x, x, x).RunAndReturn(s.invalidateCache).Maybe()
	c.EXPECT().GetCacheInvalidation(x, x, x).RunAndReturn(s.getCacheInvalidation).Maybe()
	c.EXPECT().ListCacheInvalidations(x, x).RunAndReturn(s.listCacheInvalidations).Maybe()
	c.EXPECT().WarmCache(x, x, x).RunAndReturn(s.warmCache).Maybe()
	c.EXPECT().GetCacheWarmJob(x, x, x).RunAndReturn(s.getCacheWarmJob).Maybe()
	c.EXPECT().InspectCache(x, x).RunAndReturn(s.inspectCache).Maybe()
	c.EXPECT().CacheStats(x, x).RunAndReturn(s.cacheStats).Maybe()
	c.EXPECT().ListCacheWebhooks(x, x).RunAndReturn(s.listCacheWebhooks).Maybe()
	c.EXPECT().AddCacheWebhook(x, x, x).RunAndReturn(s.addCacheWebhook).Maybe()
	c.EXPECT().DeleteCacheWebhook(x, x, x).RunAndReturn(s.deleteCacheWebhook).Maybe()
	c.EXPECT().TestCacheWebhook(x, x, x).RunAndReturn(s.testCacheWebhook).Maybe()
}

// beginCache counts a call and requires an administrator (the lock is
// held on success).
func (s *memSession) beginCache(method, action string) error {
	if err := s.begin(method); err != nil {
		return err
	}
	return s.requireAdminLocked(action)
}

// cacheInvalid is the error of an invalid cache request.
func cacheInvalid(format string, args ...any) error {
	return apiError(http.StatusBadRequest, nexoadmin.CodeInvalidCacheRequest, "invalid cache configuration: "+fmt.Sprintf(format, args...))
}

// --- policies --------------------------------------------------------------------------

// builtinCachePolicies are the built-in policies, complete.
func builtinCachePolicies() map[string]nexoadmin.CachePolicy {
	mk := func(name, comment, def, minTTL, maxTTL, cc string) nexoadmin.CachePolicy {
		respect := name != "immutable-assets"
		return nexoadmin.CachePolicy{Name: name, Comment: comment, Builtin: true, CachePolicySpec: nexoadmin.CachePolicySpec{
			TTL: &nexoadmin.CacheTTL{Default: def, Min: minTTL, Max: maxTTL}, RespectObjectCacheControl: &respect,
			BrowserCacheControl: &cc, StaleWhileRevalidate: "1m0s", StaleIfError: "1d", NegativeCaching: "10s",
		}}
	}
	out := map[string]nexoadmin.CachePolicy{
		"standard":         mk("standard", "Default: 1h fresh (bounded to 7d), the Cache-Control of the objects respected.", "1h0m0s", "0s", "7d", ""),
		"disabled":         mk("disabled", "No caching: every request is served by the origin.", "1h0m0s", "0s", "7d", ""),
		"aggressive":       mk("aggressive", "Long lifetimes (1d, at least 1h, at most 30d).", "1d", "1h0m0s", "30d", ""),
		"immutable-assets": mk("immutable-assets", "Versioned assets: one year in cache and in the browsers.", "365d", "30d", "365d", "public, max-age=31536000, immutable"),
		"respect-origin":   mk("respect-origin", "Only the lifetime the objects state.", "0s", "0s", "365d", ""),
	}
	always := true
	disabled := out["disabled"]
	disabled.Bypass = &nexoadmin.CacheBypass{Always: &always}
	out["disabled"] = disabled
	return out
}

// policyLocked returns a policy, built-in or defined (lock held).
func (s *memSession) policyLocked(name string) (nexoadmin.CachePolicy, bool) {
	if p, ok := builtinCachePolicies()[name]; ok {
		return p, true
	}
	if p, ok := s.cacheLocked().policies[name]; ok {
		return *p, true
	}
	return nexoadmin.CachePolicy{}, false
}

func (s *memSession) listCachePolicies(context.Context) ([]nexoadmin.CachePolicy, error) {
	if err := s.beginCache("ListCachePolicies", "admin:ManageCache"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	all := builtinCachePolicies()
	for name, p := range s.cacheLocked().policies {
		all[name] = *p
	}
	out := make([]nexoadmin.CachePolicy, 0, len(all))
	for _, name := range cli.SortedKeys(all) {
		out = append(out, all[name])
	}
	return out, nil
}

func (s *memSession) getCachePolicy(_ context.Context, name string) (*nexoadmin.CachePolicy, error) {
	if err := s.beginCache("GetCachePolicy", "admin:ManageCache"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	p, ok := s.policyLocked(name)
	if !ok {
		return nil, notFound(nexoadmin.CodeCachePolicyNotFound, "cache policy not found: %s", name)
	}
	return &p, nil
}

// memDuration parses a duration of the cache ("90s", "1h0m0s", "365d")
// into seconds.
func memDuration(v string) (float64, bool) {
	if days, ok := strings.CutSuffix(v, "d"); ok {
		var n float64
		_, err := fmt.Sscanf(days, "%g", &n)
		return n * 86400, err == nil
	}
	var total float64
	rest := v
	for rest != "" {
		var n float64
		var unit string
		i := strings.IndexFunc(rest, func(r rune) bool { return (r < '0' || r > '9') && r != '.' })
		if i <= 0 {
			return 0, false
		}
		if _, err := fmt.Sscanf(rest[:i], "%g", &n); err != nil {
			return 0, false
		}
		j := strings.IndexFunc(rest[i:], func(r rune) bool { return r >= '0' && r <= '9' })
		if j < 0 {
			unit, rest = rest[i:], ""
		} else {
			unit, rest = rest[i:i+j], rest[i+j:]
		}
		mult := map[string]float64{"s": 1, "m": 60, "h": 3600, "ms": 0.001}[unit]
		if mult == 0 {
			return 0, false
		}
		total += n * mult
	}
	return total, true
}

// validateSpec checks the durations of a spec.
func validateSpec(spec nexoadmin.CachePolicySpec) error {
	if t := spec.TTL; t != nil {
		vals := map[string]float64{}
		for name, v := range map[string]string{"default": t.Default, "min": t.Min, "max": t.Max} {
			if v == "" {
				continue
			}
			d, ok := memDuration(v)
			if !ok {
				return cacheInvalid("ttl.%s: duration %q", name, v)
			}
			vals[name] = d
		}
		if def, ok := vals["default"]; ok {
			if maxTTL, ok := vals["max"]; ok && def > maxTTL {
				return cacheInvalid("ttl.default %s is above ttl.max %s", t.Default, t.Max)
			}
		}
	}
	return nil
}

func (s *memSession) saveCachePolicy(_ context.Context, method string, p nexoadmin.CachePolicy, create bool, ifMatch string) (*nexoadmin.CachePolicy, error) {
	if err := s.beginCache(method, "admin:ManageCache"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	st := s.cacheLocked()
	cur, exists := st.policies[p.Name]
	_, builtin := builtinCachePolicies()[p.Name]
	switch {
	case p.Name == "":
		return nil, cacheInvalid("the name of the policy is required")
	case builtin:
		return nil, apiError(http.StatusConflict, nexoadmin.CodeBuiltinCachePolicy, "built-in cache policy: "+p.Name)
	case create && exists:
		return nil, apiError(http.StatusConflict, nexoadmin.CodeAlreadyExists, "already exists: policy "+p.Name)
	case !create && !exists:
		return nil, notFound(nexoadmin.CodeCachePolicyNotFound, "cache policy not found: %s", p.Name)
	case !create && ifMatch != "" && ifMatch != cur.ETag:
		return nil, apiError(http.StatusPreconditionFailed, nexoadmin.CodePreconditionFailed, fmt.Sprintf("policy %s changed (ETag %s)", p.Name, cur.ETag))
	}
	if err := validateSpec(p.CachePolicySpec); err != nil {
		return nil, err
	}
	now := s.now()
	p.Builtin, p.UpdatedAt, p.ETag = false, &now, s.nextETag()
	stored := p
	st.policies[p.Name] = &stored
	return &p, nil
}

// policyUsedLocked returns the level naming a policy, "" when none does
// (lock held).
func (s *memSession) policyUsedLocked(name string) string {
	st := s.cacheLocked()
	if st.global.Cache.Policy == name {
		return "the cluster"
	}
	for _, id := range cli.SortedKeys(st.dists) {
		c := st.dists[id]
		used := c.Cache.Policy == name
		for _, d := range c.Domains {
			used = used || d.Cache.Policy == name
		}
		for _, b := range c.Behaviors {
			used = used || b.Cache.Policy == name
		}
		if used {
			return "distribution " + id
		}
	}
	return ""
}

func (s *memSession) deleteCachePolicy(_ context.Context, name string) error {
	if err := s.beginCache("DeleteCachePolicy", "admin:ManageCache"); err != nil {
		return err
	}
	defer s.m.mu.Unlock()
	st := s.cacheLocked()
	if _, builtin := builtinCachePolicies()[name]; builtin {
		return apiError(http.StatusConflict, nexoadmin.CodeBuiltinCachePolicy, "built-in cache policy: "+name)
	}
	if _, ok := st.policies[name]; !ok {
		return notFound(nexoadmin.CodeCachePolicyNotFound, "cache policy not found: %s", name)
	}
	if by := s.policyUsedLocked(name); by != "" {
		return apiError(http.StatusConflict, nexoadmin.CodeCachePolicyInUse, fmt.Sprintf("cache policy in use: %s is used by %s", name, by))
	}
	delete(st.policies, name)
	return nil
}

func (s *memSession) previewCachePolicy(_ context.Context, p nexoadmin.CachePolicy) (*nexoadmin.CachePolicyPreview, error) {
	if err := s.beginCache("PreviewCachePolicy", "admin:ManageCache"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	if err := validateSpec(p.CachePolicySpec); err != nil {
		return nil, err
	}
	out := &nexoadmin.CachePolicyPreview{Policy: p, Inherited: true, Summary: "preview of " + p.Name}
	if p.BrowserCacheControl != nil && *p.BrowserCacheControl != "" {
		out.CacheControl, out.Inherited = *p.BrowserCacheControl, false
	}
	return out, nil
}

// --- settings --------------------------------------------------------------------------

func (s *memSession) getCacheSettings(context.Context) (*nexoadmin.CacheGlobalSettings, error) {
	if err := s.beginCache("GetCacheSettings", "admin:ManageCache"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	g := s.cacheLocked().global
	return &g, nil
}

func (s *memSession) setCacheSettings(_ context.Context, g nexoadmin.CacheGlobalSettings, ifMatch string) (*nexoadmin.CacheGlobalSettings, error) {
	if err := s.beginCache("SetCacheSettings", "admin:ManageCache"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	st := s.cacheLocked()
	switch {
	case ifMatch != "" && ifMatch != st.global.ETag:
		return nil, apiError(http.StatusPreconditionFailed, nexoadmin.CodePreconditionFailed, "cache global settings changed")
	case g.Cache.Quota != "" || len(g.Cache.PurgeWebhooks) > 0:
		return nil, cacheInvalid("the global settings have no quota and no webhooks")
	}
	if g.Cache.Policy != "" {
		if _, ok := s.policyLocked(g.Cache.Policy); !ok {
			return nil, cacheInvalid("cache.policy: unknown policy %q", g.Cache.Policy)
		}
	}
	if err := validateSpec(g.Cache.CachePolicySpec); err != nil {
		return nil, err
	}
	now := s.now()
	g.UpdatedAt, g.ETag = &now, s.nextETag()
	st.global = g
	return &g, nil
}

// --- distributions -------------------------------------------------------------------

// distCacheLocked returns the cache settings of an existing distribution
// (lock held).
func (s *memSession) distCacheLocked(id string) (*nexoadmin.Distribution, nexoadmin.DistributionCache, error) {
	d, ok := s.distLocked().dists[id]
	if !ok {
		return nil, nexoadmin.DistributionCache{}, distNotFound(id)
	}
	if c, ok := s.cacheLocked().dists[id]; ok {
		out := *c
		out.Domains, out.Behaviors, out.Webhooks = slices.Clone(c.Domains), slices.Clone(c.Behaviors), slices.Clone(c.Webhooks)
		return d, out, nil
	}
	return d, nexoadmin.DistributionCache{Distribution: id}, nil
}

// domainOfLocked reports whether host is a domain of d.
func domainOf(d *nexoadmin.Distribution, host string) bool {
	for _, a := range d.Aliases {
		if a == host || aliasMatches(a, host) {
			return true
		}
	}
	return false
}

func (s *memSession) getDistributionCache(_ context.Context, id string) (*nexoadmin.DistributionCache, error) {
	if err := s.beginCache("GetDistributionCache", "admin:ManageCache"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	_, c, err := s.distCacheLocked(id)
	if err != nil {
		return nil, err
	}
	for i := range c.Webhooks {
		c.Webhooks[i] = maskWebhook(c.Webhooks[i])
	}
	return &c, nil
}

// validateDistCacheLocked checks the cache settings of d (lock held).
func (s *memSession) validateDistCacheLocked(d *nexoadmin.Distribution, c nexoadmin.DistributionCache) error {
	levels := []nexoadmin.CacheSettings{c.Cache}
	for i, dom := range c.Domains {
		if !domainOf(d, strings.ToLower(dom.Host)) {
			return cacheInvalid("domains[%d]: %q is not a domain of %s", i, dom.Host, d.ID)
		}
		levels = append(levels, dom.Cache)
	}
	for i, b := range c.Behaviors {
		if _, err := memPattern(b.PathPattern); err != nil {
			return cacheInvalid("behaviors[%d].pathPattern: %v", i, err)
		}
		levels = append(levels, b.Cache)
	}
	for _, l := range levels {
		if l.Policy != "" {
			if _, ok := s.policyLocked(l.Policy); !ok {
				return cacheInvalid("unknown policy %q", l.Policy)
			}
		}
		if err := validateSpec(l.CachePolicySpec); err != nil {
			return err
		}
	}
	return nil
}

func (s *memSession) setDistributionCache(_ context.Context, c nexoadmin.DistributionCache, ifMatch string) (*nexoadmin.DistributionCache, error) {
	if err := s.beginCache("SetDistributionCache", "admin:ManageCache"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	d, cur, err := s.distCacheLocked(c.Distribution)
	if err != nil {
		return nil, err
	}
	if ifMatch != "" && ifMatch != cur.ETag {
		return nil, apiError(http.StatusPreconditionFailed, nexoadmin.CodePreconditionFailed, "cache settings of "+c.Distribution+" changed")
	}
	if err := s.validateDistCacheLocked(d, c); err != nil {
		return nil, err
	}
	for i, w := range c.Webhooks {
		for _, old := range cur.Webhooks {
			if old.ID == w.ID && w.Token == memSensitiveMask {
				c.Webhooks[i].Token = old.Token
			}
			if old.ID == w.ID && w.ClientSecret == memSensitiveMask {
				c.Webhooks[i].ClientSecret = old.ClientSecret
			}
		}
	}
	now := s.now()
	c.UpdatedAt, c.ETag = &now, s.nextETag()
	stored := c
	s.cacheLocked().dists[c.Distribution] = &stored
	return &c, nil
}

// effectiveLocked computes the effective policy of a path of a host
// (lock held).
func (s *memSession) cacheEffectiveLocked(c nexoadmin.DistributionCache, host, path string) nexoadmin.CacheEffectivePolicy {
	std := builtinCachePolicies()["standard"]
	ttl := *std.TTL
	eff := nexoadmin.CacheEffectivePolicy{Enabled: true, EnabledLevel: "default", PolicyName: "standard", PolicyLevel: "default",
		Behavior: -1, AutoInvalidate: true, Fields: map[string]string{"ttl.default": "default"}}
	cc := ""
	levels := []struct {
		level string
		s     nexoadmin.CacheSettings
	}{{"global", s.cacheLocked().global.Cache}, {"distribution", c.Cache}}
	for _, d := range c.Domains {
		if strings.EqualFold(d.Host, host) || aliasMatches(d.Host, host) {
			levels = append(levels, struct {
				level string
				s     nexoadmin.CacheSettings
			}{"domain", d.Cache})
			eff.Domain = d.Host
			break
		}
	}
	for i, b := range c.Behaviors {
		re, err := memPattern(b.PathPattern)
		if err == nil && re.MatchString(path) {
			levels = append(levels, struct {
				level string
				s     nexoadmin.CacheSettings
			}{"behavior", b.Cache})
			eff.Behavior = i
			break
		}
	}
	for _, l := range levels {
		if l.s.Enabled != nil {
			eff.Enabled, eff.EnabledLevel = *l.s.Enabled, l.level
		}
		if l.s.Policy != "" {
			if p, ok := s.policyLocked(l.s.Policy); ok {
				// A defined policy is completed with the standard one.
				ttl, cc = *std.TTL, ""
				if p.TTL != nil {
					ttl = mergeTTL(ttl, *p.TTL)
				}
				if p.BrowserCacheControl != nil {
					cc = *p.BrowserCacheControl
				}
				eff.PolicyName, eff.PolicyLevel, eff.Fields["ttl.default"] = p.Name, l.level, l.level
			}
		}
		if t := l.s.TTL; t != nil {
			if t.Default != "" {
				ttl.Default, eff.Fields["ttl.default"] = t.Default, l.level
			}
			if t.Min != "" {
				ttl.Min = t.Min
			}
			if t.Max != "" {
				ttl.Max = t.Max
			}
		}
		if l.s.BrowserCacheControl != nil {
			cc = *l.s.BrowserCacheControl
		}
		if l.level == "domain" && l.s.Quota != "" {
			eff.DomainQuota = l.s.Quota
		}
	}
	eff.Policy = nexoadmin.CachePolicySpec{TTL: &ttl, BrowserCacheControl: &cc}
	eff.CacheControl = cc
	return eff
}

func (s *memSession) effectiveCachePolicy(_ context.Context, id, host, path string) (*nexoadmin.CacheEffectivePolicy, error) {
	if err := s.beginCache("EffectiveCachePolicy", "admin:ManageCache"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	_, c, err := s.distCacheLocked(id)
	if err != nil {
		return nil, err
	}
	if path == "" {
		path = "/"
	}
	eff := s.cacheEffectiveLocked(c, strings.ToLower(host), path)
	return &eff, nil
}

// mergeTTL overlays the TTL fields set in over on base.
func mergeTTL(base, over nexoadmin.CacheTTL) nexoadmin.CacheTTL {
	if over.Default != "" {
		base.Default = over.Default
	}
	if over.Min != "" {
		base.Min = over.Min
	}
	if over.Max != "" {
		base.Max = over.Max
	}
	return base
}

// --- invalidations -------------------------------------------------------------------

func (s *memSession) invalidateCache(_ context.Context, dist string, in nexoadmin.CacheInvalidationRequest) (*nexoadmin.CacheInvalidation, error) {
	if err := s.beginCache("InvalidateCache", "admin:InvalidateCache"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	st := s.cacheLocked()
	if dist == "" {
		if !in.All || in.Domain != "" || len(in.Paths)+len(in.Patterns)+len(in.Tags) > 0 {
			return nil, cacheInvalid("an invalidation names its distribution (only the global purge, all, does not)")
		}
	} else {
		d, ok := s.distLocked().dists[dist]
		if !ok {
			return nil, distNotFound(dist)
		}
		paths := make([]string, 0, len(in.Paths))
		for _, p := range in.Paths {
			if u, err := url.Parse(p); err == nil && u.Host != "" {
				if in.Domain == "" {
					in.Domain = u.Hostname()
				}
				if p = u.Path; p == "" {
					p = "/"
				}
			}
			paths = append(paths, p)
		}
		in.Paths = paths
		if in.Domain != "" && !domainOf(d, in.Domain) {
			return nil, cacheInvalid("%s is not a domain of %s", in.Domain, dist)
		}
		if !in.All && in.Domain == "" && len(in.Paths)+len(in.Patterns)+len(in.Tags) == 0 {
			return nil, cacheInvalid("nothing to invalidate (paths, patterns, tags, domain or all)")
		}
	}
	removed := 0
	for _, key := range cli.SortedKeys(st.cached) {
		parts := strings.SplitN(key, " ", 3)
		if (dist != "" && parts[0] != dist) || (in.Domain != "" && !aliasMatches(in.Domain, parts[1]) && in.Domain != parts[1]) {
			continue
		}
		hit := in.All || (in.Domain != "" && len(in.Paths)+len(in.Patterns)+len(in.Tags) == 0) || slices.Contains(in.Paths, parts[2])
		for _, p := range in.Patterns {
			if re, err := memPattern(p); err == nil && re.MatchString(parts[2]) {
				hit = true
			}
		}
		if hit {
			if !in.Soft {
				delete(st.cached, key)
			}
			removed++
		}
	}
	s.m.seq++
	now := s.now()
	inv := nexoadmin.CacheInvalidation{
		ID: fmt.Sprintf("inv-%d", s.m.seq), CreatedAt: now, State: nexoadmin.InvalidationCompleted,
		Request: nexoadmin.CacheInvalidationRecord{Distribution: dist, CacheInvalidationRequest: in, Actor: s.caller},
		Nodes:   []nexoadmin.CacheNodeStatus{{Node: memCacheNode, State: nexoadmin.NodeCompleted, At: now, Removed: removed}},
	}
	st.invs = append(st.invs, inv)
	return &inv, nil
}

func (s *memSession) getCacheInvalidation(_ context.Context, dist, id string) (*nexoadmin.CacheInvalidation, error) {
	if err := s.beginCache("GetCacheInvalidation", "admin:InvalidateCache"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	for _, inv := range s.cacheLocked().invs {
		if inv.ID == id && (dist == "" || inv.Request.Distribution == dist) {
			return &inv, nil
		}
	}
	return nil, notFound(nexoadmin.CodeInvalidationNotFound, "invalidation not found: %s", id)
}

func (s *memSession) listCacheInvalidations(_ context.Context, dist string) ([]nexoadmin.CacheInvalidation, error) {
	if err := s.beginCache("ListCacheInvalidations", "admin:InvalidateCache"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	if _, ok := s.distLocked().dists[dist]; dist != "" && !ok {
		return nil, distNotFound(dist)
	}
	out := []nexoadmin.CacheInvalidation{}
	invs := s.cacheLocked().invs
	for i := len(invs) - 1; i >= 0; i-- {
		if invs[i].Request.Distribution == dist {
			out = append(out, invs[i])
		}
	}
	return out, nil
}

// --- warm-up -------------------------------------------------------------------------

func (s *memSession) warmCache(_ context.Context, dist string, in nexoadmin.CacheWarmRequest) (*nexoadmin.CacheWarmJob, error) {
	if err := s.beginCache("WarmCache", "admin:InvalidateCache"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	d, ok := s.distLocked().dists[dist]
	if !ok {
		return nil, distNotFound(dist)
	}
	if in.Domain == "" {
		for _, a := range d.Aliases {
			if !strings.HasPrefix(a, "*.") {
				in.Domain = a
				break
			}
		}
	}
	if !domainOf(d, in.Domain) {
		return nil, cacheInvalid("warm-up domain %q is not a host of %s", in.Domain, dist)
	}
	paths := slices.Clone(in.Paths)
	for _, raw := range in.URLs {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return nil, cacheInvalid("%q is not an http(s) URL", raw)
		}
		if u.Hostname() != in.Domain {
			return nil, cacheInvalid("%s is not on the domain %s", raw, in.Domain)
		}
		paths = append(paths, u.Path)
	}
	if in.Prefix != "" {
		paths = append(paths, in.Prefix+"*")
	}
	if in.Pattern != nil {
		paths = append(paths, in.Pattern.Pattern)
	}
	if len(paths) == 0 {
		return nil, cacheInvalid("nothing to warm (paths, urls, prefix or pattern)")
	}
	st := s.cacheLocked()
	for _, p := range paths {
		st.cached[dist+" "+in.Domain+" "+p] = true
	}
	s.m.seq++
	now := s.now()
	job := &nexoadmin.CacheWarmJob{
		ID: fmt.Sprintf("warm-%d", s.m.seq), CreatedAt: now, State: nexoadmin.WarmCompleted,
		Request: nexoadmin.CacheWarmRecord{Distribution: dist, CacheWarmRequest: in, Actor: s.caller},
		Nodes: []nexoadmin.CacheWarmNode{{CacheNodeStatus: nexoadmin.CacheNodeStatus{Node: memCacheNode, State: nexoadmin.WarmCompleted, At: now},
			Total: len(paths), Done: len(paths), Bytes: int64(100 * len(paths))}},
	}
	st.warms[job.ID] = job
	out := *job
	return &out, nil
}

func (s *memSession) getCacheWarmJob(_ context.Context, dist, id string) (*nexoadmin.CacheWarmJob, error) {
	if err := s.beginCache("GetCacheWarmJob", "admin:InvalidateCache"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	job, ok := s.cacheLocked().warms[id]
	if !ok || job.Request.Distribution != dist {
		return nil, notFound(nexoadmin.CodeWarmJobNotFound, "warm-up not found: %s", id)
	}
	out := *job
	return &out, nil
}

// --- inspector and statistics ----------------------------------------------------------

func (s *memSession) inspectCache(_ context.Context, in nexoadmin.CacheInspectRequest) (*nexoadmin.CacheInspection, error) {
	if err := s.beginCache("InspectCache", "admin:ManageCache"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	raw := in.URL
	if strings.HasPrefix(raw, "/") {
		d, ok := s.distLocked().dists[in.Distribution]
		if !ok {
			if in.Distribution == "" {
				return nil, invalid("%q is not an http(s) URL (a path needs its distribution)", raw)
			}
			return nil, distNotFound(in.Distribution)
		}
		raw = "https://" + d.Aliases[0] + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, invalid("%q is not an http(s) URL", raw)
	}
	if u.Path == "" {
		u.Path = "/"
	}
	route, d := s.routeLocked(u.Host)
	out := &nexoadmin.CacheInspection{URL: raw, Host: route.Host, Path: u.Path, Mode: "local", Nodes: []nexoadmin.CacheNodePresence{}}
	if d == nil {
		out.Bypass, out.Reason = true, "no distribution serves this host"
		return out, nil
	}
	_, c, err := s.distCacheLocked(d.ID)
	if err != nil {
		return nil, err
	}
	out.Distribution, out.Bucket, out.Key = d.ID, d.Origin.Bucket, strings.TrimPrefix(u.Path, "/")
	out.CacheKey = d.ID + "|" + route.Host + "|" + u.Path
	out.Policy = s.cacheEffectiveLocked(c, route.Host, u.Path)
	if !out.Policy.Enabled {
		out.Bypass, out.Reason = true, "the cache is disabled"
	}
	entry := nexoadmin.CachePresence{Key: out.CacheKey, Encodings: []string{}}
	if s.cacheLocked().cached[d.ID+" "+route.Host+" "+u.Path] {
		entry.Present, entry.Tier, entry.Kind, entry.AgeSeconds, entry.ExpiresInSeconds, entry.SizeBytes = true, "memory", "object", 5, 595, 100
	}
	out.Nodes = append(out.Nodes, nexoadmin.CacheNodePresence{Node: memCacheNode, Online: true, Entry: entry})
	return out, nil
}

func (s *memSession) cacheStats(_ context.Context, q nexoadmin.CacheStatsQuery) (*nexoadmin.CacheStats, error) {
	if err := s.beginCache("CacheStats", "admin:ManageCache"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	if _, ok := s.distLocked().dists[q.Distribution]; q.Distribution != "" && !ok {
		return nil, distNotFound(q.Distribution)
	}
	byHost := map[string]*nexoadmin.CacheHostStats{}
	entries := 0
	for key := range s.cacheLocked().cached {
		parts := strings.SplitN(key, " ", 3)
		if (q.Distribution != "" && parts[0] != q.Distribution) || (q.Host != "" && parts[1] != q.Host) {
			continue
		}
		entries++
		h := byHost[parts[1]]
		if h == nil {
			h = &nexoadmin.CacheHostStats{Distribution: parts[0], Host: parts[1], CacheCounters: nexoadmin.CacheCounters{Requests: map[string]int64{}}}
			byHost[parts[1]] = h
		}
		h.Entries++
		h.MemoryBytes += 100
	}
	out := &nexoadmin.CacheStats{
		CacheCounters: nexoadmin.CacheCounters{Requests: map[string]int64{}}, Entries: entries, MemoryBytes: int64(100 * entries),
		Hosts: []nexoadmin.CacheHostStats{}, TopObjects: []nexoadmin.CacheObjectStat{}, TopMisses: []nexoadmin.CacheObjectStat{},
	}
	for _, host := range cli.SortedKeys(byHost) {
		out.Hosts = append(out.Hosts, *byHost[host])
	}
	out.Nodes = []nexoadmin.CacheNodeStats{{
		Node: memCacheNode, Online: true, Mode: "local", Synced: true, CacheCounters: nexoadmin.CacheCounters{Requests: map[string]int64{}},
		Entries: entries, MemoryBytes: int64(100 * entries), MemoryLimitBytes: 64 << 20,
		Hosts: out.Hosts, TopObjects: []nexoadmin.CacheObjectStat{}, TopMisses: []nexoadmin.CacheObjectStat{},
	}}
	return out, nil
}

// --- webhooks --------------------------------------------------------------------------

func (s *memSession) listCacheWebhooks(_ context.Context, dist string) ([]nexoadmin.CacheWebhook, error) {
	if err := s.beginCache("ListCacheWebhooks", "admin:ManageCache"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	_, c, err := s.distCacheLocked(dist)
	if err != nil {
		return nil, err
	}
	out := []nexoadmin.CacheWebhook{}
	for _, w := range c.Webhooks {
		out = append(out, maskWebhook(w))
	}
	return out, nil
}

// maskWebhook hides the secrets of a webhook.
func maskWebhook(w nexoadmin.CacheWebhook) nexoadmin.CacheWebhook {
	if w.Token != "" && !strings.HasPrefix(w.Token, "${secret:") {
		w.Token = memSensitiveMask
	}
	if w.ClientSecret != "" && !strings.HasPrefix(w.ClientSecret, "${secret:") {
		w.ClientSecret = memSensitiveMask
	}
	return w
}

func (s *memSession) addCacheWebhook(_ context.Context, dist string, w nexoadmin.CacheWebhook) (*nexoadmin.CacheWebhook, error) {
	if err := s.beginCache("AddCacheWebhook", "admin:ManageCache"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	d, c, err := s.distCacheLocked(dist)
	if err != nil {
		return nil, err
	}
	switch w.Type {
	case nexoadmin.WebhookCloudflare, nexoadmin.WebhookFastly:
		if w.Zone == "" || w.Token == "" {
			return nil, cacheInvalid("a %s webhook needs zone and token", w.Type)
		}
	case nexoadmin.WebhookNginx, nexoadmin.WebhookVarnish, nexoadmin.WebhookGeneric, nexoadmin.WebhookAkamai:
		if w.URL == "" {
			return nil, cacheInvalid("a %s webhook needs url", w.Type)
		}
	default:
		return nil, cacheInvalid("webhook type %q", w.Type)
	}
	for _, h := range w.Domains {
		if !domainOf(d, h) {
			return nil, cacheInvalid("%q is not a domain of %s", h, dist)
		}
	}
	for _, cur := range c.Webhooks {
		if cur.ID == w.ID {
			return nil, apiError(http.StatusConflict, nexoadmin.CodeAlreadyExists, "already exists: webhook "+w.ID)
		}
	}
	c.Webhooks = append(c.Webhooks, w)
	now := s.now()
	c.UpdatedAt, c.ETag = &now, s.nextETag()
	s.cacheLocked().dists[dist] = &c
	out := maskWebhook(w)
	return &out, nil
}

func (s *memSession) deleteCacheWebhook(_ context.Context, dist, id string) error {
	if err := s.beginCache("DeleteCacheWebhook", "admin:ManageCache"); err != nil {
		return err
	}
	defer s.m.mu.Unlock()
	_, c, err := s.distCacheLocked(dist)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(c.Webhooks, func(w nexoadmin.CacheWebhook) bool { return w.ID == id })
	if i < 0 {
		return notFound(nexoadmin.CodeWebhookNotFound, "purge webhook not found: %s", id)
	}
	c.Webhooks = slices.Delete(c.Webhooks, i, i+1)
	c.ETag = s.nextETag()
	s.cacheLocked().dists[dist] = &c
	return nil
}

func (s *memSession) testCacheWebhook(_ context.Context, dist, id string) (*nexoadmin.CacheWebhookTestResult, error) {
	if err := s.beginCache("TestCacheWebhook", "admin:ManageCache"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	_, c, err := s.distCacheLocked(dist)
	if err != nil {
		return nil, err
	}
	for _, w := range c.Webhooks {
		if w.ID != id {
			continue
		}
		if strings.Contains(w.URL, "unreachable") {
			return &nexoadmin.CacheWebhookTestResult{Error: "connection refused", DurationMs: 3}, nil
		}
		return &nexoadmin.CacheWebhookTestResult{OK: true, Status: http.StatusOK, DurationMs: 12}, nil
	}
	return nil, notFound(nexoadmin.CodeWebhookNotFound, "purge webhook not found: %s", id)
}
