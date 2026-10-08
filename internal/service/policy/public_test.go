package policy_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/policy"
)

func TestIsPublic(t *testing.T) {
	svc, _ := newEngine(t)
	stmt := func(body string) string {
		return `{"Version":"2012-10-17","Statement":[` + body + `]}`
	}
	tests := []struct {
		name string
		doc  string
		want bool
	}{
		{"anonymous read", stmt(`{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}`), true},
		{"aws wildcard principal", stmt(`{"Effect":"Allow","Principal":{"AWS":["*"]},"Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}`), true},
		{"deny only", stmt(`{"Effect":"Deny","Principal":"*","Action":"s3:*","Resource":"arn:aws:s3:::b/*"}`), false},
		{"named principal", stmt(`{"Effect":"Allow","Principal":{"AWS":["arn:aws:iam:::user/bob"]},"Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}`), false},
		{"not principal", stmt(`{"Effect":"Allow","NotPrincipal":{"AWS":["arn:aws:iam:::user/bob"]},"Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}`), true},
		{"source ip range", stmt(`{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*","Condition":{"IpAddress":{"aws:SourceIp":["10.0.0.0/8","192.168.1.1"]}}}`), false},
		{"source ip everything", stmt(`{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*","Condition":{"IpAddress":{"aws:SourceIp":["10.0.0.0/8","0.0.0.0/0"]}}}`), true},
		{"source ip invalid", stmt(`{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*","Condition":{"IpAddress":{"aws:SourceIp":"nope"}}}`), true},
		{"not ip address", stmt(`{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*","Condition":{"NotIpAddress":{"aws:SourceIp":"10.0.0.0/8"}}}`), true},
		{"if exists", stmt(`{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*","Condition":{"StringEqualsIfExists":{"aws:SourceVpc":"vpc-1"}}}`), true},
		{"vpc equals", stmt(`{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*","Condition":{"StringEquals":{"aws:SourceVpc":"vpc-1"}}}`), false},
		{"account like fixed", stmt(`{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*","Condition":{"StringLike":{"aws:SourceAccount":"123456789012"}}}`), false},
		{"account like wildcard", stmt(`{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*","Condition":{"StringLike":{"aws:SourceAccount":"1234*"}}}`), true},
		{"unrelated condition", stmt(`{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*","Condition":{"Bool":{"aws:SecureTransport":"true"}}}`), true},
		{"numeric on source key", stmt(`{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*","Condition":{"NumericEquals":{"aws:SourceAccount":"1"}}}`), true},
		{"one public statement wins", stmt(`{"Effect":"Allow","Principal":{"AWS":["bob"]},"Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"},{"Effect":"Allow","Principal":"*","Action":"s3:ListBucket","Resource":"arn:aws:s3:::b"}`), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, svc.IsPublic(context.Background(), mustPolicy(t, tt.doc)))
		})
	}
	assert.False(t, svc.IsPublic(context.Background(), nil))
}

func TestGeneratePublicPolicy(t *testing.T) {
	ctx := context.Background()
	svc, _ := newEngine(t)
	anon := func(p *domain.Policy, action policy.Action, object string, conds map[string][]string) bool {
		d, err := svc.Evaluate(ctx, policy.Args{Action: action, Bucket: "bkt", Object: object, Conditions: conds, BucketPolicy: p, Now: testNow})
		require.NoError(t, err)
		return d.Allowed
	}
	list := func(prefix string) map[string][]string {
		if prefix == "" {
			return nil
		}
		return map[string][]string{"s3:prefix": {prefix}}
	}
	tests := []struct {
		preset                    policy.Preset
		prefix                    string
		read, list, write, delete bool
	}{
		{policy.PresetDownload, "", true, true, false, false},
		{policy.PresetDownload, "pub/", true, true, false, false},
		{policy.PresetUpload, "", false, false, true, false},
		{policy.PresetUpload, "inbox/", false, false, true, false},
		{policy.PresetPublic, "", true, true, true, true},
		{policy.PresetPublic, "shared/", true, true, true, true},
	}
	for _, tc := range tests {
		t.Run(string(tc.preset)+"/"+tc.prefix, func(t *testing.T) {
			p, err := svc.GeneratePublicPolicy(ctx, "bkt", tc.prefix, tc.preset)
			require.NoError(t, err)
			require.NotNil(t, p)
			// The generated document is a valid bucket policy.
			data, err := json.Marshal(p)
			require.NoError(t, err)
			_, err = svc.ParseBucketPolicy(ctx, "bkt", data)
			require.NoError(t, err)
			assert.True(t, svc.IsPublic(ctx, p))

			key := tc.prefix + "dir/file.txt"
			assert.Equal(t, tc.read, anon(p, policy.ActionGetObject, key, nil), "read")
			assert.Equal(t, tc.list, anon(p, policy.ActionListBucket, "", list(tc.prefix)), "list")
			assert.Equal(t, tc.write, anon(p, policy.ActionPutObject, key, nil), "write")
			assert.Equal(t, tc.write, anon(p, policy.ActionAbortMultipart, key, nil), "abort")
			assert.Equal(t, tc.delete, anon(p, policy.ActionDeleteObject, key, nil), "delete")
			assert.True(t, anon(p, policy.ActionGetBucketLocation, "", nil))
			assert.False(t, anon(p, policy.ActionDeleteBucket, "", nil))
			assert.False(t, anon(p, policy.ActionPutBucketPolicy, "", nil))
			if tc.prefix != "" {
				assert.False(t, anon(p, policy.ActionGetObject, "other/file.txt", nil), "outside the prefix")
				assert.False(t, anon(p, policy.ActionPutObject, "other/file.txt", nil), "outside the prefix")
				assert.False(t, anon(p, policy.ActionListBucket, "", nil), "listing the root")
				assert.False(t, anon(p, policy.ActionListBucket, "", list("other/")), "listing another prefix")
			}
			assert.Equal(t, tc.preset, svc.DetectPreset(ctx, p, "bkt", tc.prefix))
			assert.Equal(t, []policy.PublicAccessRule{{Prefix: tc.prefix, Preset: tc.preset}}, svc.ListPublicAccess(ctx, p, "bkt"))
		})
	}

	p, err := svc.GeneratePublicPolicy(ctx, "bkt", "x/", policy.PresetPrivate)
	require.NoError(t, err)
	assert.Nil(t, p)
	for _, bad := range []struct {
		bucket, prefix string
		preset         policy.Preset
	}{
		{"", "", policy.PresetPublic},
		{"a/b", "", policy.PresetPublic},
		{"bkt", "a*", policy.PresetPublic},
		{"bkt", "a?", policy.PresetPublic},
		{"bkt", "${aws:username}/", policy.PresetPublic},
		{"bkt", "/abs", policy.PresetPublic},
		{"bkt", "", policy.PresetCustom},
		{"bkt", "", "everything"},
	} {
		_, err := svc.GeneratePublicPolicy(ctx, bad.bucket, bad.prefix, bad.preset)
		assert.ErrorIs(t, err, domain.ErrInvalidArgument, "%+v", bad)
	}
}

func TestApplyPublicPreset(t *testing.T) {
	ctx := context.Background()
	svc, _ := newEngine(t)
	custom := mustPolicy(t, doc(`{"Sid":"Partner","Effect":"Allow","Principal":{"AWS":"partner"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::bkt/*"}`))
	custom.ID = "my-policy"

	p, err := svc.ApplyPublicPreset(ctx, custom, "bkt", "pub/", policy.PresetDownload)
	require.NoError(t, err)
	assert.Equal(t, "my-policy", p.ID)
	assert.Equal(t, "Partner", p.Statement[0].SID, "custom statements are kept")
	assert.Len(t, custom.Statement, 1, "the input is not modified")
	p, err = svc.ApplyPublicPreset(ctx, p, "bkt", "inbox/", policy.PresetUpload)
	require.NoError(t, err)
	assert.Equal(t, []policy.PublicAccessRule{{Prefix: "inbox/", Preset: policy.PresetUpload}, {Prefix: "pub/", Preset: policy.PresetDownload}}, svc.ListPublicAccess(ctx, p, "bkt"))
	assert.Equal(t, policy.PresetPrivate, svc.DetectPreset(ctx, p, "bkt", "other/"))
	assert.Equal(t, policy.PresetPrivate, svc.DetectPreset(ctx, p, "bkt", ""), "the root is not public")

	// Re-applying replaces the rules of the prefix.
	p, err = svc.ApplyPublicPreset(ctx, p, "bkt", "pub/", policy.PresetPublic)
	require.NoError(t, err)
	assert.Equal(t, policy.PresetPublic, svc.DetectPreset(ctx, p, "bkt", "pub/"))
	assert.Equal(t, policy.PresetUpload, svc.DetectPreset(ctx, p, "bkt", "inbox/"))
	data, err := json.Marshal(p)
	require.NoError(t, err)
	_, err = svc.ParseBucketPolicy(ctx, "bkt", data)
	require.NoError(t, err, "Sids stay unique")

	// Back to private, prefix by prefix: the custom statement remains.
	p, err = svc.ApplyPublicPreset(ctx, p, "bkt", "pub/", policy.PresetPrivate)
	require.NoError(t, err)
	assert.Equal(t, policy.PresetPrivate, svc.DetectPreset(ctx, p, "bkt", "pub/"))
	p, err = svc.ApplyPublicPreset(ctx, p, "bkt", "inbox/", policy.PresetPrivate)
	require.NoError(t, err)
	require.Len(t, p.Statement, 1)
	assert.Equal(t, "Partner", p.Statement[0].SID)
	assert.Empty(t, svc.ListPublicAccess(ctx, p, "bkt"))

	// Nothing left: the bucket policy should be deleted.
	only, err := svc.GeneratePublicPolicy(ctx, "bkt", "", policy.PresetDownload)
	require.NoError(t, err)
	none, err := svc.ApplyPublicPreset(ctx, only, "bkt", "", policy.PresetPrivate)
	require.NoError(t, err)
	assert.Nil(t, none)
	_, err = svc.ApplyPublicPreset(ctx, only, "bkt", "", "bogus")
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

func TestDetectPreset(t *testing.T) {
	ctx := context.Background()
	svc, _ := newEngine(t)
	tests := []struct {
		name   string
		doc    string
		prefix string
		want   policy.Preset
	}{
		{"MinIO style download", doc(
			`{"Effect":"Allow","Principal":{"AWS":["*"]},"Action":["s3:GetBucketLocation","s3:ListBucket"],"Resource":["arn:aws:s3:::bkt"]}`,
			`{"Effect":"Allow","Principal":{"AWS":["*"]},"Action":["s3:GetObject"],"Resource":["arn:aws:s3:::bkt/*"]}`), "", policy.PresetDownload},
		{"bucket-wide download covers prefixes", doc(
			`{"Effect":"Allow","Principal":"*","Action":"s3:ListBucket","Resource":"arn:aws:s3:::bkt"}`,
			`{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::bkt/*"}`), "sub/", policy.PresetDownload},
		{"MinIO style readwrite", doc(
			`{"Effect":"Allow","Principal":"*","Action":["s3:ListBucket","s3:ListBucketMultipartUploads"],"Resource":"arn:aws:s3:::bkt"}`,
			`{"Effect":"Allow","Principal":"*","Action":["s3:GetObject","s3:PutObject","s3:DeleteObject"],"Resource":"arn:aws:s3:::bkt/*"}`), "", policy.PresetPublic},
		{"read without list is custom", doc(`{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::bkt/*"}`), "", policy.PresetCustom},
		{"single object below the prefix", doc(`{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::bkt/pub/a.png"}`), "pub/", policy.PresetPrivate},
		{"single object itself is custom", doc(`{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::bkt/pub/a.png"}`), "pub/a.png", policy.PresetCustom},
		{"pattern is custom", doc(`{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::bkt/*.png"}`), "", policy.PresetCustom},
		{"conditional upload is custom", doc(`{"Effect":"Allow","Principal":"*","Action":"s3:PutObject","Resource":"arn:aws:s3:::bkt/*","Condition":{"IpAddress":{"aws:SourceIp":"10.0.0.0/8"}}}`), "", policy.PresetCustom},
		{"upload with delete is custom", doc(`{"Effect":"Allow","Principal":"*","Action":["s3:PutObject","s3:DeleteObject"],"Resource":"arn:aws:s3:::bkt/*"}`), "", policy.PresetCustom},
		{"deny on the prefix", doc(
			`{"Effect":"Allow","Principal":"*","Action":["s3:GetObject","s3:ListBucket"],"Resource":["arn:aws:s3:::bkt","arn:aws:s3:::bkt/*"]}`,
			`{"Effect":"Deny","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::bkt/secret/*"}`), "secret/", policy.PresetCustom},
		{"named principal is private", doc(`{"Effect":"Allow","Principal":{"AWS":"bob"},"Action":"s3:*","Resource":"arn:aws:s3:::bkt/*"}`), "", policy.PresetPrivate},
		{"other bucket is private", doc(`{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::other/*"}`), "", policy.PresetPrivate},
		{"bucket-level grant only is private", doc(`{"Effect":"Allow","Principal":"*","Action":"s3:GetBucketLocation","Resource":"arn:aws:s3:::bkt"}`), "x/", policy.PresetPrivate},
		{"NotResource is custom", doc(`{"Effect":"Allow","Principal":"*","Action":"s3:GetObjectTagging","NotResource":"arn:aws:s3:::bkt/private/*"}`), "", policy.PresetCustom},
		{"star resource is custom", doc(`{"Effect":"Allow","Principal":"*","Action":"s3:GetObjectTagging","Resource":"*"}`), "", policy.PresetCustom},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, svc.DetectPreset(ctx, mustPolicy(t, tc.doc), "bkt", tc.prefix))
		})
	}
	assert.Equal(t, policy.PresetPrivate, svc.DetectPreset(ctx, nil, "bkt", ""))
	assert.Equal(t, policy.PresetPrivate, svc.DetectPreset(ctx, mustPolicy(t, tests[0].doc), "bkt", "bad*"))
	assert.Nil(t, svc.ListPublicAccess(ctx, nil, "bkt"))
	assert.Nil(t, svc.ListPublicAccess(ctx, mustPolicy(t, tests[0].doc), ""))
	assert.Equal(t, []policy.PublicAccessRule{{Prefix: "", Preset: policy.PresetCustom}}, svc.ListPublicAccess(ctx, mustPolicy(t, tests[6].doc), "bkt"))
	assert.Equal(t, []policy.PublicAccessRule{{Prefix: "pub/a.png", Preset: policy.PresetCustom}}, svc.ListPublicAccess(ctx, mustPolicy(t, tests[4].doc), "bkt"))
}
