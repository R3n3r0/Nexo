package object_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/crypto"
	"github.com/r3n3r0/nexo/internal/service/object"
)

// TestPinnedReads: a GetObject opens the files of its range and releases
// the read lock before the body is consumed (MVCC on the data directory):
// a writer that replaces or deletes the key meanwhile neither waits for
// the reader nor changes what it reads, the reader streams the version it
// resolved to the end (ceph/s3-tests test_atomic_read_*).
func TestPinnedReads(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("an open file cannot be removed on Windows: reads keep their lock")
	}
	// A blocked writer fails fast instead of waiting for the reader.
	opts := []object.Option{object.WithLockTimeout(time.Second)}
	multipart := func(t *testing.T, fx *fixture, key string, parts ...[]byte) {
		t.Helper()
		ctx := context.Background()
		up, err := fx.svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "bkt", Key: key})
		require.NoError(t, err)
		var done []domain.CompletePart
		for i, p := range parts {
			out, err := fx.svc.PutObjectPart(ctx, object.PutObjectPartInput{Bucket: "bkt", Key: key, UploadID: up.UploadID,
				PartNumber: i + 1, Reader: bytes.NewReader(p), Size: int64(len(p))})
			require.NoError(t, err)
			done = append(done, domain.CompletePart{Number: i + 1, ETag: out.ETag})
		}
		_, err = fx.svc.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: "bkt", Key: key, UploadID: up.UploadID, Parts: done})
		require.NoError(t, err)
	}
	big := payload(10*testBlockSize + 123)
	parts := [][]byte{payload(3*testBlockSize + 7), payload(2 * testBlockSize), payload(testBlockSize + 1)}
	var whole []byte
	for _, p := range parts {
		whole = append(whole, p...)
	}
	sse := func(in *object.PutObjectInput) { in.SSE = domain.SSERequest{Type: domain.SSES3} }
	tests := []struct {
		name   string
		setup  func(t *testing.T, fx *fixture)
		opts   object.GetObjectOptions
		want   []byte
		change func(t *testing.T, fx *fixture) error
	}{
		{
			name:  "overwrite",
			setup: func(t *testing.T, fx *fixture) { put(t, fx.svc, "bkt", "k", big) },
			want:  big,
			change: func(t *testing.T, fx *fixture) error {
				_, err := fx.svc.PutObject(context.Background(), object.PutObjectInput{Bucket: "bkt", Key: "k", Reader: bytes.NewReader([]byte("new")), Size: 3})
				return err
			},
		},
		{
			name:  "delete",
			setup: func(t *testing.T, fx *fixture) { put(t, fx.svc, "bkt", "k", big) },
			want:  big,
			change: func(t *testing.T, fx *fixture) error {
				_, err := fx.svc.DeleteObject(context.Background(), "bkt", "k", object.DeleteObjectOptions{})
				return err
			},
		},
		{
			name:  "multipart overwritten",
			setup: func(t *testing.T, fx *fixture) { multipart(t, fx, "k", parts...) },
			want:  whole,
			change: func(t *testing.T, fx *fixture) error {
				_, err := fx.svc.PutObject(context.Background(), object.PutObjectInput{Bucket: "bkt", Key: "k", Reader: bytes.NewReader(big), Size: int64(len(big))})
				return err
			},
		},
		{
			name:  "range across parts",
			setup: func(t *testing.T, fx *fixture) { multipart(t, fx, "k", parts...) },
			opts:  object.GetObjectOptions{Range: &domain.Range{Start: 100, End: int64(len(parts[0]) + 50)}},
			want:  whole[100 : len(parts[0])+51],
			change: func(t *testing.T, fx *fixture) error {
				_, err := fx.svc.DeleteObject(context.Background(), "bkt", "k", object.DeleteObjectOptions{})
				return err
			},
		},
		{
			name:  "encrypted overwritten",
			setup: func(t *testing.T, fx *fixture) { put(t, fx.svc, "bkt", "k", payload(crypto.PackageSize+3000), sse) },
			want:  payload(crypto.PackageSize + 3000),
			change: func(t *testing.T, fx *fixture) error {
				_, err := fx.svc.PutObject(context.Background(), object.PutObjectInput{Bucket: "bkt", Key: "k", Reader: bytes.NewReader(big), Size: int64(len(big)),
					SSE: domain.SSERequest{Type: domain.SSES3}})
				return err
			},
		},
		{
			name:  "inline overwritten",
			setup: func(t *testing.T, fx *fixture) { put(t, fx.svc, "bkt", "k", []byte("small")) },
			want:  []byte("small"),
			change: func(t *testing.T, fx *fixture) error {
				_, err := fx.svc.PutObject(context.Background(), object.PutObjectInput{Bucket: "bkt", Key: "k", Reader: bytes.NewReader(big), Size: int64(len(big))})
				return err
			},
		},
	}
	forEachImplWith(t, opts, func(t *testing.T, fx *fixture) {
		ctx := context.Background()
		require.NoError(t, fx.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				tt.setup(t, fx)
				r, err := fx.svc.GetObject(ctx, "bkt", "k", tt.opts)
				require.NoError(t, err)
				defer func() { _ = r.Close() }()
				head := make([]byte, min(3, len(tt.want)))
				_, err = io.ReadFull(r, head)
				require.NoError(t, err)

				require.NoError(t, tt.change(t, fx), "the writer does not wait for the open reader")
				rest, err := io.ReadAll(r)
				require.NoError(t, err)
				assert.True(t, bytes.Equal(tt.want, append(head, rest...)), "the reader streams the version it opened")
			})
		}
	})
}

// TestPinnedReadsKeepRedundancy: a read that released its lock keeps the
// redundancy of the erasure coding. A shard the decoding adds after the
// first block (bitrot of a shard, a drive failing in the middle of the
// read) reads the version the read opened, even when a writer replaced or
// deleted it meanwhile (regression: the spare shard was reopened by path,
// the read lost its quorum and the body was truncated), with local drives
// and with drives of other nodes (pinned by handle, no data moved).
func TestPinnedReadsKeepRedundancy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("an open file cannot be removed on Windows: reads keep their lock")
	}
	ctx := context.Background()
	body := payload(8*testBlockSize + 77)
	// EC 2+2 over 4 drives: shards of testBlockSize/2 bytes, each block
	// prefixed by its 32-byte bitrot hash.
	const shardBlock = 32 + testBlockSize/2
	changes := map[string]func(c *cluster) error{
		"overwrite": func(c *cluster) error {
			_, err := c.svc.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: "k", Reader: bytes.NewReader([]byte("new")), Size: 3})
			return err
		},
		"delete": func(c *cluster) error {
			_, err := c.svc.DeleteObject(ctx, "bkt", "k", object.DeleteObjectOptions{})
			return err
		},
	}
	faults := map[string]struct {
		before func(t *testing.T, c *cluster, drive int) // before the GET
		during func(c *cluster, drive int)               // once the writer is done
	}{
		"bitrot at block 3": {before: func(t *testing.T, c *cluster, drive int) {
			files, err := filepath.Glob(filepath.Join(c.roots[drive], "bkt", "k", "*", "part.1"))
			require.NoError(t, err)
			require.Len(t, files, 1)
			raw, err := os.ReadFile(files[0])
			require.NoError(t, err)
			raw[3*shardBlock+32+5] ^= 0x10
			require.NoError(t, os.WriteFile(files[0], raw, 0o600))
		}},
		"drive failing mid-read": {during: func(c *cluster, drive int) {
			c.setFaults(func(f *driveFaults) { f.brokenReads = true }, drive)
		}},
	}
	for _, remote := range []bool{false, true} {
		for fname, fault := range faults {
			for cname, change := range changes {
				t.Run(fmt.Sprintf("remote=%v/%s/%s", remote, fname, cname), func(t *testing.T) {
					c := newCluster(t, clusterSpec{pools: 1, sets: 1, drives: 4, opts: []object.Option{object.WithLockTimeout(time.Second)}})
					if remote {
						// One local drive: the read streams one remote
						// shard and pins the other two by handle.
						c.setFaults(func(f *driveFaults) { f.remote = true }, 1, 2, 3)
					}
					require.NoError(t, c.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
					put(t, c.svc, "bkt", "k", body)
					drive := driveWithShard(t, c, "k", 1)
					dataDirs, err := filepath.Glob(filepath.Join(c.roots[drive], "bkt", "k", "*-*"))
					require.NoError(t, err)
					require.NotEmpty(t, dataDirs)
					if fault.before != nil {
						fault.before(t, c, drive)
					}

					r, err := c.svc.GetObject(ctx, "bkt", "k", object.GetObjectOptions{})
					require.NoError(t, err)
					defer func() { _ = r.Close() }()
					head := make([]byte, 3)
					_, err = io.ReadFull(r, head)
					require.NoError(t, err)
					require.NoError(t, change(c), "the writer does not wait for the open reader")
					for _, dir := range dataDirs {
						_, err := os.Stat(dir)
						require.ErrorIs(t, err, os.ErrNotExist, "the data the reader opened is gone from the drives")
					}
					if fault.during != nil {
						fault.during(c, drive)
					}
					rest, err := io.ReadAll(r)
					require.NoError(t, err, "the read keeps its quorum with the shards it pinned")
					assert.True(t, bytes.Equal(body, append(head, rest...)), "the whole version the read opened")
				})
			}
		}
	}
}

// TestPinnedReadsNeedEveryShard: a read that could not open the shard of
// an online drive for a reason other than a missing shard (a peer of an
// older release answering ErrNotImplemented to storage/open-file during a
// rolling upgrade, a full handle table, a network error) is not pinned: it
// keeps the lock of the object until it ends, so a writer waits for it,
// and the read keeps all the redundancy of the parity when a drive fails
// later (regression: the read released its lock with the D shards of its
// first streams only, and a change of the object followed by the loss of
// drives within the parity truncated the body).
func TestPinnedReadsNeedEveryShard(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("reads keep their lock on Windows")
	}
	ctx := context.Background()
	body := payload(8*testBlockSize + 77)
	for name, openErr := range map[string]error{
		"older peer":        fmt.Errorf("%w: rpc: no route /storage/open-file", domain.ErrNotImplemented),
		"full handle table": fmt.Errorf("%w: rpc: too many open handles", domain.ErrTooManyRequests),
	} {
		t.Run(name, func(t *testing.T) {
			c := newCluster(t, clusterSpec{pools: 1, sets: 1, drives: 4, opts: []object.Option{object.WithLockTimeout(300 * time.Millisecond)}})
			// One local drive, three remote ones that open no handle:
			// the read streams the shards it starts with (one local, one
			// remote: D=2 of EC 2+2) and could pin no other.
			c.setFaults(func(f *driveFaults) { f.remote = true; f.failOpenFile = openErr }, 1, 2, 3)
			require.NoError(t, c.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
			put(t, c.svc, "bkt", "k", body)

			r, err := c.svc.GetObject(ctx, "bkt", "k", object.GetObjectOptions{})
			require.NoError(t, err)
			defer func() { _ = r.Close() }()
			head := make([]byte, 3)
			_, err = io.ReadFull(r, head)
			require.NoError(t, err)
			_, err = c.svc.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: "k", Reader: bytes.NewReader([]byte("new")), Size: 3})
			require.ErrorIs(t, err, domain.ErrLockTimeout, "the writer waits for the unpinned read")
			// A drive of the first streams fails mid-read: the decoding
			// reads the parity of the version, still on the drives.
			c.setFaults(func(f *driveFaults) { f.brokenReads = true }, driveWithShard(t, c, "k", 1))
			rest, err := io.ReadAll(r)
			require.NoError(t, err)
			assert.True(t, bytes.Equal(body, append(head, rest...)), "the whole version the read opened")
			require.NoError(t, r.Close())
			_, err = c.svc.PutObject(ctx, object.PutObjectInput{Bucket: "bkt", Key: "k", Reader: bytes.NewReader([]byte("new")), Size: 3})
			require.NoError(t, err, "the lock is released with the read")
		})
	}
}

// TestPinnedReadsOverTooManyParts: a range over more parts than a read
// pins (object.WithPinnedReadParts) is read under the lock without opening
// any shard first (regression: the first parts were opened, streamed and
// thrown away before falling back to the lock).
func TestPinnedReadsOverTooManyParts(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t, clusterSpec{pools: 1, sets: 1, drives: 4})
	require.NoError(t, c.svc.MakeBucket(ctx, "bkt", object.MakeBucketOptions{}))
	up, err := c.svc.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "bkt", Key: "k"})
	require.NoError(t, err)
	var (
		done  []domain.CompletePart
		whole []byte
	)
	for n := 1; n <= 9; n++ {
		p := payload(testBlockSize + n)
		out, err := c.svc.PutObjectPart(ctx, object.PutObjectPartInput{Bucket: "bkt", Key: "k", UploadID: up.UploadID,
			PartNumber: n, Reader: bytes.NewReader(p), Size: int64(len(p))})
		require.NoError(t, err)
		done = append(done, domain.CompletePart{Number: n, ETag: out.ETag})
		whole = append(whole, p...)
	}
	_, err = c.svc.CompleteMultipartUpload(ctx, object.CompleteMultipartInput{Bucket: "bkt", Key: "k", UploadID: up.UploadID, Parts: done})
	require.NoError(t, err)
	opens := func() int {
		n := 0
		for _, f := range c.faults {
			n += f.get().opens
		}
		return n
	}

	for _, tc := range []struct {
		name   string
		rng    *domain.Range
		want   []byte
		pinned bool
	}{
		{name: "nine parts", want: whole},
		{name: "two parts", rng: &domain.Range{Start: 10, End: int64(2*testBlockSize + 1)}, want: whole[10 : 2*testBlockSize+2], pinned: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := opens()
			r, err := c.svc.GetObject(ctx, "bkt", "k", object.GetObjectOptions{Range: tc.rng})
			require.NoError(t, err)
			if tc.pinned {
				assert.Positive(t, opens()-before, "the shards of the range are opened before the lock is released")
			} else {
				assert.Zero(t, opens()-before, "nothing is opened before the body is read")
			}
			got, err := io.ReadAll(r)
			require.NoError(t, err)
			require.NoError(t, r.Close())
			assert.True(t, bytes.Equal(tc.want, got))
		})
	}
}
