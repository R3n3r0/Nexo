package admin_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/admin"
	authmock "github.com/r3n3r0/nexo/internal/service/auth/mock"
	"github.com/r3n3r0/nexo/internal/service/policy"
	policymock "github.com/r3n3r0/nexo/internal/service/policy/mock"
)

func req(key string) admin.Requester {
	return admin.Requester{Identity: domain.Identity{AccessKey: key}, Conditions: map[string][]string{"aws:SourceIp": {"10.0.0.1"}}}
}

func rootReq() admin.Requester {
	return admin.Requester{Identity: domain.Identity{AccessKey: "root", IsRoot: true}}
}

var errDown = errors.New("store down")

func denyStatement() *domain.Statement { return &domain.Statement{Effect: domain.Deny} }

func TestNewAuthorizerRequiresDeps(t *testing.T) {
	_, err := admin.NewAuthorizer(admin.AuthorizerDeps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

func TestAuthorize(t *testing.T) {
	allow := policy.Decision{Allowed: true}
	implicit := policy.Decision{Reason: "implicit deny"}
	explicit := policy.Decision{Reason: "explicit deny", MatchedStatement: denyStatement()}
	tests := []struct {
		name      string
		in        admin.AuthorizeInput
		decisions map[policy.Action]policy.Decision
		evalErr   error
		want      admin.Decision
		wantErr   error
	}{
		{name: "no action", in: admin.AuthorizeInput{Requester: req("bob")}, wantErr: domain.ErrInvalidArgument},
		{name: "anonymous", in: admin.AuthorizeInput{Requester: admin.Requester{Identity: *domain.Anonymous()}, Actions: []policy.Action{policy.ActionAdminServerInfo}},
			want: admin.Decision{DeniedAction: policy.ActionAdminServerInfo, Reason: "authentication required"}},
		{name: "allowed", in: admin.AuthorizeInput{Requester: req("bob"), Actions: []policy.Action{policy.ActionGetObject, policy.ActionPutObject}, Bucket: "b", Object: "k"},
			decisions: map[policy.Action]policy.Decision{policy.ActionGetObject: allow, policy.ActionPutObject: allow}, want: admin.Decision{Allowed: true}},
		{name: "first denied action decides", in: admin.AuthorizeInput{Requester: req("bob"), Actions: []policy.Action{policy.ActionGetObject, policy.ActionPutObject}},
			decisions: map[policy.Action]policy.Decision{policy.ActionGetObject: allow, policy.ActionPutObject: implicit},
			want:      admin.Decision{DeniedAction: policy.ActionPutObject, Reason: "implicit deny"}},
		{name: "self-service overrides an implicit deny", in: admin.AuthorizeInput{Requester: req("bob"), Actions: []policy.Action{policy.ActionAdminCreateAccessKey}, SelfService: true},
			decisions: map[policy.Action]policy.Decision{policy.ActionAdminCreateAccessKey: implicit},
			want:      admin.Decision{Allowed: true, SelfService: true, Reason: "self-service: the requester manages its own resources only"}},
		{name: "self-service never overrides an explicit deny", in: admin.AuthorizeInput{Requester: req("bob"), Actions: []policy.Action{policy.ActionAdminCreateAccessKey}, SelfService: true},
			decisions: map[policy.Action]policy.Decision{policy.ActionAdminCreateAccessKey: explicit},
			want:      admin.Decision{DeniedAction: policy.ActionAdminCreateAccessKey, Reason: "explicit deny"}},
		{name: "self-service with an allow is not self-service", in: admin.AuthorizeInput{Requester: req("bob"), Actions: []policy.Action{policy.ActionAdminCreateAccessKey}, SelfService: true},
			decisions: map[policy.Action]policy.Decision{policy.ActionAdminCreateAccessKey: allow}, want: admin.Decision{Allowed: true}},
		{name: "engine failure", in: admin.AuthorizeInput{Requester: req("bob"), Actions: []policy.Action{policy.ActionGetObject}}, evalErr: errDown, wantErr: errDown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			engine := policymock.NewService(t)
			for action, d := range tt.decisions {
				engine.EXPECT().Evaluate(mock.Anything, mock.MatchedBy(func(a policy.Args) bool {
					return a.Action == action && a.Bucket == tt.in.Bucket && a.Object == tt.in.Object && a.Conditions["aws:SourceIp"][0] == "10.0.0.1"
				})).Return(d, nil).Maybe()
			}
			if tt.evalErr != nil {
				engine.EXPECT().Evaluate(mock.Anything, mock.Anything).Return(policy.Decision{}, tt.evalErr)
			}
			a, err := admin.NewAuthorizer(admin.AuthorizerDeps{Policy: engine, Creds: authmock.NewCredentialLookup(t)})
			require.NoError(t, err)
			got, err := a.Authorize(context.Background(), tt.in)
			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestAuthorizeMergesConditions(t *testing.T) {
	engine := policymock.NewService(t)
	engine.EXPECT().Evaluate(mock.Anything, mock.MatchedBy(func(a policy.Args) bool {
		return a.Conditions["aws:SourceIp"][0] == "10.0.0.1" && a.Conditions["s3:prefix"][0] == "docs/"
	})).Return(policy.Decision{Allowed: true}, nil)
	a, err := admin.NewAuthorizer(admin.AuthorizerDeps{Policy: engine, Creds: authmock.NewCredentialLookup(t)})
	require.NoError(t, err)
	r := req("bob")
	d, err := a.Authorize(context.Background(), admin.AuthorizeInput{Requester: r, Actions: []policy.Action{policy.ActionListBucket}, Bucket: "b",
		Conditions: map[string][]string{"s3:prefix": {"docs/"}}})
	require.NoError(t, err)
	assert.True(t, d.Allowed)
	assert.NotContains(t, r.Conditions, "s3:prefix", "the requester's conditions are not modified")
}

func TestPermissions(t *testing.T) {
	ctx := context.Background()
	engine := policymock.NewService(t)
	// bob may read objects under docs/ and list the bucket; nothing else.
	engine.EXPECT().Evaluate(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, a policy.Args) (policy.Decision, error) {
		switch {
		case a.Action == policy.ActionGetObject && a.Bucket == "bkt" && a.Object == "docs/*":
			return policy.Decision{Allowed: true}, nil
		case a.Action == policy.ActionListBucket && a.Bucket == "bkt" && a.Object == "":
			return policy.Decision{Allowed: true}, nil
		case a.Identity.AccessKey == "alice" && a.Action == policy.ActionAdminServerInfo && a.Bucket == "":
			return policy.Decision{Allowed: true}, nil
		}
		return policy.Decision{}, nil
	})
	creds := authmock.NewCredentialLookup(t)
	creds.EXPECT().LookupCredentials(mock.Anything, "alice").Return(domain.Credentials{}, domain.Identity{PolicyNames: []string{"diagnostics"}}, nil)
	creds.EXPECT().LookupCredentials(mock.Anything, "ghost").Return(domain.Credentials{}, domain.Identity{}, fmt.Errorf("x: %w", domain.ErrInvalidAccessKey))
	a, err := admin.NewAuthorizer(admin.AuthorizerDeps{Policy: engine, Creds: creds})
	require.NoError(t, err)

	p, err := a.Permissions(ctx, admin.PermissionsInput{Requester: req("bob"), Bucket: "bkt", Prefix: "docs/"})
	require.NoError(t, err)
	assert.Equal(t, "bob", p.User)
	assert.Equal(t, []policy.Action{policy.ActionGetObject, policy.ActionListBucket}, p.Allowed)
	assert.Equal(t, len(admin.CatalogActions()), len(p.Allowed)+len(p.Denied))
	assert.Contains(t, p.Denied, policy.ActionPutObject)

	p, err = a.Permissions(ctx, admin.PermissionsInput{Requester: rootReq(), User: "alice"})
	require.NoError(t, err)
	assert.Equal(t, "alice", p.User)
	assert.Equal(t, []policy.Action{policy.ActionAdminServerInfo}, p.Allowed)

	_, err = a.Permissions(ctx, admin.PermissionsInput{Requester: rootReq(), User: "ghost"})
	assert.ErrorIs(t, err, domain.ErrUserNotFound)
	_, err = a.Permissions(ctx, admin.PermissionsInput{Requester: req("bob"), Prefix: "docs/"})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = a.Permissions(ctx, admin.PermissionsInput{Requester: admin.Requester{Identity: *domain.Anonymous()}})
	assert.ErrorIs(t, err, domain.ErrUnauthenticated)
}

func TestCatalogIsComplete(t *testing.T) {
	seen := map[policy.Action]bool{}
	for _, a := range admin.CatalogActions() {
		assert.False(t, seen[a], "duplicate %s", a)
		seen[a] = true
	}
	for _, a := range []policy.Action{
		policy.ActionAdminServerInfo, policy.ActionAdminDataUsage, policy.ActionAdminCreateUser, policy.ActionAdminDeleteUser,
		policy.ActionAdminListUsers, policy.ActionAdminGetUser, policy.ActionAdminEnableUser, policy.ActionAdminDisableUser,
		policy.ActionAdminCreatePolicy, policy.ActionAdminDeletePolicy, policy.ActionAdminGetPolicy, policy.ActionAdminListUserPolicies,
		policy.ActionAdminAttachUserOrGroupPolicy, policy.ActionAdminSimulatePolicy, policy.ActionAdminAddUserToGroup,
		policy.ActionAdminRemoveUserFromGroup, policy.ActionAdminGetGroup, policy.ActionAdminListGroups, policy.ActionAdminEnableGroup,
		policy.ActionAdminDisableGroup, policy.ActionAdminCreateAccessKey, policy.ActionAdminUpdateAccessKey,
		policy.ActionAdminRemoveServiceAccount, policy.ActionAdminListAccessKeys, policy.ActionAdminConfigWrite,
		policy.ActionAdminServerTrace, policy.ActionAdminHeal, policy.ActionAdminServiceRestart, policy.ActionAdminManageShares,
	} {
		assert.True(t, seen[a], "the catalog lacks %s", a)
	}
}

// TestAuthorizePartial: with Partial an action granted under conditions
// the request does not meet (the s3:prefix of a home directory) is
// allowed, unless an explicit Deny applies or the inline policy of derived
// credentials grants nothing.
func TestAuthorizePartial(t *testing.T) {
	allowStmt := &domain.Statement{Effect: domain.Allow}
	evaluated := func(es ...policy.EvaluatedStatement) policy.Decision {
		return policy.Decision{Reason: "no policy allows s3:ListBucket", Evaluated: es}
	}
	tests := []struct {
		name      string
		simulated policy.Decision
		want      bool
	}{
		{"condition only", evaluated(policy.EvaluatedStatement{Source: policy.SourceIAM, Effect: domain.Allow, Mismatch: policy.MismatchCondition, Statement: allowStmt}), true},
		{"bucket policy condition", evaluated(policy.EvaluatedStatement{Source: policy.SourceBucket, Effect: domain.Allow, Mismatch: policy.MismatchCondition, Statement: allowStmt}), true},
		{"other resource", evaluated(policy.EvaluatedStatement{Source: policy.SourceIAM, Effect: domain.Allow, Mismatch: policy.MismatchResource, Statement: allowStmt}), false},
		{"other action", evaluated(policy.EvaluatedStatement{Source: policy.SourceIAM, Effect: domain.Allow, Mismatch: policy.MismatchAction, Statement: allowStmt}), false},
		{"inline grants nothing", evaluated(
			policy.EvaluatedStatement{Source: policy.SourceIAM, Effect: domain.Allow, Mismatch: policy.MismatchCondition, Statement: allowStmt},
			policy.EvaluatedStatement{Source: policy.SourceInline, Effect: domain.Allow, Mismatch: policy.MismatchResource, Statement: allowStmt}), false},
		{"inline grants under conditions", evaluated(
			policy.EvaluatedStatement{Source: policy.SourceIAM, Effect: domain.Allow, Mismatch: policy.MismatchNone, Statement: allowStmt},
			policy.EvaluatedStatement{Source: policy.SourceInline, Effect: domain.Allow, Mismatch: policy.MismatchCondition, Statement: allowStmt}), true},
		{"a conditional deny does not hide", evaluated(
			policy.EvaluatedStatement{Source: policy.SourceIAM, Effect: domain.Allow, Mismatch: policy.MismatchCondition, Statement: allowStmt},
			policy.EvaluatedStatement{Source: policy.SourceBucket, Effect: domain.Deny, Mismatch: policy.MismatchCondition, Statement: denyStatement()}), true},
		{"nothing", evaluated(), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			engine := policymock.NewService(t)
			engine.EXPECT().Evaluate(mock.Anything, mock.Anything).Return(policy.Decision{Reason: "no policy allows s3:ListBucket"}, nil)
			engine.EXPECT().Simulate(mock.Anything, mock.MatchedBy(func(a policy.Args) bool {
				return a.Action == policy.ActionListBucket && a.Bucket == "homes"
			})).Return(tt.simulated, nil)
			a, err := admin.NewAuthorizer(admin.AuthorizerDeps{Policy: engine, Creds: authmock.NewCredentialLookup(t)})
			require.NoError(t, err)
			got, err := a.Authorize(context.Background(), admin.AuthorizeInput{Requester: req("gina"), Actions: []policy.Action{policy.ActionListBucket}, Bucket: "homes", Partial: true})
			require.NoError(t, err)
			assert.Equal(t, tt.want, got.Allowed)
		})
	}

	t.Run("an explicit deny is final", func(t *testing.T) {
		engine := policymock.NewService(t)
		engine.EXPECT().Evaluate(mock.Anything, mock.Anything).Return(policy.Decision{Reason: "explicit deny", MatchedStatement: denyStatement()}, nil)
		a, err := admin.NewAuthorizer(admin.AuthorizerDeps{Policy: engine, Creds: authmock.NewCredentialLookup(t)})
		require.NoError(t, err)
		got, err := a.Authorize(context.Background(), admin.AuthorizeInput{Requester: req("gina"), Actions: []policy.Action{policy.ActionListBucket}, Bucket: "homes", Partial: true})
		require.NoError(t, err)
		assert.False(t, got.Allowed)
	})
	t.Run("without Partial the conditions decide", func(t *testing.T) {
		engine := policymock.NewService(t)
		engine.EXPECT().Evaluate(mock.Anything, mock.Anything).Return(policy.Decision{Reason: "no policy allows s3:ListBucket"}, nil)
		a, err := admin.NewAuthorizer(admin.AuthorizerDeps{Policy: engine, Creds: authmock.NewCredentialLookup(t)})
		require.NoError(t, err)
		got, err := a.Authorize(context.Background(), admin.AuthorizeInput{Requester: req("gina"), Actions: []policy.Action{policy.ActionListBucket}, Bucket: "homes"})
		require.NoError(t, err)
		assert.False(t, got.Allowed)
	})
}
