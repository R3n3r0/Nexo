package admin_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/accessrule"
	accessrulemock "github.com/r3n3r0/nexo/internal/service/accessrule/mock"
	"github.com/r3n3r0/nexo/internal/service/admin"
	adminmock "github.com/r3n3r0/nexo/internal/service/admin/mock"
	iammock "github.com/r3n3r0/nexo/internal/service/iam/mock"
	"github.com/r3n3r0/nexo/internal/service/match"
	"github.com/r3n3r0/nexo/internal/service/policy"
)

func TestNewAccessRuleManagerRequiresDeps(t *testing.T) {
	_, err := admin.NewAccessRuleManager(admin.AccessRuleManagerDeps{})
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = admin.NewAccessRuleManager(admin.AccessRuleManagerDeps{Rules: accessrulemock.NewService(t), Authz: adminmock.NewAuthorizer(t)})
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
}

func requesterOf(key string) admin.Requester {
	return admin.Requester{Identity: domain.Identity{AccessKey: key}}
}

func TestAccessRuleManagerAttributesChanges(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name  string
		req   admin.Requester
		actor string
	}{
		{"user", requesterOf("mario"), "mario"},
		{"service account", admin.Requester{Identity: domain.Identity{AccessKey: "AK1", Parent: "mario"}}, "mario/AK1"},
		{"root", admin.Requester{Identity: domain.Identity{AccessKey: "admin", IsRoot: true}}, "admin"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rules := accessrulemock.NewService(t)
			m, err := admin.NewAccessRuleManager(admin.AccessRuleManagerDeps{Rules: rules, Authz: allowAll(t), Creds: iammock.NewService(t)})
			require.NoError(t, err)
			want := &accessrule.Config{Bucket: "media", ETag: "e2"}
			rules.EXPECT().Put(mock.Anything, accessrule.PutInput{Bucket: "media", Rules: []accessrule.Rule{}, IfMatch: "e1", Actor: tc.actor}).Return(want, nil)
			got, err := m.Put(ctx, admin.PutAccessRulesInput{Requester: tc.req, Bucket: "media", IfMatch: "e1"})
			require.NoError(t, err)
			assert.Same(t, want, got)

			pattern := &match.Pattern{Type: match.TypeGlob, Pattern: "pub/**"}
			rules.EXPECT().ApplyPreset(mock.Anything, accessrule.PresetInput{Bucket: "media", Preset: accessrule.PresetPublicPrefix, Match: pattern, Actor: tc.actor}).Return(want, nil)
			_, err = m.ApplyPreset(ctx, admin.AccessPresetInput{Requester: tc.req, Bucket: "media", Preset: accessrule.PresetPublicPrefix, Match: pattern})
			require.NoError(t, err)

			rules.EXPECT().SetBlockPublic(mock.Anything, accessrule.BlockPublicInput{Enabled: true, Mode: accessrule.BlockModeIgnore, Actor: tc.actor}).
				Return(&accessrule.BlockPublic{Enabled: true}, nil)
			_, err = m.SetBlockPublic(ctx, admin.SetBlockPublicInput{Requester: tc.req, Enabled: true, Mode: accessrule.BlockModeIgnore})
			require.NoError(t, err)
		})
	}
}

func TestAccessRuleManagerSetBlockPublicValidatesMode(t *testing.T) {
	m, err := admin.NewAccessRuleManager(admin.AccessRuleManagerDeps{Rules: accessrulemock.NewService(t), Authz: allowAll(t), Creds: iammock.NewService(t)})
	require.NoError(t, err)
	_, err = m.SetBlockPublic(context.Background(), admin.SetBlockPublicInput{Bucket: "media", Enabled: true, Mode: accessrule.BlockModeReject})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument, "the mode is a cluster setting")
	_, err = m.SetBlockPublic(context.Background(), admin.SetBlockPublicInput{Enabled: true, Mode: "sometimes"})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

func TestAccessRuleManagerTestPrincipals(t *testing.T) {
	ctx := context.Background()
	mario := domain.Identity{AccessKey: "mario", PolicyNames: []string{"readonly"}}
	tests := []struct {
		name      string
		principal string
		requester string
		// simulate is whether admin:SimulatePolicy is evaluated.
		simulate bool
		identity *domain.Identity
		dist     string
		wantErr  error
	}{
		{name: "anonymous by default", requester: "admin"},
		{name: "anonymous", principal: "anonymous", requester: "admin"},
		{name: "everyone", principal: "*", requester: "admin"},
		{name: "distribution", principal: "distribution:site", requester: "admin", dist: "site"},
		{name: "user of another", principal: "user:mario", requester: "admin", simulate: true, identity: &mario},
		{name: "bare access key", principal: "mario", requester: "admin", simulate: true, identity: &mario},
		{name: "access key", principal: "accesskey:mario", requester: "admin", simulate: true, identity: &mario},
		{name: "the requester itself", principal: "user:mario", requester: "mario", identity: &mario},
		{name: "unknown user", principal: "user:nobody", requester: "admin", wantErr: domain.ErrUserNotFound},
		{name: "group", principal: "group:staff", requester: "admin", wantErr: domain.ErrInvalidArgument},
		{name: "empty distribution", principal: "distribution:", requester: "admin", wantErr: domain.ErrInvalidArgument},
		{name: "unknown form", principal: "role:x", requester: "admin", wantErr: domain.ErrInvalidArgument},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rules := accessrulemock.NewService(t)
			creds := iammock.NewService(t)
			creds.EXPECT().LookupCredentials(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, key string) (domain.Credentials, domain.Identity, error) {
				if key == "mario" {
					return domain.Credentials{AccessKey: key}, domain.Identity{PolicyNames: []string{"readonly"}}, nil
				}
				return domain.Credentials{}, domain.Identity{}, fmt.Errorf("%w: %s", domain.ErrInvalidAccessKey, key)
			}).Maybe()
			var asked []policy.Action
			authz := allowing(t, func(in admin.AuthorizeInput) bool {
				asked = append(asked, in.Actions...)
				return true
			})
			m, err := admin.NewAccessRuleManager(admin.AccessRuleManagerDeps{Rules: rules, Authz: authz, Creds: creds})
			require.NoError(t, err)
			if tc.wantErr == nil {
				rules.EXPECT().Test(mock.Anything, mock.MatchedBy(func(in accessrule.TestInput) bool {
					return in.Bucket == "media" && assert.Equal(t, tc.identity, in.Identity) && in.Distribution == tc.dist &&
						in.IP == "10.1.2.3" && assert.Equal(t, []string{"a.jpg"}, in.Keys)
				})).Return(&accessrule.TestResult{Bucket: "media"}, nil)
			}
			_, err = m.Test(ctx, admin.TestAccessRulesInput{Requester: requesterOf(tc.requester), Bucket: "media", Keys: []string{"a.jpg"}, Principal: tc.principal, IP: "10.1.2.3"})
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			want := []policy.Action{policy.ActionAdminManageAccessRules}
			if tc.simulate {
				want = append(want, policy.ActionAdminSimulatePolicy)
			}
			assert.Equal(t, want, asked)
		})
	}
}

func TestAccessRuleManagerTestDenied(t *testing.T) {
	ctx := context.Background()
	creds := iammock.NewService(t)
	creds.EXPECT().LookupCredentials(mock.Anything, "mario").Return(domain.Credentials{AccessKey: "mario"}, domain.Identity{}, nil).Maybe()
	tests := []struct {
		name      string
		principal string
		allow     func(in admin.AuthorizeInput) bool
		denied    policy.Action
	}{
		{"no ManageAccessRules", "anonymous", func(admin.AuthorizeInput) bool { return false }, policy.ActionAdminManageAccessRules},
		{"another user without SimulatePolicy", "user:mario", func(in admin.AuthorizeInput) bool {
			return in.Actions[0] == policy.ActionAdminManageAccessRules && in.Bucket == "media"
		}, policy.ActionAdminSimulatePolicy},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, err := admin.NewAccessRuleManager(admin.AccessRuleManagerDeps{Rules: accessrulemock.NewService(t), Authz: allowing(t, tc.allow), Creds: creds})
			require.NoError(t, err)
			_, err = m.Test(ctx, admin.TestAccessRulesInput{Requester: requesterOf("luigi"), Bucket: "media", Principal: tc.principal})
			require.ErrorIs(t, err, domain.ErrAccessDenied)
			assert.Contains(t, err.Error(), string(tc.denied))
		})
	}
}

func TestAccessRuleManagerTestConflictingDistribution(t *testing.T) {
	m, err := admin.NewAccessRuleManager(admin.AccessRuleManagerDeps{Rules: accessrulemock.NewService(t), Authz: allowAll(t), Creds: iammock.NewService(t)})
	require.NoError(t, err)
	_, err = m.Test(context.Background(), admin.TestAccessRulesInput{Requester: requesterOf("admin"), Bucket: "media", Principal: "distribution:a", Distribution: "b"})
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = m.Test(context.Background(), admin.TestAccessRulesInput{Requester: requesterOf("admin"), Bucket: "Not_A_Bucket"})
	require.ErrorIs(t, err, domain.ErrInvalidBucketName)
}
