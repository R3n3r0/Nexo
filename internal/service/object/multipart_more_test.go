package object_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

func uploadPart(t *testing.T, svc object.Service, bucket, key, uploadID string, n int, data []byte) domain.Part {
	t.Helper()
	p, err := svc.PutObjectPart(context.Background(), object.PutObjectPartInput{Bucket: bucket, Key: key, UploadID: uploadID, PartNumber: n, Reader: bytes.NewReader(data), Size: int64(len(data))})
	require.NoError(t, err, "part %d", n)
	return p
}

// Upload ids are base64url(uuid || initiation time) and the bookkeeping
// lives in multipart/<bucket>/<uploadID>/.
func TestUploadIDAndLayout(t *testing.T) {
	fx := newFixture(t, "fs")
	ctx := context.Background()
	require.NoError(t, fx.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
	up, err := fx.svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "bkt", Key: "a/b.bin"})
	require.NoError(t, err)
	raw, err := base64.RawURLEncoding.DecodeString(up.UploadID)
	require.NoError(t, err)
	require.Len(t, raw, 24)
	assert.Equal(t, fx.clk.Now().UnixNano(), int64(binary.BigEndian.Uint64(raw[16:])), "the id carries the initiation time")

	dir := filepath.Join(fx.roots[0], domain.SystemBucket, storage.MultipartDir, "bkt", up.UploadID)
	uploadPart(t, fx.svc, "bkt", "a/b.bin", up.UploadID, 1, []byte("one"))
	uploadPart(t, fx.svc, "bkt", "a/b.bin", up.UploadID, 1, []byte("again"))
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.Contains(t, names, "upload.json")
	assert.Contains(t, names, "part.1.meta")
	assert.Len(t, names, 3, "the replaced part data is removed: %v", names)

	require.NoError(t, fx.svc.AbortMultipartUpload(ctx, "bkt", "a/b.bin", up.UploadID))
	_, err = os.Stat(dir)
	assert.True(t, os.IsNotExist(err))
	for _, bad := range []string{"", "../x", "a/b", strings.Repeat("a", 65)} {
		assert.ErrorIs(t, fx.svc.AbortMultipartUpload(ctx, "bkt", "a/b.bin", bad), domain.ErrNoSuchUpload, bad)
	}
	assert.ErrorIs(t, fx.svc.AbortMultipartUpload(ctx, "nobucket", "a/b.bin", up.UploadID), domain.ErrBucketNotFound)
}

// TS-I-06: parts out of order, repeated parts, small non-final parts,
// paginated listings and part copies.
func TestMultipartS3Rules(t *testing.T) {
	forEachImpl(t, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		svc := fx.svc
		require.NoError(t, svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
		up, err := svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "bkt", Key: "k"})
		require.NoError(t, err)

		// Parts uploaded out of order and one of them twice.
		bodies := map[int][]byte{}
		for _, n := range []int{3, 1, 5, 2, 4, 2} {
			bodies[n] = bytes.Repeat([]byte{byte('0' + n)}, 20+n)
			if n == 2 {
				bodies[n] = append(bodies[n], byte(len(bodies)))
			}
			uploadPart(t, svc, "bkt", "k", up.UploadID, n, bodies[n])
		}
		var listed []domain.Part
		marker := 0
		for pages := 0; pages < 10; pages++ {
			res, err := svc.ListObjectParts(ctx, domain.ListPartsInput{Bucket: "bkt", Key: "k", UploadID: up.UploadID, MaxParts: 2, PartNumberMarker: marker})
			require.NoError(t, err)
			listed = append(listed, res.Parts...)
			if !res.IsTruncated {
				break
			}
			marker = res.NextPartNumberMarker
		}
		require.Len(t, listed, 5)
		for i, p := range listed {
			assert.Equal(t, i+1, p.Number)
			assert.Equal(t, md5hex(bodies[i+1]), p.ETag, "the latest upload of a part wins")
		}

		// A non-final part below the minimum (16 bytes in these tests).
		uploadPart(t, svc, "bkt", "k", up.UploadID, 6, []byte("tiny"))
		_, err = svc.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: "bkt", Key: "k", UploadID: up.UploadID,
			Parts: []domain.CompletePart{{Number: 6, ETag: md5hex([]byte("tiny"))}, {Number: 1, ETag: listed[0].ETag}}})
		assert.ErrorIs(t, err, domain.ErrInvalidPartOrder, "order is checked first")
		_, err = svc.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: "bkt", Key: "k", UploadID: up.UploadID,
			Parts: []domain.CompletePart{{Number: 5, ETag: listed[4].ETag}, {Number: 6, ETag: md5hex([]byte("tiny"))}}})
		require.NoError(t, err, "a small last part is fine")
		_, err = svc.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: "bkt", Key: "k", UploadID: up.UploadID,
			Parts: []domain.CompletePart{{Number: 1, ETag: listed[0].ETag}}})
		assert.ErrorIs(t, err, domain.ErrNoSuchUpload, "a completed upload is gone")

		// Too many parts, bad part numbers.
		up2, err := svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "bkt", Key: "k2"})
		require.NoError(t, err)
		_, err = svc.PutObjectPart(ctx, object.PutObjectPartInput{Bucket: "bkt", Key: "k2", UploadID: up2.UploadID, PartNumber: 10001, Reader: strings.NewReader("x"), Size: 1})
		assert.ErrorIs(t, err, domain.ErrInvalidArgument)
		_, err = svc.PutObjectPart(ctx, object.PutObjectPartInput{Bucket: "bkt", Key: "k2", UploadID: up2.UploadID, PartNumber: 1, Reader: strings.NewReader("x"), Size: 5})
		assert.ErrorIs(t, err, domain.ErrInvalidArgument, "short body")
		_, err = svc.PutObjectPart(ctx, object.PutObjectPartInput{Bucket: "bkt", Key: "k2", UploadID: up2.UploadID, PartNumber: 1, Reader: strings.NewReader("x"), Size: -7})
		assert.ErrorIs(t, err, domain.ErrInvalidArgument, "invalid size")
		many := make([]domain.CompletePart, 10001)
		for i := range many {
			many[i] = domain.CompletePart{Number: i + 1}
		}
		_, err = svc.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: "bkt", Key: "k2", UploadID: up2.UploadID, Parts: many})
		assert.ErrorIs(t, err, domain.ErrMalformedXML)

		// UploadPartCopy: ranges within the source, source conditions.
		src := payload(3 * testBlockSize)
		srcInfo := put(t, svc, "bkt", "src", src)
		for _, tc := range []struct {
			spec string
			ok   bool
		}{
			{"bytes=0-0", true},
			{"bytes=100-5000", true},
			{fmt.Sprintf("bytes=0-%d", len(src)-1), true},
			{fmt.Sprintf("bytes=0-%d", len(src)), false},
			{"bytes=100-", false},
			{"bytes=-10", false},
		} {
			rg, err := domain.ParseRange(tc.spec)
			require.NoError(t, err)
			p, err := svc.CopyObjectPart(ctx, object.CopyObjectPartInput{SrcBucket: "bkt", SrcKey: "src", SrcRange: rg, DstBucket: "bkt", DstKey: "k2", UploadID: up2.UploadID, PartNumber: 1})
			if !tc.ok {
				assert.ErrorIs(t, err, domain.ErrInvalidRange, tc.spec)
				continue
			}
			require.NoError(t, err, tc.spec)
			off, n, _ := rg.Resolve(int64(len(src)))
			assert.Equal(t, md5hex(src[off:off+n]), p.ETag, tc.spec)
		}
		_, err = svc.CopyObjectPart(ctx, object.CopyObjectPartInput{SrcBucket: "bkt", SrcKey: "src", DstBucket: "bkt", DstKey: "k2", UploadID: up2.UploadID, PartNumber: 2,
			SrcConditions: domain.Conditions{IfNoneMatch: srcInfo.ETag}})
		assert.ErrorIs(t, err, domain.ErrPreconditionFailed, "a copy source condition never yields 304")
		_, err = svc.CopyObjectPart(ctx, object.CopyObjectPartInput{SrcBucket: "bkt", SrcKey: "missing", DstBucket: "bkt", DstKey: "k2", UploadID: up2.UploadID, PartNumber: 2})
		assert.ErrorIs(t, err, domain.ErrObjectNotFound)
		_, err = svc.CopyObjectPart(ctx, object.CopyObjectPartInput{SrcBucket: "bkt", SrcKey: "src", DstBucket: "bkt", DstKey: "k2", UploadID: "AAAA", PartNumber: 2})
		assert.ErrorIs(t, err, domain.ErrNoSuchUpload)
		p, err := svc.CopyObjectPart(ctx, object.CopyObjectPartInput{SrcBucket: "bkt", SrcKey: "src", DstBucket: "bkt", DstKey: "k2", UploadID: up2.UploadID, PartNumber: 2})
		require.NoError(t, err)
		assert.Equal(t, srcInfo.ETag, p.ETag, "a copy without range takes the whole source")
		require.NoError(t, svc.AbortMultipartUpload(ctx, "bkt", "k2", up2.UploadID))
		assertNoTmpLeft(t, fx)
	})
}

// ListMultipartUploads orders uploads by key then initiation time and
// resumes after key-marker / upload-id-marker.
func TestListMultipartUploadsMarkers(t *testing.T) {
	forEachImpl(t, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		svc := fx.svc
		require.NoError(t, svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
		type ref struct{ key, id string }
		var want []ref
		for _, k := range []string{"b", "a", "b", "c/x", "c/y", "a"} {
			up, err := svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "bkt", Key: k})
			require.NoError(t, err)
			want = append(want, ref{k, up.UploadID})
			fx.clk.Advance(time.Second)
		}
		// Expected order: by key, then by initiation.
		ordered := []ref{want[1], want[5], want[0], want[2], want[3], want[4]}

		for _, size := range []int{1, 2, 4, 1000} {
			t.Run(fmt.Sprint("page ", size), func(t *testing.T) {
				var got []ref
				keyMarker, idMarker := "", ""
				for pages := 0; pages < 20; pages++ {
					res, err := svc.ListMultipartUploads(ctx, domain.ListMultipartUploadsInput{Bucket: "bkt", MaxUploads: size, KeyMarker: keyMarker, UploadIDMarker: idMarker})
					require.NoError(t, err)
					for _, u := range res.Uploads {
						got = append(got, ref{u.Key, u.UploadID})
					}
					if !res.IsTruncated {
						assert.Empty(t, res.NextKeyMarker)
						break
					}
					keyMarker, idMarker = res.NextKeyMarker, res.NextUploadIDMarker
				}
				assert.Equal(t, ordered, got)
			})
		}

		res, err := svc.ListMultipartUploads(ctx, domain.ListMultipartUploadsInput{Bucket: "bkt", KeyMarker: "a"})
		require.NoError(t, err)
		require.Len(t, res.Uploads, 4, "a key-marker alone skips the whole key")
		res, err = svc.ListMultipartUploads(ctx, domain.ListMultipartUploadsInput{Bucket: "bkt", KeyMarker: "a", UploadIDMarker: "garbage"})
		require.NoError(t, err)
		assert.Len(t, res.Uploads, 4, "an unknown upload-id-marker skips the key too")
		res, err = svc.ListMultipartUploads(ctx, domain.ListMultipartUploadsInput{Bucket: "bkt", UploadIDMarker: want[0].id})
		require.NoError(t, err)
		assert.Len(t, res.Uploads, 6, "an upload-id-marker without key-marker is ignored")
		res, err = svc.ListMultipartUploads(ctx, domain.ListMultipartUploadsInput{Bucket: "bkt", Prefix: "c/", Delimiter: "/"})
		require.NoError(t, err)
		assert.Len(t, res.Uploads, 2)
		res, err = svc.ListMultipartUploads(ctx, domain.ListMultipartUploadsInput{Bucket: "bkt", Delimiter: "/", MaxUploads: 3})
		require.NoError(t, err)
		assert.Len(t, res.Uploads, 3)
		assert.True(t, res.IsTruncated)
		res, err = svc.ListMultipartUploads(ctx, domain.ListMultipartUploadsInput{Bucket: "bkt", Delimiter: "/", KeyMarker: res.NextKeyMarker, UploadIDMarker: res.NextUploadIDMarker})
		require.NoError(t, err)
		assert.Len(t, res.Uploads, 1)
		assert.Equal(t, []string{"c/"}, res.CommonPrefixes)

		_, err = svc.ListMultipartUploads(ctx, domain.ListMultipartUploadsInput{Bucket: "nobucket"})
		assert.ErrorIs(t, err, domain.ErrBucketNotFound)
		res, err = svc.ListMultipartUploads(ctx, domain.ListMultipartUploadsInput{Bucket: "bkt", Prefix: "../"})
		require.NoError(t, err, "the prefix only filters the keys")
		assert.Empty(t, res.Uploads)
	})
}

// TestListMultipartUploadsMarkerInsidePrefix is a regression test: with a
// delimiter, a common prefix holding the key-marker ("a/" with marker
// "a/x") is listed when uploads after the marker roll up into it, as in
// ListObjects; the prefix equal to the marker (the previous page's
// NextKeyMarker) is not repeated.
func TestListMultipartUploadsMarkerInsidePrefix(t *testing.T) {
	forEachImpl(t, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		svc := fx.svc
		require.NoError(t, svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
		for _, k := range []string{"a/w", "a/x", "a/y", "b/z", "c"} {
			_, err := svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "bkt", Key: k})
			require.NoError(t, err)
		}
		tests := []struct {
			name      string
			keyMarker string
			prefixes  []string
			uploads   []string
		}{
			{name: "no marker", prefixes: []string{"a/", "b/"}, uploads: []string{"c"}},
			{name: "marker inside prefix", keyMarker: "a/x", prefixes: []string{"a/", "b/"}, uploads: []string{"c"}},
			{name: "marker at the last key of a prefix", keyMarker: "a/y", prefixes: []string{"b/"}, uploads: []string{"c"}},
			{name: "marker equal to the prefix", keyMarker: "a/", prefixes: []string{"b/"}, uploads: []string{"c"}},
			{name: "marker after the prefixes", keyMarker: "b/z", uploads: []string{"c"}},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				res, err := svc.ListMultipartUploads(ctx, domain.ListMultipartUploadsInput{Bucket: "bkt", Delimiter: "/", KeyMarker: tt.keyMarker})
				require.NoError(t, err)
				assert.Equal(t, tt.prefixes, res.CommonPrefixes)
				keys := make([]string, 0, len(res.Uploads))
				for _, u := range res.Uploads {
					keys = append(keys, u.Key)
				}
				assert.Equal(t, tt.uploads, keys)
			})
		}
	})
}

// Stale uploads are aborted by age; orphaned directories are removed.
func TestCleanupStaleUploads(t *testing.T) {
	forEachImpl(t, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		svc := fx.svc
		for _, b := range []string{"one", "two"} {
			require.NoError(t, svc.MakeBucket(ctx, b, object.MakeBucketOptions{}))
		}
		old1, err := svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "one", Key: "old"})
		require.NoError(t, err)
		uploadPart(t, svc, "one", "old", old1.UploadID, 1, []byte("data"))
		old2, err := svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "two", Key: "old"})
		require.NoError(t, err)
		fx.clk.Advance(48 * time.Hour)
		fresh, err := svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "one", Key: "fresh"})
		require.NoError(t, err)

		// An orphaned directory (no description) older than the cutoff.
		orphan := filepath.Join(fx.roots[0], domain.SystemBucket, storage.MultipartDir, "one", "orphanAAAA")
		require.NoError(t, os.MkdirAll(orphan, 0o755))
		ancient := fx.clk.Now().Add(-72 * time.Hour)
		require.NoError(t, os.Chtimes(orphan, ancient, ancient))

		_, err = svc.CleanupStaleUploads(ctx, "", -time.Second)
		assert.ErrorIs(t, err, domain.ErrInvalidArgument)
		_, err = svc.CleanupStaleUploads(ctx, "Bad_Name", time.Hour)
		assert.ErrorIs(t, err, domain.ErrInvalidBucketName)

		n, err := svc.CleanupStaleUploads(ctx, "one", 24*time.Hour)
		require.NoError(t, err)
		assert.Equal(t, 1, n)
		_, err = os.Stat(orphan)
		assert.True(t, os.IsNotExist(err), "orphans go too")
		_, err = svc.ListObjectParts(ctx, domain.ListPartsInput{Bucket: "one", Key: "old", UploadID: old1.UploadID})
		assert.ErrorIs(t, err, domain.ErrNoSuchUpload)

		n, err = svc.CleanupStaleUploads(ctx, "", 24*time.Hour)
		require.NoError(t, err)
		assert.Equal(t, 1, n, "the upload of the other bucket")
		_, err = svc.ListObjectParts(ctx, domain.ListPartsInput{Bucket: "two", Key: "old", UploadID: old2.UploadID})
		assert.ErrorIs(t, err, domain.ErrNoSuchUpload)
		_, err = svc.ListObjectParts(ctx, domain.ListPartsInput{Bucket: "one", Key: "fresh", UploadID: fresh.UploadID})
		require.NoError(t, err, "recent uploads are kept")

		n, err = svc.CleanupStaleUploads(ctx, "", 0)
		require.NoError(t, err)
		assert.Equal(t, 0, n, "an upload initiated now is not older than now")
		fx.clk.Advance(time.Second)
		n, err = svc.CleanupStaleUploads(ctx, "", 0)
		require.NoError(t, err)
		assert.Equal(t, 1, n)
	})
}

// crc32B64 is the base64 CRC32 (IEEE) of b, as S3 reports it.
func crc32B64(b []byte) string {
	var raw [4]byte
	binary.BigEndian.PutUint32(raw[:], crc32.ChecksumIEEE(b))
	return base64.StdEncoding.EncodeToString(raw[:])
}

// Regression: CompleteMultipartUpload stored the x-amz-checksum-* declared
// by the client without verifying it and ignored the part checksums of
// the request, so GetObject (checksum mode) returned a value chosen by the
// uploader. The object checksum is now the composite of the stored part
// checksums ("<checksum of checksums>-N"), and what the client declares is
// verified against it.
func TestCompleteMultipartChecksum(t *testing.T) {
	part1, part2 := bytes.Repeat([]byte("a"), 16), []byte("tail")
	sum1, sum2 := crc32B64(part1), crc32B64(part2)
	raw1, _ := base64.StdEncoding.DecodeString(sum1)
	raw2, _ := base64.StdEncoding.DecodeString(sum2)
	composite := crc32B64(append(append([]byte{}, raw1...), raw2...))
	crc := func(v string) domain.Checksum { return domain.Checksum{Algorithm: domain.ChecksumCRC32, Value: v} }
	whole := func(v string) domain.Checksum {
		return domain.Checksum{Algorithm: domain.ChecksumCRC32, Value: v, Type: domain.ChecksumTypeComposite}
	}
	both := [2]domain.Checksum{crc(sum1), crc(sum2)}

	tests := []struct {
		name       string
		uploadAlgo domain.ChecksumAlgorithm
		partSums   [2]domain.Checksum
		declared   domain.Checksum
		wantErr    error
		wantCode   string
		want       domain.Checksum
	}{
		// As S3, a COMPOSITE upload needs the checksum of every part in
		// the completion (InvalidRequest without them).
		{name: "part checksums missing from the request", uploadAlgo: domain.ChecksumCRC32,
			wantErr: domain.ErrInvalidArgument, wantCode: "InvalidRequest"},
		{name: "one part checksum missing", uploadAlgo: domain.ChecksumCRC32, partSums: [2]domain.Checksum{crc(sum1)},
			wantErr: domain.ErrInvalidArgument, wantCode: "InvalidRequest"},
		{name: "part checksums in the request", uploadAlgo: domain.ChecksumCRC32, partSums: both, want: whole(composite + "-2")},
		{name: "declared composite", uploadAlgo: domain.ChecksumCRC32, partSums: both, declared: crc(composite + "-2"), want: whole(composite + "-2")},
		{name: "declared without the suffix", uploadAlgo: domain.ChecksumCRC32, partSums: both, declared: crc(composite), want: whole(composite + "-2")},
		{name: "declared value chosen by the client", uploadAlgo: domain.ChecksumCRC32, partSums: both, declared: crc("AAAAAA=="),
			wantErr: domain.ErrBadDigest, wantCode: "BadDigest"},
		{name: "declared with another algorithm", uploadAlgo: domain.ChecksumCRC32, partSums: both,
			declared: domain.Checksum{Algorithm: domain.ChecksumSHA256, Value: base64.StdEncoding.EncodeToString(make([]byte, 32))},
			wantErr:  domain.ErrInvalidArgument, wantCode: "InvalidRequest"},
		{name: "part checksum that does not match", uploadAlgo: domain.ChecksumCRC32, partSums: [2]domain.Checksum{crc(sum1), crc("AAAAAA==")},
			wantErr: domain.ErrInvalidPart, wantCode: "InvalidPart"},
		{name: "declared on an upload without checksums", declared: crc("AAAAAA=="), wantErr: domain.ErrInvalidArgument, wantCode: "InvalidRequest"},
		{name: "no checksum at all"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newFixture(t, "fs")
			ctx := context.Background()
			require.NoError(t, fx.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
			up, err := fx.svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "bkt", Key: "k", Checksum: tt.uploadAlgo})
			require.NoError(t, err)
			p1 := uploadPart(t, fx.svc, "bkt", "k", up.UploadID, 1, part1)
			p2 := uploadPart(t, fx.svc, "bkt", "k", up.UploadID, 2, part2)
			if tt.uploadAlgo != domain.ChecksumNone {
				assert.Equal(t, crc(sum1), p1.Checksum, "the part checksum is computed")
			}
			info, err := fx.svc.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: "bkt", Key: "k", UploadID: up.UploadID,
				Parts:    []domain.CompletePart{{Number: 1, ETag: p1.ETag, Checksum: tt.partSums[0]}, {Number: 2, ETag: p2.ETag, Checksum: tt.partSums[1]}},
				Checksum: tt.declared})
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				var s3err *domain.S3Error
				require.ErrorAs(t, err, &s3err)
				assert.Equal(t, tt.wantCode, s3err.Code)
				_, err := fx.svc.GetObjectInfo(ctx, "bkt", "k", object.GetObjectOptions{})
				assert.ErrorIs(t, err, domain.ErrObjectNotFound, "nothing is committed")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, info.Checksum)
			stored, err := fx.svc.GetObjectInfo(ctx, "bkt", "k", object.GetObjectOptions{})
			require.NoError(t, err)
			assert.Equal(t, tt.want, stored.Checksum, "the stored checksum is the computed one")
		})
	}
}

// TestCompleteMultipartObjectSize: the x-amz-mp-object-size of a
// completion must be the size of the object (the sum of the listed parts,
// plaintext), or the completion fails with InvalidRequest and commits
// nothing, in every implementation, encrypted or not.
func TestCompleteMultipartObjectSize(t *testing.T) {
	forEachImpl(t, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		require.NoError(t, fx.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
		for _, sse := range []domain.SSERequest{{}, {Type: domain.SSES3}} {
			key := "k-" + string(sse.Type)
			up, err := fx.svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "bkt", Key: key, SSE: sse})
			require.NoError(t, err)
			p1 := uploadPart(t, fx.svc, "bkt", key, up.UploadID, 1, bytes.Repeat([]byte("a"), 16))
			p2 := uploadPart(t, fx.svc, "bkt", key, up.UploadID, 2, []byte("tail"))
			parts := []domain.CompletePart{{Number: 1, ETag: p1.ETag}, {Number: 2, ETag: p2.ETag}}
			for _, wrong := range []int64{0, 5, 19, 21} {
				_, err = fx.svc.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: "bkt", Key: key, UploadID: up.UploadID, Parts: parts, ObjectSize: &wrong})
				assert.Equal(t, "InvalidRequest", s3Code(err), "%s: declared %d", key, wrong)
				_, err = fx.svc.GetObjectInfo(ctx, "bkt", key, object.GetObjectOptions{})
				require.ErrorIs(t, err, domain.ErrObjectNotFound, "nothing is committed")
			}
			right := int64(20)
			info, err := fx.svc.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: "bkt", Key: key, UploadID: up.UploadID, Parts: parts, ObjectSize: &right})
			require.NoError(t, err)
			assert.Equal(t, right, info.Size)
		}
	})
}

// A retried completion (its response lost) answers with the object the
// upload created, as S3 does, as long as that object is the current
// version of the key and the parts are the same.
func TestCompleteMultipartRetry(t *testing.T) {
	part1, part2 := bytes.Repeat([]byte("a"), 16), []byte("tail")
	tests := []struct {
		name    string
		mutate  func(t *testing.T, fx *fixture)
		parts   func(p1, p2 domain.Part) []domain.CompletePart
		wantErr bool
	}{
		{name: "same parts"},
		{name: "quoted etags", parts: func(p1, p2 domain.Part) []domain.CompletePart {
			return []domain.CompletePart{{Number: 1, ETag: `"` + p1.ETag + `"`}, {Number: 2, ETag: `"` + p2.ETag + `"`}}
		}},
		{name: "other parts", wantErr: true, parts: func(p1, _ domain.Part) []domain.CompletePart {
			return []domain.CompletePart{{Number: 1, ETag: p1.ETag}}
		}},
		{name: "key overwritten since", wantErr: true, mutate: func(t *testing.T, fx *fixture) {
			_, err := fx.svc.PutObject(context.Background(), object.PutObjectInput{Bucket: "bkt", Key: "k", Reader: strings.NewReader("new"), Size: 3})
			require.NoError(t, err)
		}},
		{name: "key deleted since", wantErr: true, mutate: func(t *testing.T, fx *fixture) {
			_, err := fx.svc.DeleteObject(context.Background(), "bkt", "k", object.DeleteObjectOptions{})
			require.NoError(t, err)
		}},
	}
	for _, kind := range implKinds() {
		for _, tt := range tests {
			t.Run(kind+"/"+tt.name, func(t *testing.T) {
				fx := newFixture(t, kind)
				ctx := context.Background()
				require.NoError(t, fx.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
				up, err := fx.svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "bkt", Key: "k"})
				require.NoError(t, err)
				p1 := uploadPart(t, fx.svc, "bkt", "k", up.UploadID, 1, part1)
				p2 := uploadPart(t, fx.svc, "bkt", "k", up.UploadID, 2, part2)
				in := object.CompleteMultipartInput{Bucket: "bkt", Key: "k", UploadID: up.UploadID,
					Parts: []domain.CompletePart{{Number: 1, ETag: p1.ETag}, {Number: 2, ETag: p2.ETag}}}
				first, err := fx.svc.CompleteMultipartUpload(ctx, in)
				require.NoError(t, err)
				if tt.mutate != nil {
					tt.mutate(t, fx)
				}
				if tt.parts != nil {
					in.Parts = tt.parts(p1, p2)
				}
				again, err := fx.svc.CompleteMultipartUpload(ctx, in)
				if tt.wantErr {
					require.ErrorIs(t, err, domain.ErrNoSuchUpload)
					return
				}
				require.NoError(t, err)
				assert.Equal(t, first.ETag, again.ETag)
				assert.Equal(t, first.Size, again.Size)
				assert.Equal(t, first.VersionID, again.VersionID)
				assert.Empty(t, again.UserMetadata, "the upload id stays internal")
			})
		}
	}
}
