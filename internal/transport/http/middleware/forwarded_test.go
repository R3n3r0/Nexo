package middleware_test

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
)

// host is the Host of the requests of httptest.NewRequest.
const host = "example.com"

func TestForwardedClient(t *testing.T) {
	tests := []struct {
		name    string
		trusted []string
		family  middleware.ForwardedHeaders
		remote  string
		tls     bool
		proxied *middleware.ProxyConn
		headers map[string][]string
		want    middleware.Client
	}{
		{name: "no proxy configured: the peer", remote: "203.0.113.9:5000",
			headers: map[string][]string{"X-Forwarded-For": {"198.51.100.1"}, "X-Forwarded-Proto": {"https"}, "X-Forwarded-Host": {"cdn.example.com"}},
			want:    middleware.Client{IP: "203.0.113.9", Host: host}},
		{name: "untrusted peer: headers ignored", trusted: []string{"10.0.0.0/8"}, remote: "203.0.113.9:5000",
			headers: map[string][]string{"X-Forwarded-For": {"198.51.100.1"}, "X-Forwarded-Proto": {"https"}, "X-Forwarded-Host": {"cdn.example.com"}},
			want:    middleware.Client{IP: "203.0.113.9", Host: host}},
		{name: "trusted proxy: the forwarded client", trusted: []string{"10.0.0.1"}, remote: "10.0.0.1:5000",
			headers: map[string][]string{"X-Forwarded-For": {"198.51.100.1"}, "X-Forwarded-Proto": {"https"}, "X-Forwarded-Host": {"cdn.example.com"}},
			want:    middleware.Client{IP: "198.51.100.1", Secure: true, Host: "cdn.example.com"}},
		{name: "forged entries left of the client are ignored", trusted: []string{"10.0.0.0/8"}, remote: "10.0.0.2:5000",
			headers: map[string][]string{"X-Forwarded-For": {"1.2.3.4, 198.51.100.1", "10.0.0.7"}},
			want:    middleware.Client{IP: "198.51.100.1", Host: host}},
		{name: "garbage left of the client", trusted: []string{"10.0.0.0/8"}, remote: "10.0.0.2:5000",
			headers: map[string][]string{"X-Forwarded-For": {"not-an-ip, 198.51.100.1"}},
			want:    middleware.Client{IP: "198.51.100.1", Host: host}},
		{name: "only proxies: the farthest one", trusted: []string{"10.0.0.0/8"}, remote: "10.0.0.2:5000",
			headers: map[string][]string{"X-Forwarded-For": {"10.0.0.9"}},
			want:    middleware.Client{IP: "10.0.0.9", Host: host}},
		{name: "trusted proxy without headers", trusted: []string{"10.0.0.0/8"}, remote: "10.0.0.2:5000",
			want: middleware.Client{IP: "10.0.0.2", Host: host}},
		{name: "ipv6 and mapped ipv4", trusted: []string{"::1"}, remote: "[::1]:5000",
			headers: map[string][]string{"X-Forwarded-For": {"::ffff:198.51.100.1"}},
			want:    middleware.Client{IP: "198.51.100.1", Host: host}},
		{name: "last proto wins", trusted: []string{"10.0.0.1"}, remote: "10.0.0.1:5000",
			headers: map[string][]string{"X-Forwarded-Proto": {"https, http"}},
			want:    middleware.Client{IP: "10.0.0.1", Host: host}},
		{name: "host without client", trusted: []string{"10.0.0.1"}, remote: "10.0.0.1:5000",
			headers: map[string][]string{"X-Forwarded-Host": {"a.example.com, www.example.com:8443"}},
			want:    middleware.Client{IP: "10.0.0.1", Host: "www.example.com:8443"}},
		{name: "direct tls", remote: "203.0.113.9:5000", tls: true,
			want: middleware.Client{IP: "203.0.113.9", Secure: true, Host: host}},
		// RFC 7239.
		{name: "forwarded: trusted proxy", trusted: []string{"10.0.0.0/8"}, family: middleware.HeadersForwarded, remote: "10.0.0.1:5000",
			headers: map[string][]string{"Forwarded": {`for=198.51.100.17;proto=https;host=www.example.com`}},
			want:    middleware.Client{IP: "198.51.100.17", Secure: true, Host: "www.example.com"}},
		{name: "forwarded: chain and quoted ipv6 with port", trusted: []string{"10.0.0.0/8"}, family: middleware.HeadersForwarded, remote: "10.0.0.1:5000",
			headers: map[string][]string{"Forwarded": {`for=192.0.2.43, for="[2001:db8:cafe::17]:4711";proto=http;host="cdn.example.com", for=10.0.0.5;proto=https`}},
			want:    middleware.Client{IP: "2001:db8:cafe::17", Host: "cdn.example.com"}},
		{name: "forwarded: several headers, port of ipv4", trusted: []string{"10.0.0.0/8"}, family: middleware.HeadersForwarded, remote: "10.0.0.1:5000",
			headers: map[string][]string{"Forwarded": {`for=1.2.3.4`, `For="198.51.100.9:1234";Proto=HTTPS`}},
			want:    middleware.Client{IP: "198.51.100.9", Secure: true, Host: host}},
		{name: "forwarded: unknown node ends the chain", trusted: []string{"10.0.0.0/8"}, family: middleware.HeadersForwarded, remote: "10.0.0.1:5000",
			headers: map[string][]string{"Forwarded": {`for=1.2.3.4, for=unknown, for=10.0.0.3`}},
			want:    middleware.Client{IP: "10.0.0.3", Host: host}},
		{name: "forwarded: obfuscated node", trusted: []string{"10.0.0.0/8"}, family: middleware.HeadersForwarded, remote: "10.0.0.1:5000",
			headers: map[string][]string{"Forwarded": {`for=_hidden`}},
			want:    middleware.Client{IP: "10.0.0.1", Host: host}},
		{name: "forwarded: semicolon inside quotes", trusted: []string{"10.0.0.0/8"}, family: middleware.HeadersForwarded, remote: "10.0.0.1:5000",
			headers: map[string][]string{"Forwarded": {`host="a;b,c";for=198.51.100.2`}},
			want:    middleware.Client{IP: "198.51.100.2", Host: "a;b,c"}},
		{name: "forwarded: untrusted peer", trusted: []string{"10.0.0.0/8"}, family: middleware.HeadersForwarded, remote: "203.0.113.9:5000",
			headers: map[string][]string{"Forwarded": {`for=198.51.100.17;proto=https;host=evil.com`}},
			want:    middleware.Client{IP: "203.0.113.9", Host: host}},
		// Only the configured family is read: the other one is what the
		// client sent, forwarded as is by the proxy.
		{name: "x-forwarded family ignores a forged Forwarded", trusted: []string{"10.0.0.0/8"}, remote: "10.0.0.1:5000",
			headers: map[string][]string{"Forwarded": {`for=1.2.3.4;host=evil.com`}, "X-Forwarded-For": {"198.51.100.1"}},
			want:    middleware.Client{IP: "198.51.100.1", Host: host}},
		{name: "forwarded family ignores a forged X-Forwarded-For", trusted: []string{"10.0.0.0/8"}, family: middleware.HeadersForwarded, remote: "10.0.0.1:5000",
			headers: map[string][]string{"X-Forwarded-For": {"1.2.3.4"}, "X-Forwarded-Host": {"evil.com"}, "Forwarded": {"for=198.51.100.1"}},
			want:    middleware.Client{IP: "198.51.100.1", Host: host}},
		// PROXY protocol: the listener already substituted the source of
		// the header to the remote address.
		{name: "proxy protocol over tls of the proxy", trusted: []string{"10.0.0.0/8"}, remote: "198.51.100.30:40000",
			proxied: &middleware.ProxyConn{Proxy: "10.0.0.1:5000", Secure: true},
			headers: map[string][]string{"X-Forwarded-For": {"1.2.3.4"}},
			want:    middleware.Client{IP: "198.51.100.30", Secure: true, Host: host}},
		{name: "proxy protocol in clear", trusted: []string{"10.0.0.0/8"}, remote: "198.51.100.30:40000",
			proxied: &middleware.ProxyConn{Proxy: "10.0.0.1:5000"},
			want:    middleware.Client{IP: "198.51.100.30", Host: host}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mw, err := middleware.NewForwarded(middleware.ForwardedDeps{}, middleware.WithTrustedProxies(tt.trusted...), middleware.WithForwardedHeaders(tt.family))
			require.NoError(t, err)
			var got middleware.Client
			h := mw.Wrap(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = middleware.ClientOf(r) }))
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tt.remote
			if tt.tls {
				req.TLS = &tls.ConnectionState{}
			}
			if tt.proxied != nil {
				pc := *tt.proxied
				req = req.WithContext(middleware.WithProxyConn(req.Context(), func() (middleware.ProxyConn, bool) { return pc, true }))
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

func TestForwardedRejectsInvalidOptions(t *testing.T) {
	_, err := middleware.NewForwarded(middleware.ForwardedDeps{}, middleware.WithTrustedProxies("10.0.0.0/33"))
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = middleware.NewForwarded(middleware.ForwardedDeps{}, middleware.WithTrustedProxies("proxy.local"))
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = middleware.NewForwarded(middleware.ForwardedDeps{}, middleware.WithForwardedHeaders("both"))
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

func TestClientOfWithoutMiddleware(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.9:5000"
	req.Header.Set("X-Forwarded-For", "198.51.100.1")
	req.Header.Set("X-Forwarded-Host", "evil.com")
	assert.Equal(t, middleware.Client{IP: "203.0.113.9", Host: host}, middleware.ClientOf(req), "the peer, headers ignored")
	assert.Equal(t, host, middleware.HostOf(req))
}

func TestProxyConnFromContext(t *testing.T) {
	_, ok := middleware.ProxyConnFromContext(context.Background())
	assert.False(t, ok)
	ctx := middleware.WithProxyConn(context.Background(), func() (middleware.ProxyConn, bool) { return middleware.ProxyConn{}, false })
	_, ok = middleware.ProxyConnFromContext(ctx)
	assert.False(t, ok, "a connection without PROXY header")
	ctx = middleware.WithProxyConn(context.Background(), func() (middleware.ProxyConn, bool) {
		return middleware.ProxyConn{Proxy: "10.0.0.1:1", Secure: true}, true
	})
	pc, ok := middleware.ProxyConnFromContext(ctx)
	assert.True(t, ok)
	assert.Equal(t, middleware.ProxyConn{Proxy: "10.0.0.1:1", Secure: true}, pc)
}
