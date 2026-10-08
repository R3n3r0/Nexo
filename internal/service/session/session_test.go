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
		"sts":      {AccessKey: "sts", SecretKey: "secret", SessionToken: "token", Expiration: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)},
	}, identities: map[string]domain.Identity{}}
	users := iammock.NewService(t)
	users.EXPECT().LookupCredentials(mock.Anything, mock.Anything).RunAndReturn(d.lookup).Maybe()
	opts = append([]session.Option{session.WithSigningKey([]byte("0123456789abcdef0123456789abcdef"))}, opts...)
	svc, err := session.New(session.Deps{IAM: users, Clock: clk}, opts...)
	require.NoError(t, err)
	return svc, d
}

func login(accessKey, secretKey string) session.LoginInput {
	return session.LoginInput{AccessKey: accessKey, SecretKey: secretKey, Client: "192.0.2.1"}
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

	sess, err := svc.Login(ctx, login("root", "rootsecret"))
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

	sess, err := svc.Login(ctx, login("bob", "bobsecret"))
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
		"temporary":    {"sts", "secret"},
		"empty":        {"", ""},
	} {
		_, err := svc.Login(ctx, login(tc[0], tc[1]))
		assert.ErrorIs(t, err, domain.ErrAccessDenied, name)
	}

	d.err = errors.New("store down")
	_, err := svc.Login(ctx, login("root", "rootsecret"))
	require.Error(t, err)
	assert.NotErrorIs(t, err, domain.ErrAccessDenied, "infrastructure errors are not access denied")
}

func TestLoginThrottle(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC))
	svc, _ := newTestService(t, clk, session.WithLoginThrottle(3, time.Minute, 30*time.Second))
	from := func(client, accessKey, secretKey string) error {
		_, err := svc.Login(ctx, session.LoginInput{AccessKey: accessKey, SecretKey: secretKey, Client: client})
		return err
	}

	// The failures under the limit are plain denials.
	for range 3 {
		err := from("192.0.2.1", "root", "nope")
		require.ErrorIs(t, err, domain.ErrAccessDenied)
		assert.NotErrorIs(t, err, domain.ErrTooManyRequests)
	}
	// Then the pair is locked out, even with the right secret.
	err := from("192.0.2.1", "root", "rootsecret")
	require.ErrorIs(t, err, domain.ErrTooManyRequests)
	var throttled *session.ThrottledError
	require.ErrorAs(t, err, &throttled)
	assert.Equal(t, 30*time.Second, throttled.RetryAfter)
	assert.NotErrorIs(t, err, domain.ErrAccessDenied)

	// Other clients and other access keys are not affected.
	require.NoError(t, from("192.0.2.2", "root", "rootsecret"))
	require.ErrorIs(t, from("192.0.2.1", "ghost", "x"), domain.ErrAccessDenied)

	// The lockout ends; a success clears the failures.
	clk.Advance(10 * time.Second)
	err = from("192.0.2.1", "root", "rootsecret")
	require.ErrorAs(t, err, &throttled)
	assert.Equal(t, 20*time.Second, throttled.RetryAfter)
	clk.Advance(21 * time.Second)
	require.NoError(t, from("192.0.2.1", "root", "rootsecret"))
	for range 2 {
		require.ErrorIs(t, from("192.0.2.1", "root", "nope"), domain.ErrAccessDenied)
	}
	require.NoError(t, from("192.0.2.1", "root", "rootsecret"), "the counter restarted after the success")

	// Failures spread beyond the window never lock out.
	for range 5 {
		require.ErrorIs(t, from("192.0.2.3", "root", "nope"), domain.ErrAccessDenied)
		clk.Advance(31 * time.Second)
	}
	require.NoError(t, from("192.0.2.3", "root", "rootsecret"))

	// A disabled throttle never refuses.
	open, _ := newTestService(t, clk, session.WithLoginThrottle(0, time.Minute, time.Minute))
	for range 10 {
		_, err := open.Login(ctx, login("root", "nope"))
		require.ErrorIs(t, err, domain.ErrAccessDenied)
	}
}

// TestLoginThrottlePerClient: a client spraying passwords over many
// access keys is locked out as a whole, while other clients are not; a
// success on its own account does not reset its budget (regression: the
// throttle keyed on (client, access key) only).
func TestLoginThrottlePerClient(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC))
	svc, _ := newTestService(t, clk, session.WithLoginThrottle(3, time.Minute, 30*time.Second), session.WithClientLoginThrottle(5))
	from := func(client, accessKey, secretKey string) error {
		_, err := svc.Login(ctx, session.LoginInput{AccessKey: accessKey, SecretKey: secretKey, Client: client})
		return err
	}
	for i := range 4 {
		require.ErrorIs(t, from("192.0.2.9", fmt.Sprintf("user%d", i), "guess"), domain.ErrAccessDenied)
	}
	require.NoError(t, from("192.0.2.9", "root", "rootsecret"), "under the client limit")
	require.ErrorIs(t, from("192.0.2.9", "user4", "guess"), domain.ErrAccessDenied, "the fifth failure locks the client out")
	err := from("192.0.2.9", "root", "rootsecret")
	var throttled *session.ThrottledError
	require.ErrorAs(t, err, &throttled, "every access key of the client is refused")
	assert.Equal(t, 30*time.Second, throttled.RetryAfter)
	require.NoError(t, from("192.0.2.10", "root", "rootsecret"), "other clients are not affected")

	clk.Advance(31 * time.Second)
	require.NoError(t, from("192.0.2.9", "root", "rootsecret"), "the lockout ends")

	// An empty client (unknown address) is throttled per access key only.
	for i := range 6 {
		require.ErrorIs(t, from("", fmt.Sprintf("other%d", i), "guess"), domain.ErrAccessDenied)
	}
	require.NoError(t, from("", "root", "rootsecret"))
}

// TestLoginThrottleIsBounded: the memory of the throttle is bounded; the
// least recently failed entries are forgotten first (regression: random
// access keys grew the table without limit).
func TestLoginThrottleIsBounded(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC))
	svc, _ := newTestService(t, clk, session.WithLoginThrottle(3, time.Minute, 30*time.Second),
		session.WithClientLoginThrottle(0), session.WithThrottleEntries(2))
	from := func(client, accessKey, secretKey string) error {
		_, err := svc.Login(ctx, session.LoginInput{AccessKey: accessKey, SecretKey: secretKey, Client: client})
		return err
	}
	// The pair (192.0.2.1, root) is one failure away from the lockout,
	// then two other pairs push it out of the table.
	for range 2 {
		require.ErrorIs(t, from("192.0.2.1", "root", "nope"), domain.ErrAccessDenied)
	}
	require.ErrorIs(t, from("192.0.2.2", "root", "nope"), domain.ErrAccessDenied)
	require.ErrorIs(t, from("192.0.2.3", "root", "nope"), domain.ErrAccessDenied)
	require.ErrorIs(t, from("192.0.2.1", "root", "nope"), domain.ErrAccessDenied)
	assert.NotErrorIs(t, from("192.0.2.1", "root", "nope"), domain.ErrTooManyRequests, "the evicted pair counts again from zero")

	// A recently failing pair stays: the least recently failed one goes.
	require.ErrorIs(t, from("192.0.2.1", "root", "nope"), domain.ErrAccessDenied, "third failure: locked out")
	require.ErrorIs(t, from("192.0.2.1", "root", "rootsecret"), domain.ErrTooManyRequests)
}

func TestValidateRejectsExpiredTamperedAndRevoked(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC))
	svc, _ := newTestService(t, clk, session.WithTTL(time.Hour))
	sess, err := svc.Login(ctx, login("root", "rootsecret"))
	require.NoError(t, err)

	_, err = svc.Validate(ctx, sess.Token+"x")
	assert.ErrorIs(t, err, domain.ErrAccessDenied)
	_, err = svc.Validate(ctx, "")
	assert.ErrorIs(t, err, domain.ErrAccessDenied)

	// A token signed with another key is rejected.
	other, _ := newTestService(t, clk, session.WithSigningKey([]byte("another-key-another-key-another!")))
	otherSess, err := other.Login(ctx, login("root", "rootsecret"))
	require.NoError(t, err)
	_, err = svc.Validate(ctx, otherSess.Token)
	assert.ErrorIs(t, err, domain.ErrAccessDenied)

	// Revocation.
	require.NoError(t, svc.Logout(ctx, sess.Token))
	_, err = svc.Validate(ctx, sess.Token)
	assert.ErrorIs(t, err, domain.ErrAccessDenied)
	require.NoError(t, svc.Logout(ctx, "garbage"), "logout of invalid tokens is a no-op")

	// Expiry.
	sess2, err := svc.Login(ctx, login("root", "rootsecret"))
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
