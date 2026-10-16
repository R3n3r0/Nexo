package storage_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// Benchmarks of the data files of a local drive (docs/bench, T-9.2):
//
//	go test -run '^$' -bench . -count 6 ./internal/service/storage/ > new.txt
//	benchstat old.txt new.txt
//
// The drive lives under NEXO_BENCH_DIR (default: the test temporary
// directory, so the file system of $TMPDIR): point it at the file system to
// measure (a disk rather than tmpfs for the cost of the writes). Writes are
// synced (the default of a server drive) unless the name says nosync.
func benchDrive(b *testing.B, opts ...storage.Option) storage.StorageAPI {
	b.Helper()
	parent := os.Getenv("NEXO_BENCH_DIR")
	if parent == "" {
		parent = b.TempDir()
	}
	dir, err := os.MkdirTemp(parent, "drive")
	require.NoError(b, err)
	b.Cleanup(func() { _ = os.RemoveAll(dir) })
	ep, err := domain.ParseEndpoint(dir)
	require.NoError(b, err)
	d, err := storage.NewLocal(storage.Deps{Logger: slog.New(slog.DiscardHandler)}, ep, opts...)
	require.NoError(b, err)
	b.Cleanup(func() { _ = d.Close(context.Background()) })
	return d
}

var benchSizes = []int{64 << 10, 1 << 20, 4 << 20, 64 << 20}

func sizeName(n int) string {
	if n >= 1<<20 {
		return fmt.Sprintf("%dMiB", n>>20)
	}
	return fmt.Sprintf("%dKiB", n>>10)
}

// BenchmarkCreateFile writes and commits a part file of each size (an
// SNSD part, or the shard of an erasure write), then deletes it.
func BenchmarkCreateFile(b *testing.B) {
	ctx := context.Background()
	for _, mode := range []string{"sync", "nosync"} {
		var opts []storage.Option
		if mode == "nosync" {
			opts = append(opts, storage.WithoutFsync())
		}
		d := benchDrive(b, opts...)
		for _, size := range benchSizes {
			payload := bytes.Repeat([]byte("0123456789abcdef"), size/16)
			b.Run(mode+"/"+sizeName(size), func(b *testing.B) {
				b.SetBytes(int64(size))
				for b.Loop() {
					path := storage.TmpDir + "/" + uuid.NewString() + "/part.1"
					if err := d.CreateFile(ctx, domain.SystemBucket, path, int64(size), bytes.NewReader(payload)); err != nil {
						b.Fatal(err)
					}
					if err := d.Delete(ctx, domain.SystemBucket, path, storage.DeleteOptions{}); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// BenchmarkCreateFileParallel writes 4 MiB shards from GOMAXPROCS
// goroutines at once (the shards of an erasure write on several drives of
// one disk).
func BenchmarkCreateFileParallel(b *testing.B) {
	ctx := context.Background()
	d := benchDrive(b)
	const size = 4 << 20
	payload := bytes.Repeat([]byte("0123456789abcdef"), size/16)
	b.SetBytes(size)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			path := storage.TmpDir + "/" + uuid.NewString() + "/part.1"
			if err := d.CreateFile(ctx, domain.SystemBucket, path, size, bytes.NewReader(payload)); err != nil {
				b.Error(err)
				return
			}
			if err := d.Delete(ctx, domain.SystemBucket, path, storage.DeleteOptions{}); err != nil {
				b.Error(err)
				return
			}
		}
	})
}

// BenchmarkReadFileStream reads a part file of each size back (warm page
// cache: the file was just written), whole and from an unaligned offset.
func BenchmarkReadFileStream(b *testing.B) {
	ctx := context.Background()
	d := benchDrive(b)
	for _, size := range benchSizes {
		payload := bytes.Repeat([]byte("0123456789abcdef"), size/16)
		path := "tmp/bench/" + sizeName(size) + "/part.1"
		require.NoError(b, d.CreateFile(ctx, domain.SystemBucket, path, int64(size), bytes.NewReader(payload)))
		for _, off := range []int64{0, 1000} {
			b.Run(fmt.Sprintf("%s/off%d", sizeName(size), off), func(b *testing.B) {
				b.SetBytes(int64(size) - off)
				for b.Loop() {
					rc, err := d.ReadFileStream(ctx, domain.SystemBucket, path, off, -1)
					if err != nil {
						b.Fatal(err)
					}
					if _, err := io.Copy(io.Discard, rc); err != nil {
						b.Fatal(err)
					}
					_ = rc.Close()
				}
			})
		}
	}
}
