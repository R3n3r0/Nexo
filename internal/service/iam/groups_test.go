package iam_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/iam"
	"github.com/r3n3r0/nexo/internal/service/policy"
)

func TestGroupLifecycle(t *testing.T) {
	ctx := context.Background()
	svc, _ := newIAM(t, nil)
	mustUser(t, svc, "alice", "readonly")
	mustUser(t, svc, "bob")
	mustUser(t, svc, "carol")

	g, err := svc.CreateGroup(ctx, iam.CreateGroupInput{Name: "dev team", Members: []string{"bob", "alice", "bob"}, PolicyNames: []string{"readwrite"}})
	require.NoError(t, err)
	assert.Equal(t, &iam.Group{Name: "dev team", Status: domain.StatusEnabled, Members: []string{"alice", "bob"}, PolicyNames: []string{"readwrite"}, CreatedAt: t0(), UpdatedAt: t0()}, g)
	_, err = svc.CreateGroup(ctx, iam.CreateGroupInput{Name: "ops"})
	require.NoError(t, err)

	groups, err := svc.ListGroups(ctx)
	require.NoError(t, err)
	require.Len(t, groups, 2)
	assert.Equal(t, "dev team", groups[0].Name)
	assert.Equal(t, "ops", groups[1].Name)

	require.NoError(t, svc.AddGroupMembers(ctx, "ops", []string{"carol", "alice"}))
	require.NoError(t, svc.RemoveGroupMembers(ctx, "dev team", []string{"bob", "ghost"}))
	require.NoError(t, svc.SetGroupPolicy(ctx, "ops", []string{"diagnostics"}))

	u, err := svc.GetUser(ctx, "alice")
	require.NoError(t, err)
	assert.Equal(t, []string{"dev team", "ops"}, u.Groups)

	_, id, err := svc.LookupCredentials(ctx, "alice")
	require.NoError(t, err)
	assert.Equal(t, []string{"dev team", "ops"}, id.Groups)
	assert.Equal(t, []string{"diagnostics", "readonly", "readwrite"}, id.PolicyNames, "own and group policies")

	// A disabled group grants nothing.
	require.NoError(t, svc.SetGroupStatus(ctx, "dev team", domain.StatusDisabled))
	_, id, err = svc.LookupCredentials(ctx, "alice")
	require.NoError(t, err)
	assert.Equal(t, []string{"ops"}, id.Groups)
	assert.Equal(t, []string{"diagnostics", "readonly"}, id.PolicyNames)
	u, err = svc.GetUser(ctx, "alice")
	require.NoError(t, err)
	assert.Equal(t, []string{"dev team", "ops"}, u.Groups, "membership is still reported")

	require.NoError(t, svc.DeleteGroup(ctx, "ops"))
	_, err = svc.GetGroup(ctx, "ops")
	assert.ErrorIs(t, err, domain.ErrGroupNotFound)
	_, id, err = svc.LookupCredentials(ctx, "carol")
	require.NoError(t, err)
	assert.Empty(t, id.Groups)
	assert.Empty(t, id.PolicyNames)
}

func TestGroupErrors(t *testing.T) {
	ctx := context.Background()
	svc, _ := newIAM(t, nil)
	mustUser(t, svc, "alice")
	_, err := svc.CreateGroup(ctx, iam.CreateGroupInput{Name: "dev"})
	require.NoError(t, err)
	tests := []struct {
		name string
		call func() error
		want error
	}{
		{"empty name", func() error { _, err := svc.CreateGroup(ctx, iam.CreateGroupInput{}); return err }, domain.ErrInvalidArgument},
		{"long name", func() error {
			_, err := svc.CreateGroup(ctx, iam.CreateGroupInput{Name: strings.Repeat("g", 129)})
			return err
		}, domain.ErrInvalidArgument},
		{"comma in name", func() error { _, err := svc.CreateGroup(ctx, iam.CreateGroupInput{Name: "a,b"}); return err }, domain.ErrInvalidArgument},
		{"invalid policy", func() error {
			_, err := svc.CreateGroup(ctx, iam.CreateGroupInput{Name: "x", PolicyNames: []string{""}})
			return err
		}, domain.ErrInvalidArgument},
		{"duplicate", func() error { _, err := svc.CreateGroup(ctx, iam.CreateGroupInput{Name: "dev"}); return err }, domain.ErrGroupAlreadyExists},
		{"unknown member", func() error {
			_, err := svc.CreateGroup(ctx, iam.CreateGroupInput{Name: "x", Members: []string{"ghost"}})
			return err
		}, domain.ErrUserNotFound},
		{"get unknown", func() error { _, err := svc.GetGroup(ctx, "ghost"); return err }, domain.ErrGroupNotFound},
		{"delete unknown", func() error { return svc.DeleteGroup(ctx, "ghost") }, domain.ErrGroupNotFound},
		{"add to unknown", func() error { return svc.AddGroupMembers(ctx, "ghost", []string{"alice"}) }, domain.ErrGroupNotFound},
		{"add unknown member", func() error { return svc.AddGroupMembers(ctx, "dev", []string{"ghost"}) }, domain.ErrUserNotFound},
		{"remove from unknown", func() error { return svc.RemoveGroupMembers(ctx, "ghost", nil) }, domain.ErrGroupNotFound},
		{"status of unknown", func() error { return svc.SetGroupStatus(ctx, "ghost", domain.StatusEnabled) }, domain.ErrGroupNotFound},
		{"invalid status", func() error { return svc.SetGroupStatus(ctx, "dev", "x") }, domain.ErrInvalidArgument},
		{"policy of unknown", func() error { return svc.SetGroupPolicy(ctx, "ghost", nil) }, domain.ErrGroupNotFound},
		{"invalid policy name", func() error { return svc.SetGroupPolicy(ctx, "dev", []string{","}) }, domain.ErrInvalidArgument},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.ErrorIs(t, tt.call(), tt.want)
		})
	}
	_, err = svc.GetGroup(ctx, "x")
	assert.ErrorIs(t, err, domain.ErrGroupNotFound, "failed creations leave nothing behind")
}

func TestPolicyAttachments(t *testing.T) {
	ctx := context.Background()
	svc, _ := newIAM(t, nil)
	mustUser(t, svc, "alice", "readonly")
	mustUser(t, svc, "bob")
	_, err := svc.CreateGroup(ctx, iam.CreateGroupInput{Name: "dev"})
	require.NoError(t, err)

	// The service is usable wherever attachments are read or written.
	var attachments policy.Attachments = svc

	require.NoError(t, attachments.Attach(ctx, policy.AttachInput{User: "alice", PolicyNames: []string{"readwrite", "readonly"}}))
	require.NoError(t, attachments.Attach(ctx, policy.AttachInput{Group: "dev", PolicyNames: []string{"diagnostics"}}))

	got, err := attachments.Attached(ctx, policy.Entity{Kind: policy.EntityUser, Name: "alice"})
	require.NoError(t, err)
	assert.Equal(t, []string{"readonly", "readwrite"}, got)
	got, err = attachments.Attached(ctx, policy.Entity{Kind: policy.EntityGroup, Name: "dev"})
	require.NoError(t, err)
	assert.Equal(t, []string{"diagnostics"}, got)
	got, err = attachments.Attached(ctx, policy.Entity{Kind: policy.EntityUser, Name: "ghost"})
	require.NoError(t, err)
	assert.Nil(t, got, "unknown entities have no policies")
	got, err = attachments.Attached(ctx, policy.Entity{Kind: policy.EntityGroup, Name: "ghost"})
	require.NoError(t, err)
	assert.Nil(t, got)

	users, err := attachments.ListAttached(ctx, policy.EntityUser)
	require.NoError(t, err)
	assert.Equal(t, map[string][]string{"alice": {"readonly", "readwrite"}}, users, "bob has none")
	groups, err := attachments.ListAttached(ctx, policy.EntityGroup)
	require.NoError(t, err)
	assert.Equal(t, map[string][]string{"dev": {"diagnostics"}}, groups)

	require.NoError(t, attachments.Detach(ctx, policy.AttachInput{User: "alice", PolicyNames: []string{"readonly", "unknown"}}))
	require.NoError(t, attachments.Detach(ctx, policy.AttachInput{Group: "dev", PolicyNames: []string{"diagnostics"}}))
	got, err = attachments.Attached(ctx, policy.Entity{Kind: policy.EntityUser, Name: "alice"})
	require.NoError(t, err)
	assert.Equal(t, []string{"readwrite"}, got)
	groups, err = attachments.ListAttached(ctx, policy.EntityGroup)
	require.NoError(t, err)
	assert.Empty(t, groups)

	tests := []struct {
		name string
		call func() error
		want error
	}{
		{"user and group", func() error {
			return attachments.Attach(ctx, policy.AttachInput{User: "alice", Group: "dev", PolicyNames: []string{"x"}})
		}, domain.ErrInvalidArgument},
		{"neither", func() error { return attachments.Attach(ctx, policy.AttachInput{PolicyNames: []string{"x"}}) }, domain.ErrInvalidArgument},
		{"no names", func() error { return attachments.Attach(ctx, policy.AttachInput{User: "alice"}) }, domain.ErrInvalidArgument},
		{"invalid name", func() error {
			return attachments.Attach(ctx, policy.AttachInput{User: "alice", PolicyNames: []string{"a,b"}})
		}, domain.ErrInvalidArgument},
		{"unknown user", func() error {
			return attachments.Attach(ctx, policy.AttachInput{User: "ghost", PolicyNames: []string{"x"}})
		}, domain.ErrUserNotFound},
		{"unknown group", func() error {
			return attachments.Detach(ctx, policy.AttachInput{Group: "ghost", PolicyNames: []string{"x"}})
		}, domain.ErrGroupNotFound},
		{"root", func() error {
			return attachments.Attach(ctx, policy.AttachInput{User: rootAK, PolicyNames: []string{"x"}})
		}, domain.ErrInvalidArgument},
		{"unknown kind", func() error {
			_, err := attachments.Attached(ctx, policy.Entity{Kind: "role", Name: "x"})
			return err
		}, domain.ErrInvalidArgument},
		{"unknown kind listing", func() error { _, err := attachments.ListAttached(ctx, "role"); return err }, domain.ErrInvalidArgument},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.ErrorIs(t, tt.call(), tt.want)
		})
	}
}
