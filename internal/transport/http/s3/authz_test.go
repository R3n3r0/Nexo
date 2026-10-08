package s3_test

import (
	"context"
	"encoding/xml"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
	"github.com/r3n3r0/nexo/internal/transport/http/s3"
)

func TestActionOf(t *testing.T) {
	tests := []struct {
		op        string
		plain     policy.Action
		versioned policy.Action
	}{
		{"ListBuckets", "s3:ListAllMyBuckets", "s3:ListAllMyBuckets"},
		{"CreateBucket", "s3:CreateBucket", "s3:CreateBucket"},
		{"DeleteBucket", "s3:DeleteBucket", "s3:DeleteBucket"},
		{"HeadBucket", "s3:ListBucket", "s3:ListBucket"},
		{"ListObjects", "s3:ListBucket", "s3:ListBucket"},
		{"ListObjectsV2", "s3:ListBucket", "s3:ListBucket"},
		{"PresignedListObjects", "s3:ListBucket", "s3:ListBucket"},
		{"ListObjectVersions", "s3:ListBucketVersions", "s3:ListBucketVersions"},
		{"ListMultipartUploads", "s3:ListBucketMultipartUploads", "s3:ListBucketMultipartUploads"},
		{"GetBucketLocation", "s3:GetBucketLocation", "s3:GetBucketLocation"},
		{"GetBucketPolicy", "s3:GetBucketPolicy", "s3:GetBucketPolicy"},
		{"PutBucketPolicy", "s3:PutBucketPolicy", "s3:PutBucketPolicy"},
		{"DeleteBucketPolicy", "s3:DeleteBucketPolicy", "s3:DeleteBucketPolicy"},
		{"GetBucketPolicyStatus", "s3:GetBucketPolicyStatus", "s3:GetBucketPolicyStatus"},
		{"GetBucketVersioning", "s3:GetBucketVersioning", "s3:GetBucketVersioning"},
		{"PutBucketVersioning", "s3:PutBucketVersioning", "s3:PutBucketVersioning"},
		{"GetBucketTagging", "s3:GetBucketTagging", "s3:GetBucketTagging"},
		{"PutBucketTagging", "s3:PutBucketTagging", "s3:PutBucketTagging"},
		{"DeleteBucketTagging", "s3:PutBucketTagging", "s3:PutBucketTagging"},
		{"GetBucketCors", "s3:GetBucketCORS", "s3:GetBucketCORS"},
		{"DeleteBucketCors", "s3:PutBucketCORS", "s3:PutBucketCORS"},
		{"GetBucketLifecycle", "s3:GetLifecycleConfiguration", "s3:GetLifecycleConfiguration"},
		{"DeleteBucketLifecycle", "s3:PutLifecycleConfiguration", "s3:PutLifecycleConfiguration"},
		{"GetBucketEncryption", "s3:GetEncryptionConfiguration", "s3:GetEncryptionConfiguration"},
		{"DeleteBucketEncryption", "s3:PutEncryptionConfiguration", "s3:PutEncryptionConfiguration"},
		{"GetObjectLockConfiguration", "s3:GetBucketObjectLockConfiguration", "s3:GetBucketObjectLockConfiguration"},
		{"PutObjectLockConfiguration", "s3:PutBucketObjectLockConfiguration", "s3:PutBucketObjectLockConfiguration"},
		{"GetBucketNotification", "s3:GetBucketNotification", "s3:GetBucketNotification"},
		{"PutBucketNotification", "s3:PutBucketNotification", "s3:PutBucketNotification"},
		{"GetBucketAcl", "s3:GetBucketAcl", "s3:GetBucketAcl"},
		{"PutBucketAcl", "s3:PutBucketAcl", "s3:PutBucketAcl"},
		{"GetObject", "s3:GetObject", "s3:GetObjectVersion"},
		{"PresignedGetObject", "s3:GetObject", "s3:GetObjectVersion"},
		{"HeadObject", "s3:GetObject", "s3:GetObjectVersion"},
		{"PutObject", "s3:PutObject", "s3:PutObject"},
		{"PresignedPutObject", "s3:PutObject", "s3:PutObject"},
		{"CopyObject", "s3:PutObject", "s3:PutObject"},
		{"PostObject", "s3:PutObject", "s3:PutObject"},
		{"DeleteObject", "s3:DeleteObject", "s3:DeleteObjectVersion"},
		{"DeleteObjects", "s3:DeleteObject", "s3:DeleteObjectVersion"},
		{"CreateMultipartUpload", "s3:PutObject", "s3:PutObject"},
		{"UploadPart", "s3:PutObject", "s3:PutObject"},
		{"UploadPartCopy", "s3:PutObject", "s3:PutObject"},
		{"CompleteMultipartUpload", "s3:PutObject", "s3:PutObject"},
		{"AbortMultipartUpload", "s3:AbortMultipartUpload", "s3:AbortMultipartUpload"},
		{"ListParts", "s3:ListMultipartUploadParts", "s3:ListMultipartUploadParts"},
		{"GetObjectTagging", "s3:GetObjectTagging", "s3:GetObjectVersionTagging"},
		{"PutObjectTagging", "s3:PutObjectTagging", "s3:PutObjectVersionTagging"},
		{"DeleteObjectTagging", "s3:DeleteObjectTagging", "s3:DeleteObjectVersionTagging"},
		{"GetObjectAcl", "s3:GetObjectAcl", "s3:GetObjectVersionAcl"},
		{"PutObjectAcl", "s3:PutObjectAcl", "s3:PutObjectVersionAcl"},
		{"Unknown", "", ""},
		{"", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.op, func(t *testing.T) {
			assert.Equal(t, tc.plain, s3.ActionOf(tc.op, false))
			assert.Equal(t, tc.versioned, s3.ActionOf(tc.op, true))
		})
	}
}

// TestEveryOperationIsAuthorized checks that each operation mounted by the
// groups maps to the expected action when served.
func TestEveryOperationIsAuthorized(t *testing.T) {
	alice := &domain.Identity{AccessKey: "alice"}
	tests := []struct {
		method, target string
		headers        []string
		action         policy.Action
		bucket, object string
	}{
		{"GET", "/", nil, policy.ActionListAllMyBuckets, "", ""},
		{"PUT", "/b", nil, policy.ActionCreateBucket, "b", ""},
		{"HEAD", "/b", nil, policy.ActionListBucket, "b", ""},
		{"DELETE", "/b", nil, policy.ActionDeleteBucket, "b", ""},
		{"GET", "/b", nil, policy.ActionListBucket, "b", ""},
		{"GET", "/b?list-type=2", nil, policy.ActionListBucket, "b", ""},
		{"GET", "/b?versions", nil, policy.ActionListBucketVersions, "b", ""},
		{"GET", "/b?uploads", nil, policy.ActionListMultipart, "b", ""},
		{"GET", "/b?location", nil, policy.ActionGetBucketLocation, "b", ""},
		{"GET", "/b?policy", nil, policy.ActionGetBucketPolicy, "b", ""},
		{"PUT", "/b?policy", nil, policy.ActionPutBucketPolicy, "b", ""},
		{"DELETE", "/b?policy", nil, policy.ActionDeleteBucketPolicy, "b", ""},
		{"GET", "/b?policyStatus", nil, policy.ActionGetBucketPolicyStatus, "b", ""},
		{"GET", "/b?versioning", nil, policy.ActionGetBucketVersioning, "b", ""},
		{"PUT", "/b?versioning", nil, policy.ActionPutBucketVersioning, "b", ""},
		{"GET", "/b?tagging", nil, "s3:GetBucketTagging", "b", ""},
		{"PUT", "/b?tagging", nil, "s3:PutBucketTagging", "b", ""},
		{"DELETE", "/b?tagging", nil, "s3:PutBucketTagging", "b", ""},
		{"GET", "/b?cors", nil, "s3:GetBucketCORS", "b", ""},
		{"GET", "/b?lifecycle", nil, "s3:GetLifecycleConfiguration", "b", ""},
		{"GET", "/b?encryption", nil, "s3:GetEncryptionConfiguration", "b", ""},
		{"GET", "/b?object-lock", nil, "s3:GetBucketObjectLockConfiguration", "b", ""},
		{"GET", "/b?notification", nil, "s3:GetBucketNotification", "b", ""},
		{"GET", "/b?acl", nil, "s3:GetBucketAcl", "b", ""},
		{"GET", "/b/k", nil, policy.ActionGetObject, "b", "k"},
		{"GET", "/b/k?versionId=v1", nil, policy.ActionGetObjectVersion, "b", "k"},
		{"HEAD", "/b/k", nil, policy.ActionGetObject, "b", "k"},
		{"PUT", "/b/k", nil, policy.ActionPutObject, "b", "k"},
		{"DELETE", "/b/k", nil, policy.ActionDeleteObject, "b", "k"},
		{"DELETE", "/b/k?versionId=v1", nil, policy.ActionDeleteObjectVersion, "b", "k"},
		{"GET", "/b/k?tagging", nil, policy.ActionGetObjectTagging, "b", "k"},
		{"GET", "/b/k?tagging&versionId=v1", nil, "s3:GetObjectVersionTagging", "b", "k"},
		{"PUT", "/b/k?tagging", nil, policy.ActionPutObjectTagging, "b", "k"},
		{"DELETE", "/b/k?tagging", nil, policy.ActionDeleteObjectTagging, "b", "k"},
		{"GET", "/b/k?acl", nil, "s3:GetObjectAcl", "b", "k"},
		{"POST", "/b/k?uploads", nil, policy.ActionPutObject, "b", "k"},
		{"PUT", "/b/k?uploadId=u&partNumber=1", nil, policy.ActionPutObject, "b", "k"},
		{"POST", "/b/k?uploadId=u", nil, policy.ActionPutObject, "b", "k"},
		{"DELETE", "/b/k?uploadId=u", nil, policy.ActionAbortMultipart, "b", "k"},
		{"GET", "/b/k?uploadId=u", nil, policy.ActionListMultipartParts, "b", "k"},
		{"PUT", "/b/k", []string{"x-amz-copy-source", "/src/key"}, policy.ActionGetObject, "src", "key"},
		{"PUT", "/b/k?uploadId=u&partNumber=1", []string{"x-amz-copy-source", "/src/key?versionId=v"}, policy.ActionGetObjectVersion, "src", "key"},
	}
	for _, tc := range tests {
		t.Run(tc.method+" "+tc.target, func(t *testing.T) {
			f := newFixture(t, asIdentity(alice), withStrictPolicy())
			f.policy.EXPECT().Evaluate(mock.Anything, mock.MatchedBy(func(a policy.Args) bool {
				return a.Action == tc.action && a.Bucket == tc.bucket && a.Object == tc.object && a.Identity == alice
			})).Return(policy.Decision{Allowed: false, Reason: "denied by test"}, nil).Once()
			rec := f.do(tc.method, tc.target, "", tc.headers...)
			if tc.method == http.MethodHead {
				assert.Equal(t, http.StatusForbidden, rec.Code)
				assert.Empty(t, rec.Body.String())
				return
			}
			requireError(t, rec, http.StatusForbidden, "AccessDenied")
		})
	}
}

func TestAuthorizationConditions(t *testing.T) {
	alice := &domain.Identity{AccessKey: "alice"}
	f := newFixture(t, asIdentity(alice), withStrictPolicy())
	var args policy.Args
	f.policy.EXPECT().Evaluate(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, a policy.Args) (policy.Decision, error) {
		args = a
		return policy.Decision{Allowed: false}, nil
	}).Once()
	req := request(http.MethodGet, "/pub?prefix=docs/&delimiter=/&max-keys=10", "",
		"User-Agent", "aws-sdk-go/2", "Referer", "https://app.example.com/",
		"Authorization", "AWS4-HMAC-SHA256 Credential=alice/20261008/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=abc",
		"X-Amz-Date", "20261008T115900Z", "x-amz-acl", "private", "x-amz-security-token", "secret", "x-amz-meta-a", "b")
	req.RemoteAddr = "10.1.2.3:4567"
	rec := f.serve(req)
	requireError(t, rec, http.StatusForbidden, "AccessDenied")

	assert.Nil(t, args.BucketPolicy, "the engine resolves the bucket policy itself (one source, one parser)")
	assert.Equal(t, "pub", args.Bucket)
	assert.Equal(t, now, args.Now)
	c := args.Conditions
	assert.Equal(t, []string{"10.1.2.3"}, c["aws:SourceIp"])
	assert.Equal(t, []string{"false"}, c["aws:SecureTransport"])
	assert.Equal(t, []string{"aws-sdk-go/2"}, c["aws:UserAgent"])
	assert.Equal(t, []string{"https://app.example.com/"}, c["aws:Referer"])
	assert.Equal(t, []string{"2026-10-08T12:00:00Z"}, c["aws:CurrentTime"])
	assert.Equal(t, []string{"docs/"}, c["s3:prefix"])
	assert.Equal(t, []string{"/"}, c["s3:delimiter"])
	assert.Equal(t, []string{"10"}, c["s3:max-keys"])
	assert.Equal(t, []string{"private"}, c["s3:x-amz-acl"])
	assert.Equal(t, []string{"REST-HEADER"}, c["s3:authType"])
	assert.Equal(t, []string{"AWS4-HMAC-SHA256"}, c["s3:signatureversion"])
	assert.Equal(t, []string{"60000"}, c["s3:signatureAge"], "signed one minute before the fake clock")
	assert.NotContains(t, c, "s3:x-amz-security-token", "credentials never reach the condition context")
	assert.NotContains(t, c, "s3:x-amz-meta-a")
}

// TestAuthorizationConditionsUseTheForwardedClient: behind trusted
// proxies the client of aws:SourceIp and aws:SecureTransport is the one
// they forward (middleware.NewForwarded), as on the console listener, so
// a policy decides alike through S3 and through the console (regression:
// the S3 listener used the address and the scheme of the proxy).
func TestAuthorizationConditionsUseTheForwardedClient(t *testing.T) {
	forwarded, err := middleware.NewForwarded(middleware.ForwardedDeps{}, middleware.WithTrustedProxies("10.0.0.0/8"))
	require.NoError(t, err)
	tests := []struct {
		name, peer, xff, proto string
		wantIP, wantSecure     string
	}{
		{name: "trusted proxy", peer: "10.0.0.5:1234", xff: "198.51.100.7", proto: "https", wantIP: "198.51.100.7", wantSecure: "true"},
		{name: "untrusted peer", peer: "192.0.2.9:1234", xff: "198.51.100.7", proto: "https", wantIP: "192.0.2.9", wantSecure: "false"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, asIdentity(&domain.Identity{AccessKey: "alice"}), withStrictPolicy())
			var args policy.Args
			f.policy.EXPECT().Evaluate(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, a policy.Args) (policy.Decision, error) {
				args = a
				return policy.Decision{Allowed: false}, nil
			}).Once()
			req := request(http.MethodGet, "/pub", "", "X-Forwarded-For", tt.xff, "X-Forwarded-Proto", tt.proto)
			req.RemoteAddr = tt.peer
			rec := httptest.NewRecorder()
			forwarded.Wrap(f.handler).ServeHTTP(rec, req)
			requireError(t, rec, http.StatusForbidden, "AccessDenied")
			assert.Equal(t, []string{tt.wantIP}, args.Conditions[policy.KeySourceIP])
			assert.Equal(t, []string{tt.wantSecure}, args.Conditions[policy.KeySecureTransport])
		})
	}
}

func TestAuthorizationPresignedAndTagConditions(t *testing.T) {
	f := newFixture(t, asIdentity(&domain.Identity{AccessKey: "alice"}), withStrictPolicy())
	var args policy.Args
	f.policy.EXPECT().Evaluate(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, a policy.Args) (policy.Decision, error) {
		args = a
		return policy.Decision{Allowed: false}, nil
	}).Once()
	rec := f.do(http.MethodPut, "/b/k?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Date=20261008T113000Z&X-Amz-Signature=s&versionId=v", "data",
		"x-amz-tagging", "team=a&env=prod")
	requireError(t, rec, http.StatusForbidden, "AccessDenied")
	assert.Nil(t, args.BucketPolicy, "the engine resolves the bucket policy")
	c := args.Conditions
	assert.Equal(t, []string{"REST-QUERY-STRING"}, c["s3:authType"])
	assert.Equal(t, []string{"1800000"}, c["s3:signatureAge"])
	assert.Equal(t, []string{"v"}, c["s3:versionid"])
	assert.Equal(t, []string{"a"}, c["s3:RequestObjectTag/team"])
	assert.Equal(t, []string{"prod"}, c["s3:RequestObjectTag/env"])
	assert.ElementsMatch(t, []string{"team", "env"}, c["s3:RequestObjectTagKeys"])
}

func TestAuthorizationIdentities(t *testing.T) {
	t.Run("anonymous without middleware", func(t *testing.T) {
		f := newFixture(t, withoutIdentity(), withStrictPolicy())
		f.policy.EXPECT().Evaluate(mock.Anything, mock.MatchedBy(func(a policy.Args) bool {
			return a.Identity.IsAnonymous && a.Conditions["s3:authType"] == nil && a.Conditions["s3:signatureAge"] == nil
		})).Return(policy.Decision{Allowed: true}, nil).Once()
		f.objects.EXPECT().GetObjectInfo(mock.Anything, "pub", "k", mock.Anything).Return(domain.ObjectInfo{Size: 1, ModTime: now}, nil).Once()
		rec := f.do(http.MethodHead, "/pub/k", "")
		assert.Equal(t, http.StatusOK, rec.Code)
	})
	t.Run("root skips the bucket policy", func(t *testing.T) {
		f := newFixture(t, withStrictPolicy())
		f.policy.EXPECT().Evaluate(mock.Anything, mock.MatchedBy(func(a policy.Args) bool { return a.Identity.IsRoot && a.BucketPolicy == nil })).
			Return(policy.Decision{Allowed: true}, nil).Once()
		f.buckets.EXPECT().Delete(mock.Anything, "b", bucket.DeleteOptions{}).Return(nil).Once()
		rec := f.do(http.MethodDelete, "/b", "")
		assert.Equal(t, http.StatusNoContent, rec.Code)
	})
	t.Run("engine failure", func(t *testing.T) {
		f := newFixture(t, withStrictPolicy())
		f.policy.EXPECT().Evaluate(mock.Anything, mock.Anything).Return(policy.Decision{}, errors.New("engine down")).Once()
		requireError(t, f.do(http.MethodGet, "/", ""), http.StatusInternalServerError, "InternalError")
	})
	t.Run("bucket policy lookup failure", func(t *testing.T) {
		f := newFixture(t, asIdentity(&domain.Identity{AccessKey: "alice"}), withStrictPolicy())
		f.policy.EXPECT().Evaluate(mock.Anything, mock.Anything).Return(policy.Decision{}, domain.ErrReadQuorum).Once()
		requireError(t, f.do(http.MethodGet, "/b", ""), http.StatusServiceUnavailable, "SlowDown")
	})
	t.Run("unreadable bucket policy fails closed", func(t *testing.T) {
		f := newFixture(t, asIdentity(&domain.Identity{AccessKey: "alice"}), withStrictPolicy())
		f.policy.EXPECT().Evaluate(mock.Anything, mock.Anything).Return(policy.Decision{}, errors.New("stored bucket policy does not parse")).Once()
		requireError(t, f.do(http.MethodGet, "/b", ""), http.StatusInternalServerError, "InternalError")
	})
}

func TestForceDeleteBucketNeedsItsOwnAction(t *testing.T) {
	alice := &domain.Identity{AccessKey: "alice"}
	f := newFixture(t, asIdentity(alice), withStrictPolicy())
	f.policy.EXPECT().Evaluate(mock.Anything, mock.MatchedBy(func(a policy.Args) bool { return a.Action == policy.ActionDeleteBucket })).
		Return(policy.Decision{Allowed: true}, nil).Twice()
	f.policy.EXPECT().Evaluate(mock.Anything, mock.MatchedBy(func(a policy.Args) bool { return a.Action == "s3:ForceDeleteBucket" })).
		Return(policy.Decision{Allowed: false}, nil).Once()
	requireError(t, f.do(http.MethodDelete, "/b", "", "x-nexo-force-delete", "true"), http.StatusForbidden, "AccessDenied")
	f.policy.EXPECT().Evaluate(mock.Anything, mock.MatchedBy(func(a policy.Args) bool { return a.Action == "s3:ForceDeleteBucket" })).
		Return(policy.Decision{Allowed: true}, nil).Once()
	f.buckets.EXPECT().Delete(mock.Anything, "b", bucket.DeleteOptions{Force: true}).Return(nil).Once()
	assert.Equal(t, http.StatusNoContent, f.do(http.MethodDelete, "/b", "", "x-amz-force-delete", "true").Code)
}

func TestDeleteObjectsAuthorizesEveryKey(t *testing.T) {
	alice := &domain.Identity{AccessKey: "alice"}
	f := newFixture(t, asIdentity(alice), withStrictPolicy())
	f.policy.EXPECT().Evaluate(mock.Anything, mock.MatchedBy(func(a policy.Args) bool { return a.Object == "ok" && a.Action == policy.ActionDeleteObject })).
		Return(policy.Decision{Allowed: true}, nil).Once()
	f.policy.EXPECT().Evaluate(mock.Anything, mock.MatchedBy(func(a policy.Args) bool {
		return a.Object == "denied" && a.Action == policy.ActionDeleteObjectVersion
	})).Return(policy.Decision{Allowed: false}, nil).Once()
	f.objects.EXPECT().DeleteObjects(mock.Anything, object.DeleteObjectsInput{Bucket: "b", Objects: []object.ObjectToDelete{{Key: "ok"}}}).
		Return(object.DeleteObjectsResult{Deleted: []object.DeletedObject{{Key: "ok"}}}, nil).Once()
	rec := f.do(http.MethodPost, "/b?delete", `<Delete><Object><Key>ok</Key></Object><Object><Key>denied</Key><VersionId>v1</VersionId></Object></Delete>`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var res s3.DeleteResult
	require.NoError(t, xml.Unmarshal(rec.Body.Bytes(), &res))
	require.Len(t, res.Deleted, 1)
	require.Len(t, res.Errors, 1)
	assert.Equal(t, s3.DeleteErrorEntry{Key: "denied", VersionID: "v1", Code: "AccessDenied", Message: "Access Denied."}, res.Errors[0])

	// Every key denied: the object layer is not called.
	f.policy.EXPECT().Evaluate(mock.Anything, mock.Anything).Return(policy.Decision{Allowed: false}, nil).Once()
	rec = f.do(http.MethodPost, "/b?delete", `<Delete><Object><Key>x</Key></Object></Delete>`)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "<Code>AccessDenied</Code>")

	// An engine failure (bucket policy unreadable) fails the key.
	f.policy.EXPECT().Evaluate(mock.Anything, mock.Anything).Return(policy.Decision{}, domain.ErrReadQuorum).Once()
	rec = f.do(http.MethodPost, "/b?delete", `<Delete><Object><Key>x</Key></Object></Delete>`)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "<Code>SlowDown</Code>")
}

func TestPostObjectAuthorizesTheSigner(t *testing.T) {
	f := newFixture(t, withoutIdentity(), withStrictPolicy())
	signer := &domain.Identity{AccessKey: "uploader"}
	f.verifier.EXPECT().VerifyPostPolicy(mock.Anything, mock.Anything).Return(signer, nil, nil).Once()
	f.policy.EXPECT().Evaluate(mock.Anything, mock.MatchedBy(func(a policy.Args) bool {
		return a.Identity == signer && a.Action == policy.ActionPutObject && a.Object == "k" && a.Conditions["s3:authType"][0] == "POST"
	})).Return(policy.Decision{Allowed: false}, nil).Once()
	body, ct := postForm(map[string]string{"key": "k", "policy": "p", "x-amz-signature": "s"}, "f.txt", "x")
	rec := f.serve(request(http.MethodPost, "/b", body, "Content-Type", ct))
	requireError(t, rec, http.StatusForbidden, "AccessDenied")
}
