package transcode

import (
	"image"
	"math"
	"runtime"
	"sync"
)

// taps are the source samples of each output sample of a resampling, flat:
// output i is the sum of src[idx[k]] * wt[k] for k in off[i]:off[i+1],
// with weights in 16.16 summing to 1<<16.
type taps struct {
	off, idx, wt []int32
}

// newTaps returns the taps of a resampling of src samples into dst: the
// area they cover when reducing, the two neighbours (linear) when
// enlarging.
func newTaps(src, dst int) taps {
	t := taps{off: make([]int32, 0, dst+1)}
	scale := float64(src) / float64(dst)
	for i := range dst {
		first := len(t.idx)
		t.off = append(t.off, int32(first)) // #nosec G115 -- bounded by the picture size
		if scale > 1 {
			lo, hi := float64(i)*scale, float64(i+1)*scale
			for j := int(lo); j < int(math.Ceil(hi)) && j < src; j++ {
				w := math.Min(hi, float64(j+1)) - math.Max(lo, float64(j))
				if w > 0 {
					t.idx = append(t.idx, int32(j)) // #nosec G115 -- bounded by the picture size
					t.wt = append(t.wt, int32(w/scale*65536))
				}
			}
		} else {
			c := (float64(i)+0.5)*scale - 0.5
			j := int(math.Floor(c))
			f := c - float64(j)
			t.idx = append(t.idx, int32(clampIdx(j, src)), int32(clampIdx(j+1, src))) // #nosec G115 -- bounded by the picture size
			t.wt = append(t.wt, int32((1-f)*65536), int32(f*65536))
		}
		// Rounding leftovers go to the heaviest sample.
		var sum int32
		best := first
		for k := first; k < len(t.wt); k++ {
			sum += t.wt[k]
			if t.wt[k] > t.wt[best] {
				best = k
			}
		}
		t.wt[best] += 65536 - sum
	}
	t.off = append(t.off, int32(len(t.idx))) // #nosec G115 -- bounded by the picture size
	return t
}

func clampIdx(i, n int) int { return max(0, min(i, n-1)) }

// planeScaler resamples planes of one size into another: the vertical
// pass first, row by row (contiguous reads, and when reducing it leaves
// fewer rows for the horizontal pass, whose reads are indexed).
type planeScaler struct {
	sw, sh, dw, dh int
	h, v           taps
	tmp            []int32   // vertical pass: dh rows of sw, 8.8 fixed point
	acc            [][]int32 // vertical pass: one row of sw per band, 16.16 >> 8
	bands          int
}

// minBandPixels is the smallest share of a plane a goroutine resamples: a
// smaller plane is resampled on the calling goroutine.
const minBandPixels = 64 << 10

func newPlaneScaler(sw, sh, dw, dh int) *planeScaler {
	bands := max(1, min(runtime.GOMAXPROCS(0), max(sw, dw)*dh/minBandPixels))
	p := &planeScaler{sw: sw, sh: sh, dw: dw, dh: dh, h: newTaps(sw, dw), v: newTaps(sh, dh),
		tmp: make([]int32, sw*dh), bands: bands}
	for range bands {
		p.acc = append(p.acc, make([]int32, sw))
	}
	return p
}

// inBands runs fn over n rows split into the bands of p, in parallel.
func (p *planeScaler) inBands(n int, fn func(band, lo, hi int)) {
	if p.bands == 1 {
		fn(0, 0, n)
		return
	}
	var wg sync.WaitGroup
	for b := range p.bands {
		lo, hi := n*b/p.bands, n*(b+1)/p.bands
		wg.Go(func() { fn(b, lo, hi) })
	}
	wg.Wait()
}

// resize resamples src (stride sstride) into dst (stride dstride).
func (p *planeScaler) resize(src []byte, sstride int, dst []byte, dstride int) {
	sw, dw := p.sw, p.dw
	p.inBands(p.dh, func(band, lo, hi int) {
		off, idx, wt := p.v.off, p.v.idx, p.v.wt
		acc := p.acc[band]
		for y := lo; y < hi; y++ {
			clear(acc)
			for k := off[y]; k < off[y+1]; k++ {
				w := wt[k]
				row := src[int(idx[k])*sstride : int(idx[k])*sstride+sw]
				for x, v := range row {
					acc[x] += int32(v) * w >> 8
				}
			}
			copy(p.tmp[y*sw:(y+1)*sw], acc)
		}
	})
	p.inBands(p.dh, func(_, lo, hi int) {
		off, idx, wt := p.h.off, p.h.idx, p.h.wt
		for y := lo; y < hi; y++ {
			row := p.tmp[y*sw : (y+1)*sw]
			out := dst[y*dstride : y*dstride+dw]
			for x := range out {
				var acc int64
				for k := off[x]; k < off[x+1]; k++ {
					acc += int64(row[idx[k]]) * int64(wt[k])
				}
				out[x] = uint8(max(0, min(255, (acc+1<<23)>>24))) // #nosec G115 -- clamped
			}
		}
	})
}

// Scaler resamples I420 pictures, keeping its weights and buffers from one
// picture to the next: a rendition scales every picture of a video from
// and to the same sizes. The picture it returns is reused by the next call.
type Scaler struct {
	sw, sh, w, h int
	y, c         *planeScaler
	dst          *image.YCbCr
}

// Scale resamples src to w x h (even sizes); the result is valid until the
// next call.
func (s *Scaler) Scale(src *image.YCbCr, w, h int) *image.YCbCr {
	sw, sh := src.Rect.Dx(), src.Rect.Dy()
	if s.dst == nil || s.sw != sw || s.sh != sh || s.w != w || s.h != h {
		s.sw, s.sh, s.w, s.h = sw, sh, w, h
		s.dst = image.NewYCbCr(image.Rect(0, 0, w, h), image.YCbCrSubsampleRatio420)
		s.y, s.c = nil, nil
		if sw != w || sh != h {
			s.y = newPlaneScaler(sw, sh, w, h)
			s.c = newPlaneScaler((sw+1)/2, (sh+1)/2, (w+1)/2, (h+1)/2)
		}
	}
	dst := s.dst
	if s.y == nil {
		for y := range h {
			copy(dst.Y[y*dst.YStride:y*dst.YStride+w], src.Y[src.YOffset(src.Rect.Min.X, src.Rect.Min.Y+y):])
		}
		for y := range (h + 1) / 2 {
			co := src.COffset(src.Rect.Min.X, src.Rect.Min.Y+2*y)
			copy(dst.Cb[y*dst.CStride:y*dst.CStride+(w+1)/2], src.Cb[co:])
			copy(dst.Cr[y*dst.CStride:y*dst.CStride+(w+1)/2], src.Cr[co:])
		}
		return dst
	}
	s.y.resize(src.Y[src.YOffset(src.Rect.Min.X, src.Rect.Min.Y):], src.YStride, dst.Y, dst.YStride)
	co := src.COffset(src.Rect.Min.X, src.Rect.Min.Y)
	s.c.resize(src.Cb[co:], src.CStride, dst.Cb, dst.CStride)
	s.c.resize(src.Cr[co:], src.CStride, dst.Cr, dst.CStride)
	return dst
}

// Scale resamples an I420 picture to w x h (even sizes) into a new picture.
func Scale(src *image.YCbCr, w, h int) *image.YCbCr {
	return new(Scaler).Scale(src, w, h)
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
