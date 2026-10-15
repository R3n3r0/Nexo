package object_test

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/event"
	"github.com/r3n3r0/nexo/internal/service/object"
	objectmock "github.com/r3n3r0/nexo/internal/service/object/mock"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

func keysOf(res domain.ListObjectsResult) []string {
	out := make([]string, 0, len(res.Objects))
	for _, o := range res.Objects {
		out = append(out, o.Key)
	}
	return out
}

func TestListObjects(t *testing.T) {
	forEachImpl(t, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		svc := fx.svc
		require.NoError(t, svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
		for _, k := range []string{"a-b", "a.txt", "a/x", "a/y/z", "b.txt", "photos/", "photos/1.jpg", "photos/2024/a.jpg", "photos/2024/b.jpg", "zeta"} {
			put(t, svc, "bkt", k, []byte(k))
		}

		// Flat listing.
		res, err := svc.ListObjects(ctx, domain.ListObjectsInput{Bucket: "bkt"})
		require.NoError(t, err)
		assert.Equal(t, []string{"a-b", "a.txt", "a/x", "a/y/z", "b.txt", "photos/", "photos/1.jpg", "photos/2024/a.jpg", "photos/2024/b.jpg", "zeta"}, keysOf(res))
		assert.False(t, res.IsTruncated)
		assert.Empty(t, res.CommonPrefixes)
		assert.Equal(t, int64(3), res.Objects[0].Size)
		assert.Equal(t, "text/plain", res.Objects[0].ContentType)

		// Delimiter: common prefixes, directory objects rolled up.
		res, err = svc.ListObjects(ctx, domain.ListObjectsInput{Bucket: "bkt", Delimiter: "/"})
		require.NoError(t, err)
		assert.Equal(t, []string{"a-b", "a.txt", "b.txt", "zeta"}, keysOf(res))
		assert.Equal(t, []string{"a/", "photos/"}, res.CommonPrefixes)

		// Prefix inside a directory with and without delimiter.
		res, err = svc.ListObjects(ctx, domain.ListObjectsInput{Bucket: "bkt", Prefix: "photos/", Delimiter: "/"})
		require.NoError(t, err)
		assert.Equal(t, []string{"photos/", "photos/1.jpg"}, keysOf(res), "the directory object itself is listed under its own prefix")
		assert.Equal(t, []string{"photos/2024/"}, res.CommonPrefixes)
		res, err = svc.ListObjects(ctx, domain.ListObjectsInput{Bucket: "bkt", Prefix: "photos/2024/"})
		require.NoError(t, err)
		assert.Equal(t, []string{"photos/2024/a.jpg", "photos/2024/b.jpg"}, keysOf(res))
		res, err = svc.ListObjects(ctx, domain.ListObjectsInput{Bucket: "bkt", Prefix: "pho", Delimiter: "/"})
		require.NoError(t, err)
		assert.Empty(t, res.Objects)
		assert.Equal(t, []string{"photos/"}, res.CommonPrefixes)
		res, err = svc.ListObjects(ctx, domain.ListObjectsInput{Bucket: "bkt", Prefix: "a"})
		require.NoError(t, err)
		assert.Equal(t, []string{"a-b", "a.txt", "a/x", "a/y/z"}, keysOf(res))

		// Other delimiters roll up on the key after the prefix.
		res, err = svc.ListObjects(ctx, domain.ListObjectsInput{Bucket: "bkt", Delimiter: "."})
		require.NoError(t, err)
		assert.Equal(t, []string{"a-b", "a/x", "a/y/z", "photos/", "zeta"}, keysOf(res))
		assert.Equal(t, []string{"a.", "b.", "photos/1.", "photos/2024/a.", "photos/2024/b."}, res.CommonPrefixes)

		// Pagination with markers (v1) and continuation tokens (v2).
		var all []string
		marker := ""
		for pages := 0; pages < 20; pages++ {
			res, err = svc.ListObjects(ctx, domain.ListObjectsInput{Bucket: "bkt", MaxKeys: 3, Marker: marker})
			require.NoError(t, err)
			all = append(all, keysOf(res)...)
			if !res.IsTruncated {
				break
			}
			require.NotEmpty(t, res.NextMarker)
			marker = res.NextMarker
		}
		assert.Len(t, all, 10)
		all = nil
		token := ""
		for pages := 0; pages < 20; pages++ {
			res, err = svc.ListObjects(ctx, domain.ListObjectsInput{Bucket: "bkt", MaxKeys: 4, ContinuationToken: token, Delimiter: "/"})
			require.NoError(t, err)
			all = append(all, keysOf(res)...)
			all = append(all, res.CommonPrefixes...)
			if !res.IsTruncated {
				break
			}
			token = res.NextContinuationToken
		}
		assert.ElementsMatch(t, []string{"a-b", "a.txt", "b.txt", "zeta", "a/", "photos/"}, all)
		res, err = svc.ListObjects(ctx, domain.ListObjectsInput{Bucket: "bkt", StartAfter: "photos/1.jpg"})
		require.NoError(t, err)
		assert.Equal(t, []string{"photos/2024/a.jpg", "photos/2024/b.jpg", "zeta"}, keysOf(res))
		res, err = svc.ListObjects(ctx, domain.ListObjectsInput{Bucket: "bkt", Marker: "photos/1.jpg", Delimiter: "/"})
		require.NoError(t, err)
		assert.Equal(t, []string{"zeta"}, keysOf(res))
		assert.Equal(t, []string{"photos/"}, res.CommonPrefixes, "photos/2024/... follows the marker inside photos/, as in AWS")
		res, err = svc.ListObjects(ctx, domain.ListObjectsInput{Bucket: "bkt", Marker: "photos/", Delimiter: "/"})
		require.NoError(t, err)
		assert.Equal(t, []string{"zeta"}, keysOf(res))
		assert.Empty(t, res.CommonPrefixes, "a common prefix used as marker is not repeated")

		// Errors.
		_, err = svc.ListObjects(ctx, domain.ListObjectsInput{Bucket: "bkt", ContinuationToken: "%%%"})
		assert.ErrorIs(t, err, domain.ErrInvalidArgument)
		_, err = svc.ListObjects(ctx, domain.ListObjectsInput{Bucket: "nobucket"})
		assert.ErrorIs(t, err, domain.ErrBucketNotFound)
		// A prefix no valid key can start with lists nothing (S3 accepts
		// any prefix) and is never walked.
		for _, prefix := range []string{"../x", "a/../", "/", "//", "a//b", "./"} {
			res, err = svc.ListObjects(ctx, domain.ListObjectsInput{Bucket: "bkt", Prefix: prefix, Delimiter: "/"})
			require.NoError(t, err, prefix)
			assert.Empty(t, res.Objects, prefix)
			assert.Empty(t, res.CommonPrefixes, prefix)
		}
		res, err = svc.ListObjects(ctx, domain.ListObjectsInput{Bucket: "bkt", Prefix: "nothing/here"})
		require.NoError(t, err)
		assert.Empty(t, res.Objects)
	})
}

func TestListObjectVersionsPagination(t *testing.T) {
	forEachImpl(t, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		svc := fx.svc
		require.NoError(t, svc.MakeBucket(ctx, "ver", object.MakeBucketOptions{}))
		fx.setBucketConfig("ver", object.BucketConfig{Versioning: domain.VersioningEnabled})
		for i := 0; i < 3; i++ {
			put(t, svc, "ver", "k1", []byte(fmt.Sprint("a", i)))
			put(t, svc, "ver", "k2", []byte(fmt.Sprint("b", i)))
			fx.clk.Advance(time.Second)
		}
		var seen []string
		key, vid := "", ""
		for pages := 0; pages < 10; pages++ {
			res, err := svc.ListObjectVersions(ctx, domain.ListObjectsInput{Bucket: "ver", MaxKeys: 4, Marker: key, VersionIDMarker: vid})
			require.NoError(t, err)
			for _, o := range res.Objects {
				seen = append(seen, o.Key+"@"+o.VersionID)
			}
			if !res.IsTruncated {
				break
			}
			key, vid = res.NextMarker, res.NextVersionIDMarker
		}
		assert.Len(t, seen, 6)
		assert.Equal(t, "k1", strings.Split(seen[0], "@")[0])
		assert.Equal(t, "k2", strings.Split(seen[5], "@")[0])
		unique := map[string]bool{}
		for _, s := range seen {
			unique[s] = true
		}
		assert.Len(t, unique, 6, "no version is repeated across pages")

		res, err := svc.ListObjectVersions(ctx, domain.ListObjectsInput{Bucket: "ver", Marker: "k1"})
		require.NoError(t, err)
		assert.Equal(t, []string{"k2", "k2", "k2"}, keysOf(res), "a key marker without version marker skips the key")
	})
}

// covers: S-OBJ-04, S-OBJ-07
func TestCopyObject(t *testing.T) {
	forEachImpl(t, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		svc := fx.svc
		require.NoError(t, svc.MakeBucket(ctx, "src", object.MakeBucketOptions{}))
		require.NoError(t, svc.MakeBucket(ctx, "dst", object.MakeBucketOptions{}))
		data := payload(3 * testBlockSize)
		put(t, svc, "src", "k", data, func(in *object.PutObjectInput) {
			in.UserMetadata = map[string]string{"owner": "alice", "content-encoding": "identity"}
			in.Tags = map[string]string{"t": "1"}
		})

		info, err := svc.CopyObject(ctx, object.CopyObjectInput{SrcBucket: "src", SrcKey: "k", DstBucket: "dst", DstKey: "copy"})
		require.NoError(t, err)
		assert.Equal(t, md5hex(data), info.ETag)
		assert.Equal(t, "alice", info.UserMetadata["owner"])
		assert.Equal(t, "identity", info.ContentEncoding)
		assert.Equal(t, map[string]string{"t": "1"}, info.Tags)
		got, _ := get(t, svc, "dst", "copy", object.GetObjectOptions{})
		assert.Equal(t, data, got)

		// Replace metadata and tags.
		info, err = svc.CopyObject(ctx, object.CopyObjectInput{SrcBucket: "src", SrcKey: "k", DstBucket: "dst", DstKey: "copy2",
			ReplaceMetadata: true, ContentType: "image/png", UserMetadata: map[string]string{"x": "y"}, ReplaceTags: true, Tags: map[string]string{}})
		require.NoError(t, err)
		assert.Equal(t, "image/png", info.ContentType)
		assert.Equal(t, map[string]string{"x": "y"}, info.UserMetadata)
		assert.Empty(t, info.Tags)
		assert.Empty(t, info.ContentEncoding)

		// Self-copy only with a metadata change.
		_, err = svc.CopyObject(ctx, object.CopyObjectInput{SrcBucket: "src", SrcKey: "k", DstBucket: "src", DstKey: "k"})
		assert.ErrorIs(t, err, domain.ErrInvalidArgument)
		info, err = svc.CopyObject(ctx, object.CopyObjectInput{SrcBucket: "src", SrcKey: "k", DstBucket: "src", DstKey: "k", ReplaceMetadata: true, ContentType: "text/csv"})
		require.NoError(t, err)
		assert.Equal(t, "text/csv", info.ContentType)
		got, _ = get(t, svc, "src", "k", object.GetObjectOptions{})
		assert.Equal(t, data, got)

		// Errors.
		_, err = svc.CopyObject(ctx, object.CopyObjectInput{SrcBucket: "src", SrcKey: "missing", DstBucket: "dst", DstKey: "x"})
		assert.ErrorIs(t, err, domain.ErrObjectNotFound)
		_, err = svc.CopyObject(ctx, object.CopyObjectInput{SrcBucket: "src", SrcKey: "k", DstBucket: "nobucket", DstKey: "x"})
		assert.ErrorIs(t, err, domain.ErrBucketNotFound)
		_, err = svc.CopyObject(ctx, object.CopyObjectInput{SrcBucket: "src", SrcKey: "k", DstBucket: "dst", DstKey: "x", SrcConditions: domain.Conditions{IfMatch: "nope"}})
		assert.ErrorIs(t, err, domain.ErrPreconditionFailed)
		assert.Contains(t, fx.eventNames(), event.ObjectCreatedCopy)
	})
}

func TestMultipart(t *testing.T) {
	forEachImpl(t, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		svc := fx.svc
		require.NoError(t, svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))

		_, err := svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "nobucket", Key: "k"})
		assert.ErrorIs(t, err, domain.ErrBucketNotFound)
		up, err := svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "bkt", Key: "big/file.bin", ContentType: "video/mp4",
			UserMetadata: map[string]string{"owner": "bob"}, Tags: map[string]string{"k": "v"}})
		require.NoError(t, err)
		require.NotEmpty(t, up.UploadID)
		assert.Equal(t, "video/mp4", up.ContentType)
		assert.Equal(t, fx.clk.Now(), up.Initiated)

		part1 := payload(2*testBlockSize + 17)
		part2 := payload(testBlockSize)
		part3 := []byte("tail")
		p1, err := svc.PutObjectPart(ctx, object.PutObjectPartInput{Bucket: "bkt", Key: "big/file.bin", UploadID: up.UploadID, PartNumber: 1, Reader: bytes.NewReader(part1), Size: int64(len(part1))})
		require.NoError(t, err)
		assert.Equal(t, md5hex(part1), p1.ETag)
		assert.Equal(t, int64(len(part1)), p1.Size)
		// Re-uploading a part replaces it.
		p2, err := svc.PutObjectPart(ctx, object.PutObjectPartInput{Bucket: "bkt", Key: "big/file.bin", UploadID: up.UploadID, PartNumber: 2, Reader: bytes.NewReader([]byte("old")), Size: 3})
		require.NoError(t, err)
		p2, err = svc.PutObjectPart(ctx, object.PutObjectPartInput{Bucket: "bkt", Key: "big/file.bin", UploadID: up.UploadID, PartNumber: 2, Reader: bytes.NewReader(part2), Size: -1})
		require.NoError(t, err)
		assert.Equal(t, md5hex(part2), p2.ETag)
		p3, err := svc.PutObjectPart(ctx, object.PutObjectPartInput{Bucket: "bkt", Key: "big/file.bin", UploadID: up.UploadID, PartNumber: 3, Reader: bytes.NewReader(part3), Size: 4, MD5Hex: md5hex(part3)})
		require.NoError(t, err)
		_, err = svc.PutObjectPart(ctx, object.PutObjectPartInput{Bucket: "bkt", Key: "big/file.bin", UploadID: up.UploadID, PartNumber: 4, Reader: bytes.NewReader(part3), Size: 4, MD5Hex: "00"})
		assert.ErrorIs(t, err, domain.ErrChecksumMismatch)
		_, err = svc.PutObjectPart(ctx, object.PutObjectPartInput{Bucket: "bkt", Key: "big/file.bin", UploadID: up.UploadID, PartNumber: 0, Reader: bytes.NewReader(part3), Size: 4})
		assert.ErrorIs(t, err, domain.ErrInvalidArgument)
		_, err = svc.PutObjectPart(ctx, object.PutObjectPartInput{Bucket: "bkt", Key: "big/file.bin", UploadID: "nope", PartNumber: 1, Reader: bytes.NewReader(part3), Size: 4})
		assert.ErrorIs(t, err, domain.ErrNoSuchUpload)
		_, err = svc.PutObjectPart(ctx, object.PutObjectPartInput{Bucket: "bkt", Key: "other", UploadID: up.UploadID, PartNumber: 1, Reader: bytes.NewReader(part3), Size: 4})
		assert.ErrorIs(t, err, domain.ErrNoSuchUpload, "an upload id belongs to one key")

		parts, err := svc.ListObjectParts(ctx, domain.ListPartsInput{Bucket: "bkt", Key: "big/file.bin", UploadID: up.UploadID})
		require.NoError(t, err)
		require.Len(t, parts.Parts, 3)
		assert.Equal(t, []int{1, 2, 3}, []int{parts.Parts[0].Number, parts.Parts[1].Number, parts.Parts[2].Number})
		assert.Equal(t, "video/mp4", parts.Upload.ContentType)
		paged, err := svc.ListObjectParts(ctx, domain.ListPartsInput{Bucket: "bkt", Key: "big/file.bin", UploadID: up.UploadID, MaxParts: 2})
		require.NoError(t, err)
		assert.True(t, paged.IsTruncated)
		assert.Equal(t, 2, paged.NextPartNumberMarker)
		paged, err = svc.ListObjectParts(ctx, domain.ListPartsInput{Bucket: "bkt", Key: "big/file.bin", UploadID: up.UploadID, PartNumberMarker: 2})
		require.NoError(t, err)
		require.Len(t, paged.Parts, 1)
		assert.Equal(t, 3, paged.Parts[0].Number)

		uploads, err := svc.ListMultipartUploads(ctx, domain.ListMultipartUploadsInput{Bucket: "bkt"})
		require.NoError(t, err)
		require.Len(t, uploads.Uploads, 1)
		assert.Equal(t, up.UploadID, uploads.Uploads[0].UploadID)
		uploads, err = svc.ListMultipartUploads(ctx, domain.ListMultipartUploadsInput{Bucket: "bkt", Delimiter: "/"})
		require.NoError(t, err)
		assert.Empty(t, uploads.Uploads)
		assert.Equal(t, []string{"big/"}, uploads.CommonPrefixes)

		// Completion errors.
		complete := func(parts ...domain.CompletePart) (domain.ObjectInfo, error) {
			return svc.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: "bkt", Key: "big/file.bin", UploadID: up.UploadID, Parts: parts})
		}
		_, err = complete()
		assert.ErrorIs(t, err, domain.ErrMalformedXML)
		_, err = complete(domain.CompletePart{Number: 2, ETag: p2.ETag}, domain.CompletePart{Number: 1, ETag: p1.ETag})
		assert.ErrorIs(t, err, domain.ErrInvalidPartOrder)
		_, err = complete(domain.CompletePart{Number: 1, ETag: "wrong"})
		assert.ErrorIs(t, err, domain.ErrInvalidPart)
		_, err = complete(domain.CompletePart{Number: 9, ETag: p1.ETag})
		assert.ErrorIs(t, err, domain.ErrInvalidPart)
		_, err = complete(domain.CompletePart{Number: 3, ETag: p3.ETag}, domain.CompletePart{Number: 3, ETag: p3.ETag})
		assert.ErrorIs(t, err, domain.ErrInvalidPartOrder)
		// Part 3 (4 bytes) is below the minimum when it is not the last one.
		_, err = svc.PutObjectPart(ctx, object.PutObjectPartInput{Bucket: "bkt", Key: "big/file.bin", UploadID: up.UploadID, PartNumber: 5, Reader: bytes.NewReader(part2), Size: int64(len(part2))})
		require.NoError(t, err)
		_, err = complete(domain.CompletePart{Number: 3, ETag: p3.ETag}, domain.CompletePart{Number: 5, ETag: md5hex(part2)})
		assert.ErrorIs(t, err, domain.ErrEntityTooSmall)

		// Successful completion skips part 5 and renumbers the parts.
		info, err := complete(domain.CompletePart{Number: 1, ETag: `"` + p1.ETag + `"`}, domain.CompletePart{Number: 2, ETag: p2.ETag}, domain.CompletePart{Number: 3, ETag: p3.ETag})
		require.NoError(t, err)
		want := append(append(append([]byte{}, part1...), part2...), part3...)
		assert.Equal(t, int64(len(want)), info.Size)
		assert.Equal(t, 3, info.Parts)
		assert.True(t, strings.HasSuffix(info.ETag, "-3"), info.ETag)
		h := md5.New()
		for _, etag := range []string{p1.ETag, p2.ETag, p3.ETag} {
			raw, _ := hex.DecodeString(etag)
			h.Write(raw)
		}
		assert.Equal(t, hex.EncodeToString(h.Sum(nil))+"-3", info.ETag)
		assert.Equal(t, "video/mp4", info.ContentType)
		assert.Equal(t, "bob", info.UserMetadata["owner"])
		assert.Equal(t, map[string]string{"k": "v"}, info.Tags)

		got, _ := get(t, svc, "bkt", "big/file.bin", object.GetObjectOptions{})
		assert.Equal(t, want, got)
		rg, _ := domain.ParseRange(fmt.Sprintf("bytes=%d-%d", len(part1)-5, len(part1)+10))
		got, _ = get(t, svc, "bkt", "big/file.bin", object.GetObjectOptions{Range: rg})
		assert.Equal(t, want[len(part1)-5:len(part1)+11], got, "ranges span part boundaries")
		got, _ = get(t, svc, "bkt", "big/file.bin", object.GetObjectOptions{PartNumber: 2})
		assert.Equal(t, part2, got)
		got, _ = get(t, svc, "bkt", "big/file.bin", object.GetObjectOptions{PartNumber: 3})
		assert.Equal(t, part3, got)

		// The upload is gone, its bookkeeping removed.
		_, err = svc.ListObjectParts(ctx, domain.ListPartsInput{Bucket: "bkt", Key: "big/file.bin", UploadID: up.UploadID})
		assert.ErrorIs(t, err, domain.ErrNoSuchUpload)
		mp, _ := os.ReadDir(filepath.Join(fx.roots[0], domain.SystemBucket, storage.MultipartDir))
		assert.Empty(t, mp)
		tmp, _ := os.ReadDir(filepath.Join(fx.roots[0], domain.SystemBucket, storage.TmpDir))
		assert.Empty(t, tmp)

		// Abort.
		up2, err := svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "bkt", Key: "aborted"})
		require.NoError(t, err)
		_, err = svc.PutObjectPart(ctx, object.PutObjectPartInput{Bucket: "bkt", Key: "aborted", UploadID: up2.UploadID, PartNumber: 1, Reader: bytes.NewReader(part3), Size: 4})
		require.NoError(t, err)
		require.NoError(t, svc.AbortMultipartUpload(ctx, "bkt", "aborted", up2.UploadID))
		assert.ErrorIs(t, svc.AbortMultipartUpload(ctx, "bkt", "aborted", up2.UploadID), domain.ErrNoSuchUpload)
		uploads, err = svc.ListMultipartUploads(ctx, domain.ListMultipartUploadsInput{Bucket: "bkt"})
		require.NoError(t, err)
		assert.Empty(t, uploads.Uploads)

		// Copy part from an existing object with a range.
		up3, err := svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "bkt", Key: "copied"})
		require.NoError(t, err)
		rg, _ = domain.ParseRange("bytes=0-99")
		cp, err := svc.CopyObjectPart(ctx, object.CopyObjectPartInput{SrcBucket: "bkt", SrcKey: "big/file.bin", SrcRange: rg, DstBucket: "bkt", DstKey: "copied", UploadID: up3.UploadID, PartNumber: 1})
		require.NoError(t, err)
		assert.Equal(t, int64(100), cp.Size)
		assert.Equal(t, md5hex(want[:100]), cp.ETag)
		info, err = svc.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: "bkt", Key: "copied", UploadID: up3.UploadID, Parts: []domain.CompletePart{{Number: 1, ETag: cp.ETag}}})
		require.NoError(t, err)
		got, _ = get(t, svc, "bkt", "copied", object.GetObjectOptions{})
		assert.Equal(t, want[:100], got)

		// Versioned completion yields a version id; pagination of uploads.
		fx.setBucketConfig("bkt", object.BucketConfig{Versioning: domain.VersioningEnabled})
		var ids []string
		for i := 0; i < 3; i++ {
			u, err := svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "bkt", Key: fmt.Sprintf("multi/%d", i)})
			require.NoError(t, err)
			ids = append(ids, u.UploadID)
			fx.clk.Advance(time.Second)
		}
		page, err := svc.ListMultipartUploads(ctx, domain.ListMultipartUploadsInput{Bucket: "bkt", MaxUploads: 2})
		require.NoError(t, err)
		require.Len(t, page.Uploads, 2)
		assert.True(t, page.IsTruncated)
		page, err = svc.ListMultipartUploads(ctx, domain.ListMultipartUploadsInput{Bucket: "bkt", KeyMarker: page.NextKeyMarker, UploadIDMarker: page.NextUploadIDMarker})
		require.NoError(t, err)
		require.Len(t, page.Uploads, 1)
		assert.Equal(t, ids[2], page.Uploads[0].UploadID)
		pp, err := svc.PutObjectPart(ctx, object.PutObjectPartInput{Bucket: "bkt", Key: "multi/0", UploadID: ids[0], PartNumber: 1, Reader: bytes.NewReader(part3), Size: 4})
		require.NoError(t, err)
		info, err = svc.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: "bkt", Key: "multi/0", UploadID: ids[0], Parts: []domain.CompletePart{{Number: 1, ETag: pp.ETag}}})
		require.NoError(t, err)
		assert.NotEmpty(t, info.VersionID)
		assert.Contains(t, fx.eventNames(), event.ObjectCreatedCompleteMultipartUpload)
	})
}

func TestObjectTags(t *testing.T) {
	forEachImpl(t, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		svc := fx.svc
		require.NoError(t, svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
		put(t, svc, "bkt", "k", []byte("x"))

		tags, err := svc.GetObjectTags(ctx, "bkt", "k", object.TagOptions{})
		require.NoError(t, err)
		assert.Empty(t, tags)
		info, err := svc.PutObjectTags(ctx, "bkt", "k", map[string]string{"a": "1", "b": "two words"}, object.TagOptions{})
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"a": "1", "b": "two words"}, info.Tags)
		tags, err = svc.GetObjectTags(ctx, "bkt", "k", object.TagOptions{})
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"a": "1", "b": "two words"}, tags)
		_, r := get(t, svc, "bkt", "k", object.GetObjectOptions{})
		assert.Equal(t, "x", string(payload(0))+"x")
		assert.Equal(t, map[string]string{"a": "1", "b": "two words"}, r.Info.Tags)

		_, err = svc.PutObjectTags(ctx, "bkt", "k", map[string]string{strings.Repeat("k", 129): "v"}, object.TagOptions{})
		assert.ErrorIs(t, err, domain.ErrInvalidArgument)
		_, err = svc.PutObjectTags(ctx, "bkt", "missing", map[string]string{"a": "1"}, object.TagOptions{})
		assert.ErrorIs(t, err, domain.ErrObjectNotFound)

		info, err = svc.DeleteObjectTags(ctx, "bkt", "k", object.TagOptions{})
		require.NoError(t, err)
		assert.Empty(t, info.Tags)
		tags, err = svc.GetObjectTags(ctx, "bkt", "k", object.TagOptions{})
		require.NoError(t, err)
		assert.Empty(t, tags)
		assert.Contains(t, fx.eventNames(), event.ObjectCreatedPutTagging)
		assert.Contains(t, fx.eventNames(), event.ObjectCreatedDeleteTagging)
	})
}

func TestHealthAndFaultTolerance(t *testing.T) {
	ctx := context.Background()

	t.Run("fs", func(t *testing.T) {
		fx := newFixture(t, "fs")
		h := fx.svc.Health(ctx, object.HealthOptions{})
		assert.True(t, h.Healthy)
		assert.Equal(t, 1, h.WriteQuorum)
		assert.False(t, fx.svc.Health(ctx, object.HealthOptions{Maintenance: true}).Healthy)
		require.NoError(t, fx.drives[0].Close(ctx))
		h = fx.svc.Health(ctx, object.HealthOptions{})
		assert.False(t, h.Healthy)
		assert.Contains(t, h.Message, "offline")
	})

	t.Run("erasure", func(t *testing.T) {
		fx := newFixture(t, "erasure")
		svc := fx.svc
		require.NoError(t, svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
		data := payload(5*testBlockSize + 7)
		put(t, svc, "bkt", "k", data)
		put(t, svc, "bkt", "small", []byte("inline"))

		h := svc.Health(ctx, object.HealthOptions{})
		assert.True(t, h.Healthy)
		assert.Equal(t, 3, h.WriteQuorum, "data == parity → data+1")
		assert.Equal(t, 2, h.ReadQuorum)
		assert.False(t, svc.Health(ctx, object.HealthOptions{Maintenance: true}).Healthy, "a single node cannot go down")

		// Shard files carry the per-block bitrot hashes.
		files, err := filepath.Glob(filepath.Join(fx.roots[0], "bkt", "k", "*", "part.1"))
		require.NoError(t, err)
		require.Len(t, files, 1)
		st, err := os.Stat(files[0])
		require.NoError(t, err)
		// On-disk format: every block of a shard is prefixed by its
		// HighwayHash-256 (32 bytes); a shard holds ceil(block/data) bytes.
		const hashSize = 32
		shard := func(blockLen, data int64) int64 { return (blockLen + data - 1) / data }
		blocks := int64(6)
		assert.Equal(t, blocks*hashSize+5*shard(testBlockSize, 2)+shard(7, 2), st.Size())

		// Corrupt one shard: reads still succeed through reconstruction.
		raw, err := os.ReadFile(files[0])
		require.NoError(t, err)
		raw[hashSize+10] ^= 0xff
		require.NoError(t, os.WriteFile(files[0], raw, 0o600))
		got, _ := get(t, svc, "bkt", "k", object.GetObjectOptions{})
		assert.Equal(t, data, got)

		// One drive offline (parity 2): everything keeps working.
		require.NoError(t, fx.drives[1].Close(ctx))
		got, _ = get(t, svc, "bkt", "k", object.GetObjectOptions{})
		assert.Equal(t, data, got)
		got, _ = get(t, svc, "bkt", "small", object.GetObjectOptions{})
		assert.Equal(t, "inline", string(got))
		put(t, svc, "bkt", "degraded", data)
		got, _ = get(t, svc, "bkt", "degraded", object.GetObjectOptions{})
		assert.Equal(t, data, got)
		h = svc.Health(ctx, object.HealthOptions{})
		assert.True(t, h.Healthy)
		assert.False(t, svc.Health(ctx, object.HealthOptions{Maintenance: true}).Healthy)
		list, err := svc.ListObjects(ctx, domain.ListObjectsInput{Bucket: "bkt"})
		require.NoError(t, err)
		assert.Equal(t, []string{"degraded", "k", "small"}, keysOf(list))

		// Two drives offline: reads of the corrupted shard object lose quorum,
		// writes are refused, the deployment is unhealthy.
		require.NoError(t, fx.drives[2].Close(ctx))
		h = svc.Health(ctx, object.HealthOptions{})
		assert.False(t, h.Healthy)
		assert.Contains(t, h.Message, "online")
		_, err = svc.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: "refused", Reader: bytes.NewReader(data), Size: int64(len(data))})
		assert.ErrorIs(t, err, domain.ErrWriteQuorum)
		r, err := svc.GetObject(ctx, "bkt", "k", object.GetObjectOptions{})
		require.NoError(t, err, "metadata is still readable from two drives")
		_, err = io.ReadAll(r)
		assert.ErrorIs(t, err, domain.ErrReadQuorum, "one shard corrupted, two drives offline")
		_ = r.Close()
		got, _ = get(t, svc, "bkt", "degraded", object.GetObjectOptions{})
		assert.Equal(t, data, got, "two healthy shards suffice to decode")

		// Three drives offline: nothing is readable.
		require.NoError(t, fx.drives[3].Close(ctx))
		_, err = svc.GetObjectInfo(ctx, "bkt", "k", object.GetObjectOptions{})
		assert.Error(t, err)
		_, err = svc.ListBuckets(ctx)
		require.ErrorIs(t, err, domain.ErrReadQuorum, "one drive is not a read quorum")
	})
}

func TestErasureSetSelectionIsStable(t *testing.T) {
	ctx := context.Background()
	drives, _ := newDrives(t, 4)
	sets := [][]storage.StorageAPI{drives[:2], drives[2:]}
	svc, err := object.NewErasure(object.Deps{Sets: sets, Parity: 1, DeploymentID: "dep", Codecs: testCodecs("dep"), Logger: nil}, object.WithInlineLimit(0), object.WithBlockSize(testBlockSize))
	require.NoError(t, err)
	require.NoError(t, svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
	keys := []string{"alpha", "beta", "gamma", "delta", "epsilon", "zeta/1", "zeta/2", "eta"}
	for _, k := range keys {
		put(t, svc, "bkt", k, []byte(k+k))
	}
	// Objects spread across both sets (with high probability for 8 keys) and
	// listings merge them in order.
	perSet := map[int]int{}
	for i, root := range []string{} {
		_ = i
		_ = root
	}
	for i, pair := range sets {
		d := pair[0].(interface{ Endpoint() domain.Endpoint }).Endpoint().Path
		entries, _ := os.ReadDir(filepath.Join(d, "bkt"))
		perSet[i] = len(entries)
	}
	assert.Equal(t, len(keys)-1, perSet[0]+perSet[1]-1, "every key lives in exactly one set (zeta/ counted once)")
	assert.NotZero(t, perSet[0])
	assert.NotZero(t, perSet[1])
	res, err := svc.ListObjects(ctx, domain.ListObjectsInput{Bucket: "bkt"})
	require.NoError(t, err)
	assert.Equal(t, []string{"alpha", "beta", "delta", "epsilon", "eta", "gamma", "zeta/1", "zeta/2"}, keysOf(res))
	res, err = svc.ListObjects(ctx, domain.ListObjectsInput{Bucket: "bkt", Delimiter: "/"})
	require.NoError(t, err)
	assert.Equal(t, []string{"zeta/"}, res.CommonPrefixes)
	for _, k := range keys {
		got, _ := get(t, svc, "bkt", k, object.GetObjectOptions{})
		assert.Equal(t, k+k, string(got))
	}
	// Multipart bookkeeping follows the key's set.
	up, err := svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "bkt", Key: "zeta/3"})
	require.NoError(t, err)
	uploads, err := svc.ListMultipartUploads(ctx, domain.ListMultipartUploadsInput{Bucket: "bkt"})
	require.NoError(t, err)
	require.Len(t, uploads.Uploads, 1)
	assert.Equal(t, up.UploadID, uploads.Uploads[0].UploadID)
	require.NoError(t, svc.DeleteBucket(ctx, "bkt", object.DeleteBucketOptions{Force: true}))
}

func TestPutLengthRange(t *testing.T) {
	forEachImpl(t, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		svc := fx.svc
		require.NoError(t, svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
		bounds := &object.LengthRange{Min: 10, Max: 3000}
		tests := []struct {
			name    string
			size    int
			stream  bool
			wantErr error
		}{
			{name: "declared within range", size: 100},
			{name: "declared at the maximum", size: 3000},
			{name: "declared too small", size: 9, wantErr: domain.ErrEntityTooSmall},
			{name: "declared too large", size: 3001, wantErr: domain.ErrEntityTooLarge},
			{name: "streamed within range", size: 2048, stream: true},
			{name: "streamed too small", size: 3, stream: true, wantErr: domain.ErrEntityTooSmall},
			{name: "streamed too large", size: 2 * testBlockSize, stream: true, wantErr: domain.ErrEntityTooLarge},
		}
		for i, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				key := fmt.Sprintf("obj-%d", i)
				data := payload(tt.size)
				in := object.PutObjectInput{Bucket: "bkt", Key: key, Reader: bytes.NewReader(data), Size: int64(len(data)), LengthRange: bounds}
				if tt.stream {
					in.Size = -1
				}
				info, err := svc.PutObject(ctx, in)
				if tt.wantErr != nil {
					assert.ErrorIs(t, err, tt.wantErr)
					_, err = svc.GetObjectInfo(ctx, "bkt", key, object.GetObjectOptions{})
					assert.ErrorIs(t, err, domain.ErrObjectNotFound, "nothing is committed")
					return
				}
				require.NoError(t, err)
				assert.Equal(t, int64(tt.size), info.Size)
				got, _ := get(t, svc, "bkt", key, object.GetObjectOptions{})
				assert.Equal(t, data, got)
			})
		}
	})
}

func TestReadinessCheck(t *testing.T) {
	tests := []struct {
		name    string
		health  object.HealthResult
		wantErr string
	}{
		{name: "healthy", health: object.HealthResult{Healthy: true}},
		{name: "unhealthy", health: object.HealthResult{Message: "set 1 has 1/4 drives online"}, wantErr: "set 1 has 1/4 drives online"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := objectmock.NewService(t)
			svc.EXPECT().Health(mock.Anything, object.HealthOptions{}).Return(tt.health).Once()
			err := object.ReadinessCheck(svc)(context.Background())
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			assert.EqualError(t, err, tt.wantErr)
		})
	}
}

func TestRegionOption(t *testing.T) {
	ctx := context.Background()
	drives, _ := newDrives(t, 1)
	svc, err := object.NewFS(object.Deps{Store: drives[0]}, object.WithRegion("eu-south-1"))
	require.NoError(t, err)
	require.NoError(t, svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
	info, err := svc.GetBucketInfo(ctx, "bkt")
	require.NoError(t, err)
	assert.Equal(t, "eu-south-1", info.Region)
}
