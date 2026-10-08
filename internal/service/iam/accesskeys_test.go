package iam_test

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/iam"
)

func TestCreateAccessKey(t *testing.T) {
	ctx := context.Background()
	svc, _ := newIAM(t, nil)
	mustUser(t, svc, "alice", "readwrite")
	bob := domain.Identity{AccessKey: "bob"}
	mustUser(t, svc, "bob")

	// Generated credentials.
	key, err := svc.CreateAccessKey(ctx, iam.CreateAccessKeyInput{Caller: domain.Identity{AccessKey: "alice"}, Name: "ci", Description: "build pipeline"})
	require.NoError(t, err)
	assert.Regexp(t, regexp.MustCompile(`^[A-Z0-9]{20}$`), key.AccessKey)
	assert.Regexp(t, regexp.MustCompile(`^[A-Za-z0-9]{40}$`), key.SecretKey)
	assert.Equal(t, "alice", key.ParentUser)
	assert.Equal(t, domain.StatusEnabled, key.Status)
	assert.Equal(t, t0(), key.CreatedAt)
	assert.True(t, key.Expiration.IsZero())

	// Explicit credentials, policy and expiry, created by root for alice.
	exp := t0().Add(24 * time.Hour)
	pol := readOnlyPolicy("arn:aws:s3:::photos/*")
	key2, err := svc.CreateAccessKey(ctx, iam.CreateAccessKeyInput{
		Caller: rootID(), ParentUser: "alice", AccessKey: "alice-ro", SecretKey: "alice-ro-secret",
		Expiration: exp, Policy: pol,
	})
	require.NoError(t, err)
	assert.Equal(t, "alice-ro-secret", key2.SecretKey)
	assert.Equal(t, exp, key2.Expiration)
	assert.Equal(t, pol, key2.Policy)
	pol.Statement[0].Action[0] = "s3:*"
	got, err := svc.GetAccessKey(ctx, rootID(), "alice-ro")
	require.NoError(t, err)
	assert.Equal(t, domain.StringSet{"s3:GetObject"}, got.Policy.Statement[0].Action, "the stored policy is a copy")
	assert.Empty(t, got.SecretKey, "the secret is shown at creation only")

	// Keys of root.
	rootKey, err := svc.CreateAccessKey(ctx, iam.CreateAccessKeyInput{Caller: rootID()})
	require.NoError(t, err)
	assert.Equal(t, rootAK, rootKey.ParentUser)

	tests := []struct {
		name string
		in   iam.CreateAccessKeyInput
		want error
	}{
		{"anonymous", iam.CreateAccessKeyInput{Caller: *domain.Anonymous()}, domain.ErrAccessDenied},
		{"derived caller", iam.CreateAccessKeyInput{Caller: domain.Identity{AccessKey: key.AccessKey, Parent: "alice"}}, domain.ErrAccessDenied},
		{"derived caller of root", iam.CreateAccessKeyInput{Caller: domain.Identity{AccessKey: rootKey.AccessKey, Parent: rootAK, IsRoot: true}}, domain.ErrAccessDenied},
		{"user for another user", iam.CreateAccessKeyInput{Caller: bob, ParentUser: "alice"}, domain.ErrAccessDenied},
		{"unknown parent", iam.CreateAccessKeyInput{Caller: rootID(), ParentUser: "ghost"}, domain.ErrUserNotFound},
		{"unknown caller user", iam.CreateAccessKeyInput{Caller: domain.Identity{AccessKey: "ghost"}}, domain.ErrUserNotFound},
		{"invalid access key", iam.CreateAccessKeyInput{Caller: bob, AccessKey: "a b"}, domain.ErrInvalidArgument},
		{"invalid secret", iam.CreateAccessKeyInput{Caller: bob, SecretKey: "short"}, domain.ErrInvalidArgument},
		{"long name", iam.CreateAccessKeyInput{Caller: bob, Name: strings.Repeat("n", 65)}, domain.ErrInvalidArgument},
		{"long description", iam.CreateAccessKeyInput{Caller: bob, Description: strings.Repeat("d", 257)}, domain.ErrInvalidArgument},
		{"past expiration", iam.CreateAccessKeyInput{Caller: bob, Expiration: t0()}, domain.ErrInvalidArgument},
		{"policy without version", iam.CreateAccessKeyInput{Caller: bob, Policy: &domain.Policy{Statement: domain.Statements{{Effect: domain.Allow}}}}, domain.ErrMalformedPolicy},
		{"policy without statements", iam.CreateAccessKeyInput{Caller: bob, Policy: &domain.Policy{Version: domain.PolicyVersion}}, domain.ErrMalformedPolicy},
		{"policy with bad effect", iam.CreateAccessKeyInput{Caller: bob, Policy: &domain.Policy{Version: domain.PolicyVersion, Statement: domain.Statements{{Effect: "Maybe"}}}}, domain.ErrMalformedPolicy},
		{"key of a user", iam.CreateAccessKeyInput{Caller: bob, AccessKey: "alice"}, domain.ErrAccessKeyAlreadyExists},
		{"key of root", iam.CreateAccessKeyInput{Caller: bob, AccessKey: rootAK}, domain.ErrAccessKeyAlreadyExists},
		{"existing key", iam.CreateAccessKeyInput{Caller: rootID(), ParentUser: "alice", AccessKey: "alice-ro"}, domain.ErrAccessKeyAlreadyExists},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := svc.CreateAccessKey(ctx, tt.in)
			assert.ErrorIs(t, err, tt.want)
		})
	}

	// A user name cannot collide with a key either.
	_, err = svc.CreateUser(ctx, iam.CreateUserInput{AccessKey: "alice-ro", SecretKey: "secret123"})
	assert.ErrorIs(t, err, domain.ErrAccessKeyAlreadyExists)
	assert.ErrorIs(t, err, domain.ErrUserAlreadyExists)
}

func TestAccessKeyOwnership(t *testing.T) {
	ctx := context.Background()
	svc, _ := newIAM(t, nil)
	mustUser(t, svc, "alice")
	mustUser(t, svc, "bob")
	alice := domain.Identity{AccessKey: "alice"}
	bob := domain.Identity{AccessKey: "bob"}
	aliceKey, err := svc.CreateAccessKey(ctx, iam.CreateAccessKeyInput{Caller: alice, AccessKey: "alice-key"})
	require.NoError(t, err)
	_, err = svc.CreateAccessKey(ctx, iam.CreateAccessKeyInput{Caller: bob, AccessKey: "bob-key"})
	require.NoError(t, err)
	_, err = svc.CreateAccessKey(ctx, iam.CreateAccessKeyInput{Caller: rootID(), AccessKey: "root-key"})
	require.NoError(t, err)
	derived := domain.Identity{AccessKey: aliceKey.AccessKey, Parent: "alice"}
	disabled := domain.StatusDisabled

	keys, err := svc.ListAccessKeys(ctx, rootID(), "")
	require.NoError(t, err)
	assert.Equal(t, []string{"alice-key", "bob-key", "root-key"}, accessKeys(keys))
	keys, err = svc.ListAccessKeys(ctx, rootID(), "bob")
	require.NoError(t, err)
	assert.Equal(t, []string{"bob-key"}, accessKeys(keys))
	keys, err = svc.ListAccessKeys(ctx, alice, "")
	require.NoError(t, err)
	assert.Equal(t, []string{"alice-key"}, accessKeys(keys))
	keys, err = svc.ListAccessKeys(ctx, rootID(), rootAK)
	require.NoError(t, err)
	assert.Equal(t, []string{"root-key"}, accessKeys(keys))
	for _, k := range keys {
		assert.Empty(t, k.SecretKey)
	}

	tests := []struct {
		name string
		call func() error
		want error
	}{
		{"user lists another user", func() error { _, err := svc.ListAccessKeys(ctx, alice, "bob"); return err }, domain.ErrAccessDenied},
		{"root lists unknown user", func() error { _, err := svc.ListAccessKeys(ctx, rootID(), "ghost"); return err }, domain.ErrUserNotFound},
		{"derived lists", func() error { _, err := svc.ListAccessKeys(ctx, derived, ""); return err }, domain.ErrAccessDenied},
		{"user gets another's key", func() error { _, err := svc.GetAccessKey(ctx, alice, "bob-key"); return err }, domain.ErrAccessDenied},
		{"user gets root's key", func() error { _, err := svc.GetAccessKey(ctx, alice, "root-key"); return err }, domain.ErrAccessDenied},
		{"get unknown", func() error { _, err := svc.GetAccessKey(ctx, rootID(), "nope"); return err }, domain.ErrAccessKeyNotFound},
		{"get a user", func() error { _, err := svc.GetAccessKey(ctx, rootID(), "alice"); return err }, domain.ErrAccessKeyNotFound},
		{"anonymous gets", func() error { _, err := svc.GetAccessKey(ctx, *domain.Anonymous(), "alice-key"); return err }, domain.ErrAccessDenied},
		{"derived updates", func() error {
			_, err := svc.UpdateAccessKey(ctx, derived, "alice-key", iam.UpdateAccessKeyInput{Status: &disabled})
			return err
		}, domain.ErrAccessDenied},
		{"user updates another's", func() error {
			_, err := svc.UpdateAccessKey(ctx, alice, "bob-key", iam.UpdateAccessKeyInput{Status: &disabled})
			return err
		}, domain.ErrAccessDenied},
		{"update unknown", func() error {
			_, err := svc.UpdateAccessKey(ctx, rootID(), "nope", iam.UpdateAccessKeyInput{})
			return err
		}, domain.ErrAccessKeyNotFound},
		{"user deletes another's", func() error { return svc.DeleteAccessKey(ctx, alice, "bob-key") }, domain.ErrAccessDenied},
		{"derived deletes", func() error { return svc.DeleteAccessKey(ctx, derived, "alice-key") }, domain.ErrAccessDenied},
		{"delete unknown", func() error { return svc.DeleteAccessKey(ctx, rootID(), "nope") }, domain.ErrAccessKeyNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.ErrorIs(t, tt.call(), tt.want)
		})
	}

	got, err := svc.GetAccessKey(ctx, alice, "alice-key")
	require.NoError(t, err)
	assert.Equal(t, "alice", got.ParentUser)
	require.NoError(t, svc.DeleteAccessKey(ctx, alice, "alice-key"))
	require.NoError(t, svc.DeleteAccessKey(ctx, rootID(), "bob-key"))
	keys, err = svc.ListAccessKeys(ctx, rootID(), "")
	require.NoError(t, err)
	assert.Equal(t, []string{"root-key"}, accessKeys(keys))
}

func TestUpdateAccessKey(t *testing.T) {
	ctx := context.Background()
	svc, clk := newIAM(t, nil)
	mustUser(t, svc, "alice")
	alice := domain.Identity{AccessKey: "alice"}
	_, err := svc.CreateAccessKey(ctx, iam.CreateAccessKeyInput{Caller: alice, AccessKey: "ak-alice", Expiration: t0().Add(time.Hour)})
	require.NoError(t, err)

	clk.Advance(time.Minute)
	secret, name, desc := "rotated-secret", "renamed", "described"
	disabled := domain.StatusDisabled
	exp := t0().Add(48 * time.Hour)
	pol := readOnlyPolicy("arn:aws:s3:::b/*")
	got, err := svc.UpdateAccessKey(ctx, alice, "ak-alice", iam.UpdateAccessKeyInput{
		SecretKey: &secret, Name: &name, Description: &desc, Status: &disabled, Expiration: &exp, Policy: pol,
	})
	require.NoError(t, err)
	assert.Equal(t, "renamed", got.Name)
	assert.Equal(t, "described", got.Description)
	assert.Equal(t, domain.StatusDisabled, got.Status)
	assert.Equal(t, exp, got.Expiration)
	assert.Equal(t, pol, got.Policy)
	assert.Equal(t, t0().Add(time.Minute), got.UpdatedAt)
	assert.Empty(t, got.SecretKey)
	_, _, err = svc.LookupCredentials(ctx, "ak-alice")
	assert.ErrorIs(t, err, domain.ErrAccessDenied, "disabled")

	enabled := domain.StatusEnabled
	noExpiry := time.Time{}
	got, err = svc.UpdateAccessKey(ctx, alice, "ak-alice", iam.UpdateAccessKeyInput{Status: &enabled, Expiration: &noExpiry, ClearPolicy: true})
	require.NoError(t, err)
	assert.True(t, got.Expiration.IsZero())
	assert.Nil(t, got.Policy)
	assert.Equal(t, "renamed", got.Name, "unset fields are kept")
	creds, id, err := svc.LookupCredentials(ctx, "ak-alice")
	require.NoError(t, err)
	assert.Equal(t, "rotated-secret", creds.SecretKey)
	assert.Nil(t, id.SessionPolicy)

	past := t0()
	bad := "bad"
	long := strings.Repeat("x", 300)
	tests := []struct {
		name string
		in   iam.UpdateAccessKeyInput
		want error
	}{
		{"invalid secret", iam.UpdateAccessKeyInput{SecretKey: &bad}, domain.ErrInvalidArgument},
		{"invalid status", iam.UpdateAccessKeyInput{Status: (*domain.CredentialStatus)(&bad)}, domain.ErrInvalidArgument},
		{"long description", iam.UpdateAccessKeyInput{Description: &long}, domain.ErrInvalidArgument},
		{"past expiration", iam.UpdateAccessKeyInput{Expiration: &past}, domain.ErrInvalidArgument},
		{"policy and clear", iam.UpdateAccessKeyInput{Policy: pol, ClearPolicy: true}, domain.ErrInvalidArgument},
		{"malformed policy", iam.UpdateAccessKeyInput{Policy: &domain.Policy{}}, domain.ErrMalformedPolicy},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := svc.UpdateAccessKey(ctx, alice, "ak-alice", tt.in)
			assert.ErrorIs(t, err, tt.want)
		})
	}
}

func accessKeys(keys []iam.AccessKey) []string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k.AccessKey)
	}
	return out
}
