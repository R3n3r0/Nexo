package resample

import (
	"math"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTaps(t *testing.T) {
	for _, c := range [][2]int{{1920, 640}, {360, 240}, {100, 100}, {90, 160}, {7, 3}} {
		tp := newTaps(c[0], c[1])
		require.Len(t, tp.off, c[1]+1)
		for i := range c[1] {
			var sum int32
			for k := tp.off[i]; k < tp.off[i+1]; k++ {
				sum += tp.wt[k]
				assert.True(t, tp.idx[k] >= 0 && int(tp.idx[k]) < c[0])
			}
			assert.Equal(t, int32(65536), sum, "%v output %d", c, i)
		}
	}
}

// reference resamples one channel of interleaved samples in floating
// point: the box (area) filter when reducing, linear when enlarging.
func reference(src []byte, sw, sh, ch, c, dw, dh int) []float64 {
	weights := func(src, dst int) [][]struct {
		j int
		w float64
	} {
		out := make([][]struct {
			j int
			w float64
		}, dst)
		scale := float64(src) / float64(dst)
		for i := range dst {
			if scale > 1 {
				lo, hi := float64(i)*scale, float64(i+1)*scale
				for j := int(lo); j < int(math.Ceil(hi)) && j < src; j++ {
					if w := math.Min(hi, float64(j+1)) - math.Max(lo, float64(j)); w > 0 {
						out[i] = append(out[i], struct {
							j int
							w float64
						}{j, w / scale})
					}
				}
				continue
			}
			x := (float64(i)+0.5)*scale - 0.5
			j := int(math.Floor(x))
			f := x - float64(j)
			out[i] = append(out[i], struct {
				j int
				w float64
			}{clampIdx(j, src), 1 - f}, struct {
				j int
				w float64
			}{clampIdx(j+1, src), f})
		}
		return out
	}
	hw, vw := weights(sw, dw), weights(sh, dh)
	out := make([]float64, dw*dh)
	for y := range dh {
		for x := range dw {
			var v float64
			for _, a := range vw[y] {
				for _, b := range hw[x] {
					v += float64(src[(a.j*sw+b.j)*ch+c]) * a.w * b.w
				}
			}
			out[y*dw+x] = v
		}
	}
	return out
}

func TestScalerMatchesReference(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for _, ch := range []int{1, 4} {
		for _, c := range [][4]int{{640, 360, 213, 120}, {300, 200, 160, 107}, {97, 61, 16, 10}, {64, 48, 128, 96}, {50, 50, 50, 50}, {1200, 900, 1199, 3}} {
			sw, sh, dw, dh := c[0], c[1], c[2], c[3]
			src := make([]byte, sw*sh*ch)
			for i := range src {
				src[i] = byte(rng.IntN(256))
			}
			dst := make([]byte, dw*dh*ch)
			New(sw, sh, dw, dh, ch).Resize(src, sw*ch, dst, dw*ch)
			for k := range ch {
				want := reference(src, sw, sh, ch, k, dw, dh)
				for i, v := range want {
					got := float64(dst[i*ch+k])
					require.InDelta(t, v, got, 1.5, "%v ch %d/%d sample %d", c, k, ch, i)
				}
			}
		}
	}
}

// TestParallelBands: a resize split into bands gives the same samples as
// one band.
func TestParallelBands(t *testing.T) {
	sw, sh, dw, dh := 2000, 1500, 1280, 960
	src := make([]byte, sw*sh*4)
	for i := range src {
		src[i] = byte(i * 7 / 3)
	}
	s := New(sw, sh, dw, dh, 4)
	require.Greater(t, s.bands, 0)
	got := make([]byte, dw*dh*4)
	s.Resize(src, sw*4, got, dw*4)
	one := New(sw, sh, dw, dh, 4)
	one.bands, one.rows = 1, one.rows[:1]
	want := make([]byte, dw*dh*4)
	one.Resize(src, sw*4, want, dw*4)
	assert.Equal(t, want, got)
}

func TestFlatStaysFlat(t *testing.T) {
	src := make([]byte, 333*222*4)
	for i := range src {
		src[i] = []byte{10, 200, 255, 255}[i%4]
	}
	for _, size := range [][2]int{{160, 107}, {16, 10}, {333, 222}, {500, 333}} {
		dst := make([]byte, size[0]*size[1]*4)
		New(333, 222, size[0], size[1], 4).Resize(src, 333*4, dst, size[0]*4)
		for i, v := range dst {
			require.Equal(t, []byte{10, 200, 255, 255}[i%4], v, "%v sample %d", size, i)
		}
	}
}
