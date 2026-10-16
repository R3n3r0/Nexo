package middleware

import (
	"net/http"
	"strconv"
	"time"
)

// DefaultHSTSMaxAge is the max-age of the Strict-Transport-Security header
// of NewSecurityHeaders (one year).
const DefaultHSTSMaxAge = 365 * 24 * time.Hour

// SecurityHeadersDeps are the dependencies of the security headers
// middleware (none: the struct keeps the constructor shape of every
// middleware).
type SecurityHeadersDeps struct{}

// SecurityHeadersOption configures the security headers middleware.
type SecurityHeadersOption func(*securityHeaders)

// WithHSTSMaxAge sets the max-age of Strict-Transport-Security (default
// DefaultHSTSMaxAge); a non-positive duration disables the header.
func WithHSTSMaxAge(d time.Duration) SecurityHeadersOption {
	return func(s *securityHeaders) { s.hstsMaxAge = d }
}

// WithNoStore selects the requests whose responses default to
// Cache-Control: no-store (the responses of an authenticated API: their
// bodies, the CSRF token of the session among them, must not be kept by
// the browser nor by a cache on the way).
func WithNoStore(match func(*http.Request) bool) SecurityHeadersOption {
	return func(s *securityHeaders) { s.noStore = match }
}

type securityHeaders struct {
	hstsMaxAge time.Duration
	noStore    func(*http.Request) bool
	hsts       string
}

var _ Middleware = (*securityHeaders)(nil)

// NewSecurityHeaders returns the middleware of the baseline security
// headers of a listener serving browsers (the console):
//
//   - X-Content-Type-Options: nosniff on every response, so that a browser
//     never renders a body (a JSON error echoing a name, an object) as
//     another type than the declared one;
//   - X-Frame-Options: SAMEORIGIN, so that no other site frames a response
//     (the console frames its own PDF previews; its pages send DENY);
//   - Strict-Transport-Security on the responses to the clients that came
//     over TLS (directly or through a trusted proxy, see ClientOf), so that
//     the browser never sends the session cookie in plain text again;
//   - Cache-Control: no-store on the responses of the requests selected by
//     WithNoStore.
//
// The headers are defaults set before the handler runs: a handler that
// sets its own value (the SPA, the downloads, the event streams) wins. It
// must run after the middleware that resolves the client (NewForwarded).
func NewSecurityHeaders(_ SecurityHeadersDeps, opts ...SecurityHeadersOption) Middleware {
	s := &securityHeaders{hstsMaxAge: DefaultHSTSMaxAge}
	for _, o := range opts {
		o(s)
	}
	if s.hstsMaxAge > 0 {
		s.hsts = "max-age=" + strconv.FormatInt(int64(s.hstsMaxAge/time.Second), 10)
	}
	return s
}

// Wrap implements Middleware.
func (s *securityHeaders) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "SAMEORIGIN")
		if s.hsts != "" && ClientOf(r).Secure {
			h.Set("Strict-Transport-Security", s.hsts)
		}
		if s.noStore != nil && s.noStore(r) {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}
