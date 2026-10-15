package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testCert is a certificate with its key, as PEM and as a tls.Certificate.
type testCert struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
	tls  tls.Certificate
}

// newTestCert issues a certificate for dnsNames, signed by parent (nil:
// self-signed); isCA makes it a CA.
func newTestCert(t *testing.T, cn string, dnsNames []string, isCA bool, parent *testCert) *testCert {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: dnsNames, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if isCA {
		tmpl.IsCA, tmpl.BasicConstraintsValid = true, true
		tmpl.KeyUsage |= x509.KeyUsageCertSign
		tmpl.ExtKeyUsage = nil
	}
	signer, signerKey := tmpl, key
	if parent != nil {
		signer, signerKey = parent.cert, parent.key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signer, &key.PublicKey, signerKey)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return &testCert{
		cert: cert, key: key,
		pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		tls: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: cert},
	}
}

// healthServer serves 200 on the health endpoints, over TLS with cert
// (plain HTTP when nil).
func healthServer(t *testing.T, cert *testCert) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/nexo/health/live", "/nexo/health/ready":
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	// The failed handshakes of the negative cases are expected.
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	if cert == nil {
		srv.Start()
	} else {
		srv.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert.tls}}
		srv.StartTLS()
	}
	t.Cleanup(srv.Close)
	return srv
}

// writeCertsDir writes public.crt and the CAs of a certificates directory.
func writeCertsDir(t *testing.T, public *testCert, cas ...*testCert) string {
	t.Helper()
	dir := t.TempDir()
	if public != nil {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "public.crt"), public.pem, 0o600))
	}
	if len(cas) > 0 {
		require.NoError(t, os.Mkdir(filepath.Join(dir, "CAs"), 0o700))
		for i, ca := range cas {
			name := filepath.Join(dir, "CAs", "ca"+string(rune('0'+i))+".crt")
			require.NoError(t, os.WriteFile(name, ca.pem, 0o600))
		}
		// Hidden entries and subdirectories (e.g. the ..data symbolic
		// links of a Kubernetes secret) are skipped.
		require.NoError(t, os.Mkdir(filepath.Join(dir, "CAs", "sub"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "CAs", ".hidden"), []byte("not a certificate"), 0o600))
	}
	return dir
}

// TestHealthcheckTLS: with a certificates directory (NEXO_CERTS_DIR or
// --certs-dir) the probe of the local console uses HTTPS and trusts the
// certificate of the server and its CAs, verifying the chain but not the
// host name; an explicit https --url is verified in full.
//
// covers: S-OPS-13
func TestHealthcheckTLS(t *testing.T) {
	ca := newTestCert(t, "nexo test CA", nil, true, nil)
	otherCA := newTestCert(t, "other CA", nil, true, nil)
	// Node certificates naming the node host names only, never 127.0.0.1.
	leaf := newTestCert(t, "nexo1", []string{"nexo1", "s3.example.com"}, false, ca)
	selfSigned := newTestCert(t, "nexo1", []string{"nexo1"}, false, nil)
	localhost := newTestCert(t, "localhost", []string{"localhost"}, false, ca)
	untrusted := newTestCert(t, "nexo1", []string{"nexo1"}, false, otherCA)

	tests := []struct {
		name    string
		server  *testCert // nil: plain HTTP
		dir     func(t *testing.T) string
		viaFlag bool // --certs-dir instead of NEXO_CERTS_DIR
		args    func(srv *httptest.Server) []string
		wantErr string
		wantURL string // prefix of the probed URL
	}{
		{
			name: "CA-signed certificate, CA in CAs/", server: leaf,
			dir:     func(t *testing.T) string { return writeCertsDir(t, leaf, ca) },
			wantURL: "https://127.0.0.1:",
		},
		{
			name: "certificate of the server trusted as itself", server: leaf,
			dir:     func(t *testing.T) string { return writeCertsDir(t, leaf) },
			wantURL: "https://127.0.0.1:",
		},
		{
			name: "self-signed certificate", server: selfSigned,
			dir:     func(t *testing.T) string { return writeCertsDir(t, selfSigned) },
			wantURL: "https://127.0.0.1:",
		},
		{
			name: "--certs-dir flag", server: leaf, viaFlag: true,
			dir:     func(t *testing.T) string { return writeCertsDir(t, nil, ca) },
			wantURL: "https://127.0.0.1:",
		},
		{
			name: "readiness over https", server: leaf,
			dir:     func(t *testing.T) string { return writeCertsDir(t, leaf, ca) },
			args:    func(*httptest.Server) []string { return []string{"--ready"} },
			wantURL: "https://127.0.0.1:",
		},
		{
			name: "certificate of an unknown CA", server: untrusted,
			dir:     func(t *testing.T) string { return writeCertsDir(t, leaf, ca) },
			wantErr: "unknown authority",
		},
		{
			name: "certificates directory but plain HTTP server", server: nil,
			dir:     func(t *testing.T) string { return writeCertsDir(t, leaf, ca) },
			wantErr: "healthcheck https://",
		},
		{
			name: "explicit https URL verified with the host name", server: localhost,
			dir: func(t *testing.T) string { return writeCertsDir(t, nil, ca) },
			args: func(srv *httptest.Server) []string {
				return []string{"--url", strings.Replace(srv.URL, "127.0.0.1", "localhost", 1) + "/nexo/health/live"}
			},
			wantURL: "https://localhost:",
		},
		{
			name: "explicit https URL whose host the certificate does not name", server: leaf,
			dir: func(t *testing.T) string { return writeCertsDir(t, leaf, ca) },
			args: func(srv *httptest.Server) []string {
				return []string{"--url", srv.URL + "/nexo/health/live"}
			},
			wantErr: "IP SANs",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := healthServer(t, tc.server)
			dir := tc.dir(t)
			env := map[string]string{"NEXO_CONSOLE_ADDRESS": srv.Listener.Addr().String()}
			args := []string{"healthcheck"}
			if tc.viaFlag {
				args = append(args, "--certs-dir", dir)
			} else {
				env["NEXO_CERTS_DIR"] = dir
			}
			if tc.args != nil {
				args = append(args, tc.args(srv)...)
			}
			deps, out, _ := testDeps(env, &http.Client{Timeout: 5 * time.Second})
			cmd := newRootCmd(deps)
			cmd.SetArgs(args)
			err := cmd.Execute()
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.True(t, strings.HasPrefix(out.String(), "ok: "+tc.wantURL), out.String())
		})
	}
}

// TestHealthcheckTLSConfig covers the trust store of the probe: an
// unreadable CAs directory fails, a missing one is fine, and the client of
// the command is never modified.
func TestHealthcheckTLSConfig(t *testing.T) {
	t.Run("missing directory", func(t *testing.T) {
		cfg, err := probeTLSConfig(filepath.Join(t.TempDir(), "absent"), true)
		require.NoError(t, err)
		assert.True(t, cfg.InsecureSkipVerify)
		assert.NotNil(t, cfg.VerifyConnection)
		assert.Equal(t, uint16(tls.VersionTLS12), cfg.MinVersion)
	})
	t.Run("explicit URL keeps the default verification", func(t *testing.T) {
		cfg, err := probeTLSConfig(t.TempDir(), false)
		require.NoError(t, err)
		assert.False(t, cfg.InsecureSkipVerify)
		assert.Nil(t, cfg.VerifyConnection)
	})
	t.Run("CAs is not a directory", func(t *testing.T) {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "CAs"), []byte("x"), 0o600))
		_, err := probeTLSConfig(dir, true)
		require.Error(t, err)
	})
	t.Run("no peer certificate", func(t *testing.T) {
		require.Error(t, verifyChain(nil, x509.NewCertPool()))
	})
	t.Run("base client untouched", func(t *testing.T) {
		base := &http.Client{Timeout: time.Second}
		c := withTLSConfig(base, &tls.Config{MinVersion: tls.VersionTLS13})
		assert.Nil(t, base.Transport)
		assert.Equal(t, time.Second, c.Timeout)
		tr, ok := c.Transport.(*http.Transport)
		require.True(t, ok)
		assert.Equal(t, uint16(tls.VersionTLS13), tr.TLSClientConfig.MinVersion)
	})
}
