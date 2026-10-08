package erasure

import (
	"context"
	"fmt"
	"io"
	"time"
)

// Decode implements Codec.
func (c *codec) Decode(ctx context.Context, dst io.Writer, readers []ShardReader, offset, length, totalLength int64) (DecodeResult, error) {
	res := DecodeResult{ShardErrs: make([]error, c.shards())}
	if len(readers) != c.shards() {
		return res, fmt.Errorf("%w: %d readers, want %d", ErrInvalidLayout, len(readers), c.shards())
	}
	if offset < 0 || length < 0 || offset+length > totalLength {
		return res, fmt.Errorf("%w: range [%d, %d) outside a part of %d bytes", ErrInvalidLayout, offset, offset+length, totalLength)
	}
	if length == 0 {
		return res, nil
	}
	pr := newParallelReader(ctx, c, readers, res.ShardErrs)
	first, last := offset/c.blockSize, (offset+length-1)/c.blockSize
	for b := first; b <= last; b++ {
		blockLen, shardLen := c.blockGeometry(b, totalLength)
		shards, err := pr.readBlock(b*c.shardSize, shardLen)
		if err != nil {
			return res, fmt.Errorf("erasure: decode block %d: %w", b, err)
		}
		if !pr.haveData(shards) {
			if err := c.Reconstruct(ctx, shards, true); err != nil {
				return res, fmt.Errorf("erasure: decode block %d: %w", b, err)
			}
		}
		from, to := int64(0), blockLen
		if b == first {
			from = offset - b*c.blockSize
		}
		if b == last {
			to = offset + length - b*c.blockSize
		}
		n, err := writeData(dst, shards[:c.data], shardLen, from, to)
		res.Written += n
		if err != nil {
			return res, err
		}
	}
	return res, nil
}

// writeData writes bytes [from, to) of a block held by the data shards.
func writeData(dst io.Writer, data [][]byte, shardLen, from, to int64) (int64, error) {
	var written int64
	for i, shard := range data {
		lo, hi := int64(i)*shardLen, int64(i+1)*shardLen
		if hi <= from || lo >= to {
			continue
		}
		s, e := max(from, lo)-lo, min(to, hi)-lo
		n, err := dst.Write(shard[s:e])
		written += int64(n)
		if err != nil {
			return written, err
		}
	}
	return written, nil
}

// parallelReader reads the shard blocks needed to decode a block: D shards
// in preference order (data shards first), with spares added when a shard
// fails or is slower than the hedge delay. A shard that failed, or whose
// read was still running when the block was complete, is set aside for
// the rest of the decoding: its buffer still belongs to that read.
type parallelReader struct {
	ctx     context.Context
	c       *codec
	readers []ShardReader
	errs    []error
	// bufs are the per-shard read buffers, recon the buffers of the
	// shards rebuilt by the reconstruction.
	bufs  [][]byte
	recon [][]byte
	// dropped marks the shards set aside.
	dropped []bool
}

// shardRead is the outcome of one shard block read.
type shardRead struct {
	idx int
	err error
}

func newParallelReader(ctx context.Context, c *codec, readers []ShardReader, errs []error) *parallelReader {
	n := c.shards()
	return &parallelReader{
		ctx: ctx, c: c, readers: readers, errs: errs,
		bufs: make([][]byte, n), recon: make([][]byte, n), dropped: make([]bool, n),
	}
}

// next returns the first usable shard (data shards first) that is neither
// set aside nor already launched, or -1.
func (p *parallelReader) next(launched []bool) int {
	for i, r := range p.readers {
		if r != nil && !p.dropped[i] && !launched[i] {
			return i
		}
	}
	return -1
}

// haveData reports whether every data shard of the block was read.
func (p *parallelReader) haveData(shards [][]byte) bool {
	for i := 0; i < p.c.data; i++ {
		if len(shards[i]) == 0 {
			return false
		}
	}
	return true
}

// readBlock reads the shard blocks at shard offset off, of shardLen bytes,
// until D of them are available. The returned slice has one entry per
// shard: the shards read, and empty buffers (with capacity) for the others.
func (p *parallelReader) readBlock(off, shardLen int64) ([][]byte, error) {
	n := p.c.shards()
	results := make(chan shardRead, n)
	launched := make([]bool, n)
	done := make([]bool, n)
	inflight, got := 0, 0
	launch := func() bool {
		i := p.next(launched)
		if i < 0 {
			return false
		}
		if p.bufs[i] == nil {
			p.bufs[i] = make([]byte, p.c.shardSize)
		}
		launched[i] = true
		inflight++
		buf, r := p.bufs[i][:shardLen], p.readers[i]
		go func() {
			nr, err := r.ReadAt(buf, off)
			if err == nil && int64(nr) != shardLen {
				err = fmt.Errorf("%w: read %d of %d bytes", ErrShardTruncated, nr, shardLen)
			}
			results <- shardRead{idx: i, err: err}
		}()
		return true
	}
	for k := 0; k < p.c.data; k++ {
		if !launch() {
			break
		}
	}
	var hedge <-chan time.Time
	if p.c.hedgeDelay > 0 {
		t := time.NewTicker(p.c.hedgeDelay)
		defer t.Stop()
		hedge = t.C
	}
	for got < p.c.data {
		if inflight == 0 {
			return nil, fmt.Errorf("%w: %d of %d shards readable, %d needed", ErrTooFewShards, got, n, p.c.data)
		}
		select {
		case r := <-results:
			inflight--
			if r.err != nil {
				p.errs[r.idx] = r.err
				p.dropped[r.idx] = true
				p.c.logger.DebugContext(p.ctx, "erasure: shard set aside", "shard", r.idx, "offset", off, "err", r.err)
				launch()
				continue
			}
			done[r.idx] = true
			got++
		case <-hedge:
			launch()
		case <-p.ctx.Done():
			return nil, p.ctx.Err()
		}
	}
	shards := make([][]byte, n)
	for i := range shards {
		switch {
		case done[i]:
			shards[i] = p.bufs[i][:shardLen]
		case launched[i]:
			// Still running: the shard lags behind, its buffer is busy.
			p.dropped[i] = true
		}
		if shards[i] == nil {
			if p.recon[i] == nil {
				p.recon[i] = make([]byte, 0, p.c.shardSize)
			}
			shards[i] = p.recon[i][:0]
		}
	}
	return shards, nil
}
