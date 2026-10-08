package admin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/auth"
	"github.com/r3n3r0/nexo/internal/service/policy"
)

// PolicyManagerDeps are the dependencies of the policy manager.
type PolicyManagerDeps struct {
	// Policy is the policy engine and store (required).
	Policy policy.Service
	// Creds resolves the identity of a simulated user (required).
	Creds  auth.CredentialLookup
	Logger *slog.Logger
}

// PolicyManagerOption configures the policy manager.
type PolicyManagerOption func(*policyManager)

type policyManager struct {
	deps PolicyManagerDeps
}

var _ PolicyManager = (*policyManager)(nil)

// NewPolicyManager returns the policy manager. Missing dependencies are a
// wiring error (domain.ErrInvalidArgument).
func NewPolicyManager(deps PolicyManagerDeps, opts ...PolicyManagerOption) (PolicyManager, error) {
	if deps.Policy == nil || deps.Creds == nil {
		return nil, fmt.Errorf("%w: policy manager requires the policy engine and the credential lookup", domain.ErrInvalidArgument)
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	m := &policyManager{deps: deps}
	for _, o := range opts {
		o(m)
	}
	return m, nil
}

// Get implements PolicyManager.
func (m *policyManager) Get(ctx context.Context, name string) (*policy.Named, error) {
	all, err := m.deps.Policy.ListPolicies(ctx)
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].Name == name {
			return &all[i], nil
		}
	}
	return nil, fmt.Errorf("%w: %q", domain.ErrPolicyNotFound, name)
}

// Save implements PolicyManager.
func (m *policyManager) Save(ctx context.Context, in SavePolicyInput) (*policy.Named, error) {
	if strings.TrimSpace(in.Name) == "" {
		return nil, fmt.Errorf("%w: policy name is required", domain.ErrInvalidArgument)
	}
	if len(in.Document) == 0 || string(in.Document) == "null" {
		return nil, fmt.Errorf("%w: policy document is required", domain.ErrInvalidArgument)
	}
	doc, err := m.deps.Policy.Parse(ctx, in.Document)
	if err != nil {
		return nil, err
	}
	if in.Create {
		_, err := m.deps.Policy.GetPolicy(ctx, in.Name)
		switch {
		case err == nil:
			return nil, fmt.Errorf("%w: policy %q already exists", domain.ErrConflict, in.Name)
		case !errors.Is(err, domain.ErrPolicyNotFound):
			return nil, err
		}
	}
	if err := m.deps.Policy.SetPolicy(ctx, in.Name, doc); err != nil {
		return nil, err
	}
	m.deps.Logger.InfoContext(ctx, "admin: policy saved", "policy", in.Name)
	return m.Get(ctx, in.Name)
}

// Validate implements PolicyManager.
func (m *policyManager) Validate(ctx context.Context, in ValidatePolicyInput) (*ValidationReport, error) {
	var err error
	if in.Bucket != "" {
		if verr := domain.ValidateBucketName(in.Bucket); verr != nil {
			return nil, verr
		}
		_, err = m.deps.Policy.ParseBucketPolicy(ctx, in.Bucket, in.Document)
	} else {
		_, err = m.deps.Policy.Parse(ctx, in.Document)
	}
	if err == nil {
		return &ValidationReport{Valid: true, Problems: []PolicyProblem{}}, nil
	}
	if p, ok := ProblemOf(err); ok {
		return &ValidationReport{Problems: []PolicyProblem{p}}, nil
	}
	if errors.Is(err, domain.ErrInvalidArgument) {
		return &ValidationReport{Problems: []PolicyProblem{{Message: err.Error()}}}, nil
	}
	return nil, err
}

// ProblemOf extracts the location of a policy validation error (a
// *policy.ValidationError in the chain of err).
func ProblemOf(err error) (PolicyProblem, bool) {
	var ve *policy.ValidationError
	if !errors.As(err, &ve) {
		return PolicyProblem{}, false
	}
	return PolicyProblem{Statement: ve.Statement, Sid: ve.Sid, Element: ve.Element, Line: ve.Line, Column: ve.Column, Message: ve.Msg}, true
}

// Simulate implements PolicyManager.
func (m *policyManager) Simulate(ctx context.Context, in SimulateInput) (*Simulation, error) {
	if in.Action == "" {
		return nil, fmt.Errorf("%w: action is required", domain.ErrInvalidArgument)
	}
	if in.Object != "" && in.Bucket == "" {
		return nil, fmt.Errorf("%w: an object needs a bucket", domain.ErrInvalidArgument)
	}
	id := in.Requester.Identity
	user := id.AccessKey
	if in.User != "" && in.User != id.AccessKey {
		other, err := lookupIdentity(ctx, m.deps.Creds, in.User)
		if err != nil {
			return nil, err
		}
		id, user = other, in.User
	}
	d, err := m.deps.Policy.Simulate(ctx, policy.Args{Identity: &id, Action: in.Action, Bucket: in.Bucket, Object: in.Object, Conditions: in.Conditions, Now: in.Now})
	if err != nil {
		return nil, err
	}
	return &Simulation{User: user, Decision: d}, nil
}
