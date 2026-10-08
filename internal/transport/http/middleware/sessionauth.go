package middleware

import (
	"context"
	"fmt"
	"net/http"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/session"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

type sessionKey struct{}

// WithSession stores the console session in the context.
func WithSession(ctx context.Context, s *session.Session) context.Context {
	return context.WithValue(ctx, sessionKey{}, s)
}

// SessionFromContext returns the console session set by the session
// middleware, or nil.
func SessionFromContext(ctx context.Context) *session.Session {
	s, _ := ctx.Value(sessionKey{}).(*session.Session)
	return s
}

// SessionAuthDeps are the dependencies of the session middleware.
type SessionAuthDeps struct {
	Sessions session.Service
	Enc      httpx.Encoder
}

type sessionAuth struct {
	deps SessionAuthDeps
}

var _ Middleware = (*sessionAuth)(nil)

// NewSessionAuth returns a middleware that authenticates console requests
// with the session cookie: it validates the token, enforces the CSRF header
// on state-changing methods and stores the session and its identity in the
// context. Failures are problem+json responses (401 unauthenticated, 403
// invalid_csrf_token).
func NewSessionAuth(deps SessionAuthDeps) (Middleware, error) {
	if deps.Sessions == nil {
		return nil, fmt.Errorf("%w: session middleware requires the session service", domain.ErrInvalidArgument)
	}
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	return &sessionAuth{deps: deps}, nil
}

// Wrap implements Middleware.
func (m *sessionAuth) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(session.CookieName)
		if err != nil || c.Value == "" {
			m.deps.Enc.WriteProblem(w, r, httpx.Problem{Status: http.StatusUnauthorized, Code: "unauthenticated", Detail: "no session cookie"})
			return
		}
		sess, err := m.deps.Sessions.Validate(r.Context(), c.Value)
		if err != nil {
			m.deps.Enc.WriteProblem(w, r, httpx.Problem{Status: http.StatusUnauthorized, Code: "unauthenticated", Detail: "no valid session"})
			return
		}
		if mutating(r.Method) {
			if err := m.deps.Sessions.ValidateCSRF(r.Context(), sess, r.Header.Get(session.CSRFHeader)); err != nil {
				m.deps.Enc.WriteProblem(w, r, httpx.Problem{Status: http.StatusForbidden, Code: "invalid_csrf_token", Detail: "missing or invalid CSRF token"})
				return
			}
		}
		identity := sess.Identity
		ctx := WithIdentity(WithSession(r.Context(), sess), &identity)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func mutating(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}
