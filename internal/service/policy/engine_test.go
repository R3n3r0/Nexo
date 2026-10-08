package policy_test

import (
	"context"
	"encoding/json"
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

func mustPolicy(t *testing.T, doc string) *domain.Policy {
	t.Helper()
	var p domain.Policy
	require.NoError(t, json.Unmarshal([]byte(doc), &p), doc)
	return &p
}

func newEngine(t *testing.T) (policy.Service, policy.PolicyStore, *clock.Fake) {
	t.Helper()
	store := policy.NewMemoryStore(nil)
	clk := clock.NewFake(time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC))
	return policy.New(policy.Deps{Store: store, Clock: clk}), store, clk
}

func user(name string, policies ...string) *domain.Identity {
	return &domain.Identity{AccessKey: name, PolicyNames: policies}
}

func TestEvaluateTable(t *testing.T) {
	ctx := context.Background()
	svc, _, clk := newEngine(t)
	require.NoError(t, svc.SetPolicy(ctx, "photos-owner", mustPolicy(t, `{
		"Version": "2012-10-17",
		"Statement": [
			{"Effect": "Allow", "Action": ["s3:GetObject", "s3:PutObject", "s3:ListBucket"], "Resource": ["arn:aws:s3:::photos", "arn:aws:s3:::photos/${aws:username}/*"]},
			{"Effect": "Deny", "Action": "s3:DeleteObject", "Resource": "arn:aws:s3:::photos/*"},
			{"Effect": "Allow", "Action": "s3:Get?bject", "NotResource": ["arn:aws:s3:::secret/*", "arn:aws:s3:::photos/*"]},
			{"Effect": "Allow", "Action": "s3:ListBucket", "Resource": "arn:aws:s3:::logs",
			 "Condition": {"StringLike": {"s3:prefix": ["${aws:username}/*", "shared/*"]}}},
			{"Effect": "Deny", "Action": "s3:*", "Resource": "arn:aws:s3:::*",
			 "Condition": {"NotIpAddress": {"aws:SourceIp": ["10.0.0.0/8", "192.168.1.1"]}}},
			{"Effect": "Allow", "Action": "s3:PutObject", "Resource": "arn:aws:s3:::secure/*",
			 "Condition": {"Bool": {"aws:SecureTransport": "true"}, "NumericLessThanEquals": {"s3:content-length": "1024"}}},
			{"Effect": "Allow", "Action": "s3:GetObjectTagging", "Resource": "arn:aws:s3:::tagged/*",
			 "Condition": {"Null": {"s3:ExistingObjectTag/env": "false"}, "ForAnyValue:StringEquals": {"aws:TagKeys": ["env", "team"]}}},
			{"Effect": "Allow", "Action": "s3:GetBucketLocation", "Resource": "arn:aws:s3:::dated",
			 "Condition": {"DateGreaterThan": {"aws:CurrentTime": "2026-01-01T00:00:00Z"}, "StringEqualsIfExists": {"aws:Referer": "https://ok.example.com"}}}
		]}`)))
	require.NoError(t, svc.Attach(ctx, policy.AttachInput{PolicyNames: []string{"readonly"}, Group: "viewers"}))
	require.NoError(t, svc.Attach(ctx, policy.AttachInput{PolicyNames: []string{"writeonly"}, User: "uploader"}))

	bucketPolicy := mustPolicy(t, `{"Version": "2012-10-17", "Statement": [
		{"Effect": "Allow", "Principal": "*", "Action": "s3:GetObject", "Resource": "arn:aws:s3:::public/*"},
		{"Effect": "Allow", "Principal": {"AWS": ["arn:aws:iam:::user/bob", "carol"]}, "Action": "s3:PutObject", "Resource": "arn:aws:s3:::public/uploads/*"},
		{"Effect": "Deny", "Principal": "*", "Action": "s3:GetObject", "Resource": "arn:aws:s3:::public/private/*"},
		{"Effect": "Allow", "NotPrincipal": {"AWS": "mallory"}, "Action": "s3:ListBucket", "Resource": "arn:aws:s3:::public"}
	]}`)
	sessionPolicy := mustPolicy(t, `{"Version": "2012-10-17", "Statement": [
		{"Effect": "Allow", "Action": "s3:GetObject", "Resource": "arn:aws:s3:::photos/alice/narrow/*"}
	]}`)
	fromLAN := map[string][]string{"aws:SourceIp": {"10.1.2.3"}}

	tests := []struct {
		name    string
		args    policy.Args
		allowed bool
		reason  string
	}{
		{"root is always allowed", policy.Args{Identity: &domain.Identity{IsRoot: true}, Action: policy.ActionDeleteBucket, Bucket: "any"}, true, "root"},
		{"nil identity is anonymous and denied", policy.Args{Action: policy.ActionGetObject, Bucket: "photos", Object: "a"}, false, "anonymous"},
		{"no policy denies", policy.Args{Identity: user("nobody"), Action: policy.ActionGetObject, Bucket: "photos", Object: "x", Conditions: fromLAN}, false, "no policy"},
		{"owner reads own prefix", policy.Args{Identity: user("alice", "photos-owner"), Action: policy.ActionGetObject, Bucket: "photos", Object: "alice/1.jpg", Conditions: fromLAN}, true, "identity"},
		{"owner writes own prefix", policy.Args{Identity: user("alice", "photos-owner"), Action: policy.ActionPutObject, Bucket: "photos", Object: "alice/1.jpg", Conditions: fromLAN}, true, "identity"},
		{"owner cannot write another prefix", policy.Args{Identity: user("alice", "photos-owner"), Action: policy.ActionPutObject, Bucket: "photos", Object: "bob/1.jpg", Conditions: fromLAN}, false, "no policy"},
		{"explicit deny wins over allow", policy.Args{Identity: user("alice", "photos-owner"), Action: policy.ActionDeleteObject, Bucket: "photos", Object: "alice/1.jpg", Conditions: fromLAN}, false, "explicit deny in identity"},
		{"action wildcard with NotResource", policy.Args{Identity: user("alice", "photos-owner"), Action: policy.ActionGetObject, Bucket: "other", Object: "k", Conditions: fromLAN}, true, "identity"},
		{"action wildcard does not match other actions", policy.Args{Identity: user("alice", "photos-owner"), Action: policy.ActionGetObjectTagging, Bucket: "other", Object: "k", Conditions: fromLAN}, false, "no policy"},
		{"NotResource excludes", policy.Args{Identity: user("alice", "photos-owner"), Action: policy.ActionGetObject, Bucket: "secret", Object: "k", Conditions: fromLAN}, false, "no policy"},
		{"actions match case-insensitively", policy.Args{Identity: user("alice", "photos-owner"), Action: "s3:getobject", Bucket: "photos", Object: "alice/x", Conditions: fromLAN}, true, "identity"},
		{"prefix condition with variable", policy.Args{Identity: user("alice", "photos-owner"), Action: policy.ActionListBucket, Bucket: "logs",
			Conditions: map[string][]string{"s3:prefix": {"alice/2026"}, "aws:SourceIp": {"10.1.2.3"}}}, true, "identity"},
		{"prefix condition fails", policy.Args{Identity: user("alice", "photos-owner"), Action: policy.ActionListBucket, Bucket: "logs",
			Conditions: map[string][]string{"s3:prefix": {"bob/2026"}, "aws:SourceIp": {"10.1.2.3"}}}, false, "no policy"},
		{"prefix condition without the key", policy.Args{Identity: user("alice", "photos-owner"), Action: policy.ActionListBucket, Bucket: "logs", Conditions: fromLAN}, false, "no policy"},
		{"deny outside the LAN", policy.Args{Identity: user("alice", "photos-owner"), Action: policy.ActionGetObject, Bucket: "photos", Object: "alice/1.jpg",
			Conditions: map[string][]string{"aws:SourceIp": {"8.8.8.8"}}}, false, "explicit deny in identity"},
		{"allowed from the listed host", policy.Args{Identity: user("alice", "photos-owner"), Action: policy.ActionGetObject, Bucket: "photos", Object: "alice/1.jpg",
			Conditions: map[string][]string{"aws:SourceIp": {"192.168.1.1:4321"}}}, true, "identity"},
		{"negated condition with missing key passes", policy.Args{Identity: user("alice", "photos-owner"), Action: policy.ActionDeleteObject, Bucket: "photos", Object: "alice/1.jpg"}, false, "explicit deny in identity"},
		{"bool and numeric conditions", policy.Args{Identity: user("alice", "photos-owner"), Action: policy.ActionPutObject, Bucket: "secure", Object: "k",
			Conditions: map[string][]string{"aws:SecureTransport": {"true"}, "s3:content-length": {"512"}, "aws:SourceIp": {"10.0.0.1"}}}, true, "identity"},
		{"numeric condition fails", policy.Args{Identity: user("alice", "photos-owner"), Action: policy.ActionPutObject, Bucket: "secure", Object: "k",
			Conditions: map[string][]string{"aws:SecureTransport": {"true"}, "s3:content-length": {"4096"}, "aws:SourceIp": {"10.0.0.1"}}}, false, "no policy"},
		{"bool condition fails", policy.Args{Identity: user("alice", "photos-owner"), Action: policy.ActionPutObject, Bucket: "secure", Object: "k",
			Conditions: map[string][]string{"aws:SecureTransport": {"false"}, "s3:content-length": {"1"}, "aws:SourceIp": {"10.0.0.1"}}}, false, "no policy"},
		{"null and ForAnyValue", policy.Args{Identity: user("alice", "photos-owner"), Action: policy.ActionGetObjectTagging, Bucket: "tagged", Object: "k",
			Conditions: map[string][]string{"s3:ExistingObjectTag/env": {"prod"}, "aws:TagKeys": {"owner", "team"}, "aws:SourceIp": {"10.0.0.1"}}}, true, "identity"},
		{"null condition fails when the tag is absent", policy.Args{Identity: user("alice", "photos-owner"), Action: policy.ActionGetObjectTagging, Bucket: "tagged", Object: "k",
			Conditions: map[string][]string{"aws:TagKeys": {"team"}, "aws:SourceIp": {"10.0.0.1"}}}, false, "no policy"},
		{"date and IfExists conditions", policy.Args{Identity: user("alice", "photos-owner"), Action: policy.ActionGetBucketLocation, Bucket: "dated", Conditions: fromLAN}, true, "identity"},
		{"IfExists with a wrong value fails", policy.Args{Identity: user("alice", "photos-owner"), Action: policy.ActionGetBucketLocation, Bucket: "dated",
			Conditions: map[string][]string{"aws:Referer": {"https://evil.example.com"}, "aws:SourceIp": {"10.0.0.1"}}}, false, "no policy"},
		{"date condition fails in the past", policy.Args{Identity: user("alice", "photos-owner"), Action: policy.ActionGetBucketLocation, Bucket: "dated", Conditions: fromLAN,
			Now: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)}, false, "no policy"},
		{"built-in readonly via group", policy.Args{Identity: &domain.Identity{AccessKey: "viewer", Groups: []string{"viewers"}}, Action: policy.ActionGetObject, Bucket: "any", Object: "k"}, true, "identity"},
		{"built-in readonly denies writes", policy.Args{Identity: &domain.Identity{AccessKey: "viewer", Groups: []string{"viewers"}}, Action: policy.ActionPutObject, Bucket: "any", Object: "k"}, false, "no policy"},
		{"attached user policy", policy.Args{Identity: user("uploader"), Action: policy.ActionPutObject, Bucket: "any", Object: "k"}, true, "identity"},
		{"service account inherits the parent's attachments", policy.Args{Identity: &domain.Identity{AccessKey: "svc-1", Parent: "uploader"}, Action: policy.ActionPutObject, Bucket: "any", Object: "k"}, true, "identity"},
		{"session policy narrows", policy.Args{Identity: &domain.Identity{AccessKey: "alice", PolicyNames: []string{"photos-owner"}, SessionPolicy: sessionPolicy},
			Action: policy.ActionGetObject, Bucket: "photos", Object: "alice/narrow/1", Conditions: fromLAN}, true, "identity"},
		{"session policy excludes the rest", policy.Args{Identity: &domain.Identity{AccessKey: "alice", PolicyNames: []string{"photos-owner"}, SessionPolicy: sessionPolicy},
			Action: policy.ActionGetObject, Bucket: "photos", Object: "alice/wide/1", Conditions: fromLAN}, false, "no policy"},
		{"session policy cannot widen", policy.Args{Identity: &domain.Identity{AccessKey: "nobody", SessionPolicy: sessionPolicy},
			Action: policy.ActionGetObject, Bucket: "photos", Object: "alice/narrow/1"}, false, "no policy"},
		{"admin action needs an admin policy", policy.Args{Identity: user("uploader"), Action: policy.ActionAdminServerInfo}, false, "no policy"},
		{"consoleAdmin grants admin actions", policy.Args{Identity: user("ops", "consoleAdmin"), Action: policy.ActionAdminCreateUser}, true, "identity"},
		{"consoleAdmin grants s3 actions", policy.Args{Identity: user("ops", "consoleAdmin"), Action: policy.ActionDeleteBucket, Bucket: "x"}, true, "identity"},
		{"diagnostics grants server info only", policy.Args{Identity: user("mon", "diagnostics"), Action: policy.ActionAdminServerInfo}, true, "identity"},
		{"diagnostics denies heal", policy.Args{Identity: user("mon", "diagnostics"), Action: policy.ActionAdminHeal}, false, "no policy"},
		{"unknown attached policy is ignored", policy.Args{Identity: user("x", "does-not-exist"), Action: policy.ActionGetObject, Bucket: "b", Object: "k"}, false, "no policy"},
		{"anonymous allowed by bucket policy", policy.Args{Action: policy.ActionGetObject, Bucket: "public", Object: "a.png", BucketPolicy: bucketPolicy}, true, "bucket policy"},
		{"anonymous denied by explicit bucket deny", policy.Args{Action: policy.ActionGetObject, Bucket: "public", Object: "private/a", BucketPolicy: bucketPolicy}, false, "explicit deny in bucket"},
		{"anonymous cannot write", policy.Args{Action: policy.ActionPutObject, Bucket: "public", Object: "uploads/a", BucketPolicy: bucketPolicy}, false, "anonymous"},
		{"named principal by ARN", policy.Args{Identity: user("bob"), Action: policy.ActionPutObject, Bucket: "public", Object: "uploads/a", BucketPolicy: bucketPolicy}, true, "bucket policy"},
		{"named principal by access key", policy.Args{Identity: user("carol"), Action: policy.ActionPutObject, Bucket: "public", Object: "uploads/a", BucketPolicy: bucketPolicy}, true, "bucket policy"},
		{"other principal is not granted", policy.Args{Identity: user("dave"), Action: policy.ActionPutObject, Bucket: "public", Object: "uploads/a", BucketPolicy: bucketPolicy}, false, "no policy"},
		{"NotPrincipal excludes", policy.Args{Identity: user("mallory"), Action: policy.ActionListBucket, Bucket: "public", BucketPolicy: bucketPolicy}, false, "no policy"},
		{"NotPrincipal grants others", policy.Args{Identity: user("dave"), Action: policy.ActionListBucket, Bucket: "public", BucketPolicy: bucketPolicy}, true, "bucket policy"},
		{"bucket deny beats identity allow", policy.Args{Identity: user("ops", "consoleAdmin"), Action: policy.ActionGetObject, Bucket: "public", Object: "private/a", BucketPolicy: bucketPolicy}, false, "explicit deny in bucket"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.args.Now.IsZero() {
				tc.args.Now = clk.Now()
			}
			d, err := svc.Evaluate(ctx, tc.args)
			require.NoError(t, err)
			assert.Equal(t, tc.allowed, d.Allowed, d.Reason)
			assert.Contains(t, d.Reason, tc.reason)
			if tc.allowed && tc.args.Identity != nil && !tc.args.Identity.IsRoot {
				assert.NotNil(t, d.MatchedStatement)
			}
		})
	}
	_, err := svc.Evaluate(ctx, policy.Args{Identity: user("x"), Action: ""})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

func TestNamedPoliciesAndAttachments(t *testing.T) {
	ctx := context.Background()
	svc, store, clk := newEngine(t)

	list, err := svc.ListPolicies(ctx)
	require.NoError(t, err)
	require.Len(t, list, 5, "the built-in policies")
	names := make([]string, 0, len(list))
	for _, n := range list {
		names = append(names, n.Name)
		assert.True(t, n.BuiltIn)
		assert.NotNil(t, n.Policy)
	}
	assert.Equal(t, []string{"consoleAdmin", "diagnostics", "readonly", "readwrite", "writeonly"}, names)

	ro, err := svc.GetPolicy(ctx, policy.PolicyReadOnly)
	require.NoError(t, err)
	assert.Len(t, ro.Statement, 1)
	_, err = svc.GetPolicy(ctx, "missing")
	assert.ErrorIs(t, err, domain.ErrPolicyNotFound)

	doc := mustPolicy(t, `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`)
	assert.ErrorIs(t, svc.SetPolicy(ctx, "readonly", doc), domain.ErrInvalidArgument, "built-ins are read-only")
	assert.ErrorIs(t, svc.SetPolicy(ctx, "bad name!", doc), domain.ErrInvalidArgument)
	assert.ErrorIs(t, svc.SetPolicy(ctx, "x", &domain.Policy{Version: "2008"}), domain.ErrInvalidArgument)
	require.NoError(t, svc.SetPolicy(ctx, "custom", doc))
	got, err := svc.GetPolicy(ctx, "custom")
	require.NoError(t, err)
	assert.Equal(t, doc, got)
	got.Statement[0].Effect = domain.Deny
	again, _ := svc.GetPolicy(ctx, "custom")
	assert.Equal(t, domain.Allow, again.Statement[0].Effect, "callers get copies")
	list, err = svc.ListPolicies(ctx)
	require.NoError(t, err)
	require.Len(t, list, 6)
	assert.Equal(t, "custom", list[5].Name)
	assert.False(t, list[5].BuiltIn)
	assert.Equal(t, clk.Now(), list[5].UpdatedAt)

	// Attachments.
	assert.ErrorIs(t, svc.Attach(ctx, policy.AttachInput{PolicyNames: []string{"custom"}}), domain.ErrInvalidArgument)
	assert.ErrorIs(t, svc.Attach(ctx, policy.AttachInput{PolicyNames: []string{"custom"}, User: "u", Group: "g"}), domain.ErrInvalidArgument)
	assert.ErrorIs(t, svc.Attach(ctx, policy.AttachInput{User: "u"}), domain.ErrInvalidArgument)
	assert.ErrorIs(t, svc.Attach(ctx, policy.AttachInput{PolicyNames: []string{"ghost"}, User: "u"}), domain.ErrPolicyNotFound)
	require.NoError(t, svc.Attach(ctx, policy.AttachInput{PolicyNames: []string{"custom", "readonly", "custom"}, User: "alice"}))
	require.NoError(t, svc.Attach(ctx, policy.AttachInput{PolicyNames: []string{"custom"}, Group: "team"}))
	snap, err := store.Load(ctx)
	require.NoError(t, err)
	assert.Equal(t, []string{"custom", "readonly"}, snap.Users["alice"])
	assert.Equal(t, []string{"custom"}, snap.Groups["team"])
	d, err := svc.Evaluate(ctx, policy.Args{Identity: user("alice"), Action: policy.ActionGetObject, Bucket: "b", Object: "k"})
	require.NoError(t, err)
	assert.True(t, d.Allowed)

	// A new engine over the same store sees the persisted state.
	svc2 := policy.New(policy.Deps{Store: store, Clock: clk})
	d, err = svc2.Evaluate(ctx, policy.Args{Identity: &domain.Identity{AccessKey: "bob", Groups: []string{"team"}}, Action: policy.ActionGetObject, Bucket: "b", Object: "k"})
	require.NoError(t, err)
	assert.True(t, d.Allowed)

	// Detach is idempotent; deleting a policy detaches it everywhere.
	require.NoError(t, svc.Detach(ctx, policy.AttachInput{PolicyNames: []string{"readonly", "never-attached"}, User: "alice"}))
	snap, _ = store.Load(ctx)
	assert.Equal(t, []string{"custom"}, snap.Users["alice"])
	assert.ErrorIs(t, svc.DeletePolicy(ctx, "readonly"), domain.ErrInvalidArgument)
	assert.ErrorIs(t, svc.DeletePolicy(ctx, "ghost"), domain.ErrPolicyNotFound)
	require.NoError(t, svc.DeletePolicy(ctx, "custom"))
	snap, _ = store.Load(ctx)
	assert.Empty(t, snap.Users)
	assert.Empty(t, snap.Groups)
	d, err = svc.Evaluate(ctx, policy.Args{Identity: user("alice"), Action: policy.ActionGetObject, Bucket: "b", Object: "k"})
	require.NoError(t, err)
	assert.False(t, d.Allowed)
}

func TestParseAndValidate(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newEngine(t)

	p, err := svc.Parse(ctx, []byte(`{"Version":"2012-10-17","Statement":{"Sid":"one","Effect":"Allow","Action":"s3:*","Resource":["arn:aws:s3:::b","arn:aws:s3:::b/*"],"Condition":{"IpAddress":{"aws:SourceIp":"10.0.0.0/8"}}}}`))
	require.NoError(t, err)
	require.Len(t, p.Statement, 1)
	assert.Equal(t, "one", p.Statement[0].SID)

	bad := map[string]string{
		"empty":                ``,
		"not json":             `{`,
		"unknown field":        `{"Version":"2012-10-17","Statement":[],"Foo":1}`,
		"wrong version":        `{"Version":"2008-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"*"}]}`,
		"no statements":        `{"Version":"2012-10-17","Statement":[]}`,
		"bad effect":           `{"Version":"2012-10-17","Statement":[{"Effect":"Maybe","Action":"s3:GetObject","Resource":"*"}]}`,
		"no action":            `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Resource":"*"}]}`,
		"action and notaction": `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","NotAction":"s3:PutObject","Resource":"*"}]}`,
		"bad action":           `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"ec2:Run","Resource":"*"}]}`,
		"malformed action":     `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"GetObject","Resource":"*"}]}`,
		"missing resource":     `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject"}]}`,
		"bad resource":         `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:iam::123:user/x"}]}`,
		"both resources":       `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"*","NotResource":"*"}]}`,
		"both principals":      `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","NotPrincipal":"*","Action":"s3:GetObject","Resource":"*"}]}`,
		"duplicate sid":        `{"Version":"2012-10-17","Statement":[{"Sid":"a","Effect":"Allow","Action":"s3:GetObject","Resource":"*"},{"Sid":"a","Effect":"Allow","Action":"s3:GetObject","Resource":"*"}]}`,
		"unknown operator":     `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"*","Condition":{"StringMatches":{"a":"b"}}}]}`,
		"empty condition":      `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"*","Condition":{"StringEquals":{}}}]}`,
		"bad principal":        `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"alice","Action":"s3:GetObject","Resource":"*"}]}`,
	}
	for name, doc := range bad {
		t.Run(name, func(t *testing.T) {
			_, err := svc.Parse(ctx, []byte(doc))
			assert.ErrorIs(t, err, domain.ErrInvalidArgument)
		})
	}
	// Admin statements do not need a resource; size limit applies.
	_, err = svc.Parse(ctx, []byte(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"admin:ServerInfo"}]}`))
	require.NoError(t, err)
	small := policy.New(policy.Deps{}, policy.WithMaxPolicySize(10))
	_, err = small.Parse(ctx, []byte(`{"Version":"2012-10-17","Statement":[]}`))
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	assert.ErrorIs(t, svc.Validate(ctx, nil), domain.ErrInvalidArgument)
	var s3err *domain.S3Error
	require.ErrorAs(t, svc.Validate(ctx, &domain.Policy{}), &s3err)
	assert.Equal(t, "MalformedPolicy", s3err.Code)
}

func TestStoreErrors(t *testing.T) {
	ctx := context.Background()
	store := policymock.NewPolicyStore(t)
	svc := policy.New(policy.Deps{Store: store})

	store.EXPECT().Load(mock.Anything).Return(nil, domain.ErrFaultyDisk).Once()
	_, err := svc.ListPolicies(ctx)
	assert.ErrorIs(t, err, domain.ErrFaultyDisk)

	store.EXPECT().Load(mock.Anything).Return(&policy.Snapshot{}, nil).Once()
	store.EXPECT().Save(mock.Anything, mock.Anything).Return(domain.ErrWriteQuorum).Once()
	doc := mustPolicy(t, `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"*"}]}`)
	err = svc.SetPolicy(ctx, "p", doc)
	assert.ErrorIs(t, err, domain.ErrWriteQuorum)
	_, err = svc.GetPolicy(ctx, "p")
	assert.ErrorIs(t, err, domain.ErrPolicyNotFound, "a failed save leaves the state unchanged")
}

func TestMemoryStoreNormalizes(t *testing.T) {
	ctx := context.Background()
	st := policy.NewMemoryStore(nil)
	snap, err := st.Load(ctx)
	require.NoError(t, err)
	assert.NotNil(t, snap.Policies)
	assert.NotNil(t, snap.Users)
	assert.NotNil(t, snap.Groups)
	snap.Users["a"] = []string{"x"}
	require.NoError(t, st.Save(ctx, snap))
	snap.Users["a"] = nil
	again, _ := st.Load(ctx)
	assert.Equal(t, []string{"x"}, again.Users["a"], "the store keeps its own copy")
	seeded := policy.NewMemoryStore(&policy.Snapshot{Groups: map[string][]string{"g": {"readonly"}}})
	s2, _ := seeded.Load(ctx)
	assert.Equal(t, []string{"readonly"}, s2.Groups["g"])
	assert.NotNil(t, s2.Policies)
}
