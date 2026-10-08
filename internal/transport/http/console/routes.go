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
	FS() (fs.FS, bool)
}

// SPARoutes serves the single-page application.
type SPARoutes interface{ Routes }

type staticAssets struct {
	fsys fs.FS
}

var _ Assets = (*staticAssets)(nil)

// NewAssets wraps a filesystem as Assets; a nil fsys means "not embedded".
func NewAssets(fsys fs.FS) Assets {
	return &staticAssets{fsys: fsys}
}

// FS implements Assets.
func (a *staticAssets) FS() (fs.FS, bool) {
	if a == nil || a.fsys == nil {
		return nil, false
	}
	return a.fsys, true
}
