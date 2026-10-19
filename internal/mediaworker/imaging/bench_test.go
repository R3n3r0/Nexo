package imaging

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"os"
	"runtime"
	"sync"
	"testing"

	"github.com/gen2brain/avif"
	"github.com/gen2brain/h265/heic"
	gwebp "github.com/gen2brain/webp"
)

// The benchmarks run on a 12-megapixel photo-like picture (4000x3000:
// gradients, edges, sensor noise), the size of a phone camera. With
// NEXO_BENCH_IMAGE=<file> BenchmarkJob also measures an image of your own.
const benchW, benchH = 4000, 3000

var (
	benchOnce sync.Once
	benchPic  *image.NRGBA
	benchSrc  map[string][]byte
)

// benchSources returns the 12 MP picture and its encodings.
func benchSources(b *testing.B) (*image.NRGBA, map[string][]byte) {
	b.Helper()
	benchOnce.Do(func() {
		benchPic = photo(benchW, benchH, false)
		benchSrc = map[string][]byte{JPEG: encodeJPEG(b, benchPic, 90)}
		var buf bytes.Buffer
		if err := png.Encode(&buf, benchPic); err != nil {
			b.Fatal(err)
		}
		benchSrc[PNG] = bytes.Clone(buf.Bytes())
		buf.Reset()
		if err := gwebp.Encode(&buf, benchPic, gwebp.Options{Quality: 85}); err != nil {
			b.Fatal(err)
		}
		benchSrc[WebP] = bytes.Clone(buf.Bytes())
		buf.Reset()
		if err := avif.Encode(&buf, benchPic, avif.Options{Quality: 70, Speed: 10}); err != nil {
			b.Fatal(err)
		}
		benchSrc[AVIF] = bytes.Clone(buf.Bytes())
		buf.Reset()
		if err := heic.Encode(&buf, benchPic, heic.EncodeOptions{Quality: 70}); err != nil {
			b.Fatal(err)
		}
		benchSrc[HEIC] = bytes.Clone(buf.Bytes())
	})
	return benchPic, benchSrc
}

// report adds the milliseconds per operation and the megapixels per
// second of pixels pixels per operation.
func report(b *testing.B, pixels int) {
	b.Helper()
	sec := b.Elapsed().Seconds() / float64(b.N)
	b.ReportMetric(sec*1000, "ms")
	b.ReportMetric(float64(pixels)/1e6/sec, "MP/s")
}

// BenchmarkDecode decodes the 12 MP picture from each source format, to
// the picture the variants are made from.
func BenchmarkDecode(b *testing.B) {
	_, src := benchSources(b)
	for _, f := range []string{JPEG, PNG, WebP, AVIF, HEIC} {
		b.Run(f, func(b *testing.B) {
			b.SetBytes(int64(len(src[f])))
			for b.Loop() {
				if _, err := Decode(src[f], Limits{}); err != nil {
					b.Fatal(err)
				}
			}
			report(b, benchW*benchH)
		})
	}
}

// BenchmarkResize resizes the 12 MP picture to the widths of a rule:
// from premultiplied RGBA (PNG, WebP... sources), and from the YCbCr
// planes of a JPEG (converted to RGBA after resizing).
func BenchmarkResize(b *testing.B) {
	pic, src := benchSources(b)
	rgba := &Image{Width: benchW, Height: benchH, orientation: 1, rgba: toRGBA(pic), opaque: true}
	ycc, err := Decode(src[JPEG], Limits{})
	if err != nil || ycc.ycc == nil {
		b.Fatal("the JPEG decodes to YCbCr", err)
	}
	for _, c := range []struct {
		name string
		im   *Image
	}{{"rgba", rgba}, {"ycbcr", ycc}} {
		for _, w := range []int{2560, 1280, 640, 160} {
			s := Sizes(benchW, benchH, []int{w})[1]
			b.Run(fmt.Sprintf("%s/%d", c.name, w), func(b *testing.B) {
				for b.Loop() {
					c.im.picture(s)
				}
				report(b, benchW*benchH)
			})
		}
	}
	b.Run("ycbcr/full", func(b *testing.B) {
		for b.Loop() {
			ycc.picture(Size{PixelWidth: benchW, PixelHeight: benchH})
		}
		report(b, benchW*benchH)
	})
	b.Run("orient-6/full", func(b *testing.B) {
		for b.Loop() {
			orient(rgba.rgba, 6)
		}
		report(b, benchW*benchH)
	})
}

// BenchmarkEncode encodes one variant per format, on one core: the 12 MP
// picture at its own size, 1920x1440 (a large screen) and 640x480.
func BenchmarkEncode(b *testing.B) {
	pic, _ := benchSources(b)
	src := &Image{Width: benchW, Height: benchH, orientation: 1, rgba: toRGBA(pic), opaque: true}
	for _, w := range []int{benchW, 1920, 640} {
		s := Size{PixelWidth: benchW, PixelHeight: benchH}
		if w < benchW {
			s = Sizes(benchW, benchH, []int{w})[1]
		}
		m := src.picture(s)
		for _, f := range EncodeFormats() {
			b.Run(fmt.Sprintf("%s/%d", f, w), func(b *testing.B) {
				var size int
				for b.Loop() {
					data, err := Encode(f, m, true)
					if err != nil {
						b.Fatal(err)
					}
					size = len(data)
				}
				report(b, s.PixelWidth*s.PixelHeight)
				b.ReportMetric(float64(size)/1024, "KiB")
			})
		}
	}
}

// BenchmarkJob is the job of the worker for one image: decode, then every
// default width (160 to 2560) and the image at its own size, in the four
// formats (28 variants of the 12 MP JPEG), on every core.
func BenchmarkJob(b *testing.B) {
	_, src := benchSources(b)
	inputs := []struct {
		name string
		data []byte
	}{{"12MP-jpeg", src[JPEG]}, {"12MP-heic", src[HEIC]}}
	if path := os.Getenv("NEXO_BENCH_IMAGE"); path != "" {
		data, err := os.ReadFile(path) // #nosec G304 G703 -- a benchmark input
		if err != nil {
			b.Fatal(err)
		}
		inputs = append(inputs, struct {
			name string
			data []byte
		}{"own", data})
	}
	widths := []int{160, 320, 640, 1280, 1920, 2560}
	for _, in := range inputs {
		for _, formats := range [][]string{EncodeFormats(), {WebP, JPEG}} {
			name := in.name + "/all"
			if len(formats) == 2 {
				name = in.name + "/webp+jpeg"
			}
			b.Run(name, func(b *testing.B) {
				var px, n int
				for b.Loop() {
					im, err := Decode(in.data, Limits{})
					if err != nil {
						b.Fatal(err)
					}
					px, n = im.Width*im.Height, 0
					err = im.Variants(b.Context(), Options{Formats: formats, Widths: widths}, func(Variant) error {
						n++
						return nil
					})
					if err != nil {
						b.Fatal(err)
					}
				}
				report(b, px)
				b.ReportMetric(float64(n), "variants")
			})
		}
	}
}

// BenchmarkColor measures the conversion to sRGB of the 12 MP picture:
// building the tables of a Display P3 profile, converting the decoded
// RGBA in place (every core), and decoding the JPEG with and without the
// profile embedded (which converts it to RGBA, not kept in YCbCr).
func BenchmarkColor(b *testing.B) {
	pic, src := benchSources(b)
	icc := displayP3ICC(b)
	b.Run("tables", func(b *testing.B) {
		for b.Loop() {
			if newTransform(parseICC(icc)) == nil {
				b.Fatal("no conversion")
			}
		}
	})
	b.Run("convert", func(b *testing.B) {
		m := toRGBA(pic)
		tr := newTransform(parseICC(icc))
		for b.Loop() {
			tr.apply(m)
		}
		report(b, benchW*benchH)
	})
	b.Run("convert/1-core", func(b *testing.B) {
		m := toRGBA(pic)
		tr := newTransform(parseICC(icc))
		defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
		for b.Loop() {
			tr.apply(m)
		}
		report(b, benchW*benchH)
	})
	for name, data := range map[string][]byte{"jpeg/srgb": src[JPEG], "jpeg/p3": withICC(b, src[JPEG], icc, 1)} {
		b.Run("decode-"+name, func(b *testing.B) {
			for b.Loop() {
				if _, err := Decode(data, Limits{}); err != nil {
					b.Fatal(err)
				}
			}
			report(b, benchW*benchH)
		})
	}
}
