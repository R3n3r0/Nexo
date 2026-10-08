package object_test

import (
	"context"
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

// The creation time and region of a bucket are recorded in the system
// volume and survive changes of the bucket directory.
func TestBucketRecord(t *testing.T) {
	forEachImpl(t, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		created := fx.clk.Now()
		require.NoError(t, fx.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{Region: "eu-west-3"}))
		record := filepath.Join(fx.roots[0], domain.SystemBucket, storage.BucketsDir, "bkt", ".bucket.json")
		_, err := os.Stat(record)
		require.NoError(t, err)

		// Writing objects changes the directory mtime, not the creation time.
		fx.clk.Advance(time.Hour)
		put(t, fx.svc, "bkt", "k", []byte("x"))
		require.NoError(t, os.Chtimes(filepath.Join(fx.roots[0], "bkt"), time.Now(), time.Now().Add(time.Hour)))
		info, err := fx.svc.GetBucketInfo(ctx, "bkt")
		require.NoError(t, err)
		assert.True(t, created.Equal(info.Created), "created %s, got %s", created, info.Created)
		assert.Equal(t, "eu-west-3", info.Region)

		// A forced re-creation keeps the original record.
		require.NoError(t, fx.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{ForceCreate: true, Region: "other"}))
		list, err := fx.svc.ListBuckets(ctx)
		require.NoError(t, err)
		require.Len(t, list, 1)
		assert.True(t, created.Equal(list[0].Created))
		assert.Equal(t, "eu-west-3", list[0].Region)

		// Deletion removes the record; a new bucket of the same name is new.
		require.NoError(t, fx.svc.DeleteBucket(ctx, "bkt", object.DeleteBucketOptions{Force: true}))
		_, err = os.Stat(record)
		assert.True(t, os.IsNotExist(err))
		fx.clk.Advance(time.Hour)
		require.NoError(t, fx.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
		info, err = fx.svc.GetBucketInfo(ctx, "bkt")
		require.NoError(t, err)
		assert.True(t, fx.clk.Now().Equal(info.Created))
		assert.Equal(t, "us-east-1", info.Region)

		// A bucket without record (created by an older version) falls back
		// to the directory time.
		require.NoError(t, os.Remove(record))
		info, err = fx.svc.GetBucketInfo(ctx, "bkt")
		require.NoError(t, err)
		assert.False(t, info.Created.IsZero())
		require.NoError(t, os.WriteFile(record, []byte("{broken"), 0o600))
		_, err = fx.svc.GetBucketInfo(ctx, "bkt")
		require.NoError(t, err, "an unreadable record is not fatal")
	})
}

// Bucket names: DNS rules and the reserved system volume.
func TestBucketNames(t *testing.T) {
	fx := newFixture(t, "fs")
	ctx := context.Background()
	for _, name := range []string{"ab", "Upper", "a_b", "-ab", "ab-", "a..b", "192.168.1.1", "xn--abc", domain.SystemBucket, ".hidden", domain.ReservedBucket} {
		assert.ErrorIs(t, fx.svc.MakeBucket(ctx, name, object.MakeBucketOptions{}), domain.ErrInvalidBucketName, name)
		_, err := fx.svc.GetBucketInfo(ctx, name)
		assert.ErrorIs(t, err, domain.ErrInvalidBucketName, name)
		assert.ErrorIs(t, fx.svc.DeleteBucket(ctx, name, object.DeleteBucketOptions{}), domain.ErrInvalidBucketName, name)
	}
	for _, name := range []string{"abc", "my.bucket-1", "0123456789"} {
		require.NoError(t, fx.svc.MakeBucket(ctx, name, object.MakeBucketOptions{}), name)
	}
	// Volumes on disk that are not valid bucket names are not listed.
	require.NoError(t, os.Mkdir(filepath.Join(fx.roots[0], "Not_A_Bucket"), 0o755))
	require.NoError(t, os.Mkdir(filepath.Join(fx.roots[0], domain.ReservedBucket), 0o755))
	list, err := fx.svc.ListBuckets(ctx)
	require.NoError(t, err)
	var names []string
	for _, b := range list {
		names = append(names, b.Name)
	}
	assert.Equal(t, []string{"0123456789", "abc", "my.bucket-1"}, names, "sorted, system volume hidden")
}

// A bucket holding only delete markers or versions is not empty.
func TestDeleteBucketWithVersions(t *testing.T) {
	forEachImpl(t, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		require.NoError(t, fx.svc.MakeBucket(ctx, "ver", object.MakeBucketOptions{}))
		fx.setBucketConfig("ver", object.BucketConfig{Versioning: domain.VersioningEnabled})
		_, err := fx.svc.DeleteObject(ctx, "ver", "ghost", object.DeleteObjectOptions{})
		require.NoError(t, err)
		assert.ErrorIs(t, fx.svc.DeleteBucket(ctx, "ver", object.DeleteBucketOptions{}), domain.ErrBucketNotEmpty)
		require.NoError(t, fx.svc.DeleteBucket(ctx, "ver", object.DeleteBucketOptions{Force: true}))
	})
}

// DataUsage counts objects, versions, delete markers and bytes.
func TestDataUsage(t *testing.T) {
	forEachImpl(t, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		svc := fx.svc
		require.NoError(t, svc.MakeBucket(ctx, "plain", object.MakeBucketOptions{}))
		require.NoError(t, svc.MakeBucket(ctx, "ver", object.MakeBucketOptions{}))
		require.NoError(t, svc.MakeBucket(ctx, "empty", object.MakeBucketOptions{}))
		put(t, svc, "plain", "a", payload(100))
		put(t, svc, "plain", "dir/b", payload(3*testBlockSize))
		put(t, svc, "plain", "dir/sub/c", nil)
		fx.setBucketConfig("ver", object.BucketConfig{Versioning: domain.VersioningEnabled})
		put(t, svc, "ver", "k", payload(10))
		fx.clk.Advance(time.Second)
		put(t, svc, "ver", "k", payload(20))
		fx.clk.Advance(time.Second)
		put(t, svc, "ver", "gone", payload(5))
		fx.clk.Advance(time.Second)
		_, err := svc.DeleteObject(ctx, "ver", "gone", object.DeleteObjectOptions{})
		require.NoError(t, err)

		all, err := svc.DataUsage(ctx, "")
		require.NoError(t, err)
		require.Len(t, all, 3)
		byName := map[string]domain.BucketInfo{}
		for _, b := range all {
			byName[b.Name] = b
		}
		assert.Equal(t, []string{"empty", "plain", "ver"}, []string{all[0].Name, all[1].Name, all[2].Name})
		assert.Equal(t, uint64(3), byName["plain"].Objects)
		assert.Equal(t, uint64(100+3*testBlockSize), byName["plain"].Size)
		assert.Equal(t, uint64(1), byName["ver"].Objects, "gone is hidden by its marker")
		assert.Equal(t, uint64(2), byName["ver"].Versions)
		assert.Equal(t, uint64(1), byName["ver"].DeleteMarkers)
		assert.Equal(t, uint64(35), byName["ver"].Size)
		assert.Equal(t, domain.VersioningEnabled, byName["ver"].Versioning)
		assert.Zero(t, byName["empty"].Objects)

		one, err := svc.DataUsage(ctx, "plain")
		require.NoError(t, err)
		require.Len(t, one, 1)
		assert.Equal(t, byName["plain"], one[0])
		_, err = svc.DataUsage(ctx, "missing")
		assert.ErrorIs(t, err, domain.ErrBucketNotFound)
	})
}
