package policy_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/policy"
	policymock "github.com/r3n3r0/nexo/internal/service/policy/mock"
)

// evalCase is one row of the evaluation matrix.
type evalCase struct {
	name    string
	args    policy.Args
	allowed bool
	source  policy.Source
	// name of the policy or Sid of the deciding statement, when checked.
	policyName string
	sid        string
}

func runEvalCases(t *testing.T, svc policy.Service, cases []evalCase) {
	t.Helper()
	ctx := context.Background()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.args.Now.IsZero() {
				tc.args.Now = testNow
			}
			d, err := svc.Evaluate(ctx, tc.args)
			require.NoError(t, err)
			assert.Equal(t, tc.allowed, d.Allowed, d.Reason)
			assert.Equal(t, tc.source, d.Source, d.Reason)
			if tc.policyName != "" {
				assert.Equal(t, tc.policyName, d.PolicyName)
			}
			if tc.sid != "" {
				assert.Equal(t, tc.sid, d.Sid)
			}
			assert.NotEmpty(t, d.Reason)
			if d.MatchedStatement != nil {
				assert.Equal(t, d.MatchedStatement.SID, d.Sid)
			}
			sim, err := svc.Simulate(ctx, tc.args)
			require.NoError(t, err)
			evaluated := sim.Evaluated
			sim.Evaluated = nil
			assert.Equal(t, d, sim, "Simulate and Evaluate agree")
			for _, e := range evaluated {
				assert.Equal(t, e.Matched, e.Mismatch == policy.MismatchNone)
			}
		})
	}
}

// TestEvaluateStatements covers Action/NotAction, Resource/NotResource,
// wildcards, variables and the built-in policies.
func TestEvaluateStatements(t *testing.T) {
	svc := engineWith(t, map[string]string{
		"get-wild": doc(`{"Sid":"GetAll","Effect":"Allow","Action":"s3:Get*","Resource":"arn:aws:s3:::b/*"}`),
		"question": doc(`{"Effect":"Allow","Action":"s3:?etObject","Resource":"arn:aws:s3:::b/*"}`),
		"all":      doc(`{"Effect":"Allow","Action":"*","Resource":"*"}`),
		"upper":    doc(`{"Effect":"Allow","Action":"S3:GETOBJECT","Resource":"arn:aws:s3:::b/*"}`),
		"not-del":  doc(`{"Effect":"Allow","NotAction":"s3:DeleteObject","Resource":"arn:aws:s3:::b/*"}`),
		"deny-not-get": doc(
			`{"Effect":"Allow","Action":"s3:*","Resource":"arn:aws:s3:::b/*"}`,
			`{"Sid":"OnlyGet","Effect":"Deny","NotAction":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}`),
		"jpg":         doc(`{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*.jpg"}`),
		"one-char":    doc(`{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/v?/*"}`),
		"upper-res":   doc(`{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::B/*"}`),
		"not-secret":  doc(`{"Effect":"Allow","Action":"s3:GetObject","NotResource":["arn:aws:s3:::secret/*","arn:aws:s3:::secret"]}`),
		"bucket-only": doc(`{"Effect":"Allow","Action":["s3:ListBucket","s3:GetObject"],"Resource":"arn:aws:s3:::b"}`),
		"list-all":    doc(`{"Effect":"Allow","Action":"s3:ListAllMyBuckets","Resource":"arn:aws:s3:::*"}`),
		"list-b":      doc(`{"Effect":"Allow","Action":"s3:ListAllMyBuckets","Resource":"arn:aws:s3:::b"}`),
		"home": doc(
			`{"Effect":"Allow","Action":"s3:ListBucket","Resource":"arn:aws:s3:::home","Condition":{"StringLike":{"s3:prefix":["${aws:username}/*","${aws:username}"]}}}`,
			`{"Effect":"Allow","Action":["s3:GetObject","s3:PutObject"],"Resource":"arn:aws:s3:::home/${aws:username}/*"}`),
		"not-home": doc(`{"Effect":"Allow","Action":"s3:GetObject","NotResource":"arn:aws:s3:::home/${aws:nothing}/*"}`),
		"escaped":  doc(`{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/literal${*}"}`),
	})
	alice := func(policies ...string) *domain.Identity { return user("alice", policies...) }
	get := func(id *domain.Identity, bucket, object string) policy.Args {
		return policy.Args{Identity: id, Action: policy.ActionGetObject, Bucket: bucket, Object: object}
	}
	act := func(id *domain.Identity, action policy.Action, bucket, object string) policy.Args {
		return policy.Args{Identity: id, Action: action, Bucket: bucket, Object: object}
	}
	runEvalCases(t, svc, []evalCase{
		{name: "action wildcard", args: get(alice("get-wild"), "b", "k"), allowed: true, source: policy.SourceIAM, policyName: "get-wild", sid: "GetAll"},
		{name: "action wildcard other Get", args: act(alice("get-wild"), policy.ActionGetObjectTagging, "b", "k"), allowed: true, source: policy.SourceIAM},
		{name: "action wildcard not Put", args: act(alice("get-wild"), policy.ActionPutObject, "b", "k")},
		{name: "action question mark", args: get(alice("question"), "b", "k"), allowed: true, source: policy.SourceIAM},
		{name: "action question mark is one char", args: act(alice("question"), "s3:GGetObject", "b", "k")},
		{name: "star action matches s3", args: act(alice("all"), policy.ActionDeleteBucket, "x", ""), allowed: true, source: policy.SourceIAM},
		{name: "star action matches admin", args: act(alice("all"), policy.ActionAdminHeal, "", ""), allowed: true, source: policy.SourceIAM},
		{name: "actions are case-insensitive", args: get(alice("upper"), "b", "k"), allowed: true, source: policy.SourceIAM},
		{name: "requested action case-insensitive", args: act(alice("get-wild"), "S3:getobject", "b", "k"), allowed: true, source: policy.SourceIAM},
		{name: "NotAction allows the others", args: get(alice("not-del"), "b", "k"), allowed: true, source: policy.SourceIAM},
		{name: "NotAction excludes", args: act(alice("not-del"), policy.ActionDeleteObject, "b", "k")},
		{name: "NotAction deny excludes the listed action", args: get(alice("deny-not-get"), "b", "k"), allowed: true, source: policy.SourceIAM},
		{name: "NotAction deny applies to the rest", args: act(alice("deny-not-get"), policy.ActionPutObject, "b", "k"), source: policy.SourceIAM, sid: "OnlyGet"},
		{name: "resource wildcard suffix", args: get(alice("jpg"), "b", "a/x.jpg"), allowed: true, source: policy.SourceIAM},
		{name: "resource wildcard suffix mismatch", args: get(alice("jpg"), "b", "a/x.png")},
		{name: "resource question mark", args: get(alice("one-char"), "b", "v1/x"), allowed: true, source: policy.SourceIAM},
		{name: "resource question mark is one char", args: get(alice("one-char"), "b", "v10/x")},
		{name: "resources are case-sensitive", args: get(alice("upper-res"), "b", "k")},
		{name: "NotResource allows elsewhere", args: get(alice("not-secret"), "b", "k"), allowed: true, source: policy.SourceIAM},
		{name: "NotResource excludes objects", args: get(alice("not-secret"), "secret", "k")},
		{name: "NotResource excludes the bucket ARN", args: act(alice("not-secret"), policy.ActionGetObject, "secret", "")},
		{name: "bucket ARN matches bucket actions", args: act(alice("bucket-only"), policy.ActionListBucket, "b", ""), allowed: true, source: policy.SourceIAM},
		{name: "bucket ARN does not match objects", args: get(alice("bucket-only"), "b", "k")},
		{name: "ListAllMyBuckets on arn:aws:s3:::*", args: act(alice("list-all"), policy.ActionListAllMyBuckets, "", ""), allowed: true, source: policy.SourceIAM},
		{name: "ListAllMyBuckets needs the wildcard resource", args: act(alice("list-b"), policy.ActionListAllMyBuckets, "", "")},
		{name: "home directory read", args: get(alice("home"), "home", "alice/doc.txt"), allowed: true, source: policy.SourceIAM},
		{name: "home directory of another user", args: get(alice("home"), "home", "bob/doc.txt")},
		{name: "home directory listing", args: policy.Args{Identity: alice("home"), Action: policy.ActionListBucket, Bucket: "home", Conditions: map[string][]string{"s3:prefix": {"alice/"}}}, allowed: true, source: policy.SourceIAM},
		{name: "home directory listing of another user", args: policy.Args{Identity: alice("home"), Action: policy.ActionListBucket, Bucket: "home", Conditions: map[string][]string{"s3:prefix": {"bob/"}}}},
		{name: "home directory of a service account is the parent's", args: get(&domain.Identity{AccessKey: "svc1", Parent: "alice", PolicyNames: []string{"home"}}, "home", "alice/x"), allowed: true, source: policy.SourceIAM},
		{name: "variable value is literal in resources", args: get(user("a*", "home"), "home", "abc/x")},
		{name: "variable value with star matches itself", args: get(user("a*", "home"), "home", "a*/x"), allowed: true, source: policy.SourceIAM},
		{name: "unresolved variable in NotResource excludes", args: get(alice("not-home"), "b", "k")},
		{name: "escaped star in resource is literal", args: get(alice("escaped"), "b", "literalX")},
		{name: "escaped star in resource matches star", args: get(alice("escaped"), "b", "literal*"), allowed: true, source: policy.SourceIAM},
		{name: "built-in readonly reads", args: get(alice(policy.PolicyReadOnly), "any", "k"), allowed: true, source: policy.SourceIAM, policyName: policy.PolicyReadOnly},
		{name: "built-in readonly lists", args: act(alice(policy.PolicyReadOnly), policy.ActionListBucket, "any", ""), allowed: true, source: policy.SourceIAM},
		{name: "built-in readonly lists buckets", args: act(alice(policy.PolicyReadOnly), policy.ActionListAllMyBuckets, "", ""), allowed: true, source: policy.SourceIAM},
		{name: "built-in readonly cannot write", args: act(alice(policy.PolicyReadOnly), policy.ActionPutObject, "any", "k")},
		{name: "built-in readonly cannot delete", args: act(alice(policy.PolicyReadOnly), policy.ActionDeleteObject, "any", "k")},
		{name: "built-in writeonly writes", args: act(alice(policy.PolicyWriteOnly), policy.ActionPutObject, "any", "k"), allowed: true, source: policy.SourceIAM},
		{name: "built-in writeonly aborts uploads", args: act(alice(policy.PolicyWriteOnly), policy.ActionAbortMultipart, "any", "k"), allowed: true, source: policy.SourceIAM},
		{name: "built-in writeonly cannot read", args: get(alice(policy.PolicyWriteOnly), "any", "k")},
		{name: "built-in writeonly cannot list", args: act(alice(policy.PolicyWriteOnly), policy.ActionListBucket, "any", "")},
		{name: "built-in readwrite deletes buckets", args: act(alice(policy.PolicyReadWrite), policy.ActionDeleteBucket, "any", ""), allowed: true, source: policy.SourceIAM},
		{name: "built-in readwrite has no admin", args: act(alice(policy.PolicyReadWrite), policy.ActionAdminServerInfo, "", "")},
		{name: "consoleAdmin admin actions", args: act(alice(policy.PolicyConsoleAdmin), policy.ActionAdminCreateUser, "", ""), allowed: true, source: policy.SourceIAM},
		{name: "consoleAdmin admin bucket actions", args: act(alice(policy.PolicyConsoleAdmin), policy.ActionAdminDeleteBucket, "b", ""), allowed: true, source: policy.SourceIAM},
		{name: "consoleAdmin s3 actions", args: act(alice(policy.PolicyConsoleAdmin), policy.ActionPutBucketPolicy, "b", ""), allowed: true, source: policy.SourceIAM},
		{name: "consoleAdmin kms actions", args: act(alice(policy.PolicyConsoleAdmin), "kms:CreateKey", "", ""), allowed: true, source: policy.SourceIAM},
		{name: "diagnostics server info", args: act(alice(policy.PolicyDiagnostics), policy.ActionAdminServerInfo, "", ""), allowed: true, source: policy.SourceIAM},
		{name: "diagnostics prometheus", args: act(alice(policy.PolicyDiagnostics), "admin:Prometheus", "", ""), allowed: true, source: policy.SourceIAM},
		{name: "diagnostics no heal", args: act(alice(policy.PolicyDiagnostics), policy.ActionAdminHeal, "", "")},
		{name: "diagnostics no s3", args: get(alice(policy.PolicyDiagnostics), "b", "k")},
		{name: "unknown attached policy grants nothing", args: get(alice("ghost"), "b", "k")},
		{name: "no policy", args: get(alice(), "b", "k")},
	})
}

// TestEvaluateAdminActions covers the admin:* actions, evaluated with the
// same logic as S3 actions.
func TestEvaluateAdminActions(t *testing.T) {
	svc := engineWith(t, map[string]string{
		"users":     doc(`{"Effect":"Allow","Action":["admin:CreateUser","admin:ListUsers"]}`),
		"bucket-ad": doc(`{"Effect":"Allow","Action":"admin:*Bucket","Resource":"arn:aws:s3:::photos"}`),
		"no-heal":   doc(`{"Effect":"Allow","Action":"admin:*"}`, `{"Effect":"Deny","Action":"admin:Heal"}`),
		"from-lan":  doc(`{"Effect":"Allow","Action":"admin:ServerInfo","Condition":{"IpAddress":{"aws:SourceIp":"10.0.0.0/8"}}}`),
	})
	act := func(policyName string, action policy.Action, bucket string) policy.Args {
		return policy.Args{Identity: user("ops", policyName), Action: action, Bucket: bucket}
	}
	runEvalCases(t, svc, []evalCase{
		{name: "statement without resource", args: act("users", policy.ActionAdminCreateUser, ""), allowed: true, source: policy.SourceIAM},
		{name: "action not listed", args: act("users", policy.ActionAdminDeleteUser, "")},
		{name: "bucket-scoped admin action on the bucket", args: act("bucket-ad", policy.ActionAdminGetBucket, "photos"), allowed: true, source: policy.SourceIAM},
		{name: "bucket-scoped admin action on another bucket", args: act("bucket-ad", policy.ActionAdminGetBucket, "other")},
		{name: "bucket-scoped admin action without bucket", args: act("bucket-ad", policy.ActionAdminListBuckets, "")},
		{name: "admin wildcard", args: act("no-heal", policy.ActionAdminConfigWrite, ""), allowed: true, source: policy.SourceIAM},
		{name: "admin deny wins", args: act("no-heal", policy.ActionAdminHeal, ""), source: policy.SourceIAM},
		{name: "admin action condition holds", args: policy.Args{Identity: user("ops", "from-lan"), Action: policy.ActionAdminServerInfo, Conditions: map[string][]string{"aws:SourceIp": {"10.0.0.9"}}}, allowed: true, source: policy.SourceIAM},
		{name: "admin action condition fails", args: policy.Args{Identity: user("ops", "from-lan"), Action: policy.ActionAdminServerInfo, Conditions: map[string][]string{"aws:SourceIp": {"192.0.2.1"}}}},
		{name: "admin action for anonymous", args: policy.Args{Action: policy.ActionAdminServerInfo}},
	})
}

// TestEvaluateCombination covers deny precedence, groups, inline
// (session) policy intersection, root and anonymous access.
func TestEvaluateCombination(t *testing.T) {
	ctx := context.Background()
	svc, _ := newEngine(t)
	for name, d := range map[string]string{
		"rw-b":       doc(`{"Sid":"RW","Effect":"Allow","Action":"s3:*","Resource":["arn:aws:s3:::b","arn:aws:s3:::b/*"]}`),
		"deny-del":   doc(`{"Sid":"NoDelete","Effect":"Deny","Action":"s3:DeleteObject","Resource":"arn:aws:s3:::*"}`),
		"get-narrow": doc(`{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/narrow/*"}`),
	} {
		require.NoError(t, svc.SetPolicy(ctx, name, mustPolicy(t, d)))
	}
	require.NoError(t, svc.Attach(ctx, policy.AttachInput{PolicyNames: []string{"rw-b"}, User: "alice"}))
	require.NoError(t, svc.Attach(ctx, policy.AttachInput{PolicyNames: []string{"deny-del"}, Group: "careful"}))
	require.NoError(t, svc.Attach(ctx, policy.AttachInput{PolicyNames: []string{policy.PolicyReadOnly}, Group: "viewers"}))

	inline := mustPolicy(t, doc(`{"Sid":"Narrow","Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/narrow/*"}`))
	inlineWide := mustPolicy(t, doc(`{"Effect":"Allow","Action":"s3:*","Resource":"*"}`))
	inlineDeny := mustPolicy(t, doc(`{"Effect":"Allow","Action":"s3:*","Resource":"*"}`, `{"Sid":"NoPut","Effect":"Deny","Action":"s3:PutObject","Resource":"*"}`))
	bucketPolicy := mustPolicy(t, doc(
		`{"Sid":"PublicRead","Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::pub/*"}`,
		`{"Sid":"PrivateDeny","Effect":"Deny","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::pub/private/*"}`,
		`{"Sid":"BobUpload","Effect":"Allow","Principal":{"AWS":["arn:aws:iam::123456789012:user/bob","carol"]},"Action":"s3:PutObject","Resource":"arn:aws:s3:::pub/uploads/*"}`,
		`{"Sid":"NotMallory","Effect":"Allow","NotPrincipal":{"AWS":"mallory"},"Action":"s3:ListBucket","Resource":"arn:aws:s3:::pub"}`,
		`{"Sid":"Wild","Effect":"Allow","Principal":{"AWS":"arn:aws:iam::*:user/d*"},"Action":"s3:GetObjectTagging","Resource":"arn:aws:s3:::pub/*"}`,
		`{"Sid":"NoPrincipal","Effect":"Allow","Action":"s3:DeleteObject","Resource":"arn:aws:s3:::pub/*"}`,
		`{"Sid":"AnonOnly","Effect":"Allow","Principal":"*","Action":"s3:GetObjectAcl","Resource":"arn:aws:s3:::pub/*","Condition":{"StringEquals":{"aws:principaltype":"Anonymous"}}}`,
		`{"Sid":"NotAll","Effect":"Allow","NotPrincipal":{"AWS":"*"},"Action":"s3:GetObjectVersion","Resource":"arn:aws:s3:::pub/*"}`,
	))
	denyAll := mustPolicy(t, doc(`{"Effect":"Deny","Principal":"*","Action":"s3:*","Resource":["arn:aws:s3:::b","arn:aws:s3:::b/*"]}`))
	alice := &domain.Identity{AccessKey: "alice", Groups: []string{"careful"}}
	viewer := &domain.Identity{AccessKey: "vic", Groups: []string{"nonexistent", "viewers"}}
	svcAcct := func(p *domain.Policy) *domain.Identity {
		return &domain.Identity{AccessKey: "AKSVC", Parent: "alice", Groups: []string{"careful"}, SessionPolicy: p}
	}
	root := &domain.Identity{AccessKey: "admin", IsRoot: true}
	rootKey := func(p *domain.Policy) *domain.Identity {
		return &domain.Identity{AccessKey: "AKROOT", Parent: "admin", IsRoot: true, SessionPolicy: p}
	}
	on := func(id *domain.Identity, action policy.Action, bucket, object string, bp *domain.Policy) policy.Args {
		return policy.Args{Identity: id, Action: action, Bucket: bucket, Object: object, BucketPolicy: bp}
	}
	runEvalCases(t, svc, []evalCase{
		// Identity policies, groups and deny precedence.
		{name: "attached user policy", args: on(alice, policy.ActionPutObject, "b", "k", nil), allowed: true, source: policy.SourceIAM, policyName: "rw-b", sid: "RW"},
		{name: "group deny beats user allow", args: on(alice, policy.ActionDeleteObject, "b", "k", nil), source: policy.SourceGroup, policyName: "deny-del", sid: "NoDelete"},
		{name: "group policy grants", args: on(viewer, policy.ActionGetObject, "any", "k", nil), allowed: true, source: policy.SourceGroup, policyName: policy.PolicyReadOnly},
		{name: "group policy does not grant writes", args: on(viewer, policy.ActionPutObject, "any", "k", nil)},
		{name: "bucket deny beats identity allow", args: on(alice, policy.ActionGetObject, "b", "k", denyAll), source: policy.SourceBucket},
		{name: "identity deny beats bucket allow", args: on(&domain.Identity{AccessKey: "dave", Groups: []string{"careful"}}, policy.ActionDeleteObject, "pub", "x", mustPolicy(t, doc(`{"Effect":"Allow","Principal":"*","Action":"s3:*","Resource":"arn:aws:s3:::pub/*"}`))), source: policy.SourceGroup},
		// Inline (session) policies: intersection with the parent.
		{name: "inline narrows: inside", args: on(svcAcct(inline), policy.ActionGetObject, "b", "narrow/k", nil), allowed: true, source: policy.SourceIAM, policyName: "rw-b"},
		{name: "inline narrows: outside", args: on(svcAcct(inline), policy.ActionGetObject, "b", "wide/k", nil), source: policy.SourceInline},
		{name: "inline narrows: other action", args: on(svcAcct(inline), policy.ActionPutObject, "b", "narrow/k", nil), source: policy.SourceInline},
		{name: "inline cannot widen the parent", args: on(svcAcct(inlineWide), policy.ActionGetObject, "other", "k", nil)},
		{name: "inline wide keeps the parent", args: on(svcAcct(inlineWide), policy.ActionPutObject, "b", "k", nil), allowed: true, source: policy.SourceIAM},
		{name: "inline deny", args: on(svcAcct(inlineDeny), policy.ActionPutObject, "b", "k", nil), source: policy.SourceInline, sid: "NoPut"},
		{name: "parent group deny applies to the service account", args: on(svcAcct(inlineWide), policy.ActionDeleteObject, "b", "k", nil), source: policy.SourceGroup},
		{name: "service account without inline inherits", args: on(svcAcct(nil), policy.ActionPutObject, "b", "k", nil), allowed: true, source: policy.SourceIAM},
		{name: "bucket grant is intersected with the inline policy", args: on(&domain.Identity{AccessKey: "AKC", Parent: "carol", SessionPolicy: inline}, policy.ActionPutObject, "pub", "uploads/a", bucketPolicy), source: policy.SourceInline},
		{name: "bucket grant within the inline policy", args: on(&domain.Identity{AccessKey: "AKC", Parent: "carol", SessionPolicy: inlineWide}, policy.ActionPutObject, "pub", "uploads/a", bucketPolicy), allowed: true, source: policy.SourceBucket, sid: "BobUpload"},
		// Root.
		{name: "root is allowed", args: on(root, policy.ActionDeleteBucket, "b", "", nil), allowed: true, source: policy.SourceRoot},
		{name: "root bypasses bucket deny", args: on(root, policy.ActionGetObject, "b", "k", denyAll), allowed: true, source: policy.SourceRoot},
		{name: "root bypasses admin", args: on(root, policy.ActionAdminServiceStop, "", "", nil), allowed: true, source: policy.SourceRoot},
		{name: "root key limited by its inline policy", args: on(rootKey(inline), policy.ActionGetObject, "b", "narrow/k", nil), allowed: true, source: policy.SourceInline, sid: "Narrow"},
		{name: "root key outside its inline policy", args: on(rootKey(inline), policy.ActionGetObject, "b", "wide/k", denyAll), source: policy.SourceInline},
		{name: "root key inline deny", args: on(rootKey(inlineDeny), policy.ActionPutObject, "b", "k", nil), source: policy.SourceInline},
		// Anonymous and bucket policy principals.
		{name: "nil identity is anonymous", args: on(nil, policy.ActionGetObject, "pub", "a.png", bucketPolicy), allowed: true, source: policy.SourceBucket, sid: "PublicRead"},
		{name: "anonymous explicit deny", args: on(nil, policy.ActionGetObject, "pub", "private/a", bucketPolicy), source: policy.SourceBucket, sid: "PrivateDeny"},
		{name: "anonymous not a named principal", args: on(domain.Anonymous(), policy.ActionPutObject, "pub", "uploads/a", bucketPolicy)},
		{name: "anonymous matches NotPrincipal", args: on(domain.Anonymous(), policy.ActionListBucket, "pub", "", bucketPolicy), allowed: true, source: policy.SourceBucket, sid: "NotMallory"},
		{name: "anonymous principaltype condition", args: on(domain.Anonymous(), policy.Action("s3:GetObjectAcl"), "pub", "a", bucketPolicy), allowed: true, source: policy.SourceBucket, sid: "AnonOnly"},
		{name: "anonymous without bucket policy", args: on(nil, policy.ActionGetObject, "pub", "a.png", nil)},
		{name: "anonymous ignores identity policies", args: on(&domain.Identity{IsAnonymous: true, PolicyNames: []string{policy.PolicyReadWrite}}, policy.ActionGetObject, "b", "k", nil)},
		{name: "NotPrincipal everyone excludes all", args: on(domain.Anonymous(), policy.ActionGetObjectVersion, "pub", "a", bucketPolicy)},
		{name: "principal by account ARN", args: on(user("bob"), policy.ActionPutObject, "pub", "uploads/a", bucketPolicy), allowed: true, source: policy.SourceBucket, sid: "BobUpload"},
		{name: "principal by name", args: on(user("carol"), policy.ActionPutObject, "pub", "uploads/a", bucketPolicy), allowed: true, source: policy.SourceBucket},
		{name: "principal via the parent user", args: on(&domain.Identity{AccessKey: "AKB", Parent: "bob"}, policy.ActionPutObject, "pub", "uploads/a", bucketPolicy), allowed: true, source: policy.SourceBucket},
		{name: "principal by access key", args: on(&domain.Identity{AccessKey: "carol", Parent: "zed"}, policy.ActionPutObject, "pub", "uploads/a", bucketPolicy), allowed: true, source: policy.SourceBucket},
		{name: "principal wildcard ARN", args: on(user("dave"), policy.ActionGetObjectTagging, "pub", "a", bucketPolicy), allowed: true, source: policy.SourceBucket, sid: "Wild"},
		{name: "principal wildcard ARN mismatch", args: on(user("erin"), policy.ActionGetObjectTagging, "pub", "a", bucketPolicy)},
		{name: "other principal", args: on(user("erin"), policy.ActionPutObject, "pub", "uploads/a", bucketPolicy)},
		{name: "NotPrincipal excludes the named user", args: on(user("mallory"), policy.ActionListBucket, "pub", "", bucketPolicy)},
		{name: "NotPrincipal grants other users", args: on(user("erin"), policy.ActionListBucket, "pub", "", bucketPolicy), allowed: true, source: policy.SourceBucket},
		{name: "bucket statement without principal applies to nobody", args: on(user("erin"), policy.ActionDeleteObject, "pub", "x", bucketPolicy)},
		{name: "users get public statements", args: on(user("erin"), policy.ActionGetObject, "pub", "a", bucketPolicy), allowed: true, source: policy.SourceBucket},
		{name: "principaltype condition excludes users", args: on(user("erin"), policy.Action("s3:GetObjectAcl"), "pub", "a", bucketPolicy)},
		{name: "identity allow and bucket allow: identity reported", args: on(user("erin", policy.PolicyReadOnly), policy.ActionGetObject, "pub", "a", bucketPolicy), allowed: true, source: policy.SourceIAM},
	})
}

// TestEvaluateBucketPolicyProvider checks the lookup of the bucket policy
// when Args.BucketPolicy is nil.
func TestEvaluateBucketPolicyProvider(t *testing.T) {
	ctx := context.Background()
	public := []byte(doc(`{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::pub/*"}`))
	provider := policymock.NewBucketPolicyProvider(t)
	svc := policy.New(policy.Deps{Buckets: provider, Clock: clock.NewFake(testNow)})

	provider.EXPECT().GetBucketPolicy(mock.Anything, "pub").Return(public, nil).Once()
	d, err := svc.Evaluate(ctx, policy.Args{Action: policy.ActionGetObject, Bucket: "pub", Object: "a"})
	require.NoError(t, err)
	assert.True(t, d.Allowed)
	assert.Equal(t, policy.SourceBucket, d.Source)

	// The stored document goes through the strict bucket policy parser:
	// one that no longer parses (or names another bucket) fails closed,
	// and an empty document is no policy.
	for name, raw := range map[string][]byte{
		"not a policy":   []byte(`{"Statement":7}`),
		"other bucket":   []byte(doc(`{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::other/*"}`)),
		"unknown action": []byte(doc(`{"Effect":"Allow","Principal":"*","Action":"s3:Teleport","Resource":"arn:aws:s3:::pub/*"}`)),
	} {
		provider.EXPECT().GetBucketPolicy(mock.Anything, "pub").Return(raw, nil).Once()
		_, err = svc.Evaluate(ctx, policy.Args{Action: policy.ActionGetObject, Bucket: "pub", Object: "a"})
		assert.ErrorIs(t, err, domain.ErrInvalidFormat, name)
	}
	provider.EXPECT().GetBucketPolicy(mock.Anything, "pub").Return(nil, nil).Once()
	d, err = svc.Evaluate(ctx, policy.Args{Action: policy.ActionGetObject, Bucket: "pub", Object: "a"})
	require.NoError(t, err)
	assert.False(t, d.Allowed)

	// An explicit Args.BucketPolicy wins: the provider is not called.
	d, err = svc.Evaluate(ctx, policy.Args{Action: policy.ActionGetObject, Bucket: "pub", Object: "a", BucketPolicy: mustPolicy(t, doc(`{"Effect":"Deny","Principal":"*","Action":"s3:*","Resource":"arn:aws:s3:::pub/*"}`))})
	require.NoError(t, err)
	assert.False(t, d.Allowed)

	for _, missing := range []error{domain.ErrPolicyNotFound, domain.ErrBucketNotFound, domain.ErrInvalidBucketName} {
		provider.EXPECT().GetBucketPolicy(mock.Anything, "pub").Return(nil, missing).Once()
		d, err = svc.Evaluate(ctx, policy.Args{Action: policy.ActionGetObject, Bucket: "pub", Object: "a"})
		require.NoError(t, err)
		assert.False(t, d.Allowed)
	}
	provider.EXPECT().GetBucketPolicy(mock.Anything, "pub").Return(nil, domain.ErrReadQuorum).Once()
	_, err = svc.Evaluate(ctx, policy.Args{Action: policy.ActionGetObject, Bucket: "pub", Object: "a"})
	assert.ErrorIs(t, err, domain.ErrReadQuorum)

	// Root and bucket-less requests never consult the provider.
	d, err = svc.Evaluate(ctx, policy.Args{Identity: &domain.Identity{IsRoot: true}, Action: policy.ActionGetObject, Bucket: "pub", Object: "a"})
	require.NoError(t, err)
	assert.True(t, d.Allowed)
	_, err = svc.Evaluate(ctx, policy.Args{Identity: user("u"), Action: policy.ActionListAllMyBuckets})
	require.NoError(t, err)
}

func TestEvaluateErrors(t *testing.T) {
	ctx := context.Background()
	svc, _ := newEngine(t)
	_, err := svc.Evaluate(ctx, policy.Args{Identity: user("x")})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = svc.Simulate(ctx, policy.Args{Identity: user("x"), Action: policy.ActionGetObject, Object: "k"})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)

	store := policymock.NewPolicyStore(t)
	broken := policy.New(policy.Deps{Store: store})
	store.EXPECT().Attached(mock.Anything, policy.Entity{Kind: policy.EntityUser, Name: "u"}).Return(nil, domain.ErrFaultyDisk).Once()
	_, err = broken.Evaluate(ctx, policy.Args{Identity: user("u"), Action: policy.ActionGetObject, Bucket: "b", Object: "k"})
	assert.ErrorIs(t, err, domain.ErrFaultyDisk)

	// Specific expectations first: testify picks the first that matches.
	store.EXPECT().Attached(mock.Anything, policy.Entity{Kind: policy.EntityGroup, Name: "g"}).Return(nil, domain.ErrFaultyDisk).Once()
	store.EXPECT().Attached(mock.Anything, mock.Anything).Return(nil, nil)
	_, err = broken.Evaluate(ctx, policy.Args{Identity: &domain.Identity{AccessKey: "v", Groups: []string{"g"}}, Action: policy.ActionGetObject, Bucket: "b", Object: "k"})
	assert.ErrorIs(t, err, domain.ErrFaultyDisk)

	store.EXPECT().Get(mock.Anything, "custom").Return(policy.Named{}, domain.ErrReadQuorum).Once()
	_, err = broken.Evaluate(ctx, policy.Args{Identity: user("u", "custom"), Action: policy.ActionGetObject, Bucket: "b", Object: "k"})
	assert.ErrorIs(t, err, domain.ErrReadQuorum)
}

// TestEvaluateClock checks that the engine clock is the default time.
func TestEvaluateClock(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(testNow)
	svc := policy.New(policy.Deps{Clock: clk})
	require.NoError(t, svc.SetPolicy(ctx, "until-2027", mustPolicy(t, doc(`{"Effect":"Allow","Action":"s3:GetObject","Resource":"*","Condition":{"DateLessThan":{"aws:CurrentTime":"2027-01-01T00:00:00Z"}}}`))))
	args := policy.Args{Identity: user("u", "until-2027"), Action: policy.ActionGetObject, Bucket: "b", Object: "k"}
	d, err := svc.Evaluate(ctx, args)
	require.NoError(t, err)
	assert.True(t, d.Allowed)
	clk.Advance(365 * 24 * time.Hour)
	d, err = svc.Evaluate(ctx, args)
	require.NoError(t, err)
	assert.False(t, d.Allowed)
}

// TestSimulateLists checks the statements reported by the simulator.
func TestSimulateLists(t *testing.T) {
	ctx := context.Background()
	svc, _ := newEngine(t)
	require.NoError(t, svc.SetPolicy(ctx, "mixed", mustPolicy(t, doc(
		`{"Sid":"A","Effect":"Allow","Action":"s3:PutObject","Resource":"*"}`,
		`{"Sid":"B","Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::other/*"}`,
		`{"Sid":"C","Effect":"Allow","Action":"s3:GetObject","Resource":"*","Condition":{"Bool":{"aws:SecureTransport":"true"}}}`,
		`{"Sid":"D","Effect":"Deny","Action":"s3:GetObject","Resource":"*"}`,
		`{"Sid":"E","Effect":"Deny","Action":"s3:*","Resource":"*"}`,
	))))
	require.NoError(t, svc.Attach(ctx, policy.AttachInput{PolicyNames: []string{"mixed"}, Group: "g"}))
	bp := mustPolicy(t, doc(`{"Sid":"P","Effect":"Allow","Principal":{"AWS":"zed"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}`))
	args := policy.Args{Identity: &domain.Identity{AccessKey: "u", Groups: []string{"g"}}, Action: policy.ActionGetObject, Bucket: "b", Object: "k", BucketPolicy: bp, Now: testNow}

	d, err := svc.Simulate(ctx, args)
	require.NoError(t, err)
	assert.False(t, d.Allowed)
	assert.Equal(t, "D", d.Sid, "the first deny decides")
	assert.Equal(t, 3, d.Index)
	assert.Equal(t, policy.SourceGroup, d.Source)
	assert.Equal(t, "g", d.Group)
	require.Len(t, d.Evaluated, 6)
	want := []struct {
		sid      string
		mismatch policy.Mismatch
	}{{"A", policy.MismatchAction}, {"B", policy.MismatchResource}, {"C", policy.MismatchCondition}, {"D", ""}, {"E", ""}, {"P", policy.MismatchPrincipal}}
	for i, w := range want {
		e := d.Evaluated[i]
		assert.Equal(t, w.sid, e.Sid)
		assert.Equal(t, w.mismatch, e.Mismatch, w.sid)
		assert.Equal(t, w.mismatch == "", e.Matched)
		assert.NotNil(t, e.Statement)
	}
	assert.Equal(t, policy.SourceBucket, d.Evaluated[5].Source)
	assert.Equal(t, "mixed", d.Evaluated[0].PolicyName)

	// Evaluate stops at the first deny but takes the same decision.
	ev, err := svc.Evaluate(ctx, args)
	require.NoError(t, err)
	d.Evaluated = nil
	assert.Equal(t, d, ev)

	root, err := svc.Simulate(ctx, policy.Args{Identity: &domain.Identity{IsRoot: true}, Action: policy.ActionGetObject})
	require.NoError(t, err)
	assert.Equal(t, policy.SourceRoot, root.Source)
	assert.Empty(t, root.Evaluated)
}

// TestUnvalidatedDocuments checks that documents that skipped validation
// (decoded directly, as transports do for stored bucket policies) never
// widen access.
func TestUnvalidatedDocuments(t *testing.T) {
	ctx := context.Background()
	svc, _ := newEngine(t)
	bp := mustPolicy(t, doc(
		`{"Effect":"Allow","Principal":"*","Action":"s3:*","Resource":"arn:aws:s3:::b/*"}`,
		`{"Sid":"Weird","Effect":"Deny","Principal":"*","Action":"s3:DeleteObject","Resource":"arn:aws:s3:::b/*","Condition":{"StringWhatever":{"k":"v"}}}`,
		`{"Sid":"WeirdAllow","Effect":"Allow","Principal":"*","Action":"s3:PutBucketPolicy","Resource":"arn:aws:s3:::b","Condition":{"Bogus":{"k":"v"}}}`,
	))
	d, err := svc.Evaluate(ctx, policy.Args{Action: policy.ActionDeleteObject, Bucket: "b", Object: "k", BucketPolicy: bp})
	require.NoError(t, err)
	assert.False(t, d.Allowed, "a deny with an unknown operator applies")
	assert.Equal(t, "Weird", d.Sid)
	d, err = svc.Evaluate(ctx, policy.Args{Action: policy.ActionPutBucketPolicy, Bucket: "b", BucketPolicy: bp})
	require.NoError(t, err)
	assert.False(t, d.Allowed, "an allow with an unknown operator does not")
	d, err = svc.Evaluate(ctx, policy.Args{Action: policy.ActionGetObject, Bucket: "b", Object: "k", BucketPolicy: bp})
	require.NoError(t, err)
	assert.True(t, d.Allowed)

	odd := mustPolicy(t, doc(`{"Effect":"allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}`))
	d, err = svc.Evaluate(ctx, policy.Args{Action: policy.ActionGetObject, Bucket: "b", Object: "k", BucketPolicy: odd})
	require.NoError(t, err)
	assert.False(t, d.Allowed, "a malformed effect denies")
}
