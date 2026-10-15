package integration_test

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const partSize = 5 << 20

// multipartETag is the S3 ETag of an object assembled from parts.
func multipartETag(parts ...[]byte) string {
	var sums []byte
	for _, p := range parts {
		s := md5.Sum(p)
		sums = append(sums, s[:]...)
	}
	all := md5.Sum(sums)
	return fmt.Sprintf("%s-%d", hex.EncodeToString(all[:]), len(parts))
}

// TS-I-06: multipart uploads: parts out of order, repeated part, a small
// non-final part, abort, paginated ListParts, UploadPartCopy.
//
// covers: S-OBJ-06, S-OBJ-07
func TestMultipart(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	const bucket = "mpu"
	e.mkBucket(bucket)
	ctx := e.ctx
	create := func(key string) string {
		out, err := e.root.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String(key),
			ContentType: aws.String("application/x-parts"), Metadata: map[string]string{"origin": "mpu"}})
		require.NoError(t, err)
		require.NotEmpty(t, aws.ToString(out.UploadId))
		return aws.ToString(out.UploadId)
	}
	upload := func(key, id string, n int32, data []byte) string {
		out, err := e.root.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String(bucket), Key: aws.String(key), UploadId: aws.String(id),
			PartNumber: aws.Int32(n), Body: bytes.NewReader(data)})
		require.NoError(t, err, "part %d", n)
		return aws.ToString(out.ETag)
	}
	complete := func(key, id string, parts ...types.CompletedPart) (*s3.CompleteMultipartUploadOutput, error) {
		return e.root.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String(key), UploadId: aws.String(id),
			MultipartUpload: &types.CompletedMultipartUpload{Parts: parts}})
	}
	part := func(n int32, etag string) types.CompletedPart {
		return types.CompletedPart{PartNumber: aws.Int32(n), ETag: aws.String(etag)}
	}

	p1, p2, p3 := pseudoRandom(11, partSize), pseudoRandom(12, partSize), pseudoRandom(13, 1234)

	t.Run("out of order upload, repeated part", func(t *testing.T) {
		id := create("ordered")
		e3 := upload("ordered", id, 3, p3)
		upload("ordered", id, 1, pseudoRandom(99, partSize)) // replaced below
		e2 := upload("ordered", id, 2, p2)
		e1 := upload("ordered", id, 1, p1)

		// The parts must be listed in order in the completion request.
		_, err := complete("ordered", id, part(2, e2), part(1, e1), part(3, e3))
		assert.Equal(t, "InvalidPartOrder", apiCode(err))
		assert.Equal(t, http.StatusBadRequest, httpStatus(err))
		// The ETag must be the one of the last upload of the part.
		_, err = complete("ordered", id, part(1, `"`+md5Hex(pseudoRandom(99, partSize))+`"`), part(2, e2), part(3, e3))
		assert.Equal(t, "InvalidPart", apiCode(err))
		// Unknown part.
		_, err = complete("ordered", id, part(1, e1), part(4, e3))
		assert.Equal(t, "InvalidPart", apiCode(err))

		done, err := complete("ordered", id, part(1, e1), part(2, e2), part(3, e3))
		require.NoError(t, err)
		assert.Equal(t, `"`+multipartETag(p1, p2, p3)+`"`, aws.ToString(done.ETag))
		got := e.get(bucket, "ordered")
		assert.True(t, bytes.Equal(append(append(append([]byte(nil), p1...), p2...), p3...), got))
		head, err := e.root.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("ordered")})
		require.NoError(t, err)
		assert.Equal(t, "application/x-parts", aws.ToString(head.ContentType))
		assert.Equal(t, "mpu", head.Metadata["origin"])
		// Part reads.
		pr, err := e.root.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("ordered"), PartNumber: aws.Int32(3)})
		require.NoError(t, err)
		assert.Equal(t, int64(len(p3)), aws.ToInt64(pr.ContentLength))
		assert.Equal(t, int32(3), aws.ToInt32(pr.PartsCount))
		_ = pr.Body.Close()
		// The upload is gone.
		_, err = e.root.ListParts(ctx, &s3.ListPartsInput{Bucket: aws.String(bucket), Key: aws.String("ordered"), UploadId: aws.String(id)})
		assert.Equal(t, "NoSuchUpload", apiCode(err))
	})

	t.Run("small part not last", func(t *testing.T) {
		id := create("small")
		e1 := upload("small", id, 1, p3) // 1234 bytes
		e2 := upload("small", id, 2, p3)
		_, err := complete("small", id, part(1, e1), part(2, e2))
		assert.Equal(t, "EntityTooSmall", apiCode(err))
		assert.Equal(t, http.StatusBadRequest, httpStatus(err))
		// A single small part is fine (it is the last).
		done, err := complete("small", id, part(1, e1))
		require.NoError(t, err)
		assert.Equal(t, `"`+multipartETag(p3)+`"`, aws.ToString(done.ETag))
	})

	t.Run("abort", func(t *testing.T) {
		id := create("aborted")
		upload("aborted", id, 1, p3)
		uploads, err := e.root.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: aws.String(bucket)})
		require.NoError(t, err)
		found := false
		for _, u := range uploads.Uploads {
			found = found || aws.ToString(u.UploadId) == id
		}
		assert.True(t, found, "listed while in progress")
		_, err = e.root.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String("aborted"), UploadId: aws.String(id)})
		require.NoError(t, err)
		_, err = e.root.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String(bucket), Key: aws.String("aborted"), UploadId: aws.String(id),
			PartNumber: aws.Int32(2), Body: bytes.NewReader(p3)})
		assert.Equal(t, "NoSuchUpload", apiCode(err))
		_, err = complete("aborted", id, part(1, "x"))
		assert.Equal(t, "NoSuchUpload", apiCode(err))
		_, err = e.root.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String("aborted"), UploadId: aws.String(id)})
		assert.Equal(t, "NoSuchUpload", apiCode(err))
		_, err = e.root.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("aborted")})
		assert.Equal(t, http.StatusNotFound, httpStatus(err))
	})

	t.Run("list parts paginated", func(t *testing.T) {
		id := create("listed")
		var etags []string
		for n := int32(1); n <= 7; n++ {
			etags = append(etags, upload("listed", id, n, []byte(strings.Repeat(fmt.Sprint(n), 100))))
		}
		var got []int32
		var marker *string
		pages := 0
		for {
			out, err := e.root.ListParts(ctx, &s3.ListPartsInput{Bucket: aws.String(bucket), Key: aws.String("listed"), UploadId: aws.String(id),
				MaxParts: aws.Int32(2), PartNumberMarker: marker})
			require.NoError(t, err)
			pages++
			for _, p := range out.Parts {
				got = append(got, aws.ToInt32(p.PartNumber))
				assert.Equal(t, etags[aws.ToInt32(p.PartNumber)-1], aws.ToString(p.ETag))
				assert.Equal(t, int64(100), aws.ToInt64(p.Size))
			}
			if !aws.ToBool(out.IsTruncated) {
				break
			}
			marker = out.NextPartNumberMarker
		}
		assert.Equal(t, []int32{1, 2, 3, 4, 5, 6, 7}, got)
		assert.Equal(t, 4, pages)
		// Max parts 1.
		out, err := e.root.ListParts(ctx, &s3.ListPartsInput{Bucket: aws.String(bucket), Key: aws.String("listed"), UploadId: aws.String(id),
			MaxParts: aws.Int32(1), PartNumberMarker: aws.String("6")})
		require.NoError(t, err)
		require.Len(t, out.Parts, 1)
		assert.Equal(t, int32(7), aws.ToInt32(out.Parts[0].PartNumber))
		assert.False(t, aws.ToBool(out.IsTruncated))
		_, err = e.root.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String("listed"), UploadId: aws.String(id)})
		require.NoError(t, err)
	})

	t.Run("upload part copy", func(t *testing.T) {
		src := pseudoRandom(21, partSize+4096)
		e.put(bucket, "source", src)
		id := create("copied")
		c1, err := e.root.UploadPartCopy(ctx, &s3.UploadPartCopyInput{Bucket: aws.String(bucket), Key: aws.String("copied"), UploadId: aws.String(id),
			PartNumber: aws.Int32(1), CopySource: aws.String(bucket + "/source"), CopySourceRange: aws.String(fmt.Sprintf("bytes=0-%d", partSize-1))})
		require.NoError(t, err)
		// A range beyond the source is refused.
		_, err = e.root.UploadPartCopy(ctx, &s3.UploadPartCopyInput{Bucket: aws.String(bucket), Key: aws.String("copied"), UploadId: aws.String(id),
			PartNumber: aws.Int32(2), CopySource: aws.String(bucket + "/source"), CopySourceRange: aws.String(fmt.Sprintf("bytes=0-%d", len(src)))})
		assert.Equal(t, "InvalidRange", apiCode(err))
		// The whole source (no range) as the last part.
		c2, err := e.root.UploadPartCopy(ctx, &s3.UploadPartCopyInput{Bucket: aws.String(bucket), Key: aws.String("copied"), UploadId: aws.String(id),
			PartNumber: aws.Int32(2), CopySource: aws.String(bucket + "/source")})
		require.NoError(t, err)
		done, err := complete("copied", id, part(1, aws.ToString(c1.CopyPartResult.ETag)), part(2, aws.ToString(c2.CopyPartResult.ETag)))
		require.NoError(t, err)
		assert.Equal(t, `"`+multipartETag(src[:partSize], src)+`"`, aws.ToString(done.ETag))
		assert.True(t, bytes.Equal(append(append([]byte(nil), src[:partSize]...), src...), e.get(bucket, "copied")))
	})

	t.Run("invalid part numbers", func(t *testing.T) {
		id := create("numbers")
		for _, n := range []string{"0", "10001", "x"} {
			resp := e.signedDo(http.MethodPut, "/"+bucket+"/numbers?partNumber="+n+"&uploadId="+id, e.rootCreds(), []byte("x"), nil)
			requireS3Error(t, resp, http.StatusBadRequest, "InvalidArgument")
		}
		resp := e.signedDo(http.MethodPut, "/"+bucket+"/numbers?partNumber=1&uploadId=bogus-upload", e.rootCreds(), []byte("x"), nil)
		requireS3Error(t, resp, http.StatusNotFound, "NoSuchUpload")
	})
}
