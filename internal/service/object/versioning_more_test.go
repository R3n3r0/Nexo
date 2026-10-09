package object_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"hash/crc32"
	"hash/crc64"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
)

func TestDeleteMarkerReadDetails(t *testing.T) {
	forEachImpl(t, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		require.NoError(t, fx.svc.MakeBucket(ctx, "ver", object.MakeBucketOptions{}))
		fx.setBucketConfig("ver", object.BucketConfig{Versioning: domain.VersioningEnabled})
		v1 := put(t, fx.svc, "ver", "doc", []byte("v1"))
		del, err := fx.svc.DeleteObject(ctx, "ver", "doc", object.DeleteObjectOptions{})
		require.NoError(t, err)
		require.True(t, del.DeleteMarker)

		_, err = fx.svc.GetObjectInfo(ctx, "ver", "doc", object.GetObjectOptions{})
		require.ErrorIs(t, err, domain.ErrObjectNotFound)
		var marker *domain.DeleteMarkerError
		require.ErrorAs(t, err, &marker)
		assert.Equal(t, del.DeleteMarkerVersionID, marker.VersionID)
		assert.False(t, marker.ModTime.IsZero())
		assert.Equal(t, "NoSuchKey", s3Code(err))

		_, err = fx.svc.GetObject(ctx, "ver", "doc", object.GetObjectOptions{VersionID: del.DeleteMarkerVersionID})
		require.ErrorIs(t, err, domain.ErrMethodNotAllowed)
		require.ErrorAs(t, err, &marker)
		assert.Equal(t, del.DeleteMarkerVersionID, marker.VersionID)

		// Copies: a marker version cannot be a source, a key whose latest
		// version is a marker does not exist.
		_, err = fx.svc.CopyObject(ctx, object.CopyObjectInput{SrcBucket: "ver", SrcKey: "doc", SrcVersionID: del.DeleteMarkerVersionID, DstBucket: "ver", DstKey: "c"})
		assert.Equal(t, "InvalidRequest", s3Code(err))
		_, err = fx.svc.CopyObject(ctx, object.CopyObjectInput{SrcBucket: "ver", SrcKey: "doc", DstBucket: "ver", DstKey: "c"})
		assert.ErrorIs(t, err, domain.ErrObjectNotFound)
		assert.False(t, errors.As(err, &marker), "a copy reports no delete marker")
		// The version before the marker stays copyable.
		cp, err := fx.svc.CopyObject(ctx, object.CopyObjectInput{SrcBucket: "ver", SrcKey: "doc", SrcVersionID: v1.VersionID, DstBucket: "ver", DstKey: "c"})
		require.NoError(t, err)
		assert.Equal(t, v1.ETag, cp.ETag)

		// Suspended: the marker is the null version.
		fx.setBucketConfig("ver", object.BucketConfig{Versioning: domain.VersioningSuspended})
		del, err = fx.svc.DeleteObject(ctx, "ver", "c", object.DeleteObjectOptions{})
		require.NoError(t, err)
		assert.Equal(t, "null", del.DeleteMarkerVersionID)
		_, err = fx.svc.GetObjectInfo(ctx, "ver", "c", object.GetObjectOptions{})
		require.ErrorAs(t, err, &marker)
		assert.Equal(t, "null", marker.VersionID)
	})
}

// crc32Of and crc64Of are the S3 checksums of data.
func crc32Of(data []byte, table *crc32.Table) string {
	h := crc32.New(table)
	_, _ = h.Write(data)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func crc64NVMEOf(data []byte) string {
	h := crc64.New(crc64.MakeTable(0x9a6c9329ac4bc9b5))
	_, _ = h.Write(data)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func TestChecksumAlgorithmsAndTypes(t *testing.T) {
	forEachImpl(t, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		require.NoError(t, fx.svc.MakeBucket(ctx, "sums", object.MakeBucketOptions{}))
		data := payload(3000)
		info := put(t, fx.svc, "sums", "nvme", data, func(in *object.PutObjectInput) {
			in.Checksum = domain.Checksum{Algorithm: domain.ChecksumCRC64NVME, Value: crc64NVMEOf(data)}
		})
		assert.Equal(t, domain.Checksum{Algorithm: domain.ChecksumCRC64NVME, Value: crc64NVMEOf(data), Type: domain.ChecksumTypeFullObject}, info.Checksum)
		_, err := fx.svc.PutObject(ctx, object.PutObjectInput{Bucket: "sums", Key: "bad", Reader: bytes.NewReader(data), Size: int64(len(data)),
			Checksum: domain.Checksum{Algorithm: domain.ChecksumCRC64NVME, Value: crc64NVMEOf(data[1:])}})
		assert.ErrorIs(t, err, domain.ErrBadDigest)

		// A FULL_OBJECT multipart upload stores the CRC of the whole content.
		parts := [][]byte{payload(40), []byte("this is the middle part!"), []byte("end")}
		up, err := fx.svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "sums", Key: "full", Checksum: domain.ChecksumCRC32C,
			ChecksumType: domain.ChecksumTypeFullObject})
		require.NoError(t, err)
		assert.Equal(t, domain.ChecksumTypeFullObject, up.ChecksumType)
		_, err = fx.svc.PutObjectPart(ctx, object.PutObjectPartInput{Bucket: "sums", Key: "full", UploadID: up.UploadID, PartNumber: 1,
			Reader: bytes.NewReader(parts[0]), Size: int64(len(parts[0])), Checksum: domain.Checksum{Algorithm: domain.ChecksumSHA1}})
		assert.Equal(t, "InvalidRequest", s3Code(err), "a part checksum of another algorithm")
		var complete []domain.CompletePart
		var whole []byte
		for i, p := range parts {
			part := uploadPart(t, fx.svc, "sums", "full", up.UploadID, i+1, p)
			complete = append(complete, domain.CompletePart{Number: i + 1, ETag: part.ETag})
			whole = append(whole, p...)
		}
		_, err = fx.svc.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: "sums", Key: "full", UploadID: up.UploadID,
			Parts: complete, ChecksumType: domain.ChecksumTypeComposite})
		assert.Equal(t, "InvalidRequest", s3Code(err), "the checksum type of the upload is kept")
		done, err := fx.svc.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: "sums", Key: "full", UploadID: up.UploadID,
			Parts: complete, Checksum: domain.Checksum{Algorithm: domain.ChecksumCRC32C, Value: crc32Of(whole, crc32.MakeTable(crc32.Castagnoli))}})
		require.NoError(t, err)
		assert.Equal(t, domain.Checksum{Algorithm: domain.ChecksumCRC32C, Value: crc32Of(whole, crc32.MakeTable(crc32.Castagnoli)), Type: domain.ChecksumTypeFullObject}, done.Checksum)

		// The read of one part reports its checksum.
		head, err := fx.svc.GetObjectInfo(ctx, "sums", "full", object.GetObjectOptions{PartNumber: 2})
		require.NoError(t, err)
		assert.Equal(t, domain.Checksum{Algorithm: domain.ChecksumCRC32C, Value: crc32Of(parts[1], crc32.MakeTable(crc32.Castagnoli))}, head.Checksum)

		// Copies recompute the checksum, in the source algorithm or the
		// requested one.
		cp, err := fx.svc.CopyObject(ctx, object.CopyObjectInput{SrcBucket: "sums", SrcKey: "nvme", DstBucket: "sums", DstKey: "copy"})
		require.NoError(t, err)
		assert.Equal(t, info.Checksum, cp.Checksum)
		cp, err = fx.svc.CopyObject(ctx, object.CopyObjectInput{SrcBucket: "sums", SrcKey: "nvme", DstBucket: "sums", DstKey: "copy",
			ChecksumAlgorithm: domain.ChecksumCRC32})
		require.NoError(t, err)
		assert.Equal(t, crc32Of(data, crc32.IEEETable), cp.Checksum.Value)

		// Invalid combinations of algorithm and type.
		_, err = fx.svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "sums", Key: "x", Checksum: domain.ChecksumSHA256, ChecksumType: domain.ChecksumTypeFullObject})
		assert.Equal(t, "InvalidRequest", s3Code(err))
	})
}

func TestGetObjectAttributes(t *testing.T) {
	forEachImpl(t, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		require.NoError(t, fx.svc.MakeBucket(ctx, "attrs", object.MakeBucketOptions{}))
		single := put(t, fx.svc, "attrs", "single", []byte("hello"))
		attrs, err := fx.svc.GetObjectAttributes(ctx, "attrs", "single", object.AttributesOptions{})
		require.NoError(t, err)
		assert.Equal(t, single.ETag, attrs.Info.ETag)
		assert.Equal(t, int64(5), attrs.Info.Size)
		assert.Zero(t, attrs.PartsCount)
		assert.Empty(t, attrs.Parts)

		up, err := fx.svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "attrs", Key: "mp", Checksum: domain.ChecksumCRC32})
		require.NoError(t, err)
		bodies := [][]byte{payload(20), payload(21), []byte("x")}
		var complete []domain.CompletePart
		for i, b := range bodies {
			p := uploadPart(t, fx.svc, "attrs", "mp", up.UploadID, i+1, b)
			complete = append(complete, domain.CompletePart{Number: i + 1, ETag: p.ETag})
		}
		_, err = fx.svc.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: "attrs", Key: "mp", UploadID: up.UploadID, Parts: complete})
		require.NoError(t, err)

		page, err := fx.svc.GetObjectAttributes(ctx, "attrs", "mp", object.AttributesOptions{MaxParts: 2})
		require.NoError(t, err)
		assert.Equal(t, 3, page.PartsCount)
		require.Len(t, page.Parts, 2)
		assert.True(t, page.IsTruncated)
		assert.Equal(t, 2, page.NextPartNumberMarker)
		assert.Equal(t, domain.ObjectPart{Number: 1, Size: 20, Checksum: domain.Checksum{Algorithm: domain.ChecksumCRC32, Value: crc32Of(bodies[0], crc32.IEEETable)}}, page.Parts[0])
		assert.Equal(t, domain.ChecksumTypeComposite, page.Info.Checksum.Type)

		rest, err := fx.svc.GetObjectAttributes(ctx, "attrs", "mp", object.AttributesOptions{PartNumberMarker: 2})
		require.NoError(t, err)
		require.Len(t, rest.Parts, 1)
		assert.Equal(t, 3, rest.Parts[0].Number)
		assert.Equal(t, int64(1), rest.Parts[0].Size)
		assert.False(t, rest.IsTruncated)

		none, err := fx.svc.GetObjectAttributes(ctx, "attrs", "mp", object.AttributesOptions{MaxParts: domain.ListLimitNone})
		require.NoError(t, err)
		assert.Empty(t, none.Parts)
		assert.False(t, none.IsTruncated)

		_, err = fx.svc.GetObjectAttributes(ctx, "attrs", "missing", object.AttributesOptions{})
		assert.ErrorIs(t, err, domain.ErrObjectNotFound)
	})
}
