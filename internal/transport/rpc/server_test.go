package rpc_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/metrics"
	"github.com/r3n3r0/nexo/internal/transport/rpc"
)

// syncBuffer is a log sink safe for concurrent writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// h2Client returns a cleartext HTTP/2 client (prior knowledge, as the
// internode RPC dials).
func h2Client() *http.Client {
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	return &http.Client{Transport: &http.Transport{Protocols: &protocols}}
}

// h2cServer returns an unstarted test server that serves handler with
// HTTP/1 and cleartext HTTP/2, with the HTTP/2 settings of config (nil:
// the defaults).
func h2cServer(handler http.Handler, config *http.HTTP2Config) *httptest.Server {
	srv := httptest.NewUnstartedServer(handler)
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	srv.Config.Protocols = &protocols
	srv.Config.HTTP2 = config
	return srv
}

// TestServerRefusalsAreLoggedCountedAndThrottled: authentication failures
// are logged as warnings (once per remote address and window, never the
// token), counted by reason, and a remote address that keeps failing is
// refused without verification until its allowance refills.
func TestServerRefusalsAreLoggedCountedAndThrottled(t *testing.T) {
	ctx := context.Background()
	logs := &syncBuffer{}
	registry := metrics.New(metrics.Deps{Logger: discard()})
	ln := listen(t)
	addr := ln.Addr().String()
	s, err := rpc.NewServer(rpc.ServerDeps{Auth: newAuth(t, addr), Metrics: registry, Logger: slog.New(slog.NewTextHandler(logs, nil))},
		rpc.WithAuthFailureLimit(3, time.Hour))
	require.NoError(t, err)
	srv := serve(t, ln, s)
	ping := func(token string) int {
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/nexo/rpc/v1/health/ping", strings.NewReader(""))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := h2Client().Do(req)
		require.NoError(t, err)
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	for range 3 {
		assert.Equal(t, http.StatusUnauthorized, ping("forged.secret-token.value"))
	}
	assert.Equal(t, http.StatusTooManyRequests, ping("forged.secret-token.value"), "the address is throttled")
	// A valid caller from the same address waits for the allowance too.
	conn := dial(t, newAuth(t, "client:9000"), addr)
	err = conn.Call(ctx, "health/ping", struct{}{}, nil)
	require.ErrorIs(t, err, domain.ErrTooManyRequests)

	out := logs.String()
	assert.Equal(t, 1, strings.Count(out, "reason=invalid_token"), "one warning per address and reason per window:\n%s", out)
	assert.Contains(t, out, "level=WARN")
	assert.Contains(t, out, "remote=127.0.0.1")
	assert.NotContains(t, out, "secret-token", "the token is never logged")

	rec := httptest.NewRecorder()
	registry.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	assert.Contains(t, rec.Body.String(), `nexo_rpc_auth_refused_total{reason="invalid_token"} 3`)
	assert.Contains(t, rec.Body.String(), `nexo_rpc_auth_refused_total{reason="throttled"} 2`)
}

// TestServerWrapSkipsVirtualHosts: a virtual-host S3 request whose object
// key starts with the RPC prefix goes to the S3 API, not to the RPC
// server.
func TestServerWrapSkipsVirtualHosts(t *testing.T) {
	s, err := rpc.NewServer(rpc.ServerDeps{Auth: newAuth(t, "n:1"), Logger: discard()},
		rpc.WithSkip(func(r *http.Request) bool { return strings.HasPrefix(r.Host, "bucket.") }))
	require.NoError(t, err)
	h := s.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "s3 api") }))
	for name, tc := range map[string]struct {
		host string
		rpc  bool
	}{
		"virtual host": {host: "bucket.s3.test:9000"},
		"path style":   {host: "s3.test:9000", rpc: true},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+tc.host+"/nexo/rpc/v1/report.txt", nil))
		if tc.rpc {
			assert.NotEqual(t, "s3 api", rec.Body.String(), name)
		} else {
			assert.Equal(t, "s3 api", rec.Body.String(), name)
		}
	}
}

// TestConnBoundsReplies: the answer of a unary call over the bound of the
// connection is refused instead of being buffered.
func TestConnBoundsReplies(t *testing.T) {
	ctx := context.Background()
	ln := listen(t)
	addr := ln.Addr().String()
	big := bytes.Repeat([]byte("x"), 4096)
	reply, err := msgpack.Marshal(big)
	require.NoError(t, err)
	srv := h2cServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/x-msgpack")
		_, _ = w.Write(reply)
	}), nil)
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	var out []byte
	err = dial(t, newAuth(t, "client:9000"), addr, rpc.WithMaxReply(1024)).Call(ctx, "storage/read-all", struct{}{}, &out)
	require.ErrorIs(t, err, rpc.ErrProtocol)
	require.NoError(t, dial(t, newAuth(t, "client:9000"), addr).Call(ctx, "storage/read-all", struct{}{}, &out))
	assert.Equal(t, big, out)
}

// testPKI is a CA and a certificate it signed for 127.0.0.1, usable as a
// server and as a client certificate.
type testPKI struct {
	pool   *x509.CertPool
	cert   tls.Certificate
	ca     *x509.Certificate
	caKey  *ecdsa.PrivateKey
	serial int64
}

// issue returns a certificate of the CA for the DNS name, usable as a
// client certificate.
func (p *testPKI) issue(t *testing.T, name string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	p.serial++
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(100 + p.serial), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.ca, &key.PublicKey, p.caKey)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func newTestPKI(t *testing.T) testPKI {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "nexo test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	require.NoError(t, err)
	ca, err := x509.ParseCertificate(caDER)
	require.NoError(t, err)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "nexo node"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	require.NoError(t, err)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return testPKI{pool: pool, cert: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, ca: ca, caKey: caKey}
}

// TestServerMutualTLS: with WithClientCertRequired the internode calls
// need a client certificate the listener verified; a caller without one
// is refused even with a valid token.
func TestServerMutualTLS(t *testing.T) {
	ctx := context.Background()
	pki := newTestPKI(t)
	ln := listen(t)
	addr := ln.Addr().String()
	s, err := rpc.NewServer(rpc.ServerDeps{Auth: newAuth(t, addr), Logger: discard()}, rpc.WithClientCertRequired())
	require.NoError(t, err)
	srv := httptest.NewUnstartedServer(s)
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.EnableHTTP2 = true
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pki.cert}, ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: pki.pool}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	withCert := &tls.Config{RootCAs: pki.pool, Certificates: []tls.Certificate{pki.cert}, MinVersion: tls.VersionTLS12}
	require.NoError(t, dial(t, newAuth(t, "client:9000"), addr, rpc.WithTLSConfig(withCert)).Call(ctx, "health/ping", struct{}{}, nil))
	withoutCert := &tls.Config{RootCAs: pki.pool, MinVersion: tls.VersionTLS12}
	err = dial(t, newAuth(t, "client:9000"), addr, rpc.WithTLSConfig(withoutCert)).Call(ctx, "health/ping", struct{}{}, nil)
	require.ErrorIs(t, err, domain.ErrUnauthenticated)
}

// TestServerMutualTLSBindsTheNodes: with WithClientCertHosts a client
// certificate of a trusted CA must name a node of the deployment
// (regression: any certificate of the CA, issued for another service,
// passed the mutual TLS).
func TestServerMutualTLSBindsTheNodes(t *testing.T) {
	ctx := context.Background()
	pki := newTestPKI(t)
	ln := listen(t)
	addr := ln.Addr().String()
	s, err := rpc.NewServer(rpc.ServerDeps{Auth: newAuth(t, addr), Logger: discard()},
		rpc.WithClientCertRequired(), rpc.WithClientCertHosts("node1.cluster.test", "127.0.0.1"))
	require.NoError(t, err)
	srv := httptest.NewUnstartedServer(s)
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.EnableHTTP2 = true
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pki.cert}, ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: pki.pool}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	node := &tls.Config{RootCAs: pki.pool, Certificates: []tls.Certificate{pki.cert}, MinVersion: tls.VersionTLS12}
	require.NoError(t, dial(t, newAuth(t, "client:9000"), addr, rpc.WithTLSConfig(node)).Call(ctx, "health/ping", struct{}{}, nil))

	other := pki.issue(t, "evil-unrelated-service")
	stranger := &tls.Config{RootCAs: pki.pool, Certificates: []tls.Certificate{other}, MinVersion: tls.VersionTLS12}
	err = dial(t, newAuth(t, "client:9000"), addr, rpc.WithTLSConfig(stranger)).Call(ctx, "health/ping", struct{}{}, nil)
	require.ErrorIs(t, err, domain.ErrUnauthenticated)
}
