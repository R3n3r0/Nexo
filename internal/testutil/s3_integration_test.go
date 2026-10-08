package testutil_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/service/iam"
	"github.com/r3n3r0/nexo/internal/testutil"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// sdkConfig returns an SDK configuration for the server with static
// credentials.
func sdkConfig(srv *testutil.Server, accessKey, secretKey, token string) aws.Config {
	return aws.Config{
		Region:      srv.Config.Region,
		Credentials: credentials.NewStaticCredentialsProvider(accessKey, secretKey, token),
		HTTPClient:  &http.Client{Timeout: 30 * time.Second},
	}
}

// s3Client returns a path-style S3 client for the server.
func s3Client(srv *testutil.Server, accessKey, secretKey, token string) *s3.Client {
	return s3.NewFromConfig(sdkConfig(srv, accessKey, secretKey, token), func(o *s3.Options) {
		o.BaseEndpoint = aws.String(srv.S3URL)
		o.UsePathStyle = true
	})
}

// apiCode returns the error code of an SDK error ("" when none).
func apiCode(err error) string {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode()
	}
	return ""
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return b
}

func TestS3SDKEndToEnd(t *testing.T) {
	srv := testutil.StartServer(t)
	ctx := context.Background()
	root := s3Client(srv, srv.RootUser, srv.RootPassword, "")
	const bucket = "smoke"

	_, err := root.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	list, err := root.ListBuckets(ctx, &s3.ListBucketsInput{})
	require.NoError(t, err)
	require.Len(t, list.Buckets, 1, "the system directory is not a bucket")
	assert.Equal(t, bucket, aws.ToString(list.Buckets[0].Name))

	// PutObject 1 MiB with metadata, HeadObject, ranged GetObject.
	payload := randomBytes(t, 1<<20)
	_, err = root.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String("dir/one.bin"), Body: bytes.NewReader(payload),
		ContentType: aws.String("application/x-test"), Metadata: map[string]string{"owner": "smoke"},
	})
	require.NoError(t, err)
	head, err := root.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("dir/one.bin")})
	require.NoError(t, err)
	assert.Equal(t, int64(1<<20), aws.ToInt64(head.ContentLength))
	assert.Equal(t, "application/x-test", aws.ToString(head.ContentType))
	assert.Equal(t, "smoke", head.Metadata["owner"])
	got, err := root.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("dir/one.bin"), Range: aws.String("bytes=100-199")})
	require.NoError(t, err)
	part, err := io.ReadAll(got.Body)
	require.NoError(t, got.Body.Close())
	require.NoError(t, err)
	assert.Equal(t, payload[100:200], part)
	assert.Equal(t, "bytes 100-199/1048576", aws.ToString(got.ContentRange))

	// ListObjectsV2 with prefix and delimiter.
	for _, k := range []string{"dir/two.txt", "dir/sub/three.txt", "top.txt"} {
		_, err = root.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(k), Body: strings.NewReader(k)})
		require.NoError(t, err)
	}
	lst, err := root.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String("dir/"), Delimiter: aws.String("/")})
	require.NoError(t, err)
	var keys, prefixes []string
	for _, o := range lst.Contents {
		keys = append(keys, aws.ToString(o.Key))
	}
	for _, p := range lst.CommonPrefixes {
		prefixes = append(prefixes, aws.ToString(p.Prefix))
	}
	assert.Equal(t, []string{"dir/one.bin", "dir/two.txt"}, keys)
	assert.Equal(t, []string{"dir/sub/"}, prefixes)

	// CopyObject keeps the metadata.
	_, err = root.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String(bucket), Key: aws.String("copy.bin"), CopySource: aws.String(bucket + "/dir/one.bin")})
	require.NoError(t, err)
	head, err = root.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("copy.bin")})
	require.NoError(t, err)
	assert.Equal(t, "smoke", head.Metadata["owner"])

	// Multipart upload: three 5 MiB parts.
	created, err := root.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String("big.bin")})
	require.NoError(t, err)
	var parts []types.CompletedPart
	var whole []byte
	for n := int32(1); n <= 3; n++ {
		data := randomBytes(t, 5<<20)
		whole = append(whole, data...)
		up, err := root.UploadPart(ctx, &s3.UploadPartInput{
			Bucket: aws.String(bucket), Key: aws.String("big.bin"), UploadId: created.UploadId,
			PartNumber: aws.Int32(n), Body: bytes.NewReader(data),
		})
		require.NoError(t, err)
		parts = append(parts, types.CompletedPart{ETag: up.ETag, PartNumber: aws.Int32(n)})
	}
	done, err := root.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(bucket), Key: aws.String("big.bin"), UploadId: created.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	})
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(strings.Trim(aws.ToString(done.ETag), `"`), "-3"))
	big, err := root.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("big.bin")})
	require.NoError(t, err)
	bigData, err := io.ReadAll(big.Body)
	require.NoError(t, big.Body.Close())
	require.NoError(t, err)
	assert.True(t, bytes.Equal(whole, bigData), "multipart content round-trips")

	// Presigned GET, downloaded with a plain HTTP client.
	presigned, err := s3.NewPresignClient(root).PresignGetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("top.txt")},
		s3.WithPresignExpires(5*time.Minute))
	require.NoError(t, err)
	resp, err := http.Get(presigned.URL)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	assert.Equal(t, "top.txt", string(body))

	// DeleteObjects.
	del, err := root.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: aws.String(bucket), Delete: &types.Delete{Objects: []types.ObjectIdentifier{
		{Key: aws.String("top.txt")}, {Key: aws.String("copy.bin")}, {Key: aws.String("missing")},
	}}})
	require.NoError(t, err)
	assert.Len(t, del.Deleted, 3)
	assert.Empty(t, del.Errors)
	_, err = root.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("top.txt")})
	require.Error(t, err)

	// Wrong secret: 403 SignatureDoesNotMatch.
	_, err = s3Client(srv, srv.RootUser, "wrong-secret-key", "").ListBuckets(ctx, &s3.ListBucketsInput{})
	require.Error(t, err)
	assert.Equal(t, "SignatureDoesNotMatch", apiCode(err))
	var respErr interface{ HTTPStatusCode() int }
	require.ErrorAs(t, err, &respErr)
	assert.Equal(t, http.StatusForbidden, respErr.HTTPStatusCode())
	_, err = s3Client(srv, "nobody", "nobody-secret", "").ListBuckets(ctx, &s3.ListBucketsInput{})
	assert.Equal(t, "InvalidAccessKeyId", apiCode(err))
}

func TestS3SDKReadOnlyUserPublicBucketAndSTS(t *testing.T) {
	srv := testutil.StartServer(t)
	ctx := context.Background()
	root := s3Client(srv, srv.RootUser, srv.RootPassword, "")
	const bucket = "shared"
	_, err := root.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	_, err = root.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String("doc.txt"), Body: strings.NewReader("hello")})
	require.NoError(t, err)

	// A readonly user reads but cannot write.
	_, err = srv.App.IAM().CreateUser(ctx, iam.CreateUserInput{AccessKey: "reader", SecretKey: "reader-secret", PolicyNames: []string{"readonly"}})
	require.NoError(t, err)
	reader := s3Client(srv, "reader", "reader-secret", "")
	obj, err := reader.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("doc.txt")})
	require.NoError(t, err)
	_ = obj.Body.Close()
	_, err = reader.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String("new.txt"), Body: strings.NewReader("x")})
	require.Error(t, err)
	assert.Equal(t, "AccessDenied", apiCode(err))

	// Anonymous access follows the bucket policy.
	anonGet := func(key string) int {
		resp, err := http.Get(srv.S3URL + "/" + bucket + "/" + key)
		require.NoError(t, err)
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	assert.Equal(t, http.StatusForbidden, anonGet("doc.txt"))
	publicRead := fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":["*"]},"Action":["s3:GetObject"],"Resource":["arn:aws:s3:::%s/*"]}]}`, bucket)
	_, err = root.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{Bucket: aws.String(bucket), Policy: aws.String(publicRead)})
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, anonGet("doc.txt"))
	status, err := root.GetBucketPolicyStatus(ctx, &s3.GetBucketPolicyStatusInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	assert.True(t, aws.ToBool(status.PolicyStatus.IsPublic))
	req, err := http.NewRequest(http.MethodPut, srv.S3URL+"/"+bucket+"/anon.txt", strings.NewReader("x"))
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "the policy grants reads only")
	// A policy naming another bucket is rejected.
	_, err = root.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{Bucket: aws.String(bucket),
		Policy: aws.String(strings.ReplaceAll(publicRead, "arn:aws:s3:::"+bucket, "arn:aws:s3:::other"))})
	assert.Equal(t, "MalformedPolicy", apiCode(err))

	// STS: the reader assumes a role restricted by a session policy.
	stsClient := sts.NewFromConfig(sdkConfig(srv, "reader", "reader-secret", ""), func(o *sts.Options) {
		o.BaseEndpoint = aws.String(srv.S3URL)
	})
	sessionPolicy := fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject"],"Resource":["arn:aws:s3:::%s/doc.txt"]}]}`, bucket)
	assumed, err := stsClient.AssumeRole(ctx, &sts.AssumeRoleInput{
		RoleArn: aws.String("arn:xxx:xxx:xxx:xxxx"), RoleSessionName: aws.String("smoke"),
		DurationSeconds: aws.Int32(900), Policy: aws.String(sessionPolicy),
	})
	require.NoError(t, err)
	creds := assumed.Credentials
	require.NotNil(t, creds)
	assert.NotEmpty(t, aws.ToString(creds.SessionToken))
	assert.WithinDuration(t, time.Now().Add(15*time.Minute), aws.ToTime(creds.Expiration), time.Minute)
	temp := s3Client(srv, aws.ToString(creds.AccessKeyId), aws.ToString(creds.SecretAccessKey), aws.ToString(creds.SessionToken))
	obj, err = temp.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("doc.txt")})
	require.NoError(t, err)
	_ = obj.Body.Close()
	_, err = temp.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
	assert.Equal(t, "AccessDenied", apiCode(err), "the session policy narrows the readonly user")
	_, err = root.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String("other.txt"), Body: strings.NewReader("x")})
	require.NoError(t, err)
	_, err = temp.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("other.txt")})
	assert.Equal(t, "AccessDenied", apiCode(err), "the session policy also narrows what the bucket policy grants")
	// Without the session token the temporary key is refused.
	_, err = s3Client(srv, aws.ToString(creds.AccessKeyId), aws.ToString(creds.SecretAccessKey), "").GetObject(ctx,
		&s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("doc.txt")})
	require.Error(t, err)

	// Temporary credentials cannot assume a role again; anonymous callers
	// get an STS error document.
	_, err = sts.NewFromConfig(sdkConfig(srv, aws.ToString(creds.AccessKeyId), aws.ToString(creds.SecretAccessKey), aws.ToString(creds.SessionToken)),
		func(o *sts.Options) { o.BaseEndpoint = aws.String(srv.S3URL) }).
		AssumeRole(ctx, &sts.AssumeRoleInput{RoleArn: aws.String("arn:x"), RoleSessionName: aws.String("again")})
	assert.Equal(t, "AccessDenied", apiCode(err))
	resp, err = http.Post(srv.S3URL+"/", "application/x-www-form-urlencoded", strings.NewReader("Action=AssumeRole&Version=2011-06-15"))
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Contains(t, string(body), "<ErrorResponse")
	var s3err httpx.S3ErrorResponse
	assert.Error(t, xml.Unmarshal(body, &s3err), "STS answers in the Query protocol format")
}

func TestAnonymousStreamingUploadToPublicBucket(t *testing.T) {
	srv := testutil.StartServer(t)
	ctx := context.Background()
	root := s3Client(srv, srv.RootUser, srv.RootPassword, "")
	const bucket = "dropbox"
	_, err := root.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	publicWrite := fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":["s3:PutObject"],"Resource":["arn:aws:s3:::%s/*"]}]}`, bucket)
	_, err = root.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{Bucket: aws.String(bucket), Policy: aws.String(publicWrite)})
	require.NoError(t, err)

	// An unsigned aws-chunked body with a trailing CRC32, as the SDKs send
	// it without credentials.
	payload := []byte("anonymous upload through aws-chunked")
	crc := crc32.ChecksumIEEE(payload)
	sum := base64.StdEncoding.EncodeToString([]byte{byte(crc >> 24), byte(crc >> 16), byte(crc >> 8), byte(crc)})
	put := func(checksum string) *http.Response {
		var body bytes.Buffer
		fmt.Fprintf(&body, "%x\r\n%s\r\n0\r\nx-amz-checksum-crc32:%s\r\n\r\n", len(payload), payload, checksum)
		req, err := http.NewRequest(http.MethodPut, srv.S3URL+"/"+bucket+"/anon.txt", &body)
		require.NoError(t, err)
		req.Header.Set("X-Amz-Content-Sha256", "STREAMING-UNSIGNED-PAYLOAD-TRAILER")
		req.Header.Set("Content-Encoding", "aws-chunked")
		req.Header.Set("X-Amz-Decoded-Content-Length", fmt.Sprint(len(payload)))
		req.Header.Set("X-Amz-Trailer", "x-amz-checksum-crc32")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp
	}
	assert.Equal(t, http.StatusBadRequest, put("AAAAAA==").StatusCode, "a wrong trailing checksum fails the upload")
	resp := put(sum)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	got, err := root.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("anon.txt")})
	require.NoError(t, err)
	data, err := io.ReadAll(got.Body)
	require.NoError(t, got.Body.Close())
	require.NoError(t, err)
	assert.Equal(t, payload, data)
}
