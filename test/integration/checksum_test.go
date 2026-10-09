package integration_test

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"hash"
	"hash/crc32"
	"hash/crc64"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// s3Checksum computes the base64 S3 checksum of data with an algorithm.
func s3Checksum(algo types.ChecksumAlgorithm, data []byte) string {
	var h hash.Hash
	switch algo {
	case types.ChecksumAlgorithmCrc32:
		h = crc32.NewIEEE()
	case types.ChecksumAlgorithmCrc32c:
		h = crc32.New(crc32.MakeTable(crc32.Castagnoli))
	case types.ChecksumAlgorithmCrc64nvme:
		h = crc64.New(crc64.MakeTable(0x9a6c9329ac4bc9b5))
	case types.ChecksumAlgorithmSha1:
		h = sha1.New()
	default:
		h = sha256.New()
	}
	_, _ = h.Write(data)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// checksumOf returns the checksum of an algorithm among the fields of an
// SDK output.
func checksumOf(algo types.ChecksumAlgorithm, crc32v, crc32c, crc64, sha1v, sha256v *string) string {
	switch algo {
	case types.ChecksumAlgorithmCrc32:
		return aws.ToString(crc32v)
	case types.ChecksumAlgorithmCrc32c:
		return aws.ToString(crc32c)
	case types.ChecksumAlgorithmCrc64nvme:
		return aws.ToString(crc64)
	case types.ChecksumAlgorithmSha1:
		return aws.ToString(sha1v)
	default:
		return aws.ToString(sha256v)
	}
}

// TestChecksums covers the additional checksums: every algorithm computed
// by the SDK on PutObject, verified (BadDigest), stored and returned with
// x-amz-checksum-mode (the SDK validates the content against it), the
// composite and full-object checksums of the multipart uploads, a
// trailing checksum of an aws-chunked body, and GetObjectAttributes.
//
// covers: S-OBJ-09, S-OBJ-15
func TestChecksums(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	const bucket = "sums"
	e.mkBucket(bucket)
	ctx := e.ctx
	data := bytes.Repeat([]byte("checksum data "), 1000)

	algos := []types.ChecksumAlgorithm{types.ChecksumAlgorithmCrc32, types.ChecksumAlgorithmCrc32c, types.ChecksumAlgorithmCrc64nvme,
		types.ChecksumAlgorithmSha1, types.ChecksumAlgorithmSha256}
	for _, algo := range algos {
		key := "single-" + string(algo)
		want := s3Checksum(algo, data)
		put, err := e.root.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(data),
			ChecksumAlgorithm: algo})
		require.NoError(t, err, algo)
		assert.Equal(t, want, checksumOf(algo, put.ChecksumCRC32, put.ChecksumCRC32C, put.ChecksumCRC64NVME, put.ChecksumSHA1, put.ChecksumSHA256), algo)

		get, err := e.root.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), ChecksumMode: types.ChecksumModeEnabled})
		require.NoError(t, err, algo)
		body, err := io.ReadAll(get.Body)
		_ = get.Body.Close()
		require.NoError(t, err, "the SDK validates the content against the checksum (%s)", algo)
		assert.Equal(t, data, body)
		assert.Equal(t, want, checksumOf(algo, get.ChecksumCRC32, get.ChecksumCRC32C, get.ChecksumCRC64NVME, get.ChecksumSHA1, get.ChecksumSHA256), algo)
		assert.Equal(t, types.ChecksumTypeFullObject, get.ChecksumType, algo)

		head, err := e.root.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), ChecksumMode: types.ChecksumModeEnabled})
		require.NoError(t, err)
		assert.Equal(t, want, checksumOf(algo, head.ChecksumCRC32, head.ChecksumCRC32C, head.ChecksumCRC64NVME, head.ChecksumSHA1, head.ChecksumSHA256), algo)
		head, err = e.root.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
		require.NoError(t, err)
		assert.Empty(t, checksumOf(algo, head.ChecksumCRC32, head.ChecksumCRC32C, head.ChecksumCRC64NVME, head.ChecksumSHA1, head.ChecksumSHA256),
			"without x-amz-checksum-mode no checksum is returned")

		// A declared checksum that does not match: BadDigest.
		resp := e.signedDo(http.MethodPut, "/"+bucket+"/bad-"+string(algo), e.rootCreds(), data,
			http.Header{"X-Amz-Checksum-" + string(algo): {s3Checksum(algo, []byte("other"))}})
		requireS3Error(t, resp, http.StatusBadRequest, "BadDigest")
	}
	// Two checksums at once are refused.
	resp := e.signedDo(http.MethodPut, "/"+bucket+"/two", e.rootCreds(), data, http.Header{
		"X-Amz-Checksum-Crc32": {s3Checksum(types.ChecksumAlgorithmCrc32, data)}, "X-Amz-Checksum-Sha1": {s3Checksum(types.ChecksumAlgorithmSha1, data)}})
	requireS3Error(t, resp, http.StatusBadRequest, "InvalidRequest")

	// A trailing checksum of an aws-chunked body (STREAMING-UNSIGNED-PAYLOAD-TRAILER).
	trailerSum := s3Checksum(types.ChecksumAlgorithmCrc32c, data)
	var chunked bytes.Buffer
	fmt.Fprintf(&chunked, "%x\r\n", len(data))
	chunked.Write(data)
	chunked.WriteString("\r\n0\r\nx-amz-checksum-crc32c:" + trailerSum + "\r\n\r\n")
	req := newRequest(t, http.MethodPut, e.srv.S3URL+"/"+bucket+"/trailer", chunked.Bytes())
	req.Header.Set("Content-Encoding", "aws-chunked")
	req.Header.Set("X-Amz-Decoded-Content-Length", strconv.Itoa(len(data)))
	req.Header.Set("X-Amz-Trailer", "x-amz-checksum-crc32c")
	req.Header.Set("X-Amz-Content-Sha256", "STREAMING-UNSIGNED-PAYLOAD-TRAILER")
	require.NoError(t, s3Signer().SignHTTP(ctx, e.rootCreds().aws(), req, "STREAMING-UNSIGNED-PAYLOAD-TRAILER", "s3", e.srv.Config.Region, time.Now()))
	resp = do(t, req)
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	head, err := e.root.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("trailer"), ChecksumMode: types.ChecksumModeEnabled})
	require.NoError(t, err)
	assert.Equal(t, trailerSum, aws.ToString(head.ChecksumCRC32C), "the trailing checksum is stored")
	assert.Equal(t, []byte(data), e.get(bucket, "trailer"), "the aws-chunked framing is not stored")
}

// TestMultipartChecksums covers the checksums of the multipart uploads:
// COMPOSITE (checksum of the part checksums, "-N") and FULL_OBJECT (the
// CRC of the whole content, validated by the SDK on GetObject), the part
// checksums in ListParts and GetObjectAttributes.
//
// covers: S-OBJ-09, S-OBJ-15
func TestMultipartChecksums(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	const bucket = "mpsums"
	e.mkBucket(bucket)
	ctx := e.ctx
	parts := [][]byte{bytes.Repeat([]byte("p1"), 5<<19), bytes.Repeat([]byte("p2"), 5<<19), []byte("the last part")}
	whole := bytes.Join(parts, nil)

	upload := func(key string, algo types.ChecksumAlgorithm, typ types.ChecksumType) *s3.CompleteMultipartUploadOutput {
		t.Helper()
		up, err := e.root.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String(key),
			ChecksumAlgorithm: algo, ChecksumType: typ})
		require.NoError(t, err)
		assert.Equal(t, algo, up.ChecksumAlgorithm)
		var completed []types.CompletedPart
		for i, p := range parts {
			out, err := e.root.UploadPart(ctx, &s3.UploadPartInput{Bucket: aws.String(bucket), Key: aws.String(key), UploadId: up.UploadId,
				PartNumber: aws.Int32(int32(i + 1)), Body: bytes.NewReader(p), ChecksumAlgorithm: algo})
			require.NoError(t, err)
			sum := checksumOf(algo, out.ChecksumCRC32, out.ChecksumCRC32C, out.ChecksumCRC64NVME, out.ChecksumSHA1, out.ChecksumSHA256)
			assert.Equal(t, s3Checksum(algo, p), sum, "part %d", i+1)
			cp := types.CompletedPart{PartNumber: aws.Int32(int32(i + 1)), ETag: out.ETag}
			switch algo {
			case types.ChecksumAlgorithmCrc32:
				cp.ChecksumCRC32 = aws.String(sum)
			case types.ChecksumAlgorithmCrc64nvme:
				cp.ChecksumCRC64NVME = aws.String(sum)
			default:
				cp.ChecksumSHA256 = aws.String(sum)
			}
			completed = append(completed, cp)
		}
		lp, err := e.root.ListParts(ctx, &s3.ListPartsInput{Bucket: aws.String(bucket), Key: aws.String(key), UploadId: up.UploadId})
		require.NoError(t, err)
		assert.Equal(t, algo, lp.ChecksumAlgorithm)
		require.Len(t, lp.Parts, 3)
		done, err := e.root.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String(key),
			UploadId: up.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: completed}})
		require.NoError(t, err)
		return done
	}

	// COMPOSITE: the checksum of the part checksums, with "-3".
	done := upload("composite", types.ChecksumAlgorithmSha256, "")
	var raw []byte
	for _, p := range parts {
		b, _ := base64.StdEncoding.DecodeString(s3Checksum(types.ChecksumAlgorithmSha256, p))
		raw = append(raw, b...)
	}
	composite := s3Checksum(types.ChecksumAlgorithmSha256, raw) + "-3"
	assert.Equal(t, composite, aws.ToString(done.ChecksumSHA256))
	assert.Equal(t, types.ChecksumTypeComposite, done.ChecksumType)
	head, err := e.root.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("composite"), ChecksumMode: types.ChecksumModeEnabled})
	require.NoError(t, err)
	assert.Equal(t, composite, aws.ToString(head.ChecksumSHA256))
	assert.Equal(t, types.ChecksumTypeComposite, head.ChecksumType)
	// The read of one part reports the checksum of the part.
	head, err = e.root.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("composite"), PartNumber: aws.Int32(2),
		ChecksumMode: types.ChecksumModeEnabled})
	require.NoError(t, err)
	assert.Equal(t, s3Checksum(types.ChecksumAlgorithmSha256, parts[1]), aws.ToString(head.ChecksumSHA256))

	// FULL_OBJECT: the CRC of the whole content, which the SDK checks.
	for _, tc := range []struct {
		algo types.ChecksumAlgorithm
		typ  types.ChecksumType
	}{{types.ChecksumAlgorithmCrc64nvme, ""}, {types.ChecksumAlgorithmCrc32, types.ChecksumTypeFullObject}} {
		key := "full-" + string(tc.algo)
		done := upload(key, tc.algo, tc.typ)
		want := s3Checksum(tc.algo, whole)
		assert.Equal(t, want, checksumOf(tc.algo, done.ChecksumCRC32, done.ChecksumCRC32C, done.ChecksumCRC64NVME, done.ChecksumSHA1, done.ChecksumSHA256))
		assert.Equal(t, types.ChecksumTypeFullObject, done.ChecksumType)
		get, err := e.root.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), ChecksumMode: types.ChecksumModeEnabled})
		require.NoError(t, err)
		body, err := io.ReadAll(get.Body)
		_ = get.Body.Close()
		require.NoError(t, err, "the SDK validates the full-object %s", tc.algo)
		assert.Equal(t, whole, body)
	}
	// FULL_OBJECT needs a CRC.
	_, err = e.root.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String("x"),
		ChecksumAlgorithm: types.ChecksumAlgorithmSha1, ChecksumType: types.ChecksumTypeFullObject})
	assert.Equal(t, "InvalidRequest", apiCode(err))

	// GetObjectAttributes: every attribute, parts paginated.
	attrs, err := e.root.GetObjectAttributes(ctx, &s3.GetObjectAttributesInput{Bucket: aws.String(bucket), Key: aws.String("composite"),
		ObjectAttributes: []types.ObjectAttributes{types.ObjectAttributesEtag, types.ObjectAttributesChecksum, types.ObjectAttributesObjectParts,
			types.ObjectAttributesStorageClass, types.ObjectAttributesObjectSize},
		MaxParts: aws.Int32(2)})
	require.NoError(t, err)
	assert.Equal(t, strings.Trim(aws.ToString(head.ETag), `"`), strings.Trim(aws.ToString(attrs.ETag), `"`))
	assert.Equal(t, int64(len(whole)), aws.ToInt64(attrs.ObjectSize))
	assert.Equal(t, types.StorageClassStandard, attrs.StorageClass)
	require.NotNil(t, attrs.Checksum)
	assert.Equal(t, composite, aws.ToString(attrs.Checksum.ChecksumSHA256))
	assert.Equal(t, types.ChecksumTypeComposite, attrs.Checksum.ChecksumType)
	require.NotNil(t, attrs.ObjectParts)
	assert.Equal(t, int32(3), aws.ToInt32(attrs.ObjectParts.TotalPartsCount))
	assert.True(t, aws.ToBool(attrs.ObjectParts.IsTruncated))
	assert.Equal(t, "2", aws.ToString(attrs.ObjectParts.NextPartNumberMarker))
	require.Len(t, attrs.ObjectParts.Parts, 2)
	assert.Equal(t, int64(len(parts[0])), aws.ToInt64(attrs.ObjectParts.Parts[0].Size))
	assert.Equal(t, s3Checksum(types.ChecksumAlgorithmSha256, parts[0]), aws.ToString(attrs.ObjectParts.Parts[0].ChecksumSHA256))
	rest, err := e.root.GetObjectAttributes(ctx, &s3.GetObjectAttributesInput{Bucket: aws.String(bucket), Key: aws.String("composite"),
		ObjectAttributes: []types.ObjectAttributes{types.ObjectAttributesObjectParts}, PartNumberMarker: aws.String("2")})
	require.NoError(t, err)
	require.Len(t, rest.ObjectParts.Parts, 1)
	assert.Equal(t, int32(3), aws.ToInt32(rest.ObjectParts.Parts[0].PartNumber))
	assert.Nil(t, rest.Checksum, "only the requested attributes")

	single, err := e.root.GetObjectAttributes(ctx, &s3.GetObjectAttributesInput{Bucket: aws.String(bucket), Key: aws.String("full-" + string(types.ChecksumAlgorithmCrc32)),
		ObjectAttributes: []types.ObjectAttributes{types.ObjectAttributesChecksum}})
	require.NoError(t, err)
	assert.Equal(t, s3Checksum(types.ChecksumAlgorithmCrc32, whole), aws.ToString(single.Checksum.ChecksumCRC32))
	_, err = e.root.GetObjectAttributes(ctx, &s3.GetObjectAttributesInput{Bucket: aws.String(bucket), Key: aws.String("missing"),
		ObjectAttributes: []types.ObjectAttributes{types.ObjectAttributesEtag}})
	assert.Equal(t, http.StatusNotFound, httpStatus(err))
}
