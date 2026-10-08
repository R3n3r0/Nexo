package object_test

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/event"
	eventmock "github.com/r3n3r0/nexo/internal/service/event/mock"
	"github.com/r3n3r0/nexo/internal/service/object"
	objectmock "github.com/r3n3r0/nexo/internal/service/object/mock"
	"github.com/r3n3r0/nexo/internal/service/storage"
	"github.com/r3n3r0/nexo/internal/testutil"
)

// fixture is an object layer under test with its drives. The bucket
// settings and the event sink are the generated mocks, backed by a map of
// configurations and a log of published events.
type fixture struct {
	svc     object.Service
	drives  []storage.StorageAPI
	roots   []string
	buckets *objectmock.BucketConfigSource
	events  *eventmock.Service
	clk     *clock.Fake

	mu        sync.Mutex
	cfgs      map[string]object.BucketConfig
	published []event.Event
}

// setBucketConfig sets the settings the mocked BucketConfigSource returns.
func (fx *fixture) setBucketConfig(bucket string, cfg object.BucketConfig) {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	fx.cfgs[bucket] = cfg
}

// eventNames returns the names of the events published so far.
func (fx *fixture) eventNames() []event.Name {
	fx.mu.Lock()
	defer fx.mu.Unlock()
	out := make([]event.Name, 0, len(fx.published))
	for _, e := range fx.published {
		out = append(out, e.EventName)
	}
	return out
}

const (
	testBlockSize   = 4096
	testInlineLimit = 1024
)

func newDrives(t *testing.T, n int) ([]storage.StorageAPI, []string) {
	t.Helper()
	base := t.TempDir()
	drives := make([]storage.StorageAPI, 0, n)
	roots := make([]string, 0, n)
	for i := 0; i < n; i++ {
		root := filepath.Join(base, fmt.Sprintf("d%d", i+1))
		drives = append(drives, testutil.NewLocalDriveIndex(t, root, i))
		roots = append(roots, root)
	}
	return drives, roots
}

// newFixture builds an object layer of the given kind ("fs" or "erasure")
// over fresh drives; extra options override the test defaults.
func newFixture(t *testing.T, kind string, extra ...object.Option) *fixture {
	t.Helper()
	fx := &fixture{
		buckets: objectmock.NewBucketConfigSource(t),
		events:  eventmock.NewService(t),
		clk:     clock.NewFake(time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)),
		cfgs:    map[string]object.BucketConfig{},
	}
	fx.buckets.EXPECT().BucketConfig(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, bucket string) (object.BucketConfig, error) {
		fx.mu.Lock()
		defer fx.mu.Unlock()
		return fx.cfgs[bucket], nil
	}).Maybe()
	fx.events.EXPECT().Publish(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, e event.Event) error {
		fx.mu.Lock()
		defer fx.mu.Unlock()
		fx.published = append(fx.published, e)
		return nil
	}).Maybe()
	deps := object.Deps{Buckets: fx.buckets, Events: fx.events, Clock: fx.clk, Logger: slog.New(slog.DiscardHandler)}
	opts := []object.Option{object.WithInlineLimit(testInlineLimit), object.WithBlockSize(testBlockSize), object.WithMinPartSize(16), object.WithLockTimeout(5 * time.Second)}
	opts = append(opts, extra...)
	var err error
	switch kind {
	case "fs":
		fx.drives, fx.roots = newDrives(t, 1)
		deps.Store = fx.drives[0]
		fx.svc, err = object.NewFS(deps, opts...)
	case "erasure":
		fx.drives, fx.roots = newDrives(t, 4)
		deps.Sets = [][]storage.StorageAPI{fx.drives}
		deps.Parity = 2
		deps.DeploymentID = "deployment-test"
		fx.svc, err = object.NewErasure(deps, opts...)
	default:
		t.Fatalf("unknown kind %s", kind)
	}
	require.NoError(t, err)
	return fx
}

// forEachImpl runs fn against both implementations.
func forEachImpl(t *testing.T, fn func(t *testing.T, fx *fixture)) {
	t.Helper()
	for _, kind := range []string{"fs", "erasure"} {
		t.Run(kind, func(t *testing.T) { fn(t, newFixture(t, kind)) })
	}
}

func payload(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('a' + i%26)
	}
	return b
}

func md5hex(b []byte) string {
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

// assertNoTmpLeft checks that no temporary data survived on any drive.
func assertNoTmpLeft(t *testing.T, fx *fixture) {
	t.Helper()
	for _, root := range fx.roots {
		entries, _ := os.ReadDir(filepath.Join(root, domain.SystemBucket, storage.TmpDir))
		assert.Empty(t, entries, "temporary entries left on %s", root)
	}
}

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func put(t *testing.T, svc object.Service, bucket, key string, data []byte, mutate ...func(*object.PutObjectInput)) domain.ObjectInfo {
	t.Helper()
	in := object.PutObjectInput{Bucket: bucket, Key: key, Reader: bytes.NewReader(data), Size: int64(len(data)), ContentType: "text/plain"}
	for _, m := range mutate {
		m(&in)
	}
	info, err := svc.PutObject(context.Background(), in)
	require.NoError(t, err, "put %s/%s", bucket, key)
	return info
}

func get(t *testing.T, svc object.Service, bucket, key string, opts object.GetObjectOptions) ([]byte, *object.Reader) {
	t.Helper()
	r, err := svc.GetObject(context.Background(), bucket, key, opts)
	require.NoError(t, err, "get %s/%s", bucket, key)
	data, err := io.ReadAll(r)
	require.NoError(t, err)
	require.NoError(t, r.Close())
	return data, r
}

func TestConstructorsValidateDeps(t *testing.T) {
	_, err := object.NewFS(object.Deps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = object.NewErasure(object.Deps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	drives, _ := newDrives(t, 2)
	_, err = object.NewErasure(object.Deps{Sets: [][]storage.StorageAPI{drives}, Parity: 0})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = object.NewErasure(object.Deps{Sets: [][]storage.StorageAPI{drives}, Parity: 2})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument, "parity must leave at least one data shard")
	svc, err := object.NewErasure(object.Deps{Sets: [][]storage.StorageAPI{drives}, Parity: 1})
	require.NoError(t, err)
	h := svc.Health(context.Background(), object.HealthOptions{})
	assert.True(t, h.Healthy)
	assert.Equal(t, 2, h.WriteQuorum, "data == parity → data+1")
}

func TestBuckets(t *testing.T) {
	forEachImpl(t, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		svc := fx.svc
		assert.ErrorIs(t, svc.MakeBucket(ctx, "Bad_Name", object.MakeBucketOptions{}), domain.ErrInvalidBucketName)
		assert.ErrorIs(t, svc.MakeBucket(ctx, domain.SystemBucket, object.MakeBucketOptions{}), domain.ErrInvalidBucketName)
		require.NoError(t, svc.MakeBucket(ctx, "photos", object.MakeBucketOptions{}))
		assert.ErrorIs(t, svc.MakeBucket(ctx, "photos", object.MakeBucketOptions{}), domain.ErrBucketAlreadyExists)
		require.NoError(t, svc.MakeBucket(ctx, "photos", object.MakeBucketOptions{ForceCreate: true}))
		require.NoError(t, svc.MakeBucket(ctx, "docs", object.MakeBucketOptions{}))

		fx.setBucketConfig("photos", object.BucketConfig{Versioning: domain.VersioningEnabled, ObjectLock: true, QuotaBytes: 10})
		info, err := svc.GetBucketInfo(ctx, "photos")
		require.NoError(t, err)
		assert.Equal(t, "photos", info.Name)
		assert.Equal(t, "us-east-1", info.Region)
		assert.Equal(t, domain.VersioningEnabled, info.Versioning)
		assert.True(t, info.ObjectLockEnabled)
		assert.Equal(t, uint64(10), info.QuotaBytes)
		assert.False(t, info.Created.IsZero())
		_, err = svc.GetBucketInfo(ctx, "missing")
		assert.ErrorIs(t, err, domain.ErrBucketNotFound)

		list, err := svc.ListBuckets(ctx)
		require.NoError(t, err)
		require.Len(t, list, 2)
		assert.Equal(t, "docs", list[0].Name)
		assert.Equal(t, "photos", list[1].Name)

		put(t, svc, "docs", "a.txt", []byte("hello"))
		assert.ErrorIs(t, svc.DeleteBucket(ctx, "docs", object.DeleteBucketOptions{}), domain.ErrBucketNotEmpty)
		require.NoError(t, svc.DeleteBucket(ctx, "docs", object.DeleteBucketOptions{Force: true}))
		_, err = svc.GetBucketInfo(ctx, "docs")
		assert.ErrorIs(t, err, domain.ErrBucketNotFound)
		assert.ErrorIs(t, svc.DeleteBucket(ctx, "docs", object.DeleteBucketOptions{}), domain.ErrBucketNotFound)

		// An in-progress upload also blocks deletion.
		_, err = svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "photos", Key: "big"})
		require.NoError(t, err)
		assert.ErrorIs(t, svc.DeleteBucket(ctx, "photos", object.DeleteBucketOptions{}), domain.ErrBucketNotEmpty)
		require.NoError(t, svc.DeleteBucket(ctx, "photos", object.DeleteBucketOptions{Force: true}))
		require.NoError(t, svc.MakeBucket(ctx, "photos", object.MakeBucketOptions{}))
		uploads, err := svc.ListMultipartUploads(ctx, domain.ListMultipartUploadsInput{Bucket: "photos"})
		require.NoError(t, err)
		assert.Empty(t, uploads.Uploads, "forced deletion aborts the uploads")

		assert.Contains(t, fx.eventNames(), event.BucketCreated)
		assert.Contains(t, fx.eventNames(), event.BucketRemoved)
	})
}

func TestPutGetHeadDelete(t *testing.T) {
	forEachImpl(t, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		svc := fx.svc
		require.NoError(t, svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))

		small := []byte("hello world")
		info := put(t, svc, "bkt", "small.txt", small, func(in *object.PutObjectInput) {
			in.UserMetadata = map[string]string{"Owner": "alice", "cache-control": "max-age=60"}
			in.Tags = map[string]string{"env": "dev", "team": "a b"}
			in.MD5Hex = md5hex(small)
			in.Checksum = domain.Checksum{Algorithm: domain.ChecksumCRC32}
		})
		assert.Equal(t, md5hex(small), info.ETag)
		assert.Equal(t, int64(len(small)), info.Size)
		assert.Equal(t, "text/plain", info.ContentType)
		assert.Equal(t, "alice", info.UserMetadata["owner"])
		assert.Equal(t, "max-age=60", info.CacheControl)
		assert.Equal(t, map[string]string{"env": "dev", "team": "a b"}, info.Tags)
		assert.Equal(t, domain.ChecksumCRC32, info.Checksum.Algorithm)
		assert.NotEmpty(t, info.Checksum.Value)
		assert.True(t, info.Inlined)
		assert.Empty(t, info.VersionID)
		assert.Equal(t, fx.clk.Now(), info.ModTime)
		assert.Equal(t, domain.StorageClassStandard, info.StorageClass)

		data, r := get(t, svc, "bkt", "small.txt", object.GetObjectOptions{})
		assert.Equal(t, small, data)
		assert.Equal(t, int64(0), r.Offset)
		assert.Equal(t, int64(len(small)), r.Length)
		assert.Equal(t, "alice", r.Info.UserMetadata["owner"])

		// Large object: several erasure blocks, stored as a data file.
		large := payload(3*testBlockSize + 123)
		linfo := put(t, svc, "bkt", "dir/large.bin", large, func(in *object.PutObjectInput) { in.ContentType = "" })
		assert.False(t, linfo.Inlined)
		assert.Equal(t, "application/octet-stream", linfo.ContentType)
		assert.Equal(t, md5hex(large), linfo.ETag)
		data, _ = get(t, svc, "bkt", "dir/large.bin", object.GetObjectOptions{})
		assert.Equal(t, large, data)

		// Ranges crossing block boundaries, suffix and open ranges, part numbers.
		for _, tc := range []struct {
			spec  string
			start int64
			end   int64
		}{
			{"bytes=0-9", 0, 10},
			{"bytes=4090-4100", 4090, 4101},
			{"bytes=4096-", 4096, int64(len(large))},
			{"bytes=-100", int64(len(large)) - 100, int64(len(large))},
			{"bytes=8191-8192", 8191, 8193},
		} {
			rg, err := domain.ParseRange(tc.spec)
			require.NoError(t, err)
			data, r := get(t, svc, "bkt", "dir/large.bin", object.GetObjectOptions{Range: rg})
			assert.Equal(t, large[tc.start:tc.end], data, tc.spec)
			assert.Equal(t, tc.start, r.Offset, tc.spec)
		}
		rg, _ := domain.ParseRange("bytes=999999-")
		_, err := svc.GetObject(ctx, "bkt", "dir/large.bin", object.GetObjectOptions{Range: rg})
		assert.ErrorIs(t, err, domain.ErrInvalidRange)
		data, _ = get(t, svc, "bkt", "dir/large.bin", object.GetObjectOptions{PartNumber: 1})
		assert.Equal(t, large, data)
		_, err = svc.GetObject(ctx, "bkt", "dir/large.bin", object.GetObjectOptions{PartNumber: 2})
		assert.ErrorIs(t, err, domain.ErrInvalidRange)

		// Unknown size streams (chunked uploads) and empty objects.
		uinfo, err := svc.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: "unknown", Reader: bytes.NewReader(small), Size: -1})
		require.NoError(t, err)
		assert.Equal(t, int64(len(small)), uinfo.Size)
		data, _ = get(t, svc, "bkt", "unknown", object.GetObjectOptions{})
		assert.Equal(t, small, data)
		put(t, svc, "bkt", "empty", nil)
		data, _ = get(t, svc, "bkt", "empty", object.GetObjectOptions{})
		assert.Empty(t, data)
		put(t, svc, "bkt", "folder/", nil)
		hinfo, err := svc.GetObjectInfo(ctx, "bkt", "folder/", object.GetObjectOptions{})
		require.NoError(t, err)
		assert.Equal(t, int64(0), hinfo.Size)

		// Head.
		hinfo, err = svc.GetObjectInfo(ctx, "bkt", "small.txt", object.GetObjectOptions{})
		require.NoError(t, err)
		assert.Equal(t, info.ETag, hinfo.ETag)
		assert.True(t, hinfo.IsLatest)
		_, err = svc.GetObjectInfo(ctx, "bkt", "nope", object.GetObjectOptions{})
		assert.ErrorIs(t, err, domain.ErrObjectNotFound)
		_, err = svc.GetObjectInfo(ctx, "nobucket", "x", object.GetObjectOptions{})
		assert.ErrorIs(t, err, domain.ErrBucketNotFound)
		_, err = svc.GetObject(ctx, "bkt", "nope", object.GetObjectOptions{})
		assert.ErrorIs(t, err, domain.ErrObjectNotFound)
		_, err = svc.GetObject(ctx, "bkt", "../x", object.GetObjectOptions{})
		assert.ErrorIs(t, err, domain.ErrInvalidObjectName)

		// Overwrite replaces the data and purges the old data directory.
		put(t, svc, "bkt", "dir/large.bin", small)
		data, _ = get(t, svc, "bkt", "dir/large.bin", object.GetObjectOptions{})
		assert.Equal(t, small, data)
		entries, err := os.ReadDir(filepath.Join(fx.roots[0], "bkt", "dir", "large.bin"))
		require.NoError(t, err)
		assert.Len(t, entries, 1, "only xl.meta remains for an inline object")

		// Delete is idempotent in unversioned buckets.
		del, err := svc.DeleteObject(ctx, "bkt", "small.txt", object.DeleteObjectOptions{})
		require.NoError(t, err)
		assert.Equal(t, "small.txt", del.Key)
		assert.False(t, del.DeleteMarker)
		_, err = svc.GetObjectInfo(ctx, "bkt", "small.txt", object.GetObjectOptions{})
		assert.ErrorIs(t, err, domain.ErrObjectNotFound)
		_, err = svc.DeleteObject(ctx, "bkt", "small.txt", object.DeleteObjectOptions{})
		require.NoError(t, err)
		_, err = svc.DeleteObject(ctx, "nobucket", "x", object.DeleteObjectOptions{})
		assert.ErrorIs(t, err, domain.ErrBucketNotFound)

		res, err := svc.DeleteObjects(ctx, object.DeleteObjectsInput{Bucket: "bkt", Objects: []object.ObjectToDelete{{Key: "unknown"}, {Key: "empty"}, {Key: "bad//key"}}})
		require.NoError(t, err)
		assert.Len(t, res.Deleted, 2)
		require.Len(t, res.Errors, 1)
		assert.ErrorIs(t, res.Errors[0].Err, domain.ErrInvalidObjectName)
		_, err = svc.DeleteObjects(ctx, object.DeleteObjectsInput{Bucket: "bkt"})
		assert.ErrorIs(t, err, domain.ErrMalformedXML)

		names := fx.eventNames()
		assert.Contains(t, names, event.ObjectCreatedPut)
		assert.Contains(t, names, event.ObjectAccessedGet)
		assert.Contains(t, names, event.ObjectRemovedDelete)
	})
}

func TestPutValidation(t *testing.T) {
	forEachImpl(t, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		svc := fx.svc
		require.NoError(t, svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
		data := payload(2000)

		_, err := svc.PutObject(ctx, object.PutObjectInput{Bucket: "nobucket", Key: "k", Reader: bytes.NewReader(data), Size: 2000})
		assert.ErrorIs(t, err, domain.ErrBucketNotFound)
		_, err = svc.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: "", Reader: bytes.NewReader(data), Size: 2000})
		assert.ErrorIs(t, err, domain.ErrInvalidObjectName)

		// Checksum mismatches (inline and streamed).
		var s3err *domain.S3Error
		_, err = svc.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: "k", Reader: bytes.NewReader(data), Size: 2000, MD5Hex: md5hex([]byte("other"))})
		assert.ErrorIs(t, err, domain.ErrChecksumMismatch)
		require.ErrorAs(t, err, &s3err)
		assert.Equal(t, "BadDigest", s3err.Code)
		_, err = svc.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: "k", Reader: bytes.NewReader(data[:10]), Size: 10, SHA256Hex: strings.Repeat("0", 64)})
		assert.ErrorIs(t, err, domain.ErrChecksumMismatch)
		_, err = svc.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: "k", Reader: bytes.NewReader(data), Size: 2000,
			Checksum: domain.Checksum{Algorithm: domain.ChecksumSHA256, Value: base64.StdEncoding.EncodeToString(make([]byte, 32))}})
		assert.ErrorIs(t, err, domain.ErrChecksumMismatch)
		_, err = svc.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: "k", Reader: bytes.NewReader(data), Size: 2000, Checksum: domain.Checksum{Algorithm: "MD6"}})
		assert.ErrorIs(t, err, domain.ErrInvalidArgument)
		_, err = svc.GetObjectInfo(ctx, "bkt", "k", object.GetObjectOptions{})
		assert.ErrorIs(t, err, domain.ErrObjectNotFound, "failed writes leave nothing")
		tmpEntries, _ := os.ReadDir(filepath.Join(fx.roots[0], domain.SystemBucket, storage.TmpDir))
		assert.Empty(t, tmpEntries, "temporary data is cleaned up")

		// Incomplete bodies.
		_, err = svc.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: "k", Reader: bytes.NewReader(data[:100]), Size: 2000})
		require.ErrorAs(t, err, &s3err)
		assert.Equal(t, "IncompleteBody", s3err.Code)
		_, err = svc.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: "k", Reader: bytes.NewReader(data[:100]), Size: 500})
		require.ErrorAs(t, err, &s3err)
		assert.Equal(t, "IncompleteBody", s3err.Code)

		// Too many tags, quota.
		tags := map[string]string{}
		for i := 0; i < 11; i++ {
			tags[fmt.Sprintf("k%d", i)] = "v"
		}
		_, err = svc.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: "k", Reader: bytes.NewReader(data), Size: 2000, Tags: tags})
		assert.ErrorIs(t, err, domain.ErrInvalidArgument)
		fx.setBucketConfig("bkt", object.BucketConfig{QuotaBytes: 100})
		_, err = svc.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: "k", Reader: bytes.NewReader(data), Size: 2000})
		assert.ErrorIs(t, err, domain.ErrQuotaExceeded)
		fx.setBucketConfig("bkt", object.BucketConfig{})

		// Parent/child conflicts are refused.
		put(t, svc, "bkt", "a/b", []byte("x"))
		_, err = svc.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: "a", Reader: bytes.NewReader(data), Size: 2000})
		assert.ErrorIs(t, err, domain.ErrInvalidObjectName)
		_, err = svc.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: "a/b/c", Reader: bytes.NewReader(data), Size: 2000})
		assert.ErrorIs(t, err, domain.ErrInvalidObjectName)
	})
}

func TestConditions(t *testing.T) {
	forEachImpl(t, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		svc := fx.svc
		require.NoError(t, svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
		info := put(t, svc, "bkt", "k", []byte("v1"))
		later := fx.clk.Now().Add(time.Hour)
		earlier := fx.clk.Now().Add(-time.Hour)

		_, err := svc.GetObjectInfo(ctx, "bkt", "k", object.GetObjectOptions{Conditions: domain.Conditions{IfMatch: `"` + info.ETag + `"`}})
		require.NoError(t, err)
		_, err = svc.GetObjectInfo(ctx, "bkt", "k", object.GetObjectOptions{Conditions: domain.Conditions{IfMatch: "other"}})
		assert.ErrorIs(t, err, domain.ErrPreconditionFailed)
		_, err = svc.GetObjectInfo(ctx, "bkt", "k", object.GetObjectOptions{Conditions: domain.Conditions{IfNoneMatch: info.ETag}})
		assert.ErrorIs(t, err, domain.ErrNotModified)
		_, err = svc.GetObjectInfo(ctx, "bkt", "k", object.GetObjectOptions{Conditions: domain.Conditions{IfNoneMatch: "*"}})
		assert.ErrorIs(t, err, domain.ErrNotModified)
		_, err = svc.GetObject(ctx, "bkt", "k", object.GetObjectOptions{Conditions: domain.Conditions{IfModifiedSince: later}})
		assert.ErrorIs(t, err, domain.ErrNotModified)
		_, err = svc.GetObjectInfo(ctx, "bkt", "k", object.GetObjectOptions{Conditions: domain.Conditions{IfModifiedSince: earlier}})
		require.NoError(t, err)
		_, err = svc.GetObjectInfo(ctx, "bkt", "k", object.GetObjectOptions{Conditions: domain.Conditions{IfUnmodifiedSince: earlier}})
		assert.ErrorIs(t, err, domain.ErrPreconditionFailed)
		_, err = svc.GetObjectInfo(ctx, "bkt", "k", object.GetObjectOptions{Conditions: domain.Conditions{IfUnmodifiedSince: later}})
		require.NoError(t, err)

		// Write preconditions.
		_, err = svc.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: "k", Reader: strings.NewReader("v2"), Size: 2, Conditions: domain.Conditions{IfNoneMatch: "*"}})
		assert.ErrorIs(t, err, domain.ErrPreconditionFailed)
		_, err = svc.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: "new", Reader: strings.NewReader("v2"), Size: 2, Conditions: domain.Conditions{IfNoneMatch: "*"}})
		require.NoError(t, err)
		_, err = svc.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: "k", Reader: strings.NewReader("v2"), Size: 2, Conditions: domain.Conditions{IfMatch: "wrong"}})
		assert.ErrorIs(t, err, domain.ErrPreconditionFailed)
		_, err = svc.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: "k", Reader: strings.NewReader("v2"), Size: 2, Conditions: domain.Conditions{IfMatch: info.ETag}})
		require.NoError(t, err)
	})
}

func TestVersioning(t *testing.T) {
	forEachImpl(t, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		svc := fx.svc
		require.NoError(t, svc.MakeBucket(ctx, "ver", object.MakeBucketOptions{}))
		fx.setBucketConfig("ver", object.BucketConfig{Versioning: domain.VersioningEnabled})

		v1 := put(t, svc, "ver", "k", []byte("one"))
		fx.clk.Advance(time.Second)
		v2 := put(t, svc, "ver", "k", payload(5000))
		require.NotEmpty(t, v1.VersionID)
		require.NotEqual(t, v1.VersionID, v2.VersionID)

		data, r := get(t, svc, "ver", "k", object.GetObjectOptions{})
		assert.Equal(t, payload(5000), data)
		assert.Equal(t, v2.VersionID, r.Info.VersionID)
		assert.True(t, r.Info.IsLatest)
		data, r = get(t, svc, "ver", "k", object.GetObjectOptions{VersionID: v1.VersionID})
		assert.Equal(t, "one", string(data))
		assert.False(t, r.Info.IsLatest)
		_, err := svc.GetObject(ctx, "ver", "k", object.GetObjectOptions{VersionID: "missing"})
		assert.ErrorIs(t, err, domain.ErrNoSuchVersion)

		// Delete creates a marker; the object disappears from plain reads.
		fx.clk.Advance(time.Second)
		del, err := svc.DeleteObject(ctx, "ver", "k", object.DeleteObjectOptions{})
		require.NoError(t, err)
		assert.True(t, del.DeleteMarker)
		require.NotEmpty(t, del.DeleteMarkerVersionID)
		_, err = svc.GetObjectInfo(ctx, "ver", "k", object.GetObjectOptions{})
		assert.ErrorIs(t, err, domain.ErrObjectNotFound)
		_, err = svc.GetObjectInfo(ctx, "ver", "k", object.GetObjectOptions{VersionID: del.DeleteMarkerVersionID})
		assert.ErrorIs(t, err, domain.ErrMethodNotAllowed)
		r, err = svc.GetObject(ctx, "ver", "k", object.GetObjectOptions{VersionID: v2.VersionID})
		require.NoError(t, err)
		require.NoError(t, r.Close(), "closing the body releases the read lock")

		versions, err := svc.ListObjectVersions(ctx, domain.ListObjectsInput{Bucket: "ver"})
		require.NoError(t, err)
		require.Len(t, versions.Objects, 3)
		assert.True(t, versions.Objects[0].IsDeleteMarker)
		assert.True(t, versions.Objects[0].IsLatest)
		assert.Equal(t, v2.VersionID, versions.Objects[1].VersionID)
		assert.Equal(t, v1.VersionID, versions.Objects[2].VersionID)
		plain, err := svc.ListObjects(ctx, domain.ListObjectsInput{Bucket: "ver"})
		require.NoError(t, err)
		assert.Empty(t, plain.Objects, "a latest delete marker hides the key")

		// Removing the marker restores the object; removing versions permanently.
		rm, err := svc.DeleteObject(ctx, "ver", "k", object.DeleteObjectOptions{VersionID: del.DeleteMarkerVersionID})
		require.NoError(t, err)
		assert.True(t, rm.DeleteMarker)
		assert.Equal(t, del.DeleteMarkerVersionID, rm.VersionID)
		info, err := svc.GetObjectInfo(ctx, "ver", "k", object.GetObjectOptions{})
		require.NoError(t, err)
		assert.Equal(t, v2.VersionID, info.VersionID)
		rm, err = svc.DeleteObject(ctx, "ver", "k", object.DeleteObjectOptions{VersionID: v2.VersionID})
		require.NoError(t, err)
		assert.False(t, rm.DeleteMarker)
		rm, err = svc.DeleteObject(ctx, "ver", "k", object.DeleteObjectOptions{VersionID: v2.VersionID})
		require.NoError(t, err, "deleting a missing version is idempotent")
		assert.Equal(t, v2.VersionID, rm.VersionID)
		data, _ = get(t, svc, "ver", "k", object.GetObjectOptions{})
		assert.Equal(t, "one", string(data))
		entries, err := os.ReadDir(filepath.Join(fx.roots[0], "ver", "k"))
		require.NoError(t, err)
		assert.Len(t, entries, 1, "the data directory of the removed version is purged")

		// Suspended: writes replace the null version, deletes create a null marker.
		fx.setBucketConfig("ver", object.BucketConfig{Versioning: domain.VersioningSuspended})
		n1 := put(t, svc, "ver", "k", []byte("null-1"))
		assert.Equal(t, "null", n1.VersionID, "the null version of a suspended bucket is reported as \"null\"")
		put(t, svc, "ver", "k", []byte("null-2"))
		versions, err = svc.ListObjectVersions(ctx, domain.ListObjectsInput{Bucket: "ver"})
		require.NoError(t, err)
		require.Len(t, versions.Objects, 2, "null version replaced, v1 kept")
		assert.Equal(t, "null", versions.Objects[0].VersionID)
		del, err = svc.DeleteObject(ctx, "ver", "k", object.DeleteObjectOptions{})
		require.NoError(t, err)
		assert.True(t, del.DeleteMarker)
		assert.Equal(t, "null", del.DeleteMarkerVersionID)
		versions, err = svc.ListObjectVersions(ctx, domain.ListObjectsInput{Bucket: "ver"})
		require.NoError(t, err)
		require.Len(t, versions.Objects, 2)
		assert.True(t, versions.Objects[0].IsDeleteMarker)
		assert.Equal(t, "null", versions.Objects[0].VersionID)
		rm, err = svc.DeleteObject(ctx, "ver", "k", object.DeleteObjectOptions{VersionID: "null"})
		require.NoError(t, err)
		assert.Equal(t, "null", rm.VersionID)
		data, _ = get(t, svc, "ver", "k", object.GetObjectOptions{})
		assert.Equal(t, "one", string(data))

		// Deleting a marker of a missing object in a versioned bucket still records a marker.
		fx.setBucketConfig("ver", object.BucketConfig{Versioning: domain.VersioningEnabled})
		del, err = svc.DeleteObject(ctx, "ver", "ghost", object.DeleteObjectOptions{})
		require.NoError(t, err)
		assert.True(t, del.DeleteMarker)
		versions, err = svc.ListObjectVersions(ctx, domain.ListObjectsInput{Bucket: "ver", Prefix: "ghost"})
		require.NoError(t, err)
		require.Len(t, versions.Objects, 1)
		assert.True(t, versions.Objects[0].IsDeleteMarker)
		assert.Contains(t, fx.eventNames(), event.ObjectRemovedDeleteMarkerCreated)
	})
}
