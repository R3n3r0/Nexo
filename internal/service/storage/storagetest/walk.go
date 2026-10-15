package storagetest

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
)

func testWalkDir(t *testing.T, f Factory) {
	ctx := context.Background()
	s, dir := open(t, f)
	require.NoError(t, s.MakeVol(ctx, "b"))
	now := time.Now()
	for _, k := range []string{"a-b", "a.txt", "a/x", "a/y/z", "b.txt", "photos/", "photos/1.jpg", "photos/2024/a.jpg", "zeta"} {
		require.NoError(t, s.WriteMetadata(ctx, "b", k, version("", now, 1)), k)
	}
	// A stray file at the volume root is ignored.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b", "stray"), []byte("x"), 0o600))

	collect := func(opts storage.WalkDirOptions, fn func(e storage.WalkEntry) error) ([]string, error) {
		var out []string
		err := s.WalkDir(ctx, opts, func(e storage.WalkEntry) error {
			name := e.Name
			if e.IsDir {
				name = "D:" + name
			} else {
				require.NotEmpty(t, e.Metadata, name)
				fis, err := storage.DecodeFileInfos("b", e.Name, e.Metadata)
				require.NoError(t, err)
				require.Len(t, fis, 1)
				assert.Equal(t, e.Name, fis[0].Name)
			}
			out = append(out, name)
			if fn != nil {
				return fn(e)
			}
			return nil
		})
		return out, err
	}

	// Non-recursive: S3 key order ("a-b" < "a.txt" < "a/"), the directory
	// object "photos/" before the prefix of the same name.
	got, err := collect(storage.WalkDirOptions{Bucket: "b"}, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"a-b", "a.txt", "D:a/", "b.txt", "photos/", "D:photos/", "zeta"}, got)

	// Recursive.
	got, err = collect(storage.WalkDirOptions{Bucket: "b", Recursive: true}, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"a-b", "a.txt", "D:a/", "a/x", "D:a/y/", "a/y/z", "b.txt", "photos/", "D:photos/", "photos/1.jpg", "D:photos/2024/", "photos/2024/a.jpg", "zeta"}, got)

	// SkipDir skips the content of a prefix.
	got, err = collect(storage.WalkDirOptions{Bucket: "b", Recursive: true}, func(e storage.WalkEntry) error {
		if e.IsDir && e.Name == "photos/" {
			return fs.SkipDir
		}
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"a-b", "a.txt", "D:a/", "a/x", "D:a/y/", "a/y/z", "b.txt", "photos/", "D:photos/", "zeta"}, got)

	// SkipDir on a directory object skips the rest of its directory.
	got, err = collect(storage.WalkDirOptions{Bucket: "b", Recursive: true}, func(e storage.WalkEntry) error {
		if !e.IsDir && e.Name == "photos/" {
			return fs.SkipDir
		}
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"a-b", "a.txt", "D:a/", "a/x", "D:a/y/", "a/y/z", "b.txt", "photos/"}, got)

	// BaseDir + FilterPrefix + ForwardTo + Limit.
	got, err = collect(storage.WalkDirOptions{Bucket: "b", BaseDir: "photos", Recursive: true}, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"photos/1.jpg", "D:photos/2024/", "photos/2024/a.jpg"}, got)
	got, err = collect(storage.WalkDirOptions{Bucket: "b", FilterPrefix: "a", Recursive: true}, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"a-b", "a.txt", "D:a/", "a/x", "D:a/y/", "a/y/z"}, got)
	got, err = collect(storage.WalkDirOptions{Bucket: "b", FilterPrefix: "a/y/", Recursive: true}, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"D:a/", "D:a/y/", "a/y/z"}, got)
	got, err = collect(storage.WalkDirOptions{Bucket: "b", Recursive: true, ForwardTo: "b"}, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"b.txt", "photos/", "D:photos/", "photos/1.jpg", "D:photos/2024/", "photos/2024/a.jpg", "zeta"}, got)
	got, err = collect(storage.WalkDirOptions{Bucket: "b", Recursive: true, ForwardTo: "photos/2"}, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"D:photos/", "D:photos/2024/", "photos/2024/a.jpg", "zeta"}, got)
	got, err = collect(storage.WalkDirOptions{Bucket: "b", Recursive: true, Limit: 3}, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"a-b", "a.txt", "D:a/", "a/x"}, got, "prefixes do not count toward the limit")
	// Entries skipped by the callback do not count toward the limit.
	got, err = collect(storage.WalkDirOptions{Bucket: "b", Recursive: true, Limit: 3}, func(e storage.WalkEntry) error {
		if e.IsDir && e.Name == "a/" {
			return fs.SkipDir
		}
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"a-b", "a.txt", "D:a/", "b.txt"}, got)

	// Errors from the callback propagate; missing base dirs are reported on request.
	boom := errors.New("boom")
	_, err = collect(storage.WalkDirOptions{Bucket: "b"}, func(storage.WalkEntry) error { return boom })
	assert.ErrorIs(t, err, boom)
	got, err = collect(storage.WalkDirOptions{Bucket: "b", BaseDir: "nothing"}, nil)
	require.NoError(t, err)
	assert.Empty(t, got)
	_, err = collect(storage.WalkDirOptions{Bucket: "b", BaseDir: "nothing", ReportNotFound: true}, nil)
	assert.ErrorIs(t, err, domain.ErrObjectNotFound)
	_, err = collect(storage.WalkDirOptions{Bucket: "nobucket"}, nil)
	assert.ErrorIs(t, err, domain.ErrBucketNotFound)
	assert.ErrorIs(t, s.WalkDir(ctx, storage.WalkDirOptions{Bucket: "b"}, nil), domain.ErrInvalidArgument)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	assert.ErrorIs(t, s.WalkDir(canceled, storage.WalkDirOptions{Bucket: "b"}, func(storage.WalkEntry) error { return nil }), context.Canceled)
}

// testWalkDirOrder covers the keys whose order depends on the bytes that
// sort before '/' (space, '!', '-', '.') and on directory objects.
func testWalkDirOrder(t *testing.T, f Factory) {
	// Objects whose name is the start of other names ("a" vs "a b", "a/").
	objects := []string{
		"a", "a!", "a b", "a-b", "a.txt", "a/", "a0", "b/c/d", "b/c!", "b/c/",
		"c-d/e", "c/", "c/f", "file", "file.txt", "file-1/x", "z!/a", "z/a",
	}
	s, _ := open(t, f)
	require.NoError(t, s.MakeVol(context.Background(), "b"))
	writeKeys(t, s, "b", objects)
	assert.Equal(t, expectedWalk(objects), walkNames(t, s, storage.WalkDirOptions{Bucket: "b", Recursive: true}))

	// Prefixes ordered among objects ("a0" vs "a/", "c-d/" vs "c/").
	s2, _ := open(t, f)
	require.NoError(t, s2.MakeVol(context.Background(), "b"))
	prefixed := []string{"a/", "a/x", "a/ y", "a/!z", "a!", "a b", "a-b", "a.txt", "a0", "c-d/e", "c/", "c/f", "c!", "file/sub", "file.txt", "file-1/x"}
	writeKeys(t, s2, "b", prefixed)
	assert.Equal(t, expectedWalk(prefixed), walkNames(t, s2, storage.WalkDirOptions{Bucket: "b", Recursive: true}))
}

func testWalkDirTenThousandKeys(t *testing.T, f Factory) {
	ctx := context.Background()
	s, _ := open(t, f)
	require.NoError(t, s.MakeVol(ctx, "b"))
	rng := rand.New(rand.NewPCG(1, 2)) // #nosec G404 -- a deterministic generator of test data
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

func testWalkDirControl(t *testing.T, f Factory) {
	ctx := context.Background()
	s, dir := open(t, f)
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

	// SkipDir in a base directory ends the walk.
	got = nil
	require.NoError(t, s.WalkDir(ctx, storage.WalkDirOptions{Bucket: "b", BaseDir: "a", Recursive: true}, func(e storage.WalkEntry) error {
		got = append(got, e.Name)
		return fs.SkipDir
	}))
	assert.Equal(t, []string{"a/1"}, got)

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
