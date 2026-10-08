// Package storagetest is the behavior suite of storage.StorageAPI. Every
// implementation must pass it unchanged: the local xl storage
// (storage.NewLocal) and the RPC client of a drive served by another node
// (internal/transport/rpc), which runs it against client → HTTP/2 server →
// local xl storage.
//
// The suite manipulates the drive directory directly (removing it, planting
// symbolic links, making it read-only), so the factory must open a drive
// whose data lives in the given directory of the local file system.
package storagetest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// Factory opens the drive rooted at dir (created when missing). The suite
// may open the same directory several times (a restart); the factory
// registers the cleanup of what it opens with t.Cleanup.
type Factory func(t *testing.T, dir string) storage.StorageAPI

// Run runs the whole behavior suite against the drives of newDrive. The
// cases run in parallel, each on its own directory.
func Run(t *testing.T, newDrive Factory) {
	cases := []struct {
		name string
		fn   func(t *testing.T, f Factory)
	}{
		{"DiskInfo", testDiskInfo},
		{"Volumes", testVolumes},
		{"MetadataLifecycle", testMetadataLifecycle},
		{"ObjectPlacementConflicts", testObjectPlacementConflicts},
		{"ObjectPlacementUUIDPrefix", testObjectPlacementUUIDPrefix},
		{"FilesAndRenameData", testFilesAndRenameData},
		{"ReadWriteAllAndDelete", testReadWriteAllAndDelete},
		{"WalkDir", testWalkDir},
		{"WalkDirOrder", testWalkDirOrder},
		{"WalkDirTenThousandKeys", testWalkDirTenThousandKeys},
		{"WalkDirControl", testWalkDirControl},
		{"FaultyDrive", testFaultyDrive},
		{"CreateFileLargeStream", testCreateFileLargeStream},
		{"DriveOffline", testDriveOffline},
		{"SymlinksAreNotFollowed", testSymlinksAreNotFollowed},
		{"HealingTracker", testHealingTracker},
		{"StorageErrors", testStorageErrors},
		{"LegacyMetadataOnDisk", testLegacyMetadataOnDisk},
		{"SharedDataDirSurvives", testSharedDataDirSurvives},
		{"KeyNames", testKeyNames},
		{"ObjectSizes", testObjectSizes},
		{"ConcurrentDifferentKeys", testConcurrentDifferentKeys},
		{"ConcurrentSameKeyVersions", testConcurrentSameKeyVersions},
		{"ConcurrentNullVersionOverwrites", testConcurrentNullVersionOverwrites},
		{"ConcurrentCreateDeleteSamePrefix", testConcurrentCreateDeleteSamePrefix},
		{"ConcurrentParentChildPlacement", testConcurrentParentChildPlacement},
		{"ConcurrentPlacementWithDeletes", testConcurrentPlacementWithDeletes},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			c.fn(t, newDrive)
		})
	}
}

// newDir returns a fresh drive directory (not created yet).
func newDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "drive")
}

// open opens a drive on a fresh directory and returns both.
func open(t *testing.T, f Factory) (storage.StorageAPI, string) {
	t.Helper()
	dir := newDir(t)
	return f(t, dir), dir
}

func version(id string, at time.Time, size int64) storage.FileInfo {
	return storage.FileInfo{VersionID: id, ModTime: at, Size: size, Metadata: map[string]string{"content-type": "text/plain"}}
}

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

// expectedWalk computes the recursive walk of a set of keys: every object
// and every prefix, in byte order, an object before the prefix of the same
// name.
func expectedWalk(keys []string) []string {
	type entry struct {
		name string
		dir  bool
	}
	seen := map[entry]bool{}
	var entries []entry
	for _, k := range keys {
		e := entry{name: k}
		if !seen[e] {
			seen[e] = true
			entries = append(entries, e)
		}
		for i := 0; i < len(k)-1; i++ {
			if k[i] == '/' {
				p := entry{name: k[:i+1], dir: true}
				if !seen[p] {
					seen[p] = true
					entries = append(entries, p)
				}
			}
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].name != entries[j].name {
			return entries[i].name < entries[j].name
		}
		return !entries[i].dir
	})
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.dir {
			out = append(out, "D:"+e.name)
		} else {
			out = append(out, e.name)
		}
	}
	return out
}

func walkNames(t *testing.T, s storage.StorageAPI, opts storage.WalkDirOptions) []string {
	t.Helper()
	var out []string
	require.NoError(t, s.WalkDir(context.Background(), opts, func(e storage.WalkEntry) error {
		if e.IsDir {
			out = append(out, "D:"+e.Name)
			return nil
		}
		infos, err := storage.DecodeFileInfos(opts.Bucket, e.Name, e.Metadata)
		if err != nil {
			return err
		}
		if len(infos) == 0 || infos[0].Data != nil {
			return fmt.Errorf("%s: walk entries carry metadata only", e.Name)
		}
		out = append(out, e.Name)
		return nil
	}))
	return out
}

func writeKeys(t *testing.T, s storage.StorageAPI, volume string, keys []string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	run(t, 8, func(w int) error {
		for i := w; i < len(keys); i += 8 {
			if err := s.WriteMetadata(ctx, volume, keys[i], storage.FileInfo{ModTime: now, Size: 1, Data: []byte("x")}); err != nil {
				return fmt.Errorf("%s: %w", keys[i], err)
			}
		}
		return nil
	})
}
