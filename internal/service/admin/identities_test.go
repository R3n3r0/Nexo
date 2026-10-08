package admin_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/iam"
	iammock "github.com/r3n3r0/nexo/internal/service/iam/mock"
	"github.com/r3n3r0/nexo/internal/service/policy"
	sharemock "github.com/r3n3r0/nexo/internal/service/share/mock"
)

// recordingAuthz allows everything but the denied actions and records the
// actions asked.
func recordingAuthz(t *testing.T, asked *[]policy.Action, deny ...policy.Action) admin.Authorizer {
	return allowing(t, func(in admin.AuthorizeInput) bool {
		*asked = append(*asked, in.Actions...)
		for _, a := range in.Actions {
			for _, d := range deny {
				if a == d {
					return false
				}
			}
		}
		return true
	})
}

func newIdentityManager(t *testing.T, authz admin.Authorizer) (admin.IdentityManager, *iammock.Service, *sharemock.Service) {
	users, shares := iammock.NewService(t), sharemock.NewService(t)
	m, err := admin.NewIdentityManager(admin.IdentityManagerDeps{IAM: users, Authz: authz, Shares: shares})
	require.NoError(t, err)
	return m, users, shares
}

func TestCreateUser(t *testing.T) {
	ctx := context.Background()
	t.Run("with groups and policies", func(t *testing.T) {
		var asked []policy.Action
		m, users, _ := newIdentityManager(t, recordingAuthz(t, &asked))
		users.EXPECT().GetGroup(mock.Anything, "staff").Return(&iam.Group{Name: "staff"}, nil)
		users.EXPECT().CreateUser(mock.Anything, iam.CreateUserInput{AccessKey: "alice", SecretKey: "secret12", PolicyNames: []string{"readonly"}}).
			Return(&iam.User{AccessKey: "alice"}, nil)
		users.EXPECT().AddGroupMembers(mock.Anything, "staff", []string{"alice"}).Return(nil)
		users.EXPECT().GetUser(mock.Anything, "alice").Return(&iam.User{AccessKey: "alice", Groups: []string{"staff"}}, nil)
		u, err := m.CreateUser(ctx, admin.CreateUserInput{Requester: rootReq(), AccessKey: "alice", SecretKey: "secret12",
			PolicyNames: []string{"readonly"}, Groups: []string{"staff", "staff", ""}})
		require.NoError(t, err)
		assert.Equal(t, []string{"staff"}, u.Groups)
		assert.Equal(t, []policy.Action{policy.ActionAdminCreateUser, policy.ActionAdminAddUserToGroup, policy.ActionAdminAttachUserOrGroupPolicy}, asked)
	})
	t.Run("unknown group creates nothing", func(t *testing.T) {
		var asked []policy.Action
		m, users, _ := newIdentityManager(t, recordingAuthz(t, &asked))
		users.EXPECT().GetGroup(mock.Anything, "nope").Return(nil, fmt.Errorf("x: %w", domain.ErrGroupNotFound))
		_, err := m.CreateUser(ctx, admin.CreateUserInput{Requester: rootReq(), AccessKey: "alice", SecretKey: "secret12", Groups: []string{"nope"}})
		assert.ErrorIs(t, err, domain.ErrGroupNotFound)
	})
	t.Run("a failed membership removes the user", func(t *testing.T) {
		var asked []policy.Action
		m, users, _ := newIdentityManager(t, recordingAuthz(t, &asked))
		users.EXPECT().GetGroup(mock.Anything, "staff").Return(&iam.Group{Name: "staff"}, nil)
		users.EXPECT().CreateUser(mock.Anything, mock.Anything).Return(&iam.User{AccessKey: "alice"}, nil)
		users.EXPECT().AddGroupMembers(mock.Anything, "staff", []string{"alice"}).Return(errDown)
		users.EXPECT().DeleteUser(mock.Anything, "alice").Return(nil)
		_, err := m.CreateUser(ctx, admin.CreateUserInput{Requester: rootReq(), AccessKey: "alice", SecretKey: "secret12", Groups: []string{"staff"}})
		assert.ErrorIs(t, err, errDown)
	})
	t.Run("membership denied", func(t *testing.T) {
		var asked []policy.Action
		m, _, _ := newIdentityManager(t, recordingAuthz(t, &asked, policy.ActionAdminAddUserToGroup))
		_, err := m.CreateUser(ctx, admin.CreateUserInput{Requester: req("bob"), AccessKey: "alice", SecretKey: "secret12", Groups: []string{"staff"}})
		assert.ErrorIs(t, err, domain.ErrAccessDenied)
	})
}

func TestUpdateUser(t *testing.T) {
	ctx := context.Background()
	disabled := domain.StatusDisabled
	secret := "newsecret1"
	groups := []string{"b", "c"}
	tests := []struct {
		name    string
		in      admin.UpdateUserInput
		deny    []policy.Action
		setup   func(users *iammock.Service, shares *sharemock.Service)
		asked   []policy.Action
		wantErr error
	}{
		{name: "status and secret", in: admin.UpdateUserInput{AccessKey: "alice", Status: &disabled, SecretKey: &secret},
			setup: func(users *iammock.Service, shares *sharemock.Service) {
				users.EXPECT().SetUserSecret(mock.Anything, "alice", secret).Return(nil)
				// Rotating the secret revokes the share links of the user.
				shares.EXPECT().RevokeAll(mock.Anything, domain.Identity{IsRoot: true}, "alice").Return(2, nil)
				users.EXPECT().SetUserStatus(mock.Anything, "alice", domain.StatusDisabled).Return(nil)
				users.EXPECT().GetUser(mock.Anything, "alice").Return(&iam.User{AccessKey: "alice"}, nil)
			},
			asked: []policy.Action{policy.ActionAdminCreateUser, policy.ActionAdminDisableUser}},
		{name: "groups are diffed", in: admin.UpdateUserInput{AccessKey: "alice", Groups: &groups},
			setup: func(users *iammock.Service, _ *sharemock.Service) {
				users.EXPECT().GetUser(mock.Anything, "alice").Return(&iam.User{AccessKey: "alice", Groups: []string{"a", "b"}}, nil)
				users.EXPECT().AddGroupMembers(mock.Anything, "c", []string{"alice"}).Return(nil)
				users.EXPECT().RemoveGroupMembers(mock.Anything, "a", []string{"alice"}).Return(nil)
			},
			asked: []policy.Action{policy.ActionAdminGetUser, policy.ActionAdminAddUserToGroup, policy.ActionAdminRemoveUserFromGroup}},
		{name: "removal denied", in: admin.UpdateUserInput{AccessKey: "alice", Groups: &groups}, deny: []policy.Action{policy.ActionAdminRemoveUserFromGroup},
			setup: func(users *iammock.Service, _ *sharemock.Service) {
				users.EXPECT().GetUser(mock.Anything, "alice").Return(&iam.User{AccessKey: "alice", Groups: []string{"a", "b"}}, nil)
			},
			wantErr: domain.ErrAccessDenied},
		{name: "status denied before anything", in: admin.UpdateUserInput{AccessKey: "alice", Status: &disabled}, deny: []policy.Action{policy.ActionAdminDisableUser},
			wantErr: domain.ErrAccessDenied},
		{name: "nothing to change reads the user", in: admin.UpdateUserInput{AccessKey: "alice"},
			setup: func(users *iammock.Service, _ *sharemock.Service) {
				users.EXPECT().GetUser(mock.Anything, "alice").Return(&iam.User{AccessKey: "alice"}, nil)
			},
			asked: []policy.Action{policy.ActionAdminGetUser}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var asked []policy.Action
			m, users, shares := newIdentityManager(t, recordingAuthz(t, &asked, tt.deny...))
			if tt.setup != nil {
				tt.setup(users, shares)
			}
			tt.in.Requester = req("bob")
			_, err := m.UpdateUser(ctx, tt.in)
			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.asked, asked)
		})
	}
	t.Run("invalid status", func(t *testing.T) {
		var asked []policy.Action
		m, _, _ := newIdentityManager(t, recordingAuthz(t, &asked))
		bad := domain.CredentialStatus("sleeping")
		_, err := m.UpdateUser(ctx, admin.UpdateUserInput{Requester: req("bob"), AccessKey: "alice", Status: &bad})
		assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	})
}

func TestGroups(t *testing.T) {
	ctx := context.Background()
	var asked []policy.Action
	m, users, _ := newIdentityManager(t, recordingAuthz(t, &asked))
	users.EXPECT().CreateGroup(mock.Anything, iam.CreateGroupInput{Name: "g", Members: []string{"a", "b"}, PolicyNames: []string{"readonly"}}).Return(&iam.Group{Name: "g"}, nil)
	_, err := m.CreateGroup(ctx, admin.CreateGroupInput{Requester: rootReq(), Name: "g", Members: []string{"b", "a", "b"}, PolicyNames: []string{"readonly"}})
	require.NoError(t, err)
	assert.Equal(t, []policy.Action{policy.ActionAdminAddUserToGroup, policy.ActionAdminAttachUserOrGroupPolicy}, asked)

	asked = nil
	enabled := domain.StatusEnabled
	users.EXPECT().SetGroupStatus(mock.Anything, "g", domain.StatusEnabled).Return(nil)
	users.EXPECT().RemoveGroupMembers(mock.Anything, "g", []string{"a"}).Return(nil)
	users.EXPECT().GetGroup(mock.Anything, "g").Return(&iam.Group{Name: "g"}, nil)
	_, err = m.UpdateGroup(ctx, admin.UpdateGroupInput{Requester: rootReq(), Name: "g", Status: &enabled, RemoveMembers: []string{"a"}})
	require.NoError(t, err)
	assert.Equal(t, []policy.Action{policy.ActionAdminEnableGroup, policy.ActionAdminRemoveUserFromGroup}, asked)
}

func TestAccessKeyManager(t *testing.T) {
	ctx := context.Background()
	self := adminAuthorizer(t, func(in admin.AuthorizeInput) admin.Decision {
		switch in.Requester.Identity.AccessKey {
		case "admin1":
			return admin.Decision{Allowed: true}
		case "denied":
			return admin.Decision{DeniedAction: in.Actions[0], Reason: "explicit deny"}
		}
		return admin.Decision{Allowed: true, SelfService: true}
	})
	users := iammock.NewService(t)
	m, err := admin.NewAccessKeyManager(admin.AccessKeyManagerDeps{IAM: users, Authz: self})
	require.NoError(t, err)

	// A user creates its own key as itself.
	users.EXPECT().CreateAccessKey(mock.Anything, iam.CreateAccessKeyInput{Caller: domain.Identity{AccessKey: "bob"}, ParentUser: "bob", Name: "ci"}).
		Return(&iam.AccessKey{AccessKey: "K1", ParentUser: "bob"}, nil)
	_, err = m.Create(ctx, req("bob"), iam.CreateAccessKeyInput{Name: "ci", Caller: domain.Identity{AccessKey: "forged", IsRoot: true}})
	require.NoError(t, err, "the caller of the input is replaced")

	// An administrator acts with root semantics, for itself by default.
	users.EXPECT().LookupCredentials(mock.Anything, "admin1").Return(domain.Credentials{}, domain.Identity{AccessKey: "admin1"}, nil)
	users.EXPECT().CreateAccessKey(mock.Anything, iam.CreateAccessKeyInput{Caller: domain.Identity{IsRoot: true}, ParentUser: "admin1"}).
		Return(&iam.AccessKey{AccessKey: "K2", ParentUser: "admin1"}, nil)
	_, err = m.Create(ctx, req("admin1"), iam.CreateAccessKeyInput{})
	require.NoError(t, err)
	users.EXPECT().LookupCredentials(mock.Anything, "carol").Return(domain.Credentials{}, domain.Identity{AccessKey: "carol"}, nil)
	users.EXPECT().ListAccessKeys(mock.Anything, domain.Identity{IsRoot: true}, "carol").Return(nil, nil)
	_, err = m.List(ctx, req("admin1"), "carol", admin.Page{})
	require.NoError(t, err)

	// Derived credentials stay themselves (IAM refuses them).
	derived := admin.Requester{Identity: domain.Identity{AccessKey: "svc", Parent: "admin1"}}
	users.EXPECT().DeleteAccessKey(mock.Anything, derived.Identity, "K2").Return(fmt.Errorf("x: %w", domain.ErrAccessDenied))
	assert.ErrorIs(t, m.Delete(ctx, derived, "K2"), domain.ErrAccessDenied)

	_, err = m.Get(ctx, req("denied"), "K1")
	assert.ErrorIs(t, err, domain.ErrAccessDenied)
	users.EXPECT().UpdateAccessKey(mock.Anything, domain.Identity{AccessKey: "bob"}, "K1", iam.UpdateAccessKeyInput{ClearPolicy: true}).Return(&iam.AccessKey{}, nil)
	_, err = m.Update(ctx, req("bob"), "K1", iam.UpdateAccessKeyInput{ClearPolicy: true})
	require.NoError(t, err)
}

// covers: docs/09 §9.1 point 3 (rotating the secret of a user revokes its
// managed share links; regression: the links kept working)
func TestUpdateUserSecretRevokesShares(t *testing.T) {
	ctx := context.Background()
	secret := "rotated-secret-1"
	t.Run("revoked", func(t *testing.T) {
		m, users, shares := newIdentityManager(t, allowAll(t))
		users.EXPECT().SetUserSecret(mock.Anything, "erin", secret).Return(nil)
		shares.EXPECT().RevokeAll(mock.Anything, domain.Identity{IsRoot: true}, "erin").Return(3, nil)
		users.EXPECT().GetUser(mock.Anything, "erin").Return(&iam.User{AccessKey: "erin"}, nil)
		_, err := m.UpdateUser(ctx, admin.UpdateUserInput{Requester: req("admin1"), AccessKey: "erin", SecretKey: &secret})
		require.NoError(t, err)
	})
	t.Run("a failed revocation is reported", func(t *testing.T) {
		m, users, shares := newIdentityManager(t, allowAll(t))
		users.EXPECT().SetUserSecret(mock.Anything, "erin", secret).Return(nil)
		shares.EXPECT().RevokeAll(mock.Anything, domain.Identity{IsRoot: true}, "erin").Return(0, errDown)
		_, err := m.UpdateUser(ctx, admin.UpdateUserInput{Requester: req("admin1"), AccessKey: "erin", SecretKey: &secret})
		assert.ErrorIs(t, err, errDown)
	})
	t.Run("a failed rotation revokes nothing", func(t *testing.T) {
		m, users, _ := newIdentityManager(t, allowAll(t))
		users.EXPECT().SetUserSecret(mock.Anything, "erin", "short").Return(fmt.Errorf("x: %w", domain.ErrInvalidArgument))
		short := "short"
		_, err := m.UpdateUser(ctx, admin.UpdateUserInput{Requester: req("admin1"), AccessKey: "erin", SecretKey: &short})
		assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	})
}

func TestDeleteUserRemovesShares(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		// deny denies admin:DeleteUser.
		deny bool
		// deleteErr is the IAM deletion outcome; revokeErr the share
		// removal outcome (only asked once the user is gone).
		deleteErr error
		revoke    bool
		revokeErr error
		want      error
	}{
		{name: "the share links go with the user", revoke: true},
		{name: "denied changes nothing", deny: true, want: domain.ErrAccessDenied},
		{name: "a missing user revokes nothing", deleteErr: fmt.Errorf("x: %w", domain.ErrUserNotFound), want: domain.ErrUserNotFound},
		{name: "root is immutable and keeps its links", deleteErr: fmt.Errorf("x: %w", domain.ErrInvalidArgument), want: domain.ErrInvalidArgument},
		{name: "a failed removal is reported", revoke: true, revokeErr: errDown, want: errDown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var asked []policy.Action
			var denied []policy.Action
			if tt.deny {
				denied = append(denied, policy.ActionAdminDeleteUser)
			}
			m, users, shares := newIdentityManager(t, recordingAuthz(t, &asked, denied...))
			if !tt.deny {
				users.EXPECT().DeleteUser(mock.Anything, "carol").Return(tt.deleteErr).Once()
			}
			if tt.revoke {
				shares.EXPECT().RevokeAll(mock.Anything, domain.Identity{IsRoot: true}, "carol").Return(2, tt.revokeErr).Once()
			}
			err := m.DeleteUser(ctx, req("admin1"), "carol")
			if tt.want != nil {
				assert.ErrorIs(t, err, tt.want)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, []policy.Action{policy.ActionAdminDeleteUser}, asked)
		})
	}
}
