package server_test

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	proxyproto "github.com/pires/go-proxyproto"
	"github.com/pires/go-proxyproto/tlvparse"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
	"github.com/r3n3r0/nexo/internal/transport/http/server"
)

// echoServer starts a server answering the client of every request
// (middleware.ClientOf, behind the forwarded-client middleware).
func echoServer(t *testing.T, trusted []string, family middleware.ForwardedHeaders, opts ...server.Option) server.Service {
	t.Helper()
	routes := httpx.RoutesFunc(func(r chi.Router) {
		r.Get("/whoami", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(middleware.ClientOf(r))
		})
	})
	fwd, err := middleware.NewForwarded(middleware.ForwardedDeps{}, middleware.WithTrustedProxies(trusted...), middleware.WithForwardedHeaders(family))
	require.NoError(t, err)
	srv := server.New(server.Deps{Routes: []httpx.Routes{routes}, Middlewares: []middleware.Middleware{fwd}},
		append([]server.Option{server.WithAddr("127.0.0.1:0"), server.WithReadHeaderTimeout(2 * time.Second)}, opts...)...)
	require.NoError(t, srv.Start(context.Background()))
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })
	return srv
}

// rawRequest sends header (a PROXY protocol header, possibly empty) and a
// GET /whoami over a new TCP connection (wrapped in TLS when cfg is set)
// and returns the client the server saw.
func rawRequest(t *testing.T, addr string, header []byte, cfg *tls.Config, extra http.Header) middleware.Client {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
	if len(header) > 0 {
		_, err = conn.Write(header)
		require.NoError(t, err)
	}
	c := net.Conn(conn)
	if cfg != nil {
		tc := tls.Client(conn, cfg)
		require.NoError(t, tc.Handshake())
		c = tc
	}
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/whoami", nil)
	require.NoError(t, err)
	for k, vs := range extra {
		req.Header[k] = vs
	}
	require.NoError(t, req.Write(c))
	resp, err := http.ReadResponse(bufio.NewReader(c), req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out middleware.Client
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return out
}

// proxyHeader builds a PROXY protocol header from src to dst.
func proxyHeader(t *testing.T, version byte, src, dst string, tlvs ...proxyproto.TLV) []byte {
	t.Helper()
	srcAddr, err := net.ResolveTCPAddr("tcp", src)
	require.NoError(t, err)
	dstAddr, err := net.ResolveTCPAddr("tcp", dst)
	require.NoError(t, err)
	proto := proxyproto.TCPv4
	if srcAddr.IP.To4() == nil {
		proto = proxyproto.TCPv6
	}
	h := &proxyproto.Header{Version: version, Command: proxyproto.PROXY, TransportProtocol: proto, SourceAddr: srcAddr, DestinationAddr: dstAddr}
	if len(tlvs) > 0 {
		require.NoError(t, h.SetTLVs(tlvs))
	}
	data, err := h.Format()
	require.NoError(t, err)
	return data
}

// sslTLV is the PROXY v2 SSL information of a client connection over TLS.
func sslTLV(t *testing.T) proxyproto.TLV {
	t.Helper()
	tlv, err := tlvparse.PP2SSL{Client: tlvparse.PP2_BITFIELD_CLIENT_SSL, TLV: []proxyproto.TLV{
		{Type: proxyproto.PP2_SUBTYPE_SSL_VERSION, Value: []byte("TLSv1.3")},
	}}.Marshal()
	require.NoError(t, err)
	return tlv
}

// TestProxyProtocolFromTrustedProxies is TDS-08 for the PROXY protocol:
// v1 and v2 headers of a trusted proxy give the real client (and the TLS
// of the proxy), the headers of any other peer are ignored (spoofing), a
// trusted proxy may also connect without header.
func TestProxyProtocolFromTrustedProxies(t *testing.T) {
	trusted := echoServer(t, []string{"127.0.0.1"}, "", server.WithProxyProtocol("127.0.0.1/32"))
	addr := trusted.Addr()

	got := rawRequest(t, addr, proxyHeader(t, 1, "198.51.100.7:40000", addr), nil, nil)
	assert.Equal(t, "198.51.100.7", got.IP)
	assert.False(t, got.Secure)

	got = rawRequest(t, addr, proxyHeader(t, 2, "[2001:db8::7]:40000", "[::1]:9000"), nil, nil)
	assert.Equal(t, "2001:db8::7", got.IP)

	got = rawRequest(t, addr, proxyHeader(t, 2, "198.51.100.8:40000", addr, sslTLV(t)), nil, nil)
	assert.Equal(t, "198.51.100.8", got.IP)
	assert.True(t, got.Secure, "TLS terminated by the proxy (v2 SSL TLV)")

	got = rawRequest(t, addr, nil, nil, nil)
	assert.Equal(t, "127.0.0.1", got.IP, "a trusted proxy may connect without header")

	// The client behind the PROXY header is not a proxy: its forwarding
	// headers are ignored.
	got = rawRequest(t, addr, proxyHeader(t, 1, "198.51.100.9:40000", addr), nil,
		http.Header{"X-Forwarded-For": {"1.2.3.4"}, "X-Forwarded-Host": {"evil.com"}})
	assert.Equal(t, "198.51.100.9", got.IP)
	assert.NotEqual(t, "evil.com", got.Host)

	// Another server trusts another network: the header of this peer is
	// read and ignored, the request is served with the real address.
	untrusted := echoServer(t, []string{"10.0.0.0/8"}, "", server.WithProxyProtocol("10.0.0.0/8"))
	for _, version := range []byte{1, 2} {
		got = rawRequest(t, untrusted.Addr(), proxyHeader(t, version, "198.51.100.7:40000", untrusted.Addr(), sslTLV(t)), nil, nil)
		assert.Equal(t, "127.0.0.1", got.IP, "spoofed PROXY v%d header ignored", version)
		assert.False(t, got.Secure)
	}
}

// TestProxyProtocolBeforeTLS checks that the header precedes the TLS
// handshake of a TLS listener.
func TestProxyProtocolBeforeTLS(t *testing.T) {
	srv := echoServer(t, []string{"127.0.0.1"}, "", server.WithProxyProtocol("127.0.0.1"), server.WithTLSConfig(selfSigned(t)))
	got := rawRequest(t, srv.Addr(), proxyHeader(t, 2, "198.51.100.20:5555", srv.Addr()), &tls.Config{InsecureSkipVerify: true}, nil) //nolint:gosec // self-signed test certificate
	assert.Equal(t, "198.51.100.20", got.IP)
	assert.True(t, got.Secure)
}

// TestProxyProtocolDisabled checks that without the option a PROXY header
// is not accepted (it is not HTTP).
func TestProxyProtocolDisabled(t *testing.T) {
	srv := echoServer(t, []string{"127.0.0.1"}, "")
	conn, err := net.Dial("tcp", srv.Addr())
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Write(append(proxyHeader(t, 1, "198.51.100.7:1", srv.Addr()), []byte("GET /whoami HTTP/1.1\r\nHost: x\r\n\r\n")...))
	require.NoError(t, err)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestProxyProtocolInvalidProxy(t *testing.T) {
	srv := server.New(server.Deps{}, server.WithAddr("127.0.0.1:0"), server.WithProxyProtocol("not-a-cidr"))
	err := srv.Start(context.Background())
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

// TestForwardedHeadersBehindReverseProxies is TDS-08 for the forwarding
// headers: a Go reverse proxy (httputil.ReverseProxy) in front of the
// server forwards the client, the scheme and the host; a client that
// reaches the server directly cannot forge them; Forwarded (RFC 7239) is
// read when configured.
func TestForwardedHeadersBehindReverseProxies(t *testing.T) {
	backend := echoServer(t, []string{"127.0.0.1"}, "")
	target, err := url.Parse("http://" + backend.Addr())
	require.NoError(t, err)
	proxy := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) {
		r.SetURL(target)
		r.SetXForwarded()
	}}
	front := listenProxy(t, proxy)

	// Through the proxy: the client of the proxy and the host it received.
	req, err := http.NewRequest(http.MethodGet, "http://"+front+"/whoami", nil)
	require.NoError(t, err)
	req.Host = "cdn.example.com"
	req.Header.Set("X-Forwarded-For", "6.6.6.6") // forged by the client: left of the real one
	got := doJSON(t, req)
	assert.Equal(t, "127.0.0.1", got.IP, "the address the proxy saw, not the forged one")
	assert.Equal(t, "cdn.example.com", got.Host)
	assert.False(t, got.Secure)

	// Directly, from a peer that is not a proxy: the forged headers are
	// ignored.
	untrusted := echoServer(t, []string{"10.0.0.0/8"}, "")
	req, err = http.NewRequest(http.MethodGet, "http://"+untrusted.Addr()+"/whoami", nil)
	require.NoError(t, err)
	req.Header.Set("X-Forwarded-For", "6.6.6.6")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "evil.com")
	got = doJSON(t, req)
	assert.Equal(t, "127.0.0.1", got.IP)
	assert.False(t, got.Secure)
	assert.Equal(t, untrusted.Addr(), got.Host)

	// RFC 7239 from a proxy that writes Forwarded.
	fwdBackend := echoServer(t, []string{"127.0.0.1"}, middleware.HeadersForwarded)
	fwdTarget, err := url.Parse("http://" + fwdBackend.Addr())
	require.NoError(t, err)
	fwdProxy := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) {
		r.SetURL(fwdTarget)
		r.Out.Header.Set("Forwarded", `for="[2001:db8::42]:1234";proto=https;host=www.example.com`)
		r.Out.Header.Set("X-Forwarded-For", "6.6.6.6")
	}}
	front = listenProxy(t, fwdProxy)
	req, err = http.NewRequest(http.MethodGet, "http://"+front+"/whoami", nil)
	require.NoError(t, err)
	got = doJSON(t, req)
	assert.Equal(t, middleware.Client{IP: "2001:db8::42", Secure: true, Host: "www.example.com"}, got)
}

// listenProxy serves a reverse proxy on a free port of 127.0.0.1.
func listenProxy(t *testing.T, h http.Handler) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 2 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

func doJSON(t *testing.T, req *http.Request) middleware.Client {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out middleware.Client
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
	return out
}

// selfSigned returns a TLS configuration with a self-signed certificate.
func selfSigned(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12}
}
