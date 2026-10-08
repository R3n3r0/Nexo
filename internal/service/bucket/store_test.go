package bucket

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

func newStoreWithDrives(t *testing.T, n int) (MetadataStore, []storage.StorageAPI) {
	t.Helper()
	base := t.TempDir()
	drives := make([]storage.StorageAPI, 0, n)
	for i := 0; i < n; i++ {
		ep, err := domain.ParseEndpoint(filepath.Join(base, "d"+string(rune('1'+i))))
		require.NoError(t, err)
		d, err := storage.NewLocal(storage.Deps{Logger: slog.New(slog.DiscardHandler)}, ep, storage.WithoutFsync())
		require.NoError(t, err)
		drives = append(drives, d)
	}
	st, err := NewStore(StoreDeps{Drives: drives, Logger: slog.New(slog.DiscardHandler)})
	require.NoError(t, err)
	return st, drives
}

func TestNewStoreRequiresDrives(t *testing.T) {
	_, err := NewStore(StoreDeps{})
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

	doc := &Metadata{
		Created:    time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC),
		Region:     "eu-south-1",
		Versioning: domain.VersioningEnabled,
		ObjectLock: ObjectLockConfig{Enabled: true, Mode: RetentionGovernance, Days: 7},
		Policy:     json.RawMessage(`{"Version":"2012-10-17"}`),
		Quota:      Quota{Type: QuotaHard, Size: 99},
		Tags:       map[string]string{"a": "1"},
		Lifecycle:  []byte("<LifecycleConfiguration/>"),
	}
	require.NoError(t, st.Save(ctx, "photos", doc))
	assert.Equal(t, "photos", doc.Name, "Save fixes the name")

	// Every drive carries the document.
	for _, d := range drives {
		data, err := d.ReadAll(ctx, domain.SystemBucket, storage.BucketsDir+"/photos/metadata.json")
		require.NoError(t, err)
		var onDisk Metadata
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
	st2, err := NewStore(StoreDeps{Drives: drives})
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
	fresh, err := NewStore(StoreDeps{Drives: drives})
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
	require.NoError(t, st3.Save(ctx, "docs", &Metadata{Region: "r"}))
	require.NoError(t, st3.Delete(ctx, "docs"))
	require.NoError(t, st3.Delete(ctx, "docs"))
	got, err = st3.Load(ctx, "docs")
	require.NoError(t, err)
	assert.Empty(t, got.Region)
	_, err = drives3[0].ReadAll(ctx, domain.SystemBucket, storage.BucketsDir+"/docs/metadata.json")
	assert.ErrorIs(t, err, domain.ErrObjectNotFound)

	// A corrupt document on one drive is skipped in favor of a readable one.
	st4, drives4 := newStoreWithDrives(t, 2)
	require.NoError(t, st4.Save(ctx, "mixed", &Metadata{Region: "ok"}))
	require.NoError(t, drives4[0].WriteAll(ctx, domain.SystemBucket, storage.BucketsDir+"/mixed/metadata.json", []byte("{")))
	st5, err := NewStore(StoreDeps{Drives: drives4})
	require.NoError(t, err)
	got, err = st5.Load(ctx, "mixed")
	require.NoError(t, err)
	assert.Equal(t, "ok", got.Region)
}
