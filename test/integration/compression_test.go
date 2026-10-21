package integration_test

import (
	"bytes"
	"crypto/md5"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// textData returns n bytes of numbered lines, different in every block.
func textData(n int) []byte {
	var b bytes.Buffer
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "%09d the quick brown fox %d jumps over the lazy dog %d\n", i, i*7%13, i*11%17)
	}
	return b.Bytes()[:n]
}

// TestTransparentCompression (S-OBJ-14): with compression enabled, text
// objects round trip through the S3 API whole and by range, single part
// and multipart, with the size and ETag of their content.
//
// covers: S-OBJ-14
func TestTransparentCompression(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := e.ctx
	_, err := adminClient(t, e).SetConfig(ctx, "compression", map[string]string{"enable": "on"})
	require.NoError(t, err)
	const bucket = "compressed"
	e.mkBucket(bucket)

	data := textData(2<<20 + 333)
	put, err := e.root.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String("app.log"), Body: bytes.NewReader(data), ContentType: aws.String("text/plain")})
	require.NoError(t, err)
	assert.Equal(t, fmt.Sprintf(`"%x"`, md5.Sum(data)), aws.ToString(put.ETag), "the ETag is the MD5 of the content")
	head, err := e.root.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("app.log")})
	require.NoError(t, err)
	assert.Equal(t, int64(len(data)), aws.ToInt64(head.ContentLength))
	got, err := e.root.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("app.log")})
	require.NoError(t, err)
	require.True(t, bytes.Equal(data, readAll(t, got)))
	for _, rg := range [][2]int{{0, 0}, {1<<20 - 2, 1<<20 + 2}, {1 << 20, 2<<20 - 1}, {len(data) - 10, len(data) - 1}} {
		got, err := e.root.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("app.log"), Range: aws.String(fmt.Sprintf("bytes=%d-%d", rg[0], rg[1]))})
		require.NoError(t, err)
		assert.Equal(t, data[rg[0]:rg[1]+1], readAll(t, got), "range %v", rg)
		assert.Equal(t, fmt.Sprintf("bytes %d-%d/%d", rg[0], rg[1], len(data)), aws.ToString(got.ContentRange))
	}
	list, err := e.root.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	require.Len(t, list.Contents, 1)
	assert.Equal(t, int64(len(data)), aws.ToInt64(list.Contents[0].Size))

	up, err := e.root.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String("mpu.csv"), ContentType: aws.String("text/csv")})
	require.NoError(t, err)
	part1, part2 := textData(partSize), textData(300<<10)
	var parts []types.CompletedPart
	for i, p := range [][]byte{part1, part2} {
		out, err := e.root.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String(bucket), Key: aws.String("mpu.csv"), UploadId: up.UploadId,
			PartNumber: aws.Int32(int32(i + 1)), Body: bytes.NewReader(p)})
		require.NoError(t, err)
		parts = append(parts, types.CompletedPart{PartNumber: aws.Int32(int32(i + 1)), ETag: out.ETag})
	}
	_, err = e.root.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String("mpu.csv"), UploadId: up.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts}})
	require.NoError(t, err)
	whole := append(append([]byte(nil), part1...), part2...)
	got, err = e.root.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("mpu.csv")})
	require.NoError(t, err)
	require.True(t, bytes.Equal(whole, readAll(t, got)))
	got, err = e.root.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("mpu.csv"), Range: aws.String(fmt.Sprintf("bytes=%d-%d", partSize-5, partSize+5))})
	require.NoError(t, err)
	assert.Equal(t, whole[partSize-5:partSize+6], readAll(t, got), "a range across the parts")
	got, err = e.root.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("mpu.csv"), PartNumber: aws.Int32(2)})
	require.NoError(t, err)
	assert.Equal(t, part2, readAll(t, got))
}
