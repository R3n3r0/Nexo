// Package session manages console sessions: login with access/secret key
// yields a signed token stored by the transport in an HttpOnly cookie, plus
// a CSRF token, bound to the session, the browser sends back in a header.
package session

import (
	"context"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
)

// Service manages console sessions.
type Service interface {
	// Login verifies the credentials and issues a session. Invalid
	// credentials return domain.ErrAccessDenied; disabled, expired and
	// temporary (STS, which need their session token) ones too. After too
	// many failed attempts of the same client on the same access key, or
	// of the same client on any access key, the attempts are refused
	// without being verified with a *ThrottledError
	// (domain.ErrTooManyRequests) until the lockout ends. The returned
	// Session carries the signed Token.
	Login(ctx context.Context, in LoginInput) (*Session, error)
	// Validate parses and verifies a token and returns its session. Expired,
	// tampered or revoked tokens return domain.ErrAccessDenied, and so do
	// the tokens issued before the secret key of their credentials changed:
	// rotating a secret closes every console session opened with it, on
	// every node and across restarts, and so does disabling the
	// credentials or their parent user (domain.Credentials.RevocationEpoch):
	// enabling them again does not bring the sessions back.
	Validate(ctx context.Context, token string) (*Session, error)
	// Logout revokes the token until it expires; the revocation is
	// persisted (Deps.Store), so that it survives a restart and is seen by
	// every node sharing the store. Unknown tokens are ignored.
	Logout(ctx context.Context, token string) error
	// ValidateCSRF checks that csrfToken (from the request header) matches
	// the session's token; mismatches return domain.ErrAccessDenied.
	ValidateCSRF(ctx context.Context, s *Session, csrfToken string) error
	// LoginLDAP authenticates a user of the LDAP directory (identity_ldap)
	// and issues a session backed by federated temporary credentials
	// (iam.Service.AssumeFederated), whose secret never leaves the server.
	// Wrong credentials are domain.ErrAccessDenied, throttled per client
	// and username like Login; an identity mapping to no policy is
	// domain.ErrAccessDenied too; a disabled directory is
	// domain.ErrNotImplemented and an unreachable one
	// domain.ErrServiceUnavailable.
	LoginLDAP(ctx context.Context, in LDAPLoginInput) (*Session, error)
	// LoginOpenID completes the OpenID Connect authorization code flow of
	// the console (idp.Service.Exchange) and issues a session backed by
	// federated temporary credentials, as LoginLDAP.
	LoginOpenID(ctx context.Context, in OpenIDLoginInput) (*Session, error)
}

// LDAPLoginInput are the credentials of a console login with LDAP.
type LDAPLoginInput struct {
	Username string
	Password string
	// Client identifies the caller for the throttling, as in LoginInput.
	Client string
}

// OpenIDLoginInput completes a console login with OpenID Connect: the
// parameters of idp.ExchangeInput.
type OpenIDLoginInput struct {
	Code         string
	RedirectURI  string
	CodeVerifier string
	Nonce        string
}

// LoginInput are the credentials of a console login.
type LoginInput struct {
	AccessKey string
	SecretKey string
	// Client identifies the caller (its IP address) for the throttling of
	// failed attempts; empty throttles the access key alone.
	Client string
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
	// Token is the signed session token, populated by the logins only.
	Token string
	// Provider names the identity provider of a federated session
	// ("openid", "ldap"), empty for the access keys; Username is then the
	// user at the provider.
	Provider string
	Username string
}

// Header and cookie names used by the transport.
const (
	// CookieName is the HttpOnly cookie carrying the signed session token.
	CookieName = "nexo_session"
	// CSRFCookieName is the script-readable cookie older versions set with
	// the CSRF token. It is no longer set (a page of another port of the
	// same host could read it): the token travels in the session payload
	// and logout clears the cookie.
	CSRFCookieName = "nexo_csrf"
	// CSRFHeader is the request header carrying the CSRF token.
	CSRFHeader = "X-CSRF-Token"
	// CSRFFormField is the form field carrying the CSRF token of the forms
	// the console submits (application/x-www-form-urlencoded), for the
	// downloads the browser streams to disk.
	CSRFFormField = "csrfToken"
)
