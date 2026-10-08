package bucket_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/storage"
	"github.com/r3n3r0/nexo/internal/testutil"
)

func newStoreWithDrives(t *testing.T, n int) (bucket.MetadataStore, []storage.StorageAPI) {
	t.Helper()
	base := t.TempDir()
	drives := make([]storage.StorageAPI, 0, n)
	for i := 0; i < n; i++ {
		drives = append(drives, testutil.NewLocalDrive(t, filepath.Join(base, "d"+string(rune('1'+i)))))
	}
	st, err := bucket.NewStore(bucket.StoreDeps{Drives: drives, Logger: slog.New(slog.DiscardHandler)})
	require.NoError(t, err)
	return st, drives
}

func TestNewStoreRequiresDrives(t *testing.T) {
	_, err := bucket.NewStore(bucket.StoreDeps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

func TestMetadataStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	st, drives := newStoreWithDrives(t, 3)

	m, err := st.Load(ctx, "photos")
	require.NoError(t, err)
	assert.Equal(t, "photos", m.Name)
	assert.Equal(t, domain.VersioningOff, m.Versioning)
	_, err = st.Load(ctx, "x")
	assert.ErrorIs(t, err, domain.ErrInvalidBucketName)
	assert.ErrorIs(t, st.Save(ctx, "photos", nil), domain.ErrInvalidArgument)

	doc := &bucket.Metadata{
		Created:    time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC),
		Region:     "eu-south-1",
		Versioning: domain.VersioningEnabled,
		ObjectLock: bucket.ObjectLockConfig{Enabled: true, Mode: bucket.RetentionGovernance, Days: 7},
		Policy:     json.RawMessage(`{"Version":"2012-10-17"}`),
		Quota:      bucket.Quota{Type: bucket.QuotaHard, Size: 99},
		Tags:       map[string]string{"a": "1"},
		Lifecycle:  []byte("<LifecycleConfiguration/>"),
	}
	require.NoError(t, st.Save(ctx, "photos", doc))
	assert.Equal(t, "photos", doc.Name, "Save fixes the name")

	// Every drive carries the document.
	for _, d := range drives {
		data, err := d.ReadAll(ctx, domain.SystemBucket, storage.BucketsDir+"/photos/.metadata.json")
		require.NoError(t, err)
		var onDisk bucket.Metadata
		require.NoError(t, json.Unmarshal(data, &onDisk))
		assert.Equal(t, "eu-south-1", onDisk.Region)
	}

	got, err := st.Load(ctx, "photos")
	require.NoError(t, err)
	assert.Equal(t, doc, got)
	// The cache returns copies.
	got.Tags["b"] = "2"
	again, err := st.Load(ctx, "photos")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"a": "1"}, again.Tags)

	cfg, err := st.BucketConfig(ctx, "photos")
	require.NoError(t, err)
	assert.Equal(t, domain.VersioningEnabled, cfg.Versioning)
	assert.True(t, cfg.ObjectLock)
	assert.Equal(t, uint64(99), cfg.QuotaBytes)

	// A fresh store reads what was persisted (no cache).
	st2, err := bucket.NewStore(bucket.StoreDeps{Drives: drives})
	require.NoError(t, err)
	got, err = st2.Load(ctx, "photos")
	require.NoError(t, err)
	assert.Equal(t, doc, got)

	// One drive offline: a majority still answers; the surviving drives are updated.
	require.NoError(t, drives[2].Close(ctx))
	doc.Region = "us-east-2"
	require.NoError(t, st.Save(ctx, "photos", doc))
	got, err = st2.Load(ctx, "photos")
	require.NoError(t, err)
	assert.Equal(t, "eu-south-1", got.Region, "a second store keeps its own cache (single-node assumption)")
	fresh, err := bucket.NewStore(bucket.StoreDeps{Drives: drives})
	require.NoError(t, err)
	got, err = fresh.Load(ctx, "photos")
	require.NoError(t, err)
	assert.Equal(t, "us-east-2", got.Region, "the surviving drives carry the update")

	// Two drives offline: no majority.
	require.NoError(t, drives[1].Close(ctx))
	assert.ErrorIs(t, st.Save(ctx, "photos", doc), domain.ErrWriteQuorum)
	got, err = st.Load(ctx, "photos")
	require.NoError(t, err, "reads need one drive")
	assert.Equal(t, "us-east-2", got.Region)

	// Delete is idempotent and clears the cache.
	st3, drives3 := newStoreWithDrives(t, 1)
	require.NoError(t, st3.Save(ctx, "docs", &bucket.Metadata{Region: "r"}))
	require.NoError(t, st3.Delete(ctx, "docs"))
	require.NoError(t, st3.Delete(ctx, "docs"))
	got, err = st3.Load(ctx, "docs")
	require.NoError(t, err)
	assert.Empty(t, got.Region)
	_, err = drives3[0].ReadAll(ctx, domain.SystemBucket, storage.BucketsDir+"/docs/.metadata.json")
	assert.ErrorIs(t, err, domain.ErrObjectNotFound)

	// A corrupt document on one drive is skipped in favor of a readable one.
	st4, drives4 := newStoreWithDrives(t, 2)
	require.NoError(t, st4.Save(ctx, "mixed", &bucket.Metadata{Region: "ok"}))
	require.NoError(t, drives4[0].WriteAll(ctx, domain.SystemBucket, storage.BucketsDir+"/mixed/.metadata.json", []byte("{")))
	st5, err := bucket.NewStore(bucket.StoreDeps{Drives: drives4})
	require.NoError(t, err)
	got, err = st5.Load(ctx, "mixed")
	require.NoError(t, err)
	assert.Equal(t, "ok", got.Region)
}

// Loads racing with saves and invalidations never leave a stale document in
// the cache: once the writer is done, every load sees its last save.
func TestStoreCacheNeverGoesStale(t *testing.T) {
	ctx := context.Background()
	st, _ := newStoreWithDrives(t, 2)
	const saves = 200
	done := make(chan struct{})
	var wg sync.WaitGroup
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				if r == 0 {
					st.Invalidate(ctx, "bkt")
				}
				m, err := st.Load(ctx, "bkt")
				if !assert.NoError(t, err) {
					return
				}
				assert.LessOrEqual(t, m.Quota.Size, uint64(saves))
			}
		}(r)
	}
	for i := 1; i <= saves; i++ {
		require.NoError(t, st.Save(ctx, "bkt", &bucket.Metadata{Quota: bucket.Quota{Type: bucket.QuotaHard, Size: uint64(i)}}))
	}
	close(done)
	wg.Wait()
	m, err := st.Load(ctx, "bkt")
	require.NoError(t, err)
	assert.Equal(t, uint64(saves), m.Quota.Size)
	cfg, err := st.BucketConfig(ctx, "bkt")
	require.NoError(t, err)
	assert.Equal(t, uint64(saves), cfg.QuotaBytes)
	_, err = st.BucketConfig(ctx, "x")
	assert.ErrorIs(t, err, domain.ErrInvalidBucketName)
	assert.ErrorIs(t, st.Delete(ctx, "x"), domain.ErrInvalidBucketName)
	assert.ErrorIs(t, st.Save(ctx, "x", &bucket.Metadata{}), domain.ErrInvalidBucketName)
}
