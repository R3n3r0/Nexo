package console

import (
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// SPADeps are the dependencies of the SPA routes.
type SPADeps struct {
	Assets Assets
	Enc    httpx.Encoder
}

// SPAOption configures the SPA routes.
type SPAOption func(*spaRoutes)

// WithAPIPrefixes sets the path prefixes that are never served by the SPA
// fallback (default "/api/", "/nexo/").
func WithAPIPrefixes(prefixes ...string) SPAOption {
	return func(h *spaRoutes) { h.apiPrefixes = prefixes }
}

// WithCSP replaces the default Content-Security-Policy.
func WithCSP(csp string) SPAOption {
	return func(h *spaRoutes) { h.csp = csp }
}

// DefaultCSP is the baseline Content-Security-Policy of the console.
const DefaultCSP = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: blob:; font-src 'self' data:; connect-src 'self'; worker-src 'self' blob:; " +
	"frame-ancestors 'none'; base-uri 'self'; form-action 'self'; object-src 'none'"

// hashedAssetPattern matches Vite-style fingerprinted file names:
// name-<hash>.ext or name.<hash>.ext with a hash of at least 8 characters.
func hashedAssetPattern() *regexp.Regexp {
	return regexp.MustCompile(`[-.][A-Za-z0-9_-]{8,}\.[A-Za-z0-9]+$`)
}

type spaRoutes struct {
	deps        SPADeps
	apiPrefixes []string
	csp         string
	// hashedAsset recognizes fingerprinted assets (hashedAssetPattern).
	hashedAsset *regexp.Regexp
}

var _ SPARoutes = (*spaRoutes)(nil)

// NewSPARoutes returns the SPA routes.
func NewSPARoutes(deps SPADeps, opts ...SPAOption) SPARoutes {
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	if deps.Assets == nil {
		deps.Assets = NewAssets(AssetsDeps{})
	}
	h := &spaRoutes{deps: deps, apiPrefixes: []string{"/api/", "/nexo/"}, csp: DefaultCSP, hashedAsset: hashedAssetPattern()}
	for _, o := range opts {
		o(h)
	}
	return h
}

// Register implements Routes: a catch-all that must be registered after
// the API groups so that their static routes take precedence.
func (h *spaRoutes) Register(r chi.Router) {
	r.Get("/*", h.serve)
	r.Head("/*", h.serve)
}

func (h *spaRoutes) serve(w http.ResponseWriter, r *http.Request) {
	h.securityHeaders(w.Header())
	p := r.URL.Path
	if h.isAPIPath(p) {
		h.deps.Enc.WriteProblem(w, r, httpx.Problem{Status: http.StatusNotFound, Code: "not_found", Detail: "no such endpoint"})
		return
	}
	// A path climbing out of its directory ("/../etc/passwd", also
	// percent-encoded) is never a route of the console nor an asset.
	if hasDotDot(p) {
		http.NotFound(w, r)
		return
	}
	fsys, ok := h.deps.Assets.FS()
	if !ok {
		h.placeholder(w, r)
		return
	}
	clean := strings.TrimPrefix(path.Clean("/"+p), "/")
	if clean != "" && clean != "index.html" {
		if h.serveFile(w, r, fsys, clean) {
			return
		}
		// A missing asset is a 404, not the SPA shell: browsers must not
		// receive HTML for a script or stylesheet URL. The assets
		// directory holds files only; elsewhere, routes ending with "/"
		// are folders of the console (a prefix like "v1.2/"), never files.
		if strings.HasPrefix(clean, assetsDir+"/") || clean == assetsDir ||
			(!strings.HasSuffix(p, "/") && isStaticExtension(path.Ext(clean))) {
			http.NotFound(w, r)
			return
		}
	}
	h.serveIndex(w, r, fsys)
}

// assetsDir is the directory of the fingerprinted build output (Vite).
const assetsDir = "assets"

// isStaticExtension reports whether ext is the extension of a file a
// browser loads as an asset (script, style, image, font, manifest): a
// missing one is a 404 rather than the SPA shell. Other dotted paths are
// console routes (e.g. a folder "v1.2" of the object browser).
func isStaticExtension(ext string) bool {
	switch strings.ToLower(ext) {
	case ".js", ".mjs", ".css", ".map", ".json", ".webmanifest", ".html", ".txt", ".xml", ".wasm",
		".png", ".jpg", ".jpeg", ".gif", ".svg", ".ico", ".webp", ".avif",
		".woff", ".woff2", ".ttf", ".otf", ".eot":
		return true
	default:
		return false
	}
}

// hasDotDot reports whether a (decoded) URL path has a ".." segment, or a
// backslash some file systems treat as a separator.
func hasDotDot(p string) bool {
	if strings.Contains(p, "\\") {
		return true
	}
	for _, segment := range strings.Split(p, "/") {
		if segment == ".." {
			return true
		}
	}
	return false
}

func (h *spaRoutes) isAPIPath(p string) bool {
	for _, prefix := range h.apiPrefixes {
		if strings.HasPrefix(p, prefix) || p == strings.TrimSuffix(prefix, "/") {
			return true
		}
	}
	return false
}

func (h *spaRoutes) serveFile(w http.ResponseWriter, r *http.Request, fsys fs.FS, name string) bool {
	f, err := fsys.Open(name)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		return false
	}
	if h.hashedAsset.MatchString(path.Base(name)) || strings.HasPrefix(name, assetsDir+"/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=3600")
	}
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		// fs.File without Seek (fstest.MapFS files do seek; embed.FS too).
		data, err := io.ReadAll(f)
		if err != nil {
			return false
		}
		w.Header().Set("Content-Length", itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(data)
		}
		return true
	}
	http.ServeContent(w, r, name, st.ModTime(), rs)
	return true
}

func (h *spaRoutes) serveIndex(w http.ResponseWriter, r *http.Request, fsys fs.FS) {
	data, err := fs.ReadFile(fsys, "index.html")
	if err != nil {
		h.placeholder(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}

const placeholderHTML = `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Nexo</title></head>
<body style="font-family:system-ui;margin:3rem"><h1>Nexo console not included</h1>
<p>This binary was built without the embedded dashboard (build tag <code>embedui</code>).
Run <code>make build</code> to include it, or use the Admin API at <code>/api/v1</code>.</p></body></html>`

func (h *spaRoutes) placeholder(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = io.WriteString(w, placeholderHTML)
	}
}

func (h *spaRoutes) securityHeaders(hd http.Header) {
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("X-Frame-Options", "DENY")
	hd.Set("Referrer-Policy", "strict-origin-when-cross-origin")
	hd.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	if h.csp != "" {
		hd.Set("Content-Security-Policy", h.csp)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
