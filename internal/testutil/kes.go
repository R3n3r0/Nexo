package testutil

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// WithKES configures the external KMS of the server-side encryption: a
// simulated KES server (the KES API v1 the client speaks, over mutual TLS)
// started for the deployment and shared by its nodes, whose default key is
// KMSKeyName (NEXO_KMS_ENDPOINT, NEXO_KMS_KEY_NAME, NEXO_KMS_CLIENT_CERT,
// NEXO_KMS_CLIENT_KEY, NEXO_KMS_CAPATH). The server refuses the clients
// without a certificate of its CA; it stops when the test ends and keeps
// its keys across the restarts of the deployment.
func WithKES() Option {
	return func(s *settings) { s.kes = true }
}

// setupKES starts the simulated KES server of WithKES and points the
// configuration to it.
func setupKES(t testing.TB, st *settings) {
	t.Helper()
	if !st.kes || len(st.cfg.KMSEndpoints) > 0 {
		return
	}
	// The certificate of the test PKI serves both ends: it is valid for
	// 127.0.0.1 and for client authentication.
	pki := newTestPKI(t)
	srv := httptest.NewUnstartedServer(newFakeKES(KMSKeyName))
	srv.TLS = pki.server.Clone()
	srv.TLS.ClientCAs = pki.client.RootCAs
	srv.TLS.ClientAuth = tls.RequireAndVerifyClientCert
	srv.StartTLS()
	t.Cleanup(srv.Close)
	st.cfg.KMSEndpoints = []string{srv.URL}
	st.cfg.KMSKeyName = KMSKeyName
	st.cfg.KMSCertFile = filepath.Join(pki.dir, "public.crt")
	st.cfg.KMSKeyFile = filepath.Join(pki.dir, "private.key")
	st.cfg.KMSCAPath = filepath.Join(pki.dir, "CAs", "ca.crt")
}

// fakeKES simulates a KES server: named keys of random material, data
// keys sealed with AES-256-GCM bound to their context, a key listing and
// the status.
type fakeKES struct {
	mu   sync.Mutex
	keys map[string][]byte
}

func newFakeKES(keys ...string) *fakeKES {
	f := &fakeKES{keys: map[string][]byte{}}
	for _, k := range keys {
		f.keys[k] = randomKey()
	}
	return f
}

func randomKey() []byte {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // never fails (Go 1.24)
	return b
}

// aead returns the cipher of a key, false when unknown.
func (f *fakeKES) aead(name string) (cipher.AEAD, bool) {
	f.mu.Lock()
	key, ok := f.keys[name]
	f.mu.Unlock()
	if !ok {
		return nil, false
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, false
	}
	gcm, err := cipher.NewGCM(block)
	return gcm, err == nil
}

func kesReply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func kesFail(w http.ResponseWriter, status int, msg string) {
	kesReply(w, status, map[string]string{"message": msg})
}

// ServeHTTP implements the subset of the KES API v1 of the client
// (internal/service/kms).
func (f *fakeKES) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		kesFail(w, http.StatusUnauthorized, "no client certificate")
		return
	}
	var in struct {
		Context    []byte `json:"context"`
		Ciphertext []byte `json:"ciphertext"`
		Plaintext  []byte `json:"plaintext"`
	}
	if r.Method == http.MethodPost && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			kesFail(w, http.StatusBadRequest, "malformed request")
			return
		}
	}
	path := r.URL.Path
	name := path[strings.LastIndexByte(path, '/')+1:]
	switch {
	case r.Method == http.MethodGet && path == "/v1/status":
		kesReply(w, http.StatusOK, map[string]any{"version": "testutil-fake-kes"})
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/v1/key/create/"):
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, ok := f.keys[name]; ok {
			kesFail(w, http.StatusConflict, "key already exists")
			return
		}
		f.keys[name] = randomKey()
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/key/list/"):
		f.mu.Lock()
		names := []string{}
		for n := range f.keys {
			if strings.HasPrefix(n, name) {
				names = append(names, n)
			}
		}
		f.mu.Unlock()
		sort.Strings(names)
		kesReply(w, http.StatusOK, map[string]any{"names": names, "continue_at": ""})
	case r.Method == http.MethodPost && (strings.HasPrefix(path, "/v1/key/generate/") || strings.HasPrefix(path, "/v1/key/encrypt/")):
		gcm, ok := f.aead(name)
		if !ok {
			kesFail(w, http.StatusNotFound, "key does not exist")
			return
		}
		plain := in.Plaintext
		if strings.HasPrefix(path, "/v1/key/generate/") {
			plain = randomKey()
		}
		nonce := make([]byte, gcm.NonceSize())
		_, _ = rand.Read(nonce)
		kesReply(w, http.StatusOK, map[string][]byte{"plaintext": plain, "ciphertext": gcm.Seal(nonce, nonce, plain, in.Context)})
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/v1/key/decrypt/"):
		gcm, ok := f.aead(name)
		if !ok {
			kesFail(w, http.StatusNotFound, "key does not exist")
			return
		}
		n := gcm.NonceSize()
		if len(in.Ciphertext) < n {
			kesFail(w, http.StatusBadRequest, "ciphertext is not authentic")
			return
		}
		plain, err := gcm.Open(nil, in.Ciphertext[:n], in.Ciphertext[n:], in.Context)
		if err != nil {
			kesFail(w, http.StatusBadRequest, "ciphertext is not authentic")
			return
		}
		kesReply(w, http.StatusOK, map[string][]byte{"plaintext": plain})
	default:
		kesFail(w, http.StatusNotFound, "not found")
	}
}
