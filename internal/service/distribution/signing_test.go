package distribution_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/distribution"
)

// signedRequest describes a request for a signed URL.
func signedRequest(t *testing.T, group, signed, clientIP string) distribution.SignedRequest {
	t.Helper()
	u, err := url.Parse(signed)
	require.NoError(t, err)
	return distribution.SignedRequest{
		KeyGroup: group, Scheme: u.Scheme, Host: u.Host, EscapedPath: u.EscapedPath(), RawQuery: u.RawQuery, ClientIP: clientIP,
	}
}

// TestSignedURLs covers TDS-06: valid, expired, tampered, other IP,
// wildcard resource, cookies, every key algorithm and the rotation of the
// keys of a group.
func TestSignedURLs(t *testing.T) {
	ctx := context.Background()
	svc := newService(t)
	_, err := svc.CreateKeyGroup(ctx, "rilasci", "downloads")
	require.NoError(t, err)
	_, err = svc.CreateKeyGroup(ctx, "other", "")
	require.NoError(t, err)

	keys := map[string]distribution.GeneratedKey{}
	for name, in := range map[string]distribution.GenerateKeyInput{
		"ed25519":  {KeyGroup: "rilasci", Algorithm: distribution.KeyEd25519},
		"rsa-2048": {KeyGroup: "rilasci", Algorithm: distribution.KeyRSA},
		"rsa-4096": {KeyGroup: "rilasci", Algorithm: distribution.KeyRSA, Bits: 4096},
		"other":    {KeyGroup: "other"},
	} {
		k, err := svc.GenerateKey(ctx, in)
		require.NoError(t, err, name)
		require.True(t, strings.HasPrefix(k.PublicKey.ID, "K"))
		require.Contains(t, k.PrivateKeyPEM, "PRIVATE KEY")
		keys[name] = k
	}
	g, err := svc.GetKeyGroup(ctx, "rilasci")
	require.NoError(t, err)
	require.Len(t, g.Keys, 3)
	for _, k := range g.Keys {
		assert.NotContains(t, k.PublicKeyPEM, "PRIVATE", "the private key is never stored")
	}

	now := time.Now()
	signer := distribution.NewSigner(distribution.SignerDeps{})
	sign := func(key string, in distribution.SignInput) string {
		in.KeyPairID, in.PrivateKeyPEM = keys[key].PublicKey.ID, keys[key].PrivateKeyPEM
		if in.Expires.IsZero() {
			in.Expires = now.Add(time.Hour)
		}
		u, err := signer.SignURL(ctx, in)
		require.NoError(t, err)
		return u
	}
	base := "https://www.example.com/downloads/v1.zip"
	canned := sign("ed25519", distribution.SignInput{URL: base})
	assert.Contains(t, canned, "Expires=")
	assert.Contains(t, canned, "Key-Pair-Id="+keys["ed25519"].PublicKey.ID)

	tests := []struct {
		name    string
		req     distribution.SignedRequest
		wantErr error
	}{
		{name: "canned ed25519", req: signedRequest(t, "rilasci", canned, "1.2.3.4")},
		{name: "canned rsa-2048", req: signedRequest(t, "rilasci", sign("rsa-2048", distribution.SignInput{URL: base}), "1.2.3.4")},
		{name: "canned rsa-4096", req: signedRequest(t, "rilasci", sign("rsa-4096", distribution.SignInput{URL: base}), "1.2.3.4")},
		{name: "query kept and signed", req: signedRequest(t, "rilasci", sign("ed25519", distribution.SignInput{URL: base + "?v=2&lang=it"}), "")},
		{name: "query changed", wantErr: domain.ErrAccessDenied, req: signedRequest(t, "rilasci",
			strings.Replace(sign("ed25519", distribution.SignInput{URL: base + "?v=2"}), "v=2", "v=3", 1), "")},
		{name: "expired", wantErr: distribution.ErrSignatureExpired, req: func() distribution.SignedRequest {
			r := signedRequest(t, "rilasci", canned, "")
			r.Now = now.Add(2 * time.Hour)
			return r
		}()},
		{name: "path changed", wantErr: domain.ErrAccessDenied, req: signedRequest(t, "rilasci", strings.Replace(canned, "v1.zip", "v2.zip", 1), "")},
		{name: "host changed", wantErr: domain.ErrAccessDenied, req: signedRequest(t, "rilasci", strings.Replace(canned, "www.example.com", "evil.example.com", 1), "")},
		{name: "signature altered", wantErr: distribution.ErrBadSignature, req: func() distribution.SignedRequest {
			u, _ := url.Parse(canned)
			q := u.Query()
			sig := []byte(q.Get("Signature"))
			sig[3] ^= 1
			q.Set("Signature", string(sig))
			u.RawQuery = q.Encode()
			return signedRequest(t, "rilasci", u.String(), "")
		}()},
		{name: "expiration extended", wantErr: distribution.ErrBadSignature, req: func() distribution.SignedRequest {
			u, _ := url.Parse(canned)
			q := u.Query()
			q.Set("Expires", "9999999999")
			u.RawQuery = q.Encode()
			return signedRequest(t, "rilasci", u.String(), "")
		}()},
		{name: "key of another group", wantErr: distribution.ErrBadSignature,
			req: signedRequest(t, "rilasci", sign("other", distribution.SignInput{URL: base}), "")},
		{name: "unsigned", wantErr: distribution.ErrMissingSignature, req: signedRequest(t, "rilasci", base, "")},
		{name: "custom wildcard resource", req: signedRequest(t, "rilasci",
			sign("ed25519", distribution.SignInput{URL: "https://www.example.com/downloads/v2/setup.iso", Resource: "https://www.example.com/downloads/*"}), "")},
		{name: "custom wildcard reused on another file", req: func() distribution.SignedRequest {
			s := sign("rsa-2048", distribution.SignInput{URL: "https://www.example.com/downloads/a.zip", Resource: "https://www.example.com/downloads/*"})
			return signedRequest(t, "rilasci", strings.Replace(s, "a.zip", "b/c.zip", 1), "")
		}()},
		{name: "custom wildcard outside", wantErr: domain.ErrAccessDenied, req: func() distribution.SignedRequest {
			s := sign("ed25519", distribution.SignInput{URL: "https://www.example.com/downloads/a.zip", Resource: "https://www.example.com/downloads/*"})
			return signedRequest(t, "rilasci", strings.Replace(s, "/downloads/a.zip", "/private/a.zip", 1), "")
		}()},
		{name: "custom IP inside", req: signedRequest(t, "rilasci",
			sign("ed25519", distribution.SignInput{URL: base, IPRange: "192.0.2.0/24"}), "192.0.2.77")},
		{name: "custom IP outside", wantErr: domain.ErrAccessDenied, req: signedRequest(t, "rilasci",
			sign("ed25519", distribution.SignInput{URL: base, IPRange: "192.0.2.0/24"}), "198.51.100.1")},
		{name: "custom not yet valid", wantErr: distribution.ErrSignatureExpired, req: signedRequest(t, "rilasci",
			sign("ed25519", distribution.SignInput{URL: base, NotBefore: now.Add(30 * time.Minute)}), "")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := svc.VerifySignedRequest(ctx, tc.req)
			if tc.wantErr == nil {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, tc.wantErr)
			assert.ErrorIs(t, err, domain.ErrAccessDenied, "403")
		})
	}

	t.Run("cookies", func(t *testing.T) {
		in := distribution.SignInput{URL: "https://www.example.com/premium/x", Resource: "https://www.example.com/premium/*",
			KeyPairID: keys["rsa-2048"].PublicKey.ID, PrivateKeyPEM: keys["rsa-2048"].PrivateKeyPEM, Expires: now.Add(time.Hour)}
		cookies, err := signer.SignCookies(ctx, in)
		require.NoError(t, err)
		jar := map[string]string{}
		for _, c := range cookies {
			jar[c.Name] = c.Value
		}
		require.Contains(t, jar, distribution.CookiePolicy)
		req := distribution.SignedRequest{KeyGroup: "rilasci", Scheme: "https", Host: "www.example.com", EscapedPath: "/premium/video/1.mp4", Cookies: jar}
		require.NoError(t, svc.VerifySignedRequest(ctx, req))
		req.EscapedPath = "/other/1.mp4"
		require.ErrorIs(t, svc.VerifySignedRequest(ctx, req), domain.ErrAccessDenied)
		req.EscapedPath = "/premium/video/1.mp4"
		req.Now = now.Add(2 * time.Hour)
		require.ErrorIs(t, svc.VerifySignedRequest(ctx, req), distribution.ErrSignatureExpired)

		canned, err := signer.SignCookies(ctx, distribution.SignInput{URL: "https://www.example.com/premium/x",
			KeyPairID: keys["ed25519"].PublicKey.ID, PrivateKeyPEM: keys["ed25519"].PrivateKeyPEM, Expires: now.Add(time.Hour)})
		require.NoError(t, err)
		jar = map[string]string{}
		for _, c := range canned {
			jar[c.Name] = c.Value
		}
		require.Contains(t, jar, distribution.CookieExpires)
		require.NoError(t, svc.VerifySignedRequest(ctx, distribution.SignedRequest{KeyGroup: "rilasci", Scheme: "https",
			Host: "www.example.com", EscapedPath: "/premium/x", Cookies: jar}))
	})

	t.Run("rotation", func(t *testing.T) {
		// A URL signed with the old key keeps working until the key is
		// removed; the new key works at once.
		oldURL := sign("ed25519", distribution.SignInput{URL: base})
		newURL := sign("rsa-2048", distribution.SignInput{URL: base})
		require.NoError(t, svc.VerifySignedRequest(ctx, signedRequest(t, "rilasci", oldURL, "")))
		require.NoError(t, svc.DeleteKey(ctx, "rilasci", keys["ed25519"].PublicKey.ID))
		require.ErrorIs(t, svc.VerifySignedRequest(ctx, signedRequest(t, "rilasci", oldURL, "")), distribution.ErrBadSignature)
		require.NoError(t, svc.VerifySignedRequest(ctx, signedRequest(t, "rilasci", newURL, "")))
	})

	t.Run("uploaded public key", func(t *testing.T) {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		require.NoError(t, err)
		pubDER, err := x509.MarshalPKIXPublicKey(pub)
		require.NoError(t, err)
		privDER, err := x509.MarshalPKCS8PrivateKey(priv)
		require.NoError(t, err)
		k, err := svc.AddPublicKey(ctx, distribution.AddKeyInput{KeyGroup: "rilasci",
			PEM: string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))})
		require.NoError(t, err)
		u, err := signer.SignURL(ctx, distribution.SignInput{URL: base, KeyPairID: k.ID, Expires: now.Add(time.Minute),
			PrivateKeyPEM: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER}))})
		require.NoError(t, err)
		require.NoError(t, svc.VerifySignedRequest(ctx, signedRequest(t, "rilasci", u, "")))
		_, err = svc.AddPublicKey(ctx, distribution.AddKeyInput{KeyGroup: "rilasci", PEM: "not a key"})
		require.ErrorIs(t, err, domain.ErrInvalidArgument)
	})

	t.Run("key groups in use", func(t *testing.T) {
		d := sample("dl", "dl.example.com")
		d.DefaultBehavior.Access = distribution.AccessSigned
		d.DefaultBehavior.Signing = &distribution.Signing{KeyGroup: "rilasci"}
		_, err := svc.Create(ctx, d)
		require.NoError(t, err)
		require.ErrorIs(t, svc.DeleteKeyGroup(ctx, "rilasci"), domain.ErrConflict)
		require.NoError(t, svc.DeleteKeyGroup(ctx, "other"))
		_, err = svc.GetKeyGroup(ctx, "other")
		require.ErrorIs(t, err, distribution.ErrKeyGroupNotFound)
	})
}

func TestSignerRefusals(t *testing.T) {
	ctx := context.Background()
	fake := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	signer := distribution.NewSigner(distribution.SignerDeps{Clock: fake})
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	require.NoError(t, err)
	key := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	for name, in := range map[string]distribution.SignInput{
		"past expiration":   {URL: "https://a.example.com/x", KeyPairID: "K1", PrivateKeyPEM: key, Expires: fake.Now().Add(-time.Second)},
		"no key pair id":    {URL: "https://a.example.com/x", PrivateKeyPEM: key, Expires: fake.Now().Add(time.Hour)},
		"not a URL":         {URL: "a.example.com/x", KeyPairID: "K1", PrivateKeyPEM: key, Expires: fake.Now().Add(time.Hour)},
		"already signed":    {URL: "https://a.example.com/x?Signature=x", KeyPairID: "K1", PrivateKeyPEM: key, Expires: fake.Now().Add(time.Hour)},
		"bad private key":   {URL: "https://a.example.com/x", KeyPairID: "K1", PrivateKeyPEM: "x", Expires: fake.Now().Add(time.Hour)},
		"bad IP range":      {URL: "https://a.example.com/x", KeyPairID: "K1", PrivateKeyPEM: key, Expires: fake.Now().Add(time.Hour), IPRange: "x"},
		"public key as key": {URL: "https://a.example.com/x", KeyPairID: "K1", PrivateKeyPEM: "-----BEGIN PUBLIC KEY-----\nAA==\n-----END PUBLIC KEY-----", Expires: fake.Now().Add(time.Hour)},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := signer.SignURL(ctx, in)
			require.ErrorIs(t, err, domain.ErrInvalidArgument)
		})
	}
}

// FuzzVerifySignedRequest checks that forged signature parameters never
// panic nor verify (TDS-14).
func FuzzVerifySignedRequest(f *testing.F) {
	ctx := context.Background()
	svc := newService(f)
	if _, err := svc.CreateKeyGroup(ctx, "g", ""); err != nil {
		f.Fatal(err)
	}
	k, err := svc.GenerateKey(ctx, distribution.GenerateKeyInput{KeyGroup: "g"})
	if err != nil {
		f.Fatal(err)
	}
	signed, err := distribution.NewSigner(distribution.SignerDeps{}).SignURL(ctx, distribution.SignInput{
		URL: "https://www.example.com/a", KeyPairID: k.PublicKey.ID, PrivateKeyPEM: k.PrivateKeyPEM, Expires: time.Now().Add(time.Hour),
	})
	if err != nil {
		f.Fatal(err)
	}
	u, _ := url.Parse(signed)
	f.Add(u.RawQuery, "/a", "")
	f.Add("Policy=eyJTdGF0ZW1lbnQiOltdfQ__&Signature=AA__&Key-Pair-Id="+k.PublicKey.ID, "/a", "")
	f.Add("Expires=-1&Signature=~~~&Key-Pair-Id=x", "/b", "CloudFront-Signature=x; CloudFront-Key-Pair-Id=y")
	f.Fuzz(func(t *testing.T, rawQuery, path, cookie string) {
		jar := map[string]string{}
		for _, part := range strings.Split(cookie, ";") {
			if name, value, ok := strings.Cut(strings.TrimSpace(part), "="); ok {
				jar[name] = value
			}
		}
		err := svc.VerifySignedRequest(ctx, distribution.SignedRequest{KeyGroup: "g", Scheme: "https", Host: "www.example.com",
			EscapedPath: path, RawQuery: rawQuery, Cookies: jar})
		if err == nil && (path != "/a" || rawQuery != u.RawQuery) {
			// Only the genuine URL (with its parameters reordered or
			// re-encoded at most) may verify.
			q, perr := url.ParseQuery(rawQuery)
			if perr != nil || path != "/a" || q.Get("Signature") != u.Query().Get("Signature") {
				t.Fatalf("forged request verified: path %q query %q cookie %q", path, rawQuery, cookie)
			}
		}
	})
}
