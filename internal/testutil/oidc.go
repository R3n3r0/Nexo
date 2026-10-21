package testutil

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"
)

// FakeOIDC is an OpenID Connect provider for the tests: discovery, keys,
// an authorization endpoint approving at once the user set with SetUser
// (with PKCE), a token endpoint and ID tokens issued on demand (Token).
type FakeOIDC struct {
	URL          string
	ClientID     string
	ClientSecret string

	t   testing.TB
	key *rsa.PrivateKey
	srv *httptest.Server

	mu    sync.Mutex
	user  jwt.MapClaims
	codes map[string]oidcCode
	// TokenRequests counts the calls of the token endpoint.
	TokenRequests int
}

type oidcCode struct {
	nonce, challenge, redirect string
	claims                     jwt.MapClaims
}

// NewFakeOIDC starts a provider, stopped when the test ends.
func NewFakeOIDC(t testing.TB) *FakeOIDC {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	f := &FakeOIDC{t: t, key: key, ClientID: "nexo-console", ClientSecret: "client-secret-123", codes: map[string]oidcCode{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", f.discovery)
	mux.HandleFunc("GET /jwks", f.jwks)
	mux.HandleFunc("GET /authorize", f.authorize)
	mux.HandleFunc("POST /token", f.token)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	f.URL = f.srv.URL
	f.user = jwt.MapClaims{"sub": "user-1", "preferred_username": "ann"}
	return f
}

// ConfigURL is the discovery URL (identity_openid config_url).
func (f *FakeOIDC) ConfigURL() string { return f.URL + "/.well-known/openid-configuration" }

// SetUser sets the claims of the user the authorization endpoint logs in.
func (f *FakeOIDC) SetUser(claims map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.user = jwt.MapClaims(claims)
}

// Token issues an ID token for the client with the claims (iss, aud, iat
// and exp, one hour, are added unless set).
func (f *FakeOIDC) Token(claims map[string]any) string {
	f.t.Helper()
	cl := jwt.MapClaims{"iss": f.URL, "aud": f.ClientID, "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix()}
	for k, v := range claims {
		cl[k] = v
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, cl)
	tok.Header["kid"] = "k1"
	s, err := tok.SignedString(f.key)
	require.NoError(f.t, err)
	return s
}

func (f *FakeOIDC) discovery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"issuer": f.URL, "authorization_endpoint": f.URL + "/authorize", "token_endpoint": f.URL + "/token",
		"jwks_uri": f.URL + "/jwks", "token_endpoint_auth_methods_supported": []string{"client_secret_basic"},
	})
}

func (f *FakeOIDC) jwks(w http.ResponseWriter, _ *http.Request) {
	pub := f.key.PublicKey
	writeJSON(w, map[string]any{"keys": []map[string]string{{
		"kty": "RSA", "kid": "k1", "use": "sig", "alg": "RS256",
		"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}}})
}

func (f *FakeOIDC) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("client_id") != f.ClientID || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" {
		http.Error(w, "bad authorization request", http.StatusBadRequest)
		return
	}
	code := base64.RawURLEncoding.EncodeToString(randomBytes(16))
	f.mu.Lock()
	f.codes[code] = oidcCode{nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), redirect: q.Get("redirect_uri"), claims: f.user}
	f.mu.Unlock()
	target, err := url.Parse(q.Get("redirect_uri"))
	if err != nil {
		http.Error(w, "bad redirect", http.StatusBadRequest)
		return
	}
	tq := target.Query()
	tq.Set("code", code)
	tq.Set("state", q.Get("state"))
	target.RawQuery = tq.Encode()
	http.Redirect(w, r, target.String(), http.StatusFound)
}

func (f *FakeOIDC) token(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.TokenRequests++
	f.mu.Unlock()
	id, secret, ok := r.BasicAuth()
	if !ok || id != f.ClientID || secret != f.ClientSecret {
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]string{"error": "invalid_client"})
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	c, ok := f.codes[r.PostForm.Get("code")]
	delete(f.codes, r.PostForm.Get("code"))
	f.mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != c.challenge || r.PostForm.Get("redirect_uri") != c.redirect {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "invalid_grant"})
		return
	}
	claims := map[string]any{"nonce": c.nonce}
	for k, v := range c.claims {
		claims[k] = v
	}
	writeJSON(w, map[string]any{"access_token": "at", "token_type": "Bearer", "id_token": f.Token(claims)})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}
