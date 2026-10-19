package transcode

import (
	"image"
	"math"
)

// contrib is the weight of a source sample in an output sample (16.16).
type contrib struct {
	idx    int
	weight int32
}

// weights returns, for each of the dst output samples, the source samples
// it is made of: the area they cover when reducing, the two neighbours
// (linear) when enlarging. The weights of an output sample sum to 1<<16.
func weights(src, dst int) [][]contrib {
	out := make([][]contrib, dst)
	scale := float64(src) / float64(dst)
	for i := range dst {
		var cs []contrib
		if scale > 1 {
			lo, hi := float64(i)*scale, float64(i+1)*scale
			for j := int(lo); j < int(math.Ceil(hi)) && j < src; j++ {
				w := math.Min(hi, float64(j+1)) - math.Max(lo, float64(j))
				if w > 0 {
					cs = append(cs, contrib{idx: j, weight: int32(w / scale * 65536)})
				}
			}
		} else {
			c := (float64(i)+0.5)*scale - 0.5
			j := int(math.Floor(c))
			f := c - float64(j)
			a, b := clampIdx(j, src), clampIdx(j+1, src)
			cs = []contrib{{idx: a, weight: int32((1 - f) * 65536)}, {idx: b, weight: int32(f * 65536)}}
		}
		// Rounding leftovers go to the heaviest sample.
		var sum int32
		best := 0
		for k, c := range cs {
			sum += c.weight
			if c.weight > cs[best].weight {
				best = k
			}
		}
		cs[best].weight += 65536 - sum
		out[i] = cs
	}
	return out
}

func clampIdx(i, n int) int { return max(0, min(i, n-1)) }

// resizePlane resamples a plane of sw x sh into one of dw x dh.
func resizePlane(src []byte, sw, sh, sstride int, dst []byte, dw, dh, dstride int) {
	hw, vw := weights(sw, dw), weights(sh, dh)
	// Horizontal pass into rows of dw, kept 16.16.
	tmp := make([]int32, dw*sh)
	for y := range sh {
		row := src[y*sstride:]
		out := tmp[y*dw:]
		for x, cs := range hw {
			var acc int32
			for _, c := range cs {
				acc += int32(row[c.idx]) * c.weight >> 8
			}
			out[x] = acc // 8.8 fixed point
		}
	}
	for y, cs := range vw {
		out := dst[y*dstride:]
		for x := range dw {
			var acc int64
			for _, c := range cs {
				acc += int64(tmp[c.idx*dw+x]) * int64(c.weight)
			}
			v := (acc + 1<<23) >> 24
			out[x] = uint8(max(0, min(255, v))) // #nosec G115 -- clamped
		}
	}
}

// Scale resamples an I420 picture to w x h (even sizes).
func Scale(src *image.YCbCr, w, h int) *image.YCbCr {
	sw, sh := src.Rect.Dx(), src.Rect.Dy()
	dst := image.NewYCbCr(image.Rect(0, 0, w, h), image.YCbCrSubsampleRatio420)
	if sw == w && sh == h {
		copy(dst.Y, src.Y)
		copy(dst.Cb, src.Cb)
		copy(dst.Cr, src.Cr)
		return dst
	}
	resizePlane(src.Y[src.YOffset(src.Rect.Min.X, src.Rect.Min.Y):], sw, sh, src.YStride, dst.Y, w, h, dst.YStride)
	csw, csh, cw, ch := (sw+1)/2, (sh+1)/2, (w+1)/2, (h+1)/2
	co := src.COffset(src.Rect.Min.X, src.Rect.Min.Y)
	resizePlane(src.Cb[co:], csw, csh, src.CStride, dst.Cb, cw, ch, dst.CStride)
	resizePlane(src.Cr[co:], csw, csh, src.CStride, dst.Cr, cw, ch, dst.CStride)
	return dst
}

// FitHeight returns the size of a picture h pixels tall with the aspect
// ratio of w0 x h0, both even.
func FitHeight(w0, h0, h int) (int, int) {
	w := int(math.Round(float64(w0) * float64(h) / float64(h0) / 2))
	return max(2, w*2), h &^ 1
}

// FitWidth returns the size of a picture w pixels wide with the aspect
// ratio of w0 x h0, both even.
func FitWidth(w0, h0, w int) (int, int) {
	h := int(math.Round(float64(h0) * float64(w) / float64(w0) / 2))
	return w &^ 1, max(2, h*2)
}
