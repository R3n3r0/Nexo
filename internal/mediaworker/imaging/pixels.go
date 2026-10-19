package imaging

import (
	"image"
	"image/draw"
	"runtime"
	"sync"
)

// minParallelPixels is the smallest picture a conversion splits across
// goroutines.
const minParallelPixels = 256 << 10

// parallel runs fn over n rows of a picture of pixels pixels, split into
// bands on every core when it is large enough.
func parallel(n, pixels int, fn func(lo, hi int)) {
	bands := max(1, min(runtime.GOMAXPROCS(0), pixels/minParallelPixels, n))
	if bands == 1 {
		fn(0, n)
		return
	}
	var wg sync.WaitGroup
	for b := range bands {
		lo, hi := n*b/bands, n*(b+1)/bands
		wg.Go(func() { fn(lo, hi) })
	}
	wg.Wait()
}

// tight reports whether an RGBA picture starts at the origin with rows
// of exactly its width.
func tight(m *image.RGBA) bool {
	return m.Rect.Min == (image.Point{}) && m.Stride == 4*m.Rect.Dx() && len(m.Pix) == m.Stride*m.Rect.Dy()
}

// toRGBA converts a decoded picture to premultiplied RGBA at the origin,
// sharing its pixels when it already is (or is an opaque NRGBA, whose
// samples are the same).
func toRGBA(src image.Image) *image.RGBA {
	b := src.Bounds()
	switch m := src.(type) {
	case *image.RGBA:
		if tight(m) {
			return m
		}
	case *image.NRGBA:
		if r := (&image.RGBA{Pix: m.Pix, Stride: m.Stride, Rect: m.Rect}); tight(r) && m.Opaque() {
			return r
		}
	case *image.NYCbCrA:
		dst := toRGBA(&m.YCbCr)
		parallel(b.Dy(), b.Dx()*b.Dy(), func(lo, hi int) {
			for y := lo; y < hi; y++ {
				ao := m.AOffset(b.Min.X, b.Min.Y+y)
				row := dst.Pix[y*dst.Stride : y*dst.Stride+4*b.Dx()]
				for x, a := range m.A[ao : ao+b.Dx()] {
					p := row[4*x : 4*x+4 : 4*x+4]
					p[0] = uint8((uint32(p[0])*uint32(a) + 127) / 255)
					p[1] = uint8((uint32(p[1])*uint32(a) + 127) / 255)
					p[2] = uint8((uint32(p[2])*uint32(a) + 127) / 255)
					p[3] = a
				}
			}
		})
		return dst
	}
	dst := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	parallel(b.Dy(), b.Dx()*b.Dy(), func(lo, hi int) {
		draw.Draw(dst, image.Rect(0, lo, b.Dx(), hi), src, b.Min.Add(image.Pt(0, lo)), draw.Src)
	})
	return dst
}

// opaque reports whether every pixel of a tight RGBA picture is opaque.
func opaque(m *image.RGBA) bool {
	for i := 3; i < len(m.Pix); i += 4 {
		if m.Pix[i] != 0xff {
			return false
		}
	}
	return true
}

// unpremultiply returns the NRGBA picture of a premultiplied one.
func unpremultiply(m *image.RGBA) *image.NRGBA {
	dst := image.NewNRGBA(m.Rect)
	w, h := m.Rect.Dx(), m.Rect.Dy()
	parallel(h, w*h, func(lo, hi int) {
		for i := lo * m.Stride; i < hi*m.Stride; i += 4 {
			s, d := m.Pix[i:i+4:i+4], dst.Pix[i:i+4:i+4]
			switch a := uint32(s[3]); a {
			case 0xff:
				copy(d, s)
			case 0:
				d[0], d[1], d[2], d[3] = 0, 0, 0, 0
			default:
				d[0] = uint8(min(255, (uint32(s[0])*255+a/2)/a))
				d[1] = uint8(min(255, (uint32(s[1])*255+a/2)/a))
				d[2] = uint8(min(255, (uint32(s[2])*255+a/2)/a))
				d[3] = uint8(a)
			}
		}
	})
	return dst
}

// flatten returns a premultiplied picture composited onto white (for the
// formats without alpha).
func flatten(m *image.RGBA) *image.RGBA {
	dst := image.NewRGBA(m.Rect)
	w, h := m.Rect.Dx(), m.Rect.Dy()
	parallel(h, w*h, func(lo, hi int) {
		for i := lo * m.Stride; i < hi*m.Stride; i += 4 {
			s, d := m.Pix[i:i+4:i+4], dst.Pix[i:i+4:i+4]
			white := 0xff - s[3]
			d[0], d[1], d[2], d[3] = s[0]+white, s[1]+white, s[2]+white, 0xff
		}
	})
	return dst
}

// orient returns the upright picture of a stored one with an EXIF
// orientation (1 to 8): mirrored and rotated as the orientation says.
func orient(m *image.RGBA, o int) *image.RGBA {
	if o <= 1 || o > 8 {
		return m
	}
	sw, sh := m.Rect.Dx(), m.Rect.Dy()
	dw, dh := sw, sh
	if o >= 5 {
		dw, dh = sh, sw
	}
	// src returns the source pixel of the output pixel (x, y).
	src := func(x, y int) (int, int) {
		switch o {
		case 2:
			return sw - 1 - x, y
		case 3:
			return sw - 1 - x, sh - 1 - y
		case 4:
			return x, sh - 1 - y
		case 5:
			return y, x
		case 6:
			return y, sh - 1 - x
		case 7:
			return sw - 1 - y, sh - 1 - x
		default: // 8
			return sw - 1 - y, x
		}
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	parallel(dh, dw*dh, func(lo, hi int) {
		for y := lo; y < hi; y++ {
			x0, y0 := src(0, y)
			x1, y1 := src(1, y)
			off, step := y0*m.Stride+x0*4, (y1-y0)*m.Stride+(x1-x0)*4
			row := dst.Pix[y*dst.Stride : y*dst.Stride+dw*4]
			for x := 0; x < len(row); x += 4 {
				copy(row[x:x+4], m.Pix[off:off+4])
				off += step
			}
		}
	})
	return dst
}
