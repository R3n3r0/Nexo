package quota_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
	objectmock "github.com/r3n3r0/nexo/internal/service/object/mock"
	"github.com/r3n3r0/nexo/internal/service/quota"
	quotamock "github.com/r3n3r0/nexo/internal/service/quota/mock"
	"github.com/r3n3r0/nexo/internal/service/scanner"
)

const bucket = "bkt"

// env is a decorator over the generated mocks: the object layer counts the
// walks of the bucket, which answer walkSize.
type env struct {
	svc     quota.Service
	objects *objectmock.Service
	clk     *clock.Fake
	walks   int
	// walkSize is the usage a walk of the bucket measures.
	walkSize uint64
	// current are the sizes of the current versions of the keys of the
	// bucket, which the decorator reads to credit an overwrite.
	current map[string]int64
	// versioning is the versioning of the bucket.
	versioning domain.Versioning
}

func newEnv(t *testing.T, quotaBytes uint64, usage *scanner.Usage) *env {
	t.Helper()
	e := &env{objects: objectmock.NewService(t), clk: clock.NewFake(time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC))}
	cfgs := objectmock.NewBucketConfigSource(t)
	cfgs.EXPECT().BucketConfig(mock.Anything, mock.Anything).RunAndReturn(func(context.Context, string) (object.BucketConfig, error) {
		return object.BucketConfig{QuotaBytes: quotaBytes, Versioning: e.versioning}, nil
	}).Maybe()
	e.objects.EXPECT().GetObjectInfo(mock.Anything, bucket, mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, _, key string, _ object.GetObjectOptions) (domain.ObjectInfo, error) {
			size, ok := e.current[key]
			if !ok {
				return domain.ObjectInfo{}, domain.ErrObjectNotFound
			}
			return domain.ObjectInfo{Bucket: bucket, Key: key, Size: size}, nil
		}).Maybe()
	e.objects.EXPECT().DataUsage(mock.Anything, bucket).RunAndReturn(func(context.Context, string) ([]domain.BucketInfo, error) {
		e.walks++
		return []domain.BucketInfo{{Bucket: domain.Bucket{Name: bucket}, Size: e.walkSize}}, nil
	}).Maybe()
	deps := quota.Deps{Objects: e.objects, Buckets: cfgs, Clock: e.clk, Logger: slog.New(slog.DiscardHandler)}
	if usage != nil {
		src := quotamock.NewUsageSource(t)
		src.EXPECT().Usage(mock.Anything).RunAndReturn(func(context.Context) (scanner.Usage, error) { return *usage, nil }).Maybe()
		deps.Usage = src
	}
	svc, err := quota.New(deps)
	require.NoError(t, err)
	e.svc = svc
	return e
}

// expectPuts makes the object layer accept the writes, reading their body.
func (e *env) expectPuts() {
	e.objects.EXPECT().PutObject(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, in object.PutObjectInput) (domain.ObjectInfo, error) {
		n, err := io.Copy(io.Discard, in.Reader)
		if err != nil {
			return domain.ObjectInfo{}, err
		}
		return domain.ObjectInfo{Bucket: in.Bucket, Key: in.Key, Size: n}, nil
	}).Maybe()
}

func (e *env) put(size int) error {
	_, err := e.svc.PutObject(context.Background(), object.PutObjectInput{Bucket: bucket, Key: "k", Reader: bytes.NewReader(make([]byte, size)), Size: int64(size)})
	return err
}

func assertQuotaErr(t *testing.T, err error) {
	t.Helper()
	require.ErrorIs(t, err, domain.ErrQuotaExceeded)
	var s3err *domain.S3Error
	require.True(t, errors.As(err, &s3err))
	assert.Equal(t, "XNexoQuotaExceeded", s3err.Code)
}

func TestNewValidatesDeps(t *testing.T) {
	_, err := quota.New(quota.Deps{})
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
}

func TestNoQuotaPassesThrough(t *testing.T) {
	e := newEnv(t, 0, nil)
	e.expectPuts()
	require.NoError(t, e.put(1<<20))
	_, err := e.svc.PutObject(context.Background(), object.PutObjectInput{Bucket: bucket, Key: "s", Reader: strings.NewReader("stream"), Size: -1})
	require.NoError(t, err)
	assert.Zero(t, e.walks, "a bucket without quota is never measured")
}

// TestSequentialWrites (TS-I-16): the writes committed since the base
// count, so that the quota holds between two scanner cycles; a refusal is
// confirmed by a walk not older than the request.
func TestSequentialWrites(t *testing.T) {
	usage := &scanner.Usage{LastUpdate: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC), Buckets: map[string]scanner.BucketUsage{bucket: {Counts: scanner.Counts{Size: 10}}}}
	e := newEnv(t, 100, usage)
	e.expectPuts()
	steps := []struct {
		name      string
		advance   time.Duration
		size      int
		walkSize  uint64
		wantErr   bool
		wantWalks int
	}{
		{name: "fits", size: 50, wantWalks: 0},
		{name: "near the quota with a fresh base", size: 30, wantWalks: 0},
		{name: "beyond the quota, confirmed by a walk", size: 20, walkSize: 90, wantErr: true, wantWalks: 1},
		// The walk measures the deployment, where another node deleted
		// data meanwhile.
		{name: "beyond the estimate, the walk finds room", advance: 2 * time.Second, size: 20, walkSize: 60, wantWalks: 2},
		{name: "beyond the quota on a walk of the request", size: 30, wantErr: true, wantWalks: 2},
	}
	for _, st := range steps {
		e.clk.Advance(st.advance)
		e.walkSize = st.walkSize
		err := e.put(st.size)
		if st.wantErr {
			assertQuotaErr(t, err)
		} else {
			require.NoError(t, err, st.name)
		}
		assert.Equal(t, st.wantWalks, e.walks, st.name)
	}
	u, err := e.svc.BucketUsage(context.Background(), bucket)
	require.NoError(t, err)
	assert.Equal(t, uint64(100), u.Quota)
	assert.Equal(t, quota.SourceWalk, u.Source)
	assert.Equal(t, uint64(60), u.Base)
	assert.Equal(t, uint64(20), u.Recent)
}

// TestOtherNodesWrites: near the quota a stale base is refreshed, so that
// the writes of the other nodes (in the walk, not in this node's
// counters) are counted.
func TestOtherNodesWrites(t *testing.T) {
	usage := &scanner.Usage{LastUpdate: time.Date(2026, 10, 9, 11, 0, 0, 0, time.UTC), Buckets: map[string]scanner.BucketUsage{}}
	e := newEnv(t, 100, usage)
	e.expectPuts()
	require.NoError(t, e.put(60), "far from the quota the base is trusted")
	assert.Zero(t, e.walks)
	e.clk.Advance(time.Millisecond)
	e.walkSize = 95 // the 60 bytes of this node and 35 of another one
	assertQuotaErr(t, e.put(25))
	assert.Equal(t, 1, e.walks, "near the quota the stale base is refreshed")
	require.NoError(t, e.put(5))
}

func TestNoScannerUsageWalks(t *testing.T) {
	e := newEnv(t, 100, nil)
	e.expectPuts()
	e.walkSize = 90
	assertQuotaErr(t, e.put(20))
	assert.Equal(t, 1, e.walks)
	require.NoError(t, e.put(10))
}

// TestDeleteFreesSpaceAtOnce: after a delete on this node, a refusal walks
// the bucket again even with a fresh base.
func TestDeleteFreesSpaceAtOnce(t *testing.T) {
	e := newEnv(t, 100, nil)
	e.expectPuts()
	e.objects.EXPECT().DeleteObject(mock.Anything, bucket, "k", mock.Anything).Return(object.DeletedObject{Key: "k"}, nil)
	require.NoError(t, e.put(90))
	assertQuotaErr(t, e.put(20))
	walks := e.walks
	_, err := e.svc.DeleteObject(context.Background(), bucket, "k", object.DeleteObjectOptions{})
	require.NoError(t, err)
	e.clk.Advance(time.Millisecond)
	e.walkSize = 0
	require.NoError(t, e.put(20))
	assert.Equal(t, walks+1, e.walks)
}

// TestRemoteDeleteFreesSpace: a delete on another node is not seen by this
// node's counters; the walk that confirms a refusal finds the room, even
// with a base younger than the refresh interval.
func TestRemoteDeleteFreesSpace(t *testing.T) {
	e := newEnv(t, 100, nil)
	e.expectPuts()
	require.NoError(t, e.put(90))
	walks := e.walks
	e.clk.Advance(time.Millisecond)
	e.walkSize = 0 // another node deleted the object
	require.NoError(t, e.put(20))
	assert.Equal(t, walks+1, e.walks)
}

// TestStreamAbortedBeyondAllowance: a write of unknown length is aborted as
// soon as it exceeds the room left (the layer commits nothing).
func TestStreamAbortedBeyondAllowance(t *testing.T) {
	e := newEnv(t, 100, nil)
	e.expectPuts()
	e.walkSize = 40
	_, err := e.svc.PutObject(context.Background(), object.PutObjectInput{Bucket: bucket, Key: "s", Reader: bytes.NewReader(make([]byte, 61)), Size: -1})
	assertQuotaErr(t, err)
	_, err = e.svc.PutObject(context.Background(), object.PutObjectInput{Bucket: bucket, Key: "s", Reader: bytes.NewReader(make([]byte, 60)), Size: -1})
	require.NoError(t, err, "exactly the room left")
	assertQuotaErr(t, e.put(1)) // the stream is counted
}

// TestConcurrentReservations: the bytes of a write in progress are
// reserved, so two concurrent writes cannot both take the last room.
func TestConcurrentReservations(t *testing.T) {
	e := newEnv(t, 100, nil)
	var inner error
	e.objects.EXPECT().PutObject(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, in object.PutObjectInput) (domain.ObjectInfo, error) {
		if in.Key == "first" {
			// While the first write runs, a second one is checked.
			_, inner = e.svc.PutObject(context.Background(), object.PutObjectInput{Bucket: bucket, Key: "second", Reader: bytes.NewReader(make([]byte, 60)), Size: 60})
		}
		return domain.ObjectInfo{Size: in.Size}, nil
	}).Once()
	_, err := e.svc.PutObject(context.Background(), object.PutObjectInput{Bucket: bucket, Key: "first", Reader: bytes.NewReader(make([]byte, 60)), Size: 60})
	require.NoError(t, err)
	assertQuotaErr(t, inner)
}

func TestCopyObject(t *testing.T) {
	tests := []struct {
		name     string
		srcSize  int64
		srcErr   error
		walkSize uint64
		wantErr  bool
	}{
		{name: "fits", srcSize: 50, walkSize: 40},
		{name: "beyond the quota", srcSize: 70, walkSize: 40, wantErr: true},
		{name: "source unreadable: the layer reports it", srcErr: domain.ErrObjectNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, 100, nil)
			e.walkSize = tt.walkSize
			ckey := domain.SSECustomerKey{Key: make([]byte, 32), KeyMD5: "md5"}
			e.objects.EXPECT().GetObjectInfo(mock.Anything, "src", "a", object.GetObjectOptions{VersionID: "v1", SSECustomerKey: ckey}).
				Return(domain.ObjectInfo{Size: tt.srcSize}, tt.srcErr)
			in := object.CopyObjectInput{SrcBucket: "src", SrcKey: "a", SrcVersionID: "v1", SrcSSECustomerKey: ckey, DstBucket: bucket, DstKey: "b"}
			if !tt.wantErr {
				e.objects.EXPECT().CopyObject(mock.Anything, in).Return(domain.ObjectInfo{Size: tt.srcSize}, tt.srcErr)
			}
			_, err := e.svc.CopyObject(context.Background(), in)
			switch {
			case tt.wantErr:
				assertQuotaErr(t, err)
			case tt.srcErr != nil:
				require.ErrorIs(t, err, tt.srcErr)
			default:
				require.NoError(t, err)
			}
		})
	}
}

func TestMultipart(t *testing.T) {
	e := newEnv(t, 100, nil)
	e.walkSize = 20
	ctx := context.Background()
	e.objects.EXPECT().PutObjectPart(mock.Anything, mock.Anything).Return(domain.Part{Number: 1}, nil).Once()
	_, err := e.svc.PutObjectPart(ctx, object.PutObjectPartInput{Bucket: bucket, Key: "k", UploadID: "u", PartNumber: 1, Size: 50})
	require.NoError(t, err)
	_, err = e.svc.PutObjectPart(ctx, object.PutObjectPartInput{Bucket: bucket, Key: "k", UploadID: "u", PartNumber: 2, Size: 81})
	assertQuotaErr(t, err)

	// Two pages of parts; the completion selects parts 1 and 3 (70 bytes).
	e.objects.EXPECT().ListObjectParts(mock.Anything, domain.ListPartsInput{Bucket: bucket, Key: "k", UploadID: "u"}).
		Return(domain.ListPartsResult{Parts: []domain.Part{{Number: 1, Size: 50}, {Number: 2, Size: 30}}, IsTruncated: true, NextPartNumberMarker: 2}, nil)
	e.objects.EXPECT().ListObjectParts(mock.Anything, domain.ListPartsInput{Bucket: bucket, Key: "k", UploadID: "u", PartNumberMarker: 2}).
		Return(domain.ListPartsResult{Parts: []domain.Part{{Number: 3, Size: 20}}}, nil)
	selected := []domain.CompletePart{{Number: 1}, {Number: 3}}
	e.objects.EXPECT().CompleteMultipartUpload(mock.Anything, mock.Anything).Return(domain.ObjectInfo{Size: 70}, nil).Once()
	_, err = e.svc.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: bucket, Key: "k", UploadID: "u", Parts: selected})
	require.NoError(t, err)
	_, err = e.svc.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: bucket, Key: "k", UploadID: "u", Parts: selected})
	assertQuotaErr(t, err)
}

// TestOverwriteReplacesItsVersion (S-BKT-09): in a bucket without
// versioning (or with versioning suspended, the null version) an
// overwrite needs only the bytes it adds over the version it replaces; in
// a versioned bucket the old version stays and the write counts in full.
func TestOverwriteReplacesItsVersion(t *testing.T) {
	tests := []struct {
		name       string
		versioning domain.Versioning
		wantErr    bool
	}{
		{name: "unversioned", versioning: domain.VersioningOff},
		{name: "suspended", versioning: domain.VersioningSuspended},
		{name: "versioned", versioning: domain.VersioningEnabled, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, 1<<20, nil)
			e.versioning = tt.versioning
			e.expectPuts()
			e.walkSize = 600 << 10
			e.current = map[string]int64{"k": 600 << 10}
			err := e.put(600 << 10)
			if tt.wantErr {
				assertQuotaErr(t, err)
				return
			}
			require.NoError(t, err, "600 KiB over 600 KiB in a 1 MiB quota")
			// A new key of the same size does not fit.
			_, err = e.svc.PutObject(context.Background(), object.PutObjectInput{Bucket: bucket, Key: "other", Reader: bytes.NewReader(make([]byte, 600<<10)), Size: 600 << 10})
			assertQuotaErr(t, err)
			// Nor does an overwrite larger than the quota allows.
			assertQuotaErr(t, e.put(1<<20+1))
		})
	}
}
