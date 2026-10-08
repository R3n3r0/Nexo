package session

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/iam"
)

// fakeIAM is a hand-written iam.Service mock: only LookupCredentials is
// implemented, the embedded nil interface panics on anything else.
type fakeIAM struct {
	iam.Service
	users map[string]domain.Credentials
	err   error
}

func (f *fakeIAM) LookupCredentials(_ context.Context, accessKey string) (domain.Credentials, domain.Identity, error) {
	if f.err != nil {
		return domain.Credentials{}, domain.Identity{}, f.err
	}
	c, ok := f.users[accessKey]
	if !ok {
		return domain.Credentials{}, domain.Identity{}, fmt.Errorf("%w: %s", domain.ErrInvalidAccessKey, accessKey)
	}
	return c, domain.Identity{AccessKey: accessKey, IsRoot: accessKey == "root"}, nil
}

func newTestService(t *testing.T, clk clock.Clock, opts ...Option) (Service, *fakeIAM) {
	t.Helper()
	f := &fakeIAM{users: map[string]domain.Credentials{
		"root":     {AccessKey: "root", SecretKey: "rootsecret", Status: domain.StatusEnabled},
		"disabled": {AccessKey: "disabled", SecretKey: "secret", Status: domain.StatusDisabled},
		"expired":  {AccessKey: "expired", SecretKey: "secret", Expiration: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)},
	}}
	opts = append([]Option{WithSigningKey([]byte("0123456789abcdef0123456789abcdef"))}, opts...)
	svc, err := New(Deps{IAM: f, Clock: clk}, opts...)
	require.NoError(t, err)
	return svc, f
}

func TestNewRequiresIAM(t *testing.T) {
	_, err := New(Deps{})
	assert.Error(t, err)
	svc, err := New(Deps{IAM: &fakeIAM{}})
	require.NoError(t, err)
	assert.NotNil(t, svc)
}

func TestLoginAndValidate(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC))
	svc, _ := newTestService(t, clk, WithTTL(time.Hour))

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

func TestLoginFailures(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC))
	svc, f := newTestService(t, clk)

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

	f.err = fmt.Errorf("store down")
	_, err := svc.Login(ctx, "root", "rootsecret")
	require.Error(t, err)
	assert.NotErrorIs(t, err, domain.ErrAccessDenied, "infrastructure errors are not access denied")
}

func TestValidateRejectsExpiredTamperedAndRevoked(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC))
	svc, _ := newTestService(t, clk, WithTTL(time.Hour))
	sess, err := svc.Login(ctx, "root", "rootsecret")
	require.NoError(t, err)

	_, err = svc.Validate(ctx, sess.Token+"x")
	assert.ErrorIs(t, err, domain.ErrAccessDenied)
	_, err = svc.Validate(ctx, "")
	assert.ErrorIs(t, err, domain.ErrAccessDenied)

	// A token signed with another key is rejected.
	other, _ := newTestService(t, clk, WithSigningKey([]byte("another-key-another-key-another!")))
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
