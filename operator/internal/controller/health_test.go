package controller

import (
	"context"
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
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	nexov1 "github.com/r3n3r0/nexo/operator/api/v1alpha1"
)

// TestHTTPHealth (S-OPS-16): the report of /nexo/health/cluster is read
// through the S3 Service, 503 included; with TLS the certificate must be
// signed by the CA of the TLS Secret.
//
// covers: S-OPS-16
func TestHTTPHealth(t *testing.T) {
	report := `{"ok":false,"status":"maintenance not possible","nodes":4,"nodesOnline":4,"drives":8,"drivesOnline":7,"writeQuorum":true,"maintenance":false}`
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/nexo/health/cluster" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(report))
	})
	c := testCluster()
	c.Spec.TLS = &nexov1.TLSSpec{SecretName: "certs"}
	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)
	// The service host name resolves to the test server.
	dial := func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, srv.Listener.Addr().String())
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})

	// The test certificate names example.com, not the Service: refused.
	_, err := HTTPHealth{DialContext: dial}.Check(context.Background(), c, &corev1.Secret{Data: map[string][]byte{caKey: certPEM}})
	require.Error(t, err)

	// A certificate of a private CA for the S3 Service: accepted.
	caPEM, leaf := issue(t, "lake.storage.svc.cluster.local")
	signed := httptest.NewUnstartedServer(handler)
	signed.TLS = &tls.Config{Certificates: []tls.Certificate{leaf}, MinVersion: tls.VersionTLS12}
	signed.StartTLS()
	t.Cleanup(signed.Close)
	h, err := HTTPHealth{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, signed.Listener.Addr().String())
	}}.Check(context.Background(), c, &corev1.Secret{Data: map[string][]byte{caKey: caPEM}})
	require.NoError(t, err)
	assert.Equal(t, 7, h.DrivesOnline)

	// Plain HTTP: the report, 503 included.
	plain := httptest.NewServer(handler)
	t.Cleanup(plain.Close)
	c.Spec.TLS = nil
	h, err = HTTPHealth{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, plain.Listener.Addr().String())
	}}.Check(context.Background(), c, nil)
	require.NoError(t, err)
	assert.Equal(t, Health{Nodes: 4, NodesOnline: 4, Drives: 8, DrivesOnline: 7, WriteQuorum: true}, h)
	assert.False(t, h.Stoppable())
}

// issue returns the PEM of a new CA and a certificate it signs for host.
func issue(t *testing.T, host string) ([]byte, tls.Certificate) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	caTpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	caDER, err := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	require.NoError(t, err)
	ca, err := x509.ParseCertificate(caDER)
	require.NoError(t, err)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca, &key.PublicKey, caKey)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
