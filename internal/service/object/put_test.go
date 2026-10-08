package object_test

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
)

// patternReader produces n deterministic bytes without holding them in
// memory and hashes what it produced.
type patternReader struct {
	remaining int64
	pos       int64
	sum       hash.Hash
}

func newPatternReader(n int64) *patternReader { return &patternReader{remaining: n, sum: md5.New()} }

func (p *patternReader) Read(b []byte) (int, error) {
	if p.remaining <= 0 {
		return 0, io.EOF
	}
	if int64(len(b)) > p.remaining {
		b = b[:p.remaining]
	}
	for i := range b {
		b[i] = byte((p.pos + int64(i)) * 31 % 251)
	}
	p.pos += int64(len(b))
	p.remaining -= int64(len(b))
	p.sum.Write(b)
	return len(b), nil
}

func (p *patternReader) md5() string { return hex.EncodeToString(p.sum.Sum(nil)) }

// TS-I-02: objects of every size class round-trip with their ETag, with the
// default inline limit (128 KiB).
func TestPutGetSizes(t *testing.T) {
	sizes := []struct {
		name   string
		size   int64
		inline bool
		fsOnly bool
	}{
		{name: "0B", size: 0, inline: true},
		{name: "1KiB", size: 1 << 10, inline: true},
		{name: "127KiB", size: 127 << 10, inline: true},
		{name: "128KiB", size: 128 << 10, inline: true},
		{name: "129KiB", size: 129 << 10},
		{name: "8MiB", size: 8 << 20},
		{name: "100MiB", size: 100 << 20, fsOnly: true},
	}
	for _, kind := range implKinds() {
		t.Run(kind, func(t *testing.T) {
			fx := newFixture(t, kind, object.WithInlineLimit(128<<10), object.WithBlockSize(1<<20))
			ctx := context.Background()
			require.NoError(t, fx.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
			for _, sz := range sizes {
				t.Run(sz.name, func(t *testing.T) {
					if sz.fsOnly && (kind != "fs" || testing.Short()) {
						t.Skip("large object: fs only, not in -short")
					}
					src := newPatternReader(sz.size)
					info, err := fx.svc.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: "obj-" + sz.name, Reader: src, Size: sz.size})
					require.NoError(t, err)
					assert.Equal(t, src.md5(), info.ETag)
					assert.Equal(t, sz.size, info.Size)
					assert.Equal(t, sz.inline, info.Inlined)

					r, err := fx.svc.GetObject(ctx, "bkt", "obj-"+sz.name, object.GetObjectOptions{})
					require.NoError(t, err)
					h := md5.New()
					n, err := io.Copy(h, r)
					require.NoError(t, err)
					require.NoError(t, r.Close())
					assert.Equal(t, sz.size, n)
					assert.Equal(t, src.md5(), hex.EncodeToString(h.Sum(nil)), "content identical")
					assert.Equal(t, info.ETag, r.Info.ETag)
				})
			}
		})
	}
}

// TS-I-03: range reads.
func TestRangeReads(t *testing.T) {
	forEachImpl(t, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		require.NoError(t, fx.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
		data := payload(3000)
		put(t, fx.svc, "bkt", "k", data)
		put(t, fx.svc, "bkt", "empty", nil)
		tests := []struct {
			spec       string
			key        string
			wantOffset int64
			wantLen    int64
			wantErr    error
		}{
			{spec: "bytes=0-0", key: "k", wantOffset: 0, wantLen: 1},
			{spec: "bytes=-500", key: "k", wantOffset: 2500, wantLen: 500},
			{spec: "bytes=-5000", key: "k", wantOffset: 0, wantLen: 3000},
			{spec: "bytes=100-", key: "k", wantOffset: 100, wantLen: 2900},
			{spec: "bytes=2990-5000", key: "k", wantOffset: 2990, wantLen: 10},
			{spec: "bytes=2999-2999", key: "k", wantOffset: 2999, wantLen: 1},
			{spec: "bytes=3000-", key: "k", wantErr: domain.ErrInvalidRange},
			{spec: "bytes=5000-6000", key: "k", wantErr: domain.ErrInvalidRange},
			{spec: "bytes=0-0", key: "empty", wantErr: domain.ErrInvalidRange},
			{spec: "bytes=0-", key: "empty", wantErr: domain.ErrInvalidRange},
			// Regression: a suffix of an empty object is unsatisfiable too.
			{spec: "bytes=-1", key: "empty", wantErr: domain.ErrInvalidRange},
		}
		for _, tt := range tests {
			t.Run(tt.key+" "+tt.spec, func(t *testing.T) {
				rg, err := domain.ParseRange(tt.spec)
				require.NoError(t, err)
				r, err := fx.svc.GetObject(ctx, "bkt", tt.key, object.GetObjectOptions{Range: rg})
				if tt.wantErr != nil {
					assert.ErrorIs(t, err, tt.wantErr)
					return
				}
				require.NoError(t, err)
				got, err := io.ReadAll(r)
				require.NoError(t, err)
				require.NoError(t, r.Close())
				assert.Equal(t, tt.wantOffset, r.Offset)
				assert.Equal(t, tt.wantLen, r.Length)
				assert.Equal(t, data[tt.wantOffset:tt.wantOffset+tt.wantLen], got)
			})
		}
		rg, _ := domain.ParseRange("bytes=0-1")
		_, err := fx.svc.GetObject(ctx, "bkt", "k", object.GetObjectOptions{Range: rg, PartNumber: 1})
		assert.ErrorIs(t, err, domain.ErrInvalidArgument, "range and part number are exclusive")
		_, err = fx.svc.GetObjectInfo(ctx, "bkt", "k", object.GetObjectOptions{PartNumber: 2})
		assert.ErrorIs(t, err, domain.ErrInvalidRange)
	})
}

// TS-I-04: conditional reads and writes through the service.
func TestConditionalRequests(t *testing.T) {
	forEachImpl(t, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		require.NoError(t, fx.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
		info := put(t, fx.svc, "bkt", "k", []byte("v1"))
		later := fx.clk.Now().Add(time.Hour)
		earlier := fx.clk.Now().Add(-time.Hour)
		tests := []struct {
			name string
			c    domain.Conditions
			want error
		}{
			{name: "if-match", c: domain.Conditions{IfMatch: `"` + info.ETag + `"`}},
			{name: "if-match fails", c: domain.Conditions{IfMatch: `"nope"`}, want: domain.ErrPreconditionFailed},
			{name: "if-none-match", c: domain.Conditions{IfNoneMatch: info.ETag}, want: domain.ErrNotModified},
			{name: "if-modified-since", c: domain.Conditions{IfModifiedSince: later}, want: domain.ErrNotModified},
			{name: "if-modified-since earlier", c: domain.Conditions{IfModifiedSince: earlier}},
			{name: "if-unmodified-since", c: domain.Conditions{IfUnmodifiedSince: earlier}, want: domain.ErrPreconditionFailed},
			{name: "if-match beats if-unmodified-since", c: domain.Conditions{IfMatch: info.ETag, IfUnmodifiedSince: earlier}},
			{name: "if-none-match beats if-modified-since", c: domain.Conditions{IfNoneMatch: "x", IfModifiedSince: later}},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				r, err := fx.svc.GetObject(ctx, "bkt", "k", object.GetObjectOptions{Conditions: tt.c})
				if tt.want != nil {
					assert.ErrorIs(t, err, tt.want)
					return
				}
				require.NoError(t, err)
				require.NoError(t, r.Close())
			})
		}

		// If-None-Match: * on PUT creates only (TS-I-04), also for completions.
		_, err := fx.svc.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: "k", Reader: strings.NewReader("v2"), Size: 2, Conditions: domain.Conditions{IfNoneMatch: "*"}})
		assert.ErrorIs(t, err, domain.ErrPreconditionFailed)
		up, err := fx.svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "bkt", Key: "k"})
		require.NoError(t, err)
		p, err := fx.svc.PutObjectPart(ctx, object.PutObjectPartInput{Bucket: "bkt", Key: "k", UploadID: up.UploadID, PartNumber: 1, Reader: strings.NewReader("abc"), Size: 3})
		require.NoError(t, err)
		_, err = fx.svc.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: "bkt", Key: "k", UploadID: up.UploadID,
			Parts: []domain.CompletePart{{Number: 1, ETag: p.ETag}}, Conditions: domain.Conditions{IfNoneMatch: "*"}})
		assert.ErrorIs(t, err, domain.ErrPreconditionFailed)
		_, err = fx.svc.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: "bkt", Key: "k", UploadID: up.UploadID,
			Parts: []domain.CompletePart{{Number: 1, ETag: p.ETag}}, Conditions: domain.Conditions{IfMatch: info.ETag}})
		require.NoError(t, err, "the failed attempt left the upload intact")
		got, _ := get(t, fx.svc, "bkt", "k", object.GetObjectOptions{})
		assert.Equal(t, "abc", string(got))
	})
}

// S-OBJ-07: standard headers and user metadata.
func TestPutStandardHeadersAndMetadata(t *testing.T) {
	forEachImpl(t, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		require.NoError(t, fx.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
		info := put(t, fx.svc, "bkt", "doc.html", []byte("<p>hi</p>"), func(in *object.PutObjectInput) {
			in.ContentType = "text/html"
			in.ContentEncoding = "gzip"
			in.ContentDisposition = `attachment; filename="doc.html"`
			in.ContentLanguage = "it-IT"
			in.CacheControl = "max-age=3600"
			in.Expires = "Wed, 21 Oct 2026 07:28:00 GMT"
			in.UserMetadata = map[string]string{"Project-Name": "Nexo", "x-amz-meta-Owner": "alice", "cache-control": "ignored"}
			in.StorageClass = domain.StorageClassReducedRedundancy
		})
		check := func(info domain.ObjectInfo) {
			t.Helper()
			assert.Equal(t, "text/html", info.ContentType)
			assert.Equal(t, "gzip", info.ContentEncoding)
			assert.Equal(t, `attachment; filename="doc.html"`, info.ContentDisposition)
			assert.Equal(t, "it-IT", info.ContentLanguage)
			assert.Equal(t, "max-age=3600", info.CacheControl, "the explicit field wins over the legacy key")
			assert.Equal(t, time.Date(2026, 10, 21, 7, 28, 0, 0, time.UTC), info.Expires)
			assert.Equal(t, map[string]string{"project-name": "Nexo", "owner": "alice"}, info.UserMetadata)
			assert.Equal(t, domain.StorageClassReducedRedundancy, info.StorageClass)
		}
		check(info)
		head, err := fx.svc.GetObjectInfo(ctx, "bkt", "doc.html", object.GetObjectOptions{})
		require.NoError(t, err)
		check(head)

		// COPY keeps everything, REPLACE takes the new set.
		cp, err := fx.svc.CopyObject(ctx, object.CopyObjectInput{SrcBucket: "bkt", SrcKey: "doc.html", DstBucket: "bkt", DstKey: "copy.html"})
		require.NoError(t, err)
		check(cp)
		cp, err = fx.svc.CopyObject(ctx, object.CopyObjectInput{SrcBucket: "bkt", SrcKey: "doc.html", DstBucket: "bkt", DstKey: "copy.html",
			ReplaceMetadata: true, ContentType: "text/plain", CacheControl: "no-store", UserMetadata: map[string]string{"k": "v"}})
		require.NoError(t, err)
		assert.Equal(t, "text/plain", cp.ContentType)
		assert.Equal(t, "no-store", cp.CacheControl)
		assert.Empty(t, cp.ContentEncoding)
		assert.True(t, cp.Expires.IsZero())
		assert.Equal(t, map[string]string{"k": "v"}, cp.UserMetadata)
		assert.Equal(t, domain.StorageClassReducedRedundancy, cp.StorageClass, "the storage class follows the source")

		// Multipart uploads carry their metadata to the object.
		up, err := fx.svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "bkt", Key: "mp", ContentType: "video/mp4",
			ContentDisposition: "inline", CacheControl: "private", UserMetadata: map[string]string{"A": "1"}})
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"a": "1"}, up.UserMetadata)
		p, err := fx.svc.PutObjectPart(ctx, object.PutObjectPartInput{Bucket: "bkt", Key: "mp", UploadID: up.UploadID, PartNumber: 1, Reader: strings.NewReader("x"), Size: 1})
		require.NoError(t, err)
		done, err := fx.svc.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: "bkt", Key: "mp", UploadID: up.UploadID, Parts: []domain.CompletePart{{Number: 1, ETag: p.ETag}}})
		require.NoError(t, err)
		assert.Equal(t, "video/mp4", done.ContentType)
		assert.Equal(t, "inline", done.ContentDisposition)
		assert.Equal(t, "private", done.CacheControl)
		assert.Equal(t, 1, done.Parts)
	})
}

// Content-MD5 and additional checksums are verified (BadDigest).
func TestPutDigests(t *testing.T) {
	forEachImpl(t, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		require.NoError(t, fx.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
		for _, size := range []int{10, 3 * testBlockSize} {
			data := payload(size)
			t.Run(fmt.Sprint(size), func(t *testing.T) {
				key := fmt.Sprint("k", size)
				_, err := fx.svc.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: key, Reader: bytes.NewReader(data), Size: int64(size), MD5Hex: md5hex([]byte("x"))})
				assert.ErrorIs(t, err, domain.ErrBadDigest)
				assert.ErrorIs(t, err, domain.ErrChecksumMismatch)
				_, err = fx.svc.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: key, Reader: bytes.NewReader(data), Size: int64(size),
					Checksum: domain.Checksum{Algorithm: domain.ChecksumCRC32C, Value: "AAAAAA=="}})
				assert.ErrorIs(t, err, domain.ErrBadDigest)
				_, err = fx.svc.GetObjectInfo(ctx, "bkt", key, object.GetObjectOptions{})
				assert.ErrorIs(t, err, domain.ErrObjectNotFound, "a bad digest commits nothing")

				_, err = fx.svc.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: key, Reader: bytes.NewReader(data), Size: -2})
				assert.ErrorIs(t, err, domain.ErrInvalidArgument, "negative sizes other than -1 are refused")

				for _, algo := range []domain.ChecksumAlgorithm{domain.ChecksumCRC32, domain.ChecksumCRC32C, domain.ChecksumSHA1, domain.ChecksumSHA256} {
					info, err := fx.svc.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: key, Reader: bytes.NewReader(data), Size: int64(size),
						MD5Hex: md5hex(data), SHA256Hex: sha256hex(data), Checksum: domain.Checksum{Algorithm: algo}})
					require.NoError(t, err, algo)
					assert.Equal(t, algo, info.Checksum.Algorithm)
					// Declaring the computed value succeeds.
					_, err = fx.svc.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: key, Reader: bytes.NewReader(data), Size: int64(size), Checksum: info.Checksum})
					require.NoError(t, err)
				}
			})
		}
	})
}
