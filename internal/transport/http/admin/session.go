package admin

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/session"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// SessionDeps are the dependencies of the session routes.
type SessionDeps struct {
	Sessions session.Service
	Codec    httpx.Codec
	Clock    clock.Clock
}

// SessionOption configures the session routes.
type SessionOption func(*sessionRoutes)

// WithSecureCookie forces the Secure cookie flag (default: set when the
// request arrived over TLS).
func WithSecureCookie(secure bool) SessionOption {
	return func(h *sessionRoutes) { h.secure = &secure }
}

type sessionRoutes struct {
	deps   SessionDeps
	secure *bool
}

var _ SessionRoutes = (*sessionRoutes)(nil)

// NewSessionRoutes returns the console session routes.
func NewSessionRoutes(deps SessionDeps, opts ...SessionOption) SessionRoutes {
	if deps.Codec == nil {
		deps.Codec = httpx.New()
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	h := &sessionRoutes{deps: deps}
	for _, o := range opts {
		o(h)
	}
	return h
}

// Register implements Routes.
func (h *sessionRoutes) Register(r chi.Router) {
	r.Post("/auth/login", h.login)
	r.Post("/auth/logout", h.logout)
	r.Get("/auth/session", h.session)
}

// LoginRequest is the JSON body of POST /auth/login.
type LoginRequest struct {
	AccessKey string `json:"accessKey"`
	SecretKey string `json:"secretKey"`
}

// IdentityResponse is the principal behind a console session.
type IdentityResponse struct {
	AccessKey   string   `json:"accessKey"`
	IsRoot      bool     `json:"isRoot"`
	Groups      []string `json:"groups"`
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
	// /auth/session so that a reloaded page can recover it; the response is
	// JSON on a same-origin API, so a third-party page cannot read it.
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

func (h *sessionRoutes) login(w http.ResponseWriter, r *http.Request) {
	var in LoginRequest
	if err := h.deps.Codec.DecodeJSON(r.Body, 64<<10, &in); err != nil {
		h.deps.Codec.WriteProblemError(w, r, err)
		return
	}
	sess, err := h.deps.Sessions.Login(r.Context(), in.AccessKey, in.SecretKey)
	if err != nil {
		if errors.Is(err, domain.ErrAccessDenied) {
			h.deps.Codec.WriteProblem(w, r, httpx.Problem{Status: http.StatusUnauthorized, Code: "invalid_credentials", Detail: "invalid access key or secret key"})
			return
		}
		h.deps.Codec.WriteProblemError(w, r, err)
		return
	}
	http.SetCookie(w, h.cookie(r, session.CookieName, sess.Token, sess.ExpiresAt, true))
	// The CSRF cookie is readable by the console scripts (double submit).
	http.SetCookie(w, h.cookie(r, session.CSRFCookieName, sess.CSRFToken, sess.ExpiresAt, false))
	h.deps.Codec.WriteJSON(w, http.StatusOK, newSessionResponse(sess))
}

func (h *sessionRoutes) session(w http.ResponseWriter, r *http.Request) {
	sess, err := SessionFromRequest(r, h.deps.Sessions)
	if err != nil {
		h.deps.Codec.WriteProblem(w, r, httpx.Problem{Status: http.StatusUnauthorized, Code: "unauthenticated", Detail: "no valid session"})
		return
	}
	h.deps.Codec.WriteJSON(w, http.StatusOK, newSessionResponse(sess))
}

func (h *sessionRoutes) logout(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(session.CookieName)
	if err != nil {
		h.deps.Codec.WriteProblem(w, r, httpx.Problem{Status: http.StatusUnauthorized, Code: "unauthenticated", Detail: "no session cookie"})
		return
	}
	sess, err := h.deps.Sessions.Validate(r.Context(), c.Value)
	if err == nil {
		if err := h.deps.Sessions.ValidateCSRF(r.Context(), sess, r.Header.Get(session.CSRFHeader)); err != nil {
			h.deps.Codec.WriteProblem(w, r, httpx.Problem{Status: http.StatusForbidden, Code: "invalid_csrf_token", Detail: "missing or invalid CSRF token"})
			return
		}
	}
	if err := h.deps.Sessions.Logout(r.Context(), c.Value); err != nil {
		h.deps.Codec.WriteProblemError(w, r, fmt.Errorf("logout: %w", err))
		return
	}
	expired := h.deps.Clock.Now().Add(-time.Hour)
	http.SetCookie(w, h.cookie(r, session.CookieName, "", expired, true))
	http.SetCookie(w, h.cookie(r, session.CSRFCookieName, "", expired, false))
	w.WriteHeader(http.StatusNoContent)
}

// cookie builds a session-scoped cookie; an empty value clears it.
func (h *sessionRoutes) cookie(r *http.Request, name, value string, expires time.Time, httpOnly bool) *http.Cookie {
	secure := r.TLS != nil
	if h.secure != nil {
		secure = *h.secure
	}
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

// SessionFromRequest validates the session cookie of r.
func SessionFromRequest(r *http.Request, sessions session.Service) (*session.Session, error) {
	c, err := r.Cookie(session.CookieName)
	if err != nil {
		return nil, fmt.Errorf("%w: no session cookie", domain.ErrAccessDenied)
	}
	return sessions.Validate(r.Context(), c.Value)
}

// nonNil returns s, or an empty slice when s is nil, so that JSON clients
// always receive an array.
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
