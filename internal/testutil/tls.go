package testutil

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// WithTLS serves the deployment over TLS (and its internode traffic, in a
// cluster: https endpoints): a test CA issues the certificate of
// 127.0.0.1, localhost and *.localhost (the virtual-host domain), written
// in a certificates directory (public.crt, private.key, CAs/ca.crt) that
// configures the nodes (CertsDir). The Server URLs are https URLs,
// Server.Client and Server.TLS trust the CA.
func WithTLS() Option {
	return func(s *settings) { s.tls = true }
}

// testPKI is the CA and the certificate of a TLS test deployment.
type testPKI struct {
	dir string
	// server is the configuration of a listener serving the certificate
	// (the load balancers), client the one of a client trusting the CA.
	server *tls.Config
	client *tls.Config
}

// newTestPKI creates a CA and a server certificate in a fresh directory.
func newTestPKI(t testing.TB) testPKI {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "certs")
	if err := os.MkdirAll(filepath.Join(dir, "CAs"), 0o700); err != nil {
		t.Fatalf("testutil: certs directory: %v", err)
	}
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("testutil: CA key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Nexo test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("testutil: CA certificate: %v", err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("testutil: CA certificate: %v", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("testutil: server key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "127.0.0.1"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.IPv6loopback},
		DNSNames:    []string{"localhost", "*." + Domain},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatalf("testutil: server certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("testutil: server key: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	for name, data := range map[string][]byte{"public.crt": certPEM, "private.key": keyPEM, filepath.Join("CAs", "ca.crt"): caPEM} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatalf("testutil: write %s: %v", name, err)
		}
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("testutil: server key pair: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return testPKI{
		dir:    dir,
		server: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		client: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
	}
}

// LBTLS serves the load balancer over TLS with server and reaches the
// targets over TLS with client.
func LBTLS(server, client *tls.Config) LBOption {
	return func(s *lbSettings) { s.serverTLS, s.clientTLS = server, client }
}

// tlsTransport returns an HTTP transport trusting the CA of cfg.
func tlsTransport(cfg *tls.Config) *http.Transport {
	tr, _ := http.DefaultTransport.(*http.Transport)
	out := tr.Clone()
	out.TLSClientConfig = cfg.Clone()
	return out
}
