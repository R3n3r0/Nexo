package object_test

import (
	"bytes"
	"context"
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
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/lock"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// failMovesTo returns a failRename fault failing the moves whose
// destination contains pattern.
func failMovesTo(pattern string) func(f *driveFaults) {
	return func(f *driveFaults) {
		f.failRename = func(dst string) error {
			if strings.Contains(dst, pattern) {
				return fmt.Errorf("%w: injected: rename to %s failed", domain.ErrFaultyDisk, dst)
			}
			return nil
		}
	}
}

func clearMoveFaults(f *driveFaults) { f.failRename = nil }

// uploadParts creates an upload of bucket/key with the given storage class
// and uploads one part per body; it returns the upload id and the parts
// to complete.
func uploadParts(t *testing.T, svc object.Service, bucket, key, class string, bodies ...[]byte) (string, []domain.CompletePart) {
	t.Helper()
	ctx := context.Background()
	up, err := svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: bucket, Key: key, StorageClass: class})
	require.NoError(t, err)
	var parts []domain.CompletePart
	for i, body := range bodies {
		p, err := svc.PutObjectPart(ctx, object.PutObjectPartInput{Bucket: bucket, Key: key, UploadID: up.UploadID,
			PartNumber: i + 1, Reader: bytes.NewReader(body), Size: int64(len(body))})
		require.NoError(t, err, "part %d", i+1)
		parts = append(parts, domain.CompletePart{Number: i + 1, ETag: p.ETag})
	}
	return up.UploadID, parts
}

func readObject(t *testing.T, svc object.Service, bucket, key string) ([]byte, error) {
	t.Helper()
	rd, err := svc.GetObject(context.Background(), bucket, key, object.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = rd.Close() }()
	return io.ReadAll(rd)
}

// TestMultipartPartQuorumFollowsLayout: a part is acknowledged only once
// it reached the write quorum of the layout of its upload. With
// REDUCED_REDUNDANCY (EC:1) that quorum is above the one of STANDARD: a
// part moved on fewer drives used to be acknowledged, and the completed
// object was unreadable with every drive online.
func TestMultipartPartQuorumFollowsLayout(t *testing.T) {
	tests := []struct {
		name    string
		drives  int
		class   string
		failing []int
		wantErr bool
	}{
		{name: "rrs 6 drives, 2 failing", drives: 6, class: domain.StorageClassReducedRedundancy, failing: []int{0, 1}, wantErr: true},
		{name: "rrs 8 drives, 2 failing", drives: 8, class: domain.StorageClassReducedRedundancy, failing: []int{0, 1}, wantErr: true},
		{name: "rrs 6 drives, 1 failing", drives: 6, class: domain.StorageClassReducedRedundancy, failing: []int{2}},
		{name: "standard 6 drives, 2 failing", drives: 6, class: domain.StorageClassStandard, failing: []int{0, 1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			spec := clusterSpec{pools: 1, sets: 1, drives: tc.drives}
			c := newCluster(t, spec)
			require.NoError(t, c.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
			p1, p2 := payload(3*testBlockSize+5), payload(2*testBlockSize+7)
			up, err := c.svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "bkt", Key: "obj", StorageClass: tc.class})
			require.NoError(t, err)

			c.setFaults(failMovesTo("/part.1."), tc.failing...)
			r1, err := c.svc.PutObjectPart(ctx, object.PutObjectPartInput{Bucket: "bkt", Key: "obj", UploadID: up.UploadID,
				PartNumber: 1, Reader: bytes.NewReader(p1), Size: int64(len(p1))})
			c.setFaults(clearMoveFaults, tc.failing...)
			if tc.wantErr {
				require.ErrorIs(t, err, domain.ErrWriteQuorum, "a part below the quorum of its layout is not acknowledged")
				// The client retries the part.
				r1, err = c.svc.PutObjectPart(ctx, object.PutObjectPartInput{Bucket: "bkt", Key: "obj", UploadID: up.UploadID,
					PartNumber: 1, Reader: bytes.NewReader(p1), Size: int64(len(p1))})
			}
			require.NoError(t, err)
			r2, err := c.svc.PutObjectPart(ctx, object.PutObjectPartInput{Bucket: "bkt", Key: "obj", UploadID: up.UploadID,
				PartNumber: 2, Reader: bytes.NewReader(p2), Size: int64(len(p2))})
			require.NoError(t, err)
			_, err = c.svc.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: "bkt", Key: "obj", UploadID: up.UploadID,
				Parts: []domain.CompletePart{{Number: 1, ETag: r1.ETag}, {Number: 2, ETag: r2.ETag}}})
			require.NoError(t, err)
			got, err := readObject(t, c.svc, "bkt", "obj")
			require.NoError(t, err, "an acknowledged object is readable with every drive online")
			assert.Equal(t, append(p1, p2...), got)
		})
	}
}

// TestCompleteMultipartRetryAfterFailedAssembly: a completion whose part
// moves fail on some drives rebuilds the part there when enough drives
// hold it; when too few do, it fails retryably and moves back every part
// wherever it went, so that the upload stays completable.
func TestCompleteMultipartRetryAfterFailedAssembly(t *testing.T) {
	tests := []struct {
		name    string
		failing []int
		wantErr error
	}{
		{name: "moves failing on the parity", failing: []int{0, 1}},
		{name: "moves failing beyond the parity", failing: []int{0, 1, 2}, wantErr: domain.ErrWriteQuorum},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			c := newCluster(t, clusterSpec{pools: 1, sets: 1, drives: 4})
			require.NoError(t, c.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
			var bodies [][]byte
			var whole []byte
			for i := 1; i <= 3; i++ {
				b := bytes.Repeat([]byte{byte('0' + i)}, 2*testBlockSize+i)
				bodies = append(bodies, b)
				whole = append(whole, b...)
			}
			uploadID, parts := uploadParts(t, c.svc, "bkt", "obj", "", bodies...)

			// The completion moves the parts into the staging directory of
			// the upload: the failing drives refuse the move of part 2.
			c.setFaults(failMovesTo("/part.2"), tc.failing...)
			_, err := c.svc.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: "bkt", Key: "obj", UploadID: uploadID, Parts: parts})
			c.setFaults(clearMoveFaults, tc.failing...)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
				listed, lerr := c.svc.ListObjectParts(ctx, domain.ListPartsInput{Bucket: "bkt", Key: "obj", UploadID: uploadID})
				require.NoError(t, lerr)
				assert.Len(t, listed.Parts, 3)
				_, err = c.svc.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: "bkt", Key: "obj", UploadID: uploadID, Parts: parts})
			}
			require.NoError(t, err, "the upload completes")
			got, err := readObject(t, c.svc, "bkt", "obj")
			require.NoError(t, err)
			assert.Equal(t, whole, got)
			// Every drive holds a whole copy: any two of them serve it.
			c.setFaults(offline(true), 2, 3)
			got, err = readObject(t, c.svc, "bkt", "obj")
			c.setFaults(offline(false), 2, 3)
			require.NoError(t, err)
			assert.Equal(t, whole, got)
			for _, root := range c.roots {
				entries, _ := os.ReadDir(filepath.Join(root, domain.SystemBucket, storage.TmpDir))
				assert.Empty(t, entries, "temporary entries left on %s", root)
			}
			assertNoUploadLeft(t, c.roots, "bkt")
		})
	}
}

// TestCompleteMultipartRetryAfterFailedCommit: parts moved back after a
// commit that missed its quorum stay completable too.
func TestCompleteMultipartRetryAfterFailedCommit(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t, clusterSpec{pools: 1, sets: 1, drives: 4})
	require.NoError(t, c.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
	body := payload(2*testBlockSize + 3)
	uploadID, parts := uploadParts(t, c.svc, "bkt", "obj", "", body, body)
	c.setFaults(func(f *driveFaults) { f.failCommit = fmt.Errorf("%w: injected commit failure", domain.ErrFaultyDisk) }, 2, 3)
	_, err := c.svc.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: "bkt", Key: "obj", UploadID: uploadID, Parts: parts})
	require.Error(t, err)
	c.setFaults(func(f *driveFaults) { f.failCommit = nil }, 2, 3)
	_, err = c.svc.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: "bkt", Key: "obj", UploadID: uploadID, Parts: parts})
	require.NoError(t, err)
	got, err := readObject(t, c.svc, "bkt", "obj")
	require.NoError(t, err)
	assert.Equal(t, append(append([]byte{}, body...), body...), got)
}

// TestCompletionRepairsIncompleteParts: a drive whose copy of a part is
// truncated gets the part rebuilt from the others at the completion; a
// part damaged on more drives than its parity is lost (InvalidPart) and
// no unreadable version is committed.
func TestCompletionRepairsIncompleteParts(t *testing.T) {
	tests := []struct {
		name      string
		truncated []int
		wantErr   error
	}{
		{name: "one drive truncated", truncated: []int{4}},
		{name: "two drives truncated", truncated: []int{0, 4}, wantErr: domain.ErrInvalidPart},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			c := newCluster(t, clusterSpec{pools: 1, sets: 1, drives: 6})
			require.NoError(t, c.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
			p1, p2 := payload(3*testBlockSize+1), payload(testBlockSize+9)
			uploadID, parts := uploadParts(t, c.svc, "bkt", "obj", domain.StorageClassReducedRedundancy, p1, p2)
			for _, i := range tc.truncated {
				matches, err := filepath.Glob(filepath.Join(c.roots[i], domain.SystemBucket, storage.MultipartDir, "bkt", uploadID, "part.1.*-*"))
				require.NoError(t, err)
				require.Len(t, matches, 1)
				require.NoError(t, os.Truncate(matches[0], 10))
			}
			_, err := c.svc.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: "bkt", Key: "obj", UploadID: uploadID, Parts: parts})
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr, "the part is damaged beyond the parity of EC:1")
				_, err := c.svc.GetObjectInfo(ctx, "bkt", "obj", object.GetObjectOptions{})
				require.ErrorIs(t, err, domain.ErrObjectNotFound, "no unreadable version is committed")
				return
			}
			require.NoError(t, err)
			// With drive 0 offline (the parity of EC:1), the read needs the
			// rebuilt shard of drive 4.
			c.setFaults(offline(true), 0)
			got, err := readObject(t, c.svc, "bkt", "obj")
			c.setFaults(offline(false), 0)
			require.NoError(t, err)
			assert.Equal(t, append(p1, p2...), got)
		})
	}
}

// TestPurgeReplacedPerDrive: the data directory a commit replaces is
// purged on each drive that committed, whatever version that drive
// replaced; a drive where the commit failed keeps its current data.
func TestPurgeReplacedPerDrive(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t, clusterSpec{pools: 1, sets: 1, drives: 4})
	require.NoError(t, c.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
	dataDirs := func(i int) []string {
		entries, _ := os.ReadDir(filepath.Join(c.roots[i], "bkt", "obj"))
		var out []string
		for _, e := range entries {
			if e.IsDir() {
				out = append(out, e.Name())
			}
		}
		return out
	}
	put(t, c.svc, "bkt", "obj", payload(2*testBlockSize))
	first := dataDirs(0)
	require.Len(t, first, 1)

	// Drive 3 misses the second write: it keeps the first version.
	c.setFaults(func(f *driveFaults) { f.failCommit = fmt.Errorf("%w: injected commit failure", domain.ErrFaultyDisk) }, 3)
	put(t, c.svc, "bkt", "obj", payload(3*testBlockSize))
	c.setFaults(func(f *driveFaults) { f.failCommit = nil }, 3)
	second := dataDirs(0)
	require.Len(t, second, 1)
	require.NotEqual(t, first, second)
	assert.Equal(t, first, dataDirs(3), "the data of the version still current on drive 3 is kept")

	// The third write replaces the second version on drives 0-2 and the
	// first one on drive 3: each drive purges its own.
	put(t, c.svc, "bkt", "obj", payload(4*testBlockSize))
	third := dataDirs(0)
	for i := range c.roots {
		assert.Equal(t, third, dataDirs(i), "drive %d keeps only the data of the latest version", i)
	}
}

// sharedDrives returns n local drives and, for each of several nodes, the
// hooked drives (generated mocks) of that node over them: the nodes of a
// cluster reach the same drives.
func sharedDrives(t *testing.T, n, nodes int) [][]storage.StorageAPI {
	t.Helper()
	base, _ := newDrives(t, n)
	out := make([][]storage.StorageAPI, nodes)
	for node := range out {
		for _, d := range base {
			out[node] = append(out[node], hookedDrive(t, d, &driveFaults{}))
		}
	}
	return out
}

// TestLostLockStopsCommit: a writer whose distributed write lock is lost
// while it commits (lock servers restarted, admin lock clear, partition)
// must not commit: another node may have taken the lock and committed.
func TestLostLockStopsCommit(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.DiscardHandler)
	base, _ := newDrives(t, 4)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	gate := &driveFaults{beforeCommit: func(_ context.Context, volume, path string) {
		if volume == "bkt" && path == "key" {
			once.Do(func() { close(entered) })
			<-release
		}
	}}
	var drivesA, drivesB []storage.StorageAPI
	for _, d := range base {
		drivesA = append(drivesA, hookedDrive(t, d, gate))
		drivesB = append(drivesB, hookedDrive(t, d, &driveFaults{}))
	}
	servers := []lock.Server{lock.NewServer(lock.ServerDeps{Logger: logger}), lock.NewServer(lock.ServerDeps{Logger: logger})}
	node := func(name string, drives []storage.StorageAPI) object.Service {
		locks, err := lock.NewDistributed(lock.DistributedDeps{Servers: servers, Owner: name, Logger: logger},
			lock.WithRefreshInterval(20*time.Millisecond))
		require.NoError(t, err)
		svc, err := object.NewServerPools(object.PoolsDeps{Pools: []object.PoolSpec{{Sets: [][]storage.StorageAPI{drives}}},
			DeploymentID: "dep", Codecs: testCodecs("dep"), Locks: locks, Logger: logger},
			object.WithInlineLimit(testInlineLimit), object.WithBlockSize(testBlockSize), object.WithLockTimeout(5*time.Second))
		require.NoError(t, err)
		return svc
	}
	nodeA, nodeB := node("A", drivesA), node("B", drivesB)
	require.NoError(t, nodeA.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))

	bodyA, bodyB := bytes.Repeat([]byte("A"), 3*testBlockSize), bytes.Repeat([]byte("B"), 2*testBlockSize)
	errA := make(chan error, 1)
	go func() {
		_, err := nodeA.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: "key", Reader: bytes.NewReader(bodyA), Size: int64(len(bodyA))})
		errA <- err
	}()
	<-entered // A holds the write lock and is inside its commit
	for _, s := range servers {
		_, err := s.ForceUnlock(ctx, lock.Args{Resources: []string{lock.Resource("bkt", "key")}})
		require.NoError(t, err)
	}
	// B takes the lock A lost and commits.
	_, err := nodeB.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: "key", Reader: bytes.NewReader(bodyB), Size: int64(len(bodyB))})
	require.NoError(t, err)
	// A notices the loss at its next renewal.
	time.Sleep(200 * time.Millisecond)
	close(release)
	require.ErrorIs(t, <-errA, domain.ErrLockLost, "a writer that lost its lock does not commit")

	got, err := readObject(t, nodeB, "bkt", "key")
	require.NoError(t, err)
	assert.Equal(t, bodyB, got, "the last writer to hold the lock wins")
	got, err = readObject(t, nodeA, "bkt", "key")
	require.NoError(t, err)
	assert.Equal(t, bodyB, got)
}

// TestClockSkewVersionOrder (TC-13): the nodes of a cluster do not share a
// clock. Versions written through a node whose clock lags 5 s behind the
// one of the previous writer must still be the latest, and so must its
// delete markers.
func TestClockSkewVersionOrder(t *testing.T) {
	ctx := context.Background()
	drives := sharedDrives(t, 4, 2)
	locks := lock.NewLocal(lock.Deps{})
	base := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	node := func(clk clock.Clock, drives []storage.StorageAPI) object.Service {
		svc, err := object.NewServerPools(object.PoolsDeps{Pools: []object.PoolSpec{{Sets: [][]storage.StorageAPI{drives}}},
			DeploymentID: "dep", Codecs: testCodecs("dep"), Locks: locks, Buckets: versionedBuckets(t), Clock: clk, Logger: slog.New(slog.DiscardHandler)},
			object.WithInlineLimit(testInlineLimit), object.WithBlockSize(testBlockSize))
		require.NoError(t, err)
		return svc
	}
	clkA, clkB := clock.NewFake(base.Add(5*time.Second)), clock.NewFake(base)
	nodeA, nodeB := node(clkA, drives[0]), node(clkB, drives[1])
	require.NoError(t, nodeA.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))

	tests := []struct {
		name string
		body []byte
	}{
		{name: "inline", body: []byte("v2 via B")},
		{name: "erasure coded", body: payload(2*testBlockSize + 1)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			key := "k-" + strings.ReplaceAll(tc.name, " ", "-")
			v1 := put(t, nodeA, "bkt", key, []byte("v1 via A"))
			clkB.Advance(time.Millisecond)
			v2 := put(t, nodeB, "bkt", key, tc.body)
			assert.True(t, v2.ModTime.After(v1.ModTime), "the later write gets a later time: %s vs %s", v2.ModTime, v1.ModTime)
			got, err := readObject(t, nodeB, "bkt", key)
			require.NoError(t, err)
			assert.Equal(t, tc.body, got, "read-after-write: the last acknowledged PUT is the latest version")
			got, err = readObject(t, nodeA, "bkt", key)
			require.NoError(t, err)
			assert.Equal(t, tc.body, got)

			del, err := nodeB.DeleteObject(ctx, "bkt", key, object.DeleteObjectOptions{})
			require.NoError(t, err)
			require.True(t, del.DeleteMarker)
			_, err = nodeA.GetObjectInfo(ctx, "bkt", key, object.GetObjectOptions{})
			require.ErrorIs(t, err, domain.ErrObjectNotFound, "the object is gone after the DELETE")
			versions, err := nodeA.ListObjectVersions(ctx, domain.ListObjectsInput{Bucket: "bkt", Prefix: key})
			require.NoError(t, err)
			require.Len(t, versions.Objects, 3)
			assert.True(t, versions.Objects[0].IsDeleteMarker && versions.Objects[0].IsLatest, "the delete marker is the latest version")
			assert.Equal(t, v2.VersionID, versions.Objects[1].VersionID)
			assert.Equal(t, v1.VersionID, versions.Objects[2].VersionID)
		})
	}
}

// TestLostReadLockStopsRead: the body of a GET fails once its distributed
// read lock is lost, since a writer may then replace (and purge) the
// version being read.
func TestLostReadLockStopsRead(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.DiscardHandler)
	drives := sharedDrives(t, 4, 1)[0]
	servers := []lock.Server{lock.NewServer(lock.ServerDeps{Logger: logger}), lock.NewServer(lock.ServerDeps{Logger: logger})}
	locks, err := lock.NewDistributed(lock.DistributedDeps{Servers: servers, Owner: "A", Logger: logger}, lock.WithRefreshInterval(20*time.Millisecond))
	require.NoError(t, err)
	svc, err := object.NewServerPools(object.PoolsDeps{Pools: []object.PoolSpec{{Sets: [][]storage.StorageAPI{drives}}},
		DeploymentID: "dep", Codecs: testCodecs("dep"), Locks: locks, Logger: logger},
		object.WithInlineLimit(testInlineLimit), object.WithBlockSize(testBlockSize), object.WithLockTimeout(5*time.Second),
		// A read that keeps its lock until the body is closed (the
		// pinned reads release it before streaming: TestPinnedReads).
		object.WithPinnedReadParts(0))
	require.NoError(t, err)
	require.NoError(t, svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
	put(t, svc, "bkt", "key", payload(64*testBlockSize))

	rd, err := svc.GetObject(ctx, "bkt", "key", object.GetObjectOptions{})
	require.NoError(t, err)
	defer func() { _ = rd.Close() }()
	buf := make([]byte, testBlockSize)
	_, err = io.ReadFull(rd, buf)
	require.NoError(t, err)
	for _, s := range servers {
		_, err := s.ForceUnlock(ctx, lock.Args{Resources: []string{lock.Resource("bkt", "key")}})
		require.NoError(t, err)
	}
	time.Sleep(200 * time.Millisecond)
	_, err = io.Copy(io.Discard, rd)
	require.ErrorIs(t, err, domain.ErrLockLost)
}

// TestDegradedReadPrefersNewestMetadata: when the drives that answer are
// split evenly between the metadata before and after an in-place update
// (the update reached 3 of 4 drives, then 2 drives went offline, one of
// them updated), the read returns the updated metadata.
func TestDegradedReadPrefersNewestMetadata(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t, clusterSpec{pools: 1, sets: 1, drives: 4})
	require.NoError(t, c.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
	put(t, c.svc, "bkt", "obj", payload(2*testBlockSize), func(in *object.PutObjectInput) { in.Tags = map[string]string{"v": "old"} })

	offline := func(on bool) func(f *driveFaults) { return func(f *driveFaults) { f.offline = on } }
	c.setFaults(offline(true), 0)
	_, err := c.svc.PutObjectTags(ctx, "bkt", "obj", map[string]string{"v": "new"}, object.TagOptions{})
	require.NoError(t, err)
	c.setFaults(offline(false), 0)
	c.setFaults(offline(true), 1, 2)

	tags, err := c.svc.GetObjectTags(ctx, "bkt", "obj", object.TagOptions{})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"v": "new"}, tags, "drive 0 (old) and drive 3 (new) tie: the newest update wins")
	info, err := c.svc.GetObjectInfo(ctx, "bkt", "obj", object.GetObjectOptions{})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"v": "new"}, info.Tags)
	assert.NotContains(t, info.UserMetadata, "x-nexo-internal-meta-updated")
}
