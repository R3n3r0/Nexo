package bucket_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/lock"
	lockmock "github.com/r3n3r0/nexo/internal/service/lock/mock"
	"github.com/r3n3r0/nexo/internal/service/object"
	objectmock "github.com/r3n3r0/nexo/internal/service/object/mock"
	policymock "github.com/r3n3r0/nexo/internal/service/policy/mock"
	"github.com/r3n3r0/nexo/internal/service/storage"
	"github.com/r3n3r0/nexo/internal/testutil"
)

// stack is the bucket service over the real filesystem object layer and
// the real metadata store, sharing one drive.
type stack struct {
	svc     bucket.Service
	objects object.Service
	store   bucket.MetadataStore
	root    string
	clk     *clock.Fake
}

func newStack(t *testing.T) *stack {
	t.Helper()
	root := filepath.Join(t.TempDir(), "d1")
	logger := slog.New(slog.DiscardHandler)
	drive := testutil.NewLocalDrive(t, root)
	store, err := bucket.NewStore(bucket.StoreDeps{Drives: []storage.StorageAPI{drive}, Logger: logger})
	require.NoError(t, err)
	clk := clock.NewFake(time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC))
	locks := lock.NewLocal(lock.Deps{})
	objects, err := object.NewFS(object.Deps{Store: drive, Buckets: store, Locks: locks, Clock: clk, Logger: logger})
	require.NoError(t, err)
	svc, err := bucket.New(bucket.Deps{Objects: objects, Store: store, Policy: policymock.NewService(t), Locks: locks, Clock: clk, Logger: logger})
	require.NoError(t, err)
	return &stack{svc: svc, objects: objects, store: store, root: root, clk: clk}
}

// TS-I-01 at the service level: create, list, head, delete (empty and not).
func TestBucketLifecycleEndToEnd(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	created := s.clk.Now()
	_, err := s.svc.Create(ctx, bucket.CreateInput{Name: "photos", Region: "eu-south-1"})
	require.NoError(t, err)
	_, err = s.svc.Create(ctx, bucket.CreateInput{Name: "photos"})
	assert.ErrorIs(t, err, domain.ErrBucketAlreadyExists)
	_, err = s.svc.Create(ctx, bucket.CreateInput{Name: "locked", ObjectLock: true})
	require.NoError(t, err)

	// Both records live in the bucket's system directory.
	for _, name := range []string{".bucket.json", ".metadata.json"} {
		_, err := os.Stat(filepath.Join(s.root, domain.SystemBucket, storage.BucketsDir, "photos", name))
		require.NoError(t, err, name)
	}

	s.clk.Advance(time.Hour)
	info, err := s.svc.Get(ctx, "photos")
	require.NoError(t, err)
	assert.True(t, created.Equal(info.Created))
	assert.Equal(t, "eu-south-1", info.Region)
	list, err := s.svc.List(ctx, bucket.ListInput{})
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, "locked", list[0].Name)
	assert.True(t, list[0].ObjectLockEnabled)
	assert.Equal(t, domain.VersioningEnabled, list[0].Versioning)

	// The object layer enforces the stored versioning.
	put := func(bucketName, key string) domain.ObjectInfo {
		info, err := s.objects.PutObject(ctx, object.PutObjectInput{Bucket: bucketName, Key: key, Reader: bytes.NewReader([]byte("x")), Size: 1})
		require.NoError(t, err)
		return info
	}
	assert.NotEmpty(t, put("locked", "k").VersionID)
	assert.Empty(t, put("photos", "k").VersionID)
	require.NoError(t, s.svc.SetVersioning(ctx, "photos", domain.VersioningEnabled))
	assert.NotEmpty(t, put("photos", "k2").VersionID, "the change applies at once")

	usage, err := s.svc.List(ctx, bucket.ListInput{WithUsage: true})
	require.NoError(t, err)
	require.Len(t, usage, 2)
	assert.Equal(t, uint64(2), usage[1].Objects)

	assert.ErrorIs(t, s.svc.Delete(ctx, "photos", bucket.DeleteOptions{}), domain.ErrBucketNotEmpty)
	require.NoError(t, s.svc.Delete(ctx, "photos", bucket.DeleteOptions{Force: true}))
	_, err = s.svc.Get(ctx, "photos")
	assert.ErrorIs(t, err, domain.ErrBucketNotFound)
	_, err = os.Stat(filepath.Join(s.root, domain.SystemBucket, storage.BucketsDir, "photos"))
	assert.True(t, os.IsNotExist(err), "the system directory goes with the bucket")

	// A re-created bucket starts from a clean configuration.
	_, err = s.svc.Create(ctx, bucket.CreateInput{Name: "photos"})
	require.NoError(t, err)
	v, err := s.svc.GetVersioning(ctx, "photos")
	require.NoError(t, err)
	assert.Equal(t, domain.VersioningOff, v)
	_, err = s.objects.DeleteObject(ctx, "locked", "k", object.DeleteObjectOptions{})
	require.NoError(t, err)
	assert.ErrorIs(t, s.svc.Delete(ctx, "locked", bucket.DeleteOptions{}), domain.ErrBucketNotEmpty, "versions and markers remain")
	require.NoError(t, s.svc.Delete(ctx, "photos", bucket.DeleteOptions{}))
}

// Concurrent changes of different settings never overwrite each other.
func TestConcurrentConfigurationUpdates(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	_, err := s.svc.Create(ctx, bucket.CreateInput{Name: "bkt"})
	require.NoError(t, err)
	ops := []func() error{
		func() error { return s.svc.SetVersioning(ctx, "bkt", domain.VersioningEnabled) },
		func() error { _, err := s.svc.SetQuota(ctx, "bkt", bucket.Quota{Size: 10}); return err },
		func() error { return s.svc.SetTags(ctx, "bkt", map[string]string{"a": "1"}) },
		func() error { return s.svc.SetCORS(ctx, "bkt", []byte("<CORSConfiguration/>")) },
		func() error { return s.svc.SetLifecycle(ctx, "bkt", []byte("<LifecycleConfiguration/>")) },
		func() error { return s.svc.SetEncryption(ctx, "bkt", bucket.EncryptionConfig{Algorithm: "AES256"}) },
		func() error { return s.svc.SetNotification(ctx, "bkt", []byte("<NotificationConfiguration/>")) },
	}
	for round := 0; round < 5; round++ {
		var wg sync.WaitGroup
		for _, op := range ops {
			wg.Add(1)
			go func(op func() error) {
				defer wg.Done()
				assert.NoError(t, op())
			}(op)
		}
		wg.Wait()
	}
	s.store.Invalidate(ctx, "bkt")
	m, err := s.store.Load(ctx, "bkt")
	require.NoError(t, err)
	assert.Equal(t, domain.VersioningEnabled, m.Versioning)
	assert.Equal(t, uint64(10), m.Quota.Size)
	assert.Equal(t, map[string]string{"a": "1"}, m.Tags)
	assert.NotEmpty(t, m.CORS)
	assert.NotEmpty(t, m.Lifecycle)
	assert.Equal(t, "AES256", m.Encryption.Algorithm)
	assert.NotEmpty(t, m.Notification)
}

// The configuration lock bounds every update.
func TestUpdateLockTimeout(t *testing.T) {
	ctx := context.Background()
	objects := objectmock.NewService(t)
	locker := lockmock.NewLocker(t)
	ns := lockmock.NewNSLock(t)
	locker.EXPECT().NewNSLock(mock.Anything, domain.SystemBucket, storage.BucketsDir+"/bkt/.metadata.json").Return(ns).Once()
	ns.EXPECT().GetLock(mock.Anything, 2*time.Second).Return(nil, fmt.Errorf("%w: busy", domain.ErrLockTimeout)).Once()
	svc, err := bucket.New(bucket.Deps{Objects: objects, Store: newDriveStore(t), Policy: policymock.NewService(t), Locks: locker}, bucket.WithLockTimeout(2*time.Second))
	require.NoError(t, err)
	assert.ErrorIs(t, svc.SetTags(ctx, "bkt", map[string]string{"a": "b"}), domain.ErrLockTimeout)
	assert.ErrorIs(t, svc.SetTags(ctx, "Bad_Name", map[string]string{"a": "b"}), domain.ErrInvalidBucketName)
}

// Invalidate drops cached documents so that changes made elsewhere (a
// peer node) become visible.
func TestInvalidate(t *testing.T) {
	ctx := context.Background()
	st, drives := newStoreWithDrives(t, 1)
	objects := objectmock.NewService(t)
	svc, err := bucket.New(bucket.Deps{Objects: objects, Store: st, Policy: policymock.NewService(t)})
	require.NoError(t, err)
	objects.EXPECT().GetBucketInfo(mock.Anything, mock.Anything).Return(domain.BucketInfo{}, nil).Maybe()
	require.NoError(t, st.Save(ctx, "one", &bucket.Metadata{Versioning: domain.VersioningEnabled}))
	require.NoError(t, st.Save(ctx, "two", &bucket.Metadata{Versioning: domain.VersioningEnabled}))

	// Another node suspends versioning: the documents change on disk.
	for _, name := range []string{"one", "two"} {
		raw, err := json.Marshal(bucket.Metadata{Name: name, Versioning: domain.VersioningSuspended})
		require.NoError(t, err)
		require.NoError(t, drives[0].WriteAll(ctx, domain.SystemBucket, storage.BucketsDir+"/"+name+"/.metadata.json", raw))
	}
	v, err := svc.GetVersioning(ctx, "one")
	require.NoError(t, err)
	assert.Equal(t, domain.VersioningEnabled, v, "served from the cache")

	svc.Invalidate(ctx, "one")
	v, err = svc.GetVersioning(ctx, "one")
	require.NoError(t, err)
	assert.Equal(t, domain.VersioningSuspended, v)
	cfg, err := st.BucketConfig(ctx, "two")
	require.NoError(t, err)
	assert.Equal(t, domain.VersioningEnabled, cfg.Versioning, "other buckets keep their entry")
	svc.Invalidate(ctx, "")
	cfg, err = st.BucketConfig(ctx, "two")
	require.NoError(t, err)
	assert.Equal(t, domain.VersioningSuspended, cfg.Versioning)
}
