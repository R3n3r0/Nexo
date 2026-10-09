package object_test

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// TestHealPurgesOrphanDataDirs: healing every version of a key removes,
// on every drive, the data directories that no version references once
// they are older than OrphanDataDirGrace (the data of a replaced version
// whose purge failed, or of a crashed commit, which nothing else
// reclaims); recent ones, directories that are not data directories, a
// dry run and a set with an offline drive are left alone. (Regression:
// such directories leaked forever.)
func TestHealPurgesOrphanDataDirs(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	clk := clock.NewFake(start)
	drives, roots := newDrives(t, 4)
	newPools := func(set []storage.StorageAPI) object.Service {
		svc, err := object.NewServerPools(object.PoolsDeps{
			Pools: []object.PoolSpec{{Sets: [][]storage.StorageAPI{set}}}, DeploymentID: "dep", Codecs: testCodecs("dep"),
			Clock: clk, Logger: slog.New(slog.DiscardHandler),
		}, object.WithInlineLimit(0), object.WithBlockSize(testBlockSize))
		require.NoError(t, err)
		return svc
	}
	svc := newPools(drives)
	require.NoError(t, svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
	put(t, svc, "bkt", "a", []byte("object a"))

	// On every drive: an old orphan, a recent one, and a UUID-named
	// directory holding metadata (never a data directory).
	old, recent, nested := uuid.NewString(), uuid.NewString(), uuid.NewString()
	for _, root := range roots {
		for _, dir := range []string{old, recent, nested} {
			require.NoError(t, os.MkdirAll(filepath.Join(root, "bkt", "a", dir), 0o755))
		}
		require.NoError(t, os.WriteFile(filepath.Join(root, "bkt", "a", old, "part.1"), []byte("stale"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(root, "bkt", "a", recent, "part.1"), []byte("fresh"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(root, "bkt", "a", nested, "xl.meta"), []byte("x"), 0o600))
		past := time.Now().Add(-2 * object.OrphanDataDirGrace)
		require.NoError(t, os.Chtimes(filepath.Join(root, "bkt", "a", old), past, past))
		require.NoError(t, os.Chtimes(filepath.Join(root, "bkt", "a", nested), past, past))
	}
	exists := func(root, dir string) bool {
		_, err := os.Stat(filepath.Join(root, "bkt", "a", dir))
		return err == nil
	}
	healer := func(s object.Service) object.Healer { return s.(object.Healer) }
	clk.Set(time.Now()) // the cutoff is relative to the layer clock

	r, err := healer(svc).HealObject(ctx, "bkt", "a", "", object.HealOptions{AllVersions: true, DryRun: true})
	require.NoError(t, err)
	assert.Zero(t, r.Orphans, "a dry run removes nothing")

	// A drive offline: its references are unknown, nothing is removed.
	offline := append([]storage.StorageAPI{nil}, drives[1:]...)
	r, err = healer(newPools(offline)).HealObject(ctx, "bkt", "a", "", object.HealOptions{AllVersions: true})
	require.NoError(t, err)
	assert.Zero(t, r.Orphans)
	assert.True(t, exists(roots[1], old))

	r, err = healer(svc).HealObject(ctx, "bkt", "a", "", object.HealOptions{AllVersions: true})
	require.NoError(t, err)
	assert.Equal(t, 4, r.Orphans, "the old orphan of every drive")
	for _, root := range roots {
		assert.False(t, exists(root, old))
		assert.True(t, exists(root, recent), "younger than the grace period")
		assert.True(t, exists(root, nested), "not a data directory")
	}
	got, _ := get(t, svc, "bkt", "a", object.GetObjectOptions{})
	assert.Equal(t, "object a", string(got))
}
