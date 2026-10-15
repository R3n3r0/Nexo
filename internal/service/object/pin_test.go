package object_test

import (
	"bytes"
	"context"
	"io"
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
