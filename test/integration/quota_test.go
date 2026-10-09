package integration_test

import (
	"bytes"
	"io"
	"net/http"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/service/bucket"
)

// TestBucketQuota (TS-I-16, docs/05 T-7.7): the writes beyond the hard
// quota of a bucket are refused with 507 XNexoQuotaExceeded and write
// nothing: PutObject (declared length or streamed), CopyObject and
// CompleteMultipartUpload; a delete frees the space at once. The usage of
// the whole deployment counts: in a cluster the writes go to every node.
func TestBucketQuota(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := e.ctx
	const qb = "quota"
	e.mkBucket(qb)
	_, err := e.srv.Buckets.SetQuota(ctx, qb, bucket.Quota{Type: bucket.QuotaHard, Size: 1 << 20})
	require.NoError(t, err)

	requireQuota := func(err error, msg string) {
		t.Helper()
		require.Error(t, err, msg)
		assert.Equal(t, http.StatusInsufficientStorage, httpStatus(err), msg)
		assert.Equal(t, "XNexoQuotaExceeded", apiCode(err), msg)
	}
	exists := func(key string) bool {
		_, err := e.root.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(qb), Key: aws.String(key)})
		return err == nil
	}
	putSize := func(key string, n int) error {
		_, err := e.root.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(qb), Key: aws.String(key), Body: bytes.NewReader(make([]byte, n))})
		return err
	}

	require.NoError(t, putSize("a", 600<<10))
	requireQuota(putSize("b", 600<<10), "600 KiB more exceed 1 MiB")
	assert.False(t, exists("b"), "a refused write leaves nothing")
	require.NoError(t, putSize("c", 300<<10), "900 KiB fit")

	_, err = e.root.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String(qb), Key: aws.String("a-copy"), CopySource: aws.String(qb + "/a")})
	requireQuota(err, "a copy beyond the quota")
	assert.False(t, exists("a-copy"))

	// A streamed upload (no Content-Length) is aborted beyond the room left.
	req := newRequest(t, http.MethodPut, e.srv.S3URL+"/"+qb+"/stream", nil)
	req.Body = io.NopCloser(bytes.NewReader(make([]byte, 400<<10)))
	req.ContentLength = -1
	e.signRequestUnsigned(req, e.rootCreds())
	resp := do(t, req)
	requireS3Error(t, resp, http.StatusInsufficientStorage, "XNexoQuotaExceeded")
	assert.False(t, exists("stream"))

	// Deleting frees the space at once.
	_, err = e.root.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(qb), Key: aws.String("a")})
	require.NoError(t, err)
	require.NoError(t, putSize("d", 600<<10), "the space of the deleted object is free")

	// A multipart upload whose parts fit one by one but not together is
	// refused at its completion.
	_, err = e.srv.Buckets.SetQuota(ctx, qb, bucket.Quota{Size: 8 << 20})
	require.NoError(t, err)
	up, err := e.root.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(qb), Key: aws.String("mpu")})
	require.NoError(t, err)
	var parts []types.CompletedPart
	for i, n := range []int{partSize, 3 << 20} {
		out, err := e.root.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String(qb), Key: aws.String("mpu"), UploadId: up.UploadId,
			PartNumber: aws.Int32(int32(i + 1)), Body: bytes.NewReader(make([]byte, n))})
		require.NoError(t, err, "part %d fits", i+1)
		parts = append(parts, types.CompletedPart{PartNumber: aws.Int32(int32(i + 1)), ETag: out.ETag})
	}
	_, err = e.root.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String(qb), Key: aws.String("mpu"), UploadId: up.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts}})
	requireQuota(err, "the completed object exceeds the quota")
	assert.False(t, exists("mpu"))
	// With more room the same upload completes.
	_, err = e.srv.Buckets.SetQuota(ctx, qb, bucket.Quota{Size: 16 << 20})
	require.NoError(t, err)
	_, err = e.root.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String(qb), Key: aws.String("mpu"), UploadId: up.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts}})
	require.NoError(t, err)
	assert.True(t, exists("mpu"))
}
