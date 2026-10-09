package object_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"hash/crc32"
	"hash/crc64"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/event"
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

		// The read of one part reports its checksum, with the checksum
		// type of the object.
		head, err := fx.svc.GetObjectInfo(ctx, "sums", "full", object.GetObjectOptions{PartNumber: 2})
		require.NoError(t, err)
		assert.Equal(t, domain.Checksum{Algorithm: domain.ChecksumCRC32C, Value: crc32Of(parts[1], crc32.MakeTable(crc32.Castagnoli)),
			Type: domain.ChecksumTypeFullObject}, head.Checksum)

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
			complete = append(complete, domain.CompletePart{Number: i + 1, ETag: p.ETag, Checksum: p.Checksum})
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

// TestNullVersionWrittenBeforeVersioning: a version written while the
// bucket had no versioning is the null version of the bucket once
// versioning is enabled (or suspended): every read reports it as "null",
// it is addressed as "null", and deleting "null" removes it for good. An
// unversioned bucket keeps reporting it without id.
func TestNullVersionWrittenBeforeVersioning(t *testing.T) {
	for _, state := range []domain.Versioning{domain.VersioningEnabled, domain.VersioningSuspended} {
		t.Run(state.State(), func(t *testing.T) {
			forEachImpl(t, func(t *testing.T, fx *fixture) {
				ctx := context.Background()
				require.NoError(t, fx.svc.MakeBucket(ctx, "late", object.MakeBucketOptions{}))
				pre := put(t, fx.svc, "late", "dir/pre", []byte("pre"))
				assert.Empty(t, pre.VersionID, "an unversioned bucket has no version ids")
				res, err := fx.svc.ListObjectVersions(ctx, domain.ListObjectsInput{Bucket: "late"})
				require.NoError(t, err)
				require.Len(t, res.Objects, 1)
				assert.Empty(t, res.Objects[0].VersionID)

				fx.setBucketConfig("late", object.BucketConfig{Versioning: state})
				for _, vid := range []string{"", "null"} {
					info, err := fx.svc.GetObjectInfo(ctx, "late", "dir/pre", object.GetObjectOptions{VersionID: vid})
					require.NoError(t, err)
					assert.Equal(t, "null", info.VersionID, "head ?versionId=%q", vid)
					r, err := fx.svc.GetObject(ctx, "late", "dir/pre", object.GetObjectOptions{VersionID: vid})
					require.NoError(t, err)
					assert.Equal(t, "null", r.Info.VersionID, "get ?versionId=%q", vid)
					require.NoError(t, r.Close())
					attrs, err := fx.svc.GetObjectAttributes(ctx, "late", "dir/pre", object.AttributesOptions{VersionID: vid})
					require.NoError(t, err)
					assert.Equal(t, "null", attrs.Info.VersionID, "attributes ?versionId=%q", vid)
					tagged, err := fx.svc.PutObjectTags(ctx, "late", "dir/pre", map[string]string{"k": "v"}, object.TagOptions{VersionID: vid})
					require.NoError(t, err)
					assert.Equal(t, "null", tagged.VersionID, "tagging ?versionId=%q", vid)
				}
				list, err := fx.svc.ListObjects(ctx, domain.ListObjectsInput{Bucket: "late"})
				require.NoError(t, err)
				require.Len(t, list.Objects, 1)
				assert.Equal(t, "null", list.Objects[0].VersionID)

				// A newer version: the null version is noncurrent, still
				// "null", and deleting "null" removes it without a marker.
				fx.setBucketConfig("late", object.BucketConfig{Versioning: domain.VersioningEnabled})
				cur := put(t, fx.svc, "late", "dir/pre", []byte("cur"))
				require.NotEmpty(t, cur.VersionID)
				res, err = fx.svc.ListObjectVersions(ctx, domain.ListObjectsInput{Bucket: "late"})
				require.NoError(t, err)
				require.Len(t, res.Objects, 2)
				assert.Equal(t, cur.VersionID, res.Objects[0].VersionID)
				assert.Equal(t, "null", res.Objects[1].VersionID)
				assert.False(t, res.Objects[1].IsLatest)
				del, err := fx.svc.DeleteObject(ctx, "late", "dir/pre", object.DeleteObjectOptions{VersionID: "null"})
				require.NoError(t, err)
				assert.False(t, del.DeleteMarker)
				res, err = fx.svc.ListObjectVersions(ctx, domain.ListObjectsInput{Bucket: "late"})
				require.NoError(t, err)
				require.Len(t, res.Objects, 1)
				assert.Equal(t, cur.VersionID, res.Objects[0].VersionID)
			})
		})
	}
	t.Run("unversioned: deleting null removes the object", func(t *testing.T) {
		forEachImpl(t, func(t *testing.T, fx *fixture) {
			ctx := context.Background()
			require.NoError(t, fx.svc.MakeBucket(ctx, "plain", object.MakeBucketOptions{}))
			put(t, fx.svc, "plain", "k", []byte("x"))
			_, err := fx.svc.DeleteObject(ctx, "plain", "k", object.DeleteObjectOptions{VersionID: "null"})
			require.NoError(t, err)
			res, err := fx.svc.ListObjectVersions(ctx, domain.ListObjectsInput{Bucket: "plain"})
			require.NoError(t, err)
			assert.Empty(t, res.Objects)
		})
	})
}

// TestDeleteObjectIfModTime: a conditional delete (the lifecycle's) only
// removes, or hides under a marker, the version it names when it still
// has the modification time given; a version written meanwhile makes it
// fail with ErrPreconditionFailed and nothing changes.
func TestDeleteObjectIfModTime(t *testing.T) {
	tests := []struct {
		name       string
		versioning domain.Versioning
		// target is the version id of the deletion ("" for the latest).
		target string
	}{
		{name: "unversioned latest", versioning: domain.VersioningOff},
		{name: "unversioned null", versioning: domain.VersioningOff, target: "null"},
		{name: "enabled marker", versioning: domain.VersioningEnabled},
		{name: "suspended marker", versioning: domain.VersioningSuspended},
		{name: "suspended null", versioning: domain.VersioningSuspended, target: "null"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			forEachImpl(t, func(t *testing.T, fx *fixture) {
				ctx := context.Background()
				require.NoError(t, fx.svc.MakeBucket(ctx, "cond", object.MakeBucketOptions{}))
				fx.setBucketConfig("cond", object.BucketConfig{Versioning: tc.versioning})
				old := put(t, fx.svc, "cond", "k", []byte("old"))
				cur := put(t, fx.svc, "cond", "k", []byte("new"))
				require.False(t, old.ModTime.Equal(cur.ModTime))
				before, err := fx.svc.ListObjectVersions(ctx, domain.ListObjectsInput{Bucket: "cond"})
				require.NoError(t, err)

				_, err = fx.svc.DeleteObject(ctx, "cond", "k", object.DeleteObjectOptions{VersionID: tc.target, IfModTime: old.ModTime})
				require.ErrorIs(t, err, domain.ErrPreconditionFailed, "the version evaluated was replaced")
				_, err = fx.svc.DeleteObject(ctx, "cond", "missing", object.DeleteObjectOptions{VersionID: tc.target, IfModTime: old.ModTime})
				require.ErrorIs(t, err, domain.ErrPreconditionFailed, "a missing version")
				after, err := fx.svc.ListObjectVersions(ctx, domain.ListObjectsInput{Bucket: "cond"})
				require.NoError(t, err)
				assert.Equal(t, before.Objects, after.Objects, "nothing changed")

				_, err = fx.svc.DeleteObject(ctx, "cond", "k", object.DeleteObjectOptions{VersionID: tc.target, IfModTime: cur.ModTime})
				require.NoError(t, err)
				_, err = fx.svc.GetObjectInfo(ctx, "cond", "k", object.GetObjectOptions{})
				assert.ErrorIs(t, err, domain.ErrObjectNotFound, "the version evaluated is gone or hidden")
			})
		})
	}
	t.Run("enabled: a noncurrent version by id", func(t *testing.T) {
		forEachImpl(t, func(t *testing.T, fx *fixture) {
			ctx := context.Background()
			require.NoError(t, fx.svc.MakeBucket(ctx, "cond", object.MakeBucketOptions{}))
			fx.setBucketConfig("cond", object.BucketConfig{Versioning: domain.VersioningEnabled})
			v1 := put(t, fx.svc, "cond", "k", []byte("v1"))
			put(t, fx.svc, "cond", "k", []byte("v2"))
			_, err := fx.svc.DeleteObject(ctx, "cond", "k", object.DeleteObjectOptions{VersionID: v1.VersionID, IfModTime: v1.ModTime.Add(time.Nanosecond)})
			require.ErrorIs(t, err, domain.ErrPreconditionFailed)
			_, err = fx.svc.DeleteObject(ctx, "cond", "k", object.DeleteObjectOptions{VersionID: v1.VersionID, IfModTime: v1.ModTime})
			require.NoError(t, err)
			_, err = fx.svc.GetObjectInfo(ctx, "cond", "k", object.GetObjectOptions{VersionID: v1.VersionID})
			assert.ErrorIs(t, err, domain.ErrNoSuchVersion)
		})
	})
}

// TestEventSequencerFollowsCommits: the sequencer of the events of a key
// follows the order of its writes and deletions even when the clock of
// the node that publishes goes back (clock skew between the nodes of a
// cluster): it is the commit time of the version, monotonic per key, not
// the clock of the publisher.
func TestEventSequencerFollowsCommits(t *testing.T) {
	for _, state := range []domain.Versioning{domain.VersioningOff, domain.VersioningEnabled} {
		t.Run(state.State(), func(t *testing.T) {
			forEachImpl(t, func(t *testing.T, fx *fixture) {
				ctx := context.Background()
				require.NoError(t, fx.svc.MakeBucket(ctx, "seq", object.MakeBucketOptions{}))
				fx.setBucketConfig("seq", object.BucketConfig{Versioning: state})
				start := fx.clk.Now()
				put(t, fx.svc, "seq", "k", []byte("first"))
				// The next writes run on a node whose clock is an hour behind.
				fx.clk.Set(start.Add(-time.Hour))
				put(t, fx.svc, "seq", "k", []byte("second"))
				_, err := fx.svc.PutObjectTags(ctx, "seq", "k", map[string]string{"a": "b"}, object.TagOptions{})
				require.NoError(t, err)
				_, err = fx.svc.DeleteObject(ctx, "seq", "k", object.DeleteObjectOptions{})
				require.NoError(t, err)

				fx.mu.Lock()
				var seqs []int64
				var names []event.Name
				for _, e := range fx.published {
					if e.S3.Object.Key != "k" {
						continue
					}
					n, err := strconv.ParseInt(e.S3.Object.Sequencer, 16, 64)
					require.NoError(t, err)
					seqs = append(seqs, n)
					names = append(names, e.EventName)
				}
				fx.mu.Unlock()
				require.Len(t, seqs, 4, "%v", names)
				for i := 1; i < len(seqs); i++ {
					assert.Greater(t, seqs[i], seqs[i-1], "%s after %s", names[i], names[i-1])
				}
			})
		})
	}
}
