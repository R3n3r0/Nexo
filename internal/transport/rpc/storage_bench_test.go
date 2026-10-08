package rpc_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// Benchmarks of the remote drive over loopback against the local drive:
//
//	go test -run '^$' -bench . ./internal/transport/rpc/
func drives(b *testing.B) map[string]storage.StorageAPI {
	b.Helper()
	remote, _ := remoteDrive(b, b.TempDir()+"/remote")
	return map[string]storage.StorageAPI{"local": openLocal(b, b.TempDir()+"/local"), "remote": remote}
}

func BenchmarkWriteMetadata(b *testing.B) {
	ctx := context.Background()
	for name, d := range drives(b) {
		require.NoError(b, d.MakeVol(ctx, "b"))
		b.Run(name, func(b *testing.B) {
			fi := storage.FileInfo{ModTime: time.Now(), Size: 1024, Data: bytes.Repeat([]byte("x"), 1024)}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := d.WriteMetadata(ctx, "b", fmt.Sprintf("k%d", i%1000), fi); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkReadVersion(b *testing.B) {
	ctx := context.Background()
	for name, d := range drives(b) {
		require.NoError(b, d.MakeVol(ctx, "b"))
		require.NoError(b, d.WriteMetadata(ctx, "b", "k", storage.FileInfo{ModTime: time.Now(), Size: 4, Data: []byte("data")}))
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := d.ReadVersion(ctx, "b", "k", "", storage.ReadOptions{ReadData: true}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkCreateFile streams 4 MiB shards (the throughput of an erasure
// write to a remote drive).
func BenchmarkCreateFile(b *testing.B) {
	ctx := context.Background()
	payload := bytes.Repeat([]byte("0123456789abcdef"), 1<<18) // 4 MiB
	for name, d := range drives(b) {
		b.Run(name, func(b *testing.B) {
			b.SetBytes(int64(len(payload)))
			for i := 0; i < b.N; i++ {
				path := storage.TmpDir + "/" + uuid.NewString() + "/part.1"
				if err := d.CreateFile(ctx, domain.SystemBucket, path, int64(len(payload)), bytes.NewReader(payload)); err != nil {
					b.Fatal(err)
				}
				if err := d.Delete(ctx, domain.SystemBucket, path, storage.DeleteOptions{}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkReadFileStream reads 4 MiB shards back.
func BenchmarkReadFileStream(b *testing.B) {
	ctx := context.Background()
	payload := bytes.Repeat([]byte("0123456789abcdef"), 1<<18)
	for name, d := range drives(b) {
		require.NoError(b, d.CreateFile(ctx, domain.SystemBucket, "tmp/bench/part.1", int64(len(payload)), bytes.NewReader(payload)))
		b.Run(name, func(b *testing.B) {
			b.SetBytes(int64(len(payload)))
			for i := 0; i < b.N; i++ {
				rc, err := d.ReadFileStream(ctx, domain.SystemBucket, "tmp/bench/part.1", 0, -1)
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

// BenchmarkWalkDir lists 1000 objects.
func BenchmarkWalkDir(b *testing.B) {
	ctx := context.Background()
	for name, d := range drives(b) {
		require.NoError(b, d.MakeVol(ctx, "b"))
		for i := range 1000 {
			require.NoError(b, d.WriteMetadata(ctx, "b", fmt.Sprintf("p%d/k%04d", i%10, i), storage.FileInfo{ModTime: time.Now(), Size: 1, Data: []byte("x")}))
		}
		b.Run(name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				n := 0
				err := d.WalkDir(ctx, storage.WalkDirOptions{Bucket: "b", Recursive: true}, func(storage.WalkEntry) error { n++; return nil })
				if err != nil || n != 1010 {
					b.Fatalf("walk: %d entries, %v", n, err)
				}
			}
		})
	}
}
