package idp_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/config"
	"github.com/r3n3r0/nexo/internal/service/idp"
	"github.com/r3n3r0/nexo/internal/testutil"
)

func newIDP(t *testing.T, seed map[string]config.KV) (idp.Service, *clock.Fake) {
	t.Helper()
	clk := clock.NewFake(time.Now())
	svc, err := idp.New(idp.Deps{Config: config.New(config.Deps{Store: config.NewMemoryStore(seed)}), Clock: clk})
	require.NoError(t, err)
	return svc, clk
}

func oidcConfig(p *testutil.FakeOIDC) config.KV {
	return config.KV{"enable": "on", "config_url": p.ConfigURL(), "client_id": p.ClientID, "client_secret": p.ClientSecret, "display_name": "Corporate SSO"}
}

// covers: S-IAM-08
func TestWebIdentity(t *testing.T) {
	ctx := context.Background()
	p := testutil.NewFakeOIDC(t)
	svc, _ := newIDP(t, map[string]config.KV{"identity_openid": oidcConfig(p)})

	prov, err := svc.Providers(ctx)
	require.NoError(t, err)
	assert.True(t, prov.OpenID)
	assert.Equal(t, "Corporate SSO", prov.OpenIDName)
	assert.False(t, prov.LDAP)

	id, err := svc.WebIdentity(ctx, p.Token(map[string]any{"sub": "00u1", "preferred_username": "ann", "groups": []any{"analysts", "ops"}, "policy": "readonly,diagnostics"}))
	require.NoError(t, err)
	assert.Equal(t, idp.ProviderOpenID, id.Provider)
	assert.Equal(t, "00u1", id.Subject)
	assert.Equal(t, "ann", id.Username)
	assert.Equal(t, []string{"analysts", "ops"}, id.Groups)
	assert.Equal(t, []string{"readonly", "diagnostics"}, id.PolicyNames)
	assert.False(t, id.Expiry.IsZero())

	for name, token := range map[string]string{
		"wrong audience": p.Token(map[string]any{"sub": "x", "aud": "other"}),
		"wrong issuer":   p.Token(map[string]any{"sub": "x", "iss": "https://evil"}),
		"expired":        p.Token(map[string]any{"sub": "x", "exp": time.Now().Add(-time.Hour).Unix()}),
		"no subject":     p.Token(map[string]any{}),
		"tampered":       tamper(p.Token(map[string]any{"sub": "x"})),
		"unsigned":       "eyJhbGciOiJub25lIn0.eyJzdWIiOiJ4In0.",
		"garbage":        "not-a-token",
	} {
		_, err := svc.WebIdentity(ctx, token)
		require.ErrorIs(t, err, domain.ErrAccessDenied, name)
	}

	disabled, _ := newIDP(t, nil)
	_, err = disabled.WebIdentity(ctx, p.Token(map[string]any{"sub": "x"}))
	require.ErrorIs(t, err, domain.ErrNotImplemented)
}

// tamper changes the payload of a JWT, keeping its signature.
func tamper(token string) string {
	parts := strings.Split(token, ".")
	parts[1] = base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"admin","aud":"nexo-console","exp":9999999999}`))
	return strings.Join(parts, ".")
}

// covers: S-IAM-08, D-AUTH-02
func TestAuthorizationCodeFlow(t *testing.T) {
	ctx := context.Background()
	p := testutil.NewFakeOIDC(t)
	p.SetUser(map[string]any{"sub": "00u2", "email": "bob@example.com", "groups": "admins"})
	svc, _ := newIDP(t, map[string]config.KV{"identity_openid": oidcConfig(p)})

	verifier := "verifier-0123456789-0123456789-0123456789"
	sum := sha256.Sum256([]byte(verifier))
	redirect := "https://console.example.com/api/v1/auth/openid/callback"
	authURL, err := svc.AuthCodeURL(ctx, idp.AuthCodeInput{RedirectURI: redirect, State: "st", Nonce: "n-1", CodeChallenge: base64.RawURLEncoding.EncodeToString(sum[:])})
	require.NoError(t, err)
	u, err := url.Parse(authURL)
	require.NoError(t, err)
	assert.Equal(t, "openid profile email", u.Query().Get("scope"))

	// The provider approves and redirects back with the code.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(authURL)
	require.NoError(t, err)
	_ = resp.Body.Close()
	back, err := url.Parse(resp.Header.Get("Location"))
	require.NoError(t, err)
	assert.Equal(t, "st", back.Query().Get("state"))
	code := back.Query().Get("code")

	_, err = svc.Exchange(ctx, idp.ExchangeInput{Code: code, RedirectURI: redirect, CodeVerifier: "wrong", Nonce: "n-1"})
	require.ErrorIs(t, err, domain.ErrAccessDenied, "PKCE")
	resp, err = client.Get(authURL)
	require.NoError(t, err)
	_ = resp.Body.Close()
	back, _ = url.Parse(resp.Header.Get("Location"))
	_, err = svc.Exchange(ctx, idp.ExchangeInput{Code: back.Query().Get("code"), RedirectURI: redirect, CodeVerifier: verifier, Nonce: "other"})
	require.ErrorIs(t, err, domain.ErrAccessDenied, "nonce")

	resp, err = client.Get(authURL)
	require.NoError(t, err)
	_ = resp.Body.Close()
	back, _ = url.Parse(resp.Header.Get("Location"))
	id, err := svc.Exchange(ctx, idp.ExchangeInput{Code: back.Query().Get("code"), RedirectURI: redirect, CodeVerifier: verifier, Nonce: "n-1"})
	require.NoError(t, err)
	assert.Equal(t, "bob@example.com", id.Username, "the email when there is no preferred_username")
	assert.Equal(t, []string{"admins"}, id.Groups)
}

func ldapConfig(d *testutil.FakeLDAP) config.KV {
	return config.KV{
		"enable": "on", "server_addr": d.Addr, "tls": "off", "lookup_bind_dn": testutil.LDAPBindDN, "lookup_bind_password": testutil.LDAPBindPass,
		"user_dn_search_base_dn": testutil.LDAPUserBase, "group_search_base_dn": testutil.LDAPGroupBase,
	}
}

// covers: S-IAM-09
func TestLDAP(t *testing.T) {
	ctx := context.Background()
	d := testutil.NewFakeLDAP(t)
	d.AddUser("carol", "carol-pw", "analysts", "ops")
	d.AddUser("dave", "dave-pw")
	svc, clk := newIDP(t, map[string]config.KV{"identity_ldap": ldapConfig(d)})

	prov, err := svc.Providers(ctx)
	require.NoError(t, err)
	assert.True(t, prov.LDAP)

	id, err := svc.LDAP(ctx, "carol", "carol-pw")
	require.NoError(t, err)
	assert.Equal(t, idp.ProviderLDAP, id.Provider)
	assert.Equal(t, testutil.LDAPUserDN("carol"), id.Subject)
	assert.ElementsMatch(t, []string{"analysts", "ops"}, id.Groups)

	id, err = svc.LDAP(ctx, "dave", "dave-pw")
	require.NoError(t, err)
	assert.Empty(t, id.Groups)

	for _, c := range [][2]string{{"carol", "wrong"}, {"nobody", "x"}, {"carol", ""}, {"", "x"}, {"*", "carol-pw"}} {
		_, err := svc.LDAP(ctx, c[0], c[1])
		require.ErrorIs(t, err, domain.ErrAccessDenied, "%v", c)
	}

	// Repeated failures lock the name out, without asking the directory.
	for range 5 {
		_, _ = svc.LDAP(ctx, "dave", "bad")
	}
	binds := d.Binds
	_, err = svc.LDAP(ctx, "dave", "dave-pw")
	require.ErrorIs(t, err, domain.ErrTooManyRequests)
	assert.Equal(t, binds, d.Binds)
	clk.Advance(time.Minute)
	_, err = svc.LDAP(ctx, "dave", "dave-pw")
	require.NoError(t, err)

	// An unreachable directory is unavailable, not a wrong password.
	down, _ := newIDP(t, map[string]config.KV{"identity_ldap": {"enable": "on", "server_addr": "127.0.0.1:1", "tls": "off", "user_dn_search_base_dn": "dc=x"}})
	_, err = down.LDAP(ctx, "carol", "carol-pw")
	require.ErrorIs(t, err, domain.ErrServiceUnavailable)
}
