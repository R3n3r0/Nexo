package iam_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/iam"
)

// covers: S-IAM-07
func TestAssumeRole(t *testing.T) {
	ctx := context.Background()
	store := iam.NewMemoryStore()
	svc, clk := newIAM(t, store)
	mustUser(t, svc, "alice", "readwrite")
	session := readOnlyPolicy("arn:aws:s3:::reports/*")

	creds, err := svc.AssumeRole(ctx, iam.AssumeRoleInput{
		Caller: domain.Identity{AccessKey: "alice"}, Policy: session, Duration: 30 * time.Minute, RoleSessionName: "nightly-job",
	})
	require.NoError(t, err)
	assert.Len(t, creds.AccessKey, 20)
	assert.Len(t, creds.SecretKey, 40)
	assert.Equal(t, 3, strings.Count(creds.SessionToken, ".")+1, "a JWT")
	assert.Equal(t, t0().Add(30*time.Minute), creds.Expiration)
	assert.True(t, creds.IsTemporary())

	got, id, err := svc.LookupCredentials(ctx, creds.AccessKey)
	require.NoError(t, err)
	assert.Equal(t, *creds, got)
	assert.Equal(t, "alice", id.Parent)
	assert.False(t, id.IsRoot)
	assert.Equal(t, []string{"readwrite"}, id.PolicyNames)
	assert.Equal(t, session, id.SessionPolicy)
	assert.Equal(t, "alice", id.Claims["parent"])
	assert.Equal(t, creds.AccessKey, id.Claims["accessKey"])
	assert.Equal(t, "nightly-job", id.Claims["roleSessionName"])
	assert.Equal(t, creds.Expiration.Unix(), id.Claims["exp"])

	// Without a session policy, and for root.
	plain, err := svc.AssumeRole(ctx, iam.AssumeRoleInput{Caller: domain.Identity{AccessKey: "alice"}})
	require.NoError(t, err)
	assert.Equal(t, t0().Add(time.Hour), plain.Expiration, "default duration")
	_, id, err = svc.LookupCredentials(ctx, plain.AccessKey)
	require.NoError(t, err)
	assert.Nil(t, id.SessionPolicy)
	rootCreds, err := svc.AssumeRole(ctx, iam.AssumeRoleInput{Caller: rootID(), Duration: 12 * time.Hour})
	require.NoError(t, err)
	_, id, err = svc.LookupCredentials(ctx, rootCreds.AccessKey)
	require.NoError(t, err)
	assert.True(t, id.IsRoot)
	assert.Equal(t, rootAK, id.Parent)

	// Expiry, then purge on the next AssumeRole.
	clk.Advance(30 * time.Minute)
	_, _, err = svc.LookupCredentials(ctx, creds.AccessKey)
	assert.ErrorIs(t, err, domain.ErrAccessDenied)
	_, _, err = svc.LookupCredentials(ctx, plain.AccessKey)
	require.NoError(t, err)
	_, err = svc.AssumeRole(ctx, iam.AssumeRoleInput{Caller: domain.Identity{AccessKey: "alice"}})
	require.NoError(t, err)
	_, _, err = svc.LookupCredentials(ctx, creds.AccessKey)
	assert.ErrorIs(t, err, domain.ErrInvalidAccessKey, "purged")
	entries, err := store.List(ctx, iam.NamespaceSTS)
	require.NoError(t, err)
	assert.NotContains(t, entries, creds.AccessKey)
	assert.Len(t, entries, 3)

	// A disabled parent disables its sessions.
	require.NoError(t, svc.SetUserStatus(ctx, "alice", domain.StatusDisabled))
	_, _, err = svc.LookupCredentials(ctx, plain.AccessKey)
	assert.ErrorIs(t, err, domain.ErrAccessDenied)
	_, err = svc.AssumeRole(ctx, iam.AssumeRoleInput{Caller: domain.Identity{AccessKey: "alice"}})
	assert.ErrorIs(t, err, domain.ErrAccessDenied)

	// Rotating the root credentials invalidates every token.
	require.NoError(t, svc.SetUserStatus(ctx, "alice", domain.StatusEnabled))
	require.NoError(t, svc.Bootstrap(ctx, domain.Credentials{AccessKey: rootAK, SecretKey: "rotated-root-secret"}))
	_, _, err = svc.LookupCredentials(ctx, plain.AccessKey)
	assert.ErrorIs(t, err, domain.ErrAccessDenied)
}

// covers: S-IAM-07
func TestAssumeRoleValidation(t *testing.T) {
	ctx := context.Background()
	svc, _ := newIAM(t, nil)
	mustUser(t, svc, "alice")
	alice := domain.Identity{AccessKey: "alice"}
	tests := []struct {
		name string
		in   iam.AssumeRoleInput
		want error
	}{
		{"anonymous", iam.AssumeRoleInput{Caller: *domain.Anonymous()}, domain.ErrAccessDenied},
		{"empty caller", iam.AssumeRoleInput{}, domain.ErrAccessDenied},
		{"service account", iam.AssumeRoleInput{Caller: domain.Identity{AccessKey: "AK", Parent: "alice"}}, domain.ErrAccessDenied},
		{"unknown user", iam.AssumeRoleInput{Caller: domain.Identity{AccessKey: "ghost"}}, domain.ErrAccessDenied},
		{"fake root", iam.AssumeRoleInput{Caller: domain.Identity{AccessKey: "alice", IsRoot: true}}, domain.ErrAccessDenied},
		{"root key without root flag", iam.AssumeRoleInput{Caller: domain.Identity{AccessKey: rootAK}}, domain.ErrAccessDenied},
		{"too short", iam.AssumeRoleInput{Caller: alice, Duration: 899 * time.Second}, domain.ErrInvalidArgument},
		{"too long", iam.AssumeRoleInput{Caller: alice, Duration: 43201 * time.Second}, domain.ErrInvalidArgument},
		{"malformed policy", iam.AssumeRoleInput{Caller: alice, Policy: &domain.Policy{Version: "2008-10-17"}}, domain.ErrMalformedPolicy},
		{"bad session name", iam.AssumeRoleInput{Caller: alice, RoleSessionName: "a b"}, domain.ErrInvalidArgument},
		{"short session name", iam.AssumeRoleInput{Caller: alice, RoleSessionName: "a"}, domain.ErrInvalidArgument},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := svc.AssumeRole(ctx, tt.in)
			assert.ErrorIs(t, err, tt.want)
		})
	}

	for _, d := range []time.Duration{900 * time.Second, 43200 * time.Second} {
		_, err := svc.AssumeRole(ctx, iam.AssumeRoleInput{Caller: alice, Duration: d})
		assert.NoError(t, err, d)
	}

	unbootstrapped := iam.New(iam.Deps{Logger: discard()})
	_, err := unbootstrapped.AssumeRole(ctx, iam.AssumeRoleInput{Caller: rootID()})
	assert.ErrorIs(t, err, iam.ErrNotBootstrapped)
}

// TestSTSTokenTampering edits the persisted session tokens: a forged or
// swapped token never resolves.
//
// covers: S-IAM-07
func TestSTSTokenTampering(t *testing.T) {
	ctx := context.Background()
	store := iam.NewMemoryStore()
	svc, _ := newIAM(t, store)
	mustUser(t, svc, "alice")
	mustUser(t, svc, "bob")
	a, err := svc.AssumeRole(ctx, iam.AssumeRoleInput{Caller: domain.Identity{AccessKey: "alice"}})
	require.NoError(t, err)
	b, err := svc.AssumeRole(ctx, iam.AssumeRoleInput{Caller: domain.Identity{AccessKey: "bob"}})
	require.NoError(t, err)

	edit := func(t *testing.T, ak string, change func(rec map[string]any)) {
		raw, err := store.Get(ctx, iam.NamespaceSTS, ak)
		require.NoError(t, err)
		var rec map[string]any
		require.NoError(t, json.Unmarshal(raw, &rec))
		change(rec)
		raw, err = json.Marshal(rec)
		require.NoError(t, err)
		require.NoError(t, store.Put(ctx, iam.NamespaceSTS, ak, raw))
		svc.Invalidate(ctx)
	}

	tests := []struct {
		name   string
		ak     string
		change func(rec map[string]any)
	}{
		{"forged signature", a.AccessKey, func(rec map[string]any) {
			tok := rec["sessionToken"].(string)
			rec["sessionToken"] = tok[:len(tok)-4] + "AAAA"
		}},
		{"not a token", a.AccessKey, func(rec map[string]any) { rec["sessionToken"] = "garbage" }},
		{"token of another key", a.AccessKey, func(rec map[string]any) { rec["sessionToken"] = b.SessionToken }},
		{"parent changed", b.AccessKey, func(rec map[string]any) { rec["parentUser"] = "alice" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			edit(t, tt.ak, tt.change)
			_, _, err := svc.LookupCredentials(ctx, tt.ak)
			assert.ErrorIs(t, err, domain.ErrAccessDenied)
		})
	}

	// Without the root credentials no token can be verified.
	fresh := iam.New(iam.Deps{Store: store, Clock: clock.NewFake(t0()), Logger: discard()})
	_, _, err = fresh.LookupCredentials(ctx, b.AccessKey)
	assert.ErrorIs(t, err, domain.ErrInvalidAccessKey)
}
