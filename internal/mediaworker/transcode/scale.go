package transcode

import (
	"image"
	"math"

	"github.com/r3n3r0/nexo/internal/mediaworker/resample"
)

// Scaler resamples I420 pictures, keeping its weights and buffers from one
// picture to the next: a rendition scales every picture of a video from
// and to the same sizes. The picture it returns is reused by the next call.
type Scaler struct {
	sw, sh, w, h int
	y, c         *resample.Scaler
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
			s.y = resample.New(sw, sh, w, h, 1)
			s.c = resample.New((sw+1)/2, (sh+1)/2, (w+1)/2, (h+1)/2, 1)
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
	s.y.Resize(src.Y[src.YOffset(src.Rect.Min.X, src.Rect.Min.Y):], src.YStride, dst.Y, dst.YStride)
	co := src.COffset(src.Rect.Min.X, src.Rect.Min.Y)
	s.c.Resize(src.Cb[co:], src.CStride, dst.Cb, dst.CStride)
	s.c.Resize(src.Cr[co:], src.CStride, dst.Cr, dst.CStride)
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
