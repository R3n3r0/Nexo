package erasure_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/erasure"
	erasuremock "github.com/r3n3r0/nexo/internal/service/erasure/mock"
)

// TestBitrotDetectsEveryBitFlip is TS-U-04: flipping any single bit of a
// shard file (hash or content, any block) is detected by the block read,
// with the typed error.
func TestBitrotDetectsEveryBitFlip(t *testing.T) {
	const bs = 40
	c := newCodec(t, 2, 1, bs)
	data := payload(2*bs + 9)
	files := encode(t, c, data)
	file := files[0]
	blockLens := []int{20, 20, 5} // shard lengths of the 3 blocks
	blockOf := func(pos int) int {
		start := 0
		for b, l := range blockLens {
			if pos < start+erasure.BitrotHashSize+l {
				return b
			}
			start += erasure.BitrotHashSize + l
		}
		return -1
	}
	for pos := 0; pos < len(file); pos++ {
		for bit := 0; bit < 8; bit += 3 {
			corrupt := bytes.Clone(file)
			corrupt[pos] ^= 1 << bit
			r := c.BitrotReader(context.Background(), memOpen(corrupt))
			var firstErr error
			failedBlock := -1
			for b, l := range blockLens {
				buf := make([]byte, l)
				if _, err := r.ReadAt(buf, int64(b)*c.ShardSize()); err != nil {
					firstErr, failedBlock = err, b
					break
				}
			}
			_ = r.Close()
			require.Error(t, firstErr, "flip of bit %d at %d undetected", bit, pos)
			var berr *erasure.BitrotError
			require.ErrorAs(t, firstErr, &berr)
			assert.ErrorIs(t, firstErr, erasure.ErrBitrot)
			assert.ErrorIs(t, firstErr, domain.ErrFaultyDisk)
			assert.Equal(t, blockOf(pos), failedBlock, "the corrupt block is the one reported")
			assert.Equal(t, int64(failedBlock)*c.ShardSize(), berr.Offset)
			assert.Contains(t, berr.Error(), "bitrot")
		}
	}
}

func TestBitrotReaderErrors(t *testing.T) {
	ctx := context.Background()
	c := newCodec(t, 2, 1, 40)
	files := encode(t, c, payload(90))

	t.Run("truncated", func(t *testing.T) {
		for _, cut := range []int{0, 10, erasure.BitrotHashSize + 3, len(files[0]) - 1} {
			r := c.BitrotReader(ctx, memOpen(files[0][:cut]))
			var err error
			for b, l := range []int{20, 20, 5} {
				if _, err = r.ReadAt(make([]byte, l), int64(b)*c.ShardSize()); err != nil {
					break
				}
			}
			assert.ErrorIs(t, err, erasure.ErrShardTruncated, "cut at %d", cut)
			_ = r.Close()
		}
	})
	t.Run("open failure", func(t *testing.T) {
		errOpen := fmt.Errorf("%w: offline", domain.ErrDiskNotFound)
		r := c.BitrotReader(ctx, func(context.Context, int64) (io.ReadCloser, error) { return nil, errOpen })
		_, err := r.ReadAt(make([]byte, 20), 0)
		assert.ErrorIs(t, err, domain.ErrDiskNotFound)
	})
	t.Run("misaligned or oversized reads", func(t *testing.T) {
		r := c.BitrotReader(ctx, memOpen(files[0]))
		defer r.Close()
		_, err := r.ReadAt(make([]byte, 20), 3)
		assert.ErrorIs(t, err, erasure.ErrInvalidLayout)
		_, err = r.ReadAt(make([]byte, 21), 0)
		assert.ErrorIs(t, err, erasure.ErrInvalidLayout)
		n, err := r.ReadAt(nil, 0)
		assert.NoError(t, err)
		assert.Zero(t, n)
	})
	t.Run("random access reopens the stream", func(t *testing.T) {
		opens := 0
		r := c.BitrotReader(ctx, func(ctx context.Context, off int64) (io.ReadCloser, error) {
			opens++
			return memOpen(files[1])(ctx, off)
		})
		buf := make([]byte, 20)
		_, err := r.ReadAt(buf, 20)
		require.NoError(t, err)
		_, err = r.ReadAt(buf[:5], 40)
		require.NoError(t, err, "sequential: same stream")
		_, err = r.ReadAt(buf, 0)
		require.NoError(t, err, "backwards: reopened")
		assert.Equal(t, 2, opens)
		require.NoError(t, r.Close())
		_, err = r.ReadAt(buf, 0)
		assert.Error(t, err, "closed")
	})
	t.Run("writer stops with the context", func(t *testing.T) {
		cctx, cancel := context.WithCancel(ctx)
		w := c.BitrotWriter(cctx, io.Discard)
		_, err := w.Write([]byte("abc"))
		require.NoError(t, err)
		cancel()
		_, err = w.Write([]byte("abc"))
		assert.ErrorIs(t, err, context.Canceled)
	})
}

// TestDecodeSetsCorruptShardsAside: a corrupt or failing shard is
// replaced by a parity shard and reported in ShardErrs.
func TestDecodeSetsCorruptShardsAside(t *testing.T) {
	c := newCodec(t, 3, 2, 30)
	data := payload(100)
	files := encode(t, c, data)
	corrupt := bytes.Clone(files[1])
	corrupt[erasure.BitrotHashSize+10+erasure.BitrotHashSize+2] ^= 0x10 // second block of shard 1 (shards of 10 bytes)
	files[1] = corrupt
	readers := readersFor(c, files, nil)
	defer closeAll(readers)
	got, res, err := decode(c, readers, 0, int64(len(data)), int64(len(data)))
	require.NoError(t, err)
	assert.Equal(t, data, got)
	var berr *erasure.BitrotError
	require.ErrorAs(t, res.ShardErrs[1], &berr)
	assert.Equal(t, c.ShardSize(), berr.Offset)
	for i, e := range res.ShardErrs {
		if i != 1 {
			assert.NoError(t, e, "shard %d", i)
		}
	}
}

// TestDecodeHedgesSlowShards is the unit side of TS-E-06: a shard that does
// not answer within the hedge delay is replaced by a parity shard and is
// not asked again, so that the decoding does not wait for it.
func TestDecodeHedgesSlowShards(t *testing.T) {
	const bs = 32
	c := newCodec(t, 2, 2, bs, erasure.WithHedgeDelay(10*time.Millisecond))
	data := payload(6 * bs)
	files := encode(t, c, data)
	readers := readersFor(c, files, nil)
	defer closeAll(readers)

	release := make(chan struct{})
	var slowCalls atomic.Int32
	slow := erasuremock.NewShardReader(t)
	slow.EXPECT().ReadAt(mock.Anything, mock.Anything).RunAndReturn(func(p []byte, off int64) (int, error) {
		slowCalls.Add(1)
		<-release
		return 0, errors.New("too late")
	}).Maybe()
	slow.EXPECT().Close().Return(nil).Maybe()
	readers[0] = slow

	start := time.Now()
	got, res, err := decode(c, readers, 0, int64(len(data)), int64(len(data)))
	elapsed := time.Since(start)
	close(release)
	require.NoError(t, err)
	assert.Equal(t, data, got)
	assert.Less(t, elapsed, time.Second, "the slow shard is not waited for")
	assert.Equal(t, int32(1), slowCalls.Load(), "a lagging shard is set aside after its first block")
	for _, e := range res.ShardErrs {
		assert.NoError(t, e)
	}
}

// TestDecodeWithoutHedging: with hedging disabled, shards are only
// replaced on errors.
func TestDecodeWithoutHedging(t *testing.T) {
	c := newCodec(t, 2, 1, 16, erasure.WithHedgeDelay(0))
	data := payload(50)
	files := encode(t, c, data)
	readers := readersFor(c, files, nil)
	defer closeAll(readers)
	failing := erasuremock.NewShardReader(t)
	failing.EXPECT().ReadAt(mock.Anything, int64(0)).Return(0, fmt.Errorf("%w: gone", domain.ErrDiskNotFound)).Once()
	failing.EXPECT().Close().Return(nil).Maybe()
	readers[1] = failing
	got, res, err := decode(c, readers, 0, int64(len(data)), int64(len(data)))
	require.NoError(t, err)
	assert.Equal(t, data, got)
	assert.ErrorIs(t, res.ShardErrs[1], domain.ErrDiskNotFound)
}

func TestDecodeHonorsContext(t *testing.T) {
	c := newCodec(t, 2, 1, 16, erasure.WithHedgeDelay(0))
	data := payload(40)
	files := encode(t, c, data)
	readers := readersFor(c, files, nil)
	defer closeAll(readers)
	ctx, cancel := context.WithCancel(context.Background())
	blocked := erasuremock.NewShardReader(t)
	blocked.EXPECT().ReadAt(mock.Anything, mock.Anything).RunAndReturn(func([]byte, int64) (int, error) {
		cancel()
		<-ctx.Done()
		return 0, ctx.Err()
	}).Maybe()
	blocked.EXPECT().Close().Return(nil).Maybe()
	readers[0] = blocked
	_, err := c.Decode(ctx, io.Discard, readers, 0, int64(len(data)), int64(len(data)))
	assert.ErrorIs(t, err, context.Canceled)
}
