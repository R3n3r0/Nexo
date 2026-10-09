package app

import (
	adminsvc "github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/cache"
	adminhttp "github.com/r3n3r0/nexo/internal/transport/http/admin"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// buildCacheManager composes the console service of the cache of the
// distributions (docs/11 §11.11) when the cache and the distributions are
// wired.
func buildCacheManager(d adminDeps, out *adminServices) error {
	if d.cache == nil || d.dist == nil {
		return nil
	}
	var err error
	out.cache, err = adminsvc.NewCacheManager(adminsvc.CacheManagerDeps{
		Manager: d.cache, Operations: d.cache, Distributions: d.dist.service, Logger: d.logger,
	})
	return err
}

// buildCacheRoutes returns the Admin API route group of the cache when
// its service is wired.
func buildCacheRoutes(d adminDeps, svc adminServices) ([]httpx.Routes, error) {
	if svc.cache == nil {
		return nil, nil
	}
	routes, err := adminhttp.NewCacheRoutes(adminhttp.CacheDeps{Cache: svc.cache, Authz: svc.authz, Enc: d.codec})
	if err != nil {
		return nil, err
	}
	return []httpx.Routes{routes}, nil
}

// cacheManager returns the cache of d as the cache.Manager of the
// distribution manager (nil without cache: a nil interface, not a typed
// nil).
func cacheManager(d adminDeps) cache.Manager {
	if d.cache == nil {
		return nil
	}
	return d.cache
}
