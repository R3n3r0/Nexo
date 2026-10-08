package erasure_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/r3n3r0/nexo/internal/service/erasure"
)

// FuzzBitrotReader feeds arbitrary shard files to the bitrot reader: it
// must never panic, and a block it accepts must hash to the stored value.
func FuzzBitrotReader(f *testing.F) {
	c, err := erasure.New(erasure.Deps{}, erasure.WithShards(2, 1), erasure.WithBlockSize(32), erasure.WithDeploymentID("fuzz"))
	if err != nil {
		f.Fatal(err)
	}
	var good bytes.Buffer
	w := c.BitrotWriter(context.Background(), &good)
	_, _ = w.Write(bytes.Repeat([]byte{'a'}, 16))
	_, _ = w.Write([]byte("tail"))
	f.Add(good.Bytes(), uint8(16), uint8(4))
	f.Add(good.Bytes()[:40], uint8(16), uint8(4))
	f.Add([]byte{}, uint8(1), uint8(0))
	flipped := bytes.Clone(good.Bytes())
	flipped[33] ^= 1
	f.Add(flipped, uint8(16), uint8(16))

	f.Fuzz(func(t *testing.T, file []byte, first, second uint8) {
		r := c.BitrotReader(context.Background(), memOpen(file))
		defer r.Close()
		for b, l := range []uint8{first, second} {
			n := int(l) % int(c.ShardSize()+1)
			buf := make([]byte, n)
			read, err := r.ReadAt(buf, int64(b)*c.ShardSize())
			if err != nil {
				return
			}
			if read != n {
				t.Fatalf("read %d of %d bytes without error", read, n)
			}
			if n == 0 {
				continue
			}
			start := b * (erasure.BitrotHashSize + int(c.ShardSize()))
			if !bytes.Equal(file[start:start+erasure.BitrotHashSize], c.BitrotSum(buf)) {
				t.Fatalf("block %d accepted with a wrong hash", b)
			}
		}
	})
}
