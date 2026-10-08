package admin

import (
	"fmt"
	"mime"
	"net/http"
	"strings"

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

type authenticator struct {
	deps AuthenticatorDeps
}

var _ middleware.Middleware = (*authenticator)(nil)

// NewAuthenticator returns the authentication middleware of the Admin
// API. It accepts either
//
//   - a request signed with SigV4 in the Authorization header (presigned
//     URLs are refused), verified by the same verifier as the S3 API with
//     the s3 service scope: CSRF does not apply, the browser never signs;
//   - the console session cookie, validated (and re-resolved in IAM) by
//     the session service, with the CSRF token required on state-changing
//     methods: the X-CSRF-Token header (double submit), or the csrfToken
//     field of a form the console submits (see csrfToken).
//
// It stores the identity (and the session) in the context. Failures are
// problem+json responses: 401 unauthenticated (or the signature error),
// 403 invalid_csrf_token. A missing session service is a wiring error
// (domain.ErrInvalidArgument).
func NewAuthenticator(deps AuthenticatorDeps) (middleware.Middleware, error) {
	if deps.Sessions == nil {
		return nil, fmt.Errorf("%w: admin authentication requires the session service", domain.ErrInvalidArgument)
	}
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	return &authenticator{deps: deps}, nil
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
		if err := a.deps.Sessions.ValidateCSRF(r.Context(), sess, csrfToken(w, r)); err != nil {
			problem(a.deps.Enc, w, r, http.StatusForbidden, codeInvalidCSRFToken, "missing or invalid CSRF token")
			return
		}
	}
	identity := sess.Identity
	ctx := middleware.WithIdentity(middleware.WithSession(r.Context(), sess), &identity)
	next.ServeHTTP(w, r.WithContext(ctx))
}

// maxFormBody bounds the body of the forms the console submits.
const maxFormBody = 4 << 20

// csrfToken returns the CSRF token of a state-changing request: the
// X-CSRF-Token header the console scripts set, or else the csrfToken field
// of a form the console submits (POST, application/x-www-form-urlencoded)
// so that the browser streams a download to disk (zip archives). Both are
// the secret of the session: a page of another site can set neither.
func csrfToken(w http.ResponseWriter, r *http.Request) string {
	if t := r.Header.Get(session.CSRFHeader); t != "" {
		return t
	}
	if r.Method != http.MethodPost || !isForm(r) {
		return ""
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBody)
	if err := r.ParseForm(); err != nil {
		return ""
	}
	return r.PostForm.Get(session.CSRFFormField)
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
