// Package console serves the embedded React dashboard: static assets with
// long-lived caching, index.html with no-cache and a SPA fallback for every
// non-API path, plus baseline security headers.
package console

import (
	"io/fs"

	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// Routes registers a group of routes on a chi router.
type Routes = httpx.Routes

// Assets provides the dashboard files. FS returns false when the binary was
// built without the embedui tag: the console then serves a placeholder page.
type Assets interface {
	// FS is a pure accessor (no I/O, never blocks): the documented
	// exception to the context rule.
	FS() (fs.FS, bool)
}

// SPARoutes serves the single-page application.
type SPARoutes interface{ Routes }

// AssetsDeps are the dependencies of the static assets.
type AssetsDeps struct {
	// FS holds the dashboard files rooted at index.html; nil means "not
	// embedded".
	FS fs.FS
}

type staticAssets struct {
	deps AssetsDeps
}

var _ Assets = (*staticAssets)(nil)

// NewAssets wraps the dashboard filesystem as Assets.
func NewAssets(deps AssetsDeps) Assets {
	return &staticAssets{deps: deps}
}

// FS implements Assets.
func (a *staticAssets) FS() (fs.FS, bool) {
	if a == nil || a.deps.FS == nil {
		return nil, false
	}
	return a.deps.FS, true
}
