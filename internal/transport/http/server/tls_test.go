package server_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	tmock "github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/service/certs"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
	"github.com/r3n3r0/nexo/internal/transport/http/server"
	srvmock "github.com/r3n3r0/nexo/internal/transport/http/server/mock"
)

// tlsCA issues the certificates of the TLS tests.
type tlsCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newTLSCA(t *testing.T, name string) *tlsCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return &tlsCA{cert: cert, key: key}
}

// pool returns a pool trusting the CA.
func (ca *tlsCA) pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca.cert)
	return p
}

// issue returns a server and client certificate of the CA for names
// (and 127.0.0.1) as PEM chain and key, and the parsed key pair.
func (ca *tlsCA) issue(t *testing.T, names ...string) (string, string, tls.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:   time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	chain := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	pair, err := tls.X509KeyPair([]byte(chain), []byte(keyPEM))
	require.NoError(t, err)
	return chain, keyPEM, pair
}

// writeDirPair writes a key pair into dir (created), the key first: a
// reload between the two files fails and keeps the previous pair.
func writeDirPair(t *testing.T, dir, chain, key string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "private.key"), []byte(key), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "public.crt"), []byte(chain), 0o600))
}

// startTLS starts a server answering /ping and streaming /stream.
func startTLS(t *testing.T, opts ...server.Option) server.Service {
	t.Helper()
	routes := httpx.RoutesFunc(func(r chi.Router) {
		r.Get("/ping", func(w http.ResponseWriter, r *http.Request) {
			if r.TLS != nil && len(r.TLS.VerifiedChains) > 0 {
				w.Header().Set("X-Client-Verified", r.TLS.VerifiedChains[0][0].Subject.CommonName)
			}
			_, _ = w.Write([]byte("pong"))
		})
		r.Get("/stream", func(w http.ResponseWriter, r *http.Request) {
			for i := 0; i < 30; i++ {
				_, _ = w.Write([]byte("chunk\n"))
				http.NewResponseController(w).Flush()
				time.Sleep(10 * time.Millisecond)
			}
		})
	})
	srv := server.New(server.Deps{Routes: []httpx.Routes{routes}, Logger: slog.New(slog.DiscardHandler)},
		append([]server.Option{server.WithAddr("127.0.0.1:0"), server.WithShutdownTimeout(2 * time.Second)}, opts...)...)
	require.NoError(t, srv.Start(context.Background()))
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })
	return srv
}

// tlsClient returns an HTTP client trusting roots, asking for serverName.
func tlsClient(roots *x509.CertPool, serverName string, h2 bool) *http.Client {
	tr := &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: roots, ServerName: serverName, MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2: h2,
	}
	return &http.Client{Transport: tr, Timeout: 10 * time.Second}
}

// peerCert returns the certificate the server presented for a request.
func peerCert(t *testing.T, c *http.Client, url string) *x509.Certificate {
	t.Helper()
	resp, err := c.Get(url)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	return resp.TLS.PeerCertificates[0]
}

// TestKeystoreRotationWithoutInterruption rotates the certificate of a
// listener at runtime (S-OPS-02, docs/07 §7.3): the next handshakes get
// the new certificate, the established connections (HTTP/1.1 and HTTP/2)
// keep serving, a response in progress completes.
//
// covers: S-OPS-02
func TestKeystoreRotationWithoutInterruption(t *testing.T) {
	ca := newTLSCA(t, "CA")
	dir := t.TempDir()
	oldChain, oldKey, oldPair := ca.issue(t, "s3.example.org")
	writeDirPair(t, dir, oldChain, oldKey)
	ks, err := certs.NewKeystore(certs.KeystoreDeps{Logger: slog.New(slog.DiscardHandler)}, dir,
		certs.WithKeystoreDebounce(20*time.Millisecond), certs.WithKeystorePoll(-1))
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, ks.Start(ctx))
	t.Cleanup(func() { _ = ks.Stop(ctx) })

	srv := startTLS(t, server.WithKeystore(ks))
	base := "https://" + srv.Addr()
	h1, h2 := tlsClient(ca.pool(), "s3.example.org", false), tlsClient(ca.pool(), "s3.example.org", true)
	for _, c := range []*http.Client{h1, h2} {
		assert.Equal(t, oldPair.Leaf.Raw, peerCert(t, c, base+"/ping").Raw)
	}

	// A download in progress across the rotation.
	streamed := make(chan error, 1)
	go func() {
		resp, err := tlsClient(ca.pool(), "s3.example.org", false).Get(base + "/stream")
		if err != nil {
			streamed <- err
			return
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err == nil && len(body) != 30*len("chunk\n") {
			err = io.ErrUnexpectedEOF
		}
		streamed <- err
	}()

	newChain, newKey, newPair := ca.issue(t, "s3.example.org")
	writeDirPair(t, dir, newChain, newKey)
	fresh := func() *http.Client { return tlsClient(ca.pool(), "s3.example.org", false) }
	require.Eventually(t, func() bool {
		return string(peerCert(t, fresh(), base+"/ping").Raw) == string(newPair.Leaf.Raw)
	}, 5*time.Second, 20*time.Millisecond, "a new connection gets the rotated certificate")

	// The connections established before the rotation are still open:
	// their requests reuse them (and their certificate).
	for name, c := range map[string]*http.Client{"HTTP/1.1": h1, "HTTP/2": h2} {
		assert.Equal(t, oldPair.Leaf.Raw, peerCert(t, c, base+"/ping").Raw, "%s connection kept", name)
	}
	require.NoError(t, <-streamed, "the response in progress completes")
}

// TestTLSPolicy checks the versions and cipher suites the listeners
// accept.
//
// covers: S-OPS-02
func TestTLSPolicy(t *testing.T) {
	ca := newTLSCA(t, "CA")
	_, _, pair := ca.issue(t, "s3.example.org")
	src := srvmock.NewCertificateSource(t)
	src.EXPECT().GetCertificate(tmock.Anything).Return(&pair, nil).Maybe()

	tests := []struct {
		name    string
		opts    []server.Option
		client  *tls.Config
		ok      bool
		version uint16
	}{
		{name: "TLS 1.3 by default", client: &tls.Config{}, ok: true, version: tls.VersionTLS13},
		{name: "TLS 1.2 with an AEAD suite", client: &tls.Config{MaxVersion: tls.VersionTLS12}, ok: true, version: tls.VersionTLS12},
		{name: "TLS 1.1 refused", client: &tls.Config{MinVersion: tls.VersionTLS11, MaxVersion: tls.VersionTLS11}}, //nolint:gosec // G402: the test checks the refusal
		{name: "CBC suite refused", client: &tls.Config{MaxVersion: tls.VersionTLS12, CipherSuites: []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA}}},
		{name: "TLS 1.2 refused with a 1.3 minimum", opts: []server.Option{server.WithMinTLSVersion(tls.VersionTLS13)},
			client: &tls.Config{MaxVersion: tls.VersionTLS12}},
		{name: "TLS 1.3 with a 1.3 minimum", opts: []server.Option{server.WithMinTLSVersion(tls.VersionTLS13)},
			client: &tls.Config{}, ok: true, version: tls.VersionTLS13},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := startTLS(t, append([]server.Option{server.WithKeystore(src)}, tt.opts...)...)
			cfg := tt.client.Clone()
			cfg.RootCAs, cfg.ServerName = ca.pool(), "s3.example.org"
			conn, err := tls.Dial("tcp", srv.Addr(), cfg)
			if !tt.ok {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			defer func() { _ = conn.Close() }()
			assert.Equal(t, tt.version, conn.ConnectionState().Version)
		})
	}
}

// TestClientCAsReload verifies the client certificates with the current
// CAs of the source: a CA added by a reload is trusted from the next
// handshake on.
func TestClientCAsReload(t *testing.T) {
	serverCA, nodeCA := newTLSCA(t, "server CA"), newTLSCA(t, "node CA")
	_, _, serverPair := serverCA.issue(t, "node1")
	_, _, clientPair := nodeCA.issue(t, "node2")
	src := srvmock.NewCertificateSource(t)
	src.EXPECT().GetCertificate(tmock.Anything).Return(&serverPair, nil).Maybe()
	var pool atomic.Pointer[x509.CertPool]
	pool.Store(serverCA.pool())
	cas := srvmock.NewClientCASource(t)
	cas.EXPECT().ClientCAs().RunAndReturn(pool.Load)

	srv := startTLS(t, server.WithKeystore(src), server.WithClientCASource(cas))
	call := func(withCert bool) (*http.Response, error) {
		cfg := &tls.Config{RootCAs: serverCA.pool(), ServerName: "node1", MinVersion: tls.VersionTLS12}
		if withCert {
			// Presented whatever CAs the server names, like the nodes do
			// (certs.Keystore.GetClientCertificate).
			cfg.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &clientPair, nil }
		}
		c := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}, Timeout: 5 * time.Second}
		resp, err := c.Get("https://" + srv.Addr() + "/ping")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		return resp, err
	}

	resp, err := call(false)
	require.NoError(t, err, "a client without certificate (S3) is served")
	assert.Empty(t, resp.Header.Get("X-Client-Verified"))
	_, err = call(true)
	require.Error(t, err, "a certificate of an unknown CA is refused")

	both := serverCA.pool()
	both.AddCert(nodeCA.cert)
	pool.Store(both)
	resp, err = call(true)
	require.NoError(t, err, "the reloaded CAs verify the certificate")
	assert.Equal(t, "node2", resp.Header.Get("X-Client-Verified"))
}

// TestKeystoreBehindDistributionCertificates chains the certificates: the
// distributions first for the names they cover, then the certificates
// of the server by SNI, then its default one.
func TestKeystoreBehindDistributionCertificates(t *testing.T) {
	ca := newTLSCA(t, "CA")
	dir := t.TempDir()
	defChain, defKey, defPair := ca.issue(t, "s3.example.org")
	writeDirPair(t, dir, defChain, defKey)
	sniChain, sniKey, sniPair := ca.issue(t, "console.example.org")
	writeDirPair(t, filepath.Join(dir, "console"), sniChain, sniKey)
	ks, err := certs.NewKeystore(certs.KeystoreDeps{Logger: slog.New(slog.DiscardHandler)}, dir)
	require.NoError(t, err)
	_, _, sitePair := ca.issue(t, "www.example.com")
	dist := srvmock.NewCertificateSource(t)
	dist.EXPECT().GetCertificate(tmock.Anything).RunAndReturn(func(h *tls.ClientHelloInfo) (*tls.Certificate, error) {
		if h.ServerName == "www.example.com" {
			return &sitePair, nil
		}
		return nil, nil
	})
	srv := startTLS(t, server.WithCertificates(dist), server.WithKeystore(ks))

	for name, want := range map[string]tls.Certificate{
		"www.example.com": sitePair, "console.example.org": sniPair, "s3.example.org": defPair, "other.example.org": defPair,
	} {
		conn, err := tls.Dial("tcp", srv.Addr(), &tls.Config{RootCAs: ca.pool(), ServerName: name, MinVersion: tls.VersionTLS12,
			InsecureSkipVerify: name == "other.example.org"}) //nolint:gosec // G402: the default certificate does not name it
		require.NoError(t, err, name)
		assert.Equal(t, want.Leaf.Raw, conn.ConnectionState().PeerCertificates[0].Raw, name)
		_ = conn.Close()
	}
}
