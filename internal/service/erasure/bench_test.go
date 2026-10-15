package erasure_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"

	"github.com/r3n3r0/nexo/internal/service/erasure"
)

// benchLayouts are the layouts measured by the benchmarks (go test -bench).
func benchLayouts() [][2]int { return [][2]int{{2, 2}, {4, 2}, {6, 2}, {12, 4}} }

const benchSize = 8 << 20

func BenchmarkEncode(b *testing.B) {
	data := payload(benchSize)
	for _, l := range benchLayouts() {
		b.Run(fmt.Sprintf("%d+%d", l[0], l[1]), func(b *testing.B) {
			c := newCodec(b, l[0], l[1], erasure.DefaultBlockSize)
			n := l[0] + l[1]
			b.SetBytes(benchSize)
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				writers := make([]io.Writer, n)
				for j := range writers {
					writers[j] = c.BitrotWriter(context.Background(), io.Discard)
				}
				if _, err := c.Encode(context.Background(), bytes.NewReader(data), writers, n); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkDecode(b *testing.B) {
	data := payload(benchSize)
	for _, l := range benchLayouts() {
		for _, missing := range []int{0, l[1]} {
			b.Run(fmt.Sprintf("%d+%d/missing=%d", l[0], l[1], missing), func(b *testing.B) {
				c := newCodec(b, l[0], l[1], erasure.DefaultBlockSize)
				files := encode(b, c, data)
				gone := map[int]bool{}
				for i := 0; i < missing; i++ {
					gone[i] = true // data shards: the worst case
				}
				b.SetBytes(benchSize)
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					readers := readersFor(c, files, gone)
					if _, err := c.Decode(context.Background(), io.Discard, readers, 0, benchSize, benchSize); err != nil {
						b.Fatal(err)
					}
					closeAll(readers)
				}
			})
		}
	}
}

// smallSizes are object sizes far below a block (the mixed workloads of
// docs/bench: 1 KiB to 1 MiB).
func smallSizes() []int { return []int{4 << 10, 64 << 10, 512 << 10} }

// BenchmarkEncodeSmall measures the encoding of small objects, where the
// buffers of a whole block dominate (docs/bench).
func BenchmarkEncodeSmall(b *testing.B) {
	c := newCodec(b, 2, 2, erasure.DefaultBlockSize)
	for _, size := range smallSizes() {
		data := payload(size)
		b.Run(fmt.Sprintf("%dKiB", size>>10), func(b *testing.B) {
			b.SetBytes(int64(size))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				writers := make([]io.Writer, 4)
				for j := range writers {
					writers[j] = c.BitrotWriter(context.Background(), io.Discard)
				}
				if _, err := c.Encode(context.Background(), bytes.NewReader(data), writers, 4); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkDecodeSmall measures the decoding of small objects, with every
// shard and with the data shards missing (rebuilt).
func BenchmarkDecodeSmall(b *testing.B) {
	c := newCodec(b, 2, 2, erasure.DefaultBlockSize)
	for _, size := range smallSizes() {
		data := payload(size)
		files := encode(b, c, data)
		for _, missing := range []int{0, 2} {
			b.Run(fmt.Sprintf("%dKiB/missing=%d", size>>10, missing), func(b *testing.B) {
				gone := map[int]bool{}
				for i := 0; i < missing; i++ {
					gone[i] = true
				}
				b.SetBytes(int64(size))
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					readers := readersFor(c, files, gone)
					if _, err := c.Decode(context.Background(), io.Discard, readers, 0, int64(size), int64(size)); err != nil {
						b.Fatal(err)
					}
					closeAll(readers)
				}
			})
		}
	}
}
