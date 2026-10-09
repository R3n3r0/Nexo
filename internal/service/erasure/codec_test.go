package erasure_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/erasure"
)

// TestEncodeDecodeAllLayouts is TS-U-03: every D/P split of 2..16 shards,
// objects of 0 B, 1 B, block size ±1 and several blocks with a partial
// last one, decoded with 0..P shards missing; P+1 missing shards fail.
func TestEncodeDecodeAllLayouts(t *testing.T) {
	const bs = 96
	sizes := []int{0, 1, bs - 1, bs, bs + 1, 3*bs + 17}
	rng := rand.New(rand.NewPCG(1, 2))
	for n := 2; n <= 16; n++ {
		for p := 1; p < n; p++ {
			d := n - p
			t.Run(fmt.Sprintf("%d+%d", d, p), func(t *testing.T) {
				c := newCodec(t, d, p, bs)
				for _, size := range sizes {
					data := payload(size)
					files := encode(t, c, data)
					for missing := 0; missing <= p+1; missing++ {
						gone := map[int]bool{}
						for _, i := range rng.Perm(n)[:missing] {
							gone[i] = true
						}
						readers := readersFor(c, files, gone)
						got, res, err := decode(c, readers, 0, int64(size), int64(size))
						closeAll(readers)
						if missing > p && size > 0 {
							require.ErrorIs(t, err, erasure.ErrTooFewShards, "size %d, %d missing", size, missing)
							assert.ErrorIs(t, err, domain.ErrReadQuorum)
							continue
						}
						require.NoError(t, err, "size %d, %d missing", size, missing)
						assert.Equal(t, int64(size), res.Written)
						if !bytes.Equal(data, got) {
							t.Fatalf("size %d, missing %v: decoded content differs", size, gone)
						}
					}
				}
			})
		}
	}
}

// TestEncodeSourceFailure (anti-truncation): a source that fails, even
// with io.ErrUnexpectedEOF (a body cut short), fails the encoding with its
// error, wherever it fails (inside a block or at a block boundary): a
// truncated stream is never encoded as a complete one. The end of the
// source (io.EOF), however it is delivered, ends the data.
func TestEncodeSourceFailure(t *testing.T) {
	const bs = 64
	c := newCodec(t, 2, 2, bs)
	writers := func() []io.Writer {
		w := make([]io.Writer, 4)
		for i := range w {
			w[i] = c.BitrotWriter(context.Background(), &bytes.Buffer{})
		}
		return w
	}
	cut := errors.New("connection reset")
	for _, size := range []int{0, 1, bs - 1, bs, bs + 1, 3 * bs} {
		for _, srcErr := range []error{io.ErrUnexpectedEOF, cut} {
			src := io.MultiReader(bytes.NewReader(payload(size)), iotest.ErrReader(srcErr))
			_, err := c.Encode(context.Background(), src, writers(), 4)
			require.ErrorIs(t, err, srcErr, "size %d", size)
		}
		for name, src := range map[string]io.Reader{
			"one byte at a time": iotest.OneByteReader(bytes.NewReader(payload(size))),
			"data with EOF":      iotest.DataErrReader(bytes.NewReader(payload(size))),
		} {
			total, err := c.Encode(context.Background(), src, writers(), 4)
			require.NoError(t, err, "%s, size %d", name, size)
			assert.Equal(t, int64(size), total, "%s, size %d", name, size)
		}
	}
}

func TestDecodeRanges(t *testing.T) {
	const bs = 64
	c := newCodec(t, 3, 2, bs)
	data := payload(5*bs + 9)
	files := encode(t, c, data)
	total := int64(len(data))
	tests := []struct {
		name           string
		offset, length int64
		missing        map[int]bool
	}{
		{name: "whole", offset: 0, length: total},
		{name: "first byte", offset: 0, length: 1},
		{name: "last byte", offset: total - 1, length: 1},
		{name: "inside one block", offset: bs + 3, length: 10},
		{name: "across blocks", offset: bs - 5, length: 2*bs + 10},
		{name: "block boundary", offset: 2 * bs, length: bs},
		{name: "tail with missing data shards", offset: 4*bs + 1, length: total - 4*bs - 1, missing: map[int]bool{0: true, 2: true}},
		{name: "empty", offset: 7, length: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			readers := readersFor(c, files, tt.missing)
			defer closeAll(readers)
			got, res, err := decode(c, readers, tt.offset, tt.length, total)
			require.NoError(t, err)
			assert.Equal(t, tt.length, res.Written)
			assert.Equal(t, string(data[tt.offset:tt.offset+tt.length]), string(got))
		})
	}

	readers := readersFor(c, files, nil)
	defer closeAll(readers)
	_, _, err := decode(c, readers, total-1, 2, total)
	assert.ErrorIs(t, err, erasure.ErrInvalidLayout, "range beyond the part")
	_, err = c.Decode(context.Background(), io.Discard, readers[:2], 0, 1, total)
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

func TestLayoutValidation(t *testing.T) {
	tests := []struct {
		name string
		d, p int
		bs   int64
		ok   bool
	}{
		{name: "minimal", d: 1, p: 0, bs: 1, ok: true},
		{name: "no data shard", d: 0, p: 2, bs: 1024},
		{name: "negative parity", d: 2, p: -1, bs: 1024},
		{name: "too many shards", d: 200, p: 57, bs: 1024},
		{name: "max shards", d: 128, p: 128, bs: 1 << 16, ok: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := erasure.New(erasure.Deps{}, erasure.WithShards(tt.d, tt.p), erasure.WithBlockSize(tt.bs))
			if !tt.ok {
				assert.ErrorIs(t, err, erasure.ErrInvalidLayout)
				assert.ErrorIs(t, err, domain.ErrInvalidArgument)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.d, c.DataShards())
			assert.Equal(t, tt.p, c.ParityShards())
		})
	}
}

func TestSizes(t *testing.T) {
	c := newCodec(t, 4, 2, 1000)
	assert.Equal(t, int64(250), c.ShardSize())
	tests := []struct {
		total, shard, file int64
	}{
		{total: 0, shard: 0, file: 0},
		{total: 1, shard: 1, file: 33},
		{total: 999, shard: 250, file: 282},
		{total: 1000, shard: 250, file: 282},
		{total: 1001, shard: 251, file: 315},
		{total: 2500, shard: 625, file: 625 + 3*32},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.shard, c.ShardFileSize(tt.total), "shard file size of %d", tt.total)
		assert.Equal(t, tt.file, c.BitrotFileSize(tt.total), "bitrot file size of %d", tt.total)
	}
}

func TestEncodeBlockAndReconstruct(t *testing.T) {
	ctx := context.Background()
	c := newCodec(t, 4, 2, 64)
	block := payload(61)
	shards, err := c.EncodeBlock(ctx, block)
	require.NoError(t, err)
	require.Len(t, shards, 6)
	want := make([][]byte, len(shards))
	for i := range shards {
		want[i] = bytes.Clone(shards[i])
	}
	shards[1], shards[4] = nil, nil
	require.NoError(t, c.Reconstruct(ctx, shards, false))
	assert.Equal(t, want, shards)

	shards[0], shards[2] = nil, nil
	require.NoError(t, c.Reconstruct(ctx, shards, true))
	assert.Equal(t, want[:4], shards[:4])

	shards[0], shards[1], shards[2] = nil, nil, nil
	assert.ErrorIs(t, c.Reconstruct(ctx, shards, true), erasure.ErrTooFewShards)
	assert.ErrorIs(t, c.Reconstruct(ctx, shards[:3], true), erasure.ErrInvalidLayout)

	_, err = c.EncodeBlock(ctx, nil)
	assert.ErrorIs(t, err, erasure.ErrInvalidLayout)
	_, err = c.EncodeBlock(ctx, payload(65))
	assert.ErrorIs(t, err, erasure.ErrInvalidLayout)
}

// failingWriter fails after accepting limit writes.
type failingWriter struct {
	limit int
	n     int
}

func (f *failingWriter) Write(p []byte) (int, error) {
	if f.n >= f.limit {
		return 0, fmt.Errorf("%w: injected", domain.ErrFaultyDisk)
	}
	f.n++
	return len(p), nil
}

func TestEncodeWriteQuorum(t *testing.T) {
	ctx := context.Background()
	c := newCodec(t, 2, 2, 32)
	data := payload(5 * 32)
	tests := []struct {
		name    string
		failing int
		quorum  int
		wantErr error
	}{
		{name: "all writers healthy", quorum: 3},
		{name: "one failure tolerated", failing: 1, quorum: 3},
		{name: "two failures lose the quorum", failing: 2, quorum: 3, wantErr: erasure.ErrWriteQuorum},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writers := make([]io.Writer, 4)
			for i := range writers {
				if i < tt.failing {
					writers[i] = &failingWriter{limit: 2}
				} else {
					writers[i] = io.Discard
				}
			}
			n, err := c.Encode(ctx, bytes.NewReader(data), writers, tt.quorum)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				assert.ErrorIs(t, err, domain.ErrWriteQuorum)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, int64(len(data)), n)
			for i := 0; i < tt.failing; i++ {
				assert.Nil(t, writers[i], "a failed writer is set aside")
			}
		})
	}

	// Too few writers from the start, bad arguments, source errors.
	_, err := c.Encode(ctx, bytes.NewReader(data), []io.Writer{io.Discard, nil, nil, io.Discard}, 3)
	assert.ErrorIs(t, err, erasure.ErrWriteQuorum)
	_, err = c.Encode(ctx, bytes.NewReader(data), []io.Writer{io.Discard}, 1)
	assert.ErrorIs(t, err, erasure.ErrInvalidLayout)
	_, err = c.Encode(ctx, bytes.NewReader(data), make([]io.Writer, 4), 5)
	assert.ErrorIs(t, err, erasure.ErrInvalidLayout)
	errSrc := errors.New("client went away")
	_, err = c.Encode(ctx, io.MultiReader(bytes.NewReader(data[:40]), &errReader{err: errSrc}), []io.Writer{io.Discard, io.Discard, io.Discard, io.Discard}, 3)
	assert.ErrorIs(t, err, errSrc)
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	_, err = c.Encode(cctx, bytes.NewReader(data), []io.Writer{io.Discard, io.Discard, io.Discard, io.Discard}, 3)
	assert.ErrorIs(t, err, context.Canceled)
}

type errReader struct{ err error }

func (e *errReader) Read([]byte) (int, error) { return 0, e.err }

func TestHeal(t *testing.T) {
	ctx := context.Background()
	c := newCodec(t, 3, 2, 50)
	data := payload(4*50 + 13)
	files := encode(t, c, data)
	total := int64(len(data))

	// Shards 1 and 4 are lost: rebuild them from the others.
	lost := map[int]bool{1: true, 4: true}
	readers := readersFor(c, files, lost)
	defer closeAll(readers)
	rebuilt := map[int]*bytes.Buffer{}
	writers := make([]io.Writer, len(files))
	for i := range lost {
		rebuilt[i] = &bytes.Buffer{}
		writers[i] = c.BitrotWriter(ctx, rebuilt[i])
	}
	_, err := c.Heal(ctx, writers, readers, total)
	require.NoError(t, err)
	for i, b := range rebuilt {
		assert.Equal(t, files[i], b.Bytes(), "shard %d rebuilt byte for byte", i)
	}

	_, err = c.Heal(ctx, make([]io.Writer, len(files)), readers, total)
	assert.ErrorIs(t, err, erasure.ErrWriteQuorum, "nothing to heal")
	tooMany := readersFor(c, files, map[int]bool{0: true, 1: true, 2: true})
	defer closeAll(tooMany)
	_, err = c.Heal(ctx, []io.Writer{&bytes.Buffer{}, nil, nil, nil, nil}, tooMany, total)
	assert.ErrorIs(t, err, erasure.ErrTooFewShards)
}

func TestFactoryCachesCodecs(t *testing.T) {
	ctx := context.Background()
	f := erasure.NewFactory(erasure.Deps{}, erasure.WithBlockSize(128), erasure.WithDeploymentID("dep"))
	a, err := f.Codec(ctx, erasure.Layout{DataShards: 2, ParityShards: 2})
	require.NoError(t, err)
	b, err := f.Codec(ctx, erasure.Layout{DataShards: 2, ParityShards: 2, BlockSize: 128})
	require.NoError(t, err)
	assert.Same(t, a, b)
	assert.Equal(t, int64(128), a.BlockSize())
	other, err := f.Codec(ctx, erasure.Layout{DataShards: 3, ParityShards: 1})
	require.NoError(t, err)
	assert.NotSame(t, a, other)
	_, err = f.Codec(ctx, erasure.Layout{DataShards: 0, ParityShards: 1})
	assert.ErrorIs(t, err, erasure.ErrInvalidLayout)

	// The bitrot key derives from the deployment id: another deployment
	// does not accept the shards.
	same := erasure.NewFactory(erasure.Deps{}, erasure.WithBlockSize(128), erasure.WithDeploymentID("dep"))
	foreign := erasure.NewFactory(erasure.Deps{}, erasure.WithBlockSize(128), erasure.WithDeploymentID("other"))
	c1, _ := same.Codec(ctx, erasure.Layout{DataShards: 2, ParityShards: 2})
	c2, _ := foreign.Codec(ctx, erasure.Layout{DataShards: 2, ParityShards: 2})
	assert.Equal(t, a.BitrotSum([]byte("x")), c1.BitrotSum([]byte("x")))
	assert.NotEqual(t, a.BitrotSum([]byte("x")), c2.BitrotSum([]byte("x")))
	assert.Equal(t, erasure.DeriveBitrotKey("dep"), erasure.DeriveBitrotKey("dep"))
	assert.Len(t, erasure.DeriveBitrotKey(""), 32)
}
