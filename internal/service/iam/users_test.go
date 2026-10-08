package iam_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/iam"
)

func TestBootstrap(t *testing.T) {
	ctx := context.Background()
	svc := iam.New(iam.Deps{Logger: discard()})

	_, _, err := svc.LookupCredentials(ctx, rootAK)
	assert.ErrorIs(t, err, domain.ErrInvalidAccessKey, "no root before bootstrap")
	assert.ErrorIs(t, svc.Bootstrap(ctx, domain.Credentials{AccessKey: rootAK}), domain.ErrInvalidArgument)
	assert.ErrorIs(t, svc.Bootstrap(ctx, domain.Credentials{SecretKey: rootSK}), domain.ErrInvalidArgument)

	require.NoError(t, svc.Bootstrap(ctx, domain.Credentials{AccessKey: rootAK, SecretKey: rootSK, SessionToken: "x", Status: domain.StatusDisabled}))
	require.NoError(t, svc.Bootstrap(ctx, domain.Credentials{AccessKey: rootAK, SecretKey: rootSK}), "idempotent")
	creds, id, err := svc.LookupCredentials(ctx, rootAK)
	require.NoError(t, err)
	assert.Equal(t, domain.Credentials{AccessKey: rootAK, SecretKey: rootSK, Status: domain.StatusEnabled}, creds)
	assert.Equal(t, domain.Identity{AccessKey: rootAK, IsRoot: true}, id)

	u, err := svc.GetUser(ctx, rootAK)
	require.NoError(t, err)
	assert.Equal(t, domain.StatusEnabled, u.Status)
	users, err := svc.ListUsers(ctx)
	require.NoError(t, err)
	assert.Empty(t, users, "root is not listed")

	// A user shadowed by a new root key keeps existing but root wins.
	mustUser(t, svc, "newroot")
	require.NoError(t, svc.Bootstrap(ctx, domain.Credentials{AccessKey: "newroot", SecretKey: "newrootsecret"}))
	_, id, err = svc.LookupCredentials(ctx, "newroot")
	require.NoError(t, err)
	assert.True(t, id.IsRoot)
	_, _, err = svc.LookupCredentials(ctx, rootAK)
	assert.ErrorIs(t, err, domain.ErrInvalidAccessKey, "the old root key is gone")
}

func TestNewRootOnly(t *testing.T) {
	ctx := context.Background()
	svc := iam.NewRootOnly(iam.Deps{Store: iam.NewMemoryStore(), Logger: discard()})
	require.NoError(t, svc.Bootstrap(ctx, domain.Credentials{AccessKey: rootAK, SecretKey: rootSK}))
	_, id, err := svc.LookupCredentials(ctx, rootAK)
	require.NoError(t, err)
	assert.True(t, id.IsRoot)
	mustUser(t, svc, "alice")
}

func TestCreateUserValidation(t *testing.T) {
	ctx := context.Background()
	svc, _ := newIAM(t, nil)
	mustUser(t, svc, "taken")
	tests := []struct {
		name string
		in   iam.CreateUserInput
		want error
	}{
		{"short access key", iam.CreateUserInput{AccessKey: "ab", SecretKey: "secret123"}, domain.ErrInvalidArgument},
		{"long access key", iam.CreateUserInput{AccessKey: strings.Repeat("a", 21), SecretKey: "secret123"}, domain.ErrInvalidArgument},
		{"access key with slash", iam.CreateUserInput{AccessKey: "ali/ce", SecretKey: "secret123"}, domain.ErrInvalidArgument},
		{"access key with comma", iam.CreateUserInput{AccessKey: "ali,ce", SecretKey: "secret123"}, domain.ErrInvalidArgument},
		{"short secret", iam.CreateUserInput{AccessKey: "alice", SecretKey: "1234567"}, domain.ErrInvalidArgument},
		{"long secret", iam.CreateUserInput{AccessKey: "alice", SecretKey: strings.Repeat("s", 41)}, domain.ErrInvalidArgument},
		{"control char in secret", iam.CreateUserInput{AccessKey: "alice", SecretKey: "secret\n123"}, domain.ErrInvalidArgument},
		{"bad status", iam.CreateUserInput{AccessKey: "alice", SecretKey: "secret123", Status: "paused"}, domain.ErrInvalidArgument},
		{"empty policy name", iam.CreateUserInput{AccessKey: "alice", SecretKey: "secret123", PolicyNames: []string{""}}, domain.ErrInvalidArgument},
		{"policy with comma", iam.CreateUserInput{AccessKey: "alice", SecretKey: "secret123", PolicyNames: []string{"a,b"}}, domain.ErrInvalidArgument},
		{"policy with spaces", iam.CreateUserInput{AccessKey: "alice", SecretKey: "secret123", PolicyNames: []string{" a"}}, domain.ErrInvalidArgument},
		{"root key", iam.CreateUserInput{AccessKey: rootAK, SecretKey: "secret123"}, domain.ErrUserAlreadyExists},
		{"existing user", iam.CreateUserInput{AccessKey: "taken", SecretKey: "secret123"}, domain.ErrUserAlreadyExists},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := svc.CreateUser(ctx, tt.in)
			assert.ErrorIs(t, err, tt.want)
		})
	}
}

func TestUserLifecycle(t *testing.T) {
	ctx := context.Background()
	svc, clk := newIAM(t, nil)

	u, err := svc.CreateUser(ctx, iam.CreateUserInput{AccessKey: "alice", SecretKey: "alice-secret", PolicyNames: []string{"readwrite", "readonly", "readwrite"}})
	require.NoError(t, err)
	assert.Equal(t, &iam.User{AccessKey: "alice", Status: domain.StatusEnabled, PolicyNames: []string{"readonly", "readwrite"}, CreatedAt: t0(), UpdatedAt: t0()}, u)
	_, err = svc.CreateUser(ctx, iam.CreateUserInput{AccessKey: "bob.b@x+y", SecretKey: "bob-secret", Status: domain.StatusDisabled})
	require.NoError(t, err)

	users, err := svc.ListUsers(ctx)
	require.NoError(t, err)
	require.Len(t, users, 2)
	assert.Equal(t, "alice", users[0].AccessKey)
	assert.Equal(t, domain.StatusDisabled, users[1].Status)

	clk.Advance(time.Minute)
	require.NoError(t, svc.SetUserSecret(ctx, "alice", "new-secret-1"))
	creds, _, err := svc.LookupCredentials(ctx, "alice")
	require.NoError(t, err)
	assert.Equal(t, "new-secret-1", creds.SecretKey)

	require.NoError(t, svc.SetUserPolicy(ctx, "alice", []string{"diagnostics"}))
	u, err = svc.GetUser(ctx, "alice")
	require.NoError(t, err)
	assert.Equal(t, []string{"diagnostics"}, u.PolicyNames)
	assert.Equal(t, t0().Add(time.Minute), u.UpdatedAt)
	require.NoError(t, svc.SetUserPolicy(ctx, "alice", nil))
	u, err = svc.GetUser(ctx, "alice")
	require.NoError(t, err)
	assert.Nil(t, u.PolicyNames)

	require.NoError(t, svc.SetUserStatus(ctx, "alice", domain.StatusDisabled))
	_, _, err = svc.LookupCredentials(ctx, "alice")
	assert.ErrorIs(t, err, domain.ErrAccessDenied)
	require.NoError(t, svc.SetUserStatus(ctx, "alice", domain.StatusEnabled))
	_, _, err = svc.LookupCredentials(ctx, "alice")
	require.NoError(t, err)

	require.NoError(t, svc.DeleteUser(ctx, "alice"))
	_, err = svc.GetUser(ctx, "alice")
	assert.ErrorIs(t, err, domain.ErrUserNotFound)
	_, _, err = svc.LookupCredentials(ctx, "alice")
	assert.ErrorIs(t, err, domain.ErrInvalidAccessKey)
}

func TestUserErrors(t *testing.T) {
	ctx := context.Background()
	svc, _ := newIAM(t, nil)
	mustUser(t, svc, "alice")
	tests := []struct {
		name string
		call func() error
		want error
	}{
		{"get unknown", func() error { _, err := svc.GetUser(ctx, "ghost"); return err }, domain.ErrUserNotFound},
		{"delete unknown", func() error { return svc.DeleteUser(ctx, "ghost") }, domain.ErrUserNotFound},
		{"secret of unknown", func() error { return svc.SetUserSecret(ctx, "ghost", "secret123") }, domain.ErrUserNotFound},
		{"status of unknown", func() error { return svc.SetUserStatus(ctx, "ghost", domain.StatusEnabled) }, domain.ErrUserNotFound},
		{"policy of unknown", func() error { return svc.SetUserPolicy(ctx, "ghost", nil) }, domain.ErrUserNotFound},
		{"invalid secret", func() error { return svc.SetUserSecret(ctx, "alice", "short") }, domain.ErrInvalidArgument},
		{"empty status", func() error { return svc.SetUserStatus(ctx, "alice", "") }, domain.ErrInvalidArgument},
		{"invalid status", func() error { return svc.SetUserStatus(ctx, "alice", "on") }, domain.ErrInvalidArgument},
		{"invalid policy", func() error { return svc.SetUserPolicy(ctx, "alice", []string{"a\x01"}) }, domain.ErrInvalidArgument},
		{"delete root", func() error { return svc.DeleteUser(ctx, rootAK) }, domain.ErrInvalidArgument},
		{"secret of root", func() error { return svc.SetUserSecret(ctx, rootAK, "secret123") }, domain.ErrInvalidArgument},
		{"disable root", func() error { return svc.SetUserStatus(ctx, rootAK, domain.StatusDisabled) }, domain.ErrInvalidArgument},
		{"policy of root", func() error { return svc.SetUserPolicy(ctx, rootAK, []string{"readonly"}) }, domain.ErrInvalidArgument},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.ErrorIs(t, tt.call(), tt.want)
		})
	}
}

func TestDeleteUserCascades(t *testing.T) {
	ctx := context.Background()
	store := iam.NewMemoryStore()
	svc, _ := newIAM(t, store)
	mustUser(t, svc, "alice")
	mustUser(t, svc, "bob")
	_, err := svc.CreateGroup(ctx, iam.CreateGroupInput{Name: "dev", Members: []string{"alice", "bob"}})
	require.NoError(t, err)
	key, err := svc.CreateAccessKey(ctx, iam.CreateAccessKeyInput{Caller: domain.Identity{AccessKey: "alice"}})
	require.NoError(t, err)
	bobKey, err := svc.CreateAccessKey(ctx, iam.CreateAccessKeyInput{Caller: domain.Identity{AccessKey: "bob"}})
	require.NoError(t, err)
	tmp, err := svc.AssumeRole(ctx, iam.AssumeRoleInput{Caller: domain.Identity{AccessKey: "alice"}})
	require.NoError(t, err)

	require.NoError(t, svc.DeleteUser(ctx, "alice"))

	g, err := svc.GetGroup(ctx, "dev")
	require.NoError(t, err)
	assert.Equal(t, []string{"bob"}, g.Members)
	for _, ak := range []string{key.AccessKey, tmp.AccessKey} {
		_, _, err := svc.LookupCredentials(ctx, ak)
		assert.ErrorIs(t, err, domain.ErrInvalidAccessKey, ak)
	}
	_, _, err = svc.LookupCredentials(ctx, bobKey.AccessKey)
	assert.NoError(t, err, "other users keep their keys")

	// Nothing of alice is left in the store.
	for _, ns := range []iam.Namespace{iam.NamespaceUsers, iam.NamespaceServiceAccounts, iam.NamespaceSTS} {
		entries, err := store.List(ctx, ns)
		require.NoError(t, err)
		for k := range entries {
			assert.NotContains(t, []string{"alice", key.AccessKey, tmp.AccessKey}, k, ns)
		}
	}
}

func TestUsersPersistAcrossInstances(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	svc, _ := newIAM(t, newFileStore(t, dir, rootSK))
	mustUser(t, svc, "alice", "readonly")
	_, err := svc.CreateGroup(ctx, iam.CreateGroupInput{Name: "dev", Members: []string{"alice"}, PolicyNames: []string{"diagnostics"}})
	require.NoError(t, err)
	key, err := svc.CreateAccessKey(ctx, iam.CreateAccessKeyInput{Caller: rootID(), ParentUser: "alice", Policy: readOnlyPolicy("arn:aws:s3:::b/*")})
	require.NoError(t, err)
	tmp, err := svc.AssumeRole(ctx, iam.AssumeRoleInput{Caller: domain.Identity{AccessKey: "alice"}})
	require.NoError(t, err)

	// A new process with the same root credentials sees everything.
	again, _ := newIAM(t, newFileStore(t, dir, rootSK))
	_, id, err := again.LookupCredentials(ctx, "alice")
	require.NoError(t, err)
	assert.Equal(t, []string{"dev"}, id.Groups)
	assert.Equal(t, []string{"diagnostics", "readonly"}, id.PolicyNames)
	creds, id, err := again.LookupCredentials(ctx, key.AccessKey)
	require.NoError(t, err)
	assert.Equal(t, key.SecretKey, creds.SecretKey)
	assert.Equal(t, readOnlyPolicy("arn:aws:s3:::b/*"), id.SessionPolicy)
	creds, _, err = again.LookupCredentials(ctx, tmp.AccessKey)
	require.NoError(t, err)
	assert.Equal(t, tmp.SessionToken, creds.SessionToken)
}
