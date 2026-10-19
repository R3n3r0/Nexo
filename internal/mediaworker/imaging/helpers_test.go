package imaging

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/require"
)

// photo returns a picture with the structure of a photograph: smooth
// gradients and shapes, edges, and a little sensor noise; with alpha, a
// vignette from opaque in the middle to transparent in the corners.
func photo(w, h int, alpha bool) *image.NRGBA {
	m := image.NewNRGBA(image.Rect(0, 0, w, h))
	rng := rand.New(rand.NewPCG(uint64(w), uint64(h)))
	cx, cy := float64(w)/2, float64(h)/2
	for y := range h {
		for x := range w {
			fx, fy := float64(x)/float64(w), float64(y)/float64(h)
			r := 120 + 100*math.Sin(fx*5+fy*2)
			g := 110 + 90*math.Cos(fy*4-fx*3)
			b := 140 + 80*math.Sin((fx+fy)*7)
			// A few disks with sharp edges.
			for i := range 5 {
				dx, dy := fx-0.15-0.17*float64(i), fy-0.3-0.1*float64(i%3)
				if dx*dx+dy*dy < 0.004 {
					r, g, b = 240-float64(40*i), 60+float64(30*i), 50
				}
			}
			n := rng.NormFloat64() * 4
			a := 255.0
			if alpha {
				d := math.Hypot(float64(x)-cx, float64(y)-cy) / math.Hypot(cx, cy)
				a = math.Max(0, math.Min(255, 255*(1.4-1.6*d)))
			}
			o := m.PixOffset(x, y)
			m.Pix[o] = clampF(r + n)
			m.Pix[o+1] = clampF(g + n)
			m.Pix[o+2] = clampF(b + n)
			m.Pix[o+3] = clampF(a)
		}
	}
	return m
}

func clampF(v float64) uint8 { return uint8(math.Max(0, math.Min(255, math.Round(v)))) }

// quadrants returns a w x h picture of four flat quadrants: red top
// left, green top right, blue bottom left, white bottom right.
func quadrants(w, h int) *image.RGBA {
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			c := color.RGBA{255, 0, 0, 255}
			switch {
			case x >= w/2 && y < h/2:
				c = color.RGBA{0, 255, 0, 255}
			case x < w/2 && y >= h/2:
				c = color.RGBA{0, 0, 255, 255}
			case x >= w/2 && y >= h/2:
				c = color.RGBA{255, 255, 255, 255}
			}
			m.SetRGBA(x, y, c)
		}
	}
	return m
}

// exifTIFF returns a big-endian TIFF structure whose IFD0 has an
// orientation and a GPS IFD pointer (as a camera writes it).
func exifTIFF(orientation int) []byte {
	var b bytes.Buffer
	b.WriteString("MM\x00*")
	_ = binary.Write(&b, binary.BigEndian, uint32(8))
	_ = binary.Write(&b, binary.BigEndian, uint16(2))
	// Orientation, SHORT, 1 value.
	_ = binary.Write(&b, binary.BigEndian, []uint16{tagOrientation, 3})
	_ = binary.Write(&b, binary.BigEndian, uint32(1))
	_ = binary.Write(&b, binary.BigEndian, []uint16{uint16(orientation), 0}) // #nosec G115 -- 1 to 8
	// GPSInfo IFD pointer, LONG: points at an empty IFD.
	_ = binary.Write(&b, binary.BigEndian, []uint16{0x8825, 4})
	_ = binary.Write(&b, binary.BigEndian, []uint32{1, 38})
	_ = binary.Write(&b, binary.BigEndian, uint32(0)) // no IFD1
	_ = binary.Write(&b, binary.BigEndian, uint16(0)) // the GPS IFD
	_ = binary.Write(&b, binary.BigEndian, uint32(0))
	return b.Bytes()
}

// withExif inserts an APP1 Exif segment after the SOI of a JPEG file.
func withExif(t testing.TB, jpg []byte, orientation int) []byte {
	t.Helper()
	require.Equal(t, []byte{0xff, 0xd8}, jpg[:2])
	payload := append([]byte("Exif\x00\x00"), exifTIFF(orientation)...)
	seg := []byte{0xff, 0xe1, 0, 0}
	binary.BigEndian.PutUint16(seg[2:], uint16(len(payload)+2)) // #nosec G115 -- small
	out := append([]byte{0xff, 0xd8}, seg...)
	out = append(out, payload...)
	return append(out, jpg[2:]...)
}

func encodeJPEG(t testing.TB, m image.Image, q int) []byte {
	t.Helper()
	var b bytes.Buffer
	require.NoError(t, jpeg.Encode(&b, m, &jpeg.Options{Quality: q}))
	return b.Bytes()
}

// near reports whether two colors differ by at most tol per channel.
func near(a, b color.Color, tol int) bool {
	ar, ag, ab, aa := a.RGBA()
	br, bg, bb, ba := b.RGBA()
	d := func(x, y uint32) bool { return math.Abs(float64(x>>8)-float64(y>>8)) <= float64(tol) }
	return d(ar, br) && d(ag, bg) && d(ab, bb) && d(aa, ba)
}
