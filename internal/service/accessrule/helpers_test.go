package accessrule_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/accessrule"
	arm "github.com/r3n3r0/nexo/internal/service/accessrule/mock"
	auditmock "github.com/r3n3r0/nexo/internal/service/audit/mock"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	bucketmock "github.com/r3n3r0/nexo/internal/service/bucket/mock"
	"github.com/r3n3r0/nexo/internal/service/config"
	"github.com/r3n3r0/nexo/internal/service/match"
	"github.com/r3n3r0/nexo/internal/service/policy"
)

// testBucket is the bucket of the fixtures.
const testBucket = "media"

// t0 is the time of the fixtures.
func t0() time.Time { return time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC) }

// state is the access configuration of the buckets of a fixture, shared
// by the mocks of the bucket service and of the configuration source.
type state struct {
	mu    sync.Mutex
	rules map[string]bucket.AccessRules
	block map[string]bool
	// loads counts the reads of the configuration source.
	loads int
}

// fixture wires the access rules with a real policy engine, a real matcher
// and a real dynamic configuration, the bucket metadata modeled on the
// generated mocks.
type fixture struct {
	t       *testing.T
	ctx     context.Context
	clock   *clock.Fake
	st      *state
	matcher match.Service
	config  config.Service
	buckets *bucketmock.Service
	source  *arm.ConfigSource
	audit   *auditmock.Service
	eval    accessrule.Evaluator
	policy  policy.Service
	svc     accessrule.Service
	secrets map[string]string
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newFixture builds the fixture; opts configure both the Service and the
// Evaluator.
func newFixture(t *testing.T, opts ...accessrule.Option) *fixture {
	t.Helper()
	f := &fixture{
		t: t, ctx: context.Background(), clock: clock.NewFake(t0()),
		st:      &state{rules: map[string]bucket.AccessRules{}, block: map[string]bool{}},
		matcher: match.New(match.Deps{}),
		config:  config.New(config.Deps{Store: config.NewMemoryStore(nil)}),
		buckets: bucketmock.NewService(t),
		source:  arm.NewConfigSource(t),
		audit:   auditmock.NewService(t),
		secrets: map[string]string{"proxy-token": "s3cr3t"},
	}
	f.mockBuckets()
	var err error
	f.eval, err = accessrule.NewEvaluator(accessrule.EvaluatorDeps{
		Source: f.source, Matcher: f.matcher, Config: f.config, Secrets: accessrule.NewStaticSecrets(f.secrets),
		Clock: f.clock, Logger: quietLogger(),
	}, opts...)
	require.NoError(t, err)
	f.policy = policy.New(policy.Deps{AccessRules: f.eval, Clock: f.clock, Logger: quietLogger()})
	f.svc, err = accessrule.New(accessrule.Deps{
		Buckets: f.buckets, Policy: f.policy, Matcher: f.matcher, Config: f.config, Audit: f.audit,
		Clock: f.clock, Logger: quietLogger(),
	}, opts...)
	require.NoError(t, err)
	return f
}

// mockBuckets models the access configuration of the buckets on the
// mocks (RunAndReturn); only testBucket and "other" exist.
func (f *fixture) mockBuckets() {
	st := f.st
	exists := func(name string) error {
		if name != testBucket && name != "other" {
			return fmt.Errorf("%w: %s", domain.ErrBucketNotFound, name)
		}
		return nil
	}
	current := func(name string) bucket.AccessRules {
		if r, ok := st.rules[name]; ok {
			return r
		}
		return bucket.AccessRules{Document: json.RawMessage("[]"), ETag: bucket.AccessRulesETag(nil)}
	}
	f.buckets.EXPECT().GetAccessRules(mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, name string) (bucket.AccessRules, error) {
			if err := exists(name); err != nil {
				return bucket.AccessRules{}, err
			}
			st.mu.Lock()
			defer st.mu.Unlock()
			return current(name), nil
		}).Maybe()
	f.buckets.EXPECT().SetAccessRules(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, name string, in bucket.SetAccessRulesInput) (bucket.AccessRulesChange, error) {
			if err := exists(name); err != nil {
				return bucket.AccessRulesChange{}, err
			}
			st.mu.Lock()
			defer st.mu.Unlock()
			prev := current(name)
			if in.IfMatch != "" && in.IfMatch != prev.ETag {
				return bucket.AccessRulesChange{}, domain.ErrPreconditionFailed
			}
			next := bucket.AccessRules{Document: in.Document, ETag: bucket.AccessRulesETag(in.Document), UpdatedAt: f.clock.Now(), UpdatedBy: in.UpdatedBy}
			st.rules[name] = next
			return bucket.AccessRulesChange{Previous: prev, Current: next}, nil
		}).Maybe()
	f.buckets.EXPECT().GetBlockPublicAccess(mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, name string) (bool, error) {
			if err := exists(name); err != nil {
				return false, err
			}
			st.mu.Lock()
			defer st.mu.Unlock()
			return st.block[name], nil
		}).Maybe()
	f.buckets.EXPECT().SetBlockPublicAccess(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, name string, on bool) error {
			if err := exists(name); err != nil {
				return err
			}
			st.mu.Lock()
			defer st.mu.Unlock()
			st.block[name] = on
			return nil
		}).Maybe()
	f.source.EXPECT().AccessConfig(mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, name string) (bucket.AccessConfig, error) {
			st.mu.Lock()
			defer st.mu.Unlock()
			st.loads++
			out := bucket.AccessConfig{BlockPublicAccess: st.block[name]}
			if r, ok := st.rules[name]; ok {
				out.Rules = &r
			}
			return out, nil
		}).Maybe()
	f.audit.EXPECT().Log(mock.Anything, mock.Anything).Return(nil).Maybe()
}

// put saves rules on testBucket and fails the test on error.
func (f *fixture) put(rules ...accessrule.Rule) *accessrule.Config {
	f.t.Helper()
	cfg, err := f.svc.Put(f.ctx, accessrule.PutInput{Bucket: testBucket, Rules: rules, Actor: "admin"})
	require.NoError(f.t, err)
	return cfg
}

// user is an IAM user with the given named policies and groups.
func user(name string, policies []string, groups ...string) *domain.Identity {
	return &domain.Identity{AccessKey: name, PolicyNames: policies, Groups: groups, AuthType: domain.AuthTypeHeader}
}

// req describes a request for evaluate.
type req struct {
	id     *domain.Identity
	action policy.Action
	key    string
	// prefix is the listing prefix of the bucket actions.
	prefix  *string
	ip      string
	secure  *bool
	host    string
	method  string
	referer string
	headers map[string]string
	dist    string
	now     time.Time
	bucket  *domain.Policy
}

// evaluate decides a request with the policy engine.
func (f *fixture) evaluate(r req) policy.Decision {
	f.t.Helper()
	d, err := f.decide(r, false)
	require.NoError(f.t, err)
	return d
}

func (f *fixture) decide(r req, trace bool) (policy.Decision, error) {
	pr := policy.Request{Host: r.host, Method: r.method, Distribution: r.dist}
	if r.ip != "" || r.secure != nil || r.referer != "" {
		pr.Client = &policy.RequestClient{IP: r.ip, Secure: r.secure != nil && *r.secure, Referer: r.referer}
	}
	if len(r.headers) > 0 {
		pr.HTTPHeaders = map[string][]string{}
		for k, v := range r.headers {
			pr.HTTPHeaders[k] = []string{v}
		}
	}
	conds := policy.RequestConditions(pr)
	if r.secure == nil {
		delete(conds, policy.KeySecureTransport)
	}
	if r.prefix != nil {
		conds[policy.KeyPrefix] = []string{*r.prefix}
	}
	action := r.action
	if action == "" {
		action = policy.ActionGetObject
	}
	args := policy.Args{Identity: r.id, Action: action, Bucket: testBucket, Object: r.key, Conditions: conds, Now: r.now, BucketPolicy: r.bucket}
	if trace {
		return f.policy.Simulate(f.ctx, args)
	}
	return f.policy.Evaluate(f.ctx, args)
}

func ptr[T any](v T) *T { return &v }

// rule builds an allow or deny rule.
func rule(id string, priority int, effect accessrule.Effect, principals, actions []string, m *match.Pattern, c *accessrule.Conditions) accessrule.Rule {
	return accessrule.Rule{ID: id, Priority: priority, Effect: effect, Principals: principals, Actions: actions, Match: m, Conditions: c}
}

func globP(p string) *match.Pattern   { return &match.Pattern{Type: match.TypeGlob, Pattern: p} }
func regexP(p string) *match.Pattern  { return &match.Pattern{Type: match.TypeRegex, Pattern: p} }
func prefixP(p string) *match.Pattern { return &match.Pattern{Type: match.TypePrefix, Pattern: p} }

func list(v ...string) []string { return v }
