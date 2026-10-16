package iam_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/auth"
	"github.com/r3n3r0/nexo/internal/service/iam"
)

// TestLookupServiceAccounts checks the identity of service accounts: the
// parent's policies plus the inline policy, which the policy engine
// intersects (docs/09 TPS-17), and the expiry and status rules.
func TestLookupServiceAccounts(t *testing.T) {
	ctx := context.Background()
	svc, clk := newIAM(t, nil)
	mustUser(t, svc, "alice", "readwrite")
	_, err := svc.CreateGroup(ctx, iam.CreateGroupInput{Name: "dev", Members: []string{"alice"}, PolicyNames: []string{"diagnostics"}})
	require.NoError(t, err)
	inline := readOnlyPolicy("arn:aws:s3:::photos/*")
	exp := t0().Add(time.Hour)
	key, err := svc.CreateAccessKey(ctx, iam.CreateAccessKeyInput{Caller: rootID(), ParentUser: "alice", Policy: inline, Expiration: exp})
	require.NoError(t, err)
	plain, err := svc.CreateAccessKey(ctx, iam.CreateAccessKeyInput{Caller: domain.Identity{AccessKey: "alice"}})
	require.NoError(t, err)
	rootKey, err := svc.CreateAccessKey(ctx, iam.CreateAccessKeyInput{Caller: rootID(), Policy: inline})
	require.NoError(t, err)

	creds, id, err := svc.LookupCredentials(ctx, key.AccessKey)
	require.NoError(t, err)
	assert.Equal(t, domain.Credentials{AccessKey: key.AccessKey, SecretKey: key.SecretKey, Expiration: exp, Status: domain.StatusEnabled}, creds)
	assert.Equal(t, domain.Identity{
		AccessKey: key.AccessKey, Parent: "alice", Groups: []string{"dev"},
		PolicyNames: []string{"diagnostics", "readwrite"}, SessionPolicy: inline,
	}, id)
	assert.Equal(t, "alice", id.EffectiveUser())
	id.SessionPolicy.Statement[0].Effect = domain.Deny
	_, again, err := svc.LookupCredentials(ctx, key.AccessKey)
	require.NoError(t, err)
	assert.Equal(t, domain.Allow, again.SessionPolicy.Statement[0].Effect, "identities do not alias the cache")

	_, id, err = svc.LookupCredentials(ctx, plain.AccessKey)
	require.NoError(t, err)
	assert.Nil(t, id.SessionPolicy, "no inline policy: the parent's permissions")

	_, id, err = svc.LookupCredentials(ctx, rootKey.AccessKey)
	require.NoError(t, err)
	assert.Equal(t, domain.Identity{AccessKey: rootKey.AccessKey, Parent: rootAK, IsRoot: true, SessionPolicy: inline}, id,
		"keys of root are root limited by their inline policy")

	// Disabling the parent disables its keys immediately.
	require.NoError(t, svc.SetUserStatus(ctx, "alice", domain.StatusDisabled))
	_, _, err = svc.LookupCredentials(ctx, plain.AccessKey)
	assert.ErrorIs(t, err, domain.ErrCredentialsDisabled)
	_, _, err = svc.LookupCredentials(ctx, "alice")
	assert.ErrorIs(t, err, domain.ErrCredentialsDisabled, "the user itself")
	require.NoError(t, svc.SetUserStatus(ctx, "alice", domain.StatusEnabled))

	// Expiry follows the clock.
	clk.Set(exp.Add(-time.Second))
	_, _, err = svc.LookupCredentials(ctx, key.AccessKey)
	require.NoError(t, err)
	clk.Set(exp)
	_, _, err = svc.LookupCredentials(ctx, key.AccessKey)
	assert.ErrorIs(t, err, domain.ErrAccessDenied)
	assert.NotErrorIs(t, err, domain.ErrCredentialsDisabled, "expired, not disabled")
	assert.Contains(t, err.Error(), "expired")
	_, _, err = svc.LookupCredentials(ctx, plain.AccessKey)
	assert.NoError(t, err, "keys without expiration never expire")

	// A key whose root parent was replaced is unknown.
	require.NoError(t, svc.Bootstrap(ctx, domain.Credentials{AccessKey: "otherroot", SecretKey: rootSK}))
	_, _, err = svc.LookupCredentials(ctx, rootKey.AccessKey)
	assert.ErrorIs(t, err, domain.ErrInvalidAccessKey)
}

func TestServiceIsACredentialLookup(t *testing.T) {
	var lookup auth.CredentialLookup = iam.New(iam.Deps{Logger: discard()})
	assert.NotNil(t, lookup)
}

func TestLookupUnknown(t *testing.T) {
	ctx := context.Background()
	svc, _ := newIAM(t, nil)
	for _, ak := range []string{"", "nobody", "AKIAUNKNOWN"} {
		_, _, err := svc.LookupCredentials(ctx, ak)
		assert.ErrorIs(t, err, domain.ErrInvalidAccessKey, ak)
	}
}

// covers: pentest PT-05 (disabling a user or an access key moves the
// revocation epoch of its credentials and of the keys derived from it, for
// good and persistently; enabling it again does not move it back)
func TestRevocationEpoch(t *testing.T) {
	ctx := context.Background()
	store := iam.NewMemoryStore()
	svc, _ := newIAM(t, store)
	mustUser(t, svc, "alice")
	key, err := svc.CreateAccessKey(ctx, iam.CreateAccessKeyInput{Caller: domain.Identity{AccessKey: "alice"}, Name: "ci"})
	require.NoError(t, err)
	rootKey, err := svc.CreateAccessKey(ctx, iam.CreateAccessKeyInput{Caller: rootID()})
	require.NoError(t, err)

	epoch := func(svc iam.Service, ak string) uint64 {
		t.Helper()
		creds, _, err := svc.LookupCredentials(ctx, ak)
		require.NoError(t, err)
		return creds.RevocationEpoch
	}
	assert.Zero(t, epoch(svc, "alice"))
	assert.Zero(t, epoch(svc, key.AccessKey))
	assert.Zero(t, epoch(svc, rootAK))

	// Disabling the user moves its epoch and the one of its keys.
	require.NoError(t, svc.SetUserStatus(ctx, "alice", domain.StatusDisabled))
	require.NoError(t, svc.SetUserStatus(ctx, "alice", domain.StatusDisabled), "disabling twice is idempotent")
	require.NoError(t, svc.SetUserStatus(ctx, "alice", domain.StatusEnabled))
	assert.Equal(t, uint64(1), epoch(svc, "alice"))
	assert.Equal(t, uint64(1), epoch(svc, key.AccessKey))

	// Disabling the key moves its own epoch only.
	disabled, enabled := domain.StatusDisabled, domain.StatusEnabled
	_, err = svc.UpdateAccessKey(ctx, rootID(), key.AccessKey, iam.UpdateAccessKeyInput{Status: &disabled})
	require.NoError(t, err)
	_, err = svc.UpdateAccessKey(ctx, rootID(), key.AccessKey, iam.UpdateAccessKeyInput{Status: &enabled})
	require.NoError(t, err)
	assert.Equal(t, uint64(1), epoch(svc, "alice"))
	assert.Equal(t, uint64(2), epoch(svc, key.AccessKey))
	_, err = svc.UpdateAccessKey(ctx, rootID(), rootKey.AccessKey, iam.UpdateAccessKeyInput{Status: &disabled})
	require.NoError(t, err)
	_, err = svc.UpdateAccessKey(ctx, rootID(), rootKey.AccessKey, iam.UpdateAccessKeyInput{Status: &enabled})
	require.NoError(t, err)
	assert.Equal(t, uint64(1), epoch(svc, rootKey.AccessKey), "a key of root has its own epoch")

	// The epochs are persisted: another instance (node, restart) sees them.
	other, _ := newIAM(t, store)
	assert.Equal(t, uint64(1), epoch(other, "alice"))
	assert.Equal(t, uint64(2), epoch(other, key.AccessKey))
}
