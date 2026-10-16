package admin

import (
	"errors"
	"fmt"
	"math"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	adminsvc "github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/service/session"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
)

// SessionDeps are the dependencies of the session routes.
type SessionDeps struct {
	Sessions session.Service
	// Authz computes the effective permissions (/me/permissions) and
	// authorizes the routes (required).
	Authz adminsvc.Authorizer
	Codec httpx.Codec
	Clock clock.Clock
}

// SessionOption configures the session routes.
type SessionOption func(*sessionRoutes)

// WithSecureCookie forces the Secure cookie flag (default: set when the
// client used TLS, to this server or to a trusted proxy reporting it in
// X-Forwarded-Proto, see middleware.NewForwarded).
func WithSecureCookie(secure bool) SessionOption {
	return func(h *sessionRoutes) { h.secure = &secure }
}

type sessionRoutes struct {
	deps   SessionDeps
	guard  guard
	secure *bool
}

var _ SessionRoutes = (*sessionRoutes)(nil)

// NewSessionRoutes returns the console session routes. Login is public;
// the session and logout endpoints rely on the session middleware
// (middleware.NewSessionAuth) having validated the cookie.
func NewSessionRoutes(deps SessionDeps, opts ...SessionOption) (SessionRoutes, error) {
	if deps.Sessions == nil || deps.Authz == nil {
		return nil, fmt.Errorf("%w: session routes require the session service and the authorizer", domain.ErrInvalidArgument)
	}
	if deps.Codec == nil {
		deps.Codec = httpx.New()
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	h := &sessionRoutes{deps: deps, guard: guard{authz: deps.Authz, enc: deps.Codec}}
	for _, o := range opts {
		o(h)
	}
	return h, nil
}

// RegisterPublic implements PublicRoutes.
func (h *sessionRoutes) RegisterPublic(r chi.Router) {
	r.Post("/auth/login", h.login)
}

// Register implements Routes.
func (h *sessionRoutes) Register(r chi.Router) { h.guard.register(r, h.routes()) }

func (h *sessionRoutes) routes() []route {
	return []route{
		{http.MethodPost, "/auth/logout", authenticated(), h.logout},
		{http.MethodGet, "/auth/session", authenticated(), h.session},
		// The permissions of another user are those of an administrator.
		{http.MethodGet, "/me/permissions", authenticated().plus(func(r *http.Request) []policy.Action {
			if u := r.URL.Query().Get("user"); u != "" && u != caller(r).AccessKey {
				return []policy.Action{policy.ActionAdminGetUser}
			}
			return nil
		}, policy.ActionAdminGetUser), h.permissions},
	}
}

// LoginRequest is the JSON body of POST /auth/login.
type LoginRequest struct {
	AccessKey string `json:"accessKey"`
	SecretKey string `json:"secretKey"`
}

// IdentityResponse is the principal behind a console session.
type IdentityResponse struct {
	AccessKey string `json:"accessKey"`
	IsRoot    bool   `json:"isRoot"`
	// Groups are the enabled groups of the user.
	Groups []string `json:"groups"`
	// PolicyNames are the effective policies: attached to the user and to
	// its enabled groups.
	PolicyNames []string `json:"policyNames"`
}

// SessionResponse is the JSON body of the session endpoints. It mirrors the
// Session DTO of the web console (web/src/services/types.ts).
type SessionResponse struct {
	ID        string           `json:"id"`
	AccessKey string           `json:"accessKey"`
	Identity  IdentityResponse `json:"identity"`
	IssuedAt  time.Time        `json:"issuedAt"`
	ExpiresAt time.Time        `json:"expiresAt"`
	// CSRFToken is echoed by the client in the X-CSRF-Token header of
	// state-changing requests. It is returned by login and by GET
	// /auth/session so that a reloaded page can recover it, and never in a
	// cookie: the response is JSON on an API without CORS, so a page of
	// another origin (another port of the same host included) cannot read
	// it.
	CSRFToken string `json:"csrfToken"`
}

func newSessionResponse(sess *session.Session) SessionResponse {
	return SessionResponse{
		ID:        sess.ID,
		AccessKey: sess.AccessKey,
		Identity: IdentityResponse{
			AccessKey:   sess.AccessKey,
			IsRoot:      sess.Identity.IsRoot,
			Groups:      nonNil(sess.Identity.Groups),
			PolicyNames: nonNil(sess.Identity.PolicyNames),
		},
		IssuedAt:  sess.IssuedAt,
		ExpiresAt: sess.ExpiresAt,
		CSRFToken: sess.CSRFToken,
	}
}

// Problem codes of the login guard.
const (
	codeUnsupportedMediaType = "unsupported_media_type"
	codeCrossSiteRequest     = "cross_site_request"
)

// loginForgeable answers the logins a page of another site could forge
// (login CSRF: the victim would work in the account of the attacker and
// upload its files there) and reports whether it did. The body must be
// application/json, which a cross-site form cannot send and a cross-site
// fetch can only send after a CORS preflight the console never grants;
// a browser fetch must come from the console itself (Sec-Fetch-Site
// same-origin, or none for a request the user started).
func (h *sessionRoutes) loginForgeable(w http.ResponseWriter, r *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		problem(h.deps.Codec, w, r, http.StatusUnsupportedMediaType, codeUnsupportedMediaType, "the login body must be application/json")
		return true
	}
	switch strings.ToLower(r.Header.Get("Sec-Fetch-Site")) {
	case "cross-site", "same-site":
		problem(h.deps.Codec, w, r, http.StatusForbidden, codeCrossSiteRequest, "the login must come from the console itself")
		return true
	}
	return false
}

func (h *sessionRoutes) login(w http.ResponseWriter, r *http.Request) {
	if h.loginForgeable(w, r) {
		return
	}
	var in LoginRequest
	if err := decodeJSON(h.deps.Codec, r, &in, false); err != nil {
		writeError(h.deps.Codec, w, r, err)
		return
	}
	sess, err := h.deps.Sessions.Login(r.Context(), session.LoginInput{
		AccessKey: in.AccessKey, SecretKey: in.SecretKey, Client: clientIP(r),
	})
	if err != nil {
		var throttled *session.ThrottledError
		if errors.As(err, &throttled) {
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(throttled.RetryAfter.Seconds()))))
		}
		if errors.Is(err, domain.ErrAccessDenied) || errors.Is(err, domain.ErrInvalidAccessKey) {
			h.deps.Codec.WriteProblem(w, r, httpx.Problem{Status: http.StatusUnauthorized, Code: "invalid_credentials", Detail: "invalid access key or secret key"})
			return
		}
		writeError(h.deps.Codec, w, r, err)
		return
	}
	// The audit entry and the live trace of the login name the user who
	// signed in (the middlewares above read the identity holder).
	identity := sess.Identity
	middleware.WithIdentity(r.Context(), &identity)
	// The CSRF token travels only in the JSON answer, which the console
	// keeps in memory: a cookie readable by scripts would be readable by
	// the pages of every port of the host (the S3 listener among them).
	http.SetCookie(w, h.cookie(r, session.CookieName, sess.Token, sess.ExpiresAt, true))
	h.deps.Codec.WriteJSON(w, http.StatusOK, newSessionResponse(sess))
}

func (h *sessionRoutes) session(w http.ResponseWriter, r *http.Request) {
	sess := middleware.SessionFromContext(r.Context())
	if sess == nil {
		problem(h.deps.Codec, w, r, http.StatusUnauthorized, codeUnauthenticated, "no console session: signed requests have none")
		return
	}
	h.deps.Codec.WriteJSON(w, http.StatusOK, newSessionResponse(sess))
}

func (h *sessionRoutes) logout(w http.ResponseWriter, r *http.Request) {
	if middleware.SessionFromContext(r.Context()) == nil {
		problem(h.deps.Codec, w, r, http.StatusUnauthorized, codeUnauthenticated, "no console session: signed requests have none")
		return
	}
	// The middleware validated the cookie; its value is the token to revoke.
	if c, err := r.Cookie(session.CookieName); err == nil && c.Value != "" {
		if err := h.deps.Sessions.Logout(r.Context(), c.Value); err != nil {
			writeError(h.deps.Codec, w, r, fmt.Errorf("logout: %w", err))
			return
		}
	}
	expired := h.deps.Clock.Now().Add(-time.Hour)
	http.SetCookie(w, h.cookie(r, session.CookieName, "", expired, true))
	// Clears the readable CSRF cookie of the sessions opened by older
	// versions.
	http.SetCookie(w, h.cookie(r, session.CSRFCookieName, "", expired, false))
	w.WriteHeader(http.StatusNoContent)
}

// cookie builds a session-scoped cookie; an empty value clears it.
func (h *sessionRoutes) cookie(r *http.Request, name, value string, expires time.Time, httpOnly bool) *http.Cookie {
	secure := middleware.ClientOf(r).Secure
	if h.secure != nil {
		secure = *h.secure
	}
	// #nosec G124 -- HttpOnly is set for the session cookie by the caller; Secure follows the client connection or the configuration
	c := &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		HttpOnly: httpOnly,
		Secure:   secure,
		SameSite: http.SameSiteStrictMode,
		Expires:  expires,
	}
	if value == "" {
		c.MaxAge = -1
	}
	return c
}

// PermissionsResponse is the body of GET /me/permissions: the catalog of
// console actions split into allowed and denied, for the requester (or
// ?user=) on the whole deployment or on ?bucket= and ?prefix=.
type PermissionsResponse struct {
	User    string   `json:"user"`
	IsRoot  bool     `json:"isRoot"`
	Bucket  string   `json:"bucket,omitempty"`
	Prefix  string   `json:"prefix,omitempty"`
	Allowed []string `json:"allowed"`
	Denied  []string `json:"denied"`
}

func actionNames(actions []policy.Action) []string {
	out := make([]string, 0, len(actions))
	for _, a := range actions {
		out = append(out, string(a))
	}
	return out
}

func (h *sessionRoutes) permissions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	perms, err := h.deps.Authz.Permissions(r.Context(), adminsvc.PermissionsInput{
		Requester: requesterOf(r), User: q.Get("user"), Bucket: q.Get("bucket"), Prefix: q.Get("prefix"),
	})
	if err != nil {
		writeError(h.deps.Codec, w, r, err)
		return
	}
	h.deps.Codec.WriteJSON(w, http.StatusOK, PermissionsResponse{
		User: perms.User, IsRoot: perms.IsRoot, Bucket: perms.Bucket, Prefix: perms.Prefix,
		Allowed: actionNames(perms.Allowed), Denied: actionNames(perms.Denied),
	})
}
