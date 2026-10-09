package kms_test

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/kms"
	kmsmock "github.com/r3n3r0/nexo/internal/service/kms/mock"
	"github.com/r3n3r0/nexo/internal/service/sysstore"
	sysmock "github.com/r3n3r0/nexo/internal/service/sysstore/mock"
)

// fakeKES simulates the KES API (v1) the client speaks: named keys of
// random material, data keys sealed with AES-256-GCM bound to the
// context, a paged key listing and the status.
type fakeKES struct {
	mu   sync.Mutex
	keys map[string][]byte
	// pageSize splits the key listing in pages (0: one page).
	pageSize int
	calls    []string
}

func newFakeKES(keys ...string) *fakeKES {
	f := &fakeKES{keys: map[string][]byte{}}
	for _, k := range keys {
		f.keys[k] = randomBytes(32)
	}
	return f
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func (f *fakeKES) aead(name string) (cipher.AEAD, bool) {
	f.mu.Lock()
	key, ok := f.keys[name]
	f.mu.Unlock()
	if !ok {
		return nil, false
	}
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	return gcm, true
}

func kesFail(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": msg})
}

func (f *fakeKES) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	f.mu.Unlock()
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		kesFail(w, http.StatusUnauthorized, "no client certificate")
		return
	}
	path := r.URL.Path
	var in struct {
		Context    []byte `json:"context"`
		Ciphertext []byte `json:"ciphertext"`
		Plaintext  []byte `json:"plaintext"`
	}
	if r.Method == http.MethodPost && r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			kesFail(w, http.StatusBadRequest, "malformed request")
			return
		}
	}
	reply := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	switch {
	case path == "/v1/status":
		reply(map[string]any{"version": "2024-01-01T00-00-00Z", "uptime": 42})
	case strings.HasPrefix(path, "/v1/key/create/"):
		name := strings.TrimPrefix(path, "/v1/key/create/")
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, ok := f.keys[name]; ok {
			kesFail(w, http.StatusBadRequest, "key already exists")
			return
		}
		f.keys[name] = randomBytes(32)
	case strings.HasPrefix(path, "/v1/key/list/"):
		prefix := strings.TrimPrefix(path, "/v1/key/list/")
		f.mu.Lock()
		var names []string
		for n := range f.keys {
			if strings.HasPrefix(n, prefix) || (f.pageSize > 0 && n >= prefix) {
				names = append(names, n)
			}
		}
		f.mu.Unlock()
		sort.Strings(names)
		out := map[string]any{"names": names, "continue_at": ""}
		if f.pageSize > 0 && len(names) > f.pageSize {
			out["names"], out["continue_at"] = names[:f.pageSize], names[f.pageSize]
		}
		reply(out)
	case strings.HasPrefix(path, "/v1/key/generate/"), strings.HasPrefix(path, "/v1/key/encrypt/"):
		name := path[strings.LastIndexByte(path, '/')+1:]
		gcm, ok := f.aead(name)
		if !ok {
			kesFail(w, http.StatusNotFound, "key does not exist")
			return
		}
		plain := in.Plaintext
		if strings.HasPrefix(path, "/v1/key/generate/") {
			plain = randomBytes(32)
		}
		nonce := randomBytes(gcm.NonceSize())
		sealed := gcm.Seal(nonce, nonce, plain, in.Context)
		reply(map[string][]byte{"plaintext": plain, "ciphertext": sealed})
	case strings.HasPrefix(path, "/v1/key/decrypt/"):
		gcm, ok := f.aead(strings.TrimPrefix(path, "/v1/key/decrypt/"))
		if !ok {
			kesFail(w, http.StatusNotFound, "key does not exist")
			return
		}
		if len(in.Ciphertext) < gcm.NonceSize() {
			kesFail(w, http.StatusBadRequest, "ciphertext is not authentic")
			return
		}
		plain, err := gcm.Open(nil, in.Ciphertext[:gcm.NonceSize()], in.Ciphertext[gcm.NonceSize():], in.Context)
		if err != nil {
			kesFail(w, http.StatusBadRequest, "ciphertext is not authentic")
			return
		}
		reply(map[string][]byte{"plaintext": plain})
	default:
		kesFail(w, http.StatusNotFound, "not found")
	}
}

// pki is a test CA with a server and a client certificate, written as
// PEM files in a directory.
type pki struct {
	dir                  string
	caFile               string
	clientCert, clientKy string
	serverTLS            *tls.Config
}

func newPKI(t *testing.T) pki {
	t.Helper()
	dir := t.TempDir()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	require.NoError(t, err)
	ca, err := x509.ParseCertificate(caDER)
	require.NoError(t, err)
	issue := func(serial int64, usage x509.ExtKeyUsage, server bool) (certPEM, keyPEM []byte) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(t, err)
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "leaf"},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
			KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage},
		}
		if server {
			tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
			tmpl.DNSNames = []string{"localhost"}
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
		require.NoError(t, err)
		keyDER, err := x509.MarshalECPrivateKey(key)
		require.NoError(t, err)
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	}
	p := pki{dir: dir, caFile: filepath.Join(dir, "ca.crt"), clientCert: filepath.Join(dir, "client.crt"), clientKy: filepath.Join(dir, "client.key")}
	require.NoError(t, os.WriteFile(p.caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o600))
	cc, ck := issue(2, x509.ExtKeyUsageClientAuth, false)
	require.NoError(t, os.WriteFile(p.clientCert, cc, 0o600))
	require.NoError(t, os.WriteFile(p.clientKy, ck, 0o600))
	sc, sk := issue(3, x509.ExtKeyUsageServerAuth, true)
	serverCert, err := tls.X509KeyPair(sc, sk)
	require.NoError(t, err)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	p.serverTLS = &tls.Config{Certificates: []tls.Certificate{serverCert}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS12}
	return p
}

// startKES starts the simulated KES server over mutual TLS.
func startKES(t *testing.T, p pki, f *fakeKES) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(f)
	srv.TLS = p.serverTLS
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func newKESClient(t *testing.T, p pki, endpoints ...string) kms.Service {
	t.Helper()
	client, err := kms.NewKESClient(p.clientCert, p.clientKy, p.caFile)
	require.NoError(t, err)
	k, err := kms.NewKES(kms.KESDeps{HTTP: client}, kms.WithEndpoints(endpoints...), kms.WithDefaultKey("nexo-default"))
	require.NoError(t, err)
	return k
}

func TestKES(t *testing.T) {
	ctx := context.Background()
	p := newPKI(t)
	fake := newFakeKES("nexo-default")
	srv := startKES(t, p, fake)
	k := newKESClient(t, p, srv.URL)
	kctx := kms.Context{"aws:s3:arn": "arn:aws:s3:::b/o"}

	st, err := k.Status(ctx)
	require.NoError(t, err)
	assert.True(t, st.Online)
	assert.Equal(t, kms.BackendKES, st.Backend)
	assert.Equal(t, "2024-01-01T00-00-00Z", st.Version)
	assert.Equal(t, []string{srv.URL}, st.Endpoints)

	dek, err := k.GenerateKey(ctx, "", kctx)
	require.NoError(t, err)
	assert.Equal(t, "nexo-default", dek.KeyID)
	plain, err := k.Decrypt(ctx, "nexo-default", dek.Ciphertext, kctx)
	require.NoError(t, err)
	assert.Equal(t, dek.Plaintext, plain)
	_, err = k.Decrypt(ctx, "nexo-default", dek.Ciphertext, kms.Context{"aws:s3:arn": "arn:aws:s3:::b/x"})
	require.ErrorIs(t, err, domain.ErrDecryption)

	resealed, err := k.ReEncrypt(ctx, "", dek.Ciphertext, kctx)
	require.NoError(t, err)
	plain, err = k.Decrypt(ctx, "", resealed, kctx)
	require.NoError(t, err)
	assert.Equal(t, dek.Plaintext, plain)

	_, err = k.GenerateKey(ctx, "tenant", kctx)
	require.ErrorIs(t, err, domain.ErrKMSKeyNotFound)
	require.NoError(t, k.CreateKey(ctx, "tenant"))
	require.ErrorIs(t, k.CreateKey(ctx, "tenant"), domain.ErrKMSKeyExists)
	require.ErrorIs(t, k.CreateKey(ctx, "bad/name"), domain.ErrInvalidArgument)
	_, err = k.GenerateKey(ctx, "tenant", kctx)
	require.NoError(t, err)

	fake.pageSize = 1
	keys, err := k.ListKeys(ctx, "")
	require.NoError(t, err)
	assert.Equal(t, []string{"nexo-default", "tenant"}, names(keys), "every page is read")
	assert.True(t, keys[0].Default)
}

func TestKESFailover(t *testing.T) {
	ctx := context.Background()
	p := newPKI(t)
	srv := startKES(t, p, newFakeKES("nexo-default"))
	down := httptest.NewUnstartedServer(http.NotFoundHandler())
	down.TLS = p.serverTLS
	down.StartTLS()
	downURL := down.URL
	down.Close()

	k := newKESClient(t, p, downURL, srv.URL)
	_, err := k.GenerateKey(ctx, "", nil)
	require.NoError(t, err, "the unreachable server is skipped")

	only := newKESClient(t, p, downURL)
	_, err = only.GenerateKey(ctx, "", nil)
	require.ErrorIs(t, err, domain.ErrKMSUnavailable)
	st, err := only.Status(ctx)
	require.NoError(t, err)
	assert.False(t, st.Online)
	assert.NotEmpty(t, st.Error)
}

// TestKESRequiresClientCertificate: the KES server only answers the
// clients presenting a certificate of its CA (mutual TLS).
func TestKESRequiresClientCertificate(t *testing.T) {
	ctx := context.Background()
	p := newPKI(t)
	srv := startKES(t, p, newFakeKES("nexo-default"))
	client, err := kms.NewKESClient("", "", p.caFile)
	require.NoError(t, err)
	k, err := kms.NewKES(kms.KESDeps{HTTP: client}, kms.WithEndpoints(srv.URL), kms.WithDefaultKey("nexo-default"))
	require.NoError(t, err)
	_, err = k.GenerateKey(ctx, "", nil)
	require.ErrorIs(t, err, domain.ErrKMSUnavailable)
}

func TestNewKESValidation(t *testing.T) {
	doer := kmsmock.NewHTTPDoer(t)
	tests := []struct {
		name string
		deps kms.KESDeps
		opts []kms.Option
	}{
		{name: "no client", opts: []kms.Option{kms.WithEndpoints("https://kes"), kms.WithDefaultKey("k")}},
		{name: "no endpoint", deps: kms.KESDeps{HTTP: doer}, opts: []kms.Option{kms.WithDefaultKey("k")}},
		{name: "bad endpoint", deps: kms.KESDeps{HTTP: doer}, opts: []kms.Option{kms.WithEndpoints("kes:7373"), kms.WithDefaultKey("k")}},
		// The data keys travel in clear in the KES API: plain HTTP is refused.
		{name: "plain http", deps: kms.KESDeps{HTTP: doer}, opts: []kms.Option{kms.WithEndpoints("https://kes1", "http://127.0.0.1:7373"), kms.WithDefaultKey("k")}},
		{name: "credentials in the URL", deps: kms.KESDeps{HTTP: doer}, opts: []kms.Option{kms.WithEndpoints("https://user:s3cr3t-pass@kes"), kms.WithDefaultKey("k")}},
		{name: "credentials over plain http", deps: kms.KESDeps{HTTP: doer}, opts: []kms.Option{kms.WithEndpoints("http://user:s3cr3t-pass@kes?token=s3cr3t-pass"), kms.WithDefaultKey("k")}},
		{name: "credentials in a malformed URL", deps: kms.KESDeps{HTTP: doer}, opts: []kms.Option{kms.WithEndpoints("https://user:s3cr3t-pass@kes host"), kms.WithDefaultKey("k")}},
		{name: "no default key", deps: kms.KESDeps{HTTP: doer}, opts: []kms.Option{kms.WithEndpoints("https://kes")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := kms.NewKES(tt.deps, tt.opts...)
			require.ErrorIs(t, err, domain.ErrInvalidArgument)
			assert.NotContains(t, err.Error(), "s3cr3t-pass", "the errors are logged: no credentials")
		})
	}
	_, err := kms.NewKESClient("cert-only.pem", "", "")
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
}

// TestKESNoRedirect: the client never follows a redirect of a KES server
// (a data key, or the request of one, would reach another host): the
// redirect is an error.
func TestKESNoRedirect(t *testing.T) {
	ctx := context.Background()
	p := newPKI(t)
	target := startKES(t, p, newFakeKES("nexo-default"))
	redirector := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	redirector.TLS = p.serverTLS
	redirector.StartTLS()
	t.Cleanup(redirector.Close)
	k := newKESClient(t, p, redirector.URL)
	_, err := k.GenerateKey(ctx, "", kms.Context{"aws:s3:arn": "arn:aws:s3:::b/o"})
	require.ErrorIs(t, err, domain.ErrKMSUnavailable, "the redirect is not followed")
}

// TestKESErrorMapping maps the HTTP errors of a KES server to the domain
// sentinels (the transport is the generated mock).
func TestKESErrorMapping(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{name: "not found", status: http.StatusNotFound, body: `{"message":"key does not exist"}`, want: domain.ErrKMSKeyNotFound},
		{name: "exists", status: http.StatusConflict, body: `{"message":"key already exists"}`, want: domain.ErrKMSKeyExists},
		{name: "not authentic", status: http.StatusBadRequest, body: `{"message":"ciphertext is not authentic"}`, want: domain.ErrDecryption},
		{name: "forbidden", status: http.StatusForbidden, body: `{"message":"not allowed"}`, want: domain.ErrKMSUnavailable},
		{name: "server error", status: http.StatusInternalServerError, body: ``, want: domain.ErrKMSUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doer := kmsmock.NewHTTPDoer(t)
			doer.EXPECT().Do(mock.Anything).RunAndReturn(func(*http.Request) (*http.Response, error) {
				rec := httptest.NewRecorder()
				rec.WriteHeader(tt.status)
				_, _ = rec.WriteString(tt.body)
				return rec.Result(), nil
			})
			k, err := kms.NewKES(kms.KESDeps{HTTP: doer}, kms.WithEndpoints("https://kes:7373"), kms.WithDefaultKey("k"))
			require.NoError(t, err)
			_, err = k.Decrypt(context.Background(), "k", []byte("x"), nil)
			require.ErrorIs(t, err, tt.want)
		})
	}
}

// newDocsKeyStore returns a key store on an in-memory system document
// store (the generated mock, its state modeled with RunAndReturn).
func newDocsKeyStore(t *testing.T) kms.KeyStore {
	t.Helper()
	var mu sync.Mutex
	docs := map[string][]byte{}
	m := sysmock.NewStore(t)
	m.EXPECT().Create(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, path string, data []byte) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		if cur, ok := docs[path]; ok {
			return cur, nil
		}
		docs[path] = data
		return data, nil
	}).Maybe()
	m.EXPECT().Get(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, path string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		if cur, ok := docs[path]; ok {
			return cur, nil
		}
		return nil, sysstore.ErrNotFound
	}).Maybe()
	m.EXPECT().List(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, dir string) (map[string][]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		out := map[string][]byte{}
		for p, d := range docs {
			if name, ok := strings.CutPrefix(p, dir+"/"); ok && !strings.Contains(name, "/") {
				out[name] = d
			}
		}
		return out, nil
	}).Maybe()
	s, err := kms.NewKeyStore(kms.KeyStoreDeps{Docs: m})
	require.NoError(t, err)
	return s
}
