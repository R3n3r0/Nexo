package storage

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
)

// putSmall writes a small object (inline data) of one null version.
func putSmall(t *testing.T, s StorageAPI, volume, key, data string) {
	t.Helper()
	require.NoError(t, s.WriteMetadata(context.Background(), volume, key, FileInfo{ModTime: time.Now(), Size: int64(len(data)), Data: []byte(data)}))
}

// getSmall reads back the inline data of the latest version.
func getSmall(t *testing.T, s StorageAPI, volume, key string) (string, error) {
	t.Helper()
	fi, err := s.ReadVersion(context.Background(), volume, key, "", ReadOptions{ReadData: true})
	if err != nil {
		return "", err
	}
	return string(fi.Data), nil
}

func logDir(root string) string { return filepath.Join(root, domain.SystemBucket, MetaLogDir) }

func TestMetaLogReplay(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "d")
	s := openDrive(t, root, WithMetadataLog(0, 0))
	require.NoError(t, s.MakeVol(ctx, "b"))
	require.NoError(t, s.MakeVol(ctx, "gone"))
	for i := range 50 {
		putSmall(t, s, "b", fmt.Sprintf("k%02d", i), fmt.Sprintf("v%d", i))
	}
	putSmall(t, s, "b", "k07", "overwritten")
	require.NoError(t, s.DeleteVersion(ctx, "b", "k08", FileInfo{}, DeleteOptions{}))
	putSmall(t, s, "gone", "x", "y")
	require.NoError(t, s.DeleteVol(ctx, "gone", true))
	require.NoError(t, s.MakeVol(ctx, "gone"))
	putSmall(t, s, "gone", "new", "z")
	require.NoError(t, s.Close(ctx))

	// No object directory exists for small objects.
	_, err := os.Stat(filepath.Join(root, "b", "k01"))
	assert.True(t, os.IsNotExist(err))

	s = openDrive(t, root, WithMetadataLog(0, 0))
	got, err := getSmall(t, s, "b", "k07")
	require.NoError(t, err)
	assert.Equal(t, "overwritten", got)
	_, err = getSmall(t, s, "b", "k08")
	assert.ErrorIs(t, err, domain.ErrObjectNotFound)
	got, err = getSmall(t, s, "b", "k49")
	require.NoError(t, err)
	assert.Equal(t, "v49", got)
	_, err = getSmall(t, s, "gone", "x")
	assert.ErrorIs(t, err, domain.ErrObjectNotFound, "the drop hides the records of the removed volume")
	got, err = getSmall(t, s, "gone", "new")
	require.NoError(t, err)
	assert.Equal(t, "z", got)
	assert.Equal(t, int64(50), s.MetadataLogStats().Objects)
}

func TestMetaLogTornTail(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "d")
	s := openDrive(t, root, WithMetadataLog(0, 0))
	require.NoError(t, s.MakeVol(ctx, "b"))
	putSmall(t, s, "b", "a", "1")
	putSmall(t, s, "b", "b", "2")
	require.NoError(t, s.Close(ctx))

	// A crash in the middle of a write leaves half a record at the end.
	ids, err := (&metaLog{dir: logDir(root)}).segmentIDs()
	require.NoError(t, err)
	last := filepath.Join(logDir(root), segName(ids[len(ids)-1]))
	before, err := os.Stat(last)
	require.NoError(t, err)
	f, err := os.OpenFile(last, os.O_WRONLY|os.O_APPEND, 0) // #nosec G304 -- a file of the test drive
	require.NoError(t, err)
	_, err = f.Write([]byte(recMagic + "\x00\x00\x00\x00\x40\x00\x00\x00partial"))
	require.NoError(t, err)
	require.NoError(t, f.Close())

	s = openDrive(t, root, WithMetadataLog(0, 0))
	got, err := getSmall(t, s, "b", "b")
	require.NoError(t, err)
	assert.Equal(t, "2", got)
	after, err := os.Stat(last)
	require.NoError(t, err)
	assert.Equal(t, before.Size(), after.Size(), "the torn record is cut off")
	putSmall(t, s, "b", "c", "3")
	require.NoError(t, s.Close(ctx))
	s = openDrive(t, root, WithMetadataLog(0, 0))
	got, err = getSmall(t, s, "b", "c")
	require.NoError(t, err)
	assert.Equal(t, "3", got)
}

func TestMetaLogDamagedRecord(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "d")
	s := openDrive(t, root, WithMetadataLog(0, 0))
	require.NoError(t, s.MakeVol(ctx, "b"))
	for _, k := range []string{"a", "b", "c", "d"} {
		putSmall(t, s, "b", k, "value of "+k)
	}
	e, ok := s.ml().entry("b", "b")
	require.True(t, ok)
	path := filepath.Join(logDir(root), segName(e.seg))
	require.NoError(t, s.Close(ctx))
	before, err := os.Stat(path)
	require.NoError(t, err)

	// One flipped byte in the record of b, in the last segment.
	f, err := os.OpenFile(path, os.O_RDWR, 0) // #nosec G304 -- a file of the test drive
	require.NoError(t, err)
	_, err = f.WriteAt([]byte{0xff}, int64(e.off)+2)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	s = openDrive(t, root, WithMetadataLog(0, 0))
	_, err = getSmall(t, s, "b", "b")
	assert.ErrorIs(t, err, domain.ErrObjectNotFound, "only the damaged record is lost")
	for _, k := range []string{"a", "c", "d"} {
		got, err := getSmall(t, s, "b", k)
		require.NoError(t, err, k)
		assert.Equal(t, "value of "+k, got, "the records after the damage are kept")
	}
	after, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, before.Size(), after.Size(), "valid records follow: nothing is cut off")

	// The compaction of the segment skips the damage and keeps the rest.
	putSmall(t, s, "b", "a", "new a")
	putSmall(t, s, "b", "c", "new c")
	putSmall(t, s, "b", "d", "new d")
	n, _, err := s.ml().compact(false)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	require.NoError(t, s.Close(ctx))
	s = openDrive(t, root, WithMetadataLog(0, 0))
	got, err := getSmall(t, s, "b", "d")
	require.NoError(t, err)
	assert.Equal(t, "new d", got)
}

// TestMetaLogKilled reopens a drive whose process "died": no Close, the
// writer never stopped. Every acknowledged change is found again.
func TestMetaLogKilled(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "d")
	s := openDrive(t, root, WithMetadataLog(MinMetaLogSegmentSize, 0))
	require.NoError(t, s.MakeVol(ctx, "b"))
	big := string(bytes.Repeat([]byte("x"), 16<<10))
	for round := range 3 {
		for i := range 40 {
			putSmall(t, s, "b", fmt.Sprintf("k%02d", i), fmt.Sprintf("%d:%s", round, big))
		}
	}
	require.NoError(t, s.DeleteVersion(ctx, "b", "k00", FileInfo{}, DeleteOptions{}))
	_, _, err := s.ml().compact(false)
	require.NoError(t, err)
	fi := stageData(t, s, "up1", "large data")
	_, err = s.RenameData(ctx, domain.SystemBucket, TmpDir+"/up1", fi, "b", "large", RenameOptions{})
	require.NoError(t, err)
	putSmall(t, s, "b", "last", "acknowledged")

	s2 := openDrive(t, root, WithMetadataLog(MinMetaLogSegmentSize, 0))
	_, err = getSmall(t, s2, "b", "k00")
	assert.ErrorIs(t, err, domain.ErrObjectNotFound)
	for i := 1; i < 40; i++ {
		got, err := getSmall(t, s2, "b", fmt.Sprintf("k%02d", i))
		require.NoError(t, err)
		assert.Equal(t, "2:"+big, got)
	}
	got, err := getSmall(t, s2, "b", "last")
	require.NoError(t, err)
	assert.Equal(t, "acknowledged", got)
	large, err := s2.ReadVersion(ctx, "b", "large", "", ReadOptions{})
	require.NoError(t, err)
	assert.Equal(t, "large data", readPart(t, s2, "b", "large", large, 1))
}

// TestMetaLogRetiredSegmentKeepsDeletes checks that a compacted segment
// still pinned by a reader, so still on disk, counts as older: the deletes
// of the newer segments that hide its records are not dropped meanwhile.
func TestMetaLogRetiredSegmentKeepsDeletes(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "d")
	s := openDrive(t, root, WithMetadataLog(MinMetaLogSegmentSize, 0))
	require.NoError(t, s.MakeVol(ctx, "b"))
	big := string(bytes.Repeat([]byte("x"), 16<<10))
	putSmall(t, s, "b", "pinned", big)
	for i := range 80 {
		putSmall(t, s, "b", "garbage", fmt.Sprintf("%d:%s", i, big))
	}
	l := s.ml()
	_, g, ok := l.lookup("b", "pinned")
	require.True(t, ok)
	first := g.id
	_, _, err := l.compact(false)
	require.NoError(t, err)
	require.True(t, g.retired.Load(), "the first segment is compacted")
	assert.True(t, l.hasOlder(first+1), "still on disk while pinned")
	l.release(g)
	assert.False(t, l.hasOlder(first+1), "removed with its last reader")
	got, err := getSmall(t, s, "b", "pinned")
	require.NoError(t, err)
	assert.Equal(t, big, got)
}

func TestMetaLogGroupCommitConcurrent(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "d")
	s := openDrive(t, root, WithMetadataLog(0, 0))
	require.NoError(t, s.MakeVol(ctx, "b"))
	var wg sync.WaitGroup
	for w := range 16 {
		wg.Go(func() {
			for i := range 50 {
				putSmall(t, s, "b", fmt.Sprintf("w%02d/%03d", w, i), fmt.Sprintf("%d-%d", w, i))
			}
		})
	}
	wg.Wait()
	require.NoError(t, s.Close(ctx))
	s = openDrive(t, root, WithMetadataLog(0, 0))
	assert.Equal(t, int64(800), s.MetadataLogStats().Objects)
	var names []string
	require.NoError(t, s.WalkDir(ctx, WalkDirOptions{Bucket: "b"}, func(e WalkEntry) error {
		names = append(names, e.Name)
		return nil
	}))
	assert.Len(t, names, 16, "one prefix per writer")
	got, err := getSmall(t, s, "b", "w15/049")
	require.NoError(t, err)
	assert.Equal(t, "15-49", got)
}

func TestMetaLogCompaction(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "d")
	s := openDrive(t, root, WithMetadataLog(MinMetaLogSegmentSize, 0))
	require.NoError(t, s.MakeVol(ctx, "b"))
	big := string(bytes.Repeat([]byte("x"), 16<<10))
	// Five rounds of overwrites fill several segments, mostly garbage.
	for round := range 5 {
		for i := range 40 {
			putSmall(t, s, "b", fmt.Sprintf("k%02d", i), fmt.Sprintf("%d:%s", round, big))
		}
	}
	// A deleted key: its delete must survive the compaction of the
	// segments while its older puts remain.
	require.NoError(t, s.DeleteVersion(ctx, "b", "k00", FileInfo{}, DeleteOptions{}))
	before := s.MetadataLogStats()
	require.Greater(t, before.Segments, 3)

	n, _, err := s.ml().compact(false)
	require.NoError(t, err)
	assert.Positive(t, n)
	after := s.MetadataLogStats()
	assert.Less(t, after.Bytes, before.Bytes, "garbage reclaimed")
	assert.Equal(t, before.LiveBytes, after.LiveBytes)

	check := func(s *xlStorage) {
		t.Helper()
		_, err := getSmall(t, s, "b", "k00")
		assert.ErrorIs(t, err, domain.ErrObjectNotFound, "the deleted key stays deleted")
		for i := 1; i < 40; i++ {
			got, err := getSmall(t, s, "b", fmt.Sprintf("k%02d", i))
			require.NoError(t, err)
			assert.Equal(t, "4:"+big, got)
		}
	}
	check(s)
	require.NoError(t, s.Close(ctx))
	s = openDrive(t, root, WithMetadataLog(MinMetaLogSegmentSize, 0))
	check(s)
	assert.Equal(t, int64(39), s.MetadataLogStats().Objects)
}

func TestMetaLogConversion(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "d")

	// A drive with xl.meta files: small, large, directory and versioned
	// objects.
	s := openDrive(t, root)
	require.NoError(t, s.MakeVol(ctx, "b"))
	putSmall(t, s, "b", "small", "s")
	putSmall(t, s, "b", "dir/", "")
	putSmall(t, s, "b", "a/b/c", "deep")
	fi := stageData(t, s, "up1", "large data")
	_, err := s.RenameData(ctx, domain.SystemBucket, TmpDir+"/up1", fi, "b", "large", RenameOptions{})
	require.NoError(t, err)
	v1 := FileInfo{VersionID: "11111111-1111-4111-8111-111111111111", ModTime: t0, Data: []byte("one"), Size: 3}
	v2 := FileInfo{VersionID: "22222222-2222-4222-8222-222222222222", ModTime: t0.Add(time.Second), Data: []byte("two"), Size: 3}
	require.NoError(t, s.WriteMetadata(ctx, "b", "versioned", v1))
	require.NoError(t, s.WriteMetadata(ctx, "b", "versioned", v2))

	walk := func(s *xlStorage) []string {
		t.Helper()
		var names []string
		require.NoError(t, s.WalkDir(ctx, WalkDirOptions{Bucket: "b", Recursive: true}, func(e WalkEntry) error {
			names = append(names, e.Name)
			return nil
		}))
		return names
	}
	verify := func(s *xlStorage, walked ...string) {
		t.Helper()
		got, err := getSmall(t, s, "b", "small")
		require.NoError(t, err)
		assert.Equal(t, "s", got)
		got, err = getSmall(t, s, "b", "a/b/c")
		require.NoError(t, err)
		assert.Equal(t, "deep", got)
		large, err := s.ReadVersion(ctx, "b", "large", "", ReadOptions{})
		require.NoError(t, err)
		assert.Equal(t, "large data", readPart(t, s, "b", "large", large, 1))
		old, err := s.ReadVersion(ctx, "b", "versioned", v1.VersionID, ReadOptions{ReadData: true})
		require.NoError(t, err)
		assert.Equal(t, "one", string(old.Data))
		assert.Equal(t, 2, old.NumVersions)
		if len(walked) == 0 {
			walked = []string{"a/", "a/b/", "a/b/c", "dir/", "large", "small", "versioned"}
		}
		assert.Equal(t, walked, walk(s))
	}
	files := walk(s)
	require.NoError(t, s.Close(ctx))

	// Enabled: converted at open, the files are gone.
	s = openDrive(t, root, WithMetadataLog(0, 0))
	verify(s)
	assert.Equal(t, files, walk(s), "the same entries as the tree walk")
	assert.NoFileExists(t, filepath.Join(root, "b", "small", MetaFile))
	assert.NoFileExists(t, filepath.Join(root, "b", "large", MetaFile))
	assert.FileExists(t, filepath.Join(logDir(root), metaLogMarker))
	putSmall(t, s, "b", "after", "written in the log")
	require.NoError(t, s.Close(ctx))

	// Disabled again: converted back to files.
	s = openDrive(t, root)
	verify(s, "a/", "a/b/", "a/b/c", "after", "dir/", "large", "small", "versioned")
	got, err := getSmall(t, s, "b", "after")
	require.NoError(t, err)
	assert.Equal(t, "written in the log", got)
	assert.FileExists(t, filepath.Join(root, "b", "small", MetaFile))
	assert.NoDirExists(t, logDir(root))
	assert.Nil(t, s.ml())
}

func TestMetaLogInterruptedConversion(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "d")
	s := openDrive(t, root)
	require.NoError(t, s.MakeVol(ctx, "b"))
	putSmall(t, s, "b", "k", "v")
	require.NoError(t, s.Close(ctx))
	// A conversion that stopped before its marker: the files are the
	// truth, the partial log is discarded.
	require.NoError(t, os.MkdirAll(logDir(root), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(logDir(root), segName(1)), []byte("partial"), 0o600))
	s = openDrive(t, root)
	assert.NoDirExists(t, logDir(root))
	got, err := getSmall(t, s, "b", "k")
	require.NoError(t, err)
	assert.Equal(t, "v", got)
}

// TestMetaLogPruneStaleFiles checks that the xl.meta files an interrupted
// conversion leaves behind are removed before the drive serves from the
// log: a later conversion back to files must not resurrect an object
// deleted meanwhile.
func TestMetaLogPruneStaleFiles(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "d")
	s := openDrive(t, root)
	require.NoError(t, s.MakeVol(ctx, "b"))
	putSmall(t, s, "b", "gone", "deleted later")
	putSmall(t, s, "b", "keep", "kept")
	stalePath := filepath.Join(root, "b", "gone", MetaFile)
	stale, err := os.ReadFile(stalePath) // #nosec G304 -- a file of the test drive
	require.NoError(t, err)
	require.NoError(t, s.Close(ctx))

	s = openDrive(t, root, WithMetadataLog(0, 0))
	require.NoError(t, s.DeleteVersion(ctx, "b", "gone", FileInfo{}, DeleteOptions{}))
	require.NoError(t, s.Close(ctx))

	// The state of a conversion stopped before its files were removed.
	require.NoError(t, os.MkdirAll(filepath.Dir(stalePath), 0o750))
	require.NoError(t, os.WriteFile(stalePath, stale, 0o600))
	require.NoError(t, writeMarker(filepath.Join(logDir(root), metaLogPrune), false))

	s = openDrive(t, root, WithMetadataLog(0, 0))
	assert.NoFileExists(t, stalePath, "the stale file is removed at start")
	assert.NoFileExists(t, filepath.Join(logDir(root), metaLogPrune))
	require.NoError(t, s.Close(ctx))

	s = openDrive(t, root)
	_, err = getSmall(t, s, "b", "gone")
	assert.ErrorIs(t, err, domain.ErrObjectNotFound, "the deleted object stays deleted")
	got, err := getSmall(t, s, "b", "keep")
	require.NoError(t, err)
	assert.Equal(t, "kept", got)
}

func TestMetaLogDeleteVolNotEmpty(t *testing.T) {
	ctx := context.Background()
	s := openDrive(t, filepath.Join(t.TempDir(), "d"), WithMetadataLog(0, 0))
	require.NoError(t, s.MakeVol(ctx, "b"))
	putSmall(t, s, "b", "k", "v")
	assert.ErrorIs(t, s.DeleteVol(ctx, "b", false), domain.ErrBucketNotEmpty)
	require.NoError(t, s.DeleteVersion(ctx, "b", "k", FileInfo{}, DeleteOptions{}))
	require.NoError(t, s.DeleteVol(ctx, "b", false))
	assert.ErrorIs(t, s.WriteMetadata(ctx, "b", "k", FileInfo{ModTime: time.Now()}), domain.ErrBucketNotFound,
		"no record is added to a removed volume")
}

func BenchmarkMetaLogPut(b *testing.B) {
	ctx := context.Background()
	for _, mode := range []string{"files", "log"} {
		b.Run(mode, func(b *testing.B) {
			ep, err := domain.ParseEndpoint(filepath.Join(b.TempDir(), "d"))
			require.NoError(b, err)
			opts := []Option{}
			if mode == "log" {
				opts = append(opts, WithMetadataLog(0, 0))
			}
			d, err := NewLocal(Deps{Logger: slog.New(slog.DiscardHandler)}, ep, opts...)
			require.NoError(b, err)
			defer func() { _ = d.Close(ctx) }()
			require.NoError(b, d.MakeVol(ctx, "b"))
			data := bytes.Repeat([]byte("x"), 4<<10)
			var n int64
			var mu sync.Mutex
			b.SetParallelism(8)
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					mu.Lock()
					n++
					key := fmt.Sprintf("p%d/obj%d", n%64, n)
					mu.Unlock()
					if err := d.WriteMetadata(ctx, "b", key, FileInfo{ModTime: time.Now(), Size: int64(len(data)), Data: data}); err != nil {
						b.Fatal(err)
					}
				}
			})
		})
	}
}

// TestMetaLogForcedCompactionAndStats: a compaction asked for rewrites every
// sealed segment holding garbage, even under the background threshold;
// the counters and DiskInfo describe the log; nothing is lost across a
// restart.
func TestMetaLogForcedCompactionAndStats(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "d")
	s := openDrive(t, root, WithMetadataLog(MinMetaLogSegmentSize, 0))
	require.NoError(t, s.MakeVol(ctx, "b"))
	big := string(bytes.Repeat([]byte("y"), 16<<10))
	for i := range 120 {
		putSmall(t, s, "b", fmt.Sprintf("k%03d", i), big)
	}
	// A few overwrites: a little garbage, under the background threshold.
	for i := range 10 {
		putSmall(t, s, "b", fmt.Sprintf("k%03d", i), "v2:"+big)
	}
	n, _, err := s.ml().compact(false)
	require.NoError(t, err)
	assert.Zero(t, n, "the background threshold is not reached")

	before := s.MetadataLogStats()
	assert.Positive(t, before.Flushes)
	assert.GreaterOrEqual(t, before.Records, uint64(130))
	assert.Positive(t, before.FlushSeconds)
	segs, reclaimed, err := s.CompactMetadataLog(ctx)
	require.NoError(t, err)
	assert.Positive(t, segs)
	assert.Positive(t, reclaimed)
	after := s.MetadataLogStats()
	assert.Equal(t, uint64(segs), after.Compactions) // #nosec G115 -- small
	assert.Equal(t, uint64(reclaimed), after.CompactedBytes)
	assert.Equal(t, before.LiveBytes, after.LiveBytes)
	assert.Less(t, after.Bytes, before.Bytes)

	info, err := s.DiskInfo(ctx, DiskInfoOptions{})
	require.NoError(t, err)
	require.NotNil(t, info.MetadataLog)
	assert.Equal(t, int64(120), info.MetadataLog.Objects)
	assert.Equal(t, after.Compactions, info.MetadataLog.Compactions)

	check := func(s *xlStorage) {
		t.Helper()
		for i := range 120 {
			got, err := getSmall(t, s, "b", fmt.Sprintf("k%03d", i))
			require.NoError(t, err)
			want := big
			if i < 10 {
				want = "v2:" + big
			}
			assert.Equal(t, want, got, "k%03d", i)
		}
	}
	check(s)
	require.NoError(t, s.Close(ctx))
	s = openDrive(t, root, WithMetadataLog(MinMetaLogSegmentSize, 0))
	check(s)

	// A drive without the log: nothing to compact, no MetadataLog.
	plain := openDrive(t, filepath.Join(t.TempDir(), "p"))
	segs, _, err = plain.CompactMetadataLog(ctx)
	require.NoError(t, err)
	assert.Zero(t, segs)
	info, err = plain.DiskInfo(ctx, DiskInfoOptions{})
	require.NoError(t, err)
	assert.Nil(t, info.MetadataLog)
}
