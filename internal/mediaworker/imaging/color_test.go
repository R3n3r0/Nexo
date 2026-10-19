package imaging

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"math"
	"slices"
	"testing"

	"github.com/gen2brain/avif"
	"github.com/gen2brain/h265/heic"
	gwebp "github.com/gen2brain/webp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ICC profiles built for the tests, as the ICC specification lays them
// out.

func s15(v float64) []byte {
	return binary.BigEndian.AppendUint32(nil, uint32(int32(math.Round(v*65536)))) // #nosec G115 -- a signed field
}

func xyzTag(v [3]float64) []byte {
	b := []byte("XYZ \x00\x00\x00\x00")
	for _, x := range v {
		b = append(b, s15(x)...)
	}
	return b
}

func paraTag(fn uint16, params ...float64) []byte {
	b := []byte("para\x00\x00\x00\x00")
	b = binary.BigEndian.AppendUint16(b, fn)
	b = append(b, 0, 0)
	for _, p := range params {
		b = append(b, s15(p)...)
	}
	return b
}

func curvTag(table ...uint16) []byte {
	b := []byte("curv\x00\x00\x00\x00")
	b = binary.BigEndian.AppendUint32(b, uint32(len(table))) // #nosec G115 -- small
	for _, v := range table {
		b = binary.BigEndian.AppendUint16(b, v)
	}
	return b
}

func srgbPara() []byte { return paraTag(3, 2.4, 1/1.055, 0.055/1.055, 1/12.92, 0.04045) }

// iccBytes lays out a profile of a color space with its tags.
func iccBytes(space string, tags map[string][]byte) []byte {
	sigs := make([]string, 0, len(tags))
	for s := range tags {
		sigs = append(sigs, s)
	}
	slices.Sort(sigs)
	hdr := make([]byte, 128)
	binary.BigEndian.PutUint32(hdr[8:], 0x04300000)
	copy(hdr[12:], "mntr")
	copy(hdr[16:], space)
	copy(hdr[20:], "XYZ ")
	copy(hdr[36:], "acsp")
	table := binary.BigEndian.AppendUint32(nil, uint32(len(sigs))) // #nosec G115 -- small
	var data []byte
	off := 128 + 4 + 12*len(sigs)
	for _, s := range sigs {
		t := tags[s]
		table = append(table, s...)
		table = binary.BigEndian.AppendUint32(table, uint32(off+len(data))) // #nosec G115 -- small
		table = binary.BigEndian.AppendUint32(table, uint32(len(t)))        // #nosec G115 -- small
		data = append(data, t...)
		for len(data)%4 != 0 {
			data = append(data, 0)
		}
	}
	out := append(append(hdr, table...), data...)
	binary.BigEndian.PutUint32(out, uint32(len(out))) // #nosec G115 -- small
	return out
}

// rgbICC returns a matrix/TRC profile of primaries and a white point
// (the colorants adapted to D50 with Bradford, as profile makers do).
func rgbICC(t testing.TB, prims [6]float64, white [2]float64, trc []byte) []byte {
	t.Helper()
	m, ok := rgbToXYZ(prims, white)
	require.True(t, ok)
	m = bradford(xyz(white[0], white[1]), pcsD50()).mul(m)
	return iccBytes("RGB ", map[string][]byte{
		"rXYZ": xyzTag([3]float64{m[0], m[3], m[6]}),
		"gXYZ": xyzTag([3]float64{m[1], m[4], m[7]}),
		"bXYZ": xyzTag([3]float64{m[2], m[5], m[8]}),
		"rTRC": trc, "gTRC": trc, "bTRC": trc,
		"wtpt": xyzTag(pcsD50()),
	})
}

func primariesP3() [6]float64    { return [6]float64{0.680, 0.320, 0.265, 0.690, 0.150, 0.060} }
func primariesAdobe() [6]float64 { return [6]float64{0.64, 0.33, 0.21, 0.71, 0.15, 0.06} }

func displayP3ICC(t testing.TB) []byte { return rgbICC(t, primariesP3(), whiteD65(), srgbPara()) }

// expectSRGB converts a color with a published matrix to linear sRGB, as
// a reference independent of the code under test.
func expectSRGB(c [3]uint8, dec func(float64) float64, m [9]float64) [3]uint8 {
	var lin [3]float64
	for i := range 3 {
		lin[i] = dec(float64(c[i]) / 255)
	}
	var out [3]uint8
	for i := range 3 {
		v := m[3*i]*lin[0] + m[3*i+1]*lin[1] + m[3*i+2]*lin[2]
		v = math.Max(0, math.Min(1, v))
		if v <= 0.0031308 {
			v *= 12.92
		} else {
			v = 1.055*math.Pow(v, 1/2.4) - 0.055
		}
		out[i] = uint8(math.Round(v * 255))
	}
	return out
}

// The published matrices from linear Display P3 and Adobe RGB to linear
// sRGB.
func p3ToSRGB() [9]float64 {
	return [9]float64{1.2249401, -0.2249404, 0, -0.0420569, 1.0420571, 0, -0.0196376, -0.0786361, 1.0982735}
}
func adobeToSRGB() [9]float64 {
	return [9]float64{1.3982, -0.3982, 0, 0, 1, 0, 0, -0.0429, 1.0429}
}

func closeRGB(a, b [3]uint8, tol int) bool {
	for i := range 3 {
		if math.Abs(float64(a[i])-float64(b[i])) > float64(tol) {
			return false
		}
	}
	return true
}

func convert(tr *transform, c [3]uint8) [3]uint8 {
	r, g, b := tr.pixel(c[0], c[1], c[2])
	return [3]uint8{r, g, b}
}

// testColors are colors of every kind: primaries, grays, skin tones,
// saturated and dark ones.
func testColors() [][3]uint8 {
	return [][3]uint8{{255, 0, 0}, {0, 255, 0}, {0, 0, 255}, {128, 128, 128}, {255, 255, 255}, {0, 0, 0},
		{200, 150, 120}, {30, 200, 90}, {250, 220, 30}, {12, 8, 40}, {100, 150, 200}, {180, 40, 160}}
}

func TestColorDisplayP3(t *testing.T) {
	p := parseICC(displayP3ICC(t))
	require.NotNil(t, p)
	tr := newTransform(p)
	require.NotNil(t, tr, "Display P3 is not sRGB")
	for _, c := range testColors() {
		want := expectSRGB(c, srgbDecode, p3ToSRGB())
		assert.True(t, closeRGB(want, convert(tr, c), 1), "P3 %v: %v, want %v", c, convert(tr, c), want)
	}
	assert.Equal(t, [3]uint8{255, 0, 0}, convert(tr, [3]uint8{255, 0, 0}), "P3 red is clipped to sRGB red")
	assert.Equal(t, [3]uint8{128, 128, 128}, convert(tr, [3]uint8{128, 128, 128}), "a gray stays gray")
	assert.Equal(t, [3]uint8{255, 255, 255}, convert(tr, [3]uint8{255, 255, 255}))
	r := convert(tr, [3]uint8{200, 60, 40})
	assert.Greater(t, r[0], uint8(215), "a P3 red is redder than the same numbers in sRGB")

	// The same color space from nclx code points (12: P3-D65, 13: sRGB
	// transfer), and from a PNG cHRM.
	nclx := newTransform(sourceProfile(colorInfo{hasCICP: true, primaries: 12, transfer: 13}))
	require.NotNil(t, nclx)
	chrm := newTransform(sourceProfile(colorInfo{hasCHRM: true, chrm: [8]float64{0.3127, 0.3290, 0.680, 0.320, 0.265, 0.690, 0.150, 0.060}}))
	require.NotNil(t, chrm)
	for _, c := range testColors() {
		assert.True(t, closeRGB(convert(tr, c), convert(nclx, c), 1), "nclx %v", c)
		assert.True(t, closeRGB(convert(tr, c), convert(chrm, c), 1), "cHRM %v", c)
	}
}

func TestColorAdobeRGB(t *testing.T) {
	gamma := uint16(563) // 2.19921875 in u8Fixed8, as the Adobe profile has it
	tr := newTransform(parseICC(rgbICC(t, primariesAdobe(), whiteD65(), curvTag(gamma))))
	require.NotNil(t, tr)
	dec := func(v float64) float64 { return math.Pow(v, 563.0/256) }
	for _, c := range testColors() {
		want := expectSRGB(c, dec, adobeToSRGB())
		assert.True(t, closeRGB(want, convert(tr, c), 1), "Adobe RGB %v: %v, want %v", c, convert(tr, c), want)
	}
}

func TestColorSRGBSkipped(t *testing.T) {
	// sRGB profiles: parametric, a 1024-entry table, and a v2 one whose
	// colorants are rounded differently.
	table := make([]uint16, 1024)
	for i := range table {
		table[i] = uint16(math.Round(srgbDecode(float64(i)/1023) * 65535))
	}
	for name, icc := range map[string][]byte{
		"para":  rgbICC(t, primariesSRGB(), whiteD65(), srgbPara()),
		"table": rgbICC(t, primariesSRGB(), whiteD65(), curvTag(table...)),
		"gray":  iccBytes("GRAY", map[string][]byte{"kTRC": srgbPara()}),
	} {
		p := parseICC(icc)
		require.NotNil(t, p, name)
		assert.Nil(t, newTransform(p), "%s: an sRGB profile converts nothing", name)
	}
	for _, c := range []colorInfo{
		{},                               // untagged
		{srgb: true, gamma: 1 / 0.45455}, // PNG sRGB
		{gamma: 1 / 0.45455},             // PNG gAMA of 1/2.2
		{hasCICP: true, primaries: 1, transfer: 13},
		{hasCICP: true, primaries: 1, transfer: 1},
		{hasCICP: true, primaries: 2, transfer: 2}, // unspecified
	} {
		assert.Nil(t, newTransform(sourceProfile(c)), "%+v", c)
	}
	// A gray profile of gamma 1.8 is converted, gray to gray.
	tr := newTransform(parseICC(iccBytes("GRAY", map[string][]byte{"kTRC": curvTag(0x01cd)})))
	require.NotNil(t, tr)
	g := convert(tr, [3]uint8{100, 100, 100})
	assert.Equal(t, g[0], g[1])
	assert.Equal(t, g[0], g[2])
	assert.Greater(t, g[0], uint8(110), "gamma 1.8 is lighter than sRGB")
	// Linear PNG (gAMA 1.0): a mid sample is lighter in sRGB.
	lin := newTransform(sourceProfile(colorInfo{gamma: 1}))
	require.NotNil(t, lin)
	assert.Equal(t, [3]uint8{188, 188, 188}, convert(lin, [3]uint8{128, 128, 128}))
}

func TestColorCurves(t *testing.T) {
	approx := func(want, got float64) bool { return math.Abs(want-got) < 2e-4 }
	cases := []struct {
		name string
		tag  []byte
		want func(float64) float64
	}{
		{"curv identity", curvTag(), func(x float64) float64 { return x }},
		{"curv gamma", curvTag(0x0233), func(x float64) float64 { return math.Pow(x, 563.0/256) }},
		{"curv table", curvTag(0, 0x8000, 0xffff), func(x float64) float64 { return x }},
		{"para 0", paraTag(0, 2.2), func(x float64) float64 { return math.Pow(x, 2.2) }},
		{"para 1", paraTag(1, 2, 2, -0.5), func(x float64) float64 {
			if x < 0.25 {
				return 0
			}
			return (2*x - 0.5) * (2*x - 0.5)
		}},
		{"para 2", paraTag(2, 2, 2, -0.5, 0.1), func(x float64) float64 {
			if x < 0.25 {
				return 0.1
			}
			return (2*x-0.5)*(2*x-0.5) + 0.1
		}},
		{"para 3 (sRGB)", srgbPara(), srgbDecode},
		{"para 4", paraTag(4, 2.4, 1/1.055, 0.055/1.055, 1/12.92, 0.04045, 0.01, 0.02), func(x float64) float64 {
			if x < 0.04045 {
				return x/12.92 + 0.02
			}
			return math.Pow((x+0.055)/1.055, 2.4) + 0.01
		}},
	}
	for _, c := range cases {
		f := parseCurve(c.tag)
		require.NotNil(t, f, c.name)
		for _, x := range []float64{0, 0.02, 0.1, 0.25, 0.3, 0.5, 0.75, 1} {
			assert.True(t, approx(c.want(x), f(x)), "%s(%v) = %v, want %v", c.name, x, f(x), c.want(x))
		}
	}
	for _, bad := range [][]byte{nil, []byte("curv"), paraTag(5, 1), paraTag(3, 2.4), []byte("curv\x00\x00\x00\x00\x00\x00\x00\x09\x00"), []byte("sf32\x00\x00\x00\x00\x00\x00\x00\x00")} {
		assert.Nil(t, parseCurve(bad), "%q", bad)
	}
}

// TestColorMalformed: truncated or corrupt profiles are ignored (the
// picture is read as sRGB), and never crash.
func TestColorMalformed(t *testing.T) {
	icc := displayP3ICC(t)
	for n := range len(icc) {
		p := parseICC(icc[:n])
		if p != nil {
			// A profile truncated after the tags it uses still works.
			assert.NotNil(t, newTransform(p))
		}
	}
	for i := range len(icc) {
		bad := bytes.Clone(icc)
		bad[i] ^= 0xff
		if p := parseICC(bad); p != nil {
			newTransform(p).apply(quadrants(4, 4))
		}
	}
	cmyk := bytes.Clone(icc)
	copy(cmyk[16:], "CMYK")
	assert.Nil(t, parseICC(cmyk), "not an RGB profile")
	lab := bytes.Clone(icc)
	copy(lab[20:], "Lab ")
	assert.Nil(t, parseICC(lab), "a Lab connection space has no matrix")
	huge := iccBytes("RGB ", map[string][]byte{
		"rXYZ": xyzTag([3]float64{3000, 0, 0}), "gXYZ": xyzTag([3]float64{0, 1, 0}), "bXYZ": xyzTag([3]float64{0, 0, 1}),
		"rTRC": srgbPara(), "gTRC": srgbPara(), "bTRC": srgbPara(),
	})
	assert.Nil(t, parseICC(huge), "absurd colorants")
	assert.Nil(t, parseICC(bytes.Repeat([]byte{0xff}, 4096)))

	// A JPEG with a corrupt profile decodes as sRGB.
	src := encodeJPEG(t, quadrants(64, 64), 95)
	im, err := Decode(withICC(t, src, icc[:200], 1), Limits{})
	require.NoError(t, err)
	assert.NotNil(t, im.ycc, "kept in YCbCr: no usable profile")
	// A profile missing one of its chunks too.
	data := withICC(t, src, icc, 3)
	segs := bytes.Index(data, []byte("ICC_PROFILE\x00\x02\x03"))
	require.Positive(t, segs)
	data[segs+12] = 9 // the second chunk is numbered 9 of 3
	im, err = Decode(data, Limits{})
	require.NoError(t, err)
	assert.NotNil(t, im.ycc)
}

// withICC inserts a profile in chunks APP2 segments after the SOI of a
// JPEG file.
func withICC(t testing.TB, jpg, icc []byte, chunks int) []byte {
	t.Helper()
	out := []byte{0xff, 0xd8}
	size := (len(icc) + chunks - 1) / chunks
	for i := range chunks {
		part := icc[min(len(icc), i*size):min(len(icc), (i+1)*size)]
		payload := append([]byte("ICC_PROFILE\x00"), byte(i+1), byte(chunks)) // #nosec G115 -- small
		payload = append(payload, part...)
		out = append(out, 0xff, 0xe2)
		out = binary.BigEndian.AppendUint16(out, uint16(len(payload)+2)) // #nosec G115 -- small
		out = append(out, payload...)
	}
	return append(out, jpg[2:]...)
}

// pngChunk inserts a chunk after the IHDR of a PNG file.
func pngChunk(data []byte, typ string, body []byte) []byte {
	c := binary.BigEndian.AppendUint32(nil, uint32(len(body))) // #nosec G115 -- small
	c = append(c, typ...)
	c = append(c, body...)
	c = binary.BigEndian.AppendUint32(c, crc32.ChecksumIEEE(append([]byte(typ), body...)))
	const afterIHDR = 8 + 12 + 13
	return slices.Concat(data[:afterIHDR], c, data[afterIHDR:])
}

// webpExtended turns a simple (opaque) WebP file into an extended
// one with an ICC profile and EXIF.
func webpExtended(t testing.TB, simple []byte, w, h int, icc, exif []byte) []byte {
	t.Helper()
	require.Contains(t, []string{"VP8 ", "VP8L"}, string(simple[12:16]))
	chunk := func(typ string, body []byte) []byte {
		c := append([]byte(typ), binary.LittleEndian.AppendUint32(nil, uint32(len(body)))...) // #nosec G115 -- small
		c = append(c, body...)
		if len(body)%2 == 1 {
			c = append(c, 0)
		}
		return c
	}
	var flags byte
	if icc != nil {
		flags |= 0x20
	}
	if exif != nil {
		flags |= 0x08
	}
	vp8x := []byte{flags, 0, 0, 0, byte(w - 1), byte((w - 1) >> 8), byte((w - 1) >> 16), byte(h - 1), byte((h - 1) >> 8), byte((h - 1) >> 16)} // #nosec G115 -- small
	body := append([]byte("WEBP"), chunk("VP8X", vp8x)...)
	if icc != nil {
		body = append(body, chunk("ICCP", icc)...)
	}
	body = append(body, simple[12:]...)
	if exif != nil {
		body = append(body, chunk("EXIF", exif)...)
	}
	return append(append([]byte("RIFF"), binary.LittleEndian.AppendUint32(nil, uint32(len(body)))...), body...) // #nosec G115 -- small
}

// flat returns a picture of one color.
func flat(w, h int, c color.NRGBA) *image.NRGBA {
	m := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < len(m.Pix); i += 4 {
		m.Pix[i], m.Pix[i+1], m.Pix[i+2], m.Pix[i+3] = c.R, c.G, c.B, c.A
	}
	return m
}

// TestColorEndToEnd decodes sources of every format tagged Display P3 in
// the ways the format allows: their pictures come out in sRGB.
func TestColorEndToEnd(t *testing.T) {
	p3 := [3]uint8{200, 60, 40}
	want := expectSRGB(p3, srgbDecode, p3ToSRGB())
	src := flat(64, 48, color.NRGBA{p3[0], p3[1], p3[2], 255})
	icc := displayP3ICC(t)

	var pngData bytes.Buffer
	require.NoError(t, png.Encode(&pngData, src))
	var zicc bytes.Buffer
	zw := zlib.NewWriter(&zicc)
	_, _ = zw.Write(icc)
	require.NoError(t, zw.Close())
	var webpData, heicData, avifData bytes.Buffer
	require.NoError(t, gwebp.Encode(&webpData, src, gwebp.Options{Lossless: true}))
	require.NoError(t, heic.Encode(&heicData, src, heic.EncodeOptions{Quality: 95, ICC: icc}))
	require.NoError(t, avif.Encode(&avifData, src, avif.Options{Quality: 100, Speed: 10}))

	cases := map[string][]byte{
		"jpeg (3 ICC segments)": withICC(t, encodeJPEG(t, src, 100), icc, 3),
		"png iCCP":              pngChunk(pngData.Bytes(), "iCCP", append([]byte("Display P3\x00\x00"), zicc.Bytes()...)),
		"png cICP":              pngChunk(pngData.Bytes(), "cICP", []byte{12, 13, 0, 1}),
		"webp ICCP":             webpExtended(t, webpData.Bytes(), 64, 48, icc, nil),
		"heic prof":             heicData.Bytes(),
	}
	avifP3, ok := patchNCLX(avifData.Bytes(), 12, 13)
	require.True(t, ok, "the AVIF encoder writes an nclx box")
	cases["avif nclx"] = avifP3
	for name, data := range cases {
		im, err := Decode(data, Limits{})
		require.NoError(t, err, name)
		require.Nil(t, im.ycc, name)
		c := im.picture(Size{PixelWidth: 64, PixelHeight: 48}).RGBAAt(32, 24)
		assert.True(t, closeRGB(want, [3]uint8{c.R, c.G, c.B}, 6), "%s: %v, want %v", name, c, want)
	}
	// Untagged, the same file keeps its samples.
	im, err := Decode(encodeJPEG(t, src, 100), Limits{})
	require.NoError(t, err)
	require.NotNil(t, im.ycc)
	c := im.picture(Size{PixelWidth: 64, PixelHeight: 48}).RGBAAt(32, 24)
	assert.True(t, closeRGB(p3, [3]uint8{c.R, c.G, c.B}, 3), "%v", c)
}

// patchNCLX sets the primaries and the transfer of the nclx box of an
// HEIF file.
func patchNCLX(data []byte, primaries, transfer uint16) ([]byte, bool) {
	i := bytes.Index(data, []byte("colrnclx"))
	if i < 0 || i+12 > len(data) {
		return nil, false
	}
	out := bytes.Clone(data)
	binary.BigEndian.PutUint16(out[i+8:], primaries)
	binary.BigEndian.PutUint16(out[i+10:], transfer)
	return out, true
}

// TestColorAlpha: translucent pixels are converted on their straight
// color, the alpha kept.
func TestColorAlpha(t *testing.T) {
	tr := newTransform(parseICC(displayP3ICC(t)))
	require.NotNil(t, tr)
	m := image.NewRGBA(image.Rect(0, 0, 2, 1))
	m.Pix = []uint8{100, 30, 20, 128, 0, 0, 0, 0} // straight (200, 60, 40) at half alpha
	tr.apply(m)
	want := expectSRGB([3]uint8{200, 60, 40}, srgbDecode, p3ToSRGB())
	straight := [3]uint8{}
	for i := range 3 {
		straight[i] = uint8(min(255, (int(m.Pix[i])*255+64)/128)) // #nosec G115 -- at most 255
	}
	assert.True(t, closeRGB(want, straight, 3), "%v, want %v", straight, want)
	assert.Equal(t, uint8(128), m.Pix[3])
	assert.Equal(t, []uint8{0, 0, 0, 0}, m.Pix[4:])
}

func TestISOBMFFColor(t *testing.T) {
	box := func(typ string, payload ...[]byte) []byte {
		body := slices.Concat(payload...)
		return slices.Concat(binary.BigEndian.AppendUint32(nil, uint32(8+len(body))), []byte(typ), body) // #nosec G115 -- small
	}
	nclx := box("colr", []byte("nclx"), []byte{0, 9, 0, 16, 0, 9, 0x80})
	prof := box("colr", []byte("prof"), []byte("an ICC profile"))
	ispe := box("ispe", make([]byte, 12))
	// Item 1 has ispe and prof; item 2 (the primary) ispe and nclx.
	ipma := box("ipma", []byte{0, 0, 0, 0, 0, 0, 0, 2}, []byte{0, 1, 2, 0x81, 0x02}, []byte{0, 2, 2, 0x81, 0x03})
	meta := box("meta", []byte{0, 0, 0, 0}, box("hdlr", make([]byte, 20)), box("pitm", []byte{0, 0, 0, 0, 0, 2}),
		box("iprp", box("ipco", ispe, prof, nclx), ipma))
	file := slices.Concat(box("ftyp", []byte("avif\x00\x00\x00\x00mif1")), meta, box("mdat", []byte{1, 2, 3}))
	c := isobmffColor(file)
	assert.True(t, c.hasCICP)
	assert.Equal(t, uint16(9), c.primaries)
	assert.Equal(t, uint16(16), c.transfer)
	assert.Nil(t, c.icc, "the profile is another item's")
	// PQ BT.2020: converted, the reference white kept below the clip.
	tr := newTransform(sourceProfile(c))
	require.NotNil(t, tr)
	for n := range len(file) {
		isobmffColor(file[:n]) // no crash
	}
}

func TestTIFFICC(t *testing.T) {
	icc := displayP3ICC(t)
	var b bytes.Buffer
	b.WriteString("II*\x00")
	_ = binary.Write(&b, binary.LittleEndian, uint32(8))
	_ = binary.Write(&b, binary.LittleEndian, uint16(1))
	_ = binary.Write(&b, binary.LittleEndian, []uint16{tagICCProfile, 7})
	_ = binary.Write(&b, binary.LittleEndian, []uint32{uint32(len(icc)), 26}) // #nosec G115 -- small
	_ = binary.Write(&b, binary.LittleEndian, uint32(0))
	b.Write(icc)
	assert.Equal(t, icc, tiffICC(b.Bytes()))
	assert.Nil(t, tiffICC(b.Bytes()[:100]), "truncated")
	assert.Nil(t, tiffICC(exifTIFF(1)))
}

// TestPNGColorPriority: cICP comes before iCCP, iCCP before sRGB.
func TestPNGColorPriority(t *testing.T) {
	var b bytes.Buffer
	require.NoError(t, png.Encode(&b, quadrants(8, 8)))
	var zicc bytes.Buffer
	zw := zlib.NewWriter(&zicc)
	_, _ = zw.Write(displayP3ICC(t))
	require.NoError(t, zw.Close())
	iccp := append([]byte("P3\x00\x00"), zicc.Bytes()...)
	both := pngChunk(pngChunk(b.Bytes(), "iCCP", iccp), "cICP", []byte{9, 13, 0, 1})
	c := pngMeta(both).color
	assert.True(t, c.hasCICP)
	assert.Nil(t, c.icc)
	c = pngMeta(pngChunk(pngChunk(b.Bytes(), "iCCP", iccp), "sRGB", []byte{0})).color
	assert.NotNil(t, newTransform(sourceProfile(c)), "the profile, not sRGB")
	assert.Nil(t, pngICC([]byte("P3\x00\x00not zlib")))
}
