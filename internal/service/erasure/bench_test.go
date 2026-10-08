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
