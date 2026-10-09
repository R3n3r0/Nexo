package admin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/cache"
	"github.com/r3n3r0/nexo/internal/service/distribution"
)

// CacheManagerDeps are the dependencies of the cache manager.
type CacheManagerDeps struct {
	// Manager manages the configuration of the cache (required).
	Manager cache.Manager
	// Operations operates on the cached content (required).
	Operations cache.Operations
	// Distributions resolves the distributions and their domains
	// (required).
	Distributions distribution.Service
	Logger        *slog.Logger
}

// CacheManagerOption configures the cache manager.
type CacheManagerOption func(*cacheManager)

// WithMaxStatsTop bounds the top objects a statistics request may ask for
// (default 100).
func WithMaxStatsTop(n int) CacheManagerOption {
	return func(m *cacheManager) {
		if n > 0 {
			m.maxTop = n
		}
	}
}

// defaultMaxStatsTop is the default bound of StatsQuery.Top.
const defaultMaxStatsTop = 100

type cacheManager struct {
	deps   CacheManagerDeps
	maxTop int
}

var _ CacheManager = (*cacheManager)(nil)

// NewCacheManager returns the cache manager of the Admin API. A missing
// dependency is a wiring error (domain.ErrInvalidArgument).
func NewCacheManager(deps CacheManagerDeps, opts ...CacheManagerOption) (CacheManager, error) {
	if deps.Manager == nil || deps.Operations == nil || deps.Distributions == nil {
		return nil, fmt.Errorf("%w: cache manager: the cache manager, operations and distributions are required", domain.ErrInvalidArgument)
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	m := &cacheManager{deps: deps, maxTop: defaultMaxStatsTop}
	for _, o := range opts {
		o(m)
	}
	return m, nil
}

// --- policies and settings ----------------------------------------------------------

// ListPolicies implements CacheManager.
func (m *cacheManager) ListPolicies(ctx context.Context) ([]cache.Policy, error) {
	return m.deps.Manager.ListPolicies(ctx)
}

// GetPolicy implements CacheManager.
func (m *cacheManager) GetPolicy(ctx context.Context, name string) (cache.Policy, error) {
	return m.deps.Manager.GetPolicy(ctx, name)
}

// CreatePolicy implements CacheManager.
func (m *cacheManager) CreatePolicy(ctx context.Context, p cache.Policy) (cache.Policy, error) {
	return m.deps.Manager.CreatePolicy(ctx, p)
}

// UpdatePolicy implements CacheManager.
func (m *cacheManager) UpdatePolicy(ctx context.Context, p cache.Policy, ifMatch string) (cache.Policy, error) {
	return m.deps.Manager.UpdatePolicy(ctx, p, ifMatch)
}

// DeletePolicy implements CacheManager.
func (m *cacheManager) DeletePolicy(ctx context.Context, name string) error {
	return m.deps.Manager.DeletePolicy(ctx, name)
}

// PreviewPolicy implements CacheManager.
func (m *cacheManager) PreviewPolicy(ctx context.Context, p cache.Policy) (cache.PolicyPreview, error) {
	return m.deps.Manager.PreviewPolicy(ctx, p)
}

// GetSettings implements CacheManager.
func (m *cacheManager) GetSettings(ctx context.Context) (cache.GlobalConfig, error) {
	return m.deps.Manager.GetGlobal(ctx)
}

// UpdateSettings implements CacheManager.
func (m *cacheManager) UpdateSettings(ctx context.Context, g cache.GlobalConfig, ifMatch string) (cache.GlobalConfig, error) {
	return m.deps.Manager.UpdateGlobal(ctx, g, ifMatch)
}

// --- distributions -------------------------------------------------------------------

// GetDistribution implements CacheManager.
func (m *cacheManager) GetDistribution(ctx context.Context, id string) (cache.DistributionConfig, error) {
	return m.deps.Manager.GetDistribution(ctx, id)
}

// UpdateDistribution implements CacheManager.
func (m *cacheManager) UpdateDistribution(ctx context.Context, cfg cache.DistributionConfig, ifMatch string) (cache.DistributionConfig, error) {
	return m.deps.Manager.UpdateDistribution(ctx, cfg, ifMatch)
}

// Effective implements CacheManager: the path defaults to the root of the
// distribution, the host to its level (no domain).
func (m *cacheManager) Effective(ctx context.Context, in cache.EffectiveInput) (cache.EffectivePolicy, error) {
	in.Host = strings.TrimSpace(in.Host)
	if in.Path = strings.TrimSpace(in.Path); !strings.HasPrefix(in.Path, "/") {
		in.Path = "/" + in.Path
	}
	return m.deps.Manager.Effective(ctx, in)
}

// --- invalidations -------------------------------------------------------------------

// Invalidate implements CacheManager.
func (m *cacheManager) Invalidate(ctx context.Context, in InvalidateCacheInput) (cache.Invalidation, error) {
	req := in.Request
	req.Distribution = strings.TrimSpace(req.Distribution)
	req.Domain = strings.TrimSpace(req.Domain)
	req.Actor = actorOf(in.Requester)
	if req.Distribution != "" {
		d, err := m.deps.Distributions.Get(ctx, req.Distribution)
		if err != nil {
			return cache.Invalidation{}, err
		}
		if req.Paths, req.Domain, err = m.urlPaths(ctx, d, req.Paths, req.Domain); err != nil {
			return cache.Invalidation{}, err
		}
	}
	return m.deps.Operations.Invalidate(ctx, req)
}

// urlPaths turns the absolute URLs among paths into paths of d: their
// host, an alias of d, becomes the domain of the invalidation (all the
// URLs must share it, and agree with the domain given).
func (m *cacheManager) urlPaths(ctx context.Context, d distribution.Distribution, paths []string, domainName string) ([]string, string, error) {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if !isAbsoluteURL(p) {
			out = append(out, p)
			continue
		}
		u, err := m.distributionURL(ctx, d, p)
		if err != nil {
			return nil, "", err
		}
		host := strings.ToLower(u.Hostname())
		switch {
		case domainName == "":
			domainName = host
		case !strings.EqualFold(domainName, host) && !hostMatchesAlias(domainName, host):
			return nil, "", fmt.Errorf("%w: %s is not on the domain %s of the invalidation", domain.ErrInvalidArgument, p, domainName)
		}
		path := u.Path
		if path == "" {
			path = "/"
		}
		out = append(out, path)
	}
	return out, domainName, nil
}

// hostMatchesAlias reports whether host matches a wildcard alias
// ("*.example.com" matches "a.example.com").
func hostMatchesAlias(alias, host string) bool {
	suffix, ok := strings.CutPrefix(strings.ToLower(alias), "*")
	return ok && strings.HasSuffix(strings.ToLower(host), suffix) && len(host) > len(suffix)
}

// isAbsoluteURL reports an http(s) URL (as opposed to a path).
func isAbsoluteURL(s string) bool {
	l := strings.ToLower(s)
	return strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://")
}

// distributionURL parses an absolute URL whose host d serves.
func (m *cacheManager) distributionURL(ctx context.Context, d distribution.Distribution, raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("%w: %q is not an http(s) URL nor a path", domain.ErrInvalidArgument, raw)
	}
	route := m.deps.Distributions.Resolve(ctx, u.Host)
	if route.Kind != distribution.RouteDistribution || route.Distribution == nil || route.Distribution.ID != d.ID {
		return nil, fmt.Errorf("%w: the host %s is not a domain of distribution %s", domain.ErrInvalidArgument, u.Hostname(), d.ID)
	}
	return u, nil
}

// GetInvalidation implements CacheManager.
func (m *cacheManager) GetInvalidation(ctx context.Context, dist, id string) (cache.Invalidation, error) {
	if err := m.checkDistribution(ctx, dist); err != nil {
		return cache.Invalidation{}, err
	}
	return m.deps.Operations.GetInvalidation(ctx, dist, id)
}

// FindInvalidation implements CacheManager: the global history first,
// then the history of every distribution.
func (m *cacheManager) FindInvalidation(ctx context.Context, id string) (cache.Invalidation, error) {
	inv, err := m.deps.Operations.GetInvalidation(ctx, "", id)
	if !errors.Is(err, cache.ErrInvalidationNotFound) {
		return inv, err
	}
	dists, err := m.deps.Distributions.List(ctx)
	if err != nil {
		return cache.Invalidation{}, err
	}
	for _, d := range dists {
		inv, err := m.deps.Operations.GetInvalidation(ctx, d.ID, id)
		if !errors.Is(err, cache.ErrInvalidationNotFound) {
			return inv, err
		}
	}
	return cache.Invalidation{}, fmt.Errorf("%w: %s", cache.ErrInvalidationNotFound, id)
}

// ListInvalidations implements CacheManager.
func (m *cacheManager) ListInvalidations(ctx context.Context, dist string) ([]cache.Invalidation, error) {
	if err := m.checkDistribution(ctx, dist); err != nil {
		return nil, err
	}
	list, err := m.deps.Operations.ListInvalidations(ctx, dist)
	if err != nil {
		return nil, err
	}
	if list == nil {
		list = []cache.Invalidation{}
	}
	return list, nil
}

// checkDistribution reports an unknown distribution ("" is the global
// scope).
func (m *cacheManager) checkDistribution(ctx context.Context, id string) error {
	if id == "" {
		return nil
	}
	_, err := m.deps.Distributions.Get(ctx, id)
	return err
}

// --- warm-up, inspector, statistics --------------------------------------------------

// Warm implements CacheManager.
func (m *cacheManager) Warm(ctx context.Context, in WarmCacheInput) (cache.WarmJob, error) {
	req := in.Request
	req.Actor = actorOf(in.Requester)
	if req.Concurrency < 0 || req.Bandwidth < 0 {
		return cache.WarmJob{}, fmt.Errorf("%w: the concurrency and the bandwidth of a warm-up are positive", domain.ErrInvalidArgument)
	}
	return m.deps.Operations.Warm(ctx, req)
}

// GetWarmJob implements CacheManager.
func (m *cacheManager) GetWarmJob(ctx context.Context, dist, id string) (cache.WarmJob, error) {
	if err := m.checkDistribution(ctx, dist); err != nil {
		return cache.WarmJob{}, err
	}
	return m.deps.Operations.GetWarmJob(ctx, dist, id)
}

// Inspect implements CacheManager.
func (m *cacheManager) Inspect(ctx context.Context, in InspectCacheInput) (cache.InspectResult, error) {
	raw := strings.TrimSpace(in.URL)
	if raw == "" {
		return cache.InspectResult{}, fmt.Errorf("%w: the URL to inspect is required", domain.ErrInvalidArgument)
	}
	dist := strings.TrimSpace(in.Distribution)
	if dist != "" {
		d, err := m.deps.Distributions.Get(ctx, dist)
		if err != nil {
			return cache.InspectResult{}, err
		}
		if strings.HasPrefix(raw, "/") {
			host := exactAlias(d)
			if host == "" {
				return cache.InspectResult{}, fmt.Errorf("%w: distribution %s has no exact alias: give the URL", domain.ErrInvalidArgument, d.ID)
			}
			raw = "https://" + host + raw
		}
		if _, err := m.distributionURL(ctx, d, raw); err != nil {
			return cache.InspectResult{}, err
		}
	} else if !isAbsoluteURL(raw) {
		return cache.InspectResult{}, fmt.Errorf("%w: %q is not an http(s) URL (a path needs its distribution)", domain.ErrInvalidArgument, raw)
	}
	return m.deps.Operations.Inspect(ctx, cache.InspectInput{URL: raw, Header: in.Header})
}

// Stats implements CacheManager.
func (m *cacheManager) Stats(ctx context.Context, q cache.StatsQuery) (cache.StatsReport, error) {
	q.Distribution, q.Host = strings.TrimSpace(q.Distribution), strings.ToLower(strings.TrimSpace(q.Host))
	if q.Top < 0 || q.Top > m.maxTop {
		return cache.StatsReport{}, fmt.Errorf("%w: top must be between 1 and %d", domain.ErrInvalidArgument, m.maxTop)
	}
	if q.Host != "" && q.Distribution == "" {
		route := m.deps.Distributions.Resolve(ctx, q.Host)
		if route.Kind != distribution.RouteDistribution || route.Distribution == nil {
			return cache.StatsReport{}, fmt.Errorf("%w: no distribution serves the host %s", domain.ErrInvalidArgument, q.Host)
		}
		q.Distribution = route.Distribution.ID
	}
	if err := m.checkDistribution(ctx, q.Distribution); err != nil {
		return cache.StatsReport{}, err
	}
	return m.deps.Operations.Stats(ctx, q)
}

// --- webhooks ------------------------------------------------------------------------

// ListWebhooks implements CacheManager.
func (m *cacheManager) ListWebhooks(ctx context.Context, dist string) ([]cache.Webhook, error) {
	list, err := m.deps.Manager.ListWebhooks(ctx, dist)
	if err != nil {
		return nil, err
	}
	if list == nil {
		list = []cache.Webhook{}
	}
	return list, nil
}

// AddWebhook implements CacheManager.
func (m *cacheManager) AddWebhook(ctx context.Context, dist string, w cache.Webhook) (cache.Webhook, error) {
	return m.deps.Manager.AddWebhook(ctx, dist, w)
}

// DeleteWebhook implements CacheManager.
func (m *cacheManager) DeleteWebhook(ctx context.Context, dist, id string) error {
	return m.deps.Manager.DeleteWebhook(ctx, dist, id)
}

// TestWebhook implements CacheManager.
func (m *cacheManager) TestWebhook(ctx context.Context, dist, id string) (cache.WebhookTestResult, error) {
	return m.deps.Manager.TestWebhook(ctx, dist, id)
}
