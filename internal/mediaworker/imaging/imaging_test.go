package imaging

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"slices"
	"sync"
	"testing"

	"github.com/gen2brain/avif"
	"github.com/gen2brain/h265/heic"
	gwebp "github.com/gen2brain/webp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/image/bmp"
	"golang.org/x/image/tiff"
	"golang.org/x/image/webp"
)

// sources encodes a picture in every source format.
func sources(t *testing.T, m image.Image) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	enc := func(format string, fn func(*bytes.Buffer) error) {
		var b bytes.Buffer
		require.NoError(t, fn(&b), format)
		out[format] = b.Bytes()
	}
	enc(JPEG, func(b *bytes.Buffer) error { return jpeg.Encode(b, m, &jpeg.Options{Quality: 95}) })
	enc(PNG, func(b *bytes.Buffer) error { return png.Encode(b, m) })
	enc(GIF, func(b *bytes.Buffer) error { return gif.Encode(b, m, nil) })
	enc(BMP, func(b *bytes.Buffer) error { return bmp.Encode(b, m) })
	enc(TIFF, func(b *bytes.Buffer) error { return tiff.Encode(b, m, nil) })
	enc(WebP, func(b *bytes.Buffer) error { return gwebp.Encode(b, m, gwebp.Options{Quality: 95}) })
	enc(AVIF, func(b *bytes.Buffer) error { return avif.Encode(b, m, avif.Options{Quality: 90, Speed: 10}) })
	enc(HEIC, func(b *bytes.Buffer) error { return heic.Encode(b, m, heic.EncodeOptions{Quality: 90}) })
	return out
}

func TestDetect(t *testing.T) {
	for format, data := range sources(t, quadrants(32, 16)) {
		got, err := Detect(data)
		require.NoError(t, err, format)
		assert.Equal(t, format, got)
	}
	// A HEIF file whose major brand is the generic one: an AVIF brand
	// makes it AVIF.
	ftyp := func(brands ...string) []byte {
		b := []byte{0, 0, 0, byte(8 + 4*len(brands) + 4), 'f', 't', 'y', 'p'}
		b = append(b, brands[0]...)
		b = append(b, 0, 0, 0, 0)
		for _, x := range brands[1:] {
			b = append(b, x...)
		}
		return b
	}
	f, err := Detect(ftyp("mif1", "mif1", "avif", "miaf"))
	require.NoError(t, err)
	assert.Equal(t, AVIF, f)
	f, err = Detect(ftyp("mif1", "mif1", "heic"))
	require.NoError(t, err)
	assert.Equal(t, HEIC, f)
	for _, bad := range [][]byte{nil, []byte("hello world, not an image"), ftyp("isom", "isom", "mp41")} {
		_, err := Detect(bad)
		require.ErrorIs(t, err, ErrUnsupported)
	}
}

// TestDecodeEveryFormat decodes each format of a picture of flat
// quadrants: right size, right colors (within the losses of the format).
func TestDecodeEveryFormat(t *testing.T) {
	src := quadrants(64, 48)
	for format, data := range sources(t, src) {
		t.Run(format, func(t *testing.T) {
			im, err := Decode(data, Limits{})
			require.NoError(t, err)
			assert.Equal(t, format, im.Format)
			assert.Equal(t, 64, im.Width)
			assert.Equal(t, 48, im.Height)
			assert.True(t, im.opaque)
			pic := im.picture(Size{PixelWidth: 64, PixelHeight: 48})
			for _, p := range []image.Point{{8, 8}, {56, 8}, {8, 40}, {56, 40}} {
				assert.True(t, near(src.At(p.X, p.Y), pic.At(p.X, p.Y), 24), "%s at %v: %v, want %v", format, p, pic.At(p.X, p.Y), src.At(p.X, p.Y))
			}
		})
	}
}

func TestDecodeAlpha(t *testing.T) {
	src := photo(120, 80, true)
	for _, format := range []string{PNG, WebP, AVIF, HEIC, TIFF} {
		data := sources(t, src)[format]
		im, err := Decode(data, Limits{})
		require.NoError(t, err, format)
		assert.False(t, im.opaque, format)
		pic := im.picture(Size{PixelWidth: 120, PixelHeight: 80})
		// The corners are transparent, the middle opaque.
		assert.Less(t, pic.RGBAAt(0, 0).A, uint8(30), format)
		assert.Greater(t, pic.RGBAAt(60, 40).A, uint8(225), format)
	}
}

func TestSizes(t *testing.T) {
	assert.Equal(t, []Size{
		{PixelWidth: 1000, PixelHeight: 600},
		{Width: 640, PixelWidth: 640, PixelHeight: 384},
		{Width: 160, PixelWidth: 160, PixelHeight: 96},
	}, Sizes(1000, 600, []int{160, 2000, 640, 1000, 160}))
	assert.Equal(t, []Size{{PixelWidth: 100, PixelHeight: 5000}, {Width: 16, PixelWidth: 16, PixelHeight: 800}}, Sizes(100, 5000, []int{16}))
	assert.Equal(t, []Size{{PixelWidth: 5000, PixelHeight: 3}, {Width: 16, PixelWidth: 16, PixelHeight: 1}}, Sizes(5000, 3, []int{16}), "at least one pixel")
}

// TestOrientation: a JPEG with each EXIF orientation comes out upright,
// at every size.
func TestOrientation(t *testing.T) {
	red, green, blue, white := color.RGBA{255, 0, 0, 255}, color.RGBA{0, 255, 0, 255}, color.RGBA{0, 0, 255, 255}, color.RGBA{255, 255, 255, 255}
	// The corners of the upright picture (top left, top right, bottom
	// left, bottom right) of a stored picture red, green / blue, white.
	want := map[int][4]color.RGBA{
		1: {red, green, blue, white},
		2: {green, red, white, blue},
		3: {white, blue, green, red},
		4: {blue, white, red, green},
		5: {red, blue, green, white},
		6: {blue, red, white, green},
		7: {white, green, blue, red},
		8: {green, white, red, blue},
	}
	stored := encodeJPEG(t, quadrants(320, 160), 95)
	for o := 1; o <= 8; o++ {
		data := withExif(t, stored, o)
		im, err := Decode(data, Limits{})
		require.NoError(t, err)
		w, h := 320, 160
		if o >= 5 {
			w, h = 160, 320
		}
		require.Equal(t, [2]int{w, h}, [2]int{im.Width, im.Height}, "orientation %d", o)
		for _, s := range Sizes(im.Width, im.Height, []int{80}) {
			pic := im.picture(s)
			pw, ph := s.PixelWidth, s.PixelHeight
			require.Equal(t, image.Rect(0, 0, pw, ph), pic.Rect)
			corners := []image.Point{{pw / 8, ph / 8}, {pw * 7 / 8, ph / 8}, {pw / 8, ph * 7 / 8}, {pw * 7 / 8, ph * 7 / 8}}
			for i, p := range corners {
				assert.True(t, near(want[o][i], pic.At(p.X, p.Y), 40), "orientation %d, %dpx, corner %d: %v, want %v", o, pw, i, pic.At(p.X, p.Y), want[o][i])
			}
		}
	}
	// TIFF carries its orientation in its own IFD0.
	assert.Equal(t, 6, tiffOrientation(exifTIFF(6)))
	assert.Equal(t, 1, tiffOrientation([]byte("II*\x00\xff\xff\xff\xff")), "bad offset")
	assert.Equal(t, 1, jpegOrientation(stored), "no EXIF")
}

func TestLimits(t *testing.T) {
	// A PNG header claiming 50000 x 50000 pixels: refused before decoding.
	var bomb bytes.Buffer
	bomb.WriteString("\x89PNG\r\n\x1a\n")
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], 50000)
	binary.BigEndian.PutUint32(ihdr[4:], 50000)
	ihdr[8], ihdr[9] = 8, 6
	chunk := func(typ string, data []byte) {
		_ = binary.Write(&bomb, binary.BigEndian, uint32(len(data))) // #nosec G115 -- small
		bomb.WriteString(typ)
		bomb.Write(data)
		_ = binary.Write(&bomb, binary.BigEndian, crc32.ChecksumIEEE(append([]byte(typ), data...)))
	}
	chunk("IHDR", ihdr)
	chunk("IEND", nil)
	_, err := Decode(bomb.Bytes(), Limits{})
	require.ErrorIs(t, err, ErrTooLarge)

	jpg := encodeJPEG(t, quadrants(200, 100), 80)
	_, err = Decode(jpg, Limits{MaxPixels: 200*100 - 1})
	require.ErrorIs(t, err, ErrTooLarge)
	_, err = Decode(jpg, Limits{MaxBytes: int64(len(jpg) - 1)})
	require.ErrorIs(t, err, ErrTooLarge)
	_, err = Decode(jpg, Limits{MaxPixels: 200 * 100, MaxBytes: int64(len(jpg))})
	require.NoError(t, err)

	// Not an image, or a corrupt one.
	_, err = Decode([]byte("GIF89a\x01\x00\x01\x00 garbage"), Limits{})
	require.ErrorIs(t, err, ErrUnsupported)
	_, err = Decode(jpg[:len(jpg)/2], Limits{})
	require.ErrorIs(t, err, ErrUnsupported)
	_, err = Decode(append([]byte("RIFF\x10\x00\x00\x00WEBPVP8 "), make([]byte, 64)...), Limits{})
	assert.True(t, errors.Is(err, ErrUnsupported) || errors.Is(err, ErrTooLarge), "%v", err)
}

// decodeVariant decodes a variant with an independent decoder.
func decodeVariant(t *testing.T, v Variant) image.Image {
	t.Helper()
	r := bytes.NewReader(v.Data)
	var m image.Image
	var err error
	switch v.Format {
	case JPEG:
		m, err = jpeg.Decode(r)
	case PNG:
		m, err = png.Decode(r)
	case WebP:
		m, err = webp.Decode(r)
	case AVIF:
		m, err = avif.Decode(r)
	}
	require.NoError(t, err, v.Format)
	return m
}

func variants(t *testing.T, im *Image, o Options) []Variant {
	t.Helper()
	var out []Variant
	require.NoError(t, im.Variants(t.Context(), o, func(v Variant) error {
		out = append(out, v)
		return nil
	}))
	slices.SortFunc(out, func(a, b Variant) int {
		if a.Format != b.Format {
			return slices.Index(EncodeFormats(), a.Format) - slices.Index(EncodeFormats(), b.Format)
		}
		return a.PixelWidth - b.PixelWidth
	})
	return out
}

func TestVariants(t *testing.T) {
	src := photo(1000, 600, false)
	data := withExif(t, encodeJPEG(t, src, 92), 1) // with EXIF and a GPS IFD
	im, err := Decode(data, Limits{})
	require.NoError(t, err)
	require.NotNil(t, im.ycc, "a JPEG is resized in YCbCr")
	got := variants(t, im, Options{Widths: []int{160, 640, 1000, 4000}})
	require.Len(t, got, 4*3)
	for _, v := range got {
		m := decodeVariant(t, v)
		require.Equal(t, image.Rect(0, 0, v.PixelWidth, v.PixelHeight), m.Bounds(), "%s %d", v.Format, v.Width)
		assert.Equal(t, map[int]int{0: 1000, 640: 640, 160: 160}[v.Width], v.PixelWidth)
		assert.NotContains(t, string(v.Data), "Exif", "no metadata of the source")
		// The middle of the variant has the color of the middle of the
		// source (the area average of a smooth region).
		c := m.At(v.PixelWidth/2, v.PixelHeight*3/4)
		assert.True(t, near(src.At(500, 450), c, 20), "%s %d: %v, want %v", v.Format, v.Width, c, src.At(500, 450))
	}
	// The compact formats are smaller than JPEG, JPEG than PNG.
	size := map[string]int{}
	for _, v := range got {
		if v.Width == 0 {
			size[v.Format] = len(v.Data)
		}
	}
	assert.Less(t, size[AVIF], size[JPEG])
	assert.Less(t, size[WebP], size[JPEG])
	assert.Less(t, size[JPEG], size[PNG])
}

func TestVariantsAlpha(t *testing.T) {
	src := photo(400, 300, true)
	var b bytes.Buffer
	require.NoError(t, png.Encode(&b, src))
	im, err := Decode(b.Bytes(), Limits{})
	require.NoError(t, err)
	for _, v := range variants(t, im, Options{Widths: []int{200}}) {
		m := decodeVariant(t, v)
		corner := color.NRGBAModel.Convert(m.At(1, 1)).(color.NRGBA)
		middle := color.NRGBAModel.Convert(m.At(v.PixelWidth/2, v.PixelHeight/2)).(color.NRGBA)
		if v.Format == JPEG {
			assert.True(t, near(color.White, corner, 8), "flattened onto white: %v", corner)
		} else {
			assert.Less(t, corner.A, uint8(20), "%s keeps the alpha", v.Format)
		}
		assert.Greater(t, middle.A, uint8(240), v.Format)
		// Unpremultiplied correctly: the color of a semi-transparent
		// pixel is not darkened.
		p := image.Pt(v.PixelWidth/5, v.PixelHeight/2)
		sp := image.Pt(p.X*400/v.PixelWidth, p.Y*300/v.PixelHeight)
		got := color.NRGBAModel.Convert(m.At(p.X, p.Y)).(color.NRGBA)
		want := src.NRGBAAt(sp.X, sp.Y)
		if v.Format != JPEG && want.A > 60 {
			assert.True(t, near(color.NRGBA{want.R, want.G, want.B, 255}, color.NRGBA{got.R, got.G, got.B, 255}, 28),
				"%s %d at %v: %v, want %v", v.Format, v.Width, p, got, want)
		}
	}
}

func TestVariantsFormatsAndErrors(t *testing.T) {
	im, err := Decode(encodeJPEG(t, photo(300, 200, false), 90), Limits{})
	require.NoError(t, err)
	got := variants(t, im, Options{Formats: []string{PNG, JPEG}, Widths: []int{100}})
	require.Len(t, got, 4)
	assert.Equal(t, JPEG, got[0].Format)
	assert.Equal(t, PNG, got[3].Format)

	err = im.Variants(t.Context(), Options{Formats: []string{"bmp"}}, func(Variant) error { return nil })
	require.ErrorIs(t, err, ErrUnsupported)

	boom := errors.New("upload failed")
	var mu sync.Mutex
	calls := 0
	err = im.Variants(t.Context(), Options{Widths: []int{16, 32, 64}, Workers: 1}, func(Variant) error {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return boom
	})
	require.ErrorIs(t, err, boom)
	assert.Equal(t, 1, calls, "the first error stops the job")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err = im.Variants(ctx, Options{}, func(Variant) error { return nil })
	require.ErrorIs(t, err, context.Canceled)

	// A variant whose encoding would not fit in the whole memory budget is
	// not produced (an AVIF of a very large image), the others are, one at
	// a time.
	budget := EncodeMemory(WebP, im.Width*im.Height)
	got = variants(t, im, Options{Formats: []string{AVIF, JPEG}, Widths: []int{100}, MemoryBudget: budget})
	formats := map[string]int{}
	for _, v := range got {
		formats[v.Format]++
		assert.LessOrEqual(t, EncodeMemory(v.Format, v.PixelWidth*v.PixelHeight), budget, "%s %dx%d", v.Format, v.PixelWidth, v.PixelHeight)
	}
	assert.Equal(t, map[string]int{JPEG: 2, AVIF: 1}, formats, "no AVIF at the size of the image")
}
