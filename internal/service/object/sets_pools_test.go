package object_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// workloadKeys returns a deterministic mix of keys exercising the listing
// rules: nested prefixes, keys sharing a prefix with a prefix ("a-b",
// "a/..."), deep and shallow keys.
func workloadKeys(n int) []string {
	rng := rand.New(rand.NewPCG(7, 11))
	parts := []string{"a", "a-b", "b", "photos", "2024", "x.jpg", "z", "dir"}
	seen := map[string]bool{}
	var out []string
	for len(out) < n {
		depth := 1 + rng.IntN(3)
		segs := make([]string, depth)
		for i := range segs {
			segs[i] = parts[rng.IntN(len(parts))]
		}
		segs[depth-1] += fmt.Sprintf("%d", rng.IntN(40))
		k := strings.Join(segs, "/")
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

// listingQueries are the listings compared between implementations.
func listingQueries() []domain.ListObjectsInput {
	var out []domain.ListObjectsInput
	for _, maxKeys := range []int{1, 7, 1000} {
		for _, prefix := range []string{"", "a", "a/", "photos/2024", "dir/"} {
			for _, delim := range []string{"", "/"} {
				out = append(out, domain.ListObjectsInput{Bucket: "bkt", Prefix: prefix, Delimiter: delim, MaxKeys: maxKeys})
			}
		}
	}
	out = append(out,
		domain.ListObjectsInput{Bucket: "bkt", StartAfter: "a/b", MaxKeys: 5},
		domain.ListObjectsInput{Bucket: "bkt", StartAfter: "photos/", Delimiter: "/", MaxKeys: 3},
		domain.ListObjectsInput{Bucket: "bkt", Marker: "b", Delimiter: "/", MaxKeys: 2},
	)
	return out
}

// listEverything pages through a listing (v2 or versions) and returns a
// printable trace of every page.
func listEverything(t *testing.T, svc object.Service, in domain.ListObjectsInput, versions bool) []string {
	t.Helper()
	ctx := context.Background()
	var trace []string
	for page := 0; page < 5000; page++ {
		var (
			res domain.ListObjectsResult
			err error
		)
		if versions {
			res, err = svc.ListObjectVersions(ctx, in)
		} else {
			res, err = svc.ListObjects(ctx, in)
		}
		require.NoError(t, err)
		for _, o := range res.Objects {
			trace = append(trace, fmt.Sprintf("%s v=%s latest=%t dm=%t size=%d etag=%s", o.Key, o.VersionID, o.IsLatest, o.IsDeleteMarker, o.Size, o.ETag))
		}
		for _, p := range res.CommonPrefixes {
			trace = append(trace, "prefix "+p)
		}
		trace = append(trace, fmt.Sprintf("-- truncated=%t next=%s/%s", res.IsTruncated, res.NextMarker, res.NextVersionIDMarker))
		if !res.IsTruncated {
			return trace
		}
		if versions {
			in.Marker, in.VersionIDMarker = res.NextMarker, res.NextVersionIDMarker
		} else {
			in.ContinuationToken = res.NextContinuationToken
		}
	}
	t.Fatal("listing does not end")
	return nil
}

// TestErasureSetsEquivalence spreads a workload over 2 pools × 3 sets × 4
// drives and checks that every listing (flat, delimited, paginated,
// markers, versions) is the one of the single-drive layer: the heap merge
// of the sets and pools is invisible.
func TestErasureSetsEquivalence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	spec := clusterSpec{pools: 2, sets: 3, drives: 4}
	c := newCluster(t, spec)
	ref := newFixture(t, "fs")
	keys := workloadKeys(150)
	for _, svc := range []object.Service{c.svc, ref.svc} {
		require.NoError(t, svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
		for _, k := range keys {
			put(t, svc, "bkt", k, []byte(k))
		}
	}
	perSet := map[string]int{}
	for i, root := range c.roots {
		if i%spec.drives != 0 {
			continue
		}
		_ = filepath.WalkDir(filepath.Join(root, "bkt"), func(path string, d os.DirEntry, err error) error {
			if err == nil && d.Name() == storage.MetaFile {
				perSet[root]++
			}
			return nil
		})
	}
	total := 0
	for _, n := range perSet {
		total += n
	}
	assert.Equal(t, len(keys), total, "every key lives in exactly one set")
	assert.GreaterOrEqual(t, len(perSet), 4, "keys spread over the sets of both pools: %v", perSet)

	for _, q := range listingQueries() {
		name := fmt.Sprintf("prefix=%q delim=%q max=%d after=%q marker=%q", q.Prefix, q.Delimiter, q.MaxKeys, q.StartAfter, q.Marker)
		want := listEverything(t, ref.svc, q, false)
		got := listEverything(t, c.svc, q, false)
		require.Equal(t, want, got, name)
	}
	for _, k := range keys[:20] {
		got, _ := get(t, c.svc, "bkt", k, object.GetObjectOptions{})
		assert.Equal(t, k, string(got))
	}
	usage, err := c.svc.DataUsage(ctx, "bkt")
	require.NoError(t, err)
	assert.Equal(t, uint64(len(keys)), usage[0].Objects)
}

// TestVersionListingAcrossSets compares the version listings of a
// versioned bucket spread over sets with the single-drive layer.
func TestVersionListingAcrossSets(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t, clusterSpec{pools: 1, sets: 3, drives: 4, buckets: versionedBuckets(t)})
	ref := newFixture(t, "fs")
	ref.setBucketConfig("bkt", object.BucketConfig{Versioning: domain.VersioningEnabled})
	keys := workloadKeys(40)
	for _, svc := range []object.Service{c.svc, ref.svc} {
		require.NoError(t, svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
	}
	for round := 0; round < 3; round++ {
		for i, k := range keys {
			for _, svc := range []object.Service{c.svc, ref.svc} {
				if (i+round)%5 == 0 {
					_, err := svc.DeleteObject(ctx, "bkt", k, object.DeleteObjectOptions{})
					require.NoError(t, err)
					continue
				}
				put(t, svc, "bkt", k, []byte(fmt.Sprintf("%s-%d", k, round)))
			}
		}
	}
	normalize := func(trace []string) []string {
		out := make([]string, len(trace))
		for i, line := range trace {
			// Version ids are generated independently: compare the shape.
			if j := strings.Index(line, " v="); j >= 0 {
				if e := strings.Index(line[j+3:], " "); e >= 0 {
					line = line[:j] + " v=*" + line[j+3+e:]
				}
			}
			if j := strings.Index(line, "next="); j >= 0 {
				if s := strings.LastIndex(line, "/"); s > j {
					line = line[:s]
				}
			}
			out[i] = line
		}
		return out
	}
	for _, maxKeys := range []int{1, 4, 1000} {
		for _, delim := range []string{"", "/"} {
			q := domain.ListObjectsInput{Bucket: "bkt", MaxKeys: maxKeys, Delimiter: delim}
			want := normalize(listEverything(t, ref.svc, q, true))
			got := normalize(listEverything(t, c.svc, q, true))
			require.Equal(t, want, got, "versions max=%d delim=%q", maxKeys, delim)
			want = listEverything(t, ref.svc, q, false)
			got = listEverything(t, c.svc, q, false)
			require.Equal(t, normalize(want), normalize(got), "objects max=%d delim=%q", maxKeys, delim)
		}
	}
}

// TestBucketOperationsAcrossSets: buckets exist on every set or none:
// MakeBucket needs the write quorum of every set (and is undone
// otherwise), ListBuckets merges the sets, DeleteBucket is undone when a
// set cannot remove the bucket.
func TestBucketOperationsAcrossSets(t *testing.T) {
	ctx := context.Background()
	spec := clusterSpec{pools: 1, sets: 3, drives: 4}
	c := newCluster(t, spec)
	require.NoError(t, c.svc.MakeBucket(ctx, "one", object.MakeBucketOptions{}))
	for _, root := range c.roots {
		assert.DirExists(t, filepath.Join(root, "one"))
	}

	// The last set loses its quorum: the creation fails and leaves nothing.
	lastSet := seq(2*spec.drives, 3*spec.drives)
	c.setFaults(func(f *driveFaults) { f.offline = true }, lastSet[:2]...)
	err := c.svc.MakeBucket(ctx, "two", object.MakeBucketOptions{})
	require.ErrorIs(t, err, domain.ErrWriteQuorum)
	for _, root := range c.roots {
		assert.NoDirExists(t, filepath.Join(root, "two"))
	}
	c.setFaults(func(f *driveFaults) { f.offline = false }, lastSet[:2]...)

	require.NoError(t, c.svc.MakeBucket(ctx, "three", object.MakeBucketOptions{}))
	buckets, err := c.svc.ListBuckets(ctx)
	require.NoError(t, err)
	names := make([]string, 0, len(buckets))
	for _, b := range buckets {
		names = append(names, b.Name)
	}
	assert.Equal(t, []string{"one", "three"}, names)

	// A bucket holding an object in any set is not empty.
	put(t, c.svc, "one", "k", []byte("v"))
	err = c.svc.DeleteBucket(ctx, "one", object.DeleteBucketOptions{})
	require.ErrorIs(t, err, domain.ErrBucketNotEmpty)

	// A set that cannot remove the bucket: it is put back on the others.
	c.setFaults(func(f *driveFaults) { f.offline = true }, lastSet[:2]...)
	err = c.svc.DeleteBucket(ctx, "three", object.DeleteBucketOptions{})
	require.Error(t, err)
	c.setFaults(func(f *driveFaults) { f.offline = false }, lastSet[:2]...)
	for _, root := range c.roots {
		assert.DirExists(t, filepath.Join(root, "three"))
	}
	_, err = c.svc.GetBucketInfo(ctx, "three")
	require.NoError(t, err)
	require.NoError(t, c.svc.DeleteBucket(ctx, "three", object.DeleteBucketOptions{}))
	for _, root := range c.roots {
		assert.NoDirExists(t, filepath.Join(root, "three"))
	}
}

// diskOf returns a DiskInfo with the given capacity (GiB).
func diskOf(total, free float64) *domain.DiskInfo {
	const gib = 1 << 30
	return &domain.DiskInfo{Total: uint64(total * gib), Free: uint64(free * gib), Used: uint64((total - free) * gib), State: domain.StateOnline}
}

// setPoolSpace makes every drive of pool p report the given capacity.
func (c *cluster) setPoolSpace(spec clusterSpec, p int, total, free float64) {
	per := spec.sets * spec.drives
	c.setFaults(func(f *driveFaults) { f.diskInfo = diskOf(total, free) }, seq(p*per, (p+1)*per)...)
}

// TestPoolPlacement covers the choice of the pool of new objects: random,
// weighted by free space, never a pool over 95% or being decommissioned.
func TestPoolPlacement(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tests := []struct {
		name     string
		free     [2]float64
		decom    map[int]bool
		wantPool map[int][2]int // pool → [min, max] of 300 objects
		wantErr  error
	}{
		{name: "weighted by free space", free: [2]float64{1, 3}, wantPool: map[int][2]int{0: {40, 110}, 1: {190, 260}}},
		{name: "full pool excluded", free: [2]float64{2, 0.4}, wantPool: map[int][2]int{0: {300, 300}}},
		{name: "decommissioning pool excluded", free: [2]float64{5, 1}, decom: map[int]bool{0: true}, wantPool: map[int][2]int{1: {300, 300}}},
		{name: "no room anywhere", free: [2]float64{0.1, 0.2}, wantErr: domain.ErrStorageFull},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := clusterSpec{pools: 2, sets: 1, drives: 4, decommissioning: tt.decom, opts: []object.Option{object.WithInlineLimit(1 << 10)}}
			c := newCluster(t, spec)
			require.NoError(t, c.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
			c.setPoolSpace(spec, 0, 10, tt.free[0])
			c.setPoolSpace(spec, 1, 10, tt.free[1])
			if tt.wantErr != nil {
				_, err := c.svc.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: "k", Reader: bytes.NewReader([]byte("x")), Size: 1})
				require.ErrorIs(t, err, tt.wantErr)
				return
			}
			counts := map[int]int{}
			for i := 0; i < 300; i++ {
				key := fmt.Sprintf("k%03d", i)
				put(t, c.svc, "bkt", key, []byte(key))
				pools := c.poolOf(t, spec, "bkt", key)
				require.Len(t, pools, 1)
				counts[pools[0]]++
			}
			for p, bounds := range tt.wantPool {
				assert.GreaterOrEqual(t, counts[p], bounds[0], "pool %d: %v", p, counts)
				assert.LessOrEqual(t, counts[p], bounds[1], "pool %d: %v", p, counts)
			}
		})
	}
}

// TestPoolsKeepObjectsInPlace: an existing object (any version) stays in
// its pool whatever the free space says, multipart uploads stay in the
// pool chosen at their creation, reads find objects in any pool.
func TestPoolsKeepObjectsInPlace(t *testing.T) {
	ctx := context.Background()
	spec := clusterSpec{pools: 2, sets: 2, drives: 4, buckets: versionedBuckets(t), opts: []object.Option{object.WithPoolSpaceTTL(0)}}
	c := newCluster(t, spec)
	require.NoError(t, c.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))

	// Everything new goes to pool 1.
	c.setPoolSpace(spec, 0, 10, 0.1)
	c.setPoolSpace(spec, 1, 10, 9)
	v1 := put(t, c.svc, "bkt", "obj", payload(3*testBlockSize))
	assert.Equal(t, []int{1}, c.poolOf(t, spec, "bkt", "obj"))
	up, err := c.svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "bkt", Key: "mp"})
	require.NoError(t, err)

	// Now pool 0 is the only one with room for new objects.
	c.setPoolSpace(spec, 0, 10, 9)
	c.setPoolSpace(spec, 1, 10, 0.1)
	put(t, c.svc, "bkt", "probe", []byte("p"))
	assert.Equal(t, []int{0}, c.poolOf(t, spec, "bkt", "probe"))
	v2 := put(t, c.svc, "bkt", "obj", payload(2*testBlockSize))
	assert.Equal(t, []int{1}, c.poolOf(t, spec, "bkt", "obj"), "a new version stays with the others")
	_, err = c.svc.DeleteObject(ctx, "bkt", "obj", object.DeleteObjectOptions{})
	require.NoError(t, err)
	assert.Equal(t, []int{1}, c.poolOf(t, spec, "bkt", "obj"), "the delete marker too")

	for i := 1; i <= 2; i++ {
		_, err := c.svc.PutObjectPart(ctx, object.PutObjectPartInput{Bucket: "bkt", Key: "mp", UploadID: up.UploadID, PartNumber: i, Reader: bytes.NewReader(payload(40)), Size: 40})
		require.NoError(t, err)
	}
	parts, err := c.svc.ListObjectParts(ctx, domain.ListPartsInput{Bucket: "bkt", Key: "mp", UploadID: up.UploadID})
	require.NoError(t, err)
	require.Len(t, parts.Parts, 2)
	uploads, err := c.svc.ListMultipartUploads(ctx, domain.ListMultipartUploadsInput{Bucket: "bkt"})
	require.NoError(t, err)
	require.Len(t, uploads.Uploads, 1)
	_, err = c.svc.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: "bkt", Key: "mp", UploadID: up.UploadID,
		Parts: []domain.CompletePart{{Number: 1, ETag: parts.Parts[0].ETag}, {Number: 2, ETag: parts.Parts[1].ETag}}})
	require.NoError(t, err)
	assert.Equal(t, []int{1}, c.poolOf(t, spec, "bkt", "mp"), "the upload completes in its pool")

	// Reads by version find the pool.
	for _, v := range []domain.ObjectInfo{v1, v2} {
		info, err := c.svc.GetObjectInfo(ctx, "bkt", "obj", object.GetObjectOptions{VersionID: v.VersionID})
		require.NoError(t, err)
		assert.Equal(t, v.Size, info.Size)
	}
	_, err = c.svc.GetObjectInfo(ctx, "bkt", "obj", object.GetObjectOptions{})
	assert.ErrorIs(t, err, domain.ErrObjectNotFound, "latest is a delete marker")
	res, err := c.svc.ListObjectVersions(ctx, domain.ListObjectsInput{Bucket: "bkt", Prefix: "obj"})
	require.NoError(t, err)
	assert.Len(t, res.Objects, 3)
	list, err := c.svc.ListObjects(ctx, domain.ListObjectsInput{Bucket: "bkt"})
	require.NoError(t, err)
	assert.Equal(t, []string{"mp", "probe"}, keysOf(list))
}

// TestPoolsDuplicateKeys: a key written in two pools (two first writes
// racing to different pools) reads as its newest copy, and the next write
// of the null version leaves it in a single pool.
func TestPoolsDuplicateKeys(t *testing.T) {
	ctx := context.Background()
	spec := clusterSpec{pools: 2, sets: 1, drives: 4}
	c := newCluster(t, spec)
	require.NoError(t, c.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
	// Single-pool layers over the drives of each pool write the copies.
	for p, body := range []string{"older", "newer"} {
		single, err := object.NewServerPools(object.PoolsDeps{
			Pools:        []object.PoolSpec{{Sets: [][]storage.StorageAPI{c.drives[p*4 : (p+1)*4]}}},
			DeploymentID: "deployment-test", Codecs: testCodecs("deployment-test"), Logger: slog.New(slog.DiscardHandler),
		}, object.WithInlineLimit(testInlineLimit), object.WithBlockSize(testBlockSize))
		require.NoError(t, err)
		put(t, single, "bkt", "dup", []byte(body))
	}
	assert.Equal(t, []int{0, 1}, c.poolOf(t, spec, "bkt", "dup"))
	got, _ := get(t, c.svc, "bkt", "dup", object.GetObjectOptions{})
	assert.Equal(t, "newer", string(got))
	list, err := c.svc.ListObjects(ctx, domain.ListObjectsInput{Bucket: "bkt"})
	require.NoError(t, err)
	require.Len(t, list.Objects, 1)
	assert.Equal(t, int64(len("newer")), list.Objects[0].Size)
	versions, err := c.svc.ListObjectVersions(ctx, domain.ListObjectsInput{Bucket: "bkt"})
	require.NoError(t, err)
	require.Len(t, versions.Objects, 1, "the null version is listed once")
	assert.Equal(t, int64(len("newer")), versions.Objects[0].Size)

	put(t, c.svc, "bkt", "dup", []byte("final"))
	assert.Equal(t, []int{1}, c.poolOf(t, spec, "bkt", "dup"))
	got, _ = get(t, c.svc, "bkt", "dup", object.GetObjectOptions{})
	assert.Equal(t, "final", string(got))
	_, err = c.svc.DeleteObject(ctx, "bkt", "dup", object.DeleteObjectOptions{})
	require.NoError(t, err)
	_, err = c.svc.GetObjectInfo(ctx, "bkt", "dup", object.GetObjectOptions{})
	assert.ErrorIs(t, err, domain.ErrObjectNotFound, "no older copy resurfaces")
}

// TestNewServerPoolsValidation covers the constructor arguments and the
// storage class settings.
func TestNewServerPoolsValidation(t *testing.T) {
	drives, _ := newDrives(t, 8)
	set := func(n int) [][]storage.StorageAPI { return [][]storage.StorageAPI{drives[:n]} }
	tests := []struct {
		name string
		deps object.PoolsDeps
		opts []object.Option
		ok   bool
	}{
		{name: "no pool", deps: object.PoolsDeps{}},
		{name: "no codec factory", deps: object.PoolsDeps{Pools: []object.PoolSpec{{Sets: set(4)}}}},
		{name: "empty pool", deps: object.PoolsDeps{Pools: []object.PoolSpec{{}}}},
		{name: "set of one drive", deps: object.PoolsDeps{Pools: []object.PoolSpec{{Sets: set(1)}}}},
		{name: "sets of different sizes", deps: object.PoolsDeps{Pools: []object.PoolSpec{{Sets: [][]storage.StorageAPI{drives[:4], drives[4:7]}}}}},
		{name: "parity above half", deps: object.PoolsDeps{Pools: []object.PoolSpec{{Sets: set(4), Parity: 3}}}},
		{name: "default parity", deps: object.PoolsDeps{Pools: []object.PoolSpec{{Sets: set(4)}}}, ok: true},
		{name: "standard class", deps: object.PoolsDeps{Pools: []object.PoolSpec{{Sets: set(8)}}}, opts: []object.Option{object.WithStorageClass(domain.StorageClassStandard, "EC:3")}, ok: true},
		{name: "invalid class syntax", deps: object.PoolsDeps{Pools: []object.PoolSpec{{Sets: set(4)}}}, opts: []object.Option{object.WithStorageClass(domain.StorageClassStandard, "RS:2")}},
		{name: "rrs above standard", deps: object.PoolsDeps{Pools: []object.PoolSpec{{Sets: set(8)}}}, opts: []object.Option{object.WithStorageClass(domain.StorageClassReducedRedundancy, "EC:5")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.name != "no codec factory" {
				tt.deps.Codecs = testCodecs("")
			}
			svc, err := object.NewServerPools(tt.deps, tt.opts...)
			if !tt.ok {
				assert.ErrorIs(t, err, domain.ErrInvalidArgument)
				return
			}
			require.NoError(t, err)
			assert.True(t, svc.Health(context.Background(), object.HealthOptions{}).Healthy)
		})
	}
}

// TestStorageClassLayouts: the parity of a version follows its storage
// class (STANDARD default from the set size, REDUCED_REDUNDANCY 1, or the
// configured EC:n), multipart parts included, and the shards of each
// object are distributed by a permutation of the drives derived from its
// key.
func TestStorageClassLayouts(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name       string
		opts       []object.Option
		class      string
		wantParity int
	}{
		{name: "standard default", wantParity: 3},
		{name: "reduced redundancy default", class: domain.StorageClassReducedRedundancy, wantParity: 1},
		{name: "configured standard", opts: []object.Option{object.WithStorageClass(domain.StorageClassStandard, "EC:2")}, wantParity: 2},
		{name: "configured rrs", opts: []object.Option{object.WithStorageClass(domain.StorageClassReducedRedundancy, "EC:2")}, class: domain.StorageClassReducedRedundancy, wantParity: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newCluster(t, clusterSpec{pools: 1, sets: 1, drives: 6, opts: tt.opts})
			require.NoError(t, c.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
			body := payload(3*testBlockSize + 11)
			put(t, c.svc, "bkt", "obj", body, func(in *object.PutObjectInput) { in.StorageClass = tt.class })
			up, err := c.svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "bkt", Key: "mp", StorageClass: tt.class})
			require.NoError(t, err)
			p, err := c.svc.PutObjectPart(ctx, object.PutObjectPartInput{Bucket: "bkt", Key: "mp", UploadID: up.UploadID, PartNumber: 1, Reader: bytes.NewReader(body), Size: int64(len(body))})
			require.NoError(t, err)
			_, err = c.svc.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: "bkt", Key: "mp", UploadID: up.UploadID, Parts: []domain.CompletePart{{Number: 1, ETag: p.ETag}}})
			require.NoError(t, err)
			for _, key := range []string{"obj", "mp"} {
				indexes := map[int]bool{}
				var dist []int
				for _, d := range c.drives {
					fi, err := d.ReadVersion(ctx, "bkt", key, "", storage.ReadOptions{})
					require.NoError(t, err)
					assert.Equal(t, tt.wantParity, fi.Erasure.ParityBlocks, key)
					assert.Equal(t, 6-tt.wantParity, fi.Erasure.DataBlocks, key)
					indexes[fi.Erasure.Index] = true
					dist = fi.Erasure.Distribution
				}
				assert.Len(t, indexes, 6, "%s: every drive holds a different shard", key)
				sorted := append([]int(nil), dist...)
				sort.Ints(sorted)
				assert.Equal(t, []int{1, 2, 3, 4, 5, 6}, sorted, "%s: the distribution is a permutation", key)
				got, _ := get(t, c.svc, "bkt", key, object.GetObjectOptions{})
				assert.Equal(t, body, got)
			}
		})
	}
}

// TestInlineObjectsAreReplicated: an object up to the inline limit is
// stored whole in the xl.meta of every drive of its set, with its bitrot
// hash, whatever its storage class.
func TestInlineObjectsAreReplicated(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t, clusterSpec{pools: 1, sets: 1, drives: 6})
	require.NoError(t, c.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
	body := []byte("small enough to live in xl.meta")
	for _, class := range []string{"", domain.StorageClassReducedRedundancy} {
		key := "inline-" + class
		put(t, c.svc, "bkt", key, body, func(in *object.PutObjectInput) { in.StorageClass = class })
		for i, d := range c.drives {
			fi, err := d.ReadVersion(ctx, "bkt", key, "", storage.ReadOptions{ReadData: true})
			require.NoError(t, err, "drive %d", i)
			assert.Equal(t, body, fi.Data, "drive %d holds the whole content", i)
			assert.Empty(t, fi.DataDir)
			require.NotEmpty(t, fi.Erasure.Checksums)
			assert.Len(t, fi.Erasure.Checksums[0].Hash, 32, "drive %d records the bitrot hash", i)
		}
		got, _ := get(t, c.svc, "bkt", key, object.GetObjectOptions{})
		assert.Equal(t, body, got)
	}
}
