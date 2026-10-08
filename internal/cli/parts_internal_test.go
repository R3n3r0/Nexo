package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func drain(t *testing.T, src partSource) []part {
	t.Helper()
	pool := newBufferPool(2)
	var out []part
	for {
		p, ok, err := src.next(pool)
		require.NoError(t, err)
		if !ok {
			return out
		}
		out = append(out, p)
	}
}

func TestPartSources(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 25)
	sections := drain(t, &sectionParts{ra: bytes.NewReader(data), size: 25, partSize: 10, parts: 3})
	require.Len(t, sections, 3)
	assert.Equal(t, []int64{10, 10, 5}, []int64{sections[0].size, sections[1].size, sections[2].size})
	assert.Equal(t, int64(20), sections[2].offset)

	ranges := drain(t, &rangeParts{size: 25, partSize: 10, parts: 3})
	require.Len(t, ranges, 3)
	assert.Nil(t, ranges[0].body)

	known := drain(t, &streamParts{r: bytes.NewReader(data), size: 25, partSize: 10})
	require.Len(t, known, 3)
	last, err := io.ReadAll(known[2].body)
	require.NoError(t, err)
	assert.Len(t, last, 5)

	_, _, err = (&streamParts{r: strings.NewReader("short"), size: 25, partSize: 10}).next(newBufferPool(1))
	assert.ErrorIs(t, err, io.ErrUnexpectedEOF)

	unknown := drain(t, &streamParts{r: bytes.NewReader(data), size: -1, partSize: MinPartSize, first: []byte("head")})
	require.Len(t, unknown, 2)
	assert.Equal(t, int64(4), unknown[0].size)
	assert.Equal(t, int64(25), unknown[1].size)
	assert.Equal(t, int32(2), unknown[1].number)

	empty := drain(t, &streamParts{r: strings.NewReader(""), size: -1, partSize: MinPartSize, first: []byte("only")})
	require.Len(t, empty, 1, "a stream that ends with its first part")

	pool := newBufferPool(1)
	b := pool.get(8)
	pool.put(b)
	assert.Len(t, pool.get(4), 4, "a recycled buffer is resliced")
	pool.put(nil)
}

func TestRunParts(t *testing.T) {
	ctx := context.Background()
	data := bytes.Repeat([]byte("y"), 100)
	var calls atomic.Int32
	var progress atomic.Int64
	upload := func(_ context.Context, p part) (types.CompletedPart, error) {
		calls.Add(1)
		n, err := io.Copy(io.Discard, p.body)
		if err != nil || n != p.size {
			return types.CompletedPart{}, errors.New("short part")
		}
		return types.CompletedPart{PartNumber: aws.Int32(p.number), ETag: aws.String("e")}, nil
	}
	parts, total, err := runParts(ctx, &sectionParts{ra: bytes.NewReader(data), size: 100, partSize: 10, parts: 10}, 3,
		func(n int64) { progress.Add(n) }, upload)
	require.NoError(t, err)
	require.Len(t, parts, 10)
	assert.Equal(t, int64(100), total)
	assert.Equal(t, int64(100), progress.Load())
	for i, p := range parts {
		assert.Equal(t, int32(i+1), aws.ToInt32(p.PartNumber), "sorted by number")
	}

	// The first failure stops the other workers.
	boom := errors.New("boom")
	calls.Store(0)
	failing := func(ctx context.Context, p part) (types.CompletedPart, error) {
		calls.Add(1)
		if p.number == 2 {
			return types.CompletedPart{}, boom
		}
		select {
		case <-ctx.Done():
			return types.CompletedPart{}, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
		return types.CompletedPart{PartNumber: aws.Int32(p.number)}, nil
	}
	_, _, err = runParts(ctx, &rangeParts{size: 1000, partSize: 10, parts: 100}, 2, nil, failing)
	assert.ErrorIs(t, err, boom)
	assert.Less(t, calls.Load(), int32(100), "the remaining parts are not uploaded")

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, _, err = runParts(canceled, &rangeParts{size: 100, partSize: 10, parts: 10}, 2, nil, upload)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestRateLimiter(t *testing.T) {
	l := &rateLimiter{rate: 1 << 20}
	start := time.Now()
	for range 4 {
		require.NoError(t, l.wait(context.Background(), 64<<10))
	}
	assert.GreaterOrEqual(t, time.Since(start), 200*time.Millisecond, "256 KiB at 1 MiB/s")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	slow := &rateLimiter{rate: 1}
	assert.ErrorIs(t, slow.wait(ctx, 100), context.Canceled)
}
