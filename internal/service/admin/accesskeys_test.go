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
	sharemock "github.com/r3n3r0/nexo/internal/service/share/mock"
)

// rootKey is the access key of root in the access key manager tests.
const rootKey = "rootadmin"

// newDelegatedKeyManager returns an access key manager whose authorizer
// allows "mallory" by policy (a delegated administrator, elevated to the
// system actor) and root, with an IAM mock that knows root, the user
// "bob" and the key "SAROOT" of root and "SABOB" of bob.
func newDelegatedKeyManager(t *testing.T) (admin.AccessKeyManager, *iammock.Service) {
	t.Helper()
	authz := adminAuthorizer(t, func(admin.AuthorizeInput) admin.Decision { return admin.Decision{Allowed: true} })
	users := iammock.NewService(t)
	users.EXPECT().LookupCredentials(mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, ak string) (domain.Credentials, domain.Identity, error) {
			switch ak {
			case rootKey:
				return domain.Credentials{AccessKey: ak}, domain.Identity{AccessKey: ak, IsRoot: true}, nil
			case "SAROOT":
				return domain.Credentials{AccessKey: ak}, domain.Identity{AccessKey: ak, Parent: rootKey, IsRoot: true}, nil
			case "bob", "mallory":
				return domain.Credentials{AccessKey: ak}, domain.Identity{AccessKey: ak}, nil
			case "disabled":
				return domain.Credentials{}, domain.Identity{}, fmt.Errorf("x: %w", domain.ErrAccessDenied)
			}
			return domain.Credentials{}, domain.Identity{}, fmt.Errorf("x: %w", domain.ErrInvalidAccessKey)
		}).Maybe()
	users.EXPECT().GetAccessKey(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, _ domain.Identity, ak string) (*iam.AccessKey, error) {
			switch ak {
			case "SAROOT":
				return &iam.AccessKey{AccessKey: ak, ParentUser: rootKey}, nil
			case "SABOB":
				return &iam.AccessKey{AccessKey: ak, ParentUser: "bob"}, nil
			}
			return nil, fmt.Errorf("x: %w", domain.ErrAccessKeyNotFound)
		}).Maybe()
	m, err := admin.NewAccessKeyManager(admin.AccessKeyManagerDeps{IAM: users, Authz: authz, Shares: sharemock.NewService(t)})
	require.NoError(t, err)
	return m, users
}

// TestAccessKeyManagerRootKeys: a delegated administrator (a user allowed
// admin:*ServiceAccount by policy) manages the keys of other users but
// never those of root, which would resolve to root identities bypassing
// every policy (regression: escalation to root).
func TestAccessKeyManagerRootKeys(t *testing.T) {
	ctx := context.Background()
	mallory := req("mallory")
	system := domain.Identity{IsRoot: true}

	tests := []struct {
		name   string
		expect func(users *iammock.Service)
		run    func(m admin.AccessKeyManager) error
		denied bool
	}{
		{name: "create for root", denied: true, run: func(m admin.AccessKeyManager) error {
			_, err := m.Create(ctx, mallory, iam.CreateAccessKeyInput{ParentUser: rootKey, Name: "mal"})
			return err
		}},
		{name: "create for a key of root", denied: true, run: func(m admin.AccessKeyManager) error {
			_, err := m.Create(ctx, mallory, iam.CreateAccessKeyInput{ParentUser: "SAROOT"})
			return err
		}},
		{name: "update a key of root", denied: true, run: func(m admin.AccessKeyManager) error {
			status := domain.StatusEnabled
			_, err := m.Update(ctx, mallory, "SAROOT", iam.UpdateAccessKeyInput{Status: &status, ClearPolicy: true})
			return err
		}},
		{name: "delete a key of root", denied: true, run: func(m admin.AccessKeyManager) error {
			return m.Delete(ctx, mallory, "SAROOT")
		}},
		{name: "get a key of root", denied: true, run: func(m admin.AccessKeyManager) error {
			_, err := m.Get(ctx, mallory, "SAROOT")
			return err
		}},
		{name: "list the keys of root", denied: true, run: func(m admin.AccessKeyManager) error {
			_, err := m.List(ctx, mallory, rootKey, admin.Page{})
			return err
		}},
		{name: "create for another user", expect: func(users *iammock.Service) {
			users.EXPECT().CreateAccessKey(mock.Anything, iam.CreateAccessKeyInput{Caller: system, ParentUser: "bob"}).
				Return(&iam.AccessKey{AccessKey: "K", ParentUser: "bob"}, nil)
		}, run: func(m admin.AccessKeyManager) error {
			_, err := m.Create(ctx, mallory, iam.CreateAccessKeyInput{ParentUser: "bob"})
			return err
		}},
		{name: "update a key of another user", expect: func(users *iammock.Service) {
			users.EXPECT().UpdateAccessKey(mock.Anything, system, "SABOB", iam.UpdateAccessKeyInput{ClearPolicy: true}).
				Return(&iam.AccessKey{AccessKey: "SABOB"}, nil)
		}, run: func(m admin.AccessKeyManager) error {
			_, err := m.Update(ctx, mallory, "SABOB", iam.UpdateAccessKeyInput{ClearPolicy: true})
			return err
		}},
		{name: "delete a key of another user", expect: func(users *iammock.Service) {
			users.EXPECT().DeleteAccessKey(mock.Anything, system, "SABOB").Return(nil)
		}, run: func(m admin.AccessKeyManager) error {
			return m.Delete(ctx, mallory, "SABOB")
		}},
		{name: "unknown parent is reported by IAM", expect: func(users *iammock.Service) {
			users.EXPECT().CreateAccessKey(mock.Anything, iam.CreateAccessKeyInput{Caller: system, ParentUser: "ghost"}).
				Return(nil, fmt.Errorf("x: %w", domain.ErrUserNotFound))
		}, run: func(m admin.AccessKeyManager) error {
			_, err := m.Create(ctx, mallory, iam.CreateAccessKeyInput{ParentUser: "ghost"})
			if assert.ErrorIs(t, err, domain.ErrUserNotFound) {
				return nil
			}
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, users := newDelegatedKeyManager(t)
			if tt.expect != nil {
				tt.expect(users)
			}
			err := tt.run(m)
			if tt.denied {
				require.ErrorIs(t, err, domain.ErrAccessDenied)
				assert.Contains(t, err.Error(), "only root manages the access keys of root")
				return
			}
			require.NoError(t, err)
		})
	}
}

// TestAccessKeyManagerListHidesRootKeys: a listing of every user shows a
// delegated administrator the keys of the users only; root sees all.
func TestAccessKeyManagerListHidesRootKeys(t *testing.T) {
	ctx := context.Background()
	all := []iam.AccessKey{
		{AccessKey: "SAROOT", ParentUser: rootKey},
		{AccessKey: "SABOB", ParentUser: "bob"},
		{AccessKey: "SAROOT2", ParentUser: rootKey},
		{AccessKey: "SAOFF", ParentUser: "disabled"},
	}
	tests := []struct {
		name string
		req  admin.Requester
		want []string
	}{
		{name: "delegated administrator", req: req("mallory"), want: []string{"SABOB", "SAOFF"}},
		{name: "root", req: rootReq(), want: []string{"SABOB", "SAOFF", "SAROOT", "SAROOT2"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, users := newDelegatedKeyManager(t)
			users.EXPECT().ListAccessKeys(mock.Anything, mock.Anything, "").Return(all, nil)
			keys, err := m.List(ctx, tt.req, "", admin.Page{})
			require.NoError(t, err)
			got := make([]string, 0, len(keys.Items))
			for _, k := range keys.Items {
				got = append(got, k.AccessKey)
			}
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestAccessKeyManagerSelfService: a user acting on its own keys (self
// service) is presented as itself and never pays the root check.
func TestAccessKeyManagerSelfService(t *testing.T) {
	ctx := context.Background()
	authz := adminAuthorizer(t, func(admin.AuthorizeInput) admin.Decision {
		return admin.Decision{Allowed: true, SelfService: true}
	})
	users := iammock.NewService(t)
	m, err := admin.NewAccessKeyManager(admin.AccessKeyManagerDeps{IAM: users, Authz: authz, Shares: sharemock.NewService(t)})
	require.NoError(t, err)
	bob := domain.Identity{AccessKey: "bob"}
	users.EXPECT().DeleteAccessKey(mock.Anything, bob, "SAROOT").Return(fmt.Errorf("x: %w", domain.ErrAccessDenied))
	assert.ErrorIs(t, m.Delete(ctx, req("bob"), "SAROOT"), domain.ErrAccessDenied, "IAM enforces the ownership of a self-service caller")
}

// TestAccessKeyManagerDisableRevokesShares: disabling a key deletes the
// share links created with it, after IAM moved the revocation epoch that
// ends its console sessions; enabling it, or any other change, revokes
// nothing (pentest PT-05).
func TestAccessKeyManagerDisableRevokesShares(t *testing.T) {
	ctx := context.Background()
	authz := adminAuthorizer(t, func(admin.AuthorizeInput) admin.Decision {
		return admin.Decision{Allowed: true, SelfService: true}
	})
	bob := domain.Identity{AccessKey: "bob"}
	disabled, enabled := domain.StatusDisabled, domain.StatusEnabled
	newManager := func(t *testing.T) (admin.AccessKeyManager, *iammock.Service, *sharemock.Service) {
		users, shares := iammock.NewService(t), sharemock.NewService(t)
		m, err := admin.NewAccessKeyManager(admin.AccessKeyManagerDeps{IAM: users, Authz: authz, Shares: shares})
		require.NoError(t, err)
		return m, users, shares
	}

	t.Run("disable", func(t *testing.T) {
		m, users, shares := newManager(t)
		in := iam.UpdateAccessKeyInput{Status: &disabled}
		update := users.EXPECT().UpdateAccessKey(mock.Anything, bob, "SABOB", in).
			Return(&iam.AccessKey{AccessKey: "SABOB", ParentUser: "bob", Status: disabled}, nil).Call
		shares.EXPECT().RevokeIssuedBy(mock.Anything, "SABOB").Return(2, nil).NotBefore(update)
		k, err := m.Update(ctx, req("bob"), "SABOB", in)
		require.NoError(t, err)
		assert.Equal(t, disabled, k.Status)
	})
	t.Run("enable", func(t *testing.T) {
		m, users, _ := newManager(t)
		in := iam.UpdateAccessKeyInput{Status: &enabled}
		users.EXPECT().UpdateAccessKey(mock.Anything, bob, "SABOB", in).Return(&iam.AccessKey{AccessKey: "SABOB"}, nil)
		_, err := m.Update(ctx, req("bob"), "SABOB", in)
		require.NoError(t, err)
	})
	t.Run("failed update", func(t *testing.T) {
		m, users, _ := newManager(t)
		in := iam.UpdateAccessKeyInput{Status: &disabled}
		users.EXPECT().UpdateAccessKey(mock.Anything, bob, "SABOB", in).Return(nil, fmt.Errorf("x: %w", domain.ErrAccessKeyNotFound))
		_, err := m.Update(ctx, req("bob"), "SABOB", in)
		assert.ErrorIs(t, err, domain.ErrAccessKeyNotFound)
	})
	t.Run("failed revocation", func(t *testing.T) {
		m, users, shares := newManager(t)
		in := iam.UpdateAccessKeyInput{Status: &disabled}
		users.EXPECT().UpdateAccessKey(mock.Anything, bob, "SABOB", in).Return(&iam.AccessKey{AccessKey: "SABOB"}, nil)
		shares.EXPECT().RevokeIssuedBy(mock.Anything, "SABOB").Return(0, errDown)
		_, err := m.Update(ctx, req("bob"), "SABOB", in)
		assert.ErrorIs(t, err, errDown)
	})
	t.Run("missing share service", func(t *testing.T) {
		_, err := admin.NewAccessKeyManager(admin.AccessKeyManagerDeps{IAM: iammock.NewService(t), Authz: authz})
		assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	})
}
