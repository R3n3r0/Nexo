// Package session manages console sessions: login with access/secret key
// yields a signed token stored by the transport in an HttpOnly cookie, plus
// a CSRF token the browser sends back in a header (double submit).
package session

import (
	"context"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
)

// Service manages console sessions.
type Service interface {
	// Login verifies the credentials and issues a session. Invalid
	// credentials return domain.ErrAccessDenied; disabled or expired ones
	// too. The returned Session carries the signed Token.
	Login(ctx context.Context, accessKey, secretKey string) (*Session, error)
	// Validate parses and verifies a token and returns its session. Expired,
	// tampered or revoked tokens return domain.ErrAccessDenied.
	Validate(ctx context.Context, token string) (*Session, error)
	// Logout revokes the token. Unknown tokens are ignored.
	Logout(ctx context.Context, token string) error
	// ValidateCSRF checks that csrfToken (from the request header) matches
	// the session's token; mismatches return domain.ErrAccessDenied.
	ValidateCSRF(ctx context.Context, s *Session, csrfToken string) error
}

// Session is an authenticated console session.
type Session struct {
	// ID is the unique session identifier (JWT jti).
	ID        string
	AccessKey string
	Identity  domain.Identity
	IssuedAt  time.Time
	ExpiresAt time.Time
	// CSRFToken must be echoed by the browser in the X-CSRF-Token header.
	CSRFToken string
	// Token is the signed session token, populated by Login only.
	Token string
}

// Header and cookie names used by the transport.
const (
	// CookieName is the HttpOnly cookie carrying the signed session token.
	CookieName = "nexo_session"
	// CSRFCookieName is the script-readable cookie carrying the CSRF token
	// (double submit): the console sends it back in CSRFHeader.
	CSRFCookieName = "nexo_csrf"
	// CSRFHeader is the request header carrying the CSRF token.
	CSRFHeader = "X-CSRF-Token"
)
