package erasure

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/klauspost/reedsolomon"
	"github.com/minio/highwayhash"
)

// On-disk identifiers of the layout recorded in xl.meta.
const (
	// Algorithm is the erasure algorithm (Reed-Solomon, Vandermonde matrix).
	Algorithm = "rs-vandermonde"
	// BitrotAlgorithm is the per-block shard hash (streaming HighwayHash-256).
	BitrotAlgorithm = "highwayhash256S"
	// BitrotHashSize is the size of a shard block hash.
	BitrotHashSize = highwayhash.Size
)

// codec is the Reed-Solomon codec of one layout.
type codec struct {
	rs         reedsolomon.Encoder
	data       int
	parity     int
	blockSize  int64
	shardSize  int64
	key        []byte
	hedgeDelay time.Duration
	logger     *slog.Logger
}

var _ Codec = (*codec)(nil)

// New returns the codec of the layout set with WithShards (D >= 1, P >= 0,
// D+P <= MaxShards) and WithBlockSize.
func New(deps Deps, opts ...Option) (Codec, error) {
	o := defaultOptions()
	for _, opt := range opts {
		opt(&o)
	}
	return newCodec(deps, o, Layout{DataShards: o.data, ParityShards: o.parity, BlockSize: o.blockSize})
}

func newCodec(deps Deps, o options, layout Layout) (*codec, error) {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	d, p, bs := layout.DataShards, layout.ParityShards, layout.BlockSize
	if bs == 0 {
		bs = o.blockSize
	}
	if d < 1 || p < 0 || d+p > MaxShards || bs < 1 {
		return nil, fmt.Errorf("%w: %d data + %d parity shards, block size %d", ErrInvalidLayout, d, p, bs)
	}
	if len(o.key) != 32 {
		return nil, fmt.Errorf("%w: the bitrot key must be 32 bytes", ErrInvalidLayout)
	}
	shardSize := ceilDiv(bs, int64(d))
	rs, err := reedsolomon.New(d, p, reedsolomon.WithAutoGoroutines(int(shardSize)))
	if err != nil {
		return nil, fmt.Errorf("%w: %d+%d: %v", ErrInvalidLayout, d, p, err)
	}
	return &codec{rs: rs, data: d, parity: p, blockSize: bs, shardSize: shardSize, key: o.key, hedgeDelay: o.hedgeDelay, logger: deps.Logger}, nil
}

func ceilDiv(a, b int64) int64 { return (a + b - 1) / b }

// DataShards implements Codec.
func (c *codec) DataShards() int { return c.data }

// ParityShards implements Codec.
func (c *codec) ParityShards() int { return c.parity }

// BlockSize implements Codec.
func (c *codec) BlockSize() int64 { return c.blockSize }

// ShardSize implements Codec.
func (c *codec) ShardSize() int64 { return c.shardSize }

func (c *codec) shards() int { return c.data + c.parity }

// ShardFileSize implements Codec.
func (c *codec) ShardFileSize(totalLength int64) int64 {
	if totalLength <= 0 {
		return 0
	}
	full := totalLength / c.blockSize
	return full*c.shardSize + ceilDiv(totalLength%c.blockSize, int64(c.data))
}

// BitrotFileSize implements Codec.
func (c *codec) BitrotFileSize(totalLength int64) int64 {
	if totalLength <= 0 {
		return 0
	}
	return c.ShardFileSize(totalLength) + ceilDiv(totalLength, c.blockSize)*BitrotHashSize
}

// BitrotSum implements Codec.
func (c *codec) BitrotSum(p []byte) []byte {
	sum := highwayhash.Sum(p, c.key)
	return sum[:]
}

// blockGeometry returns the length of block b of a part of totalLength
// bytes and the length of its shards.
func (c *codec) blockGeometry(b, totalLength int64) (blockLen, shardLen int64) {
	blockLen = min(c.blockSize, totalLength-b*c.blockSize)
	return blockLen, ceilDiv(blockLen, int64(c.data))
}

// EncodeBlock implements Codec.
func (c *codec) EncodeBlock(ctx context.Context, block []byte) ([][]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(block) == 0 || int64(len(block)) > c.blockSize {
		return nil, fmt.Errorf("%w: block of %d bytes (block size %d)", ErrInvalidLayout, len(block), c.blockSize)
	}
	per := ceilDiv(int64(len(block)), int64(c.data))
	shards := make([][]byte, c.shards())
	for i := range shards {
		shards[i] = make([]byte, per)
	}
	c.fill(shards, block, per)
	if err := c.rs.Encode(shards); err != nil {
		return nil, fmt.Errorf("erasure: encode: %w", err)
	}
	return shards, nil
}

// fill copies block into the data shards (zero padded to per bytes).
func (c *codec) fill(shards [][]byte, block []byte, per int64) {
	for i := 0; i < c.data; i++ {
		dst := shards[i][:per]
		lo := min(int64(i)*per, int64(len(block)))
		hi := min(lo+per, int64(len(block)))
		n := copy(dst, block[lo:hi])
		clear(dst[n:])
	}
}

// Reconstruct implements Codec.
func (c *codec) Reconstruct(ctx context.Context, shards [][]byte, dataOnly bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(shards) != c.shards() {
		return fmt.Errorf("%w: %d shards, want %d", ErrInvalidLayout, len(shards), c.shards())
	}
	var err error
	if dataOnly {
		err = c.rs.ReconstructData(shards)
	} else {
		err = c.rs.Reconstruct(shards)
	}
	switch {
	case err == nil:
		return nil
	case errors.Is(err, reedsolomon.ErrTooFewShards):
		return fmt.Errorf("%w: %v", ErrTooFewShards, err)
	default:
		return fmt.Errorf("%w: reconstruct: %v", ErrInvalidLayout, err)
	}
}

// Encode implements Codec.
func (c *codec) Encode(ctx context.Context, src io.Reader, writers []io.Writer, quorum int) (int64, error) {
	if len(writers) != c.shards() {
		return 0, fmt.Errorf("%w: %d writers, want %d", ErrInvalidLayout, len(writers), c.shards())
	}
	if quorum < 1 || quorum > c.shards() {
		return 0, fmt.Errorf("%w: write quorum %d out of [1, %d]", ErrInvalidLayout, quorum, c.shards())
	}
	if err := checkWriters(writers, quorum); err != nil {
		return 0, err
	}
	buf := make([]byte, c.blockSize)
	shards := make([][]byte, c.shards())
	for i := range shards {
		shards[i] = make([]byte, c.shardSize)
	}
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		// Only the end of src (a plain io.EOF) ends the data: any other
		// error, io.ErrUnexpectedEOF included (a body cut short), fails the
		// encoding, so that a truncated stream is never stored as a
		// complete one.
		n, rerr := readFull(src, buf)
		if rerr != nil && rerr != io.EOF {
			return total, rerr
		}
		if n == 0 {
			return total, nil
		}
		total += int64(n)
		per := ceilDiv(int64(n), int64(c.data))
		block := make([][]byte, len(shards))
		for i := range shards {
			block[i] = shards[i][:per]
		}
		c.fill(block, buf[:n], per)
		if err := c.rs.Encode(block); err != nil {
			return total, fmt.Errorf("erasure: encode: %w", err)
		}
		if err := writeShards(ctx, writers, block, quorum); err != nil {
			return total, err
		}
		if rerr != nil { // io.EOF: the stream ended with this block
			return total, nil
		}
	}
}

// readFull reads into buf until it is full or src returns an error. Unlike
// io.ReadFull it returns the error of src unchanged: io.EOF (with the
// bytes read before it) is the end of the stream, any other error,
// io.ErrUnexpectedEOF included, a failure of the source. io.ReadFull
// turns an end after a partial read into io.ErrUnexpectedEOF, which a
// source cut short returns too: the two must not be confused.
func readFull(src io.Reader, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := src.Read(buf[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// checkWriters fails with ErrWriteQuorum when fewer than quorum writers
// are left.
func checkWriters(writers []io.Writer, quorum int) error {
	left := 0
	for _, w := range writers {
		if w != nil {
			left++
		}
	}
	if left < quorum {
		return fmt.Errorf("%w: %d of %d shard writers left, need %d", ErrWriteQuorum, left, len(writers), quorum)
	}
	return nil
}

// writeShards writes one block's shards in parallel; failed writers are
// set to nil.
func writeShards(ctx context.Context, writers []io.Writer, shards [][]byte, quorum int) error {
	errs := make([]error, len(writers))
	var wg sync.WaitGroup
	for i, w := range writers {
		if w == nil {
			continue
		}
		wg.Add(1)
		go func(i int, w io.Writer) {
			defer wg.Done()
			n, err := w.Write(shards[i])
			if err == nil && n != len(shards[i]) {
				err = io.ErrShortWrite
			}
			errs[i] = err
		}(i, w)
	}
	wg.Wait()
	var first error
	for i, err := range errs {
		if err != nil {
			writers[i] = nil
			if first == nil {
				first = err
			}
		}
	}
	if err := checkWriters(writers, quorum); err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if first != nil {
			return fmt.Errorf("%w (last failure: %v)", err, first)
		}
		return err
	}
	return nil
}

// Heal implements Codec.
func (c *codec) Heal(ctx context.Context, writers []io.Writer, readers []ShardReader, totalLength int64) (DecodeResult, error) {
	res := DecodeResult{ShardErrs: make([]error, c.shards())}
	if len(writers) != c.shards() || len(readers) != c.shards() {
		return res, fmt.Errorf("%w: %d writers and %d readers, want %d", ErrInvalidLayout, len(writers), len(readers), c.shards())
	}
	if totalLength < 0 {
		return res, fmt.Errorf("%w: negative length %d", ErrInvalidLayout, totalLength)
	}
	if err := checkWriters(writers, 1); err != nil {
		return res, err
	}
	pr := newParallelReader(ctx, c, readers, res.ShardErrs)
	blocks := ceilDiv(totalLength, c.blockSize)
	for b := int64(0); b < blocks; b++ {
		_, shardLen := c.blockGeometry(b, totalLength)
		shards, err := pr.readBlock(b*c.shardSize, shardLen)
		if err != nil {
			return res, fmt.Errorf("erasure: heal block %d: %w", b, err)
		}
		if err := c.Reconstruct(ctx, shards, false); err != nil {
			return res, fmt.Errorf("erasure: heal block %d: %w", b, err)
		}
		if err := writeShards(ctx, writers, shards, 1); err != nil {
			return res, fmt.Errorf("erasure: heal block %d: %w", b, err)
		}
		res.Written += shardLen
	}
	return res, nil
}
