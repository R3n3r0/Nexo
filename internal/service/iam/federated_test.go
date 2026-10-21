package iam_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/iam"
)

// covers: S-IAM-08, S-IAM-09
func TestAssumeFederated(t *testing.T) {
	ctx := context.Background()
	svc, clk := newIAM(t, iam.NewMemoryStore())
	_, err := svc.CreateGroup(ctx, iam.CreateGroupInput{Name: "analysts", PolicyNames: []string{"readonly"}})
	require.NoError(t, err)
	_, err = svc.CreateGroup(ctx, iam.CreateGroupInput{Name: "ops", PolicyNames: []string{"diagnostics"}})
	require.NoError(t, err)

	creds, err := svc.AssumeFederated(ctx, iam.FederatedInput{
		Provider: "openid", Subject: "00u1", Username: "ann@example.com",
		Groups: []string{"analysts", "ops", "unknown"}, PolicyNames: []string{"consoleAdmin"},
		Claims: map[string]any{"email": "ann@example.com", "parent": "spoof"}, Duration: 30 * time.Minute,
	})
	require.NoError(t, err)
	assert.Equal(t, t0().Add(30*time.Minute), creds.Expiration)

	_, id, err := svc.LookupCredentials(ctx, creds.AccessKey)
	require.NoError(t, err)
	assert.Equal(t, "openid:ann@example.com", id.Parent)
	assert.Equal(t, "openid:ann@example.com", id.EffectiveUser())
	assert.False(t, id.IsRoot)
	assert.Equal(t, []string{"analysts", "ops"}, id.Groups, "only the existing groups")
	assert.Equal(t, []string{"consoleAdmin", "diagnostics", "readonly"}, id.PolicyNames)
	assert.Equal(t, "ann@example.com", id.Claims["email"])
	assert.Equal(t, "openid:ann@example.com", id.Claims["parent"], "the provider claims never shadow those of the credentials")
	assert.Equal(t, "openid", id.Claims["provider"])

	// A disabled group stops giving its policies at once.
	require.NoError(t, svc.SetGroupStatus(ctx, "ops", domain.StatusDisabled))
	_, id, err = svc.LookupCredentials(ctx, creds.AccessKey)
	require.NoError(t, err)
	assert.Equal(t, []string{"consoleAdmin", "readonly"}, id.PolicyNames)

	// The expiry of the provider token bounds the credentials.
	short, err := svc.AssumeFederated(ctx, iam.FederatedInput{Provider: "ldap", Subject: "uid=bob,dc=x", Username: "bob",
		Groups: []string{"analysts"}, NotAfter: t0().Add(20 * time.Minute)})
	require.NoError(t, err)
	assert.Equal(t, t0().Add(20*time.Minute), short.Expiration)

	// No policy at all, no subject, a bad provider: refused.
	_, err = svc.AssumeFederated(ctx, iam.FederatedInput{Provider: "ldap", Subject: "uid=eve", Username: "eve", Groups: []string{"unknown"}})
	require.ErrorIs(t, err, domain.ErrAccessDenied)
	_, err = svc.AssumeFederated(ctx, iam.FederatedInput{Provider: "ldap", Username: "eve", PolicyNames: []string{"readonly"}})
	require.ErrorIs(t, err, domain.ErrAccessDenied)
	_, err = svc.AssumeFederated(ctx, iam.FederatedInput{Provider: "a:b", Subject: "x", Username: "x", PolicyNames: []string{"readonly"}})
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = svc.AssumeFederated(ctx, iam.FederatedInput{Provider: "ldap", Subject: "x", Username: "x", PolicyNames: []string{"readonly"}, NotAfter: t0().Add(-time.Minute)})
	require.ErrorIs(t, err, domain.ErrAccessDenied)

	// Federated credentials cannot assume a role in turn, and expire.
	_, err = svc.AssumeRole(ctx, iam.AssumeRoleInput{Caller: id})
	require.ErrorIs(t, err, domain.ErrAccessDenied)
	clk.Advance(31 * time.Minute)
	_, _, err = svc.LookupCredentials(ctx, creds.AccessKey)
	require.ErrorIs(t, err, domain.ErrAccessDenied)
}
