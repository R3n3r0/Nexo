package admin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/iam"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/service/share"
)

// IdentityManagerDeps are the dependencies of the identity manager.
type IdentityManagerDeps struct {
	// IAM stores users and groups (required).
	IAM iam.Service
	// Authz authorizes every change (required).
	Authz Authorizer
	// Shares revokes the share links of a user whose secret is rotated or
	// who is deleted (required).
	Shares share.Service
	Logger *slog.Logger
}

// IdentityManagerOption configures the identity manager.
type IdentityManagerOption func(*identityManager)

type identityManager struct {
	deps IdentityManagerDeps
}

var _ IdentityManager = (*identityManager)(nil)

// NewIdentityManager returns the identity manager. Missing dependencies
// are a wiring error (domain.ErrInvalidArgument).
func NewIdentityManager(deps IdentityManagerDeps, opts ...IdentityManagerOption) (IdentityManager, error) {
	if deps.IAM == nil || deps.Authz == nil || deps.Shares == nil {
		return nil, fmt.Errorf("%w: identity manager requires the IAM, authorizer and share services", domain.ErrInvalidArgument)
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	m := &identityManager{deps: deps}
	for _, o := range opts {
		o(m)
	}
	return m, nil
}

// ListUsers implements IdentityManager.
func (m *identityManager) ListUsers(ctx context.Context, page Page) (*Paged[iam.User], error) {
	if _, err := pageLimit(page); err != nil {
		return nil, err
	}
	users, err := m.deps.IAM.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	return pageOf(users, func(u iam.User) string { return u.AccessKey }, page)
}

// ListGroups implements IdentityManager.
func (m *identityManager) ListGroups(ctx context.Context, page Page) (*Paged[iam.Group], error) {
	if _, err := pageLimit(page); err != nil {
		return nil, err
	}
	groups, err := m.deps.IAM.ListGroups(ctx)
	if err != nil {
		return nil, err
	}
	return pageOf(groups, func(g iam.Group) string { return g.Name }, page)
}

// rotateSecret changes the secret of a user and revokes what it handed
// out with the old one (docs/09 §9.1, mass revocation): its managed share
// links. Its console sessions, bound to the secret, close by themselves
// (session.Service.Validate); its raw presigned URLs no longer verify.
func (m *identityManager) rotateSecret(ctx context.Context, user, secret string) error {
	if err := m.deps.IAM.SetUserSecret(ctx, user, secret); err != nil {
		return err
	}
	n, err := m.deps.Shares.RevokeAll(ctx, systemActor(), user)
	if err != nil {
		return fmt.Errorf("admin: the secret of %s changed but its share links were not revoked: %w", user, err)
	}
	m.deps.Logger.InfoContext(ctx, "admin: user secret rotated", "user", user, "revoked_shares", n)
	return nil
}

// statusAction is the action that sets a user or group status.
func statusAction(status domain.CredentialStatus, enable, disable policy.Action) (policy.Action, error) {
	switch status {
	case domain.StatusEnabled:
		return enable, nil
	case domain.StatusDisabled:
		return disable, nil
	default:
		return "", fmt.Errorf("%w: status must be %q or %q", domain.ErrInvalidArgument, domain.StatusEnabled, domain.StatusDisabled)
	}
}

// CreateUser implements IdentityManager.
func (m *identityManager) CreateUser(ctx context.Context, in CreateUserInput) (*iam.User, error) {
	actions := []policy.Action{policy.ActionAdminCreateUser}
	if len(in.Groups) > 0 {
		actions = append(actions, policy.ActionAdminAddUserToGroup)
	}
	if len(in.PolicyNames) > 0 {
		actions = append(actions, policy.ActionAdminAttachUserOrGroupPolicy)
	}
	if err := authorizeAll(ctx, m.deps.Authz, in.Requester, "", "", actions...); err != nil {
		return nil, err
	}
	groups := dedupe(in.Groups)
	for _, g := range groups {
		// The groups are checked first so that a typo does not leave a
		// half-configured user behind.
		if _, err := m.deps.IAM.GetGroup(ctx, g); err != nil {
			return nil, err
		}
	}
	u, err := m.deps.IAM.CreateUser(ctx, iam.CreateUserInput{AccessKey: in.AccessKey, SecretKey: in.SecretKey, Status: in.Status, PolicyNames: in.PolicyNames})
	if err != nil {
		return nil, err
	}
	for _, g := range groups {
		if err := m.deps.IAM.AddGroupMembers(ctx, g, []string{u.AccessKey}); err != nil {
			if derr := m.deps.IAM.DeleteUser(context.WithoutCancel(ctx), u.AccessKey); derr != nil {
				m.deps.Logger.WarnContext(ctx, "admin: cannot roll back user creation", "user", u.AccessKey, "err", derr)
			}
			return nil, fmt.Errorf("admin: add %s to group %s: %w", u.AccessKey, g, err)
		}
	}
	m.deps.Logger.InfoContext(ctx, "admin: user created", "user", u.AccessKey, "by", in.Requester.Identity.AccessKey)
	return m.deps.IAM.GetUser(ctx, u.AccessKey)
}

// UpdateUser implements IdentityManager.
func (m *identityManager) UpdateUser(ctx context.Context, in UpdateUserInput) (*iam.User, error) {
	var actions []policy.Action
	if in.SecretKey != nil {
		actions = append(actions, policy.ActionAdminCreateUser)
	}
	if in.Status != nil {
		a, err := statusAction(*in.Status, policy.ActionAdminEnableUser, policy.ActionAdminDisableUser)
		if err != nil {
			return nil, err
		}
		actions = append(actions, a)
	}
	if in.PolicyNames != nil {
		actions = append(actions, policy.ActionAdminAttachUserOrGroupPolicy)
	}
	if in.Groups != nil || len(actions) == 0 {
		// Computing the membership changes reads the user.
		actions = append(actions, policy.ActionAdminGetUser)
	}
	if err := authorizeAll(ctx, m.deps.Authz, in.Requester, "", "", actions...); err != nil {
		return nil, err
	}
	var add, remove []string
	if in.Groups != nil {
		current, err := m.deps.IAM.GetUser(ctx, in.AccessKey)
		if err != nil {
			return nil, err
		}
		add, remove = diff(current.Groups, *in.Groups)
		var groupActions []policy.Action
		if len(add) > 0 {
			groupActions = append(groupActions, policy.ActionAdminAddUserToGroup)
		}
		if len(remove) > 0 {
			groupActions = append(groupActions, policy.ActionAdminRemoveUserFromGroup)
		}
		if len(groupActions) > 0 {
			if err := authorizeAll(ctx, m.deps.Authz, in.Requester, "", "", groupActions...); err != nil {
				return nil, err
			}
		}
	}
	if in.SecretKey != nil {
		if err := m.rotateSecret(ctx, in.AccessKey, *in.SecretKey); err != nil {
			return nil, err
		}
	}
	if in.Status != nil {
		if err := m.deps.IAM.SetUserStatus(ctx, in.AccessKey, *in.Status); err != nil {
			return nil, err
		}
	}
	if in.PolicyNames != nil {
		if err := m.deps.IAM.SetUserPolicy(ctx, in.AccessKey, *in.PolicyNames); err != nil {
			return nil, err
		}
	}
	for _, g := range add {
		if err := m.deps.IAM.AddGroupMembers(ctx, g, []string{in.AccessKey}); err != nil {
			return nil, fmt.Errorf("admin: add %s to group %s: %w", in.AccessKey, g, err)
		}
	}
	for _, g := range remove {
		if err := m.deps.IAM.RemoveGroupMembers(ctx, g, []string{in.AccessKey}); err != nil && !errors.Is(err, domain.ErrGroupNotFound) {
			return nil, fmt.Errorf("admin: remove %s from group %s: %w", in.AccessKey, g, err)
		}
	}
	return m.deps.IAM.GetUser(ctx, in.AccessKey)
}

// DeleteUser implements IdentityManager. IAM deletes the user first, so
// that the root key and a missing user fail without side effects; the
// share links are then removed by owner (share.Service tolerates the
// service accounts IAM already deleted). A failure there is reported with
// the user already gone: retrying DELETE /shares?owner= cleans up.
func (m *identityManager) DeleteUser(ctx context.Context, req Requester, user string) error {
	if err := authorizeAll(ctx, m.deps.Authz, req, "", "", policy.ActionAdminDeleteUser); err != nil {
		return err
	}
	if err := m.deps.IAM.DeleteUser(ctx, user); err != nil {
		return err
	}
	n, err := m.deps.Shares.RevokeAll(context.WithoutCancel(ctx), systemActor(), user)
	if err != nil {
		return fmt.Errorf("admin: user %s deleted but its share links were not all removed: %w", user, err)
	}
	m.deps.Logger.InfoContext(ctx, "admin: user deleted", "user", user, "by", req.Identity.AccessKey, "revoked_shares", n)
	return nil
}

// CreateGroup implements IdentityManager.
func (m *identityManager) CreateGroup(ctx context.Context, in CreateGroupInput) (*iam.Group, error) {
	actions := []policy.Action{policy.ActionAdminAddUserToGroup}
	if len(in.PolicyNames) > 0 {
		actions = append(actions, policy.ActionAdminAttachUserOrGroupPolicy)
	}
	if err := authorizeAll(ctx, m.deps.Authz, in.Requester, "", "", actions...); err != nil {
		return nil, err
	}
	g, err := m.deps.IAM.CreateGroup(ctx, iam.CreateGroupInput{Name: in.Name, Members: dedupe(in.Members), PolicyNames: in.PolicyNames})
	if err != nil {
		return nil, err
	}
	m.deps.Logger.InfoContext(ctx, "admin: group created", "group", g.Name, "by", in.Requester.Identity.AccessKey)
	return g, nil
}

// UpdateGroup implements IdentityManager.
func (m *identityManager) UpdateGroup(ctx context.Context, in UpdateGroupInput) (*iam.Group, error) {
	var actions []policy.Action
	if in.Status != nil {
		a, err := statusAction(*in.Status, policy.ActionAdminEnableGroup, policy.ActionAdminDisableGroup)
		if err != nil {
			return nil, err
		}
		actions = append(actions, a)
	}
	if in.PolicyNames != nil {
		actions = append(actions, policy.ActionAdminAttachUserOrGroupPolicy)
	}
	if len(in.AddMembers) > 0 {
		actions = append(actions, policy.ActionAdminAddUserToGroup)
	}
	if len(in.RemoveMembers) > 0 {
		actions = append(actions, policy.ActionAdminRemoveUserFromGroup)
	}
	if len(actions) == 0 {
		actions = append(actions, policy.ActionAdminGetGroup)
	}
	if err := authorizeAll(ctx, m.deps.Authz, in.Requester, "", "", actions...); err != nil {
		return nil, err
	}
	if in.Status != nil {
		if err := m.deps.IAM.SetGroupStatus(ctx, in.Name, *in.Status); err != nil {
			return nil, err
		}
	}
	if in.PolicyNames != nil {
		if err := m.deps.IAM.SetGroupPolicy(ctx, in.Name, *in.PolicyNames); err != nil {
			return nil, err
		}
	}
	if len(in.AddMembers) > 0 {
		if err := m.deps.IAM.AddGroupMembers(ctx, in.Name, dedupe(in.AddMembers)); err != nil {
			return nil, err
		}
	}
	if len(in.RemoveMembers) > 0 {
		if err := m.deps.IAM.RemoveGroupMembers(ctx, in.Name, dedupe(in.RemoveMembers)); err != nil {
			return nil, err
		}
	}
	return m.deps.IAM.GetGroup(ctx, in.Name)
}

// dedupe returns the distinct non-empty names, sorted.
func dedupe(names []string) []string {
	seen := make(map[string]bool, len(names))
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// diff returns the names of want missing from have (add) and those of have
// missing from want (remove).
func diff(have, want []string) (add, remove []string) {
	in := func(list []string, n string) bool {
		for _, x := range list {
			if x == n {
				return true
			}
		}
		return false
	}
	for _, n := range dedupe(want) {
		if !in(have, n) {
			add = append(add, n)
		}
	}
	for _, n := range dedupe(have) {
		if !in(want, n) {
			remove = append(remove, n)
		}
	}
	return add, remove
}
