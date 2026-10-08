package admin

import (
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/auth"
	"github.com/r3n3r0/nexo/internal/service/session"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
)

// sigV4Scheme prefixes the Authorization header of SigV4 requests.
const sigV4Scheme = "AWS4-"

// AuthenticatorDeps are the dependencies of the Admin API authentication.
type AuthenticatorDeps struct {
	// Sessions validates the console session cookies (required).
	Sessions session.Service
	// Verifier authenticates SigV4-signed requests (ADR 007: service "s3"
	// and the region of the server), the way of the CLI and the SDK. Nil
	// disables them: only console sessions are accepted.
	Verifier auth.Verifier
	Enc      httpx.Encoder
}

// AuthenticatorOption configures the authentication middleware.
type AuthenticatorOption func(*authenticator)

// WithConsoleOrigins adds origins (scheme://host[:port]) the console is
// served from besides the host the request names: the public URL of a
// console behind a proxy that rewrites the Host header. They matter only
// for the browsers that send no Sec-Fetch-Site header (see
// authenticator.crossOrigin); an invalid origin is a wiring error
// (domain.ErrInvalidArgument).
func WithConsoleOrigins(origins ...string) AuthenticatorOption {
	return func(a *authenticator) { a.rawOrigins = append(a.rawOrigins, origins...) }
}

type authenticator struct {
	deps       AuthenticatorDeps
	rawOrigins []string
	// origins are the normalized extra console origins.
	origins []string
}

var _ middleware.Middleware = (*authenticator)(nil)

// NewAuthenticator returns the authentication middleware of the Admin
// API. It accepts either
//
//   - a request signed with SigV4 in the Authorization header (presigned
//     URLs are refused), verified by the same verifier as the S3 API with
//     the s3 service scope: CSRF does not apply, the browser never signs;
//   - the console session cookie, validated (and re-resolved in IAM) by
//     the session service. A state-changing request must come from the
//     console itself (see crossOrigin) and carry the CSRF token of the
//     session: the X-CSRF-Token header, or, on the one route the console
//     submits as a form, its csrfToken field (see csrfToken).
//
// The cookies are host-only and SameSite=Strict, but a browser sends them
// to every port of the host and from every page of the same site: a page
// served by the S3 listener of the same host (an HTML object behind a
// presigned or share link, a public bucket) is same-site with the console.
// The origin check and the token, which the console holds in memory and
// never exposes in a cookie, keep such a page from acting in the session.
//
// It stores the identity (and the session) in the context. Failures are
// problem+json responses: 401 unauthenticated (or the signature error),
// 403 invalid_csrf_token. A missing session service is a wiring error
// (domain.ErrInvalidArgument).
func NewAuthenticator(deps AuthenticatorDeps, opts ...AuthenticatorOption) (middleware.Middleware, error) {
	if deps.Sessions == nil {
		return nil, fmt.Errorf("%w: admin authentication requires the session service", domain.ErrInvalidArgument)
	}
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	a := &authenticator{deps: deps}
	for _, o := range opts {
		o(a)
	}
	for _, raw := range a.rawOrigins {
		origin, ok := originOf(raw)
		if !ok {
			return nil, fmt.Errorf("%w: console origin %q is not an absolute http(s) URL", domain.ErrInvalidArgument, raw)
		}
		a.origins = append(a.origins, origin)
	}
	return a, nil
}

// Wrap implements middleware.Middleware.
func (a *authenticator) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if q := r.URL.Query(); q.Has("X-Amz-Signature") || q.Has("X-Amz-Credential") {
			problem(a.deps.Enc, w, r, http.StatusUnauthorized, codeUnauthenticated, "presigned URLs are not accepted by the Admin API")
			return
		}
		if authz := r.Header.Get("Authorization"); authz != "" {
			if !strings.HasPrefix(authz, sigV4Scheme) {
				problem(a.deps.Enc, w, r, http.StatusUnauthorized, codeUnauthenticated, "unsupported authorization scheme: sign the request with AWS Signature Version 4")
				return
			}
			a.signed(w, r, next)
			return
		}
		a.cookie(w, r, next)
	})
}

// signed authenticates a SigV4 request.
func (a *authenticator) signed(w http.ResponseWriter, r *http.Request, next http.Handler) {
	if a.deps.Verifier == nil {
		problem(a.deps.Enc, w, r, http.StatusUnauthorized, codeUnauthenticated, "signed requests are not enabled")
		return
	}
	ctx := auth.WithExpectedService(r.Context(), auth.ServiceS3)
	id, err := a.deps.Verifier.Verify(ctx, r)
	if err != nil {
		writeError(a.deps.Enc, w, r, err)
		return
	}
	if id == nil || id.IsAnonymous {
		problem(a.deps.Enc, w, r, http.StatusUnauthorized, codeUnauthenticated, "the request is not signed")
		return
	}
	next.ServeHTTP(w, r.WithContext(middleware.WithIdentity(ctx, id)))
}

// cookie authenticates a console request.
func (a *authenticator) cookie(w http.ResponseWriter, r *http.Request, next http.Handler) {
	c, err := r.Cookie(session.CookieName)
	if err != nil || c.Value == "" {
		problem(a.deps.Enc, w, r, http.StatusUnauthorized, codeUnauthenticated, "no session cookie or signature")
		return
	}
	sess, err := a.deps.Sessions.Validate(r.Context(), c.Value)
	if err != nil {
		problem(a.deps.Enc, w, r, http.StatusUnauthorized, codeUnauthenticated, "no valid session")
		return
	}
	if mutating(r.Method) {
		if a.crossOrigin(r) {
			problem(a.deps.Enc, w, r, http.StatusForbidden, codeInvalidCSRFToken, "state-changing requests must come from the console itself")
			return
		}
		if err := a.deps.Sessions.ValidateCSRF(r.Context(), sess, csrfToken(w, r)); err != nil {
			problem(a.deps.Enc, w, r, http.StatusForbidden, codeInvalidCSRFToken, "missing or invalid CSRF token")
			return
		}
	}
	identity := sess.Identity
	ctx := middleware.WithIdentity(middleware.WithSession(r.Context(), sess), &identity)
	next.ServeHTTP(w, r.WithContext(ctx))
}

// crossOrigin reports a browser request that does not come from a page of
// the console: a Sec-Fetch-Site header other than same-origin (a page of
// another port of the same host is same-site), or, from a browser that
// sends no Sec-Fetch-Site, an Origin (else a Referer) whose host is not the
// one the request names nor a console origin (WithConsoleOrigins). The
// host comparison covers the scheme: one listener does not serve both.
// A request with none of these headers is not a browser's: a browser sets
// Origin on every cross-origin state-changing request.
func (a *authenticator) crossOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return !strings.EqualFold(site, "same-origin")
	}
	from := r.Header.Get("Origin")
	if from == "" {
		from = r.Header.Get("Referer")
	}
	if from == "" {
		return false
	}
	origin, ok := originOf(from)
	if !ok {
		// "null" (a sandboxed or opaque origin) or garbage.
		return true
	}
	if u, err := url.Parse(origin); err == nil && strings.EqualFold(u.Host, r.Host) {
		return false
	}
	for _, o := range a.origins {
		if strings.EqualFold(o, origin) {
			return false
		}
	}
	return true
}

// originOf returns the origin (scheme://host[:port]) of an absolute http(s)
// URL.
func originOf(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", false
	}
	switch scheme := strings.ToLower(u.Scheme); scheme {
	case "http", "https":
		return scheme + "://" + strings.ToLower(u.Host), true
	}
	return "", false
}

// maxFormBody bounds the body of the forms the console submits.
const maxFormBody = 4 << 20

// csrfToken returns the CSRF token of a state-changing request: the
// X-CSRF-Token header the console scripts set, or else, on the routes that
// accept it (formTokenRoute), the csrfToken field of the form the console
// submits (POST, application/x-www-form-urlencoded) so that the browser
// streams a download to disk. No other route reads a form: a form is what
// a page of another origin can send without a preflight.
func csrfToken(w http.ResponseWriter, r *http.Request) string {
	if t := r.Header.Get(session.CSRFHeader); t != "" {
		return t
	}
	if !formTokenRoute(r) || !isForm(r) {
		return ""
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBody)
	if err := r.ParseForm(); err != nil {
		return ""
	}
	return r.PostForm.Get(session.CSRFFormField)
}

// formTokenRoute reports the routes that accept the CSRF token in a form
// field: only the zip archive the console downloads with a form (POST
// zipDownloadPattern). The router matched the route before running the
// middlewares of its group, so the pattern is known here; it is the last
// one, relative to the API prefix, whatever the API is mounted under.
func formTokenRoute(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	rctx := chi.RouteContext(r.Context())
	if rctx == nil || len(rctx.RoutePatterns) == 0 {
		return false
	}
	return rctx.RoutePatterns[len(rctx.RoutePatterns)-1] == zipDownloadPattern
}

// isForm reports an application/x-www-form-urlencoded body.
func isForm(r *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mediaType == "application/x-www-form-urlencoded"
}

// mutating reports the state-changing methods that need the CSRF token.
func mutating(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}
