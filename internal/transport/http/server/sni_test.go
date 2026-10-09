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
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	tmock "github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
	"github.com/r3n3r0/nexo/internal/transport/http/server"
	srvmock "github.com/r3n3r0/nexo/internal/transport/http/server/mock"
)

// sniCertificate returns a certificate for names and its parsed leaf.
func sniCertificate(t *testing.T, names ...string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	leaf, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

// writePair writes a certificate and its key as PEM files.
func writePair(t *testing.T, c tls.Certificate) (string, string) {
	t.Helper()
	dir := t.TempDir()
	keyDER, err := x509.MarshalPKCS8PrivateKey(c.PrivateKey)
	require.NoError(t, err)
	certFile, keyFile := filepath.Join(dir, "public.crt"), filepath.Join(dir, "private.key")
	require.NoError(t, os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Certificate[0]}), 0o600))
	require.NoError(t, os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600))
	return certFile, keyFile
}

// TestCertificatesBySNI serves the certificate of a distribution by SNI
// and the static one otherwise (TDS-10), with the host routing innermost.
func TestCertificatesBySNI(t *testing.T) {
	static := sniCertificate(t, "s3.example.org")
	site := sniCertificate(t, "www.example.com")
	src := srvmock.NewCertificateSource(t)
	src.EXPECT().GetCertificate(tmock.Anything).RunAndReturn(func(h *tls.ClientHelloInfo) (*tls.Certificate, error) {
		if h.ServerName == "www.example.com" {
			return &site, nil
		}
		return nil, nil
	})
	certFile, keyFile := writePair(t, static)
	hosts := middleware.Func(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Host == "www.example.com" {
				_, _ = io.WriteString(w, "distribution")
				return
			}
			next.ServeHTTP(w, r)
		})
	})
	srv := server.New(server.Deps{Hosts: hosts, NotFound: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "routes")
	})}, server.WithAddr("127.0.0.1:0"), server.WithTLS(certFile, keyFile), server.WithCertificates(src))
	require.NoError(t, srv.Start(context.Background()))
	t.Cleanup(func() { _ = srv.Stop(context.Background()) })

	for name, want := range map[string]struct {
		cn   string
		body string
	}{
		"www.example.com": {cn: "www.example.com", body: "distribution"},
		"s3.example.org":  {cn: "s3.example.org", body: "routes"},
	} {
		t.Run(name, func(t *testing.T) {
			client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
				TLSClientConfig: &tls.Config{ServerName: name, InsecureSkipVerify: true}, //nolint:gosec // the test checks the certificate served
			}}
			req, err := http.NewRequest(http.MethodGet, "https://"+srv.Addr()+"/x", nil)
			require.NoError(t, err)
			req.Host = name
			resp, err := client.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			assert.Equal(t, want.body, string(body))
			require.NotNil(t, resp.TLS)
			assert.Equal(t, want.cn, resp.TLS.PeerCertificates[0].Subject.CommonName)
		})
	}

	// The TLS-ALPN-01 validation negotiates acme-tls/1.
	conn, err := tls.Dial("tcp", srv.Addr(), &tls.Config{ServerName: "www.example.com", NextProtos: []string{"acme-tls/1"}, InsecureSkipVerify: true}) //nolint:gosec // validator
	require.NoError(t, err)
	assert.Equal(t, "acme-tls/1", conn.ConnectionState().NegotiatedProtocol)
	_ = conn.Close()
}
