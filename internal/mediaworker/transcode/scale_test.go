package transcode

import (
	"image"
	"math"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/require"
)

// referenceResize is the first implementation of the scaler (weights
// computed per call, horizontal pass first): the Scaler, which runs the
// vertical pass first, must give the same pixels within the rounding (1).
func referenceResize(src []byte, sw, sh, sstride int, dst []byte, dw, dh, dstride int) {
	weights := func(src, dst int) [][][2]int64 {
		out := make([][][2]int64, dst)
		scale := float64(src) / float64(dst)
		for i := range dst {
			var cs [][2]int64
			if scale > 1 {
				lo, hi := float64(i)*scale, float64(i+1)*scale
				for j := int(lo); j < int(math.Ceil(hi)) && j < src; j++ {
					if w := math.Min(hi, float64(j+1)) - math.Max(lo, float64(j)); w > 0 {
						cs = append(cs, [2]int64{int64(j), int64(int32(w / scale * 65536))})
					}
				}
			} else {
				c := (float64(i)+0.5)*scale - 0.5
				j := int(math.Floor(c))
				f := c - float64(j)
				cs = [][2]int64{{int64(clampIdx(j, src)), int64(int32((1 - f) * 65536))}, {int64(clampIdx(j+1, src)), int64(int32(f * 65536))}}
			}
			var sum int64
			best := 0
			for k, c := range cs {
				sum += c[1]
				if c[1] > cs[best][1] {
					best = k
				}
			}
			cs[best][1] += 65536 - sum
			out[i] = cs
		}
		return out
	}
	hw, vw := weights(sw, dw), weights(sh, dh)
	tmp := make([]int32, dw*sh)
	for y := range sh {
		for x, cs := range hw {
			var acc int32
			for _, c := range cs {
				acc += int32(src[y*sstride+int(c[0])]) * int32(c[1]) >> 8
			}
			tmp[y*dw+x] = acc
		}
	}
	for y, cs := range vw {
		for x := range dw {
			var acc int64
			for _, c := range cs {
				acc += int64(tmp[int(c[0])*dw+x]) * c[1]
			}
			dst[y*dstride+x] = uint8(max(0, min(255, (acc+1<<23)>>24)))
		}
	}
}

func TestScalerMatchesReference(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	var s Scaler
	for _, c := range [][4]int{
		{1920, 1080, 1280, 720}, {1920, 1080, 426, 240}, {640, 360, 426, 240},
		{640, 360, 160, 90}, {320, 180, 640, 360}, {1280, 720, 854, 480}, {642, 362, 642, 362},
	} {
		src := image.NewYCbCr(image.Rect(0, 0, c[0], c[1]), image.YCbCrSubsampleRatio420)
		for i := range src.Y {
			src.Y[i] = byte(rng.IntN(256))
		}
		for i := range src.Cb {
			src.Cb[i], src.Cr[i] = byte(rng.IntN(256)), byte(rng.IntN(256))
		}
		w, h := c[2], c[3]
		want := image.NewYCbCr(image.Rect(0, 0, w, h), image.YCbCrSubsampleRatio420)
		if w == c[0] && h == c[1] {
			copy(want.Y, src.Y)
			copy(want.Cb, src.Cb)
			copy(want.Cr, src.Cr)
		} else {
			referenceResize(src.Y, c[0], c[1], src.YStride, want.Y, w, h, want.YStride)
			referenceResize(src.Cb, c[0]/2, c[1]/2, src.CStride, want.Cb, w/2, h/2, want.CStride)
			referenceResize(src.Cr, c[0]/2, c[1]/2, src.CStride, want.Cr, w/2, h/2, want.CStride)
		}
		for range 2 { // the second call reuses the weights and buffers
			got := s.Scale(src, w, h)
			requireClose(t, want.Y, got.Y, c)
			requireClose(t, want.Cb, got.Cb, c)
			requireClose(t, want.Cr, got.Cr, c)
		}
		requireClose(t, want.Y, Scale(src, w, h).Y, c)
	}
}

// requireClose checks that two planes differ by at most 1 per sample.
func requireClose(t *testing.T, want, got []byte, c [4]int) {
	t.Helper()
	require.Len(t, got, len(want))
	for i := range want {
		if d := int(want[i]) - int(got[i]); d < -1 || d > 1 {
			t.Fatalf("%v: sample %d is %d, want %d", c, i, got[i], want[i])
		}
	}
}
