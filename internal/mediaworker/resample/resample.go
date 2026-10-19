// Package resample resizes planes of 8-bit samples, one channel (the
// planes of a YCbCr picture) or four interleaved (RGBA): area averaging
// when reducing, linear when enlarging. The weights are computed once per
// pair of sizes, and large planes are resized in parallel bands.
package resample

import (
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

// minBandSamples is the smallest share of a resize a goroutine takes: a
// smaller one runs on the calling goroutine.
const minBandSamples = 64 << 10

// Scaler resizes planes of sw x sh pixels of ch interleaved channels into
// dw x dh. Each output row is the vertical pass over the source rows it
// covers (contiguous reads into a row of sw samples), then the horizontal
// pass over that row: no intermediate plane. A Scaler resizes one plane at
// a time; it keeps its weights and buffers from one call to the next.
type Scaler struct {
	sw, sh, dw, dh, ch int
	h, v               taps
	rows               [][]int32 // the vertical pass of each band, 8.8 fixed point
	bands              int
}

// New returns the Scaler of planes of sw x sh pixels of channels (1 or 4)
// interleaved samples into dw x dh.
func New(sw, sh, dw, dh, channels int) *Scaler {
	if channels != 4 {
		channels = 1
	}
	work := max(sw, dw) * dh * channels
	bands := max(1, min(runtime.GOMAXPROCS(0), work/minBandSamples, dh))
	s := &Scaler{sw: sw, sh: sh, dw: dw, dh: dh, ch: channels, h: newTaps(sw, dw), v: newTaps(sh, dh), bands: bands}
	for range bands {
		s.rows = append(s.rows, make([]int32, sw*channels))
	}
	return s
}

// Resize resamples src (rows sstride bytes apart) into dst (dstride).
func (s *Scaler) Resize(src []byte, sstride int, dst []byte, dstride int) {
	if s.bands == 1 {
		s.band(src, sstride, dst, dstride, s.rows[0], 0, s.dh)
		return
	}
	var wg sync.WaitGroup
	for b := range s.bands {
		lo, hi := s.dh*b/s.bands, s.dh*(b+1)/s.bands
		wg.Go(func() { s.band(src, sstride, dst, dstride, s.rows[b], lo, hi) })
	}
	wg.Wait()
}

// band resamples the output rows lo to hi.
func (s *Scaler) band(src []byte, sstride int, dst []byte, dstride int, acc []int32, lo, hi int) {
	n := s.sw * s.ch
	off, idx, wt := s.v.off, s.v.idx, s.v.wt
	for y := lo; y < hi; y++ {
		clear(acc)
		for k := off[y]; k < off[y+1]; k++ {
			w := wt[k]
			row := src[int(idx[k])*sstride : int(idx[k])*sstride+n]
			for x, v := range row {
				acc[x] += int32(v) * w >> 8
			}
		}
		out := dst[y*dstride : y*dstride+s.dw*s.ch]
		if s.ch == 4 {
			s.horizontal4(acc, out)
		} else {
			s.horizontal1(acc, out)
		}
	}
}

// clamp8 rounds a 16.16 sum of 8.8 samples to 8 bits.
func clamp8(acc int64) uint8 {
	return uint8(max(0, min(255, (acc+1<<23)>>24))) // #nosec G115 -- clamped
}

func (s *Scaler) horizontal1(row []int32, out []byte) {
	off, idx, wt := s.h.off, s.h.idx, s.h.wt
	for x := range out {
		var acc int64
		for k := off[x]; k < off[x+1]; k++ {
			acc += int64(row[idx[k]]) * int64(wt[k])
		}
		out[x] = clamp8(acc)
	}
}

func (s *Scaler) horizontal4(row []int32, out []byte) {
	off, idx, wt := s.h.off, s.h.idx, s.h.wt
	for x := range s.dw {
		var r, g, b, a int64
		for k := off[x]; k < off[x+1]; k++ {
			p := row[idx[k]*4 : idx[k]*4+4 : idx[k]*4+4]
			w := int64(wt[k])
			r += int64(p[0]) * w
			g += int64(p[1]) * w
			b += int64(p[2]) * w
			a += int64(p[3]) * w
		}
		o := out[x*4 : x*4+4 : x*4+4]
		o[0], o[1], o[2], o[3] = clamp8(r), clamp8(g), clamp8(b), clamp8(a)
	}
}
