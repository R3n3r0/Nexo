package auth_test

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/auth"
)

// postFixture signs POST policies with the AWS example key at testNow.
type postFixture struct {
	date       string
	credential string
	key        []byte
}

func newPostFixture() postFixture {
	return postFixture{
		date:       testNow.Format("20060102T150405Z"),
		credential: awsAccessKey + "/" + testNow.Format("20060102") + "/us-east-1/s3/aws4_request",
		key:        testSigningKey(awsSecretKey, testNow.Format("20060102"), "us-east-1"),
	}
}

// sign returns the base64 policy and its signature.
func (f postFixture) sign(policyJSON string) (string, string) {
	b64 := base64.StdEncoding.EncodeToString([]byte(policyJSON))
	return b64, hex.EncodeToString(hmacSHA256(f.key, []byte(b64)))
}

// policy renders a policy document expiring in an hour with the standard
// credential conditions plus extra ones.
func (f postFixture) policy(extra ...string) string {
	conds := append([]string{
		`{"x-amz-credential": "` + f.credential + `"}`,
		`{"x-amz-algorithm": "AWS4-HMAC-SHA256"}`,
		`["eq", "$x-amz-date", "` + f.date + `"]`,
	}, extra...)
	return fmt.Sprintf(`{"expiration": "%s", "conditions": [%s]}`, testNow.Add(time.Hour).Format("2006-01-02T15:04:05.000Z"), strings.Join(conds, ",\n"))
}

// form returns a form for the policy with the standard fields.
func (f postFixture) form(policyJSON string, fields map[string]string) auth.PostForm {
	b64, sig := f.sign(policyJSON)
	all := map[string]string{
		"X-Amz-Credential": f.credential, "x-amz-algorithm": "AWS4-HMAC-SHA256", "x-amz-date": f.date,
		"Policy": b64, "x-amz-signature": sig,
	}
	for k, v := range fields {
		all[k] = v
	}
	return auth.PostForm{Bucket: "uploads", Fields: all, FileName: "photo.png"}
}

func TestVerifyPostPolicy(t *testing.T) {
	ctx := context.Background()
	f := newPostFixture()
	policyJSON := f.policy(
		`{"bucket": "uploads"}`,
		`["starts-with", "$key", "user/alice/"]`,
		`{"acl": "private"}`,
		`["starts-with", "$Content-Type", "image/"]`,
		`["content-length-range", 1, 1048576]`,
		`["content-length-range", "10", 2097152]`,
		`["starts-with", "$x-amz-meta-tag", ""]`,
		`{"success_action_status": "201"}`,
		`["starts-with", "$success_action_redirect", "https://example.com/"]`,
	)
	fields := map[string]string{
		"key": "user/alice/${filename}", "acl": "private", "Content-Type": "image/png",
		"x-amz-meta-tag": "anything", "success_action_status": "201",
		"success_action_redirect": "https://example.com/done", "x-ignore-debug": "1", "bucket": "uploads",
	}
	form := func(mutate func(map[string]string)) auth.PostForm {
		cp := map[string]string{}
		for k, v := range fields {
			cp[k] = v
		}
		if mutate != nil {
			mutate(cp)
		}
		return f.form(policyJSON, cp)
	}

	// The form was signed hours ago: there is no clock skew check, only the
	// policy expiration.
	v := newVerifier(t, clock.NewFake(testNow.Add(50*time.Minute)))
	id, policy, err := v.VerifyPostPolicy(ctx, form(nil))
	require.NoError(t, err)
	assert.Equal(t, awsAccessKey, id.AccessKey)
	assert.Equal(t, domain.AuthTypePost, id.AuthType)
	assert.Equal(t, 50*time.Minute, id.SignatureAge)
	require.NotNil(t, policy)
	assert.Len(t, policy.Conditions, 12)
	assert.Equal(t, testNow.Add(time.Hour), policy.Expiration)
	lo, hi, ok := policy.ContentLengthRange()
	assert.True(t, ok)
	assert.Equal(t, int64(10), lo, "the ranges intersect")
	assert.Equal(t, int64(1048576), hi)
	lo2, hi2, ok2 := policy.LengthRange()
	assert.Equal(t, []any{lo, hi, ok}, []any{lo2, hi2, ok2})

	tests := []struct {
		name   string
		mutate func(map[string]string)
	}{
		{"key outside the prefix", func(m map[string]string) { m["key"] = "other/${filename}" }},
		{"acl differs", func(m map[string]string) { m["acl"] = "public-read" }},
		{"acl missing", func(m map[string]string) { delete(m, "acl") }},
		{"content type", func(m map[string]string) { m["Content-Type"] = "text/html" }},
		{"content type list", func(m map[string]string) { m["Content-Type"] = "image/png, text/html" }},
		{"extra field", func(m map[string]string) { m["x-amz-meta-extra"] = "nope" }},
		{"redirect elsewhere", func(m map[string]string) { m["success_action_redirect"] = "https://evil.example/" }},
		{"status differs", func(m map[string]string) { m["success_action_status"] = "200" }},
		{"bucket field differs", func(m map[string]string) { m["bucket"] = "another" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := v.VerifyPostPolicy(ctx, form(tt.mutate))
			assertS3Error(t, err, domain.ErrAccessDenied, "AccessDenied")
		})
	}
	t.Run("content type list within the prefix", func(t *testing.T) {
		_, _, err := v.VerifyPostPolicy(ctx, form(func(m map[string]string) { m["Content-Type"] = "image/png, image/jpeg" }))
		require.NoError(t, err)
	})
	t.Run("starts-with empty matches a missing field", func(t *testing.T) {
		_, _, err := v.VerifyPostPolicy(ctx, form(func(m map[string]string) { delete(m, "x-amz-meta-tag") }))
		require.NoError(t, err)
	})
	t.Run("wrong bucket", func(t *testing.T) {
		other := form(func(m map[string]string) { delete(m, "bucket") })
		other.Bucket = "another"
		_, _, err := v.VerifyPostPolicy(ctx, other)
		assertS3Error(t, err, domain.ErrAccessDenied, "AccessDenied")
	})
	t.Run("expired", func(t *testing.T) {
		late := newVerifier(t, clock.NewFake(testNow.Add(time.Hour)))
		_, _, err := late.VerifyPostPolicy(ctx, form(nil))
		assertS3Error(t, err, domain.ErrRequestExpired, "AccessDenied")
	})
}

func TestVerifyPostPolicyAuthErrors(t *testing.T) {
	ctx := context.Background()
	f := newPostFixture()
	v := newVerifier(t, clock.NewFake(testNow), auth.WithRegion("us-east-1"))
	policyJSON := f.policy(`{"bucket": "uploads"}`, `{"key": "k"}`)
	base := func(mutate func(map[string]string)) auth.PostForm {
		form := f.form(policyJSON, map[string]string{"key": "k"})
		if mutate != nil {
			mutate(form.Fields)
		}
		return form
	}
	zeros := strings.Repeat("0", 64)
	tests := []struct {
		name     string
		form     auth.PostForm
		sentinel error
		code     string
	}{
		{"wrong signature", base(func(m map[string]string) { m["x-amz-signature"] = zeros }), domain.ErrSignatureMismatch, "SignatureDoesNotMatch"},
		{"policy swapped", base(func(m map[string]string) {
			m["Policy"] = base64.StdEncoding.EncodeToString([]byte(f.policy(`{"bucket": "uploads"}`, `["starts-with", "$key", ""]`)))
		}), domain.ErrSignatureMismatch, "SignatureDoesNotMatch"},
		{"missing policy", base(func(m map[string]string) { delete(m, "Policy") }), domain.ErrInvalidArgument, "InvalidArgument"},
		{"wrong algorithm", base(func(m map[string]string) { m["x-amz-algorithm"] = "HMAC" }), domain.ErrInvalidArgument, "InvalidArgument"},
		{"bad credential", base(func(m map[string]string) { m["X-Amz-Credential"] = "bad" }), domain.ErrInvalidArgument, "InvalidArgument"},
		{"credential date mismatch", base(func(m map[string]string) {
			m["X-Amz-Credential"] = awsAccessKey + "/20261009/us-east-1/s3/aws4_request"
		}), domain.ErrInvalidArgument, "InvalidArgument"},
		{"wrong region", base(func(m map[string]string) {
			m["X-Amz-Credential"] = awsAccessKey + "/20261008/eu-west-1/s3/aws4_request"
		}), domain.ErrInvalidArgument, "InvalidArgument"},
		{"bad date", base(func(m map[string]string) { m["x-amz-date"] = "today" }), domain.ErrInvalidArgument, "InvalidArgument"},
		{"bad signature format", base(func(m map[string]string) { m["x-amz-signature"] = "zz" }), domain.ErrInvalidArgument, "InvalidArgument"},
		{"unknown key", base(func(m map[string]string) {
			m["X-Amz-Credential"] = "NOBODY/20261008/us-east-1/s3/aws4_request"
		}), domain.ErrInvalidAccessKey, "InvalidAccessKeyId"},
		{"signature version 2", auth.PostForm{Bucket: "uploads", Fields: map[string]string{
			"AWSAccessKeyId": awsAccessKey, "policy": "e30=", "signature": "abc", "key": "k",
		}}, domain.ErrInvalidArgument, "InvalidRequest"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := v.VerifyPostPolicy(ctx, tt.form)
			assertS3Error(t, err, tt.sentinel, tt.code)
		})
	}
	t.Run("not base64", func(t *testing.T) {
		form := base(nil)
		form.Fields["Policy"] = "***"
		form.Fields["x-amz-signature"] = hex.EncodeToString(hmacSHA256(f.key, []byte("***")))
		_, _, err := v.VerifyPostPolicy(ctx, form)
		assertS3Error(t, err, domain.ErrInvalidArgument, "InvalidPolicyDocument")
	})
}

func TestPostPolicyDocuments(t *testing.T) {
	ctx := context.Background()
	f := newPostFixture()
	v := newVerifier(t, clock.NewFake(testNow))
	exp := testNow.Add(time.Hour).Format(time.RFC3339)
	doc := func(conds string) string {
		return fmt.Sprintf(`{"expiration": "%s", "conditions": [%s]}`, exp, conds)
	}
	many := strings.TrimSuffix(strings.Repeat(`{"acl": "private"},`, 300), ",")
	for name, policyJSON := range map[string]string{
		"not json":              `not json`,
		"no expiration":         `{"conditions": []}`,
		"bad expiration":        `{"expiration": "soon", "conditions": []}`,
		"field without $":       doc(`["eq", "key", "v"]`),
		"field only $":          doc(`["eq", "$", "v"]`),
		"unknown operator":      doc(`["unknown", "$key", "v"]`),
		"operator not a string": doc(`[1, "$key", "v"]`),
		"value not a string":    doc(`["eq", "$key", 1]`),
		"two elements":          doc(`["eq", "$key"]`),
		"inverted range":        doc(`["content-length-range", 10, 1]`),
		"negative range":        doc(`["content-length-range", -1, 1]`),
		"non-numeric range":     doc(`["content-length-range", "a", 1]`),
		"fractional range":      doc(`["content-length-range", 1.5, 2]`),
		"object range bound":    doc(`["content-length-range", {}, 2]`),
		"object with two keys":  doc(`{"a": "1", "b": "2"}`),
		"object non-string":     doc(`{"a": 1}`),
		"starts-with on bucket": doc(`["starts-with", "$bucket", "up"]`),
		"starts-with on date":   doc(`["starts-with", "$x-amz-date", ""]`),
		"too many conditions":   doc(many),
		"conditions not a list": fmt.Sprintf(`{"expiration": "%s", "conditions": {}}`, exp),
	} {
		t.Run(name, func(t *testing.T) {
			form := f.form(policyJSON, nil)
			_, _, err := v.VerifyPostPolicy(ctx, form)
			assertS3Error(t, err, domain.ErrInvalidArgument, "InvalidPolicyDocument")
		})
	}
}

func TestPostPolicyCheck(t *testing.T) {
	policy := &auth.PostPolicy{Conditions: []auth.PostCondition{
		{Operator: auth.ConditionEq, Key: "acl", Value: "private"},
		{Operator: auth.ConditionStartsWith, Key: "content-type", Value: "image/"},
		{Operator: auth.ConditionStartsWith, Key: "key", Value: "uploads/"},
		{Operator: auth.ConditionLengthRange, Min: 0, Max: 10},
	}}
	tests := []struct {
		field, value string
		ok           bool
	}{
		{"acl", "private", true},
		{"ACL", "private", true},
		{"acl", "public-read", false},
		{"Content-Type", "image/png", true},
		{"Content-Type", "image/png,image/gif", true},
		{"Content-Type", "image/png, text/plain", false},
		{"content-type", "text/plain", false},
		{"key", "uploads/a", true},
		{"key", "other/a", false},
		{"x-amz-meta-x", "v", false},
		{"x-ignore-anything", "v", true},
		{"file", "v", true},
		{"policy", "v", true},
	}
	for _, tt := range tests {
		err := policy.Check(tt.field, tt.value)
		if tt.ok {
			assert.NoError(t, err, "%s=%s", tt.field, tt.value)
		} else {
			assert.ErrorIs(t, err, domain.ErrAccessDenied, "%s=%s", tt.field, tt.value)
		}
	}
	assert.ErrorIs(t, (*auth.PostPolicy)(nil).Check("acl", "private"), domain.ErrAccessDenied)
	lo, hi, ok := policy.ContentLengthRange()
	assert.Equal(t, []any{int64(0), int64(10), true}, []any{lo, hi, ok})
	_, _, ok = (&auth.PostPolicy{}).ContentLengthRange()
	assert.False(t, ok)
	_, _, ok = (*auth.PostPolicy)(nil).LengthRange()
	assert.False(t, ok, "no policy, no range")
}

func TestPresignPostPolicy(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(testNow)
	v := newVerifier(t, clk)
	s := auth.NewSigner(auth.Deps{Clock: clk})
	creds := domain.Credentials{AccessKey: awsAccessKey, SecretKey: awsSecretKey}
	toForm := func(p *auth.PostPolicyForm, bucket string, extra map[string]string) auth.PostForm {
		fields := map[string]string{}
		for k, v := range p.Fields {
			fields[k] = v
		}
		for k, v := range extra {
			fields[k] = v
		}
		return auth.PostForm{Bucket: bucket, Fields: fields, FileName: "photo.png"}
	}

	t.Run("exact key with fields and conditions", func(t *testing.T) {
		p, err := s.PresignPostPolicy(ctx, auth.PostPolicyInput{
			Bucket: "uploads", Key: "docs/report.pdf", Expires: time.Hour, Creds: creds,
			Fields: map[string]string{"Content-Type": "application/pdf", "success_action_status": "201", "x-amz-meta-owner": "alice"},
			Conditions: []auth.PostCondition{
				{Operator: auth.ConditionLengthRange, Min: 1, Max: 10 << 20},
				{Operator: auth.ConditionStartsWith, Key: "$Cache-Control", Value: ""},
			},
		})
		require.NoError(t, err)
		assert.Equal(t, testNow.Add(time.Hour), p.Expiration)
		for _, name := range []string{"key", "policy", "x-amz-algorithm", "x-amz-credential", "x-amz-date", "x-amz-signature", "Content-Type"} {
			assert.NotEmpty(t, p.Fields[name], name)
		}
		raw, err := base64.StdEncoding.DecodeString(p.Fields["policy"])
		require.NoError(t, err)
		assert.Contains(t, string(raw), `"expiration":"2026-10-08T13:00:00.000Z"`)
		assert.Contains(t, string(raw), `["content-length-range",1,10485760]`)

		id, policy, err := v.VerifyPostPolicy(ctx, toForm(p, "uploads", map[string]string{"Cache-Control": "no-cache"}))
		require.NoError(t, err)
		assert.Equal(t, awsAccessKey, id.AccessKey)
		lo, hi, ok := policy.ContentLengthRange()
		assert.Equal(t, []any{int64(1), int64(10 << 20), true}, []any{lo, hi, ok})
		assert.Equal(t, len(p.Policy.Conditions), len(policy.Conditions))

		_, _, err = v.VerifyPostPolicy(ctx, toForm(p, "uploads", map[string]string{"key": "docs/other.pdf"}))
		assert.ErrorIs(t, err, domain.ErrAccessDenied)
		_, _, err = v.VerifyPostPolicy(ctx, toForm(p, "other", nil))
		assert.ErrorIs(t, err, domain.ErrAccessDenied)
	})

	t.Run("key prefix and temporary credentials", func(t *testing.T) {
		temp := domain.Credentials{AccessKey: tempAccessKey, SecretKey: tempSecretKey, SessionToken: tempToken}
		p, err := s.PresignPostPolicy(ctx, auth.PostPolicyInput{Bucket: "uploads", KeyPrefix: "inbox/", Expires: time.Minute, Creds: temp, Region: "us-east-1"})
		require.NoError(t, err)
		assert.Equal(t, "inbox/${filename}", p.Fields["key"])
		assert.Equal(t, tempToken, p.Fields["x-amz-security-token"])
		id, _, err := v.VerifyPostPolicy(ctx, toForm(p, "uploads", nil))
		require.NoError(t, err)
		assert.Equal(t, tempParent, id.Parent)
		_, _, err = v.VerifyPostPolicy(ctx, toForm(p, "uploads", map[string]string{"key": "inbox/sub/any.bin"}))
		require.NoError(t, err)
		_, _, err = v.VerifyPostPolicy(ctx, toForm(p, "uploads", map[string]string{"key": "outbox/any.bin"}))
		assert.ErrorIs(t, err, domain.ErrAccessDenied)
		// Without the token the temporary key is refused.
		form := toForm(p, "uploads", nil)
		delete(form.Fields, "x-amz-security-token")
		_, _, err = v.VerifyPostPolicy(ctx, form)
		assert.ErrorIs(t, err, domain.ErrAccessDenied)
	})

	t.Run("validation", func(t *testing.T) {
		valid := auth.PostPolicyInput{Bucket: "b", Key: "k", Expires: time.Hour, Creds: creds}
		for name, mutate := range map[string]func(*auth.PostPolicyInput){
			"no bucket":        func(in *auth.PostPolicyInput) { in.Bucket = "" },
			"no key":           func(in *auth.PostPolicyInput) { in.Key = "" },
			"key outside":      func(in *auth.PostPolicyInput) { in.KeyPrefix = "p/" },
			"zero expiry":      func(in *auth.PostPolicyInput) { in.Expires = 0 },
			"long expiry":      func(in *auth.PostPolicyInput) { in.Expires = 8 * 24 * time.Hour },
			"no credentials":   func(in *auth.PostPolicyInput) { in.Creds = domain.Credentials{} },
			"reserved field":   func(in *auth.PostPolicyInput) { in.Fields = map[string]string{"Policy": "x"} },
			"reserved key":     func(in *auth.PostPolicyInput) { in.Fields = map[string]string{"key": "x"} },
			"condition no key": func(in *auth.PostPolicyInput) { in.Conditions = []auth.PostCondition{{Operator: auth.ConditionEq}} },
			"bad range": func(in *auth.PostPolicyInput) {
				in.Conditions = []auth.PostCondition{{Operator: auth.ConditionLengthRange, Min: 5, Max: 1}}
			},
			"unknown operator": func(in *auth.PostPolicyInput) { in.Conditions = []auth.PostCondition{{Operator: "regex", Key: "key"}} },
			"negative range": func(in *auth.PostPolicyInput) {
				in.Conditions = []auth.PostCondition{{Operator: auth.ConditionLengthRange, Min: -1, Max: 1}}
			},
			"file is not field": func(in *auth.PostPolicyInput) { in.Fields = map[string]string{"file": "x"} },
		} {
			in := valid
			mutate(&in)
			_, err := s.PresignPostPolicy(ctx, in)
			assert.ErrorIs(t, err, domain.ErrInvalidArgument, name)
		}
	})
}

// PostForm.ObjectKey is the single implementation of the key of a POST
// upload: the policy conditions are checked on it and the handler stores
// the object under it.
func TestPostFormObjectKey(t *testing.T) {
	tests := []struct {
		name   string
		fields map[string]string
		file   string
		want   string
	}{
		{name: "plain key", fields: map[string]string{"key": "a/b.txt"}, file: "ignored.txt", want: "a/b.txt"},
		{name: "filename variable", fields: map[string]string{"key": "user/alice/${filename}"}, file: "photo.png", want: "user/alice/photo.png"},
		{name: "variable twice", fields: map[string]string{"key": "${filename}/${filename}"}, file: "x", want: "x/x"},
		{name: "field name in another case", fields: map[string]string{"Key": "upper/${filename}"}, file: "f", want: "upper/f"},
		{name: "no key", fields: map[string]string{"acl": "private"}, file: "f", want: ""},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, auth.PostForm{Fields: tt.fields, FileName: tt.file}.ObjectKey(), tt.name)
	}
}
