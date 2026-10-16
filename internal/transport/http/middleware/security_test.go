package middleware_test

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
)

// apiPath selects the requests of an API under /api/.
func apiPath(r *http.Request) bool { return strings.HasPrefix(r.URL.Path, "/api/") }

// covers: D-AUTH-01
func TestSecurityHeaders(t *testing.T) {
	// Pentest 2026-10 (PT-01, PT-02): the responses of the Admin API were
	// cacheable and sniffable, and the TLS console sent no HSTS.
	mw := middleware.NewSecurityHeaders(middleware.SecurityHeadersDeps{}, middleware.WithNoStore(apiPath))
	h := mw.Wrap(okHandler())

	t.Run("plain HTTP API response", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/auth/session", nil))
		assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
		assert.Equal(t, "SAMEORIGIN", rec.Header().Get("X-Frame-Options"))
		assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
		assert.Empty(t, rec.Header().Get("Strict-Transport-Security"), "no HSTS over plain HTTP")
	})

	t.Run("TLS", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.TLS = &tls.ConnectionState{}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		assert.Equal(t, "max-age=31536000", rec.Header().Get("Strict-Transport-Security"))
		assert.Empty(t, rec.Header().Get("Cache-Control"), "only the selected requests default to no-store")
	})

	t.Run("TLS terminated by a trusted proxy", func(t *testing.T) {
		fwd, err := middleware.NewForwarded(middleware.ForwardedDeps{}, middleware.WithTrustedProxies("192.0.2.0/24"))
		assert.NoError(t, err)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/info", nil)
		req.RemoteAddr = "192.0.2.10:4000"
		req.Header.Set("X-Forwarded-Proto", "https")
		rec := httptest.NewRecorder()
		middleware.Chain(okHandler(), fwd, mw).ServeHTTP(rec, req)
		assert.Equal(t, "max-age=31536000", rec.Header().Get("Strict-Transport-Security"))
	})

	t.Run("handlers override the defaults", func(t *testing.T) {
		own := mw.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Cache-Control", "private, no-cache")
			w.Header().Set("X-Frame-Options", "DENY")
			w.WriteHeader(http.StatusOK)
		}))
		rec := httptest.NewRecorder()
		own.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/buckets/b/objects/download?key=k", nil))
		assert.Equal(t, "private, no-cache", rec.Header().Get("Cache-Control"))
		assert.Equal(t, "DENY", rec.Header().Get("X-Frame-Options"))
	})

	t.Run("HSTS disabled", func(t *testing.T) {
		off := middleware.NewSecurityHeaders(middleware.SecurityHeadersDeps{}, middleware.WithHSTSMaxAge(0)).Wrap(okHandler())
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.TLS = &tls.ConnectionState{}
		rec := httptest.NewRecorder()
		off.ServeHTTP(rec, req)
		assert.Empty(t, rec.Header().Get("Strict-Transport-Security"))
		custom := middleware.NewSecurityHeaders(middleware.SecurityHeadersDeps{}, middleware.WithHSTSMaxAge(time.Hour)).Wrap(okHandler())
		rec = httptest.NewRecorder()
		custom.ServeHTTP(rec, req)
		assert.Equal(t, "max-age=3600", rec.Header().Get("Strict-Transport-Security"))
	})
}
