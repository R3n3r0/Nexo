package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/config"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/iam"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/testutil"
)

// presignedDo executes a presigned URL with a plain HTTP client, as curl
// would.
func presignedDo(t *testing.T, method, rawURL string, body []byte, hdr http.Header) response {
	t.Helper()
	req := newRequest(t, method, rawURL, body)
	for k, vs := range hdr {
		req.Header[k] = vs
	}
	return do(t, req)
}

// TPS-01 / TS-I-07: presigned GET, PUT, HEAD and DELETE generated with
// aws-sdk-go-v2 and executed with a plain HTTP client; a presigned listing.
//
// covers: S-OBJ-08
func TestPresignedOperations(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	const bucket = "presigned"
	e.mkBucket(bucket)
	ps := e.presigner(e.rootCreds())
	ctx := e.ctx

	payload := pseudoRandom(7, 300<<10)
	put, err := ps.PresignPutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String("up/file.bin")}, s3.WithPresignExpires(5*time.Minute))
	require.NoError(t, err)
	assert.Equal(t, http.MethodPut, put.Method)
	resp := presignedDo(t, http.MethodPut, put.URL, payload, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	assert.Equal(t, `"`+md5Hex(payload)+`"`, resp.Header.Get("ETag"))

	get, err := ps.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("up/file.bin")}, s3.WithPresignExpires(5*time.Minute))
	require.NoError(t, err)
	resp = presignedDo(t, http.MethodGet, get.URL, nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	assert.True(t, bytes.Equal(payload, resp.Body))
	// A presigned GET also honors Range.
	resp = presignedDo(t, http.MethodGet, get.URL, nil, http.Header{"Range": {"bytes=0-9"}})
	require.Equal(t, http.StatusPartialContent, resp.StatusCode)
	assert.Equal(t, payload[:10], resp.Body)

	head, err := ps.PresignHeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("up/file.bin")}, s3.WithPresignExpires(5*time.Minute))
	require.NoError(t, err)
	resp = presignedDo(t, http.MethodHead, head.URL, nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, fmt.Sprint(len(payload)), resp.Header.Get("Content-Length"))

	// Presigned ListObjectsV2 (signed by hand: the SDK has no presigner
	// for it).
	listURL := e.srv.S3URL + "/" + bucket + "?list-type=2&prefix=up%2F&X-Amz-Expires=300"
	resp = presignedDo(t, http.MethodGet, e.presignRaw(http.MethodGet, listURL, e.rootCreds(), nil, time.Now()), nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	assert.Contains(t, string(resp.Body), "<Key>up/file.bin</Key>")

	del, err := ps.PresignDeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String("up/file.bin")}, s3.WithPresignExpires(5*time.Minute))
	require.NoError(t, err)
	resp = presignedDo(t, http.MethodDelete, del.URL, nil, nil)
	require.Equal(t, http.StatusNoContent, resp.StatusCode, "%s", resp.Body)
	resp = presignedDo(t, http.MethodGet, get.URL, nil, nil)
	requireS3Error(t, resp, http.StatusNotFound, "NoSuchKey")

	// Presigned multipart: create, part, complete, all through URLs.
	created, err := e.root.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String("mp.bin")})
	require.NoError(t, err)
	part, err := ps.PresignUploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String(bucket), Key: aws.String("mp.bin"), UploadId: created.UploadId,
		PartNumber: aws.Int32(1)}, s3.WithPresignExpires(5*time.Minute))
	require.NoError(t, err)
	resp = presignedDo(t, http.MethodPut, part.URL, []byte("only part"), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	completeURL := fmt.Sprintf("%s/%s/mp.bin?uploadId=%s&X-Amz-Expires=300", e.srv.S3URL, bucket, url.QueryEscape(aws.ToString(created.UploadId)))
	body := fmt.Sprintf("<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>%s</ETag></Part></CompleteMultipartUpload>", resp.Header.Get("ETag"))
	resp = presignedDo(t, http.MethodPost, e.presignRaw(http.MethodPost, completeURL, e.rootCreds(), nil, time.Now()), []byte(body), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	assert.Equal(t, []byte("only part"), e.get(bucket, "mp.bin"))
}

// TPS-02: X-Amz-Expires of 0 or above 7 days is refused, even when the
// signature is valid.
//
// covers: S-OBJ-08
func TestPresignedExpiresBounds(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.mkBucket("bounds")
	e.put("bounds", "obj", []byte("x"))
	for _, tc := range []struct {
		expires string
		ok      bool
	}{
		{"0", false}, {"-1", false}, {"604801", false}, {"abc", false}, {"1", true}, {"604800", true},
	} {
		t.Run(tc.expires, func(t *testing.T) {
			raw := e.srv.S3URL + "/bounds/obj?X-Amz-Expires=" + tc.expires
			resp := presignedDo(t, http.MethodGet, e.presignRaw(http.MethodGet, raw, e.rootCreds(), nil, time.Now()), nil, nil)
			if tc.ok {
				assert.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
				return
			}
			requireS3Error(t, resp, http.StatusBadRequest, "AuthorizationQueryParametersError")
		})
	}
	// A date too far in the future is refused (clock skew).
	raw := e.srv.S3URL + "/bounds/obj?X-Amz-Expires=600"
	resp := presignedDo(t, http.MethodGet, e.presignRaw(http.MethodGet, raw, e.rootCreds(), nil, time.Now().Add(20*time.Minute)), nil, nil)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "%s", resp.Body)
}

// TPS-03 / TS-I-07 / TPS-06: expired links, with a fake server clock: the
// link expires after X-Amz-Expires, a link signed with STS credentials
// when the session expires (before the link).
//
// covers: S-OBJ-08, S-IAM-06, S-IAM-07
func TestPresignedExpiry(t *testing.T) {
	t.Parallel()
	fake := clock.NewFake(time.Now())
	e := newEnv(t, testutil.WithClock(fake))
	const bucket = "expiry"
	e.mkBucket(bucket)
	e.put(bucket, "doc", []byte("document"))

	short, err := e.presigner(e.rootCreds()).PresignGetObject(e.ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("doc")},
		s3.WithPresignExpires(time.Minute))
	require.NoError(t, err)

	// STS session of 15 minutes (the minimum) for a readonly user, and a
	// link of one hour signed with it.
	reader := e.createUser("expiry-reader", "expiry-reader-secret", "readonly")
	assumed, err := e.stsClient(reader).AssumeRole(e.ctx, &sts.AssumeRoleInput{RoleArn: aws.String("arn:aws:iam::000000000000:role/x"),
		RoleSessionName: aws.String("expiry"), DurationSeconds: aws.Int32(900)})
	require.NoError(t, err)
	temp := creds{AccessKey: aws.ToString(assumed.Credentials.AccessKeyId), SecretKey: aws.ToString(assumed.Credentials.SecretAccessKey),
		Token: aws.ToString(assumed.Credentials.SessionToken)}
	long, err := e.presigner(temp).PresignGetObject(e.ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("doc")},
		s3.WithPresignExpires(time.Hour))
	require.NoError(t, err)
	assert.Contains(t, long.URL, "X-Amz-Security-Token=")

	// A service account expiring in 20 minutes signs a link of one hour.
	key, err := e.srv.IAM.CreateAccessKey(e.ctx, iam.CreateAccessKeyInput{Caller: e.identity(reader.AccessKey), Expiration: fake.Now().Add(20 * time.Minute)})
	require.NoError(t, err)
	keyLink, err := e.presigner(creds{AccessKey: key.AccessKey, SecretKey: key.SecretKey}).PresignGetObject(e.ctx,
		&s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("doc")}, s3.WithPresignExpires(time.Hour))
	require.NoError(t, err)

	for _, raw := range []string{short.URL, long.URL, keyLink.URL} {
		resp := presignedDo(t, http.MethodGet, raw, nil, nil)
		require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	}
	var resp response

	// The SDK signs with the wall clock, which ran ahead of the fake one
	// while the server started: move the server one second past the end
	// of the short link, whatever that delay.
	u, err := url.Parse(short.URL)
	require.NoError(t, err)
	signedAt, err := time.Parse("20060102T150405Z", u.Query().Get("X-Amz-Date"))
	require.NoError(t, err)
	fake.Set(signedAt.Add(61 * time.Second))
	resp = presignedDo(t, http.MethodGet, short.URL, nil, nil)
	doc := requireS3Error(t, resp, http.StatusForbidden, "AccessDenied")
	assert.Contains(t, doc.Message, "Request has expired")
	resp = presignedDo(t, http.MethodGet, long.URL, nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "the STS link is still valid: %s", resp.Body)

	// The session expires at 15 minutes, the link would last an hour.
	fake.Advance(15 * time.Minute)
	resp = presignedDo(t, http.MethodGet, long.URL, nil, nil)
	requireS3Error(t, resp, http.StatusForbidden, "AccessDenied")
	resp = presignedDo(t, http.MethodGet, keyLink.URL, nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "the access key is still valid: %s", resp.Body)
	// The access key expires at 20 minutes.
	fake.Advance(5 * time.Minute)
	resp = presignedDo(t, http.MethodGet, keyLink.URL, nil, nil)
	requireS3Error(t, resp, http.StatusForbidden, "AccessDenied")
}

// TPS-04: an altered signature, an added or removed parameter or another
// host fail with 403 SignatureDoesNotMatch.
//
// covers: S-OBJ-08, S-IAM-02
func TestPresignedTampering(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	const bucket = "tamper"
	e.mkBucket(bucket)
	e.put(bucket, "doc", []byte("document"))
	e.put(bucket, "other", []byte("other document"))
	ps, err := e.presigner(e.rootCreds()).PresignGetObject(e.ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("doc"),
		ResponseContentType: aws.String("text/plain")}, s3.WithPresignExpires(5*time.Minute))
	require.NoError(t, err)
	resp := presignedDo(t, http.MethodGet, ps.URL, nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)

	mutate := func(fn func(u *url.URL, q url.Values)) string {
		u, err := url.Parse(ps.URL)
		require.NoError(t, err)
		q := u.Query()
		fn(u, q)
		u.RawQuery = q.Encode()
		return u.String()
	}
	flip := func(s string) string {
		b := []byte(s)
		if b[0] == 'a' {
			b[0] = 'b'
		} else {
			b[0] = 'a'
		}
		return string(b)
	}
	cases := map[string]string{
		"signature altered": mutate(func(_ *url.URL, q url.Values) { q.Set("X-Amz-Signature", flip(q.Get("X-Amz-Signature"))) }),
		"parameter added":   mutate(func(_ *url.URL, q url.Values) { q.Set("response-cache-control", "no-store") }),
		"parameter removed": mutate(func(_ *url.URL, q url.Values) { q.Del("response-content-type") }),
		"parameter changed": mutate(func(_ *url.URL, q url.Values) { q.Set("response-content-type", "text/html") }),
		"other object":      mutate(func(u *url.URL, _ url.Values) { u.Path = "/" + bucket + "/other" }),
		"expires extended":  mutate(func(_ *url.URL, q url.Values) { q.Set("X-Amz-Expires", "600") }),
		"other host": mutate(func(u *url.URL, _ url.Values) {
			_, port, _ := strings.Cut(u.Host, ":")
			u.Host = "localhost:" + port
		}),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			resp := presignedDo(t, http.MethodGet, raw, nil, nil)
			requireS3Error(t, resp, http.StatusForbidden, "SignatureDoesNotMatch")
		})
	}
	t.Run("signed by an unknown key", func(t *testing.T) {
		raw := e.presignRaw(http.MethodGet, e.srv.S3URL+"/"+bucket+"/doc?X-Amz-Expires=60", creds{AccessKey: "ghost", SecretKey: "ghost-secret"}, nil, time.Now())
		requireS3Error(t, presignedDo(t, http.MethodGet, raw, nil, nil), http.StatusForbidden, "InvalidAccessKeyId")
	})
	t.Run("method changed", func(t *testing.T) {
		resp := presignedDo(t, http.MethodDelete, ps.URL, nil, nil)
		requireS3Error(t, resp, http.StatusForbidden, "SignatureDoesNotMatch")
		assert.Equal(t, []byte("document"), e.get(bucket, "doc"))
	})
}

// TPS-05: a signed header (Content-Type, x-amz-meta-*) must be sent
// unchanged.
//
// covers: S-OBJ-08
func TestPresignedSignedHeaders(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.mkBucket("headers")

	// The SDK signs the metadata headers (it leaves Content-Type unsigned).
	ps, err := e.presigner(e.rootCreds()).PresignPutObject(e.ctx, &s3.PutObjectInput{Bucket: aws.String("headers"), Key: aws.String("meta"),
		Metadata: map[string]string{"owner": "alice"}}, s3.WithPresignExpires(5*time.Minute))
	require.NoError(t, err)
	assert.Equal(t, "alice", ps.SignedHeader.Get("X-Amz-Meta-Owner"))
	for name, hdr := range map[string]http.Header{
		"other metadata":   {"X-Amz-Meta-Owner": {"mallory"}},
		"missing metadata": {},
	} {
		t.Run(name, func(t *testing.T) {
			requireS3Error(t, presignedDo(t, http.MethodPut, ps.URL, []byte("hello"), hdr), http.StatusForbidden, "SignatureDoesNotMatch")
		})
	}
	resp := presignedDo(t, http.MethodPut, ps.URL, []byte("hello"), http.Header{"X-Amz-Meta-Owner": {"alice"}})
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)

	// Content-Type signed explicitly (SigV4 signs every header it is given).
	typed := e.presignRaw(http.MethodPut, e.srv.S3URL+"/headers/typed?X-Amz-Expires=300", e.rootCreds(),
		http.Header{"Content-Type": {"text/plain"}}, time.Now())
	assert.Contains(t, typed, "content-type")
	for name, hdr := range map[string]http.Header{
		"other content type":   {"Content-Type": {"application/json"}},
		"missing content type": {},
	} {
		t.Run(name, func(t *testing.T) {
			requireS3Error(t, presignedDo(t, http.MethodPut, typed, []byte("hello"), hdr), http.StatusForbidden, "SignatureDoesNotMatch")
		})
	}
	resp = presignedDo(t, http.MethodPut, typed, []byte("hello"), http.Header{"Content-Type": {"text/plain"}})
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	head, err := e.root.HeadObject(e.ctx, &s3.HeadObjectInput{Bucket: aws.String("headers"), Key: aws.String("typed")})
	require.NoError(t, err)
	assert.Equal(t, "text/plain", aws.ToString(head.ContentType))
	head, err = e.root.HeadObject(e.ctx, &s3.HeadObjectInput{Bucket: aws.String("headers"), Key: aws.String("meta")})
	require.NoError(t, err)
	assert.Equal(t, "alice", head.Metadata["owner"])
}

// TPS-07: links stop working as soon as the signer is disabled, loses its
// policy or its access key is deleted: permissions are evaluated at use.
//
// covers: S-OBJ-08, S-IAM-03, S-IAM-06
func TestPresignedRevokedSigner(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	const bucket = "revoked"
	e.mkBucket(bucket)
	e.put(bucket, "doc", []byte("document"))
	ctx := e.ctx
	link := func(c creds) string {
		ps, err := e.presigner(c).PresignGetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("doc")}, s3.WithPresignExpires(time.Hour))
		require.NoError(t, err)
		resp := presignedDo(t, http.MethodGet, ps.URL, nil, nil)
		require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
		return ps.URL
	}

	t.Run("user disabled", func(t *testing.T) {
		u := e.createUser("disabled-user", "disabled-secret", "readonly")
		raw := link(u)
		require.NoError(t, e.srv.IAM.SetUserStatus(ctx, u.AccessKey, domain.StatusDisabled))
		// Disabled credentials are no valid credentials, as AWS answers for
		// an inactive key (never AccessDenied, the denial of valid ones).
		requireS3Error(t, presignedDo(t, http.MethodGet, raw, nil, nil), http.StatusForbidden, "InvalidAccessKeyId")
		require.NoError(t, e.srv.IAM.SetUserStatus(ctx, u.AccessKey, domain.StatusEnabled))
		assert.Equal(t, http.StatusOK, presignedDo(t, http.MethodGet, raw, nil, nil).StatusCode, "re-enabled")
	})
	t.Run("policy removed", func(t *testing.T) {
		u := e.createUser("policy-user", "policy-secret", "readonly")
		raw := link(u)
		require.NoError(t, e.srv.IAM.SetUserPolicy(ctx, u.AccessKey, nil))
		requireS3Error(t, presignedDo(t, http.MethodGet, raw, nil, nil), http.StatusForbidden, "AccessDenied")
	})
	t.Run("named policy deleted", func(t *testing.T) {
		e.setPolicy("doc-reader", policyJSON(`{"Effect":"Allow","Action":["s3:GetObject"],"Resource":["arn:aws:s3:::revoked/doc"]}`))
		u := e.createUser("named-user", "named-secret", "doc-reader")
		raw := link(u)
		require.NoError(t, e.srv.Policies.DeletePolicy(ctx, "doc-reader"))
		requireS3Error(t, presignedDo(t, http.MethodGet, raw, nil, nil), http.StatusForbidden, "AccessDenied")
	})
	t.Run("access key deleted", func(t *testing.T) {
		u := e.createUser("key-owner", "key-owner-secret", "readonly")
		key, err := e.srv.IAM.CreateAccessKey(ctx, iam.CreateAccessKeyInput{Caller: e.identity(u.AccessKey)})
		require.NoError(t, err)
		raw := link(creds{AccessKey: key.AccessKey, SecretKey: key.SecretKey})
		require.NoError(t, e.srv.IAM.DeleteAccessKey(ctx, e.identity(u.AccessKey), key.AccessKey))
		resp := presignedDo(t, http.MethodGet, raw, nil, nil)
		requireS3Error(t, resp, http.StatusForbidden, "InvalidAccessKeyId")
	})
	t.Run("user deleted", func(t *testing.T) {
		u := e.createUser("deleted-user", "deleted-secret", "readonly")
		raw := link(u)
		require.NoError(t, e.srv.IAM.DeleteUser(ctx, u.AccessKey))
		requireS3Error(t, presignedDo(t, http.MethodGet, raw, nil, nil), http.StatusForbidden, "InvalidAccessKeyId")
	})
	t.Run("secret rotated", func(t *testing.T) {
		u := e.createUser("rotated-user", "rotated-secret", "readonly")
		raw := link(u)
		require.NoError(t, e.srv.IAM.SetUserSecret(ctx, u.AccessKey, "rotated-secret-2"))
		requireS3Error(t, presignedDo(t, http.MethodGet, raw, nil, nil), http.StatusForbidden, "SignatureDoesNotMatch")
	})
}

// TPS-08: response-* overrides set the response headers.
//
// covers: S-OBJ-08
func TestPresignedResponseOverrides(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.mkBucket("overrides")
	_, err := e.root.PutObject(e.ctx, &s3.PutObjectInput{Bucket: aws.String("overrides"), Key: aws.String("report.csv"),
		Body: strings.NewReader("a,b\n1,2\n"), ContentType: aws.String("text/csv")})
	require.NoError(t, err)
	expires := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	ps, err := e.presigner(e.rootCreds()).PresignGetObject(e.ctx, &s3.GetObjectInput{
		Bucket: aws.String("overrides"), Key: aws.String("report.csv"),
		ResponseContentDisposition: aws.String(`attachment; filename="report-2030.csv"`),
		ResponseContentType:        aws.String("application/octet-stream"),
		ResponseCacheControl:       aws.String("no-store"),
		ResponseContentLanguage:    aws.String("it-IT"),
		ResponseContentEncoding:    aws.String("identity"),
		ResponseExpires:            aws.Time(expires),
	}, s3.WithPresignExpires(5*time.Minute))
	require.NoError(t, err)
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		resp := presignedDo(t, method, ps.URL, nil, nil)
		if method == http.MethodHead {
			// The URL is signed for GET.
			assert.Equal(t, http.StatusForbidden, resp.StatusCode)
			continue
		}
		require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
		assert.Equal(t, `attachment; filename="report-2030.csv"`, resp.Header.Get("Content-Disposition"))
		assert.Equal(t, "application/octet-stream", resp.Header.Get("Content-Type"))
		assert.Equal(t, "no-store", resp.Header.Get("Cache-Control"))
		assert.Equal(t, "it-IT", resp.Header.Get("Content-Language"))
		assert.Equal(t, "identity", resp.Header.Get("Content-Encoding"))
		assert.Equal(t, expires.Format(http.TimeFormat), resp.Header.Get("Expires"))
		assert.Equal(t, "a,b\n1,2\n", string(resp.Body))
	}
	// Overrides need a signed request: anonymous requests are refused even
	// on a public bucket.
	e.putBucketPolicy("overrides", policyJSON(`{"Effect":"Allow","Principal":"*","Action":["s3:GetObject"],"Resource":["arn:aws:s3:::overrides/*"]}`))
	resp := do(t, newRequest(t, http.MethodGet, e.srv.S3URL+"/overrides/report.csv?response-content-type=text%2Fhtml", nil))
	requireS3Error(t, resp, http.StatusBadRequest, "InvalidRequest")
	resp = do(t, newRequest(t, http.MethodGet, e.srv.S3URL+"/overrides/report.csv", nil))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/csv", resp.Header.Get("Content-Type"))
}

// TPS-21: the holder of a presigned URL cannot add x-amz-* headers the
// signature does not cover. Regression of the copy-source injection: with
// an unsigned x-amz-copy-source a presigned upload link became a
// CopyObject reading, as the signer, any object the signer can read, and
// the copy landed in a public bucket.
//
// covers: S-OBJ-08
func TestPresignedUnsignedHeaders(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.mkBucket("pubb")
	e.mkBucket("secret")
	p, err := e.srv.Policies.GeneratePublicPolicy(e.ctx, "pubb", "", policy.PresetDownload)
	require.NoError(t, err)
	e.putBucketPolicy("pubb", p)
	e.put("secret", "salaries.csv", []byte("TOP SECRET"))
	alice := e.createUser("alice", "alice-secret", "readwrite")

	ps, err := e.presigner(alice).PresignPutObject(e.ctx, &s3.PutObjectInput{Bucket: aws.String("pubb"), Key: aws.String("upload.txt")},
		s3.WithPresignExpires(5*time.Minute))
	require.NoError(t, err)
	for name, hdr := range map[string]http.Header{
		"copy source":        {"X-Amz-Copy-Source": {"/secret/salaries.csv"}},
		"tagging":            {"X-Amz-Tagging": {"a=b"}},
		"metadata":           {"X-Amz-Meta-Evil": {"1"}},
		"storage class":      {"X-Amz-Storage-Class": {"STANDARD"}},
		"metadata directive": {"X-Amz-Metadata-Directive": {"REPLACE"}, "X-Amz-Copy-Source": {"/secret/salaries.csv"}},
	} {
		t.Run(name, func(t *testing.T) {
			doc := requireS3Error(t, presignedDo(t, http.MethodPut, ps.URL, []byte("upload"), hdr), http.StatusForbidden, "AccessDenied")
			assert.Equal(t, "There were headers present in the request which were not signed", doc.Message)
			_, err := e.root.HeadObject(e.ctx, &s3.HeadObjectInput{Bucket: aws.String("pubb"), Key: aws.String("upload.txt")})
			assert.Equal(t, http.StatusNotFound, httpStatus(err), "no object is created")
		})
	}
	assert.Equal(t, http.StatusNotFound, e.anonStatus(http.MethodGet, "/pubb/upload.txt", nil), "the secret never reaches the public bucket")

	// The link works as intended without extra headers.
	resp := presignedDo(t, http.MethodPut, ps.URL, []byte("upload"), http.Header{"Content-Type": {"text/plain"}})
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	resp = do(t, newRequest(t, http.MethodGet, e.srv.S3URL+"/pubb/upload.txt", nil))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "upload", string(resp.Body))

	// A signer that means to allow a copy signs the header.
	copyURL := e.presignRaw(http.MethodPut, e.srv.S3URL+"/pubb/copy.txt?X-Amz-Expires=300", alice,
		http.Header{"X-Amz-Copy-Source": {"/secret/salaries.csv"}}, time.Now())
	resp = presignedDo(t, http.MethodPut, copyURL, nil, http.Header{"X-Amz-Copy-Source": {"/secret/salaries.csv"}})
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	assert.Contains(t, string(resp.Body), "CopyObjectResult")

	// Header-signed requests follow the same rule: an x-amz-* header added
	// after signing (by a proxy or a man in the middle) is refused.
	req := newRequest(t, http.MethodPut, e.srv.S3URL+"/pubb/mitm.txt", []byte("x"))
	e.signRequest(req, alice, []byte("x"))
	req.Header.Set("X-Amz-Copy-Source", "/secret/salaries.csv")
	requireS3Error(t, do(t, req), http.StatusForbidden, "AccessDenied")
}

// TestPresignedBehindProxy (TPS-19, docs/08 §8.5): behind a reverse proxy
// that forwards the Host header, the links the console generates with
// the public URL of the deployment (NEXO_SERVER_URL) download through the
// proxy, with the content uploaded; in a cluster, whichever node signed a
// link and whichever node serves it. A link presented with another host
// is refused: the signature covers it.
//
// covers: TPS-19
func TestPresignedBehindProxy(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	_, port, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)
	public := "http://s3.public.test:" + port
	e := newEnv(t, testutil.WithConfig(func(cfg *config.Static) { cfg.ServerURL = public }))
	target, err := url.Parse(e.srv.S3URL)
	require.NoError(t, err)
	// httputil.ReverseProxy forwards the Host of the client, as nginx with
	// proxy_set_header Host $http_host.
	proxy := httptest.NewUnstartedServer(httputil.NewSingleHostReverseProxy(target))
	_ = proxy.Listener.Close()
	proxy.Listener = ln
	proxy.Start()
	t.Cleanup(proxy.Close)
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	viaProxy := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, ln.Addr().String())
		},
	}}

	e.mkBucket("tps19")
	body := bytes.Repeat([]byte("TPS-19 "), 3000)
	e.put("tps19", "report.bin", body)
	root := e.consoleLogin(e.rootCreds())
	var links []string
	for range 3 {
		resp := root.do(http.MethodPost, "/buckets/tps19/objects/share", `{"key":"report.bin","expiresIn":3600}`)
		require.Equal(t, http.StatusCreated, resp.StatusCode, "%s", resp.Body)
		var sh struct {
			URL string `json:"url"`
		}
		require.NoError(t, json.Unmarshal(resp.Body, &sh))
		resp = root.do(http.MethodPost, "/presign", `{"bucket":"tps19","key":"report.bin","expiresIn":3600}`)
		require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
		var raw struct {
			URL string `json:"url"`
		}
		require.NoError(t, json.Unmarshal(resp.Body, &raw))
		links = append(links, sh.URL, raw.URL)
	}
	for _, link := range links {
		require.True(t, strings.HasPrefix(link, public+"/tps19/report.bin?"), "the link uses the public URL: %s", link)
		for range 3 {
			resp := doWith(t, viaProxy, newRequest(t, http.MethodGet, link, nil))
			require.Equal(t, http.StatusOK, resp.StatusCode, "%s: %s", link, resp.Body)
			assert.Equal(t, body, resp.Body)
		}
		u, err := url.Parse(link)
		require.NoError(t, err)
		u.Host = target.Host
		resp := do(t, newRequest(t, http.MethodGet, u.String(), nil))
		requireS3Error(t, resp, http.StatusForbidden, "SignatureDoesNotMatch")
	}
}
