package certs_test

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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/certs"
)

// pairSpec describes a certificate issued for a keystore test.
type pairSpec struct {
	names    []string
	ips      []net.IP
	notAfter time.Time
}

// issue returns a certificate of ca for spec, usable as server and client
// certificate, as PEM chain (leaf, then the CA) and PEM key.
func (ca *testCA) issue(t testing.TB, spec pairSpec) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	notAfter := spec.notAfter
	if notAfter.IsZero() {
		notAfter = time.Now().Add(24 * time.Hour)
	}
	cn := "node"
	if len(spec.names) > 0 {
		cn = spec.names[0]
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter, DNSNames: spec.names, IPAddresses: spec.ips,
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
}

// writeKeyPair writes a key pair in dir (created).
func writeKeyPair(t testing.TB, dir, chain, key string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, certs.PublicCertFile), []byte(chain), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, certs.PrivateKeyFile), []byte(key), 0o600))
}

// leafOf parses the leaf certificate of a PEM chain.
func leafOf(t testing.TB, chain string) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode([]byte(chain))
	require.NotNil(t, block)
	c, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	return c
}

// newKeystore loads dir with quiet logs.
func newKeystore(t testing.TB, dir string, opts ...certs.KeystoreOption) (certs.Keystore, error) {
	t.Helper()
	return certs.NewKeystore(certs.KeystoreDeps{Logger: slog.New(slog.DiscardHandler)}, dir, opts...)
}

// handshake serves ks on a TLS listener and returns the leaf certificate
// a client asking for serverName receives.
func handshake(t testing.TB, ks certs.Keystore, serverName string) *x509.Certificate {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{GetCertificate: ks.GetCertificate, MinVersion: tls.VersionTLS12})
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_ = c.(*tls.Conn).Handshake()
		_, _ = io.Copy(io.Discard, c)
		_ = c.Close()
	}()
	conn, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{
		ServerName: serverName, MinVersion: tls.VersionTLS12,
		InsecureSkipVerify: true, //nolint:gosec // G402: the test inspects the certificate the server presents
	})
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	return conn.ConnectionState().PeerCertificates[0]
}

func TestKeystoreSelectsByServerName(t *testing.T) {
	dir := t.TempDir()
	ca := newCA(t, "deployment CA")
	defChain, defKey := ca.issue(t, pairSpec{names: []string{"s3.example.org"}, ips: []net.IP{net.ParseIP("127.0.0.1")}})
	writeKeyPair(t, dir, defChain, defKey)
	siteChain, siteKey := ca.issue(t, pairSpec{names: []string{"www.example.com", "example.com"}})
	writeKeyPair(t, filepath.Join(dir, "example"), siteChain, siteKey)
	wildChain, wildKey := ca.issue(t, pairSpec{names: []string{"*.cdn.example.net"}})
	writeKeyPair(t, filepath.Join(dir, "cdn"), wildChain, wildKey)
	// A newer certificate for the same name wins over an older one.
	oldChain, oldKey := ca.issue(t, pairSpec{names: []string{"www.example.com"}, notAfter: time.Now().Add(2 * time.Hour)})
	writeKeyPair(t, filepath.Join(dir, "example-old"), oldChain, oldKey)
	// Hidden directories (the ..data of a mounted secret) and empty ones
	// are not key pairs.
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".hidden"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "empty"), 0o700))

	ks, err := newKeystore(t, dir, certs.WithoutSystemRoots())
	require.NoError(t, err)

	tests := []struct {
		name       string
		serverName string
		want       string
	}{
		{name: "exact name", serverName: "www.example.com", want: siteChain},
		{name: "other name of the same certificate", serverName: "EXAMPLE.com.", want: siteChain},
		{name: "wildcard", serverName: "img.cdn.example.net", want: wildChain},
		{name: "wildcard covers one label only", serverName: "a.img.cdn.example.net", want: defChain},
		{name: "unknown name", serverName: "unknown.example.org", want: defChain},
		{name: "no server name (IP)", serverName: "", want: defChain},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := handshake(t, ks, tt.serverName)
			assert.Equal(t, leafOf(t, tt.want).Raw, got.Raw)
		})
	}

	pairs := ks.KeyPairs(context.Background())
	require.Len(t, pairs, 4)
	assert.Empty(t, pairs[0].Name, "the default certificate first")
	assert.Equal(t, []string{"s3.example.org"}, pairs[0].Domains)
	assert.Equal(t, []string{"127.0.0.1"}, pairs[0].IPs)
	assert.Equal(t, []string{"cdn", "example", "example-old"}, []string{pairs[1].Name, pairs[2].Name, pairs[3].Name})
	assert.NotEmpty(t, pairs[0].Fingerprint)

	client, err := ks.GetClientCertificate(&tls.CertificateRequestInfo{})
	require.NoError(t, err)
	assert.Equal(t, leafOf(t, defChain).Raw, client.Leaf.Raw, "the node presents the default certificate to its peers")
}

func TestKeystoreTrustedCAs(t *testing.T) {
	dir := t.TempDir()
	nodeCA, peerCA, otherCA := newCA(t, "node CA"), newCA(t, "peer CA"), newCA(t, "other CA")
	chain, key := nodeCA.issue(t, pairSpec{names: []string{"node1"}})
	writeKeyPair(t, dir, chain, key)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, certs.CAsDir), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, certs.CAsDir, "peer.crt"), []byte(peerCA.pem), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, certs.CAsDir, "notes.txt"), []byte("no certificate here"), 0o600))

	ks, err := newKeystore(t, dir, certs.WithoutSystemRoots())
	require.NoError(t, err)

	peerChain, _ := peerCA.issue(t, pairSpec{names: []string{"node2"}})
	strangerChain, _ := otherCA.issue(t, pairSpec{names: []string{"node3"}})
	tests := []struct {
		name  string
		chain string
		ok    bool
	}{
		{name: "certificate of a CA of CAs/", chain: peerChain, ok: true},
		{name: "certificate of an unknown CA", chain: strangerChain},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, pool := range []*x509.CertPool{ks.ClientCAs(), ks.RootCAs()} {
				_, err := leafOf(t, tt.chain).Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}})
				if tt.ok {
					assert.NoError(t, err)
				} else {
					assert.Error(t, err)
				}
			}
		})
	}
}

func TestKeystoreLoadErrors(t *testing.T) {
	ca := newCA(t, "CA")
	chain, key := ca.issue(t, pairSpec{names: []string{"a.example.com"}})
	_, otherKey := ca.issue(t, pairSpec{names: []string{"a.example.com"}})
	noNameChain, noNameKey := ca.issue(t, pairSpec{ips: []net.IP{net.ParseIP("10.0.0.1")}})
	tests := []struct {
		name  string
		setup func(t *testing.T, dir string)
	}{
		{name: "no default key pair", setup: func(t *testing.T, dir string) {
			writeKeyPair(t, filepath.Join(dir, "site"), chain, key)
		}},
		{name: "key of another certificate", setup: func(t *testing.T, dir string) { writeKeyPair(t, dir, chain, otherKey) }},
		{name: "garbage", setup: func(t *testing.T, dir string) { writeKeyPair(t, dir, "not a certificate", "not a key") }},
		{name: "sub-directory with its certificate only", setup: func(t *testing.T, dir string) {
			writeKeyPair(t, dir, chain, key)
			require.NoError(t, os.MkdirAll(filepath.Join(dir, "site"), 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "site", certs.PublicCertFile), []byte(chain), 0o600))
		}},
		{name: "SNI certificate without a DNS name", setup: func(t *testing.T, dir string) {
			writeKeyPair(t, dir, chain, key)
			writeKeyPair(t, filepath.Join(dir, "ip"), noNameChain, noNameKey)
		}},
		{name: "oversized file", setup: func(t *testing.T, dir string) {
			writeKeyPair(t, dir, chain+string(make([]byte, 1<<20)), key)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			tt.setup(t, dir)
			_, err := newKeystore(t, dir)
			assert.ErrorIs(t, err, domain.ErrInvalidArgument)
		})
	}
	_, err := newKeystore(t, "")
	assert.ErrorIs(t, err, domain.ErrInvalidArgument, "the directory is required")
}

func TestKeystoreFailedReloadKeepsTheCertificates(t *testing.T) {
	dir := t.TempDir()
	ca := newCA(t, "CA")
	chain, key := ca.issue(t, pairSpec{names: []string{"s3.example.org"}})
	writeKeyPair(t, dir, chain, key)
	ks, err := newKeystore(t, dir)
	require.NoError(t, err)

	// A rotation half written: the new certificate with the old key.
	newChain, newKey := ca.issue(t, pairSpec{names: []string{"s3.example.org"}})
	require.NoError(t, os.WriteFile(filepath.Join(dir, certs.PublicCertFile), []byte(newChain), 0o600))
	require.ErrorIs(t, ks.Reload(context.Background()), domain.ErrInvalidArgument)
	assert.Equal(t, leafOf(t, chain).Raw, handshake(t, ks, "s3.example.org").Raw, "the previous certificate is still served")

	// The key arrives: the next reload installs the new pair.
	require.NoError(t, os.WriteFile(filepath.Join(dir, certs.PrivateKeyFile), []byte(newKey), 0o600))
	require.NoError(t, ks.Reload(context.Background()))
	assert.Equal(t, leafOf(t, newChain).Raw, handshake(t, ks, "s3.example.org").Raw)
}

func TestKeystoreWatchReloads(t *testing.T) {
	tests := []struct {
		name string
		opts []certs.KeystoreOption
	}{
		{name: "file notifications", opts: []certs.KeystoreOption{certs.WithKeystoreDebounce(20 * time.Millisecond), certs.WithKeystorePoll(-1)}},
		{name: "poller", opts: []certs.KeystoreOption{certs.WithoutKeystoreWatcher(), certs.WithKeystorePoll(20 * time.Millisecond)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			ca := newCA(t, "CA")
			chain, key := ca.issue(t, pairSpec{names: []string{"s3.example.org"}})
			writeKeyPair(t, dir, chain, key)
			ks, err := newKeystore(t, dir, tt.opts...)
			require.NoError(t, err)
			ctx := context.Background()
			require.NoError(t, ks.Start(ctx))
			t.Cleanup(func() { _ = ks.Stop(ctx) })

			// Rotation of the default certificate, written as an
			// atomic rename like the tools that renew certificates.
			newChain, newKey := ca.issue(t, pairSpec{names: []string{"s3.example.org"}})
			staged := filepath.Join(t.TempDir(), "staged")
			writeKeyPair(t, staged, newChain, newKey)
			require.NoError(t, os.Rename(filepath.Join(staged, certs.PrivateKeyFile), filepath.Join(dir, certs.PrivateKeyFile)))
			require.NoError(t, os.Rename(filepath.Join(staged, certs.PublicCertFile), filepath.Join(dir, certs.PublicCertFile)))
			require.Eventually(t, func() bool {
				return string(handshake(t, ks, "s3.example.org").Raw) == string(leafOf(t, newChain).Raw)
			}, 5*time.Second, 20*time.Millisecond, "the rotated certificate is served")

			// A new SNI directory is picked up too.
			siteChain, siteKey := ca.issue(t, pairSpec{names: []string{"www.example.com"}})
			writeKeyPair(t, filepath.Join(dir, "site"), siteChain, siteKey)
			require.Eventually(t, func() bool {
				return string(handshake(t, ks, "www.example.com").Raw) == string(leafOf(t, siteChain).Raw)
			}, 5*time.Second, 20*time.Millisecond, "the new SNI certificate is served")
			require.NoError(t, ks.Stop(ctx))
		})
	}
}

func TestKeystoreFollowsSymlinks(t *testing.T) {
	if _, err := os.Lstat("/proc"); err != nil {
		t.Skip("symbolic links of the test need a Unix file system")
	}
	// The layout of certbot: live/<name>/ links into archive/<name>/,
	// retargeted at each renewal outside the certificates directory.
	root := t.TempDir()
	archive, live, dir := filepath.Join(root, "archive"), filepath.Join(root, "live"), filepath.Join(root, "certs")
	for _, d := range []string{archive, live, dir} {
		require.NoError(t, os.MkdirAll(d, 0o700))
	}
	ca := newCA(t, "CA")
	write := func(n string, chain, key string) {
		require.NoError(t, os.WriteFile(filepath.Join(archive, "cert"+n+".pem"), []byte(chain), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(archive, "key"+n+".pem"), []byte(key), 0o600))
		for _, l := range []struct{ link, target string }{
			{"public.crt", "cert" + n + ".pem"}, {"private.key", "key" + n + ".pem"},
		} {
			tmp := filepath.Join(live, l.link+".tmp")
			require.NoError(t, os.Symlink(filepath.Join(archive, l.target), tmp))
			require.NoError(t, os.Rename(tmp, filepath.Join(live, l.link)))
		}
	}
	chain, key := ca.issue(t, pairSpec{names: []string{"s3.example.org"}})
	write("1", chain, key)
	require.NoError(t, os.Symlink(filepath.Join(live, "public.crt"), filepath.Join(dir, certs.PublicCertFile)))
	require.NoError(t, os.Symlink(filepath.Join(live, "private.key"), filepath.Join(dir, certs.PrivateKeyFile)))

	ks, err := newKeystore(t, dir, certs.WithKeystoreDebounce(20*time.Millisecond), certs.WithKeystorePoll(-1))
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, ks.Start(ctx))
	t.Cleanup(func() { _ = ks.Stop(ctx) })

	newChain, newKey := ca.issue(t, pairSpec{names: []string{"s3.example.org"}})
	write("2", newChain, newKey)
	require.Eventually(t, func() bool {
		return string(handshake(t, ks, "s3.example.org").Raw) == string(leafOf(t, newChain).Raw)
	}, 5*time.Second, 20*time.Millisecond, "the renewal behind the symbolic links is served")
}
