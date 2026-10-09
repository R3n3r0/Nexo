package policy_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/lock"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/service/storage"
	"github.com/r3n3r0/nexo/internal/service/sysstore"
	"github.com/r3n3r0/nexo/internal/testutil"
)

// newDocs returns a system document store over n temporary drives.
func newDocs(t *testing.T, n int) sysstore.Store {
	t.Helper()
	drives := make([]storage.StorageAPI, n)
	for i := range drives {
		drives[i] = testutil.NewLocalDriveIndex(t, "", i)
	}
	docs, err := sysstore.New(sysstore.Deps{Drives: drives, Locks: lock.NewLocal(lock.Deps{})})
	require.NoError(t, err)
	return docs
}

func TestDocsStore(t *testing.T) {
	ctx := context.Background()
	docs := newDocs(t, 4)
	st, err := policy.NewDocsStore(docs)
	require.NoError(t, err)
	d := mustPolicy(t, doc(`{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}`))

	names := []string{"readers", "Mixed.Case", ".hidden", "a+b=c,d@e_f-g"}
	for _, n := range names {
		require.NoError(t, st.Set(ctx, policy.Named{Name: n, Policy: d, UpdatedAt: testNow}))
	}
	assert.ErrorIs(t, st.Set(ctx, policy.Named{Name: "../escape", Policy: d}), domain.ErrInvalidArgument)
	alice := policy.Entity{Kind: policy.EntityUser, Name: "alice"}
	team := policy.Entity{Kind: policy.EntityGroup, Name: "team"}
	require.NoError(t, st.SetAttached(ctx, alice, []string{"readers", "Mixed.Case"}))
	require.NoError(t, st.SetAttached(ctx, team, []string{"readers"}))
	assert.ErrorIs(t, st.SetAttached(ctx, policy.Entity{Kind: "role", Name: "x"}, nil), domain.ErrInvalidArgument)
	require.NoError(t, st.Delete(ctx, ".hidden"))
	assert.ErrorIs(t, st.Delete(ctx, ".hidden"), domain.ErrPolicyNotFound)

	// Another node over the same documents sees everything.
	other, err := policy.NewDocsStore(docs)
	require.NoError(t, err)
	list, err := other.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 3)
	assert.Equal(t, []string{"Mixed.Case", "a+b=c,d@e_f-g", "readers"}, []string{list[0].Name, list[1].Name, list[2].Name})
	got, err := other.Get(ctx, "Mixed.Case")
	require.NoError(t, err)
	assert.Equal(t, d, got.Policy)
	attached, err := other.Attached(ctx, alice)
	require.NoError(t, err)
	assert.Equal(t, []string{"Mixed.Case", "readers"}, attached)
	groups, err := other.ListAttached(ctx, policy.EntityGroup)
	require.NoError(t, err)
	assert.Equal(t, map[string][]string{"team": {"readers"}}, groups)

	// A change of the first node is cached by the other until it is
	// invalidated (the peer notification).
	require.NoError(t, st.Delete(ctx, "readers"))
	_, err = other.Get(ctx, "readers")
	require.NoError(t, err, "served from the cache")
	other.Invalidate(ctx)
	_, err = other.Get(ctx, "readers")
	assert.ErrorIs(t, err, domain.ErrPolicyNotFound)

	// The engine works on top of the store.
	svc := policy.New(policy.Deps{Store: other, Clock: clock.NewFake(testNow)})
	dec, err := svc.Evaluate(ctx, policy.Args{Identity: user("alice"), Action: policy.ActionGetObject, Bucket: "b", Object: "k"})
	require.NoError(t, err)
	assert.True(t, dec.Allowed, "Mixed.Case still grants the read")

	_, err = policy.NewDocsStore(nil)
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}
