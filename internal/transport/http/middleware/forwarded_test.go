package middleware_test

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
)

func TestForwardedClient(t *testing.T) {
	tests := []struct {
		name    string
		trusted []string
		remote  string
		tls     bool
		headers map[string][]string
		want    middleware.Client
	}{
		{name: "no proxy configured: the peer", remote: "203.0.113.9:5000",
			headers: map[string][]string{"X-Forwarded-For": {"198.51.100.1"}, "X-Forwarded-Proto": {"https"}},
			want:    middleware.Client{IP: "203.0.113.9"}},
		{name: "untrusted peer: headers ignored", trusted: []string{"10.0.0.0/8"}, remote: "203.0.113.9:5000",
			headers: map[string][]string{"X-Forwarded-For": {"198.51.100.1"}, "X-Forwarded-Proto": {"https"}},
			want:    middleware.Client{IP: "203.0.113.9"}},
		{name: "trusted proxy: the forwarded client", trusted: []string{"10.0.0.1"}, remote: "10.0.0.1:5000",
			headers: map[string][]string{"X-Forwarded-For": {"198.51.100.1"}, "X-Forwarded-Proto": {"https"}},
			want:    middleware.Client{IP: "198.51.100.1", Secure: true}},
		{name: "forged entries left of the client are ignored", trusted: []string{"10.0.0.0/8"}, remote: "10.0.0.2:5000",
			headers: map[string][]string{"X-Forwarded-For": {"1.2.3.4, 198.51.100.1", "10.0.0.7"}},
			want:    middleware.Client{IP: "198.51.100.1"}},
		{name: "garbage left of the client", trusted: []string{"10.0.0.0/8"}, remote: "10.0.0.2:5000",
			headers: map[string][]string{"X-Forwarded-For": {"not-an-ip, 198.51.100.1"}},
			want:    middleware.Client{IP: "198.51.100.1"}},
		{name: "only proxies: the farthest one", trusted: []string{"10.0.0.0/8"}, remote: "10.0.0.2:5000",
			headers: map[string][]string{"X-Forwarded-For": {"10.0.0.9"}},
			want:    middleware.Client{IP: "10.0.0.9"}},
		{name: "trusted proxy without headers", trusted: []string{"10.0.0.0/8"}, remote: "10.0.0.2:5000",
			want: middleware.Client{IP: "10.0.0.2"}},
		{name: "ipv6 and mapped ipv4", trusted: []string{"::1"}, remote: "[::1]:5000",
			headers: map[string][]string{"X-Forwarded-For": {"::ffff:198.51.100.1"}},
			want:    middleware.Client{IP: "198.51.100.1"}},
		{name: "last proto wins", trusted: []string{"10.0.0.1"}, remote: "10.0.0.1:5000",
			headers: map[string][]string{"X-Forwarded-Proto": {"https, http"}},
			want:    middleware.Client{IP: "10.0.0.1"}},
		{name: "direct tls", remote: "203.0.113.9:5000", tls: true,
			want: middleware.Client{IP: "203.0.113.9", Secure: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mw, err := middleware.NewForwarded(middleware.ForwardedDeps{}, middleware.WithTrustedProxies(tt.trusted...))
			require.NoError(t, err)
			var got middleware.Client
			h := mw.Wrap(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = middleware.ClientOf(r) }))
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tt.remote
			if tt.tls {
				req.TLS = &tls.ConnectionState{}
			}
			for k, vs := range tt.headers {
				for _, v := range vs {
					req.Header.Add(k, v)
				}
			}
			h.ServeHTTP(httptest.NewRecorder(), req)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestForwardedRejectsInvalidProxies(t *testing.T) {
	_, err := middleware.NewForwarded(middleware.ForwardedDeps{}, middleware.WithTrustedProxies("10.0.0.0/33"))
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = middleware.NewForwarded(middleware.ForwardedDeps{}, middleware.WithTrustedProxies("proxy.local"))
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

func TestClientOfWithoutMiddleware(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.9:5000"
	req.Header.Set("X-Forwarded-For", "198.51.100.1")
	assert.Equal(t, middleware.Client{IP: "203.0.113.9"}, middleware.ClientOf(req), "the peer, headers ignored")
}
