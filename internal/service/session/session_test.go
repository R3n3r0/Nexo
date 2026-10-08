package session_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	iammock "github.com/r3n3r0/nexo/internal/service/iam/mock"
	"github.com/r3n3r0/nexo/internal/service/session"
)

// directory is the IAM state behind the mock: credentials and identities
// by access key, mutable by the tests.
type directory struct {
	users      map[string]domain.Credentials
	identities map[string]domain.Identity
	err        error
}

func (d *directory) lookup(_ context.Context, accessKey string) (domain.Credentials, domain.Identity, error) {
	if d.err != nil {
		return domain.Credentials{}, domain.Identity{}, d.err
	}
	c, ok := d.users[accessKey]
	if !ok {
		return domain.Credentials{}, domain.Identity{}, fmt.Errorf("%w: %s", domain.ErrInvalidAccessKey, accessKey)
	}
	id, ok := d.identities[accessKey]
	if !ok {
		id = domain.Identity{AccessKey: accessKey, IsRoot: accessKey == "root"}
	}
	return c, id, nil
}

func newTestService(t *testing.T, clk clock.Clock, opts ...session.Option) (session.Service, *directory) {
	t.Helper()
	d := &directory{users: map[string]domain.Credentials{
		"root":     {AccessKey: "root", SecretKey: "rootsecret", Status: domain.StatusEnabled},
		"disabled": {AccessKey: "disabled", SecretKey: "secret", Status: domain.StatusDisabled},
		"expired":  {AccessKey: "expired", SecretKey: "secret", Expiration: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)},
	}, identities: map[string]domain.Identity{}}
	users := iammock.NewService(t)
	users.EXPECT().LookupCredentials(mock.Anything, mock.Anything).RunAndReturn(d.lookup).Maybe()
	opts = append([]session.Option{session.WithSigningKey([]byte("0123456789abcdef0123456789abcdef"))}, opts...)
	svc, err := session.New(session.Deps{IAM: users, Clock: clk}, opts...)
	require.NoError(t, err)
	return svc, d
}

func TestNewRequiresIAM(t *testing.T) {
	_, err := session.New(session.Deps{})
	assert.Error(t, err)
	svc, err := session.New(session.Deps{IAM: iammock.NewService(t)})
	require.NoError(t, err)
	assert.NotNil(t, svc)
}

func TestLoginAndValidate(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC))
	svc, _ := newTestService(t, clk, session.WithTTL(time.Hour))

	sess, err := svc.Login(ctx, "root", "rootsecret")
	require.NoError(t, err)
	assert.NotEmpty(t, sess.Token)
	assert.NotEmpty(t, sess.CSRFToken)
	assert.True(t, sess.Identity.IsRoot)
	assert.Equal(t, clk.Now().Add(time.Hour), sess.ExpiresAt)

	got, err := svc.Validate(ctx, sess.Token)
	require.NoError(t, err)
	assert.Equal(t, sess.ID, got.ID)
	assert.Equal(t, "root", got.AccessKey)
	assert.True(t, got.Identity.IsRoot)
	assert.Equal(t, sess.CSRFToken, got.CSRFToken)
	assert.Empty(t, got.Token, "Validate must not re-issue the token")

	require.NoError(t, svc.ValidateCSRF(ctx, got, sess.CSRFToken))
	assert.ErrorIs(t, svc.ValidateCSRF(ctx, got, "wrong"), domain.ErrAccessDenied)
	assert.ErrorIs(t, svc.ValidateCSRF(ctx, got, ""), domain.ErrAccessDenied)
	assert.ErrorIs(t, svc.ValidateCSRF(ctx, nil, sess.CSRFToken), domain.ErrAccessDenied)
}

func TestValidateResolvesIdentityThroughIAM(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC))
	svc, d := newTestService(t, clk, session.WithTTL(time.Hour))
	d.users["bob"] = domain.Credentials{AccessKey: "bob", SecretKey: "bobsecret", Status: domain.StatusEnabled}
	d.identities["bob"] = domain.Identity{AccessKey: "bob", Groups: []string{"dev"}, PolicyNames: []string{"readonly"}}

	sess, err := svc.Login(ctx, "bob", "bobsecret")
	require.NoError(t, err)
	assert.Equal(t, []string{"readonly"}, sess.Identity.PolicyNames)

	// Permissions granted after login are visible on the next validation.
	d.identities["bob"] = domain.Identity{AccessKey: "bob", Groups: []string{"dev", "ops"}, PolicyNames: []string{"readonly", "readwrite"}}
	got, err := svc.Validate(ctx, sess.Token)
	require.NoError(t, err)
	assert.Equal(t, []string{"dev", "ops"}, got.Identity.Groups)
	assert.Equal(t, []string{"readonly", "readwrite"}, got.Identity.PolicyNames)
	assert.False(t, got.Identity.IsRoot)

	// Disabling or deleting the account ends the session.
	d.users["bob"] = domain.Credentials{AccessKey: "bob", SecretKey: "bobsecret", Status: domain.StatusDisabled}
	_, err = svc.Validate(ctx, sess.Token)
	assert.ErrorIs(t, err, domain.ErrAccessDenied)
	delete(d.users, "bob")
	_, err = svc.Validate(ctx, sess.Token)
	assert.ErrorIs(t, err, domain.ErrAccessDenied)

	// Infrastructure errors are not access denied.
	d.err = errors.New("store down")
	_, err = svc.Validate(ctx, sess.Token)
	require.Error(t, err)
	assert.NotErrorIs(t, err, domain.ErrAccessDenied)
}

func TestLoginFailures(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC))
	svc, d := newTestService(t, clk)

	for name, tc := range map[string][2]string{
		"wrong secret": {"root", "nope"},
		"unknown user": {"ghost", "x"},
		"disabled":     {"disabled", "secret"},
		"expired":      {"expired", "secret"},
		"empty":        {"", ""},
	} {
		_, err := svc.Login(ctx, tc[0], tc[1])
		assert.ErrorIs(t, err, domain.ErrAccessDenied, name)
	}

	d.err = errors.New("store down")
	_, err := svc.Login(ctx, "root", "rootsecret")
	require.Error(t, err)
	assert.NotErrorIs(t, err, domain.ErrAccessDenied, "infrastructure errors are not access denied")
}

func TestValidateRejectsExpiredTamperedAndRevoked(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC))
	svc, _ := newTestService(t, clk, session.WithTTL(time.Hour))
	sess, err := svc.Login(ctx, "root", "rootsecret")
	require.NoError(t, err)

	_, err = svc.Validate(ctx, sess.Token+"x")
	assert.ErrorIs(t, err, domain.ErrAccessDenied)
	_, err = svc.Validate(ctx, "")
	assert.ErrorIs(t, err, domain.ErrAccessDenied)

	// A token signed with another key is rejected.
	other, _ := newTestService(t, clk, session.WithSigningKey([]byte("another-key-another-key-another!")))
	otherSess, err := other.Login(ctx, "root", "rootsecret")
	require.NoError(t, err)
	_, err = svc.Validate(ctx, otherSess.Token)
	assert.ErrorIs(t, err, domain.ErrAccessDenied)

	// Revocation.
	require.NoError(t, svc.Logout(ctx, sess.Token))
	_, err = svc.Validate(ctx, sess.Token)
	assert.ErrorIs(t, err, domain.ErrAccessDenied)
	require.NoError(t, svc.Logout(ctx, "garbage"), "logout of invalid tokens is a no-op")

	// Expiry.
	sess2, err := svc.Login(ctx, "root", "rootsecret")
	require.NoError(t, err)
	clk.Advance(time.Hour + time.Second)
	_, err = svc.Validate(ctx, sess2.Token)
	assert.ErrorIs(t, err, domain.ErrAccessDenied)
}

func TestDeriveKey(t *testing.T) {
	k1 := session.DeriveKey("admin", "password123")
	k2 := session.DeriveKey("admin", "password123")
	k3 := session.DeriveKey("admin", "other")
	assert.Len(t, k1, 32)
	assert.Equal(t, k1, k2, "deterministic: every node accepts the same cookies")
	assert.NotEqual(t, k1, k3)
}
