package storage_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/storage"
	"github.com/r3n3r0/nexo/internal/testutil"
)

const workers = 100

// putObject stores payload as a version of volume/key the way the object
// layer does: inline below SmallFileThreshold, otherwise streamed into tmp
// and committed with RenameData.
func putObject(ctx context.Context, s storage.StorageAPI, volume, key, versionID string, modTime time.Time, payload []byte) (storage.FileInfo, error) {
	fi := storage.FileInfo{VersionID: versionID, ModTime: modTime, Size: int64(len(payload)), Versioned: versionID != "",
		Metadata: map[string]string{"etag": fmt.Sprintf("%x", len(payload)), "content-type": "application/octet-stream"}}
	if len(payload) < storage.SmallFileThreshold {
		fi.Data = payload
		return fi, s.WriteMetadata(ctx, volume, key, fi)
	}
	tmp := storage.TmpDir + "/" + uuid.NewString()
	if err := s.CreateFile(ctx, domain.SystemBucket, tmp+"/part.1", int64(len(payload)), bytes.NewReader(payload)); err != nil {
		return fi, err
	}
	fi.DataDir = uuid.NewString()
	fi.Parts = []storage.PartInfo{{Number: 1, Size: fi.Size, ActualSize: fi.Size}}
	_, err := s.RenameData(ctx, domain.SystemBucket, tmp, fi, volume, key, storage.RenameOptions{})
	return fi, err
}

// getObject reads the content of a version back.
func getObject(ctx context.Context, s storage.StorageAPI, volume, key, versionID string) ([]byte, error) {
	fi, err := s.ReadVersion(ctx, volume, key, versionID, storage.ReadOptions{ReadData: true})
	if err != nil {
		return nil, err
	}
	if fi.DataDir == "" {
		return fi.Data, nil
	}
	var out []byte
	for _, p := range fi.Parts {
		rc, err := s.ReadFileStream(ctx, volume, storage.ObjectDataPath(key, fi.DataDir, p.Number), 0, p.Size)
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			return nil, err
		}
		out = append(out, data...)
	}
	return out, nil
}

func payloadFor(i, size int) []byte {
	return bytes.Repeat([]byte{byte('a' + i%26)}, size)
}

// run executes fn for every worker index and collects the errors.
func run(t *testing.T, n int, fn func(i int) error) {
	t.Helper()
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fn(i); err != nil {
				errs <- fmt.Errorf("worker %d: %w", i, err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func objectNames(t *testing.T, s storage.StorageAPI, opts storage.WalkDirOptions) []string {
	t.Helper()
	var names []string
	require.NoError(t, s.WalkDir(context.Background(), opts, func(e storage.WalkEntry) error {
		if !e.IsDir {
			names = append(names, e.Name)
		}
		return nil
	}))
	return names
}

func TestConcurrentDifferentKeys(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "drive")
	s := testutil.NewLocalDrive(t, dir)
	require.NoError(t, s.MakeVol(ctx, "b"))
	now := time.Now()

	keys := make([]string, workers)
	run(t, workers, func(i int) error {
		keys[i] = fmt.Sprintf("p%d/q%d/obj-%03d", i%5, i%3, i)
		size := 10 + i
		if i%4 == 0 {
			size = storage.SmallFileThreshold + i // data directory
		}
		payload := payloadFor(i, size)
		if _, err := putObject(ctx, s, "b", keys[i], "", now, payload); err != nil {
			return err
		}
		got, err := getObject(ctx, s, "b", keys[i], "")
		if err != nil {
			return err
		}
		if !bytes.Equal(payload, got) {
			return fmt.Errorf("content mismatch for %s", keys[i])
		}
		return nil
	})
	sort.Strings(keys)
	assert.Equal(t, keys, objectNames(t, s, storage.WalkDirOptions{Bucket: "b", Recursive: true}))

	// Concurrent deletes prune every directory, up to the volume.
	run(t, workers, func(i int) error {
		return s.DeleteVersion(ctx, "b", keys[i], storage.FileInfo{}, storage.DeleteOptions{})
	})
	entries, err := os.ReadDir(filepath.Join(dir, "b"))
	require.NoError(t, err)
	assert.Empty(t, entries)
	entries, err = os.ReadDir(filepath.Join(dir, domain.SystemBucket, storage.TmpDir))
	require.NoError(t, err)
	assert.Empty(t, entries, "no temporary file is left behind")
}

func TestConcurrentSameKeyVersions(t *testing.T) {
	ctx := context.Background()
	s := testutil.NewLocalDrive(t, "")
	require.NoError(t, s.MakeVol(ctx, "b"))
	base := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)

	ids := make([]string, workers)
	run(t, workers, func(i int) error {
		ids[i] = uuid.NewString()
		size := 100 + i
		if i%2 == 0 {
			size = storage.SmallFileThreshold + 1
		}
		_, err := putObject(ctx, s, "b", "same", ids[i], base.Add(time.Duration(i)*time.Second), payloadFor(i, size))
		return err
	})

	// Every version survived the concurrent read-modify-write cycles.
	latest, err := s.ReadVersion(ctx, "b", "same", "", storage.ReadOptions{})
	require.NoError(t, err)
	assert.Equal(t, workers, latest.NumVersions)
	assert.Equal(t, ids[workers-1], latest.VersionID, "versions are ordered by modification time")
	for i, id := range ids {
		got, err := getObject(ctx, s, "b", "same", id)
		require.NoError(t, err)
		assert.True(t, bytes.Equal(payloadFor(i, len(got)), got), "version %d", i)
	}

	run(t, workers, func(i int) error {
		return s.DeleteVersion(ctx, "b", "same", storage.FileInfo{VersionID: ids[i]}, storage.DeleteOptions{})
	})
	_, err = s.ReadVersion(ctx, "b", "same", "", storage.ReadOptions{})
	assert.ErrorIs(t, err, domain.ErrObjectNotFound)
}

func TestConcurrentNullVersionOverwrites(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "drive")
	s := testutil.NewLocalDrive(t, dir)
	require.NoError(t, s.MakeVol(ctx, "b"))
	now := time.Now()

	run(t, workers, func(i int) error {
		payload := payloadFor(i, storage.SmallFileThreshold+i)
		tmp := storage.TmpDir + "/" + uuid.NewString()
		if err := s.CreateFile(ctx, domain.SystemBucket, tmp+"/part.1", int64(len(payload)), bytes.NewReader(payload)); err != nil {
			return err
		}
		fi := storage.FileInfo{ModTime: now.Add(time.Duration(i)), Size: int64(len(payload)), DataDir: uuid.NewString(),
			Parts: []storage.PartInfo{{Number: 1, Size: int64(len(payload))}}}
		_, err := s.RenameData(ctx, domain.SystemBucket, tmp, fi, "b", "k", storage.RenameOptions{BaseOptions: storage.DeleteOptions{Immediate: true}})
		return err
	})
	fi, err := s.ReadVersion(ctx, "b", "k", "", storage.ReadOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, fi.NumVersions, "the null version is replaced, never duplicated")
	entries, err := os.ReadDir(filepath.Join(dir, "b", "k"))
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.ElementsMatch(t, []string{storage.MetaFile, fi.DataDir}, names, "replaced data directories are purged")
	got, err := getObject(ctx, s, "b", "k", "")
	require.NoError(t, err)
	assert.Len(t, got, int(fi.Size))
}

// TestConcurrentCreateDeleteSamePrefix races the creation of objects with
// the pruning of the empty directories left by deletes in the same prefix.
func TestConcurrentCreateDeleteSamePrefix(t *testing.T) {
	ctx := context.Background()
	s := testutil.NewLocalDrive(t, "")
	require.NoError(t, s.MakeVol(ctx, "b"))
	now := time.Now()
	run(t, workers, func(i int) error {
		for round := range 5 {
			key := fmt.Sprintf("deep/shared/prefix/k%d-%d", i, round)
			if _, err := putObject(ctx, s, "b", key, "", now, payloadFor(i, 16)); err != nil {
				return err
			}
			if err := s.DeleteVersion(ctx, "b", key, storage.FileInfo{}, storage.DeleteOptions{}); err != nil {
				return err
			}
		}
		_, err := putObject(ctx, s, "b", fmt.Sprintf("deep/shared/prefix/final-%03d", i), "", now, payloadFor(i, 16))
		return err
	})
	assert.Len(t, objectNames(t, s, storage.WalkDirOptions{Bucket: "b", Recursive: true}), workers)
}

// TestConcurrentParentChildPlacement is the regression test of the
// placement race: "k" and "k/x" (or "k/x/y") written concurrently took
// different key locks, both passed the placement check and both
// committed, leaving the child readable but invisible to listings. Now
// exactly one write of each pair wins, the other is refused, and what is
// readable is what is listed.
func TestConcurrentParentChildPlacement(t *testing.T) {
	ctx := context.Background()
	s := testutil.NewLocalDrive(t, "")
	require.NoError(t, s.MakeVol(ctx, "b"))
	now := time.Now()
	const pairs = 200
	keyOf := func(i int) string {
		parent := fmt.Sprintf("k%03d", i/2)
		switch {
		case i%2 == 0:
			return parent
		case i%4 == 1:
			return parent + "/x"
		default:
			return parent + "/x/y"
		}
	}
	var won, refused atomic.Int32
	run(t, 2*pairs, func(i int) error {
		size := 16
		if (i/2)%3 == 0 {
			size = storage.SmallFileThreshold + 1 // RenameData instead of WriteMetadata
		}
		_, err := putObject(ctx, s, "b", keyOf(i), "", now, payloadFor(i, size))
		switch {
		case err == nil:
			won.Add(1)
		case errors.Is(err, domain.ErrInvalidObjectName):
			refused.Add(1)
		default:
			return err
		}
		return nil
	})
	assert.Equal(t, int32(pairs), won.Load(), "one write of each pair wins")
	assert.Equal(t, int32(pairs), refused.Load(), "the other is refused")

	var readable []string
	for i := range 2 * pairs {
		if _, err := s.ReadVersion(ctx, "b", keyOf(i), "", storage.ReadOptions{}); err == nil {
			readable = append(readable, keyOf(i))
		}
	}
	sort.Strings(readable)
	assert.Equal(t, readable, objectNames(t, s, storage.WalkDirOptions{Bucket: "b", Recursive: true}),
		"every readable object is listed")
}

// TestConcurrentPlacementWithDeletes races writes of children with the
// creation and removal of their parent object (delete markers included):
// no write may commit under a parent that is an object.
func TestConcurrentPlacementWithDeletes(t *testing.T) {
	ctx := context.Background()
	s := testutil.NewLocalDrive(t, "")
	require.NoError(t, s.MakeVol(ctx, "b"))
	now := time.Now()
	run(t, workers, func(i int) error {
		for round := range 10 {
			var err error
			switch (i + round) % 3 {
			case 0:
				_, err = putObject(ctx, s, "b", "p", "", now, payloadFor(i, 8))
			case 1:
				err = s.DeleteVersion(ctx, "b", "p", storage.FileInfo{}, storage.DeleteOptions{})
			default:
				key := fmt.Sprintf("p/c%d", i%4)
				if _, err = putObject(ctx, s, "b", key, "", now, payloadFor(i, 8)); err == nil {
					err = s.DeleteVersion(ctx, "b", key, storage.FileInfo{}, storage.DeleteOptions{})
				}
			}
			if err != nil && !errors.Is(err, domain.ErrInvalidObjectName) && !errors.Is(err, domain.ErrObjectNotFound) {
				return err
			}
		}
		return nil
	})
	names := objectNames(t, s, storage.WalkDirOptions{Bucket: "b", Recursive: true})
	if _, err := s.ReadVersion(ctx, "b", "p", "", storage.ReadOptions{}); err == nil {
		assert.Equal(t, []string{"p"}, names, "a parent object has no children")
	}
}
