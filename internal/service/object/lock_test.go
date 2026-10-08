package object_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/lock"
	lockmock "github.com/r3n3r0/nexo/internal/service/lock/mock"
	"github.com/r3n3r0/nexo/internal/service/object"
)

// lockRecorder backs a mocked lock.Locker: it records every acquisition
// and release, and fails the acquisitions of the resources listed in deny.
type lockRecorder struct {
	t    *testing.T
	mu   sync.Mutex
	log  []string
	held map[string]int
	deny map[string]error
}

func newLockRecorder(t *testing.T) (*lockRecorder, *lockmock.Locker) {
	rec := &lockRecorder{t: t, held: map[string]int{}, deny: map[string]error{}}
	locker := lockmock.NewLocker(t)
	locker.EXPECT().NewNSLock(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, volume, path string) lock.NSLock {
		return rec.nsLock(lock.Resource(volume, path))
	}).Maybe()
	return rec, locker
}

func (r *lockRecorder) nsLock(resource string) lock.NSLock {
	ns := lockmock.NewNSLock(r.t)
	acquire := func(mode string) (lock.LockContext, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if err := r.deny[resource]; err != nil {
			r.log = append(r.log, "deny "+mode+" "+resource)
			return nil, err
		}
		r.log = append(r.log, mode+" "+resource)
		r.held[resource]++
		lc := lockmock.NewLockContext(r.t)
		lc.EXPECT().Unlock(mock.Anything).Run(func(context.Context) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.log = append(r.log, "unlock "+resource)
			r.held[resource]--
		}).Once()
		return lc, nil
	}
	ns.EXPECT().GetLock(mock.Anything, mock.Anything).RunAndReturn(func(context.Context, time.Duration) (lock.LockContext, error) {
		return acquire("W")
	}).Maybe()
	ns.EXPECT().GetRLock(mock.Anything, mock.Anything).RunAndReturn(func(context.Context, time.Duration) (lock.LockContext, error) {
		return acquire("R")
	}).Maybe()
	return ns
}

// take returns and clears the recorded operations.
func (r *lockRecorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.log
	r.log = nil
	return out
}

func (r *lockRecorder) outstanding() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]int{}
	for k, v := range r.held {
		if v != 0 {
			out[k] = v
		}
	}
	return out
}

func newLockedFS(t *testing.T, locker lock.Locker) object.Service {
	t.Helper()
	drives, _ := newDrives(t, 1)
	svc, err := object.NewFS(object.Deps{Store: drives[0], Locks: locker, Logger: slog.New(slog.DiscardHandler)},
		object.WithInlineLimit(testInlineLimit), object.WithMinPartSize(1))
	require.NoError(t, err)
	return svc
}

// Every operation takes the namespace lock it needs and releases it.
func TestOperationsTakeNamespaceLocks(t *testing.T) {
	rec, locker := newLockRecorder(t)
	svc := newLockedFS(t, locker)
	ctx := context.Background()
	obj := lock.Resource("bkt", "k")

	require.NoError(t, svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
	assert.Equal(t, []string{"W " + lock.Resource("bkt", ""), "unlock " + lock.Resource("bkt", "")}, rec.take())

	put(t, svc, "bkt", "k", []byte("data"))
	assert.Equal(t, []string{"W " + obj, "unlock " + obj}, rec.take())

	r, err := svc.GetObject(ctx, "bkt", "k", object.GetObjectOptions{})
	require.NoError(t, err)
	assert.Equal(t, []string{"R " + obj}, rec.take(), "the read lock is held while the body is open")
	require.NoError(t, r.Close())
	require.NoError(t, r.Close(), "closing twice releases once")
	assert.Equal(t, []string{"unlock " + obj}, rec.take())

	_, err = svc.GetObjectInfo(ctx, "bkt", "k", object.GetObjectOptions{})
	require.NoError(t, err)
	assert.Equal(t, []string{"R " + obj, "unlock " + obj}, rec.take())

	_, err = svc.PutObjectTags(ctx, "bkt", "k", map[string]string{"a": "1"}, object.TagOptions{})
	require.NoError(t, err)
	assert.Equal(t, []string{"W " + obj, "unlock " + obj}, rec.take())

	// Copies lock both ends in resource order: source read, destination write.
	_, err = svc.CopyObject(ctx, object.CopyObjectInput{SrcBucket: "bkt", SrcKey: "k", DstBucket: "bkt", DstKey: "z"})
	require.NoError(t, err)
	dst := lock.Resource("bkt", "z")
	assert.Equal(t, []string{"R " + obj, "W " + dst, "unlock " + dst, "unlock " + obj}, rec.take())
	_, err = svc.CopyObject(ctx, object.CopyObjectInput{SrcBucket: "bkt", SrcKey: "z", DstBucket: "bkt", DstKey: "k"})
	require.NoError(t, err)
	assert.Equal(t, []string{"W " + obj, "R " + dst, "unlock " + dst, "unlock " + obj}, rec.take())

	// Multipart: parts share the upload lock (and commit under the lock of
	// their part number), completion holds the object and the upload
	// exclusively.
	up, err := svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "bkt", Key: "k"})
	require.NoError(t, err)
	rec.take()
	upload := lock.Resource(domain.SystemBucket, "multipart/bkt/"+up.UploadID)
	p, err := svc.PutObjectPart(ctx, object.PutObjectPartInput{Bucket: "bkt", Key: "k", UploadID: up.UploadID, PartNumber: 1, Reader: strings.NewReader("x"), Size: 1})
	require.NoError(t, err)
	part := lock.Resource(domain.SystemBucket, "multipart/bkt/"+up.UploadID+"/part.1")
	assert.Equal(t, []string{"R " + upload, "W " + part, "unlock " + part, "unlock " + upload}, rec.take())
	_, err = svc.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: "bkt", Key: "k", UploadID: up.UploadID, Parts: []domain.CompletePart{{Number: 1, ETag: p.ETag}}})
	require.NoError(t, err)
	assert.Equal(t, []string{"W " + obj, "W " + upload, "unlock " + upload, "unlock " + obj}, rec.take())

	_, err = svc.DeleteObject(ctx, "bkt", "k", object.DeleteObjectOptions{})
	require.NoError(t, err)
	assert.Equal(t, []string{"W " + obj, "unlock " + obj}, rec.take())
	require.NoError(t, svc.DeleteBucket(ctx, "bkt", object.DeleteBucketOptions{Force: true}))
	assert.Empty(t, rec.outstanding(), "every lock was released")
}

// A lock that cannot be acquired fails the operation without side effects.
func TestLockTimeoutFailsCleanly(t *testing.T) {
	rec, locker := newLockRecorder(t)
	svc := newLockedFS(t, locker)
	ctx := context.Background()
	require.NoError(t, svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
	put(t, svc, "bkt", "a", []byte("source"))
	put(t, svc, "bkt", "k", []byte("v1"))
	rec.deny[lock.Resource("bkt", "k")] = fmt.Errorf("%w: busy", domain.ErrLockTimeout)

	tests := []struct {
		name string
		op   func() error
	}{
		{name: "put", op: func() error {
			_, err := svc.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: "k", Reader: bytes.NewReader([]byte("v2")), Size: 2})
			return err
		}},
		{name: "get", op: func() error { _, err := svc.GetObject(ctx, "bkt", "k", object.GetObjectOptions{}); return err }},
		{name: "head", op: func() error { _, err := svc.GetObjectInfo(ctx, "bkt", "k", object.GetObjectOptions{}); return err }},
		{name: "delete", op: func() error { _, err := svc.DeleteObject(ctx, "bkt", "k", object.DeleteObjectOptions{}); return err }},
		{name: "tags", op: func() error {
			_, err := svc.PutObjectTags(ctx, "bkt", "k", map[string]string{"a": "b"}, object.TagOptions{})
			return err
		}},
		{name: "copy from", op: func() error {
			_, err := svc.CopyObject(ctx, object.CopyObjectInput{SrcBucket: "bkt", SrcKey: "k", DstBucket: "bkt", DstKey: "other"})
			return err
		}},
		{name: "copy to (after locking the source)", op: func() error {
			_, err := svc.CopyObject(ctx, object.CopyObjectInput{SrcBucket: "bkt", SrcKey: "a", DstBucket: "bkt", DstKey: "k"})
			return err
		}},
		{name: "complete", op: func() error {
			_, err := svc.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: "bkt", Key: "k", UploadID: "AAAA", Parts: []domain.CompletePart{{Number: 1, ETag: "x"}}})
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.ErrorIs(t, tt.op(), domain.ErrLockTimeout)
		})
	}
	delete(rec.deny, lock.Resource("bkt", "k"))
	got, _ := get(t, svc, "bkt", "k", object.GetObjectOptions{})
	assert.Equal(t, "v1", string(got), "nothing changed")
	_, err := svc.GetObjectInfo(ctx, "bkt", "other", object.GetObjectOptions{})
	assert.ErrorIs(t, err, domain.ErrObjectNotFound)
	assert.Empty(t, rec.outstanding(), "a failed copy releases the lock it acquired")
}
