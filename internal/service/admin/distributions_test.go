package admin_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/distribution"
	distmock "github.com/r3n3r0/nexo/internal/service/distribution/mock"
	"github.com/r3n3r0/nexo/internal/service/policy"
)

// keyPair returns an Ed25519 key pair in PEM: PKCS #8 private key, PKIX
// public key.
func keyPair(t *testing.T) (privatePEM, publicPEM string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	privDER, err := x509.MarshalPKCS8PrivateKey(priv)
	require.NoError(t, err)
	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}))
}

type distEnv struct {
	dists  *distmock.Service
	signer *distmock.Signer
	mgr    admin.DistributionManager
}

// distNow is the time of the clock of the distribution manager.
var distNow = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

func newDistEnv(t *testing.T, authz admin.Authorizer) *distEnv {
	e := &distEnv{dists: distmock.NewService(t), signer: distmock.NewSigner(t)}
	m, err := admin.NewDistributionManager(admin.DistributionManagerDeps{Distributions: e.dists, Signer: e.signer, Authz: authz, Clock: clock.NewFake(distNow)})
	require.NoError(t, err)
	e.mgr = m
	return e
}

// site is the distribution of the tests: two exact aliases and a
// wildcard, a signed behavior under /downloads/.
func site() distribution.Distribution {
	return distribution.Distribution{
		ID: "site", Enabled: true, Aliases: []string{"*.preview.example.com", "www.example.com", "example.com"},
		Origin: distribution.Origin{Bucket: "web"}, Mode: distribution.ModeWebsite,
	}
}

// resolving makes the mock resolve the aliases of d to d.
func (e *distEnv) resolving(d distribution.Distribution) {
	e.dists.EXPECT().Resolve(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, host string) distribution.Route {
		switch host {
		case "www.example.com", "example.com", "www.example.com:9000", "a.preview.example.com":
			return distribution.Route{Kind: distribution.RouteDistribution, Host: host, Distribution: &d, Bucket: d.Origin.Bucket}
		case "other.example.com":
			other := distribution.Distribution{ID: "other"}
			return distribution.Route{Kind: distribution.RouteDistribution, Host: host, Distribution: &other}
		}
		return distribution.Route{Kind: distribution.RoutePathStyle, Host: host}
	}).Maybe()
}

func TestNewDistributionManagerRequiresDeps(t *testing.T) {
	_, err := admin.NewDistributionManager(admin.DistributionManagerDeps{})
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
}

func TestDistributionManagerAuthorizesOrigins(t *testing.T) {
	ctx := context.Background()
	denyBucket := func(bucket string) func(in admin.AuthorizeInput) bool {
		return func(in admin.AuthorizeInput) bool {
			return in.Bucket != bucket && in.Actions[0] == policy.ActionAdminManageDistributions
		}
	}
	t.Run("create on an allowed bucket", func(t *testing.T) {
		e := newDistEnv(t, allowing(t, denyBucket("secret")))
		d := site()
		e.dists.EXPECT().List(mock.Anything).Return(nil, nil)
		e.dists.EXPECT().Create(mock.Anything, d).Return(d, nil)
		_, err := e.mgr.Create(ctx, admin.SaveDistributionInput{Requester: requesterOf("op"), Distribution: d})
		require.NoError(t, err)
	})
	t.Run("create on a denied bucket", func(t *testing.T) {
		e := newDistEnv(t, allowing(t, denyBucket("web")))
		_, err := e.mgr.Create(ctx, admin.SaveDistributionInput{Requester: requesterOf("op"), Distribution: site()})
		require.ErrorIs(t, err, domain.ErrAccessDenied)
	})
	t.Run("update moving to a denied bucket", func(t *testing.T) {
		e := newDistEnv(t, allowing(t, denyBucket("secret")))
		d := site()
		e.dists.EXPECT().Get(mock.Anything, "site").Return(d, nil)
		moved := site()
		moved.Origin.Bucket = "secret"
		_, err := e.mgr.Update(ctx, admin.SaveDistributionInput{Requester: requesterOf("op"), Distribution: moved})
		require.ErrorIs(t, err, domain.ErrAccessDenied)
	})
	t.Run("update of a distribution of a denied bucket", func(t *testing.T) {
		e := newDistEnv(t, allowing(t, denyBucket("secret")))
		current := site()
		current.Origin.Bucket = "secret"
		e.dists.EXPECT().Get(mock.Anything, "site").Return(current, nil)
		_, err := e.mgr.Update(ctx, admin.SaveDistributionInput{Requester: requesterOf("op"), Distribution: site()})
		require.ErrorIs(t, err, domain.ErrAccessDenied)
	})
	t.Run("update with If-Match", func(t *testing.T) {
		e := newDistEnv(t, allowAll(t))
		d := site()
		e.dists.EXPECT().Get(mock.Anything, "site").Return(d, nil)
		e.dists.EXPECT().List(mock.Anything).Return([]distribution.Distribution{d}, nil)
		e.dists.EXPECT().Update(mock.Anything, d, `"e1"`).Return(d, nil)
		_, err := e.mgr.Update(ctx, admin.SaveDistributionInput{Requester: requesterOf("op"), Distribution: d, IfMatch: `"e1"`})
		require.NoError(t, err)
	})
	// The mutations that do not save a distribution are authorized on its
	// origin bucket too: a user denied on a bucket cannot remove its
	// distribution (and reuse its aliases), rotate its origin secret or
	// change the keys that sign its URLs.
	t.Run("delete, rotate and keys of a distribution of a denied bucket", func(t *testing.T) {
		e := newDistEnv(t, allowing(t, denyBucket("secret")))
		victim := site()
		victim.ID, victim.Origin.Bucket = "victim", "secret"
		victim.Behaviors = []distribution.Behavior{{Access: distribution.AccessSigned, Signing: &distribution.Signing{KeyGroup: "premium"}}}
		e.dists.EXPECT().Get(mock.Anything, "victim").Return(victim, nil)
		e.dists.EXPECT().List(mock.Anything).Return([]distribution.Distribution{site(), victim}, nil)
		op := requesterOf("op")
		require.ErrorIs(t, e.mgr.Delete(ctx, op, "victim"), domain.ErrAccessDenied)
		_, err := e.mgr.RotateOriginSecret(ctx, op, distribution.RotateSecretInput{ID: "victim", DropPrevious: true})
		require.ErrorIs(t, err, domain.ErrAccessDenied)
		_, err = e.mgr.AddKey(ctx, admin.AddKeyInput{Requester: op, KeyGroup: "premium", PublicKeyPEM: "PUB"})
		require.ErrorIs(t, err, domain.ErrAccessDenied)
		require.ErrorIs(t, e.mgr.DeleteKey(ctx, op, "premium", "K1"), domain.ErrAccessDenied)
		require.ErrorIs(t, e.mgr.DeleteKeyGroup(ctx, op, "premium"), domain.ErrAccessDenied)
		// A group no distribution of a denied bucket uses.
		e.dists.EXPECT().DeleteKey(mock.Anything, "other", "K1").Return(nil)
		require.NoError(t, e.mgr.DeleteKey(ctx, op, "other", "K1"))
	})
	t.Run("delete of a distribution of an allowed bucket", func(t *testing.T) {
		e := newDistEnv(t, allowing(t, denyBucket("secret")))
		e.dists.EXPECT().Get(mock.Anything, "site").Return(site(), nil)
		e.dists.EXPECT().Delete(mock.Anything, "site").Return(nil)
		require.NoError(t, e.mgr.Delete(ctx, requesterOf("op"), "site"))
	})
	// A more specific alias wins the routing: an exact host or a narrower
	// wildcard under the wildcard of another distribution takes those
	// hosts away from it, so it needs the authorization on that
	// distribution's bucket too (regression: a user allowed on his own
	// bucket only served his site on app.corp.test of the root's
	// *.corp.test).
	t.Run("aliases under the wildcard of another distribution", func(t *testing.T) {
		corp := distribution.Distribution{ID: "corp", Aliases: []string{"*.corp.test"}, Origin: distribution.Origin{Bucket: "secret"}}
		for _, aliases := range [][]string{{"app.corp.test"}, {"www.ok.test", "*.x.corp.test"}, {"A.B.CORP.TEST."}} {
			e := newDistEnv(t, allowing(t, denyBucket("secret")))
			e.dists.EXPECT().List(mock.Anything).Return([]distribution.Distribution{corp, site()}, nil)
			shadow := site()
			shadow.ID, shadow.Aliases = "shadow", aliases
			_, err := e.mgr.Create(ctx, admin.SaveDistributionInput{Requester: requesterOf("op"), Distribution: shadow})
			require.ErrorIs(t, err, domain.ErrAccessDenied, "%v", aliases)
			assert.ErrorContains(t, err, "*.corp.test of distribution corp")

			// The same through an update.
			e.dists.EXPECT().Get(mock.Anything, "shadow").Return(site(), nil).Once()
			_, err = e.mgr.Update(ctx, admin.SaveDistributionInput{Requester: requesterOf("op"), Distribution: shadow})
			require.ErrorIs(t, err, domain.ErrAccessDenied, "%v", aliases)
		}
		// Allowed on the wildcard's bucket too (its owner, root): saved.
		e := newDistEnv(t, allowAll(t))
		e.dists.EXPECT().List(mock.Anything).Return([]distribution.Distribution{corp}, nil)
		shadow := site()
		shadow.ID, shadow.Aliases = "shadow", []string{"app.corp.test"}
		e.dists.EXPECT().Create(mock.Anything, shadow).Return(shadow, nil)
		_, err := e.mgr.Create(ctx, admin.SaveDistributionInput{Requester: requesterOf("op"), Distribution: shadow})
		require.NoError(t, err)
		// The wildcard's own distribution and unrelated hosts need nothing more.
		e = newDistEnv(t, allowing(t, denyBucket("secret")))
		e.dists.EXPECT().List(mock.Anything).Return([]distribution.Distribution{corp}, nil)
		other := site()
		other.Aliases = []string{"corp.test", "app.corp.test.example"}
		e.dists.EXPECT().Create(mock.Anything, other).Return(other, nil)
		_, err = e.mgr.Create(ctx, admin.SaveDistributionInput{Requester: requesterOf("op"), Distribution: other})
		require.NoError(t, err)
	})
	t.Run("update of an unknown distribution", func(t *testing.T) {
		e := newDistEnv(t, allowAll(t))
		e.dists.EXPECT().Get(mock.Anything, "site").Return(distribution.Distribution{}, distribution.ErrDistributionNotFound)
		_, err := e.mgr.Update(ctx, admin.SaveDistributionInput{Distribution: site()})
		require.ErrorIs(t, err, domain.ErrNotFound)
	})
}

func TestDistributionManagerTestURL(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name    string
		url     string
		want    string
		wantErr error
	}{
		{name: "path on the first exact alias", url: "/assets/app.js", want: "https://www.example.com/assets/app.js"},
		{name: "absolute URL", url: "http://example.com/a?b=1", want: "http://example.com/a?b=1"},
		{name: "wildcard alias", url: "https://a.preview.example.com/", want: "https://a.preview.example.com/"},
		{name: "another distribution", url: "https://other.example.com/", wantErr: domain.ErrInvalidArgument},
		{name: "unknown host", url: "https://nowhere.test/", wantErr: domain.ErrInvalidArgument},
		{name: "not a URL", url: "ftp://www.example.com/", wantErr: domain.ErrInvalidArgument},
		{name: "empty", url: " ", wantErr: domain.ErrInvalidArgument},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newDistEnv(t, allowAll(t))
			d := site()
			e.dists.EXPECT().Get(mock.Anything, "site").Return(d, nil)
			e.resolving(d)
			if tc.wantErr == nil {
				e.dists.EXPECT().Test(mock.Anything, distribution.TestInput{URL: tc.want, Method: "HEAD", ClientIP: "1.2.3.4"}).
					Return(distribution.TestResult{BehaviorIndex: -1}, nil)
			}
			_, err := e.mgr.Test(ctx, admin.TestDistributionInput{ID: "site", URL: tc.url, Method: "HEAD", ClientIP: "1.2.3.4"})
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
	t.Run("path without exact alias", func(t *testing.T) {
		e := newDistEnv(t, allowAll(t))
		d := site()
		d.Aliases = []string{"*.preview.example.com"}
		e.dists.EXPECT().Get(mock.Anything, "site").Return(d, nil)
		_, err := e.mgr.Test(ctx, admin.TestDistributionInput{ID: "site", URL: "/x"})
		require.ErrorIs(t, err, domain.ErrInvalidArgument)
	})
}

func TestDistributionManagerSign(t *testing.T) {
	ctx := context.Background()
	privPEM, pubPEM := keyPair(t)
	otherPriv, otherPub := keyPair(t)
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	rsaPEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rsaKey)}))
	group := distribution.KeyGroup{ID: "rel", Keys: []distribution.PublicKey{
		{ID: "K1", Algorithm: distribution.KeyEd25519, PublicKeyPEM: otherPub},
		{ID: "K2", Algorithm: distribution.KeyEd25519, PublicKeyPEM: pubPEM},
	}}
	expires := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	signed := distribution.Behavior{Access: distribution.AccessSigned, Signing: &distribution.Signing{KeyGroup: "rel"}}
	tests := []struct {
		name     string
		in       admin.SignDistributionInput
		behavior distribution.Behavior
		wantURL  string
		wantKey  string
		wantErr  error
	}{
		{name: "key found from the private key", in: admin.SignDistributionInput{URL: "/downloads/v1.zip", Scheme: "http", PrivateKeyPEM: privPEM},
			behavior: signed, wantURL: "http://www.example.com/downloads/v1.zip", wantKey: "K2"},
		{name: "named key", in: admin.SignDistributionInput{URL: "/downloads/v1.zip", Host: "example.com", PrivateKeyPEM: privPEM, KeyPairID: "K2"},
			behavior: signed, wantURL: "https://example.com/downloads/v1.zip", wantKey: "K2"},
		{name: "other key of the group", in: admin.SignDistributionInput{URL: "https://www.example.com:9000/downloads/v1.zip", PrivateKeyPEM: otherPriv},
			behavior: signed, wantURL: "https://www.example.com:9000/downloads/v1.zip", wantKey: "K1"},
		{name: "named key of another private key", in: admin.SignDistributionInput{URL: "/downloads/v1.zip", PrivateKeyPEM: privPEM, KeyPairID: "K1"},
			behavior: signed, wantErr: domain.ErrInvalidArgument},
		{name: "unknown key", in: admin.SignDistributionInput{URL: "/downloads/v1.zip", PrivateKeyPEM: privPEM, KeyPairID: "K9"},
			behavior: signed, wantErr: domain.ErrInvalidArgument},
		{name: "key of no key of the group", in: admin.SignDistributionInput{URL: "/downloads/v1.zip", PrivateKeyPEM: rsaPEM},
			behavior: signed, wantErr: domain.ErrInvalidArgument},
		{name: "public behavior", in: admin.SignDistributionInput{URL: "/index.html", PrivateKeyPEM: privPEM},
			behavior: distribution.Behavior{Access: distribution.AccessPublic}, wantErr: domain.ErrInvalidArgument},
		{name: "not PEM", in: admin.SignDistributionInput{URL: "/downloads/v1.zip", PrivateKeyPEM: "nope"},
			behavior: signed, wantErr: domain.ErrInvalidArgument},
		{name: "public key instead of the private one", in: admin.SignDistributionInput{URL: "/downloads/v1.zip", PrivateKeyPEM: pubPEM},
			behavior: signed, wantErr: domain.ErrInvalidArgument},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newDistEnv(t, allowAll(t))
			d := site()
			e.dists.EXPECT().Get(mock.Anything, "site").Return(d, nil)
			e.resolving(d)
			e.dists.EXPECT().Test(mock.Anything, mock.Anything).Return(distribution.TestResult{Behavior: tc.behavior}, nil)
			e.dists.EXPECT().GetKeyGroup(mock.Anything, "rel").Return(group, nil).Maybe()
			if tc.wantErr == nil {
				e.signer.EXPECT().SignURL(mock.Anything, distribution.SignInput{
					URL: tc.wantURL, KeyPairID: tc.wantKey, PrivateKeyPEM: tc.in.PrivateKeyPEM, Expires: expires,
				}).Return(tc.wantURL+"?Expires=1&Signature=s&Key-Pair-Id="+tc.wantKey, nil)
			}
			in := tc.in
			in.ID, in.Expires = "site", expires
			got, err := e.mgr.Sign(ctx, in)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantURL+"?Expires=1&Signature=s&Key-Pair-Id="+tc.wantKey, got.URL)
			assert.Equal(t, "rel", got.KeyGroup)
			assert.Equal(t, tc.wantKey, got.KeyPairID)
			assert.Equal(t, expires, got.Expires)
		})
	}
	t.Run("expiration", func(t *testing.T) {
		e := newDistEnv(t, allowAll(t))
		_, err := e.mgr.Sign(ctx, admin.SignDistributionInput{ID: "site", URL: "/downloads/v1.zip", PrivateKeyPEM: privPEM})
		require.ErrorIs(t, err, domain.ErrInvalidArgument, "an expiration is required")
		_, err = e.mgr.Sign(ctx, admin.SignDistributionInput{ID: "site", URL: "/downloads/v1.zip", PrivateKeyPEM: privPEM, Expires: expires, ExpiresIn: time.Hour})
		require.ErrorIs(t, err, domain.ErrInvalidArgument, "not both")
		_, err = e.mgr.Sign(ctx, admin.SignDistributionInput{ID: "site", URL: "/downloads/v1.zip", PrivateKeyPEM: privPEM, ExpiresIn: -time.Hour})
		require.ErrorIs(t, err, domain.ErrInvalidArgument)

		d := site()
		e.dists.EXPECT().Get(mock.Anything, "site").Return(d, nil)
		e.resolving(d)
		e.dists.EXPECT().Test(mock.Anything, mock.Anything).Return(distribution.TestResult{Behavior: signed}, nil)
		e.dists.EXPECT().GetKeyGroup(mock.Anything, "rel").Return(group, nil)
		e.signer.EXPECT().SignURL(mock.Anything, mock.MatchedBy(func(in distribution.SignInput) bool {
			return in.Expires.Equal(distNow.Add(time.Hour))
		})).Return("signed", nil)
		got, err := e.mgr.Sign(ctx, admin.SignDistributionInput{ID: "site", URL: "/downloads/v1.zip", PrivateKeyPEM: privPEM, ExpiresIn: time.Hour})
		require.NoError(t, err)
		assert.Equal(t, distNow.Add(time.Hour), got.Expires)
	})
	t.Run("cookies", func(t *testing.T) {
		e := newDistEnv(t, allowAll(t))
		d := site()
		e.dists.EXPECT().Get(mock.Anything, "site").Return(d, nil)
		e.resolving(d)
		e.dists.EXPECT().Test(mock.Anything, mock.Anything).Return(distribution.TestResult{Behavior: signed}, nil)
		e.dists.EXPECT().GetKeyGroup(mock.Anything, "rel").Return(group, nil)
		cookies := []distribution.SignedCookie{{Name: distribution.CookiePolicy, Value: "p"}}
		e.signer.EXPECT().SignCookies(mock.Anything, mock.MatchedBy(func(in distribution.SignInput) bool {
			return in.Resource == "https://www.example.com/downloads/*" && in.IPRange == "10.0.0.0/8" && in.KeyPairID == "K2"
		})).Return(cookies, nil)
		got, err := e.mgr.Sign(ctx, admin.SignDistributionInput{ID: "site", URL: "/downloads/", PrivateKeyPEM: privPEM, Expires: expires,
			Resource: "https://www.example.com/downloads/*", IPRange: "10.0.0.0/8", Cookies: true})
		require.NoError(t, err)
		assert.Equal(t, cookies, got.Cookies)
		assert.Equal(t, "https://www.example.com/downloads/", got.URL, "the URL itself, unsigned")
	})
}

func TestDistributionManagerKeyGroups(t *testing.T) {
	ctx := context.Background()
	t.Run("create with a first key", func(t *testing.T) {
		e := newDistEnv(t, allowAll(t))
		g := distribution.KeyGroup{ID: "rel", Keys: []distribution.PublicKey{}}
		e.dists.EXPECT().CreateKeyGroup(mock.Anything, "rel", "releases").Return(g, nil)
		gen := distribution.GeneratedKey{PublicKey: distribution.PublicKey{ID: "K1"}, PrivateKeyPEM: "PRIV"}
		e.dists.EXPECT().GenerateKey(mock.Anything, distribution.GenerateKeyInput{KeyGroup: "rel", Algorithm: distribution.KeyRSA, Bits: 4096, Comment: "c"}).Return(gen, nil)
		withKey := distribution.KeyGroup{ID: "rel", Keys: []distribution.PublicKey{gen.PublicKey}}
		e.dists.EXPECT().GetKeyGroup(mock.Anything, "rel").Return(withKey, nil)
		got, err := e.mgr.CreateKeyGroup(ctx, admin.CreateKeyGroupInput{ID: "rel", Comment: "releases",
			Key: &admin.GenerateKeySpec{Algorithm: distribution.KeyRSA, Bits: 4096, Comment: "c"}})
		require.NoError(t, err)
		assert.Equal(t, withKey, got.KeyGroup)
		assert.Equal(t, &gen, got.Key)
	})
	t.Run("create rolled back when the key fails", func(t *testing.T) {
		e := newDistEnv(t, allowAll(t))
		e.dists.EXPECT().CreateKeyGroup(mock.Anything, "rel", "").Return(distribution.KeyGroup{ID: "rel"}, nil)
		e.dists.EXPECT().GenerateKey(mock.Anything, mock.Anything).Return(distribution.GeneratedKey{}, fmt.Errorf("%w: bits", distribution.ErrInvalid))
		e.dists.EXPECT().DeleteKeyGroup(mock.Anything, "rel").Return(nil)
		_, err := e.mgr.CreateKeyGroup(ctx, admin.CreateKeyGroupInput{ID: "rel", Key: &admin.GenerateKeySpec{Bits: 1}})
		require.ErrorIs(t, err, domain.ErrInvalidArgument)
	})
	t.Run("create without key", func(t *testing.T) {
		e := newDistEnv(t, allowAll(t))
		e.dists.EXPECT().CreateKeyGroup(mock.Anything, "rel", "").Return(distribution.KeyGroup{ID: "rel"}, nil)
		got, err := e.mgr.CreateKeyGroup(ctx, admin.CreateKeyGroupInput{ID: "rel"})
		require.NoError(t, err)
		assert.Nil(t, got.Key)
	})
	t.Run("add a public key", func(t *testing.T) {
		e := newDistEnv(t, allowAll(t))
		e.dists.EXPECT().List(mock.Anything).Return(nil, nil)
		e.dists.EXPECT().AddPublicKey(mock.Anything, distribution.AddKeyInput{KeyGroup: "rel", PEM: "PUB", Comment: "mine"}).Return(distribution.PublicKey{ID: "K2"}, nil)
		got, err := e.mgr.AddKey(ctx, admin.AddKeyInput{KeyGroup: "rel", PublicKeyPEM: "PUB", Comment: "mine"})
		require.NoError(t, err)
		assert.Equal(t, &admin.AddedKey{PublicKey: distribution.PublicKey{ID: "K2"}}, got)
		_, err = e.mgr.AddKey(ctx, admin.AddKeyInput{KeyGroup: "rel", PublicKeyPEM: "PUB", Algorithm: distribution.KeyRSA})
		require.ErrorIs(t, err, domain.ErrInvalidArgument)
	})
	t.Run("generate a key", func(t *testing.T) {
		e := newDistEnv(t, allowAll(t))
		e.dists.EXPECT().List(mock.Anything).Return(nil, nil)
		e.dists.EXPECT().GenerateKey(mock.Anything, distribution.GenerateKeyInput{KeyGroup: "rel"}).
			Return(distribution.GeneratedKey{PublicKey: distribution.PublicKey{ID: "K3"}, PrivateKeyPEM: "PRIV"}, nil)
		got, err := e.mgr.AddKey(ctx, admin.AddKeyInput{KeyGroup: "rel"})
		require.NoError(t, err)
		assert.Equal(t, "PRIV", got.PrivateKeyPEM)
	})
}

func TestDistributionManagerRotateAndResolve(t *testing.T) {
	ctx := context.Background()
	e := newDistEnv(t, allowAll(t))
	d := site()
	d.Restrictions.OriginSecretHeader = &distribution.OriginSecret{Name: "X-Proxy", Values: []string{"sha256:aa", "sha256:bb"}}
	e.dists.EXPECT().Get(mock.Anything, "site").Return(d, nil)
	e.dists.EXPECT().RotateOriginSecret(mock.Anything, distribution.RotateSecretInput{ID: "site", Keep: 2}).Return("s3cr3t", d, nil)
	got, err := e.mgr.RotateOriginSecret(ctx, requesterOf("op"), distribution.RotateSecretInput{ID: "site", Keep: 2})
	require.NoError(t, err)
	assert.Equal(t, &admin.RotatedOriginSecret{Secret: "s3cr3t", Header: "X-Proxy", Distribution: d}, got)

	e.resolving(d)
	route, err := e.mgr.Resolve(ctx, "www.example.com")
	require.NoError(t, err)
	assert.Equal(t, "site", route.Distribution.ID)
	_, err = e.mgr.Resolve(ctx, " ")
	require.ErrorIs(t, err, domain.ErrInvalidArgument)

	e.dists.EXPECT().Get(mock.Anything, "nope").Return(distribution.Distribution{}, distribution.ErrDistributionNotFound).Once()
	_, err = e.mgr.RotateOriginSecret(ctx, requesterOf("op"), distribution.RotateSecretInput{ID: "nope"})
	require.True(t, errors.Is(err, domain.ErrNotFound))
}
