package console_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/transport/http/console"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

func testFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":                  {Data: []byte("<html>app</html>")},
		"favicon.ico":                 {Data: []byte("icon")},
		"assets/index-Bx9f3kQ2.js":    {Data: []byte("console.log(1)")},
		"assets/index-Cz81xkqa.css":   {Data: []byte("body{}")},
		"static/logo.svg":             {Data: []byte("<svg/>")},
		"static/vendor.3f2a9b8c7d.js": {Data: []byte("vendor")},
	}
}

func newSPA(t *testing.T, fsys fstest.MapFS) http.Handler {
	t.Helper()
	r := chi.NewRouter()
	// An API route registered before the SPA keeps precedence.
	r.Get("/api/v1/info", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	var assets console.Assets
	if fsys != nil {
		assets = console.NewAssets(console.AssetsDeps{FS: fsys})
	}
	console.NewSPARoutes(console.SPADeps{Assets: assets}).Register(r)
	return r
}

func get(h http.Handler, method, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

func TestSPAFallbackAndIndex(t *testing.T) {
	h := newSPA(t, testFS())
	for _, p := range []string{"/", "/index.html", "/buckets", "/buckets/photos/objects", "/deep/nested/route"} {
		rec := get(h, http.MethodGet, p)
		assert.Equal(t, http.StatusOK, rec.Code, p)
		assert.Equal(t, "<html>app</html>", rec.Body.String(), p)
		assert.Equal(t, "no-cache, no-store, must-revalidate", rec.Header().Get("Cache-Control"), p)
		assert.Contains(t, rec.Header().Get("Content-Type"), "text/html", p)
		assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"), p)
		assert.Equal(t, "DENY", rec.Header().Get("X-Frame-Options"), p)
		assert.Equal(t, "strict-origin-when-cross-origin", rec.Header().Get("Referrer-Policy"), p)
		assert.Equal(t, console.DefaultCSP, rec.Header().Get("Content-Security-Policy"), p)
	}
	rec := get(h, http.MethodHead, "/buckets")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Body.String())
	assert.Equal(t, "16", rec.Header().Get("Content-Length"))
}

func TestSPAAssets(t *testing.T) {
	h := newSPA(t, testFS())

	rec := get(h, http.MethodGet, "/assets/index-Bx9f3kQ2.js")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "console.log(1)", rec.Body.String())
	assert.Equal(t, "public, max-age=31536000, immutable", rec.Header().Get("Cache-Control"))
	assert.Contains(t, rec.Header().Get("Content-Type"), "javascript")

	rec = get(h, http.MethodGet, "/static/vendor.3f2a9b8c7d.js")
	assert.Equal(t, "public, max-age=31536000, immutable", rec.Header().Get("Cache-Control"), "dot-separated hash")

	rec = get(h, http.MethodGet, "/static/logo.svg")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "public, max-age=3600", rec.Header().Get("Cache-Control"), "unhashed asset")
	assert.Contains(t, rec.Header().Get("Content-Type"), "image/svg+xml")

	rec = get(h, http.MethodGet, "/favicon.ico")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "icon", rec.Body.String())

	// Missing asset with an extension is a 404, not index.html.
	rec = get(h, http.MethodGet, "/assets/missing-abcdefgh.js")
	assert.Equal(t, http.StatusNotFound, rec.Code)

	// Path traversal is neutralized.
	rec = get(h, http.MethodGet, "/../index.html")
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestSPADoesNotShadowAPI(t *testing.T) {
	h := newSPA(t, testFS())
	assert.Equal(t, http.StatusTeapot, get(h, http.MethodGet, "/api/v1/info").Code)

	rec := get(h, http.MethodGet, "/api/v1/unknown")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, httpx.ContentTypeProblem, rec.Header().Get("Content-Type"))
	rec = get(h, http.MethodGet, "/nexo/health/nope")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	rec = get(h, http.MethodGet, "/api")
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestSPAWithoutEmbeddedAssets(t *testing.T) {
	h := newSPA(t, nil)
	rec := get(h, http.MethodGet, "/buckets")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "console not included")
	assert.Equal(t, "DENY", rec.Header().Get("X-Frame-Options"))

	// index.html missing from the FS also falls back to the placeholder.
	h = newSPA(t, fstest.MapFS{"assets/a.js": {Data: []byte("x")}})
	rec = get(h, http.MethodGet, "/")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "console not included")

	_, ok := console.NewAssets(console.AssetsDeps{}).FS()
	assert.False(t, ok)
	fsys, ok := console.NewAssets(console.AssetsDeps{FS: testFS()}).FS()
	require.True(t, ok)
	assert.NotNil(t, fsys)
}

func TestSPAOptions(t *testing.T) {
	r := chi.NewRouter()
	console.NewSPARoutes(console.SPADeps{Assets: console.NewAssets(console.AssetsDeps{FS: testFS()})}, console.WithCSP("default-src 'none'"), console.WithAPIPrefixes("/rpc/")).Register(r)
	rec := get(r, http.MethodGet, "/api/v1/x")
	assert.Equal(t, http.StatusOK, rec.Code, "custom prefixes replace the defaults")
	assert.Equal(t, "default-src 'none'", rec.Header().Get("Content-Security-Policy"))
	rec = get(r, http.MethodGet, "/rpc/x")
	assert.Equal(t, http.StatusNotFound, rec.Code)
}
