package object_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// setLayouts are the erasure set sizes of the TS-E scenarios with their
// default parity (docs/03 §3.3).
func setLayouts() []struct{ drives, parity int } {
	return []struct{ drives, parity int }{{drives: 4, parity: 2}, {drives: 8, parity: 4}}
}

// seed writes an inline object, a multi-block object and a multipart
// object and returns their content by key.
func seed(t *testing.T, svc object.Service) map[string][]byte {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
	objects := map[string][]byte{
		"small":     []byte("inline content"),
		"dir/large": payload(5*testBlockSize + 77),
	}
	for k, v := range objects {
		put(t, svc, "bkt", k, v)
	}
	up, err := svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "bkt", Key: "dir/multi"})
	require.NoError(t, err)
	var parts []domain.CompletePart
	var whole []byte
	for i := 1; i <= 3; i++ {
		body := payload(testBlockSize + 100*i)
		p, err := svc.PutObjectPart(ctx, object.PutObjectPartInput{Bucket: "bkt", Key: "dir/multi", UploadID: up.UploadID, PartNumber: i, Reader: bytes.NewReader(body), Size: int64(len(body))})
		require.NoError(t, err)
		parts = append(parts, domain.CompletePart{Number: i, ETag: p.ETag})
		whole = append(whole, body...)
	}
	_, err = svc.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: "bkt", Key: "dir/multi", UploadID: up.UploadID, Parts: parts})
	require.NoError(t, err)
	objects["dir/multi"] = whole
	return objects
}

// assertReadable checks the content of every object and the listing.
func assertReadable(t *testing.T, svc object.Service, objects map[string][]byte) {
	t.Helper()
	for k, want := range objects {
		got, _ := get(t, svc, "bkt", k, object.GetObjectOptions{})
		assert.Equal(t, want, got, "content of %s", k)
		r, err := svc.GetObject(context.Background(), "bkt", k, object.GetObjectOptions{Range: &domain.Range{Start: 3, End: 9}})
		require.NoError(t, err)
		part, err := io.ReadAll(r)
		require.NoError(t, err)
		_ = r.Close()
		assert.Equal(t, want[3:10], part, "range of %s", k)
	}
	keys := listAll(t, svc, domain.ListObjectsInput{Bucket: "bkt", MaxKeys: 2}, true)
	want := make([]string, 0, len(objects))
	for k := range objects {
		want = append(want, k)
	}
	assert.ElementsMatch(t, want, keys)
}

// removeDrives renames the directories of the given drives (the drive is
// "pulled out") and returns the function that puts them back.
func removeDrives(t *testing.T, c *cluster, idx ...int) func() {
	t.Helper()
	for _, i := range idx {
		require.NoError(t, os.Rename(c.roots[i], c.roots[i]+".removed"))
	}
	return func() {
		for _, i := range idx {
			require.NoError(t, os.Rename(c.roots[i]+".removed", c.roots[i]))
		}
	}
}

func seq(from, to int) []int {
	var out []int
	for i := from; i < to; i++ {
		out = append(out, i)
	}
	return out
}

// TestDriveLoss covers TS-E-01, TS-E-02 and TS-E-03 on sets of 4 and 8
// drives: up to P drives removed keep reads (and writes while the write
// quorum holds) working; more lose the quorum with 503 errors and no
// corruption, and everything is back once the drives return.
func TestDriveLoss(t *testing.T) {
	ctx := context.Background()
	for _, l := range setLayouts() {
		n, p := l.drives, l.parity
		d := n - p
		writeQuorum := d
		if d == p {
			writeQuorum = d + 1
		}
		t.Run(fmt.Sprintf("%d drives", n), func(t *testing.T) {
			c := newCluster(t, clusterSpec{pools: 1, sets: 1, drives: n})
			objects := seed(t, c.svc)
			assertReadable(t, c.svc, objects)

			t.Run("TS-E-01 one drive removed", func(t *testing.T) {
				restore := removeDrives(t, c, 0)
				assertReadable(t, c.svc, objects)
				objects["written-degraded"] = payload(3*testBlockSize + 1)
				put(t, c.svc, "bkt", "written-degraded", objects["written-degraded"])
				assertReadable(t, c.svc, objects)
				restore()
				assertReadable(t, c.svc, objects)
			})

			t.Run("TS-E-02 P drives removed", func(t *testing.T) {
				restore := removeDrives(t, c, seq(0, p)...)
				defer restore()
				assertReadable(t, c.svc, objects)
				_, err := c.svc.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: "refused", Reader: bytes.NewReader(payload(2 * testBlockSize)), Size: 2 * testBlockSize})
				if n-p >= writeQuorum {
					require.NoError(t, err, "the write quorum still holds")
					objects["refused"] = payload(2 * testBlockSize)
				} else {
					require.ErrorIs(t, err, domain.ErrWriteQuorum)
					_, err = c.svc.GetObjectInfo(ctx, "bkt", "refused", object.GetObjectOptions{})
					assert.ErrorIs(t, err, domain.ErrObjectNotFound, "nothing partial is visible")
				}
			})

			t.Run("TS-E-02 writes with the quorum exactly", func(t *testing.T) {
				restore := removeDrives(t, c, seq(0, n-writeQuorum)...)
				defer restore()
				body := payload(2*testBlockSize + 5)
				put(t, c.svc, "bkt", "at-quorum", body)
				objects["at-quorum"] = body
				assertReadable(t, c.svc, objects)
			})

			t.Run("TS-E-03 more than P drives removed", func(t *testing.T) {
				restore := removeDrives(t, c, seq(0, p+1)...)
				_, err := c.svc.GetObjectInfo(ctx, "bkt", "dir/large", object.GetObjectOptions{})
				assert.ErrorIs(t, err, domain.ErrReadQuorum)
				_, err = c.svc.ListObjects(ctx, domain.ListObjectsInput{Bucket: "bkt"})
				assert.ErrorIs(t, err, domain.ErrReadQuorum)
				_, err = c.svc.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: "never", Reader: bytes.NewReader([]byte("x")), Size: 1})
				assert.True(t, errors.Is(err, domain.ErrReadQuorum) || errors.Is(err, domain.ErrWriteQuorum), "503 quorum error, got %v", err)
				_, err = c.svc.DeleteObject(ctx, "bkt", "small", object.DeleteObjectOptions{})
				assert.Error(t, err)
				h := c.svc.Health(ctx, object.HealthOptions{})
				assert.False(t, h.Healthy)
				restore()
				assertReadable(t, c.svc, objects)
				assert.True(t, c.svc.Health(ctx, object.HealthOptions{}).Healthy)
			})
		})
	}
}

// driveWithShard returns the drive holding shard index (1-based) of key.
func driveWithShard(t *testing.T, c *cluster, key string, index int) int {
	t.Helper()
	for i, d := range c.drives {
		fi, err := d.ReadVersion(context.Background(), "bkt", key, "", storage.ReadOptions{})
		require.NoError(t, err)
		if fi.Erasure.Index == index {
			return i
		}
	}
	t.Fatalf("no drive holds shard %d of %s", index, key)
	return -1
}

// TestBitrotHealOnRead is TS-E-04: a flipped bit in a data shard (or in
// the inline copy of one drive) is detected, the read still returns the
// right content from the other shards, and the object is reported to the
// heal notifier.
//
// covers: S-STO-04, S-STO-05
func TestBitrotHealOnRead(t *testing.T) {
	for _, l := range setLayouts() {
		t.Run(fmt.Sprintf("%d drives", l.drives), func(t *testing.T) {
			c := newCluster(t, clusterSpec{pools: 1, sets: 1, drives: l.drives})
			objects := seed(t, c.svc)
			c.heal.notified()

			// Shard 1 (the first data shard, always read first).
			i := driveWithShard(t, c, "dir/large", 1)
			files, err := filepath.Glob(filepath.Join(c.roots[i], "bkt", "dir", "large", "*", "part.1"))
			require.NoError(t, err)
			require.Len(t, files, 1)
			raw, err := os.ReadFile(files[0])
			require.NoError(t, err)
			raw[40] ^= 0x04
			require.NoError(t, os.WriteFile(files[0], raw, 0o600))
			got, _ := get(t, c.svc, "bkt", "dir/large", object.GetObjectOptions{})
			assert.Equal(t, objects["dir/large"], got)
			assert.Eventually(t, func() bool { return assert.ObjectsAreEqual([]string{"bkt/dir/large@null"}, c.heal.notified()) }, time.Second, 5*time.Millisecond)

			// Inline content: the copy of one drive is damaged.
			corruptStored(t, c.roots[0], objects["small"])
			got, _ = get(t, c.svc, "bkt", "small", object.GetObjectOptions{})
			assert.Equal(t, objects["small"], got)
			assert.Contains(t, c.heal.notified(), "bkt/small@null")
		})
	}
}

// TestEmptiedDrive is the object-layer side of TS-E-05: a drive emptied
// and put back holds nothing, yet reads and listings answer from the
// others (each read object is reported for healing) and writes go on.
func TestEmptiedDrive(t *testing.T) {
	for _, l := range setLayouts() {
		t.Run(fmt.Sprintf("%d drives", l.drives), func(t *testing.T) {
			c := newCluster(t, clusterSpec{pools: 1, sets: 1, drives: l.drives})
			objects := seed(t, c.svc)
			require.NoError(t, os.RemoveAll(c.roots[2]))
			require.NoError(t, os.MkdirAll(c.roots[2], 0o750))
			c.heal.notified()
			assertReadable(t, c.svc, objects)
			notified := strings.Join(c.heal.notified(), " ")
			for k := range objects {
				assert.Contains(t, notified, "bkt/"+k+"@null")
			}
			body := payload(2*testBlockSize + 9)
			put(t, c.svc, "bkt", "after", body)
			objects["after"] = body
			assertReadable(t, c.svc, objects)
		})
	}
}

// TestSlowDriveIsHedged is TS-E-06: a drive whose reads are slow does not
// slow the reads down: its shard is replaced by a parity shard after the
// hedge delay.
func TestSlowDriveIsHedged(t *testing.T) {
	ctx := context.Background()
	for _, l := range setLayouts() {
		t.Run(fmt.Sprintf("%d drives", l.drives), func(t *testing.T) {
			c := newCluster(t, clusterSpec{pools: 1, sets: 1, drives: l.drives})
			require.NoError(t, c.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
			body := payload(8 * testBlockSize)
			put(t, c.svc, "bkt", "obj", body)
			slow := driveWithShard(t, c, "obj", 1)
			const delay = 400 * time.Millisecond
			c.setFaults(func(f *driveFaults) { f.slowReads = delay }, slow)
			var worst time.Duration
			for i := 0; i < 10; i++ {
				start := time.Now()
				got, _ := get(t, c.svc, "bkt", "obj", object.GetObjectOptions{})
				worst = max(worst, time.Since(start))
				require.Equal(t, body, got)
			}
			// Without hedging every one of the 8 blocks would wait for the
			// slow drive (8 × 400 ms per read).
			assert.Less(t, worst, delay+300*time.Millisecond, "the slowest read waited for the slow drive")
			assert.Empty(t, c.heal.notified(), "a slow drive is not damaged")
		})
	}
}

// TestFullDrives is TS-E-07: drives out of space fail the write with
// ErrStorageFull (XNexoStorageFull) and leave nothing behind; a single
// full drive is tolerated and reported for healing.
func TestFullDrives(t *testing.T) {
	ctx := context.Background()
	for _, l := range setLayouts() {
		t.Run(fmt.Sprintf("%d drives", l.drives), func(t *testing.T) {
			c := newCluster(t, clusterSpec{pools: 1, sets: 1, drives: l.drives})
			require.NoError(t, c.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
			put(t, c.svc, "bkt", "obj", payload(3*testBlockSize))

			c.setFaults(func(f *driveFaults) { f.full = true }, seq(0, l.drives)...)
			_, err := c.svc.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: "obj", Reader: bytes.NewReader(payload(2 * testBlockSize)), Size: 2 * testBlockSize})
			require.ErrorIs(t, err, domain.ErrStorageFull)
			got, _ := get(t, c.svc, "bkt", "obj", object.GetObjectOptions{})
			assert.Equal(t, payload(3*testBlockSize), got, "the previous version is untouched")
			_, err = c.svc.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: "new", Reader: bytes.NewReader(payload(2 * testBlockSize)), Size: 2 * testBlockSize})
			require.ErrorIs(t, err, domain.ErrStorageFull)
			_, err = c.svc.GetObjectInfo(ctx, "bkt", "new", object.GetObjectOptions{})
			assert.ErrorIs(t, err, domain.ErrObjectNotFound)
			assertNoTmp(t, c.roots)

			// Every drive but one has room again.
			c.setFaults(func(f *driveFaults) { f.full = false }, seq(0, l.drives-1)...)
			c.heal.notified()
			body := payload(2*testBlockSize + 3)
			put(t, c.svc, "bkt", "new", body)
			got, _ = get(t, c.svc, "bkt", "new", object.GetObjectOptions{})
			assert.Equal(t, body, got)
			assert.Contains(t, c.heal.notified(), "bkt/new@null", "the drive that missed the write is reported (MRF)")
			assertNoTmp(t, c.roots)
		})
	}
}

// assertNoTmp checks that no temporary data survived on the drives.
func assertNoTmp(t *testing.T, roots []string) {
	t.Helper()
	for _, root := range roots {
		entries, _ := os.ReadDir(filepath.Join(root, domain.SystemBucket, storage.TmpDir))
		assert.Empty(t, entries, "temporary entries left on %s", root)
	}
}

// TestCommitQuorumRollback: a commit that reaches fewer drives than the
// write quorum is undone where it happened, so that the previous state
// (the replaced version, or no object) is intact on every drive; a commit
// that misses one drive succeeds and is reported for healing.
func TestCommitQuorumRollback(t *testing.T) {
	ctx := context.Background()
	errCommit := fmt.Errorf("%w: injected commit failure", domain.ErrFaultyDisk)
	for _, size := range []int{100, 3 * testBlockSize} {
		t.Run(fmt.Sprintf("%d bytes", size), func(t *testing.T) {
			c := newCluster(t, clusterSpec{pools: 1, sets: 1, drives: 4})
			require.NoError(t, c.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
			old := payload(size + 1)
			put(t, c.svc, "bkt", "obj", old)

			c.setFaults(func(f *driveFaults) { f.failCommit = errCommit }, 1, 3)
			_, err := c.svc.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: "obj", Reader: bytes.NewReader(payload(size)), Size: int64(size)})
			require.ErrorIs(t, err, domain.ErrWriteQuorum)
			_, err = c.svc.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: "fresh", Reader: bytes.NewReader(payload(size)), Size: int64(size)})
			require.ErrorIs(t, err, domain.ErrWriteQuorum)
			c.setFaults(func(f *driveFaults) { f.failCommit = nil }, 1, 3)

			for i, d := range c.drives {
				fi, err := d.ReadVersion(ctx, "bkt", "obj", "", storage.ReadOptions{})
				require.NoError(t, err, "drive %d", i)
				assert.Equal(t, int64(len(old)), fi.Size, "drive %d keeps the previous version", i)
				_, err = d.ReadVersion(ctx, "bkt", "fresh", "", storage.ReadOptions{})
				assert.ErrorIs(t, err, domain.ErrObjectNotFound, "drive %d", i)
				if size > testInlineLimit {
					dirs, _ := filepath.Glob(filepath.Join(c.roots[i], "bkt", "obj", "*-*"))
					assert.Len(t, dirs, 1, "drive %d: only the data of the previous version", i)
				}
			}
			got, _ := get(t, c.svc, "bkt", "obj", object.GetObjectOptions{})
			assert.Equal(t, old, got)
			assertNoTmp(t, c.roots)

			c.heal.notified()
			c.setFaults(func(f *driveFaults) { f.failCommit = errCommit }, 2)
			put(t, c.svc, "bkt", "obj", payload(size))
			c.setFaults(func(f *driveFaults) { f.failCommit = nil }, 2)
			assert.Contains(t, c.heal.notified(), "bkt/obj@null")
			got, _ = get(t, c.svc, "bkt", "obj", object.GetObjectOptions{})
			assert.Equal(t, payload(size), got)
		})
	}
}

// cancelingReader cancels the request context once after bytes were read.
type cancelingReader struct {
	r      io.Reader
	after  int
	read   int
	cancel context.CancelFunc
}

func (c *cancelingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += n
	if c.read >= c.after && c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
	return n, err
}

// TestCrashMidWrite: a write whose client goes away while the body is
// streamed leaves no object and no temporary data.
func TestCrashMidWrite(t *testing.T) {
	c := newCluster(t, clusterSpec{pools: 1, sets: 1, drives: 4})
	ctx := context.Background()
	require.NoError(t, c.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
	body := payload(20 * testBlockSize)
	for _, size := range []int64{int64(len(body)), -1} {
		cctx, cancel := context.WithCancel(ctx)
		_, err := c.svc.PutObject(cctx, object.PutObjectInput{Bucket: "bkt", Key: "obj", Size: size,
			Reader: &cancelingReader{r: bytes.NewReader(body), after: 5 * testBlockSize, cancel: cancel}})
		cancel()
		require.Error(t, err)
		_, err = c.svc.GetObjectInfo(ctx, "bkt", "obj", object.GetObjectOptions{})
		require.ErrorIs(t, err, domain.ErrObjectNotFound, "no partial object")
		res, err := c.svc.ListObjects(ctx, domain.ListObjectsInput{Bucket: "bkt"})
		require.NoError(t, err)
		assert.Empty(t, res.Objects)
		assertNoTmp(t, c.roots)
	}
}

// TestMaintenanceIsPerNode: the maintenance margin of the health checks
// that one more node, with all its drives, can go down without losing the
// write quorum of a set (regression: one more drive was counted, so a
// node of two drives was reported safe to stop when it was not).
func TestMaintenanceIsPerNode(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name        string
		drives      int
		nodes       int
		offline     []int
		healthy     bool
		maintenance bool
		wantDetail  string
	}{
		{name: "4 drives on 4 nodes", drives: 4, nodes: 4, healthy: true, maintenance: true},
		{name: "4 drives on 2 nodes", drives: 4, nodes: 2, healthy: true, wantDetail: "2 of them on one node"},
		{name: "8 drives on 4 nodes", drives: 8, nodes: 4, healthy: true, maintenance: true},
		{name: "8 drives on 4 nodes, one node down", drives: 8, nodes: 4, offline: []int{0, 4}, healthy: true},
		{name: "8 drives on 4 nodes, two nodes down", drives: 8, nodes: 4, offline: []int{0, 4, 1, 5}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newCluster(t, clusterSpec{pools: 1, sets: 1, drives: tc.drives})
			for i := range c.faults {
				c.setFaults(func(f *driveFaults) { f.host = fmt.Sprintf("node%d:9000", i%tc.nodes+1) }, i)
			}
			c.setFaults(func(f *driveFaults) { f.offline = true }, tc.offline...)
			assert.Equal(t, tc.healthy, c.svc.Health(ctx, object.HealthOptions{}).Healthy)
			h := c.svc.Health(ctx, object.HealthOptions{Maintenance: true})
			assert.Equal(t, tc.maintenance, h.Healthy, h.Message)
			if tc.wantDetail != "" {
				assert.Contains(t, h.Message, tc.wantDetail)
			}
		})
	}
}
