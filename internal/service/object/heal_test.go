package object_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

func healerOf(t *testing.T, svc object.Service) object.Healer {
	t.Helper()
	h, ok := svc.(object.Healer)
	require.True(t, ok, "the erasure layer implements Healer")
	return h
}

// shardFile returns the path of part 1 of key on drive root.
func shardFile(t *testing.T, root, key string) string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(root, "bkt", key, "*", "part.1"))
	require.NoError(t, err)
	require.Len(t, files, 1, "%s on %s", key, root)
	return files[0]
}

// TestHealObject repairs the damage the TS-E scenarios leave behind: a
// corrupt shard (deep heal), a missing shard file, a drive that missed a
// write, a damaged inline copy, a missing delete marker.
func TestHealObject(t *testing.T) {
	ctx := context.Background()
	for _, l := range setLayouts() {
		t.Run(fmt.Sprintf("%d drives", l.drives), func(t *testing.T) {
			c := newCluster(t, clusterSpec{pools: 1, sets: 1, drives: l.drives})
			objects := seed(t, c.svc)
			h := healerOf(t, c.svc)

			// Nothing to do on a healthy object.
			res, err := h.HealObject(ctx, "bkt", "dir/large", "", object.HealOptions{Deep: true})
			require.NoError(t, err)
			assert.Zero(t, res.Healed)

			// Corrupt shard: found by the deep check only, rebuilt byte for byte.
			path := shardFile(t, c.roots[1], "dir/large")
			want, err := os.ReadFile(path)
			require.NoError(t, err)
			bad := append([]byte(nil), want...)
			bad[len(bad)-3] ^= 0x80
			require.NoError(t, os.WriteFile(path, bad, 0o600))
			res, err = h.HealObject(ctx, "bkt", "dir/large", "", object.HealOptions{})
			require.NoError(t, err)
			assert.Zero(t, res.Healed, "the size is right: only a deep check sees the corruption")
			res, err = h.HealObject(ctx, "bkt", "dir/large", "", object.HealOptions{Deep: true, DryRun: true})
			require.NoError(t, err)
			assert.Equal(t, 1, res.Healed)
			got, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, bad, got, "dry run")
			res, err = h.HealObject(ctx, "bkt", "dir/large", "", object.HealOptions{Deep: true})
			require.NoError(t, err)
			assert.Equal(t, 1, res.Healed)
			got, err = os.ReadFile(shardFile(t, c.roots[1], "dir/large"))
			require.NoError(t, err)
			assert.Equal(t, want, got, "the shard is rebuilt")

			// Missing shard files of a multipart object.
			require.NoError(t, os.Remove(shardFile(t, c.roots[0], "dir/multi")))
			res, err = h.HealObject(ctx, "bkt", "dir/multi", "", object.HealOptions{})
			require.NoError(t, err)
			assert.Equal(t, 1, res.Healed)
			res, err = h.HealObject(ctx, "bkt", "dir/multi", "", object.HealOptions{Deep: true})
			require.NoError(t, err)
			assert.Zero(t, res.Healed)

			// A write missed by one drive (MRF).
			c.setFaults(func(f *driveFaults) { f.offline = true }, 2)
			body := payload(2*testBlockSize + 1)
			put(t, c.svc, "bkt", "dir/large", body)
			objects["dir/large"] = body
			put(t, c.svc, "bkt", "small", []byte("new inline"))
			objects["small"] = []byte("new inline")
			c.setFaults(func(f *driveFaults) { f.offline = false }, 2)
			for _, k := range []string{"dir/large", "small"} {
				res, err = h.HealObject(ctx, "bkt", k, "", object.HealOptions{})
				require.NoError(t, err, k)
				assert.Equal(t, 1, res.Healed, k)
			}
			// Every drive serves the current versions: the others can go.
			c.setFaults(func(f *driveFaults) { f.offline = true }, seq(3, 3+l.parity-1)...)
			c.setFaults(func(f *driveFaults) { f.offline = true }, 0)
			assertReadable(t, c.svc, objects)
			c.setFaults(func(f *driveFaults) { f.offline = false }, seq(0, l.drives)...)

			_, err = h.HealObject(ctx, "bkt", "missing", "", object.HealOptions{})
			assert.ErrorIs(t, err, domain.ErrObjectNotFound)
		})
	}
}

// TestHealVersionsAndDangling heals every version of a versioned key and
// removes a dangling version (left on one drive by a failed write).
func TestHealVersionsAndDangling(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t, clusterSpec{pools: 1, sets: 1, drives: 4, buckets: versionedBuckets(t)})
	require.NoError(t, c.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
	h := healerOf(t, c.svc)

	c.setFaults(func(f *driveFaults) { f.offline = true }, 3)
	v1 := put(t, c.svc, "bkt", "k", payload(3*testBlockSize))
	v2 := put(t, c.svc, "bkt", "k", []byte("inline"))
	del, err := c.svc.DeleteObject(ctx, "bkt", "k", object.DeleteObjectOptions{})
	require.NoError(t, err)
	c.setFaults(func(f *driveFaults) { f.offline = false }, 3)

	res, err := h.HealObject(ctx, "bkt", "k", "", object.HealOptions{AllVersions: true})
	require.NoError(t, err)
	assert.Len(t, res.VersionIDs, 3)
	assert.Equal(t, 3, res.Healed, "the drive missed three versions")
	fis, err := c.drives[3].ReadVersion(ctx, "bkt", "k", v1.VersionID, storage.ReadOptions{})
	require.NoError(t, err)
	assert.Equal(t, v1.Size, fis.Size)
	latest, err := c.drives[3].ReadVersion(ctx, "bkt", "k", "", storage.ReadOptions{})
	require.NoError(t, err)
	assert.True(t, latest.Deleted)
	assert.Equal(t, del.DeleteMarkerVersionID, latest.VersionID)
	_, err = c.drives[3].ReadVersion(ctx, "bkt", "k", v2.VersionID, storage.ReadOptions{})
	require.NoError(t, err)

	// A version written on one drive only: dangling.
	ghost := storage.FileInfo{VersionID: "0192f0f0-0000-7000-8000-000000000001", ModTime: time.Now().UTC(), Size: 5, Data: []byte("ghost"),
		Metadata: map[string]string{"etag": "x"}, Versioned: true}
	require.NoError(t, c.drives[1].WriteMetadata(ctx, "bkt", "k", ghost))
	res, err = h.HealObject(ctx, "bkt", "k", ghost.VersionID, object.HealOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, res.Dangling)
	assert.Zero(t, res.Removed)
	res, err = h.HealObject(ctx, "bkt", "k", "", object.HealOptions{AllVersions: true, Remove: true})
	require.NoError(t, err)
	assert.Equal(t, 1, res.Removed)
	_, err = c.drives[1].ReadVersion(ctx, "bkt", "k", ghost.VersionID, storage.ReadOptions{})
	assert.ErrorIs(t, err, domain.ErrNoSuchVersion)
	versions, err := c.svc.ListObjectVersions(ctx, domain.ListObjectsInput{Bucket: "bkt"})
	require.NoError(t, err)
	assert.Len(t, versions.Objects, 3)
}

// TestHealReplacedDrive is TS-E-05 at the object layer: a drive emptied
// and put back (its system directories recreated, as at startup) gets the
// bucket and every object back from HealBucket and HealObject.
func TestHealReplacedDrive(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t, clusterSpec{pools: 1, sets: 1, drives: 4})
	objects := seed(t, c.svc)
	h := healerOf(t, c.svc)
	root := c.roots[1]
	require.NoError(t, os.RemoveAll(root))
	for _, dir := range []string{storage.TmpDir, storage.MultipartDir, storage.BucketsDir} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, domain.SystemBucket, dir), 0o750))
	}
	res, err := h.HealBucket(ctx, "bkt", object.HealOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, res.Healed)
	_, err = h.HealBucket(ctx, "nope", object.HealOptions{})
	assert.ErrorIs(t, err, domain.ErrBucketNotFound)
	for k := range objects {
		res, err := h.HealObject(ctx, "bkt", k, "", object.HealOptions{})
		require.NoError(t, err, k)
		assert.Equal(t, 1, res.Healed, k)
	}
	// The healed drive alone with D-1 others serves everything.
	c.setFaults(func(f *driveFaults) { f.offline = true }, 0, 2)
	assertReadable(t, c.svc, objects)
	assertNoTmp(t, c.roots)
}

// statesOf returns the before/after states of the drives of a report.
func statesOf(res object.HealResult) (before, after []object.HealDriveState) {
	for _, d := range res.DriveStates {
		before = append(before, d.Before)
		after = append(after, d.After)
	}
	return before, after
}

// TestHealDriveStates checks the per-drive report of the healer: the state
// of every drive before and after the repair, the dry run leaving them
// unchanged, offline drives reported as such.
func TestHealDriveStates(t *testing.T) {
	ctx := context.Background()
	const (
		ok      = object.HealDriveOK
		missing = object.HealDriveMissing
		corrupt = object.HealDriveCorrupt
		offline = object.HealDriveOffline
	)
	c := newCluster(t, clusterSpec{pools: 1, sets: 1, drives: 4})
	seed(t, c.svc)
	h := healerOf(t, c.svc)

	res, err := h.HealObject(ctx, "bkt", "dir/large", "", object.HealOptions{Deep: true})
	require.NoError(t, err)
	before, after := statesOf(res)
	assert.Equal(t, []object.HealDriveState{ok, ok, ok, ok}, before)
	assert.Equal(t, before, after)
	assert.Equal(t, int64(5*testBlockSize+77), res.Bytes)
	for i, d := range res.DriveStates {
		assert.Equal(t, i, d.Disk)
		assert.NotEmpty(t, d.Endpoint)
	}

	// Bitrot on drive 2, a missing shard file on drive 1, drive 4 offline.
	path := shardFile(t, c.roots[1], "dir/large")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	data[10] ^= 0x01
	require.NoError(t, os.WriteFile(path, data, 0o600))
	require.NoError(t, os.Remove(shardFile(t, c.roots[0], "dir/large")))
	c.setFaults(func(f *driveFaults) { f.offline = true }, 3)

	res, err = h.HealObject(ctx, "bkt", "dir/large", "", object.HealOptions{Deep: true, DryRun: true})
	require.NoError(t, err)
	before, after = statesOf(res)
	assert.Equal(t, []object.HealDriveState{missing, corrupt, ok, offline}, before)
	assert.Equal(t, before, after, "dry run")
	assert.Equal(t, 1, res.Offline)

	// Two intact shards (D) are left once drive 4 is back: enough to rebuild.
	c.setFaults(func(f *driveFaults) { f.offline = false }, 3)
	res, err = h.HealObject(ctx, "bkt", "dir/large", "", object.HealOptions{Deep: true})
	require.NoError(t, err)
	before, after = statesOf(res)
	assert.Equal(t, []object.HealDriveState{missing, corrupt, ok, ok}, before)
	assert.Equal(t, []object.HealDriveState{ok, ok, ok, ok}, after)
	assert.Equal(t, 2, res.Healed)

	// A bucket missing on one drive.
	require.NoError(t, os.RemoveAll(filepath.Join(c.roots[2], "bkt")))
	res, err = h.HealBucket(ctx, "bkt", object.HealOptions{})
	require.NoError(t, err)
	before, after = statesOf(res)
	assert.Equal(t, []object.HealDriveState{ok, ok, missing, ok}, before)
	assert.Equal(t, []object.HealDriveState{ok, ok, ok, ok}, after)
	assert.Equal(t, 1, res.Healed)
}

// TestHealDriveWithoutBucket: a drive offline when the bucket and its
// objects were written misses the bucket itself; healing an object
// recreates the bucket there (with its record) instead of failing until
// the bucket is healed.
func TestHealDriveWithoutBucket(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t, clusterSpec{pools: 1, sets: 1, drives: 4})
	c.setFaults(func(f *driveFaults) { f.offline = true }, 1)
	objects := seed(t, c.svc)
	c.setFaults(func(f *driveFaults) { f.offline = false }, 1)
	_, err := os.Stat(filepath.Join(c.roots[1], "bkt"))
	require.ErrorIs(t, err, os.ErrNotExist, "the drive was offline at the creation")

	h := healerOf(t, c.svc)
	for k := range objects {
		res, err := h.HealObject(ctx, "bkt", k, "", object.HealOptions{})
		require.NoError(t, err, k)
		assert.Equal(t, 1, res.Healed, k)
		before, after := statesOf(res)
		assert.Equal(t, object.HealDriveMissing, before[1], k)
		assert.Equal(t, object.HealDriveOK, after[1], k)
	}
	_, err = os.Stat(filepath.Join(c.roots[1], domain.SystemBucket, storage.BucketsDir, "bkt"))
	assert.NoError(t, err, "the bucket record is restored with the bucket")
	// The healed drive alone with D-1 others serves everything.
	c.setFaults(func(f *driveFaults) { f.offline = true }, 0, 2)
	assertReadable(t, c.svc, objects)
}
