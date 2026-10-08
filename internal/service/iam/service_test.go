package iam_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/iam"
	iammock "github.com/r3n3r0/nexo/internal/service/iam/mock"
	"github.com/r3n3r0/nexo/internal/service/policy"
)

// TestConcurrentAccess exercises the cache under the race detector:
// writers on users, groups and keys while readers resolve credentials.
func TestConcurrentAccess(t *testing.T) {
	ctx := context.Background()
	for name, factory := range storeFactories() {
		t.Run(name, func(t *testing.T) {
			svc, _ := newIAM(t, factory(t))
			mustUser(t, svc, "shared")
			_, err := svc.CreateGroup(ctx, iam.CreateGroupInput{Name: "all"})
			require.NoError(t, err)

			var wg sync.WaitGroup
			for i := range 8 {
				wg.Add(2)
				go func() {
					defer wg.Done()
					ak := fmt.Sprintf("user%d", i)
					mustUser(t, svc, ak)
					assert.NoError(t, svc.AddGroupMembers(ctx, "all", []string{ak}))
					key, err := svc.CreateAccessKey(ctx, iam.CreateAccessKeyInput{Caller: domain.Identity{AccessKey: ak}})
					if assert.NoError(t, err) {
						_, _, err = svc.LookupCredentials(ctx, key.AccessKey)
						assert.NoError(t, err)
					}
					_, err = svc.AssumeRole(ctx, iam.AssumeRoleInput{Caller: domain.Identity{AccessKey: ak}})
					assert.NoError(t, err)
					assert.NoError(t, svc.Attach(ctx, policy.AttachInput{User: ak, PolicyNames: []string{"readonly"}}))
				}()
				go func() {
					defer wg.Done()
					for range 20 {
						_, _, err := svc.LookupCredentials(ctx, "shared")
						assert.NoError(t, err)
						_, err = svc.ListUsers(ctx)
						assert.NoError(t, err)
						_, err = svc.ListAttached(ctx, policy.EntityUser)
						assert.NoError(t, err)
						if i%4 == 0 {
							svc.Invalidate(ctx)
						}
					}
				}()
			}
			wg.Wait()

			g, err := svc.GetGroup(ctx, "all")
			require.NoError(t, err)
			assert.Len(t, g.Members, 8)
			keys, err := svc.ListAccessKeys(ctx, rootID(), "")
			require.NoError(t, err)
			assert.Len(t, keys, 8)
			attached, err := svc.ListAttached(ctx, policy.EntityUser)
			require.NoError(t, err)
			assert.Len(t, attached, 8)
		})
	}
}

// TestInvalidateReloads checks that Invalidate picks up changes written
// to the shared store by another node.
func TestInvalidateReloads(t *testing.T) {
	ctx := context.Background()
	store := iam.NewMemoryStore()
	node1, _ := newIAM(t, store)
	node2, _ := newIAM(t, store)
	mustUser(t, node2, "warmup")

	mustUser(t, node1, "alice")
	_, _, err := node2.LookupCredentials(ctx, "alice")
	assert.ErrorIs(t, err, domain.ErrInvalidAccessKey, "node2 serves its cache")
	node2.Invalidate(ctx)
	_, _, err = node2.LookupCredentials(ctx, "alice")
	require.NoError(t, err)

	require.NoError(t, node1.SetUserStatus(ctx, "alice", domain.StatusDisabled))
	node2.Invalidate(ctx)
	_, _, err = node2.LookupCredentials(ctx, "alice")
	assert.ErrorIs(t, err, domain.ErrAccessDenied)
}

// TestStoreFailures checks that a failing store surfaces its error and
// leaves the cache unchanged.
func TestStoreFailures(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("disk on fire")

	t.Run("load", func(t *testing.T) {
		store := iammock.NewStore(t)
		store.EXPECT().List(mock.Anything, iam.NamespaceUsers).Return(nil, boom)
		svc, _ := newIAM(t, store)
		_, err := svc.ListUsers(ctx)
		assert.ErrorIs(t, err, boom)
		_, _, err = svc.LookupCredentials(ctx, "alice")
		assert.ErrorIs(t, err, boom)
	})

	t.Run("corrupted record", func(t *testing.T) {
		store := iammock.NewStore(t)
		store.EXPECT().List(mock.Anything, iam.NamespaceUsers).Return(map[string][]byte{"alice": []byte("{")}, nil).Once()
		svc, _ := newIAM(t, store)
		_, err := svc.ListUsers(ctx)
		assert.ErrorIs(t, err, domain.ErrInvalidFormat)
	})

	t.Run("record under another key", func(t *testing.T) {
		store := iammock.NewStore(t)
		store.EXPECT().List(mock.Anything, iam.NamespaceUsers).Return(map[string][]byte{"alice": []byte(`{"accessKey":"bob"}`)}, nil).Once()
		svc, _ := newIAM(t, store)
		_, err := svc.ListUsers(ctx)
		assert.ErrorIs(t, err, domain.ErrInvalidFormat)
	})

	t.Run("writes", func(t *testing.T) {
		mem := iam.NewMemoryStore()
		store := iammock.NewStore(t)
		failPut := false
		store.EXPECT().List(mock.Anything, mock.Anything).RunAndReturn(mem.List)
		store.EXPECT().Put(mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
			func(ctx context.Context, ns iam.Namespace, key string, v []byte) error {
				if failPut {
					return boom
				}
				return mem.Put(ctx, ns, key, v)
			})
		store.EXPECT().Delete(mock.Anything, mock.Anything, mock.Anything).Return(boom).Maybe()
		svc, _ := newIAM(t, store)
		mustUser(t, svc, "alice")
		_, err := svc.CreateGroup(ctx, iam.CreateGroupInput{Name: "dev", Members: []string{"alice"}})
		require.NoError(t, err)
		_, err = svc.CreateAccessKey(ctx, iam.CreateAccessKeyInput{Caller: rootID(), ParentUser: "alice", AccessKey: "alice-key"})
		require.NoError(t, err)
		_, err = svc.AssumeRole(ctx, iam.AssumeRoleInput{Caller: domain.Identity{AccessKey: "alice"}})
		require.NoError(t, err)

		failPut = true
		disabled := domain.StatusDisabled
		calls := map[string]func() error{
			"create user": func() error {
				_, err := svc.CreateUser(ctx, iam.CreateUserInput{AccessKey: "bob", SecretKey: "bob-secret"})
				return err
			},
			"set status":   func() error { return svc.SetUserStatus(ctx, "alice", domain.StatusDisabled) },
			"create group": func() error { _, err := svc.CreateGroup(ctx, iam.CreateGroupInput{Name: "ops"}); return err },
			"add member":   func() error { return svc.AddGroupMembers(ctx, "dev", []string{"alice"}) },
			"create key": func() error {
				_, err := svc.CreateAccessKey(ctx, iam.CreateAccessKeyInput{Caller: rootID(), ParentUser: "alice"})
				return err
			},
			"update key": func() error {
				_, err := svc.UpdateAccessKey(ctx, rootID(), "alice-key", iam.UpdateAccessKeyInput{Status: &disabled})
				return err
			},
			"assume role":  func() error { _, err := svc.AssumeRole(ctx, iam.AssumeRoleInput{Caller: rootID()}); return err },
			"delete user":  func() error { return svc.DeleteUser(ctx, "alice") },
			"delete group": func() error { return svc.DeleteGroup(ctx, "dev") },
			"delete key":   func() error { return svc.DeleteAccessKey(ctx, rootID(), "alice-key") },
		}
		for name, call := range calls {
			assert.ErrorIs(t, call(), boom, name)
		}

		// Nothing changed in the cache.
		_, _, err = svc.LookupCredentials(ctx, "alice")
		require.NoError(t, err)
		_, _, err = svc.LookupCredentials(ctx, "alice-key")
		require.NoError(t, err)
		_, err = svc.GetUser(ctx, "bob")
		assert.ErrorIs(t, err, domain.ErrUserNotFound)
		g, err := svc.GetGroup(ctx, "dev")
		require.NoError(t, err)
		assert.Equal(t, []string{"alice"}, g.Members)
	})
}
