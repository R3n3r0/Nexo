package admin

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/idp"
	"github.com/r3n3r0/nexo/internal/service/session"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
)

// The console login with the external identity providers (docs/02
// D-AUTH-02): GET /auth/providers tells the login page which ones are
// enabled; GET /auth/openid/start sends the browser to the OpenID
// provider and GET /auth/openid/callback brings it back with a session;
// the LDAP users log in through POST /auth/login (LoginRequest.Provider).
const (
	// oidcCookie carries the state, the nonce and the PKCE verifier of an
	// authorization request between the start and the callback. It is
	// SameSite=Lax: the callback is a top-level navigation coming from the
	// provider, which a Strict cookie would not follow; the state it
	// carries binds the callback to the browser that started the login
	// (no login CSRF).
	oidcCookie     = "nexo_oidc"
	oidcCookieLife = 10 * time.Minute
	oidcPath       = "/auth/openid"
	callbackPath   = oidcPath + "/callback"

	// Values of the error query parameter of the login page after a
	// failed OpenID login.
	ssoDenied      = "sso_denied"
	ssoUnavailable = "sso_unavailable"
	ssoFailed      = "sso_failed"
)

// AuthProvidersResponse is the body of GET /auth/providers.
type AuthProvidersResponse struct {
	// OpenID reports a console login with OpenID Connect, shown as
	// OpenIDName.
	OpenID     bool   `json:"openid"`
	OpenIDName string `json:"openidName,omitempty"`
	// LDAP reports a console login with the LDAP username and password.
	LDAP bool `json:"ldap"`
}

func (h *sessionRoutes) providers(w http.ResponseWriter, r *http.Request) {
	var out AuthProvidersResponse
	if h.deps.IDP != nil {
		p, err := h.deps.IDP.Providers(r.Context())
		if err != nil {
			writeError(h.deps.Codec, w, r, err)
			return
		}
		out = AuthProvidersResponse{OpenID: p.OpenID, OpenIDName: p.OpenIDName, LDAP: p.LDAP}
	}
	h.deps.Codec.WriteJSON(w, http.StatusOK, out)
}

// redirectURI is the callback URL registered at the provider: the
// configured one, else the one of the public URL of the console, else the
// one of the host the browser reached.
func (h *sessionRoutes) redirectURI(r *http.Request, configured string) string {
	if configured != "" {
		return configured
	}
	if h.deps.ConsoleURL != "" {
		return strings.TrimSuffix(h.deps.ConsoleURL, "/") + Prefix + callbackPath
	}
	scheme := "http"
	if middleware.ClientOf(r).Secure {
		scheme = "https"
	}
	return scheme + "://" + r.Host + Prefix + callbackPath
}

// consolePath is a path of the console (the page after the login, the
// login page with an error).
func (h *sessionRoutes) consolePath(p string) string {
	if h.deps.ConsoleURL != "" {
		return strings.TrimSuffix(h.deps.ConsoleURL, "/") + p
	}
	return p
}

// loginFailed sends the browser back to the login page with the error,
// and the cause to the logs of the request.
func (h *sessionRoutes) loginFailed(w http.ResponseWriter, r *http.Request, code string, err error) {
	if err != nil {
		httpx.SetOperationError(r.Context(), err)
	}
	http.Redirect(w, r, h.consolePath("/login?error="+url.QueryEscape(code)), http.StatusFound)
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func (h *sessionRoutes) openIDStart(w http.ResponseWriter, r *http.Request) {
	if h.deps.IDP == nil {
		h.loginFailed(w, r, ssoUnavailable, errors.New("no identity provider"))
		return
	}
	p, err := h.deps.IDP.Providers(r.Context())
	if err != nil || !p.OpenID {
		h.loginFailed(w, r, ssoUnavailable, err)
		return
	}
	var parts [3]string // state, nonce, verifier
	for i := range parts {
		if parts[i], err = randomToken(); err != nil {
			h.loginFailed(w, r, ssoFailed, err)
			return
		}
	}
	challenge := sha256.Sum256([]byte(parts[2]))
	target, err := h.deps.IDP.AuthCodeURL(r.Context(), idp.AuthCodeInput{
		RedirectURI: h.redirectURI(r, p.OpenIDRedirect), State: parts[0], Nonce: parts[1],
		CodeChallenge: base64.RawURLEncoding.EncodeToString(challenge[:]),
	})
	if err != nil {
		code := ssoFailed
		if errors.Is(err, domain.ErrServiceUnavailable) || errors.Is(err, domain.ErrNotImplemented) {
			code = ssoUnavailable
		}
		h.loginFailed(w, r, code, err)
		return
	}
	http.SetCookie(w, h.oidcCookie(r, strings.Join(parts[:], "."), h.deps.Clock.Now().Add(oidcCookieLife)))
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, target, http.StatusFound)
}

func (h *sessionRoutes) oidcCookie(r *http.Request, value string, expires time.Time) *http.Cookie {
	c := h.cookie(r, oidcCookie, value, expires, true)
	c.Path = Prefix + oidcPath
	c.SameSite = http.SameSiteLaxMode
	return c
}

func (h *sessionRoutes) openIDCallback(w http.ResponseWriter, r *http.Request) {
	// The flow is single use: its cookie goes whatever the outcome.
	http.SetCookie(w, h.oidcCookie(r, "", h.deps.Clock.Now().Add(-time.Hour)))
	w.Header().Set("Cache-Control", "no-store")
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		h.loginFailed(w, r, ssoDenied, errors.New("openid provider: "+e))
		return
	}
	c, err := r.Cookie(oidcCookie)
	if err != nil {
		h.loginFailed(w, r, ssoFailed, errors.New("openid callback without a login in progress"))
		return
	}
	parts := strings.Split(c.Value, ".")
	state := q.Get("state")
	if len(parts) != 3 || state == "" || subtle.ConstantTimeCompare([]byte(parts[0]), []byte(state)) != 1 {
		h.loginFailed(w, r, ssoFailed, errors.New("openid callback: state mismatch"))
		return
	}
	if h.deps.IDP == nil {
		h.loginFailed(w, r, ssoUnavailable, errors.New("no identity provider"))
		return
	}
	p, err := h.deps.IDP.Providers(r.Context())
	if err != nil || !p.OpenID {
		h.loginFailed(w, r, ssoUnavailable, err)
		return
	}
	sess, err := h.deps.Sessions.LoginOpenID(r.Context(), session.OpenIDLoginInput{
		Code: q.Get("code"), RedirectURI: h.redirectURI(r, p.OpenIDRedirect), CodeVerifier: parts[2], Nonce: parts[1],
	})
	switch {
	case errors.Is(err, domain.ErrAccessDenied):
		h.loginFailed(w, r, ssoDenied, err)
		return
	case errors.Is(err, domain.ErrServiceUnavailable), errors.Is(err, domain.ErrNotImplemented):
		h.loginFailed(w, r, ssoUnavailable, err)
		return
	case err != nil:
		h.loginFailed(w, r, ssoFailed, err)
		return
	}
	identity := sess.Identity
	middleware.WithIdentity(r.Context(), &identity)
	http.SetCookie(w, h.cookie(r, session.CookieName, sess.Token, sess.ExpiresAt, true))
	http.Redirect(w, r, h.consolePath("/"), http.StatusFound)
}
