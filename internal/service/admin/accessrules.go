package admin

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/accessrule"
	"github.com/r3n3r0/nexo/internal/service/auth"
	"github.com/r3n3r0/nexo/internal/service/policy"
)

// AccessRuleManagerDeps are the dependencies of the access rules manager.
type AccessRuleManagerDeps struct {
	// Rules manages the access rules (required).
	Rules accessrule.Service
	// Authz authorizes the tester (required).
	Authz Authorizer
	// Creds resolves the principals of the tester (required).
	Creds  auth.CredentialLookup
	Logger *slog.Logger
}

// AccessRuleManagerOption configures the access rules manager.
type AccessRuleManagerOption func(*accessRuleManager)

type accessRuleManager struct {
	deps AccessRuleManagerDeps
}

var _ AccessRuleManager = (*accessRuleManager)(nil)

// NewAccessRuleManager returns the access rules manager of the Admin API.
// A missing dependency is a wiring error (domain.ErrInvalidArgument).
func NewAccessRuleManager(deps AccessRuleManagerDeps, opts ...AccessRuleManagerOption) (AccessRuleManager, error) {
	if deps.Rules == nil || deps.Authz == nil || deps.Creds == nil {
		return nil, fmt.Errorf("%w: access rules manager: the rules, authorizer and credentials are required", domain.ErrInvalidArgument)
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	m := &accessRuleManager{deps: deps}
	for _, o := range opts {
		o(m)
	}
	return m, nil
}

// actorOf names the requester in the audit of a change: its access key,
// with its parent user for derived credentials.
func actorOf(req Requester) string {
	id := req.Identity
	switch {
	case id.Parent != "":
		return id.Parent + "/" + id.AccessKey
	case id.AccessKey != "":
		return id.AccessKey
	case id.IsRoot:
		return "root"
	}
	return "anonymous"
}

// Get implements AccessRuleManager.
func (m *accessRuleManager) Get(ctx context.Context, bucket string) (*accessrule.Config, error) {
	return m.deps.Rules.Get(ctx, bucket)
}

// Put implements AccessRuleManager.
func (m *accessRuleManager) Put(ctx context.Context, in PutAccessRulesInput) (*accessrule.Config, error) {
	rules := in.Rules
	if rules == nil {
		rules = []accessrule.Rule{}
	}
	return m.deps.Rules.Put(ctx, accessrule.PutInput{Bucket: in.Bucket, Rules: rules, IfMatch: in.IfMatch, Actor: actorOf(in.Requester)})
}

// Test implements AccessRuleManager.
func (m *accessRuleManager) Test(ctx context.Context, in TestAccessRulesInput) (*accessrule.TestResult, error) {
	if err := domain.ValidateBucketName(in.Bucket); err != nil {
		return nil, err
	}
	if err := authorizeAll(ctx, m.deps.Authz, in.Requester, in.Bucket, "", policy.ActionAdminManageAccessRules); err != nil {
		return nil, err
	}
	id, dist, err := m.principal(ctx, in.Principal)
	if err != nil {
		return nil, err
	}
	if id != nil && !sameIdentity(*id, in.Requester.Identity) {
		// The decision of the requests of another identity is what the
		// policy simulator discloses.
		if err := authorizeAll(ctx, m.deps.Authz, in.Requester, "", "", policy.ActionAdminSimulatePolicy); err != nil {
			return nil, err
		}
	}
	if in.Distribution != "" {
		if dist != "" && dist != in.Distribution {
			return nil, fmt.Errorf("%w: the principal names distribution %q, the request distribution %q", domain.ErrInvalidArgument, dist, in.Distribution)
		}
		dist = in.Distribution
	}
	return m.deps.Rules.Test(ctx, accessrule.TestInput{
		Bucket: in.Bucket, Keys: in.Keys, Action: in.Action, Identity: id, IP: in.IP, Host: in.Host, Method: in.Method,
		Secure: in.Secure, Referer: in.Referer, Headers: in.Headers, Distribution: dist, Now: in.Time,
	})
}

// principal resolves the principal of a test: the identity of its
// credentials (nil for an anonymous request) and the distribution of a
// "distribution:<id>" principal.
func (m *accessRuleManager) principal(ctx context.Context, p string) (*domain.Identity, string, error) {
	p = strings.TrimSpace(p)
	switch {
	case p == "" || p == accessrule.PrincipalEveryone || strings.EqualFold(p, PrincipalAnonymous):
		return nil, "", nil
	case strings.HasPrefix(p, accessrule.PrincipalDistributionPrefix):
		dist := strings.TrimPrefix(p, accessrule.PrincipalDistributionPrefix)
		if dist == "" {
			return nil, "", fmt.Errorf("%w: principal %q names no distribution", domain.ErrInvalidArgument, p)
		}
		return nil, dist, nil
	case strings.HasPrefix(p, accessrule.PrincipalGroupPrefix):
		return nil, "", fmt.Errorf("%w: principal %q: a group makes no request, test one of its users", domain.ErrInvalidArgument, p)
	}
	key := p
	for _, prefix := range []string{accessrule.PrincipalUserPrefix, accessrule.PrincipalAccessKeyPrefix} {
		if rest, ok := strings.CutPrefix(p, prefix); ok {
			key = rest
			break
		}
	}
	if key == "" || strings.Contains(key, ":") {
		return nil, "", fmt.Errorf("%w: principal %q: expected anonymous, user:<name>, accesskey:<key> or distribution:<id>", domain.ErrInvalidArgument, p)
	}
	id, err := lookupIdentity(ctx, m.deps.Creds, key)
	if err != nil {
		return nil, "", err
	}
	return &id, "", nil
}

// sameIdentity reports whether id is the requester itself.
func sameIdentity(id, requester domain.Identity) bool {
	return !requester.IsAnonymous && id.AccessKey != "" && id.AccessKey == requester.AccessKey
}

// Public implements AccessRuleManager.
func (m *accessRuleManager) Public(ctx context.Context, bucket string) (*accessrule.PublicStatus, error) {
	return m.deps.Rules.PublicStatus(ctx, bucket)
}

// ApplyPreset implements AccessRuleManager.
func (m *accessRuleManager) ApplyPreset(ctx context.Context, in AccessPresetInput) (*accessrule.Config, error) {
	return m.deps.Rules.ApplyPreset(ctx, accessrule.PresetInput{
		Bucket: in.Bucket, Preset: in.Preset, Match: in.Match, Confirm: in.Confirm, IfMatch: in.IfMatch, Actor: actorOf(in.Requester),
	})
}

// BlockPublic implements AccessRuleManager.
func (m *accessRuleManager) BlockPublic(ctx context.Context, bucket string) (*accessrule.BlockPublic, error) {
	return m.deps.Rules.GetBlockPublic(ctx, bucket)
}

// SetBlockPublic implements AccessRuleManager.
func (m *accessRuleManager) SetBlockPublic(ctx context.Context, in SetBlockPublicInput) (*accessrule.BlockPublic, error) {
	if in.Bucket != "" && in.Mode != "" {
		return nil, fmt.Errorf("%w: the mode of the block public access is a setting of the cluster", domain.ErrInvalidArgument)
	}
	switch in.Mode {
	case "", accessrule.BlockModeReject, accessrule.BlockModeIgnore:
	default:
		return nil, fmt.Errorf("%w: block public access mode %q (reject, ignore)", domain.ErrInvalidArgument, in.Mode)
	}
	return m.deps.Rules.SetBlockPublic(ctx, accessrule.BlockPublicInput{
		Bucket: in.Bucket, Enabled: in.Enabled, Mode: in.Mode, Actor: actorOf(in.Requester),
	})
}
