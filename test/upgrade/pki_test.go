//go:build upgrade

package upgrade_test

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

	"github.com/stretchr/testify/require"
)

// pki is the TLS of a deployment: a test CA and the certificate of
// 127.0.0.1 it issued, in a certificates directory (public.crt,
// private.key, CAs/ca.crt) both releases read. The deployments run over
// TLS, as in production: SSE-C needs it.
type pki struct {
	dir string
	// server serves the certificate (the load balancer), client trusts
	// the CA.
	server *tls.Config
	client *tls.Config
}

// newPKI creates the CA and the certificate in dir.
func newPKI(t *testing.T, dir string) pki {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "CAs"), 0o700))
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Nexo upgrade test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	require.NoError(t, err)
	ca, err := x509.ParseCertificate(caDER)
	require.NoError(t, err)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "127.0.0.1"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:    []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	for name, data := range map[string][]byte{"public.crt": certPEM, "private.key": keyPEM, filepath.Join("CAs", "ca.crt"): caPEM} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), data, 0o600))
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	require.NoError(t, err)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return pki{
		dir:    dir,
		server: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		client: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
	}
}

// httpClient returns an HTTP client trusting the CA.
func (p pki) httpClient(timeout time.Duration) *http.Client {
	tr, _ := http.DefaultTransport.(*http.Transport)
	tr = tr.Clone()
	tr.TLSClientConfig = p.client.Clone()
	tr.Proxy = nil
	return &http.Client{Timeout: timeout, Transport: tr}
}
