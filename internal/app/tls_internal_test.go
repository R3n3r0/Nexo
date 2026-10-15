package app

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
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/config"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/certs"
)

// testIssuer issues the certificates of the TLS tests of the application.
type testIssuer struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newTestIssuer(t *testing.T) *testIssuer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return &testIssuer{cert: cert, key: key}
}

// write issues a certificate for names and 127.0.0.1 into dir and returns
// its DER.
func (i *testIssuer) write(t *testing.T, dir string, names ...string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:   time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, i.cert, &key.PublicKey, i.key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, certs.PrivateKeyFile), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, certs.PublicCertFile), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	return der
}

// TestRunReloadsTLSOnSIGHUP serves S3 and the console over TLS from a
// certificates directory, rotates the certificates and sends SIGHUP: the
// listeners present the new certificates from the next handshake on, the
// open connections keep working and the application is not restarted
// (S-OPS-02, docs/07 §7.3). The file notifications are disabled: only
// the signal reloads.
func TestRunReloadsTLSOnSIGHUP(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGHUP is a Unix signal")
	}
	ca := newTestIssuer(t)
	certsDir := filepath.Join(t.TempDir(), "certs")
	oldDefault := ca.write(t, certsDir, "localhost")
	oldConsole := ca.write(t, filepath.Join(certsDir, "console"), "console.example.test")

	cfg := config.Default()
	cfg.RootUser, cfg.RootPassword = "admin", "password123"
	cfg.Endpoints = []string{filepath.Join(t.TempDir(), "data")}
	cfg.Address, cfg.ConsoleAddress = freeAddr(t), freeAddr(t)
	cfg.CertsDir = certsDir
	onlySignals := func(o *options) {
		o.keystoreOpts = []certs.KeystoreOption{certs.WithKeystoreDebounce(time.Hour), certs.WithKeystorePoll(-1)}
	}
	a, err := Build(context.Background(), cfg, WithLogger(slog.New(slog.DiscardHandler)), WithStopTimeout(5*time.Second), onlySignals)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	require.Eventually(t, a.running.Load, 10*time.Second, 10*time.Millisecond)

	roots := x509.NewCertPool()
	roots.AddCert(ca.cert)
	client := func(serverName string) *http.Client {
		return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: serverName, MinVersion: tls.VersionTLS12},
		}}
	}
	served := func(c *http.Client, addr string) []byte {
		resp, err := c.Get("https://" + addr + domain.HealthLivePath)
		if err != nil {
			return nil
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(io.Discard, resp.Body)
		if resp.StatusCode != http.StatusOK {
			return nil
		}
		return resp.TLS.PeerCertificates[0].Raw
	}
	kept := client("localhost")
	require.Eventually(t, func() bool { return served(kept, cfg.ConsoleAddress) != nil }, 10*time.Second, 20*time.Millisecond)
	assert.Equal(t, oldDefault, served(kept, cfg.ConsoleAddress))
	assert.Equal(t, oldConsole, served(client("console.example.test"), cfg.ConsoleAddress), "the SNI certificate of a sub-directory")

	newDefault := ca.write(t, certsDir, "localhost")
	newConsole := ca.write(t, filepath.Join(certsDir, "console"), "console.example.test")
	assert.Equal(t, oldDefault, served(client("localhost"), cfg.ConsoleAddress), "no reload before the signal")

	require.NoError(t, signalSelf(syscall.SIGHUP))
	require.Eventually(t, func() bool {
		return string(served(client("localhost"), cfg.ConsoleAddress)) == string(newDefault)
	}, 10*time.Second, 20*time.Millisecond, "the console presents the new certificate")
	assert.Equal(t, newDefault, served(client("localhost"), cfg.Address), "and so does the S3 listener")
	assert.Equal(t, newConsole, served(client("console.example.test"), cfg.ConsoleAddress))
	assert.Equal(t, oldDefault, served(kept, cfg.ConsoleAddress), "the open connection is kept")
	assert.True(t, a.running.Load(), "the application was not restarted")

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return")
	}
}
