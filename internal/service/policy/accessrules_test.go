package policy_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/policy"
	policymock "github.com/r3n3r0/nexo/internal/service/policy/mock"
)

// engineWithRules returns an engine whose access rules come from the mock.
func engineWithRules(t *testing.T, rules *policymock.AccessRules) policy.Service {
	t.Helper()
	svc := policy.New(policy.Deps{Store: policy.NewMemoryStore(), AccessRules: rules, Clock: clock.NewFake(testNow), Logger: slog.New(slog.DiscardHandler)})
	require.NoError(t, svc.SetPolicy(context.Background(), "deny-secret", mustPolicy(t, doc(
		`{"Effect":"Deny","Action":"s3:*","Resource":"arn:aws:s3:::b/secret/*"}`,
		`{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}`))))
	return svc
}

func denyRule(id string) *policy.RuleMatch {
	return &policy.RuleMatch{ID: id, Priority: 1, Effect: domain.Deny}
}

func allowRule(id string) *policy.RuleMatch {
	return &policy.RuleMatch{ID: id, Priority: 2, Effect: domain.Allow}
}

// TestAccessRulesSource checks how the engine combines the access rules
// with the other sources (docs/10 §10.1 Evaluation).
func TestAccessRulesSource(t *testing.T) {
	inline := mustPolicy(t, doc(`{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/only/*"}`))
	tests := []struct {
		name    string
		id      *domain.Identity
		object  string
		outcome policy.RuleOutcome
		allowed bool
		source  policy.Source
		rule    string
		// called reports whether the rules are evaluated at all.
		called bool
	}{
		{name: "deny rule beats IAM allow", id: user("u", "deny-secret"), object: "a",
			outcome: policy.RuleOutcome{Deny: denyRule("d"), Allow: allowRule("a")}, source: policy.SourceAccessRule, rule: "d", called: true},
		{name: "IAM allow without rules", id: user("u", "deny-secret"), object: "a", allowed: true, source: policy.SourceIAM, called: true},
		{name: "IAM deny decides first", id: user("u", "deny-secret"), object: "secret/x",
			source: policy.SourceIAM, called: false},
		{name: "allow rule for a user without policies", id: user("v"), object: "a",
			outcome: policy.RuleOutcome{Allow: allowRule("a")}, allowed: true, source: policy.SourceAccessRule, rule: "a", called: true},
		{name: "IAM allow named before an allow rule", id: user("u", "deny-secret"), object: "a",
			outcome: policy.RuleOutcome{Allow: allowRule("a")}, allowed: true, source: policy.SourceIAM, called: true},
		{name: "anonymous allow rule", id: domain.Anonymous(), object: "a",
			outcome: policy.RuleOutcome{Allow: allowRule("a")}, allowed: true, source: policy.SourceAccessRule, rule: "a", called: true},
		{name: "anonymous without rules", id: domain.Anonymous(), object: "a", called: true},
		{name: "inline policy limits an allow rule", id: &domain.Identity{AccessKey: "svc", Parent: "v", SessionPolicy: inline}, object: "a",
			outcome: policy.RuleOutcome{Allow: allowRule("a")}, source: policy.SourceInline, called: true},
		{name: "inline policy within an allow rule", id: &domain.Identity{AccessKey: "svc", Parent: "v", SessionPolicy: inline}, object: "only/x",
			outcome: policy.RuleOutcome{Allow: allowRule("a")}, allowed: true, source: policy.SourceAccessRule, rule: "a", called: true},
		{name: "root bypasses the rules", id: &domain.Identity{AccessKey: "root", IsRoot: true}, object: "a",
			allowed: true, source: policy.SourceRoot},
		{name: "root with an inline policy bypasses the rules", id: &domain.Identity{AccessKey: "rsvc", IsRoot: true, SessionPolicy: inline}, object: "only/x",
			allowed: true, source: policy.SourceInline},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rules := policymock.NewAccessRules(t)
			var got policy.RuleRequest
			if tt.called {
				rules.EXPECT().EvaluateRules(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, in policy.RuleRequest) (policy.RuleOutcome, error) {
					got = in
					return tt.outcome, nil
				}).Once()
			}
			svc := engineWithRules(t, rules)
			args := policy.Args{Identity: tt.id, Action: policy.ActionGetObject, Bucket: "b", Object: tt.object,
				Conditions: map[string][]string{"aws:SourceIp": {"10.0.0.1"}, "nexo:Host": {"cdn.example.com"}}}
			d, err := svc.Evaluate(context.Background(), args)
			require.NoError(t, err)
			assert.Equal(t, tt.allowed, d.Allowed, d.Reason)
			assert.Equal(t, tt.source, d.Source, d.Reason)
			if tt.rule != "" {
				require.NotNil(t, d.Rule)
				assert.Equal(t, tt.rule, d.Rule.ID)
				assert.Contains(t, d.Reason, `access rule "`+tt.rule+`"`)
			} else {
				assert.Nil(t, d.Rule)
			}
			if tt.called {
				assert.Equal(t, "b", got.Bucket)
				assert.Equal(t, tt.object, got.Object)
				assert.Equal(t, policy.ActionGetObject, got.Action)
				assert.Equal(t, testNow, got.Now)
				assert.False(t, got.Trace)
				assert.Equal(t, []string{"10.0.0.1"}, got.Conditions["aws:sourceip"], "normalized keys")
				assert.Equal(t, []string{"cdn.example.com"}, got.Conditions["nexo:host"])
				if !tt.id.IsAnonymous {
					assert.Equal(t, []string{tt.id.EffectiveUser()}, got.Conditions["aws:username"])
				}
			}
		})
	}
}

// TestAccessRulesInSimulator checks that the simulator traces the rules
// and names the deciding one, even after an explicit deny.
//
// covers: S-IAM-04b, S-ACC-04
func TestAccessRulesInSimulator(t *testing.T) {
	rules := policymock.NewAccessRules(t)
	evaluated := []policy.EvaluatedRule{{RuleMatch: *denyRule("d"), Matched: true}, {RuleMatch: *allowRule("a"), Mismatch: policy.RuleMismatchKey}}
	rules.EXPECT().EvaluateRules(mock.Anything, mock.MatchedBy(func(in policy.RuleRequest) bool { return in.Trace })).
		Return(policy.RuleOutcome{Deny: denyRule("d"), Evaluated: evaluated}, nil).Twice()
	svc := engineWithRules(t, rules)
	d, err := svc.Simulate(context.Background(), policy.Args{Identity: user("u", "deny-secret"), Action: policy.ActionGetObject, Bucket: "b", Object: "a"})
	require.NoError(t, err)
	assert.False(t, d.Allowed)
	assert.Equal(t, policy.SourceAccessRule, d.Source)
	assert.Equal(t, "d", d.Rule.ID)
	assert.Equal(t, evaluated, d.EvaluatedRules)
	assert.NotEmpty(t, d.Evaluated)

	// An IAM deny decides, the rules are still traced.
	d, err = svc.Simulate(context.Background(), policy.Args{Identity: user("u", "deny-secret"), Action: policy.ActionGetObject, Bucket: "b", Object: "secret/x"})
	require.NoError(t, err)
	assert.Equal(t, policy.SourceIAM, d.Source)
	assert.Nil(t, d.Rule)
	assert.Equal(t, evaluated, d.EvaluatedRules)
}

// TestAccessRulesFailClosed checks that an error of the rules is returned
// (the caller denies) and that requests without a bucket skip them.
func TestAccessRulesFailClosed(t *testing.T) {
	rules := policymock.NewAccessRules(t)
	boom := errors.New("store down")
	rules.EXPECT().EvaluateRules(mock.Anything, mock.Anything).Return(policy.RuleOutcome{}, boom).Once()
	svc := engineWithRules(t, rules)
	_, err := svc.Evaluate(context.Background(), policy.Args{Identity: user("u", "deny-secret"), Action: policy.ActionGetObject, Bucket: "b", Object: "a"})
	assert.ErrorIs(t, err, boom)
	d, err := svc.Evaluate(context.Background(), policy.Args{Identity: user("u", "deny-secret"), Action: policy.ActionListAllMyBuckets})
	require.NoError(t, err)
	assert.False(t, d.Allowed, "no bucket: the rules are not asked")
}

// TestRequestConditionsNexoKeys checks the condition keys of the access
// rules: host without port, method, distribution and the headers but
// those carrying credentials.
func TestRequestConditionsNexoKeys(t *testing.T) {
	conds := policy.RequestConditions(policy.Request{
		Host: "CDN.Example.com:9000", Method: "get", Distribution: "website",
		HTTPHeaders: map[string][]string{
			"X-Origin-Secret": {"s"}, "Authorization": {"AWS4..."}, "Cookie": {"c"}, "X-Amz-Security-Token": {"t"},
			"X-Amz-Server-Side-Encryption-Customer-Key": {"k"}, "X-Amz-Server-Side-Encryption-Customer-Key-Md5": {"m"}, "Referer": {"r"},
		},
	})
	assert.Equal(t, []string{"cdn.example.com"}, conds[policy.KeyHost])
	assert.Equal(t, []string{"GET"}, conds[policy.KeyMethod])
	assert.Equal(t, []string{"website"}, conds[policy.KeyDistribution])
	assert.Equal(t, []string{"s"}, conds[policy.KeyHeaderPrefix+"x-origin-secret"])
	assert.Equal(t, []string{"m"}, conds[policy.KeyHeaderPrefix+"x-amz-server-side-encryption-customer-key-md5"])
	assert.Equal(t, []string{"r"}, conds[policy.KeyHeaderPrefix+"referer"])
	for _, secret := range []string{"authorization", "cookie", "x-amz-security-token", "x-amz-server-side-encryption-customer-key"} {
		assert.NotContains(t, conds, policy.KeyHeaderPrefix+secret)
	}
	assert.Equal(t, []string{"::1"}, policy.RequestConditions(policy.Request{Host: "[::1]:9000"})[policy.KeyHost])
	assert.Empty(t, policy.RequestConditions(policy.Request{}))
}
