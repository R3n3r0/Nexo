package admin_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/admin"
	authmock "github.com/r3n3r0/nexo/internal/service/auth/mock"
	"github.com/r3n3r0/nexo/internal/service/policy"
	policymock "github.com/r3n3r0/nexo/internal/service/policy/mock"
)

func newPolicyManager(t *testing.T) (admin.PolicyManager, *policymock.Service, *authmock.CredentialLookup) {
	engine := policymock.NewService(t)
	creds := authmock.NewCredentialLookup(t)
	m, err := admin.NewPolicyManager(admin.PolicyManagerDeps{Policy: engine, Creds: creds})
	require.NoError(t, err)
	return m, engine, creds
}

func TestPolicyManagerSave(t *testing.T) {
	ctx := context.Background()
	doc := &domain.Policy{Version: domain.PolicyVersion}
	raw := json.RawMessage(`{"Version":"2012-10-17","Statement":[]}`)
	t.Run("create refuses an existing name", func(t *testing.T) {
		m, engine, _ := newPolicyManager(t)
		engine.EXPECT().Parse(mock.Anything, []byte(raw)).Return(doc, nil)
		engine.EXPECT().GetPolicy(mock.Anything, "p").Return(doc, nil)
		_, err := m.Save(ctx, admin.SavePolicyInput{Name: "p", Document: raw, Create: true})
		assert.ErrorIs(t, err, domain.ErrConflict)
	})
	t.Run("create stores a new name", func(t *testing.T) {
		m, engine, _ := newPolicyManager(t)
		engine.EXPECT().Parse(mock.Anything, []byte(raw)).Return(doc, nil)
		engine.EXPECT().GetPolicy(mock.Anything, "p").Return(nil, fmt.Errorf("x: %w", domain.ErrPolicyNotFound))
		engine.EXPECT().SetPolicy(mock.Anything, "p", doc).Return(nil)
		engine.EXPECT().ListPolicies(mock.Anything).Return([]policy.Named{{Name: "readonly", BuiltIn: true}, {Name: "p", Policy: doc}}, nil)
		n, err := m.Save(ctx, admin.SavePolicyInput{Name: "p", Document: raw, Create: true})
		require.NoError(t, err)
		assert.Equal(t, "p", n.Name)
	})
	t.Run("a malformed document keeps its position", func(t *testing.T) {
		m, engine, _ := newPolicyManager(t)
		engine.EXPECT().Parse(mock.Anything, mock.Anything).Return(nil, fmt.Errorf("policy: %w", &policy.ValidationError{Statement: 2, Line: 7, Column: 3, Msg: "bad action"}))
		_, err := m.Save(ctx, admin.SavePolicyInput{Name: "p", Document: raw})
		require.ErrorIs(t, err, domain.ErrMalformedPolicy)
		p, ok := admin.ProblemOf(err)
		require.True(t, ok)
		assert.Equal(t, admin.PolicyProblem{Statement: 2, Line: 7, Column: 3, Message: "bad action"}, p)
	})
	t.Run("name and document are required", func(t *testing.T) {
		m, _, _ := newPolicyManager(t)
		_, err := m.Save(ctx, admin.SavePolicyInput{Document: raw})
		assert.ErrorIs(t, err, domain.ErrInvalidArgument)
		_, err = m.Save(ctx, admin.SavePolicyInput{Name: "p", Document: json.RawMessage("null")})
		assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	})
}

func TestPolicyManagerGet(t *testing.T) {
	m, engine, _ := newPolicyManager(t)
	engine.EXPECT().ListPolicies(mock.Anything).Return([]policy.Named{{Name: "readonly", BuiltIn: true}}, nil)
	n, err := m.Get(context.Background(), "readonly")
	require.NoError(t, err)
	assert.True(t, n.BuiltIn)
	_, err = m.Get(context.Background(), "nope")
	assert.ErrorIs(t, err, domain.ErrPolicyNotFound)
}

func TestPolicyManagerValidate(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name   string
		in     admin.ValidatePolicyInput
		setup  func(engine *policymock.Service)
		valid  bool
		report []admin.PolicyProblem
	}{
		{name: "valid IAM policy", in: admin.ValidatePolicyInput{Document: json.RawMessage(`{}`)},
			setup: func(e *policymock.Service) {
				e.EXPECT().Parse(mock.Anything, []byte(`{}`)).Return(&domain.Policy{}, nil)
			}, valid: true, report: []admin.PolicyProblem{}},
		{name: "bucket policy with a position", in: admin.ValidatePolicyInput{Document: json.RawMessage(`{}`), Bucket: "bkt"},
			setup: func(e *policymock.Service) {
				e.EXPECT().ParseBucketPolicy(mock.Anything, "bkt", []byte(`{}`)).Return(nil, fmt.Errorf("x: %w", &policy.ValidationError{Statement: 1, Element: "Principal", Msg: "required"}))
			}, report: []admin.PolicyProblem{{Statement: 1, Element: "Principal", Message: "required"}}},
		{name: "other invalid argument", in: admin.ValidatePolicyInput{Document: json.RawMessage(`{}`)},
			setup: func(e *policymock.Service) {
				e.EXPECT().Parse(mock.Anything, mock.Anything).Return(nil, fmt.Errorf("%w: too odd", domain.ErrInvalidArgument))
			}, report: []admin.PolicyProblem{{Message: "invalid argument: too odd"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, engine, _ := newPolicyManager(t)
			tt.setup(engine)
			rep, err := m.Validate(ctx, tt.in)
			require.NoError(t, err)
			assert.Equal(t, tt.valid, rep.Valid)
			assert.Equal(t, tt.report, rep.Problems)
		})
	}
	t.Run("invalid bucket", func(t *testing.T) {
		m, _, _ := newPolicyManager(t)
		_, err := m.Validate(ctx, admin.ValidatePolicyInput{Document: json.RawMessage(`{}`), Bucket: "B"})
		assert.ErrorIs(t, err, domain.ErrInvalidBucketName)
	})
}

// covers: S-IAM-04b
func TestPolicyManagerSimulate(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m, engine, creds := newPolicyManager(t)
	creds.EXPECT().LookupCredentials(mock.Anything, "alice").Return(domain.Credentials{}, domain.Identity{PolicyNames: []string{"readonly"}}, nil)
	engine.EXPECT().Simulate(mock.Anything, mock.MatchedBy(func(a policy.Args) bool {
		return a.Identity.AccessKey == "alice" && a.Identity.PolicyNames[0] == "readonly" && a.Action == policy.ActionGetObject &&
			a.Bucket == "bkt" && a.Object == "k" && a.Now.Equal(now) && a.Conditions["aws:SourceIp"][0] == "1.2.3.4"
	})).Return(policy.Decision{Allowed: true}, nil)
	sim, err := m.Simulate(ctx, admin.SimulateInput{Requester: rootReq(), User: "alice", Action: policy.ActionGetObject, Bucket: "bkt", Object: "k",
		Conditions: map[string][]string{"aws:SourceIp": {"1.2.3.4"}}, Now: now})
	require.NoError(t, err)
	assert.Equal(t, "alice", sim.User)
	assert.True(t, sim.Decision.Allowed)

	engine.EXPECT().Simulate(mock.Anything, mock.MatchedBy(func(a policy.Args) bool { return a.Identity.AccessKey == "bob" })).Return(policy.Decision{}, nil)
	sim, err = m.Simulate(ctx, admin.SimulateInput{Requester: req("bob"), Action: policy.ActionPutObject})
	require.NoError(t, err)
	assert.Equal(t, "bob", sim.User, "the requester by default")

	creds.EXPECT().LookupCredentials(mock.Anything, "ghost").Return(domain.Credentials{}, domain.Identity{}, domain.ErrInvalidAccessKey)
	_, err = m.Simulate(ctx, admin.SimulateInput{Requester: rootReq(), User: "ghost", Action: policy.ActionGetObject})
	assert.ErrorIs(t, err, domain.ErrUserNotFound)
	_, err = m.Simulate(ctx, admin.SimulateInput{Requester: rootReq()})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = m.Simulate(ctx, admin.SimulateInput{Requester: rootReq(), Action: policy.ActionGetObject, Object: "k"})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

// TestPolicyManagerSimulateClient: the service composes the keys of the
// simulated client with the shared definition (policy.RequestConditions)
// over the other conditions; an unspecified transport stays unset
// (regression: the transport handler built the keys by hand).
func TestPolicyManagerSimulateClient(t *testing.T) {
	ctx := context.Background()
	yes, no := true, false
	tests := []struct {
		name   string
		in     admin.SimulateInput
		expect map[string][]string
	}{
		{name: "no client", in: admin.SimulateInput{Conditions: map[string][]string{"aws:Referer": {"r"}}},
			expect: map[string][]string{"aws:Referer": {"r"}}},
		{name: "address only", in: admin.SimulateInput{SourceIP: "10.0.0.1"},
			expect: map[string][]string{policy.KeySourceIP: {"10.0.0.1"}}},
		{name: "transport only", in: admin.SimulateInput{SecureTransport: &no},
			expect: map[string][]string{policy.KeySecureTransport: {"false"}}},
		{name: "client over conditions", in: admin.SimulateInput{SourceIP: "10.0.0.1", SecureTransport: &yes,
			Conditions: map[string][]string{policy.KeySourceIP: {"192.0.2.1"}, "s3:prefix": {"docs/"}}},
			expect: map[string][]string{policy.KeySourceIP: {"10.0.0.1"}, policy.KeySecureTransport: {"true"}, "s3:prefix": {"docs/"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, engine, _ := newPolicyManager(t)
			var got map[string][]string
			engine.EXPECT().Simulate(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, a policy.Args) (policy.Decision, error) {
				got = a.Conditions
				return policy.Decision{}, nil
			})
			in := tt.in
			in.Requester, in.Action = req("bob"), policy.ActionGetObject
			_, err := m.Simulate(ctx, in)
			require.NoError(t, err)
			assert.Equal(t, tt.expect, got)
		})
	}
}
