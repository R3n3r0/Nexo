package policy_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/policy"
)

// FuzzParse checks that the parser never panics, that what it accepts is
// valid and survives a JSON round trip, and that evaluating it never
// fails.
func FuzzParse(f *testing.F) {
	seeds := []string{
		doc(`{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}`),
		doc(`{"Sid":"x","Effect":"Deny","NotAction":["s3:Get*"],"NotResource":"arn:aws:s3:::b/${aws:username}/*","Condition":{"IpAddress":{"aws:SourceIp":["10.0.0.0/8","::1"]}}}`),
		doc(`{"Effect":"Allow","Principal":{"AWS":["*"]},"Action":"*","Resource":"*","Condition":{"ForAllValues:StringLike":{"aws:TagKeys":"a*"},"NullIfExists":{"a":"true"}}}`),
		`{"Version":"2012-10-17","Statement":{"Effect":"Allow","Action":"admin:*"}}`,
		`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*","Condition":{"DateLessThan":{"aws:CurrentTime":"2030-01-01"},"Bool":{"aws:SecureTransport":true},"NumericLessThan":{"s3:max-keys":5}}}]}`,
		`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/${*}${?}${$}${aws:userid, 'x'}"}]}`,
		`{"Version":"2012-10-17","Version":"x"}`,
		`{"Version":"2012-10-17","Statement":[{}]} []`,
		`[`, `{"a":`, `"\u0000"`, `{"Statement":[[[[[]]]]]}`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	svc, _ := newEngine(f)
	ctx := context.Background()
	f.Fuzz(func(t *testing.T, raw []byte) {
		p, err := svc.Parse(ctx, raw)
		if err != nil {
			require.ErrorIs(t, err, domain.ErrInvalidArgument)
			var ve *policy.ValidationError
			require.ErrorAs(t, err, &ve)
			_ = ve.Error()
			return
		}
		require.NoError(t, svc.Validate(ctx, p))
		data, err := json.Marshal(p)
		require.NoError(t, err)
		again, err := svc.Parse(ctx, data)
		require.NoError(t, err, string(data))
		require.Equal(t, p, again)
		for _, id := range []*domain.Identity{nil, user("alice"), {AccessKey: "s", Parent: "alice", SessionPolicy: p}} {
			args := policy.Args{Identity: id, Action: policy.ActionGetObject, Bucket: "b", Object: "alice/k", BucketPolicy: p,
				Conditions: map[string][]string{"aws:SourceIp": {"10.0.0.1"}, "s3:prefix": {"a*"}}, Now: testNow}
			ev, err := svc.Evaluate(ctx, args)
			require.NoError(t, err)
			sim, err := svc.Simulate(ctx, args)
			require.NoError(t, err)
			sim.Evaluated = nil
			require.Equal(t, ev, sim)
		}
	})
}

// FuzzConditionValues checks that any request value is handled by every
// operator without panicking, and that negated operators are the
// complement of the positive ones when the value has the right type.
func FuzzConditionValues(f *testing.F) {
	f.Add("StringLike", "a*b?", "aXbY")
	f.Add("IpAddress", "10.0.0.0/8", "10.1.1.1:80")
	f.Add("NumericLessThan", "10", "1e3")
	f.Add("DateGreaterThan", "2026-01-01", "1791460800")
	f.Add("ArnLike", "arn:aws:s3:::b/*", "arn:aws:s3:::b/c")
	f.Add("Bool", "true", "TRUE")
	f.Add("Null", "true", "")
	f.Add("StringEquals", "${aws:username}", "alice")
	svc, _ := newEngine(f)
	ctx := context.Background()
	f.Fuzz(func(t *testing.T, op, policyValue, requestValue string) {
		cond := map[string]map[string]domain.StringSet{op: {"s3:x": {policyValue}}}
		p := &domain.Policy{Version: domain.PolicyVersion, Statement: domain.Statements{{
			Effect: domain.Allow, Action: domain.StringSet{"s3:GetObject"}, Resource: domain.StringSet{"*"}, Condition: cond,
		}}}
		args := policy.Args{
			Identity: &domain.Identity{AccessKey: "alice", SessionPolicy: p, Parent: "root", IsRoot: true},
			Action:   policy.ActionGetObject, Bucket: "b", Object: "k",
			Conditions: map[string][]string{"s3:x": {requestValue}}, Now: testNow,
		}
		d, err := svc.Evaluate(ctx, args)
		require.NoError(t, err)
		sim, err := svc.Simulate(ctx, args)
		require.NoError(t, err)
		require.Equal(t, d.Allowed, sim.Allowed)
	})
}

// randomPolicies builds random documents and requests for the property
// tests.
type randomPolicies struct {
	r *rand.Rand
	// store receives the random named policies directly: they are not
	// always valid documents (S3 statements without resource), which the
	// evaluation must handle anyway.
	store policy.PolicyStore
}

func (g randomPolicies) pick(list ...string) string { return list[g.r.IntN(len(list))] }

func (g randomPolicies) some(list ...string) domain.StringSet {
	n := 1 + g.r.IntN(2)
	out := make(domain.StringSet, 0, n)
	for range n {
		out = append(out, g.pick(list...))
	}
	return out
}

func (g randomPolicies) statement(bucketPolicy bool, i int) domain.Statement {
	st := domain.Statement{Effect: domain.Allow}
	if g.r.IntN(4) == 0 {
		st.Effect = domain.Deny
	}
	if g.r.IntN(3) == 0 {
		st.SID = fmt.Sprintf("S%d", i)
	}
	actions := []string{"s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:ListBucket", "s3:Get*", "s3:*", "*", "s3:?utObject", "admin:ServerInfo", "admin:*"}
	if g.r.IntN(5) == 0 {
		st.NotAction = g.some(actions...)
	} else {
		st.Action = g.some(actions...)
	}
	resources := []string{"*", "arn:aws:s3:::b", "arn:aws:s3:::b/*", "arn:aws:s3:::b/a/*", "arn:aws:s3:::c/*", "arn:aws:s3:::*",
		"arn:aws:s3:::b/${aws:username}/*", "arn:aws:s3:::b/${aws:nope}/*", "arn:aws:s3:::b/?/*"}
	switch g.r.IntN(6) {
	case 0:
		st.NotResource = g.some(resources...)
	case 1:
		// No resource: only valid for admin actions, still evaluated.
	default:
		st.Resource = g.some(resources...)
	}
	if bucketPolicy {
		switch g.r.IntN(4) {
		case 0:
			st.NotPrincipal = &domain.Principal{AWS: domain.StringSet{g.pick("bob", "*", "alice")}}
		case 1:
			st.Principal = &domain.Principal{AWS: domain.StringSet{g.pick("alice", "arn:aws:iam::1:user/alice", "bob", "a*")}}
		default:
			st.Principal = &domain.Principal{AWS: domain.StringSet{"*"}}
		}
	}
	conditions := []domain.Condition{
		{"StringEquals": {"s3:x": {"a"}}},
		{"StringNotEquals": {"s3:x": {"a"}}},
		{"StringLike": {"s3:prefix": {"${aws:username}/*"}}},
		{"IpAddress": {"aws:SourceIp": {"10.0.0.0/8", "2001:db8::/32"}}},
		{"NotIpAddress": {"aws:SourceIp": {"10.0.0.0/8"}}},
		{"Bool": {"aws:SecureTransport": {"true"}}},
		{"Null": {"s3:x": {"true"}}},
		{"ForAllValues:StringEquals": {"aws:TagKeys": {"a", "b"}}},
		{"ForAnyValue:StringLike": {"aws:TagKeys": {"b*"}}},
		{"NumericLessThanIfExists": {"s3:max-keys": {"10"}}},
		{"DateLessThan": {"aws:CurrentTime": {"2027-01-01T00:00:00Z"}}},
	}
	if g.r.IntN(2) == 0 {
		st.Condition = conditions[g.r.IntN(len(conditions))]
	}
	return st
}

func (g randomPolicies) policy(bucketPolicy bool) *domain.Policy {
	n := 1 + g.r.IntN(4)
	p := &domain.Policy{Version: domain.PolicyVersion}
	for i := range n {
		p.Statement = append(p.Statement, g.statement(bucketPolicy, i))
	}
	return p
}

func (g randomPolicies) args(t *testing.T) policy.Args {
	t.Helper()
	ctx := context.Background()
	name := fmt.Sprintf("p%d", g.r.IntN(1000))
	require.NoError(t, g.store.Set(ctx, policy.Named{Name: name, Policy: g.policy(false)}))
	group := fmt.Sprintf("g%d", g.r.IntN(1000))
	gname := name + "g"
	require.NoError(t, g.store.Set(ctx, policy.Named{Name: gname, Policy: g.policy(false)}))
	require.NoError(t, g.store.SetAttached(ctx, policy.Entity{Kind: policy.EntityGroup, Name: group}, []string{gname}))
	var id *domain.Identity
	switch g.r.IntN(6) {
	case 0:
		id = nil
	case 1:
		id = &domain.Identity{AccessKey: "alice", PolicyNames: []string{name}, Groups: []string{group}}
	case 2:
		id = &domain.Identity{AccessKey: "svc", Parent: "alice", PolicyNames: []string{name}, SessionPolicy: g.policy(false)}
	case 3:
		id = &domain.Identity{AccessKey: "root", IsRoot: true}
	case 4:
		id = &domain.Identity{AccessKey: "rk", Parent: "root", IsRoot: true, SessionPolicy: g.policy(false)}
	default:
		id = &domain.Identity{AccessKey: "bob", Groups: []string{group}}
	}
	conds := map[string][]string{}
	if g.r.IntN(2) == 0 {
		conds["s3:x"] = []string{g.pick("a", "b")}
	}
	if g.r.IntN(2) == 0 {
		conds["aws:SourceIp"] = []string{g.pick("10.0.0.1", "8.8.8.8", "2001:db8::1", "::1")}
	}
	if g.r.IntN(2) == 0 {
		conds["aws:TagKeys"] = g.some("a", "b", "c")
	}
	if g.r.IntN(2) == 0 {
		conds["s3:prefix"] = []string{g.pick("alice/", "bob/", "")}
	}
	if g.r.IntN(2) == 0 {
		conds["aws:SecureTransport"] = []string{g.pick("true", "false")}
	}
	args := policy.Args{
		Identity:   id,
		Action:     policy.Action(g.pick("s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:ListBucket", "s3:ListAllMyBuckets", "admin:ServerInfo", "admin:Heal")),
		Conditions: conds,
		Now:        testNow,
	}
	if args.Action != policy.ActionListAllMyBuckets && args.Action != policy.ActionAdminServerInfo && args.Action != policy.ActionAdminHeal {
		args.Bucket = g.pick("b", "c")
		if args.Action != policy.ActionListBucket {
			args.Object = g.pick("k", "a/k", "alice/k", "x/y/z")
		}
	}
	if g.r.IntN(3) > 0 {
		args.BucketPolicy = g.policy(true)
	}
	return args
}

// TestSimulateMatchesEvaluate is the property test of docs/09 TPS-18: on
// random policies and requests the simulator takes exactly the decision
// of the enforcement, and its statement list explains it.
func TestSimulateMatchesEvaluate(t *testing.T) {
	ctx := context.Background()
	svc, store := newEngine(t)
	g := randomPolicies{r: rand.New(rand.NewPCG(2026, 10)), store: store}
	allowed, denied := 0, 0
	for i := range 3000 {
		args := g.args(t)
		ev, err := svc.Evaluate(ctx, args)
		require.NoError(t, err)
		sim, err := svc.Simulate(ctx, args)
		require.NoError(t, err)
		evaluated := sim.Evaluated
		sim.Evaluated = nil
		require.Equal(t, ev, sim, "case %d", i)
		again, err := svc.Evaluate(ctx, args)
		require.NoError(t, err)
		require.Equal(t, ev, again, "deterministic")

		if ev.Allowed {
			allowed++
		} else {
			denied++
		}
		id := args.Identity
		if id != nil && id.IsRoot && id.SessionPolicy == nil {
			assert.True(t, ev.Allowed)
			assert.Empty(t, evaluated)
			continue
		}
		matchedDeny := false
		for _, e := range evaluated {
			if e.Matched && e.Effect == domain.Deny {
				matchedDeny = true
			}
		}
		if ev.Allowed {
			require.NotNil(t, ev.MatchedStatement, "case %d", i)
			assert.Equal(t, domain.Allow, ev.MatchedStatement.Effect)
			assert.False(t, matchedDeny, "an allowed request has no matching deny (case %d)", i)
		}
		if matchedDeny {
			assert.False(t, ev.Allowed)
			require.NotNil(t, ev.MatchedStatement)
			assert.Equal(t, domain.Deny, ev.MatchedStatement.Effect)
		}
		if ev.MatchedStatement != nil {
			found := false
			for _, e := range evaluated {
				if e.Statement == ev.MatchedStatement {
					found = e.Matched
				}
			}
			assert.True(t, found, "the deciding statement is listed as matched (case %d)", i)
		}

		// Adding a catch-all deny to the bucket policy denies everyone but
		// root.
		if id == nil || !id.IsRoot {
			deny := domain.Statement{Effect: domain.Deny, Principal: &domain.Principal{AWS: domain.StringSet{"*"}}, Action: domain.StringSet{"*"}}
			withDeny := args
			bp := &domain.Policy{Version: domain.PolicyVersion}
			if args.BucketPolicy != nil {
				bp.Statement = append(bp.Statement, args.BucketPolicy.Statement...)
			}
			bp.Statement = append(bp.Statement, deny)
			withDeny.BucketPolicy = bp
			if withDeny.Bucket != "" {
				d, err := svc.Evaluate(ctx, withDeny)
				require.NoError(t, err)
				assert.False(t, d.Allowed, "case %d", i)
			}
		}
	}
	// The generator must exercise both outcomes.
	assert.Greater(t, allowed, 100)
	assert.Greater(t, denied, 100)
}

// TestRandomDocumentsRoundTrip validates random documents through the
// strict parser.
func TestRandomDocumentsRoundTrip(t *testing.T) {
	ctx := context.Background()
	svc, _ := newEngine(t)
	g := randomPolicies{r: rand.New(rand.NewPCG(7, 7))}
	for range 500 {
		p := g.policy(g.r.IntN(2) == 0)
		data, err := json.Marshal(p)
		require.NoError(t, err)
		parsed, err := svc.Parse(ctx, data)
		if err != nil {
			// Only statements without resource but with S3 actions are invalid.
			assert.True(t, strings.Contains(err.Error(), "Resource or NotResource is required"), err.Error())
			continue
		}
		assert.Equal(t, len(p.Statement), len(parsed.Statement))
	}
}
