package erasure_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/service/erasure"
)

// newCodec builds a codec of d+p shards and block size bs.
func newCodec(t testing.TB, d, p int, bs int64, opts ...erasure.Option) erasure.Codec {
	t.Helper()
	opts = append([]erasure.Option{erasure.WithShards(d, p), erasure.WithBlockSize(bs), erasure.WithDeploymentID("deployment-test")}, opts...)
	c, err := erasure.New(erasure.Deps{Logger: slog.New(slog.DiscardHandler)}, opts...)
	require.NoError(t, err)
	return c
}

func payload(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/251)
	}
	return b
}

// encode erasure-codes data into one in-memory shard file per shard.
func encode(t testing.TB, c erasure.Codec, data []byte) [][]byte {
	t.Helper()
	n := c.DataShards() + c.ParityShards()
	bufs := make([]*bytes.Buffer, n)
	writers := make([]io.Writer, n)
	for i := range writers {
		bufs[i] = &bytes.Buffer{}
		writers[i] = c.BitrotWriter(context.Background(), bufs[i])
	}
	total, err := c.Encode(context.Background(), bytes.NewReader(data), writers, n)
	require.NoError(t, err)
	require.Equal(t, int64(len(data)), total)
	files := make([][]byte, n)
	for i, b := range bufs {
		files[i] = b.Bytes()
		require.Equal(t, c.BitrotFileSize(int64(len(data))), int64(len(files[i])), "shard file %d", i)
	}
	return files
}

// memOpen opens a shard file held in memory.
func memOpen(file []byte) erasure.StreamFunc {
	return func(_ context.Context, offset int64) (io.ReadCloser, error) {
		if offset > int64(len(file)) {
			offset = int64(len(file))
		}
		return io.NopCloser(bytes.NewReader(file[offset:])), nil
	}
}

// readersFor returns bitrot readers over files; missing shards are nil.
func readersFor(c erasure.Codec, files [][]byte, missing map[int]bool) []erasure.ShardReader {
	readers := make([]erasure.ShardReader, len(files))
	for i, f := range files {
		if missing[i] {
			continue
		}
		readers[i] = c.BitrotReader(context.Background(), memOpen(f))
	}
	return readers
}

func closeAll(readers []erasure.ShardReader) {
	for _, r := range readers {
		if r != nil {
			_ = r.Close()
		}
	}
}

// decode decodes [offset, offset+length) of a part of total bytes.
func decode(c erasure.Codec, readers []erasure.ShardReader, offset, length, total int64) ([]byte, erasure.DecodeResult, error) {
	var out bytes.Buffer
	res, err := c.Decode(context.Background(), &out, readers, offset, length, total)
	return out.Bytes(), res, err
}
