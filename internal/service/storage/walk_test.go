package storage_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/storage"
	"github.com/r3n3r0/nexo/internal/testutil"
)

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

// TestWalkDirOrder covers the keys whose order depends on the bytes that
// sort before '/' (space, '!', '-', '.') and on directory objects.
func TestWalkDirOrder(t *testing.T) {
	// Objects whose name is the start of other names ("a" vs "a b", "a/").
	objects := []string{
		"a", "a!", "a b", "a-b", "a.txt", "a/", "a0", "b/c/d", "b/c!", "b/c/",
		"c-d/e", "c/", "c/f", "file", "file.txt", "file-1/x", "z!/a", "z/a",
	}
	s := testutil.NewLocalDrive(t, "")
	require.NoError(t, s.MakeVol(context.Background(), "b"))
	writeKeys(t, s, "b", objects)
	assert.Equal(t, expectedWalk(objects), walkNames(t, s, storage.WalkDirOptions{Bucket: "b", Recursive: true}))

	// Prefixes ordered among objects ("a0" vs "a/", "c-d/" vs "c/").
	s2 := testutil.NewLocalDrive(t, "")
	require.NoError(t, s2.MakeVol(context.Background(), "b"))
	prefixed := []string{"a/", "a/x", "a/ y", "a/!z", "a!", "a b", "a-b", "a.txt", "a0", "c-d/e", "c/", "c/f", "c!", "file/sub", "file.txt", "file-1/x"}
	writeKeys(t, s2, "b", prefixed)
	assert.Equal(t, expectedWalk(prefixed), walkNames(t, s2, storage.WalkDirOptions{Bucket: "b", Recursive: true}))
}

func TestWalkDirTenThousandKeys(t *testing.T) {
	ctx := context.Background()
	s := testutil.NewLocalDrive(t, "")
	require.NoError(t, s.MakeVol(ctx, "b"))
	rng := rand.New(rand.NewPCG(1, 2))
	alphabet := []rune("ab-._ ~!+%é文Z09")
	seen := map[string]bool{}
	var keys []string
	for len(keys) < 10_000 {
		var sb strings.Builder
		for range 1 + rng.IntN(2) { // one or two prefix levels
			seg := ""
			for seg == "" || seg == "." || seg == ".." {
				seg = string(alphabet[rng.IntN(len(alphabet))])
				if rng.IntN(2) == 0 {
					seg += string(alphabet[rng.IntN(len(alphabet))])
				}
			}
			sb.WriteString(seg + "/")
		}
		fmt.Fprintf(&sb, "obj-%05d", len(keys))
		if k := sb.String(); !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	writeKeys(t, s, "b", keys)
	want := expectedWalk(keys)
	assert.Equal(t, want, walkNames(t, s, storage.WalkDirOptions{Bucket: "b", Recursive: true}))

	sorted := append([]string(nil), keys...)
	sort.Strings(sorted)

	// Paging with Limit and ForwardTo returns every object exactly once.
	var paged []string
	forward := ""
	for {
		page := objectNames(t, s, storage.WalkDirOptions{Bucket: "b", Recursive: true, Limit: 1000, ForwardTo: forward})
		paged = append(paged, page...)
		if len(page) < 1000 {
			break
		}
		forward = page[len(page)-1] + "\x00"
	}
	assert.Equal(t, sorted, paged)

	// Non-recursive walks report the first level: the common prefixes of a
	// listing with the "/" delimiter.
	var top []string
	for _, e := range want {
		name := strings.TrimPrefix(e, "D:")
		if strings.Count(strings.TrimSuffix(name, "/"), "/") == 0 {
			top = append(top, e)
		}
	}
	assert.Equal(t, top, walkNames(t, s, storage.WalkDirOptions{Bucket: "b"}))

	// BaseDir + FilterPrefix select one branch.
	branch := strings.SplitN(sorted[len(sorted)/2], "/", 2)[0]
	var wantBranch []string
	for _, k := range sorted {
		if strings.HasPrefix(k, branch+"/") {
			wantBranch = append(wantBranch, k)
		}
	}
	assert.Equal(t, wantBranch, objectNames(t, s, storage.WalkDirOptions{Bucket: "b", BaseDir: branch, Recursive: true}))
	assert.Equal(t, wantBranch, objectNames(t, s, storage.WalkDirOptions{Bucket: "b", FilterPrefix: branch + "/", Recursive: true}))
}

func TestWalkDirControl(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "drive")
	s := testutil.NewLocalDrive(t, dir)
	require.NoError(t, s.MakeVol(ctx, "b"))
	writeKeys(t, s, "b", []string{"a/1", "a/2", "a/3", "b/1", "c"})

	// fs.SkipAll stops the walk without error.
	var got []string
	require.NoError(t, s.WalkDir(ctx, storage.WalkDirOptions{Bucket: "b", Recursive: true}, func(e storage.WalkEntry) error {
		got = append(got, e.Name)
		if e.Name == "a/2" {
			return fs.SkipAll
		}
		return nil
	}))
	assert.Equal(t, []string{"a/", "a/1", "a/2"}, got)

	// fs.SkipDir on an object skips the rest of its directory.
	got = nil
	require.NoError(t, s.WalkDir(ctx, storage.WalkDirOptions{Bucket: "b", Recursive: true}, func(e storage.WalkEntry) error {
		got = append(got, e.Name)
		if e.Name == "a/1" {
			return fs.SkipDir
		}
		return nil
	}))
	assert.Equal(t, []string{"a/", "a/1", "b/", "b/1", "c"}, got)

	// Cancellation interrupts the walk.
	canceled, cancel := context.WithCancel(ctx)
	got = nil
	err := s.WalkDir(canceled, storage.WalkDirOptions{Bucket: "b", Recursive: true}, func(e storage.WalkEntry) error {
		got = append(got, e.Name)
		cancel()
		return nil
	})
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, []string{"a/"}, got)

	// Leftovers are not entries: empty directories, stray files, links and
	// directories holding only files.
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "b", "empty", "deeper"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b", "stray"), []byte("x"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "b", "files"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b", "files", "f"), []byte("x"), 0o600))
	require.NoError(t, os.Symlink(filepath.Join(dir, "b", "a"), filepath.Join(dir, "b", "link")))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "b", "dirobj"+"__XLDIR__"), 0o750))
	assert.Equal(t, []string{"D:a/", "a/1", "a/2", "a/3", "D:b/", "b/1", "c"}, walkNames(t, s, storage.WalkDirOptions{Bucket: "b", Recursive: true}))

	// Base directories.
	assert.Empty(t, walkNames(t, s, storage.WalkDirOptions{Bucket: "b", BaseDir: "missing"}))
	assert.Empty(t, walkNames(t, s, storage.WalkDirOptions{Bucket: "b", BaseDir: "c"}), "an object directory has no entries")
	assert.Empty(t, walkNames(t, s, storage.WalkDirOptions{Bucket: "b", BaseDir: "stray"}))
	err = s.WalkDir(ctx, storage.WalkDirOptions{Bucket: "b", BaseDir: "stray", ReportNotFound: true}, func(storage.WalkEntry) error { return nil })
	assert.ErrorIs(t, err, domain.ErrObjectNotFound)
	err = s.WalkDir(ctx, storage.WalkDirOptions{Bucket: "b", BaseDir: "link"}, func(storage.WalkEntry) error { return nil })
	assert.ErrorIs(t, err, domain.ErrAccessDenied, "links are never followed")
	err = s.WalkDir(ctx, storage.WalkDirOptions{Bucket: "b", BaseDir: "../x"}, func(storage.WalkEntry) error { return nil })
	assert.ErrorIs(t, err, domain.ErrInvalidObjectName)

	// Errors of the callback and of the metadata reads propagate.
	boom := errors.New("boom")
	err = s.WalkDir(ctx, storage.WalkDirOptions{Bucket: "b", Recursive: true}, func(e storage.WalkEntry) error {
		if !e.IsDir {
			return boom
		}
		return nil
	})
	assert.ErrorIs(t, err, boom)
}
