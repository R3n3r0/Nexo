package erasure

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/minio/highwayhash"
)

// BitrotWriter implements Codec.
func (c *codec) BitrotWriter(ctx context.Context, w io.Writer) io.Writer {
	return &bitrotWriter{ctx: ctx, w: w, key: c.key}
}

// bitrotWriter prefixes every shard block with its HighwayHash-256.
type bitrotWriter struct {
	ctx context.Context
	w   io.Writer
	key []byte
}

func (b *bitrotWriter) Write(p []byte) (int, error) {
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	sum := highwayhash.Sum(p, b.key)
	if _, err := b.w.Write(sum[:]); err != nil {
		return 0, err
	}
	return b.w.Write(p)
}

// BitrotReader implements Codec.
func (c *codec) BitrotReader(ctx context.Context, open StreamFunc) ShardReader {
	return &bitrotReader{ctx: ctx, open: open, key: c.key, shardSize: c.shardSize}
}

// bitrotReader verifies the shard blocks of one shard file, read through
// a sequential stream opened lazily at the first block requested.
type bitrotReader struct {
	ctx       context.Context
	open      StreamFunc
	key       []byte
	shardSize int64

	mu     sync.Mutex
	rc     io.ReadCloser
	pos    int64 // file offset of the next byte of rc
	closed bool
}

var _ ShardReader = (*bitrotReader)(nil)

// stream returns the stream positioned at file offset off, (re)opening it
// when needed.
func (r *bitrotReader) stream(off int64) (io.ReadCloser, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, fmt.Errorf("erasure: shard reader closed: %w", io.ErrClosedPipe)
	}
	if r.rc != nil && r.pos == off {
		return r.rc, nil
	}
	if r.rc != nil {
		_ = r.rc.Close()
		r.rc = nil
	}
	rc, err := r.open(r.ctx, off)
	if err != nil {
		return nil, err
	}
	r.rc, r.pos = rc, off
	return rc, nil
}

// ReadAt implements ShardReader.
func (r *bitrotReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off%r.shardSize != 0 || int64(len(p)) > r.shardSize {
		return 0, fmt.Errorf("%w: shard read of %d bytes at %d (shard size %d)", ErrInvalidLayout, len(p), off, r.shardSize)
	}
	if len(p) == 0 {
		return 0, nil
	}
	fileOff := off / r.shardSize * (BitrotHashSize + r.shardSize)
	rc, err := r.stream(fileOff)
	if err != nil {
		return 0, err
	}
	var want [BitrotHashSize]byte
	if _, err := io.ReadFull(rc, want[:]); err != nil {
		r.invalidate()
		return 0, truncated(err, off)
	}
	n, err := io.ReadFull(rc, p)
	if err != nil {
		r.invalidate()
		return n, truncated(err, off)
	}
	r.mu.Lock()
	r.pos += BitrotHashSize + int64(n)
	r.mu.Unlock()
	got := highwayhash.Sum(p, r.key)
	if !bytes.Equal(got[:], want[:]) {
		r.invalidate()
		return n, &BitrotError{Offset: off, Want: want[:], Got: got[:]}
	}
	return n, nil
}

// truncated maps the end of a shard stream to ErrShardTruncated.
func truncated(err error, off int64) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("%w: block at offset %d", ErrShardTruncated, off)
	}
	return err
}

// invalidate forgets the stream position after a failed read: the next
// read reopens the stream.
func (r *bitrotReader) invalidate() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pos = -1
}

// Close implements ShardReader.
func (r *bitrotReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	if r.rc == nil {
		return nil
	}
	err := r.rc.Close()
	r.rc = nil
	return err
}
