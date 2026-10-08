package policy_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/policy"
	policymock "github.com/r3n3r0/nexo/internal/service/policy/mock"
)

func TestBuiltinPolicies(t *testing.T) {
	ctx := context.Background()
	svc, store := newEngine(t)
	builtins := svc.ListBuiltin(ctx)
	names := make([]string, 0, len(builtins))
	for _, b := range builtins {
		names = append(names, b.Name)
		assert.True(t, b.BuiltIn)
		require.NoError(t, svc.Validate(ctx, b.Policy), b.Name)
		assert.Equal(t, testNow, b.UpdatedAt)
	}
	assert.Equal(t, []string{policy.PolicyConsoleAdmin, policy.PolicyDiagnostics, policy.PolicyReadOnly, policy.PolicyReadWrite, policy.PolicyWriteOnly}, names)

	// Callers get copies.
	builtins[0].Policy.Statement[0].Effect = domain.Deny
	again := svc.ListBuiltin(ctx)
	assert.Equal(t, domain.Allow, again[0].Policy.Statement[0].Effect)

	// Built-ins are read-only and never stored.
	doc := mustPolicy(t, doc(`{"Effect":"Allow","Action":"s3:GetObject","Resource":"*"}`))
	for _, n := range names {
		assert.ErrorIs(t, svc.SetPolicy(ctx, n, doc), domain.ErrInvalidArgument)
		assert.ErrorIs(t, svc.DeletePolicy(ctx, n), domain.ErrInvalidArgument)
		got, err := svc.GetPolicy(ctx, n)
		require.NoError(t, err)
		assert.NotEmpty(t, got.Statement)
	}
	stored, err := store.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, stored)
}

func TestNamedPolicies(t *testing.T) {
	ctx := context.Background()
	svc, store := newEngine(t)
	d := mustPolicy(t, doc(`{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}`))

	for _, bad := range []string{"", "bad name!", ".", "..", "a/b", string(make([]byte, 129))} {
		assert.ErrorIs(t, svc.SetPolicy(ctx, bad, d), domain.ErrInvalidArgument, bad)
	}
	assert.ErrorIs(t, svc.SetPolicy(ctx, "x", &domain.Policy{Version: "2008"}), domain.ErrMalformedPolicy)
	assert.ErrorIs(t, svc.SetPolicy(ctx, "x", nil), domain.ErrMalformedPolicy)
	withPrincipal := mustPolicy(t, doc(`{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"*"}`))
	assert.ErrorIs(t, svc.SetPolicy(ctx, "x", withPrincipal), domain.ErrMalformedPolicy, "identity policies have no principal")

	require.NoError(t, svc.SetPolicy(ctx, "custom.v1@team_a-b+c=d,e", d))
	require.NoError(t, svc.SetPolicy(ctx, "Custom", d))
	got, err := svc.GetPolicy(ctx, "Custom")
	require.NoError(t, err)
	assert.Equal(t, d, got)
	got.Statement[0].Effect = domain.Deny
	again, err := svc.GetPolicy(ctx, "Custom")
	require.NoError(t, err)
	assert.Equal(t, domain.Allow, again.Statement[0].Effect, "callers get copies")
	d.Statement[0].Effect = domain.Deny
	again, _ = svc.GetPolicy(ctx, "Custom")
	assert.Equal(t, domain.Allow, again.Statement[0].Effect, "the engine keeps its own copy")
	_, err = svc.GetPolicy(ctx, "missing")
	assert.ErrorIs(t, err, domain.ErrPolicyNotFound)

	list, err := svc.ListPolicies(ctx)
	require.NoError(t, err)
	require.Len(t, list, 7)
	assert.True(t, list[0].BuiltIn)
	assert.Equal(t, "Custom", list[5].Name)
	assert.Equal(t, "custom.v1@team_a-b+c=d,e", list[6].Name)
	assert.False(t, list[6].BuiltIn)
	assert.Equal(t, testNow, list[6].UpdatedAt)

	// Replacing keeps the name, deleting removes it.
	d2 := mustPolicy(t, doc(`{"Effect":"Allow","Action":"s3:PutObject","Resource":"*"}`))
	require.NoError(t, svc.SetPolicy(ctx, "Custom", d2))
	got, _ = svc.GetPolicy(ctx, "Custom")
	assert.Equal(t, domain.StringSet{"s3:PutObject"}, got.Statement[0].Action)
	assert.ErrorIs(t, svc.DeletePolicy(ctx, "ghost"), domain.ErrPolicyNotFound)
	require.NoError(t, svc.DeletePolicy(ctx, "Custom"))
	_, err = store.Get(ctx, "Custom")
	assert.ErrorIs(t, err, domain.ErrPolicyNotFound)
}

func TestAttachments(t *testing.T) {
	ctx := context.Background()
	svc, store := newEngine(t)
	d := mustPolicy(t, doc(`{"Effect":"Allow","Action":"s3:GetObject","Resource":"*"}`))
	require.NoError(t, svc.SetPolicy(ctx, "custom", d))

	bad := []policy.AttachInput{
		{PolicyNames: []string{"custom"}},
		{PolicyNames: []string{"custom"}, User: "u", Group: "g"},
		{User: "u"},
	}
	for _, in := range bad {
		assert.ErrorIs(t, svc.Attach(ctx, in), domain.ErrInvalidArgument)
		assert.ErrorIs(t, svc.Detach(ctx, in), domain.ErrInvalidArgument)
	}
	assert.ErrorIs(t, svc.Attach(ctx, policy.AttachInput{PolicyNames: []string{"ghost"}, User: "u"}), domain.ErrPolicyNotFound)

	require.NoError(t, svc.Attach(ctx, policy.AttachInput{PolicyNames: []string{"custom", policy.PolicyReadOnly, "custom"}, User: "alice"}))
	require.NoError(t, svc.Attach(ctx, policy.AttachInput{PolicyNames: []string{"custom"}, User: "alice"}), "idempotent")
	require.NoError(t, svc.Attach(ctx, policy.AttachInput{PolicyNames: []string{"custom"}, Group: "team"}))
	names, err := store.Attached(ctx, policy.Entity{Kind: policy.EntityUser, Name: "alice"})
	require.NoError(t, err)
	assert.Equal(t, []string{"custom", "readonly"}, names)

	ent, err := svc.Entities(ctx, "custom")
	require.NoError(t, err)
	assert.Equal(t, policy.PolicyEntities{Policy: "custom", Users: []string{"alice"}, Groups: []string{"team"}}, ent)
	ent, err = svc.Entities(ctx, policy.PolicyWriteOnly)
	require.NoError(t, err)
	assert.Empty(t, ent.Users)
	assert.Empty(t, ent.Groups)
	_, err = svc.Entities(ctx, "ghost")
	assert.ErrorIs(t, err, domain.ErrPolicyNotFound)

	// A new engine over the same store sees the attachments.
	svc2 := policy.New(policy.Deps{Store: store, Clock: clock.NewFake(testNow)})
	dec, err := svc2.Evaluate(ctx, policy.Args{Identity: &domain.Identity{AccessKey: "bob", Groups: []string{"team"}}, Action: policy.ActionGetObject, Bucket: "b", Object: "k"})
	require.NoError(t, err)
	assert.True(t, dec.Allowed)
	assert.Equal(t, "team", dec.Group)

	// Detach is idempotent; the last detach removes the mapping.
	require.NoError(t, svc.Detach(ctx, policy.AttachInput{PolicyNames: []string{policy.PolicyReadOnly, "never-attached"}, User: "alice"}))
	names, _ = store.Attached(ctx, policy.Entity{Kind: policy.EntityUser, Name: "alice"})
	assert.Equal(t, []string{"custom"}, names)
	require.NoError(t, svc.Detach(ctx, policy.AttachInput{PolicyNames: []string{policy.PolicyReadOnly}, Group: "nobody"}))

	// Deleting a policy detaches it everywhere.
	require.NoError(t, svc.DeletePolicy(ctx, "custom"))
	users, _ := store.ListAttached(ctx, policy.EntityUser)
	groups, _ := store.ListAttached(ctx, policy.EntityGroup)
	assert.Empty(t, users)
	assert.Empty(t, groups)
	dec, err = svc.Evaluate(ctx, policy.Args{Identity: user("alice"), Action: policy.ActionGetObject, Bucket: "b", Object: "k"})
	require.NoError(t, err)
	assert.False(t, dec.Allowed)
}

// TestExternalAttachments checks the delegation to Deps.Attachments (the
// IAM service in production).
func TestExternalAttachments(t *testing.T) {
	ctx := context.Background()
	att := policymock.NewAttachments(t)
	store := policy.NewMemoryStore()
	svc := policy.New(policy.Deps{Store: store, Attachments: att, Clock: clock.NewFake(testNow)})
	require.NoError(t, svc.SetPolicy(ctx, "custom", mustPolicy(t, doc(`{"Effect":"Allow","Action":"s3:GetObject","Resource":"*"}`))))

	in := policy.AttachInput{PolicyNames: []string{"custom"}, Group: "team"}
	att.EXPECT().Attach(mock.Anything, in).Return(nil).Once()
	require.NoError(t, svc.Attach(ctx, in))
	assert.ErrorIs(t, svc.Attach(ctx, policy.AttachInput{PolicyNames: []string{"ghost"}, User: "u"}), domain.ErrPolicyNotFound, "names are checked before delegating")
	att.EXPECT().Attach(mock.Anything, mock.Anything).Return(domain.ErrGroupNotFound).Once()
	assert.ErrorIs(t, svc.Attach(ctx, policy.AttachInput{PolicyNames: []string{"custom"}, Group: "ghost"}), domain.ErrGroupNotFound)

	// Evaluation reads the attachments of the user and of the groups.
	att.EXPECT().Attached(mock.Anything, policy.Entity{Kind: policy.EntityUser, Name: "bob"}).Return(nil, nil).Once()
	att.EXPECT().Attached(mock.Anything, policy.Entity{Kind: policy.EntityGroup, Name: "team"}).Return([]string{"custom"}, nil).Once()
	d, err := svc.Evaluate(ctx, policy.Args{Identity: &domain.Identity{AccessKey: "bob", Groups: []string{"team"}, PolicyNames: []string{"custom"}}, Action: policy.ActionGetObject, Bucket: "b", Object: "k"})
	require.NoError(t, err)
	assert.True(t, d.Allowed)
	assert.Equal(t, policy.SourceGroup, d.Source, "group attribution wins over the names resolved by IAM")

	att.EXPECT().Attached(mock.Anything, mock.Anything).Return(nil, domain.ErrReadQuorum).Once()
	_, err = svc.Evaluate(ctx, policy.Args{Identity: user("bob"), Action: policy.ActionGetObject, Bucket: "b", Object: "k"})
	assert.ErrorIs(t, err, domain.ErrReadQuorum)

	// Entities and DeletePolicy go through the attachments too.
	att.EXPECT().ListAttached(mock.Anything, policy.EntityUser).Return(map[string][]string{"bob": {"custom"}, "eve": {"other"}}, nil)
	att.EXPECT().ListAttached(mock.Anything, policy.EntityGroup).Return(map[string][]string{"team": {"custom"}}, nil)
	ent, err := svc.Entities(ctx, "custom")
	require.NoError(t, err)
	assert.Equal(t, []string{"bob"}, ent.Users)
	assert.Equal(t, []string{"team"}, ent.Groups)

	att.EXPECT().Detach(mock.Anything, policy.AttachInput{PolicyNames: []string{"custom"}, User: "bob"}).Return(nil).Once()
	att.EXPECT().Detach(mock.Anything, policy.AttachInput{PolicyNames: []string{"custom"}, Group: "team"}).Return(nil).Once()
	require.NoError(t, svc.DeletePolicy(ctx, "custom"))

	att.EXPECT().Detach(mock.Anything, policy.AttachInput{PolicyNames: []string{"x"}, User: "u"}).Return(nil).Once()
	require.NoError(t, svc.Detach(ctx, policy.AttachInput{PolicyNames: []string{"x"}, User: "u"}))
}

func TestStoreFailures(t *testing.T) {
	ctx := context.Background()
	store := policymock.NewPolicyStore(t)
	svc := policy.New(policy.Deps{Store: store})
	d := mustPolicy(t, doc(`{"Effect":"Allow","Action":"s3:GetObject","Resource":"*"}`))

	store.EXPECT().Set(mock.Anything, mock.Anything).Return(domain.ErrWriteQuorum).Once()
	assert.ErrorIs(t, svc.SetPolicy(ctx, "p", d), domain.ErrWriteQuorum)

	store.EXPECT().List(mock.Anything).Return(nil, domain.ErrFaultyDisk).Once()
	_, err := svc.ListPolicies(ctx)
	assert.ErrorIs(t, err, domain.ErrFaultyDisk)

	// A stored policy shadowing a built-in name is ignored by the listing.
	store.EXPECT().List(mock.Anything).Return([]policy.Named{{Name: policy.PolicyReadOnly, Policy: d}, {Name: "z", Policy: d}}, nil).Once()
	list, err := svc.ListPolicies(ctx)
	require.NoError(t, err)
	require.Len(t, list, 6)
	assert.Equal(t, "z", list[5].Name)

	store.EXPECT().Get(mock.Anything, "p").Return(policy.Named{Name: "p", Policy: d}, nil)
	store.EXPECT().ListAttached(mock.Anything, policy.EntityUser).Return(nil, domain.ErrReadQuorum).Once()
	assert.ErrorIs(t, svc.DeletePolicy(ctx, "p"), domain.ErrReadQuorum)
	store.EXPECT().ListAttached(mock.Anything, policy.EntityUser).Return(map[string][]string{"u": {"p"}}, nil).Once()
	store.EXPECT().Attached(mock.Anything, policy.Entity{Kind: policy.EntityUser, Name: "u"}).Return([]string{"p"}, nil).Once()
	store.EXPECT().SetAttached(mock.Anything, policy.Entity{Kind: policy.EntityUser, Name: "u"}, []string{}).Return(domain.ErrWriteQuorum).Once()
	assert.ErrorIs(t, svc.DeletePolicy(ctx, "p"), domain.ErrWriteQuorum)
	store.EXPECT().ListAttached(mock.Anything, mock.Anything).Return(nil, nil)
	store.EXPECT().Delete(mock.Anything, "p").Return(domain.ErrWriteQuorum).Once()
	assert.ErrorIs(t, svc.DeletePolicy(ctx, "p"), domain.ErrWriteQuorum)
	_, err = svc.Entities(ctx, "p")
	require.NoError(t, err)

	store.EXPECT().Attached(mock.Anything, policy.Entity{Kind: policy.EntityUser, Name: "u"}).Return(nil, domain.ErrReadQuorum).Once()
	assert.ErrorIs(t, svc.Attach(ctx, policy.AttachInput{PolicyNames: []string{"p"}, User: "u"}), domain.ErrReadQuorum)
	store.EXPECT().Attached(mock.Anything, policy.Entity{Kind: policy.EntityUser, Name: "u"}).Return(nil, domain.ErrReadQuorum).Once()
	assert.ErrorIs(t, svc.Detach(ctx, policy.AttachInput{PolicyNames: []string{"p"}, User: "u"}), domain.ErrReadQuorum)
	store.EXPECT().Attached(mock.Anything, policy.Entity{Kind: policy.EntityGroup, Name: "g"}).Return(nil, nil).Once()
	store.EXPECT().SetAttached(mock.Anything, policy.Entity{Kind: policy.EntityGroup, Name: "g"}, []string{"p"}).Return(domain.ErrWriteQuorum).Once()
	assert.ErrorIs(t, svc.Attach(ctx, policy.AttachInput{PolicyNames: []string{"p"}, Group: "g"}), domain.ErrWriteQuorum)

	failing := policymock.NewPolicyStore(t)
	svc = policy.New(policy.Deps{Store: failing})
	failing.EXPECT().ListAttached(mock.Anything, mock.Anything).Return(nil, domain.ErrFaultyDisk).Once()
	failing.EXPECT().Get(mock.Anything, "p").Return(policy.Named{Name: "p", Policy: d}, nil).Once()
	_, err = svc.Entities(ctx, "p")
	assert.ErrorIs(t, err, domain.ErrFaultyDisk)
}

func TestMemoryStore(t *testing.T) {
	ctx := context.Background()
	st := policy.NewMemoryStore()
	d := mustPolicy(t, doc(`{"Effect":"Allow","Action":"s3:GetObject","Resource":"*"}`))
	assert.ErrorIs(t, st.Set(ctx, policy.Named{Name: "bad name", Policy: d}), domain.ErrInvalidArgument)
	assert.ErrorIs(t, st.Set(ctx, policy.Named{Name: "p"}), domain.ErrInvalidArgument)
	require.NoError(t, st.Set(ctx, policy.Named{Name: "p", Policy: d, BuiltIn: true}))
	d.Statement[0].Effect = domain.Deny
	got, err := st.Get(ctx, "p")
	require.NoError(t, err)
	assert.Equal(t, domain.Allow, got.Policy.Statement[0].Effect, "the store keeps its own copy")
	assert.False(t, got.BuiltIn)
	assert.ErrorIs(t, st.Delete(ctx, "ghost"), domain.ErrPolicyNotFound)

	alice := policy.Entity{Kind: policy.EntityUser, Name: "alice"}
	for _, bad := range []policy.Entity{{Kind: "role", Name: "x"}, {Kind: policy.EntityUser}} {
		assert.ErrorIs(t, st.SetAttached(ctx, bad, []string{"p"}), domain.ErrInvalidArgument)
		_, err := st.Attached(ctx, bad)
		assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	}
	_, err = st.ListAttached(ctx, "role")
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	require.NoError(t, st.SetAttached(ctx, alice, []string{"b", "a", "", "b"}))
	names, err := st.Attached(ctx, alice)
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, names)
	names[0] = "changed"
	all, err := st.ListAttached(ctx, policy.EntityUser)
	require.NoError(t, err)
	assert.Equal(t, map[string][]string{"alice": {"a", "b"}}, all)
	all["alice"][0] = "changed"
	again, _ := st.Attached(ctx, alice)
	assert.Equal(t, []string{"a", "b"}, again, "callers get copies")
	require.NoError(t, st.SetAttached(ctx, alice, nil))
	all, _ = st.ListAttached(ctx, policy.EntityUser)
	assert.Empty(t, all)
}
