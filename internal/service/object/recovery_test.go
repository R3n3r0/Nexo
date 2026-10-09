package object_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/storage"
	"github.com/r3n3r0/nexo/internal/testutil"
)

// Regression tests of the durability review (round 2): multipart parts
// acknowledged on different drives, interrupted completions, refused
// system writes and deletes, undecodable metadata, null-version healing.

func offline(on bool) func(f *driveFaults) {
	return func(f *driveFaults) { f.offline = on }
}

// failWriteAllOf fails the WriteAll calls whose path contains pattern.
func failWriteAllOf(pattern string) func(f *driveFaults) {
	return func(f *driveFaults) {
		f.failWriteAll = func(p string) error {
			if strings.Contains(p, pattern) {
				return fmt.Errorf("%w: injected: write %s failed", domain.ErrFaultyDisk, p)
			}
			return nil
		}
	}
}

// TestMultipartPartsOnDisjointDrives: every part is acknowledged on its
// write quorum, but on different drives (a rolling restart during a long
// upload). With every drive back online the completion succeeds: the
// drives missing a part get its shards rebuilt from the others.
func TestMultipartPartsOnDisjointDrives(t *testing.T) {
	tests := []struct {
		name   string
		drives int
		// offline[i] are the drives offline while part i+1 is uploaded.
		offline [][]int
		// down are the drives offline at the completion.
		down []int
	}{
		{name: "4 drives EC:2", drives: 4, offline: [][]int{{3}, {0}}},
		{name: "8 drives EC:4, one drive per part", drives: 8, offline: [][]int{{7}, {0}}},
		{name: "8 drives EC:4, one node of two drives per part", drives: 8, offline: [][]int{{6, 7}, {0, 1}, {2, 3}}},
		{name: "6 drives EC:3", drives: 6, offline: [][]int{{5}, {0}}},
		{name: "8 drives EC:4, a node still down at the completion", drives: 8, offline: [][]int{{6, 7}, {0, 1}}, down: []int{2, 3}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			c := newCluster(t, clusterSpec{pools: 1, sets: 1, drives: tc.drives})
			require.NoError(t, c.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
			up, err := c.svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "bkt", Key: "obj"})
			require.NoError(t, err)
			var parts []domain.CompletePart
			var want []byte
			for i, off := range tc.offline {
				body := payload(2*testBlockSize + i + 1)
				want = append(want, body...)
				c.setFaults(offline(true), off...)
				p, err := c.svc.PutObjectPart(ctx, object.PutObjectPartInput{Bucket: "bkt", Key: "obj", UploadID: up.UploadID,
					PartNumber: i + 1, Reader: bytes.NewReader(body), Size: int64(len(body))})
				c.setFaults(offline(false), off...)
				require.NoError(t, err, "part %d is acknowledged", i+1)
				parts = append(parts, domain.CompletePart{Number: i + 1, ETag: p.ETag})
			}
			c.setFaults(offline(true), tc.down...)
			_, err = c.svc.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: "bkt", Key: "obj", UploadID: up.UploadID, Parts: parts})
			require.NoError(t, err, "every part is acknowledged: the completion succeeds")
			c.setFaults(offline(false), tc.down...)
			got, err := readObject(t, c.svc, "bkt", "obj")
			require.NoError(t, err)
			assert.Equal(t, want, got)

			// Every drive online at the completion holds a whole copy: the
			// object survives the loss of any parity's worth of them.
			parity := tc.drives / 2
			up2 := make([]int, 0, tc.drives)
			for i := range tc.drives {
				if !contains(tc.down, i) {
					up2 = append(up2, i)
				}
			}
			for _, i := range up2 {
				_, err := os.Stat(filepath.Join(c.roots[i], "bkt", "obj", storage.MetaFile))
				assert.NoError(t, err, "drive %d committed the object", i)
			}
			lost := append(append([]int(nil), up2[:parity-len(tc.down)]...), tc.down...)
			c.setFaults(offline(true), lost...)
			got, err = readObject(t, c.svc, "bkt", "obj")
			c.setFaults(offline(false), lost...)
			require.NoError(t, err, "readable with %d drives lost", parity)
			assert.Equal(t, want, got)
			// The drives down at the completion keep their copy of the
			// upload: no read quorum names it, the stale upload cleanup
			// removes it.
			var roots []string
			for _, i := range up2 {
				roots = append(roots, c.roots[i])
			}
			assertNoUploadLeft(t, roots, "bkt")
		})
	}
}

func contains(s []int, v int) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// assertNoUploadLeft checks that no upload directory of bucket is left on
// the drives.
func assertNoUploadLeft(t *testing.T, roots []string, bucket string) {
	t.Helper()
	for _, r := range roots {
		entries, _ := os.ReadDir(filepath.Join(r, domain.SystemBucket, storage.MultipartDir, bucket))
		assert.Empty(t, entries, "upload directories left on %s", r)
	}
}

// TestMultipartLostPartIsInvalid: a part whose data is gone from more
// drives than its parity cannot be completed (InvalidPart), and the
// upload keeps its other parts.
func TestMultipartLostPartIsInvalid(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t, clusterSpec{pools: 1, sets: 1, drives: 4})
	require.NoError(t, c.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
	uploadID, parts := uploadParts(t, c.svc, "bkt", "obj", "", payload(2*testBlockSize+1), payload(testBlockSize+3))
	for _, i := range []int{0, 1, 2} {
		matches, err := filepath.Glob(filepath.Join(c.roots[i], domain.SystemBucket, storage.MultipartDir, "bkt", uploadID, "part.2.*-*"))
		require.NoError(t, err)
		require.Len(t, matches, 1)
		require.NoError(t, os.Remove(matches[0]))
	}
	_, err := c.svc.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: "bkt", Key: "obj", UploadID: uploadID, Parts: parts})
	require.ErrorIs(t, err, domain.ErrInvalidPart)
	_, err = c.svc.GetObjectInfo(ctx, "bkt", "obj", object.GetObjectOptions{})
	require.ErrorIs(t, err, domain.ErrObjectNotFound)
	// Part 1 is intact: a completion with it alone succeeds.
	_, err = c.svc.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: "bkt", Key: "obj", UploadID: uploadID, Parts: parts[:1]})
	require.NoError(t, err)
}

// TestFailedPartRetryKeepsAcknowledgedPart: a re-upload of part 1 whose
// description misses the write quorum (D == P) is rolled back: the
// acknowledged part 1 stays listed and completable, whichever drives the
// failed write reached.
func TestFailedPartRetryKeepsAcknowledgedPart(t *testing.T) {
	for _, failing := range [][]int{{2, 3}, {0, 1}} {
		t.Run(fmt.Sprint(failing), func(t *testing.T) {
			ctx := context.Background()
			c := newCluster(t, clusterSpec{pools: 1, sets: 1, drives: 4})
			require.NoError(t, c.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
			up, err := c.svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "bkt", Key: "obj"})
			require.NoError(t, err)
			b1 := payload(2*testBlockSize + 1)
			p1, err := c.svc.PutObjectPart(ctx, object.PutObjectPartInput{Bucket: "bkt", Key: "obj", UploadID: up.UploadID,
				PartNumber: 1, Reader: bytes.NewReader(b1), Size: int64(len(b1))})
			require.NoError(t, err)

			b2 := bytes.Repeat([]byte("z"), 2*testBlockSize+1)
			c.setFaults(failWriteAllOf("part.1.meta"), failing...)
			_, err = c.svc.PutObjectPart(ctx, object.PutObjectPartInput{Bucket: "bkt", Key: "obj", UploadID: up.UploadID,
				PartNumber: 1, Reader: bytes.NewReader(b2), Size: int64(len(b2))})
			c.setFaults(func(f *driveFaults) { f.failWriteAll = nil }, failing...)
			require.ErrorIs(t, err, domain.ErrWriteQuorum, "the retry is not acknowledged")

			lp, err := c.svc.ListObjectParts(ctx, domain.ListPartsInput{Bucket: "bkt", Key: "obj", UploadID: up.UploadID})
			require.NoError(t, err)
			require.Len(t, lp.Parts, 1)
			assert.Equal(t, p1.ETag, lp.Parts[0].ETag, "ListParts shows the acknowledged part, not the refused retry")
			_, err = c.svc.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: "bkt", Key: "obj", UploadID: up.UploadID,
				Parts: []domain.CompletePart{{Number: 1, ETag: p1.ETag}}})
			require.NoError(t, err, "the acknowledged part is still completable")
			got, err := readObject(t, c.svc, "bkt", "obj")
			require.NoError(t, err)
			assert.Equal(t, b1, got)
		})
	}
}

// reopen builds the erasure layer over the drives at roots, as a restart
// of the node does (the drives empty their temporary area when opened).
func reopen(t *testing.T, roots []string) object.Service {
	t.Helper()
	var set []storage.StorageAPI
	for i, r := range roots {
		set = append(set, testutil.NewLocalDriveIndex(t, r, i))
	}
	svc, err := object.NewServerPools(object.PoolsDeps{
		Pools: []object.PoolSpec{{Sets: [][]storage.StorageAPI{set}}}, DeploymentID: "deployment-test",
		Codecs: testCodecs("deployment-test"), Logger: slog.New(slog.DiscardHandler),
	}, object.WithInlineLimit(testInlineLimit), object.WithBlockSize(testBlockSize), object.WithMinPartSize(16),
		object.WithLockTimeout(5*time.Second))
	require.NoError(t, err)
	return svc
}

// snapshotDrives copies the drives at roots into a fresh directory and
// returns the roots of the copy.
func snapshotDrives(t *testing.T, roots []string) []string {
	t.Helper()
	base := t.TempDir()
	out := make([]string, len(roots))
	for i, r := range roots {
		out[i] = filepath.Join(base, fmt.Sprintf("d%d", i))
		res, err := exec.Command("cp", "-a", r, out[i]).CombinedOutput()
		require.NoError(t, err, string(res))
	}
	return out
}

// TestCompletionCrashKeepsUpload: the node crashes during
// CompleteMultipartUpload, once the parts are assembled and before the
// commit. After the restart the object does not exist and the retried
// completion (SDKs retry by themselves) succeeds with the acknowledged
// parts.
func TestCompletionCrashKeepsUpload(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t, clusterSpec{pools: 1, sets: 1, drives: 4})
	require.NoError(t, c.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
	p1, p2 := payload(3*testBlockSize+5), payload(2*testBlockSize+7)
	up, parts := uploadParts(t, c.svc, "bkt", "obj", "", p1, p2)

	var once sync.Once
	var crashed []string
	c.setFaults(func(f *driveFaults) {
		f.beforeCommit = func(context.Context, string, string) {
			once.Do(func() { crashed = snapshotDrives(t, c.roots) })
		}
	}, 0, 1, 2, 3)
	_, err := c.svc.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: "bkt", Key: "obj", UploadID: up, Parts: parts})
	require.NoError(t, err)
	require.Len(t, crashed, 4)

	restarted := reopen(t, crashed)
	_, err = readObject(t, restarted, "bkt", "obj")
	require.ErrorIs(t, err, domain.ErrObjectNotFound, "the interrupted completion left no object")
	lp, err := restarted.ListObjectParts(ctx, domain.ListPartsInput{Bucket: "bkt", Key: "obj", UploadID: up})
	require.NoError(t, err, "the upload survives the crash")
	assert.Len(t, lp.Parts, 2)
	_, err = restarted.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: "bkt", Key: "obj", UploadID: up, Parts: parts})
	require.NoError(t, err, "the retried completion finds its acknowledged parts")
	got, err := readObject(t, restarted, "bkt", "obj")
	require.NoError(t, err)
	assert.Equal(t, append(p1, p2...), got)
	assertNoUploadLeft(t, crashed, "bkt")
}

// TestCompletionInterruptedAfterCommit: the completion committed but the
// removal of the upload did not happen (crash right after the commit):
// the retried completion returns the committed object and removes the
// upload.
func TestCompletionInterruptedAfterCommit(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t, clusterSpec{pools: 1, sets: 1, drives: 4})
	require.NoError(t, c.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
	p1, p2 := payload(2*testBlockSize+5), payload(testBlockSize+7)
	up, parts := uploadParts(t, c.svc, "bkt", "obj", "", p1, p2)
	all := []int{0, 1, 2, 3}
	c.setFaults(func(f *driveFaults) {
		f.failDelete = func(p string) error {
			if strings.HasSuffix(p, "/"+up) {
				return fmt.Errorf("%w: injected: crash before the upload removal", domain.ErrFaultyDisk)
			}
			return nil
		}
	}, all...)
	first, err := c.svc.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: "bkt", Key: "obj", UploadID: up, Parts: parts})
	require.NoError(t, err)
	c.setFaults(func(f *driveFaults) { f.failDelete = nil }, all...)

	again, err := c.svc.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: "bkt", Key: "obj", UploadID: up, Parts: parts})
	require.NoError(t, err, "the retried completion finds its committed version")
	assert.Equal(t, first.ETag, again.ETag)
	got, err := readObject(t, c.svc, "bkt", "obj")
	require.NoError(t, err)
	assert.Equal(t, append(p1, p2...), got)
	assertNoUploadLeft(t, c.roots, "bkt")
}

// TestRefusedSystemWriteRollsBack: a system file write below the write
// quorum leaves the previous content on every drive (D == P: a refused
// write on two drives must not tie with the acknowledged content), and
// two contents each on a read quorum are refused rather than chosen by
// drive order.
func TestRefusedSystemWriteRollsBack(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t, clusterSpec{pools: 1, sets: 1, drives: 4})
	require.NoError(t, c.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
	up, err := c.svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "bkt", Key: "obj"})
	require.NoError(t, err)
	dir := filepath.Join(domain.SystemBucket, storage.MultipartDir, "bkt", up.UploadID)
	before := make([][]byte, 4)
	for i, r := range c.roots {
		before[i], err = os.ReadFile(filepath.Join(r, dir, "upload.json"))
		require.NoError(t, err)
	}

	// A new upload whose description reaches two drives only is refused
	// and removed from them.
	c.setFaults(failWriteAllOf("upload.json"), 0, 1)
	_, err = c.svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "bkt", Key: "other"})
	c.setFaults(func(f *driveFaults) { f.failWriteAll = nil }, 0, 1)
	require.ErrorIs(t, err, domain.ErrWriteQuorum)
	res, err := c.svc.ListMultipartUploads(ctx, domain.ListMultipartUploadsInput{Bucket: "bkt"})
	require.NoError(t, err)
	require.Len(t, res.Uploads, 1, "the refused upload is not listed")
	assert.Equal(t, up.UploadID, res.Uploads[0].UploadID)
	for i, r := range c.roots {
		got, err := os.ReadFile(filepath.Join(r, dir, "upload.json"))
		require.NoError(t, err)
		assert.Equal(t, before[i], got)
	}

	// Two contents each on two drives (a crash in the middle of a write):
	// the read refuses to choose.
	for _, i := range []int{2, 3} {
		require.NoError(t, os.WriteFile(filepath.Join(c.roots[i], dir, "upload.json"), []byte(`{"tie":true}`), 0o640))
	}
	_, err = c.svc.ListObjectParts(ctx, domain.ListPartsInput{Bucket: "bkt", Key: "obj", UploadID: up.UploadID})
	require.ErrorIs(t, err, domain.ErrReadQuorum)
}

// TestRefusedDeleteRestoresVersion: a delete that misses its write quorum
// puts the version back on the drives it reached: the object stays whole
// and deletable once the drives recover.
func TestRefusedDeleteRestoresVersion(t *testing.T) {
	for _, size := range []int{10, 2*testBlockSize + 5} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			ctx := context.Background()
			c := newCluster(t, clusterSpec{pools: 1, sets: 1, drives: 4})
			require.NoError(t, c.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
			body := payload(size)
			put(t, c.svc, "bkt", "k", body)
			c.setFaults(func(f *driveFaults) { f.failDeleteVersion = fmt.Errorf("%w: injected", domain.ErrFaultyDisk) }, 2, 3)
			_, err := c.svc.DeleteObject(ctx, "bkt", "k", object.DeleteObjectOptions{})
			c.setFaults(func(f *driveFaults) { f.failDeleteVersion = nil }, 2, 3)
			require.ErrorIs(t, err, domain.ErrWriteQuorum)
			// Drives 0 and 1 got the version back, with its data.
			c.setFaults(offline(true), 2, 3)
			got, err := readObject(t, c.svc, "bkt", "k")
			c.setFaults(offline(false), 2, 3)
			require.NoError(t, err, "the refused delete restored drives 0 and 1")
			assert.Equal(t, body, got)

			// Too few drives online: refused before touching any drive.
			c.setFaults(offline(true), 2, 3)
			_, err = c.svc.DeleteObject(ctx, "bkt", "k", object.DeleteObjectOptions{})
			c.setFaults(offline(false), 2, 3)
			require.ErrorIs(t, err, domain.ErrWriteQuorum)
			for _, i := range []int{0, 1} {
				_, err := os.Stat(filepath.Join(c.roots[i], "bkt", "k", storage.MetaFile))
				require.NoError(t, err, "drive %d untouched", i)
			}

			_, err = c.svc.DeleteObject(ctx, "bkt", "k", object.DeleteObjectOptions{})
			require.NoError(t, err)
			for _, r := range c.roots {
				_, err := os.Stat(filepath.Join(r, "bkt", "k"))
				assert.ErrorIs(t, err, os.ErrNotExist, "the object and its data are gone from %s", r)
			}
		})
	}
}

// TestNullVersionHealNotification: a heal-on-read of the null version,
// which is not the latest one, queues the null version ("null"), not the
// latest one ("").
func TestNullVersionHealNotification(t *testing.T) {
	ctx := context.Background()
	buckets := &switchingBuckets{state: domain.VersioningSuspended}
	c := newCluster(t, clusterSpec{pools: 1, sets: 1, drives: 4, buckets: buckets})
	require.NoError(t, c.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
	nullBody := payload(2*testBlockSize + 3)
	put(t, c.svc, "bkt", "k", nullBody)
	buckets.set(domain.VersioningEnabled)
	put(t, c.svc, "bkt", "k", payload(2*testBlockSize+9))
	_ = c.heal.notified()

	matches, err := filepath.Glob(filepath.Join(c.roots[0], "bkt", "k", "*", "part.1"))
	require.NoError(t, err)
	require.Len(t, matches, 2)
	for _, m := range matches {
		require.NoError(t, os.Remove(m))
	}
	rd, err := c.svc.GetObject(ctx, "bkt", "k", object.GetObjectOptions{VersionID: "null"})
	require.NoError(t, err)
	got, err := io.ReadAll(rd)
	require.NoError(t, err)
	require.NoError(t, rd.Close())
	assert.Equal(t, nullBody, got)
	assert.Contains(t, c.heal.notified(), "bkt/k@null")

	// The queued entry heals the null version.
	res, err := healerOf(t, c.svc).HealObject(ctx, "bkt", "k", "null", object.HealOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, res.Healed)
}

// switchingBuckets is a BucketConfigSource whose versioning state the test
// changes.
type switchingBuckets struct {
	mu    sync.Mutex
	state domain.Versioning
}

func (s *switchingBuckets) BucketConfig(context.Context, string) (object.BucketConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return object.BucketConfig{Versioning: s.state}, nil
}

func (s *switchingBuckets) set(v domain.Versioning) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = v
}

// corruptMeta overwrites the xl.meta of bucket/key on drive i.
func corruptMeta(t *testing.T, c *cluster, i int, bucket, key string) string {
	t.Helper()
	name := filepath.Join(c.roots[i], bucket, key, storage.MetaFile)
	require.NoError(t, os.WriteFile(name, []byte("garbage, not an xl.meta"), 0o640))
	return name
}

// TestHealCorruptMetadata: a drive whose xl.meta is undecodable is
// repaired by HealObject, every version of the key included: afterwards
// the drive alone with others of the minimum read quorum serves them.
func TestHealCorruptMetadata(t *testing.T) {
	for _, size := range []int{10, 2*testBlockSize + 5} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			ctx := context.Background()
			c := newCluster(t, clusterSpec{pools: 1, sets: 1, drives: 4, buckets: versionedBuckets(t)})
			require.NoError(t, c.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
			old := payload(size + 1)
			v1 := put(t, c.svc, "bkt", "k", old)
			body := payload(size)
			put(t, c.svc, "bkt", "k", body)
			meta := corruptMeta(t, c, 1, "bkt", "k")

			got, err := readObject(t, c.svc, "bkt", "k")
			require.NoError(t, err)
			assert.Equal(t, body, got)

			res, err := healerOf(t, c.svc).HealObject(ctx, "bkt", "k", "", object.HealOptions{})
			require.NoError(t, err, "the corrupt metadata is healed")
			assert.Len(t, res.VersionIDs, 2, "both versions go back on the drive")
			data, err := os.ReadFile(meta)
			require.NoError(t, err)
			assert.NotContains(t, string(data), "garbage")
			quarantined, err := os.ReadDir(filepath.Join(c.roots[1], domain.SystemBucket, storage.QuarantineDir))
			require.NoError(t, err)
			assert.Len(t, quarantined, 1, "the corrupt copy is kept in quarantine")

			c.setFaults(offline(true), 2, 3)
			defer c.setFaults(offline(false), 2, 3)
			got, err = readObject(t, c.svc, "bkt", "k")
			require.NoError(t, err, "drive 1 holds a valid copy after the heal")
			assert.Equal(t, body, got)
			rd, err := c.svc.GetObject(ctx, "bkt", "k", object.GetObjectOptions{VersionID: v1.VersionID})
			require.NoError(t, err, "the older version too")
			got, err = io.ReadAll(rd)
			require.NoError(t, err)
			require.NoError(t, rd.Close())
			assert.Equal(t, old, got)
		})
	}
}

// TestCorruptMetadataDoesNotBlockWrites: with the xl.meta of two drives
// of four undecodable (as many as the parity), the object is readable,
// and it can be overwritten and deleted: an undecodable xl.meta does not
// take the write quorum away.
func TestCorruptMetadataDoesNotBlockWrites(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t, clusterSpec{pools: 1, sets: 1, drives: 4})
	require.NoError(t, c.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
	body := payload(2*testBlockSize + 5)
	put(t, c.svc, "bkt", "k", body)
	corruptMeta(t, c, 0, "bkt", "k")
	corruptMeta(t, c, 1, "bkt", "k")
	got, err := readObject(t, c.svc, "bkt", "k")
	require.NoError(t, err, "readable from drives 2 and 3")
	assert.Equal(t, body, got)

	nb := payload(testBlockSize + 1)
	put(t, c.svc, "bkt", "k", nb)
	c.setFaults(offline(true), 2, 3)
	got, err = readObject(t, c.svc, "bkt", "k")
	c.setFaults(offline(false), 2, 3)
	require.NoError(t, err, "the overwrite reached drives 0 and 1")
	assert.Equal(t, nb, got)

	corruptMeta(t, c, 2, "bkt", "k")
	corruptMeta(t, c, 3, "bkt", "k")
	_, err = c.svc.DeleteObject(ctx, "bkt", "k", object.DeleteObjectOptions{})
	require.NoError(t, err)
	_, err = c.svc.GetObjectInfo(ctx, "bkt", "k", object.GetObjectOptions{})
	require.ErrorIs(t, err, domain.ErrObjectNotFound)
}
