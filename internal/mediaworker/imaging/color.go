package imaging

import (
	"encoding/binary"
	"image"
	"math"
)

// Color management: every picture is converted to sRGB once decoded,
// before it is resized, so that the variants (untagged, which every viewer
// reads as sRGB) show the colors of the source. A source is described by
// an ICC profile of the matrix/TRC kind (sRGB, Display P3, Adobe RGB,
// ProPhoto, Rec. 2020: three colorants and three tone curves; gray
// profiles too), by the H.273 code points of an nclx box or a PNG cICP
// chunk, or by the gAMA and cHRM chunks of a PNG. A profile it cannot use
// (a LUT-based or CMYK profile, a malformed one) leaves the picture as it
// is, read as sRGB.
//
// The conversion is a lookup table per channel from the 8-bit samples to
// linear light (16 bits), a 3x3 matrix in fixed point to linear sRGB
// (from the D50 connection space of ICC with the Bradford adaptation),
// clipped to the sRGB gamut, and a 64 Ki lookup table back to 8-bit sRGB.

// mat3 is a 3x3 matrix, row-major.
type mat3 [9]float64

func (a mat3) mul(b mat3) mat3 {
	var c mat3
	for i := range 3 {
		for j := range 3 {
			c[3*i+j] = a[3*i]*b[j] + a[3*i+1]*b[3+j] + a[3*i+2]*b[6+j]
		}
	}
	return c
}

func (a mat3) vec(v [3]float64) [3]float64 {
	return [3]float64{
		a[0]*v[0] + a[1]*v[1] + a[2]*v[2],
		a[3]*v[0] + a[4]*v[1] + a[5]*v[2],
		a[6]*v[0] + a[7]*v[1] + a[8]*v[2],
	}
}

// inv returns the inverse of a matrix; ok is false when it is singular.
func (a mat3) inv() (mat3, bool) {
	c := mat3{
		a[4]*a[8] - a[5]*a[7], a[2]*a[7] - a[1]*a[8], a[1]*a[5] - a[2]*a[4],
		a[5]*a[6] - a[3]*a[8], a[0]*a[8] - a[2]*a[6], a[2]*a[3] - a[0]*a[5],
		a[3]*a[7] - a[4]*a[6], a[1]*a[6] - a[0]*a[7], a[0]*a[4] - a[1]*a[3],
	}
	det := a[0]*c[0] + a[1]*c[3] + a[2]*c[6]
	if math.Abs(det) < 1e-12 || math.IsNaN(det) || math.IsInf(det, 0) {
		return mat3{}, false
	}
	for i := range c {
		c[i] /= det
	}
	return c, true
}

func identity3() mat3 { return mat3{1, 0, 0, 0, 1, 0, 0, 0, 1} }

// xyz returns the XYZ (Y = 1) of a chromaticity.
func xyz(x, y float64) [3]float64 { return [3]float64{x / y, 1, (1 - x - y) / y} }

// White points.
func whiteD65() [2]float64      { return [2]float64{0.3127, 0.3290} }
func whiteDCI() [2]float64      { return [2]float64{0.314, 0.351} }
func pcsD50() [3]float64        { return [3]float64{0.9642, 1, 0.8249} }
func primariesSRGB() [6]float64 { return [6]float64{0.64, 0.33, 0.30, 0.60, 0.15, 0.06} }

// rgbToXYZ returns the matrix from linear RGB of some primaries (red,
// green and blue x, y) and white point to XYZ.
func rgbToXYZ(p [6]float64, w [2]float64) (mat3, bool) {
	r, g, b := xyz(p[0], p[1]), xyz(p[2], p[3]), xyz(p[4], p[5])
	m := mat3{r[0], g[0], b[0], r[1], g[1], b[1], r[2], g[2], b[2]}
	mi, ok := m.inv()
	if !ok {
		return mat3{}, false
	}
	s := mi.vec(xyz(w[0], w[1]))
	for i := range 3 {
		m[3*i] *= s[0]
		m[3*i+1] *= s[1]
		m[3*i+2] *= s[2]
	}
	return m, true
}

// bradford returns the chromatic adaptation from a white (XYZ) to
// another.
func bradford(from, to [3]float64) mat3 {
	b := mat3{0.8951, 0.2664, -0.1614, -0.7502, 1.7135, 0.0367, 0.0389, -0.0685, 1.0296}
	bi, _ := b.inv()
	f, t := b.vec(from), b.vec(to)
	d := mat3{t[0] / f[0], 0, 0, 0, t[1] / f[1], 0, 0, 0, t[2] / f[2]}
	return bi.mul(d).mul(b)
}

// xyzD65ToSRGB returns the matrix from XYZ (D65) to linear sRGB.
func xyzD65ToSRGB() mat3 {
	m, _ := rgbToXYZ(primariesSRGB(), whiteD65())
	mi, _ := m.inv()
	return mi
}

// xyzD50ToSRGB returns the matrix from the XYZ of the ICC connection space
// (D50) to linear sRGB.
func xyzD50ToSRGB() mat3 {
	w := whiteD65()
	return xyzD65ToSRGB().mul(bradford(pcsD50(), xyz(w[0], w[1])))
}

// curve is a tone curve: from an encoded sample (0 to 1) to linear light.
type curve func(float64) float64

func srgbDecode(v float64) float64 {
	if v <= 0.04045 {
		return v / 12.92
	}
	return math.Pow((v+0.055)/1.055, 2.4)
}

func gammaCurve(g float64) curve { return func(v float64) float64 { return math.Pow(v, g) } }

// hdrKnee compresses the linear light of an HDR source (1 = the reference
// white, more for the highlights) into 0 to 1: unchanged up to 0.6, then
// smoothly towards 1.
func hdrKnee(y float64) float64 {
	const k = 0.6
	if y <= k {
		return y
	}
	return k + (1-k)*(1-math.Exp(-(y-k)/(1-k)))
}

// pqCurve is the PQ (SMPTE ST 2084) transfer, its 203 cd/m² reference
// white mapped to 1 and the highlights compressed (a simple tone mapping).
func pqCurve(v float64) float64 {
	const m1, m2 = 2610.0 / 16384, 2523.0 / 4096 * 128
	const c1, c2, c3 = 3424.0 / 4096, 2413.0 / 4096 * 32, 2392.0 / 4096 * 32
	p := math.Pow(v, 1/m2)
	l := math.Pow(math.Max(p-c1, 0)/(c2-c3*p), 1/m1)
	return hdrKnee(l * 10000 / 203)
}

// hlgCurve is the HLG (ARIB STD-B67) transfer with the system gamma of a
// 1000 cd/m² display, its 203 cd/m² reference white mapped to 1 and the
// highlights compressed.
func hlgCurve(v float64) float64 {
	const a = 0.17883277
	const b = 1 - 4*a
	c := 0.5 - a*math.Log(4*a)
	var e float64
	if v <= 0.5 {
		e = v * v / 3
	} else {
		e = (math.Exp((v-c)/a) + b) / 12
	}
	return hdrKnee(math.Pow(e, 1.2) * 1000 / 203)
}

// profile is a color space a picture is converted from: the tone curves
// of its channels and the matrix from their linear values to linear sRGB.
type profile struct {
	trc    [3]curve
	toSRGB mat3
}

// cicpPrimaries returns the primaries and the white point of an H.273
// ColourPrimaries code point; ok is false for an unknown one.
func cicpPrimaries(code uint16) (p [6]float64, w [2]float64, ok bool) {
	switch code {
	case 1: // BT.709, sRGB
		return primariesSRGB(), whiteD65(), true
	case 5: // BT.470 B/G, BT.601 625
		return [6]float64{0.64, 0.33, 0.29, 0.60, 0.15, 0.06}, whiteD65(), true
	case 6, 7: // BT.601 525, SMPTE 240M
		return [6]float64{0.630, 0.340, 0.310, 0.595, 0.155, 0.070}, whiteD65(), true
	case 9: // BT.2020, BT.2100
		return [6]float64{0.708, 0.292, 0.170, 0.797, 0.131, 0.046}, whiteD65(), true
	case 11: // DCI-P3
		return [6]float64{0.680, 0.320, 0.265, 0.690, 0.150, 0.060}, whiteDCI(), true
	case 12: // Display P3
		return [6]float64{0.680, 0.320, 0.265, 0.690, 0.150, 0.060}, whiteD65(), true
	}
	return p, w, false
}

// cicpTransfer returns the tone curve of an H.273 TransferCharacteristics
// code point. The BT.709 family (1, 6, 14, 15) is read as sRGB, as the
// browsers show still images; unknown and unspecified ones too.
func cicpTransfer(code uint16) curve {
	switch code {
	case 4:
		return gammaCurve(2.2)
	case 5:
		return gammaCurve(2.8)
	case 8:
		return func(v float64) float64 { return v }
	case 16:
		return pqCurve
	case 18:
		return hlgCurve
	}
	return srgbDecode
}

// primariesProfile returns the profile of linear RGB of some primaries
// and white point, with a tone curve.
func primariesProfile(p [6]float64, w [2]float64, trc curve) *profile {
	m, ok := rgbToXYZ(p, w)
	if !ok {
		return nil
	}
	d65 := whiteD65()
	m = xyzD65ToSRGB().mul(bradford(xyz(w[0], w[1]), xyz(d65[0], d65[1]))).mul(m)
	return &profile{trc: [3]curve{trc, trc, trc}, toSRGB: m}
}

// sourceProfile returns the profile of a picture from what its source
// declares: its ICC profile when usable, else its H.273 code points, else
// the gAMA and cHRM of a PNG; nil for sRGB and for what is unknown.
func sourceProfile(c colorInfo) *profile {
	if c.icc != nil {
		if p := parseICC(c.icc); p != nil {
			return p
		}
	}
	switch {
	case c.hasCICP:
		p, w, ok := cicpPrimaries(c.primaries)
		if !ok {
			p, w = primariesSRGB(), whiteD65()
		}
		return primariesProfile(p, w, cicpTransfer(c.transfer))
	case c.srgb:
		return nil
	case c.gamma > 0 || c.hasCHRM:
		trc := curve(srgbDecode)
		if c.gamma > 0 && math.Abs(c.gamma-2.2) > 0.05 && c.gamma >= 0.2 && c.gamma <= 5 {
			trc = gammaCurve(c.gamma)
		}
		p, w := primariesSRGB(), whiteD65()
		if c.hasCHRM {
			w = [2]float64{c.chrm[0], c.chrm[1]}
			copy(p[:], c.chrm[2:])
		}
		return primariesProfile(p, w, trc)
	}
	return nil
}

// parseICC returns the profile of an ICC profile of the matrix/TRC kind
// (RGB, or gray), version 2 or 4; nil when the profile is of another kind
// or malformed.
func parseICC(b []byte) (p *profile) {
	defer func() {
		if recover() != nil {
			p = nil
		}
	}()
	if len(b) < 132 {
		return nil
	}
	if size := binary.BigEndian.Uint32(b); size >= 132 && int64(size) < int64(len(b)) {
		b = b[:size]
	}
	if string(b[36:40]) != "acsp" || string(b[20:24]) != "XYZ " {
		return nil
	}
	space := string(b[16:20])
	n := int64(binary.BigEndian.Uint32(b[128:]))
	if n > int64(len(b)-132)/12 {
		return nil
	}
	tags := map[string][]byte{}
	for i := range n {
		e := 132 + 12*i
		off, size := int64(binary.BigEndian.Uint32(b[e+4:])), int64(binary.BigEndian.Uint32(b[e+8:]))
		if off < 128 || off+size > int64(len(b)) || size < 8 {
			continue
		}
		tags[string(b[e:e+4])] = b[off : off+size]
	}
	switch space {
	case "GRAY":
		k := parseCurve(tags["kTRC"])
		if k == nil {
			return nil
		}
		return &profile{trc: [3]curve{k, k, k}, toSRGB: identity3()}
	case "RGB ":
	default:
		return nil
	}
	var m mat3
	for j, sig := range []string{"rXYZ", "gXYZ", "bXYZ"} {
		v, ok := parseXYZ(tags[sig])
		if !ok {
			return nil
		}
		m[j], m[3+j], m[6+j] = v[0], v[1], v[2]
	}
	p = &profile{toSRGB: xyzD50ToSRGB().mul(m)}
	for j, sig := range []string{"rTRC", "gTRC", "bTRC"} {
		if p.trc[j] = parseCurve(tags[sig]); p.trc[j] == nil {
			return nil
		}
	}
	for _, v := range p.toSRGB {
		if math.IsNaN(v) || math.Abs(v) > 8 {
			return nil
		}
	}
	return p
}

// s15f16 reads an ICC s15Fixed16Number.
func s15f16(b []byte) float64 { return float64(int32(binary.BigEndian.Uint32(b))) / 65536 } // #nosec G115 -- a signed field

// parseXYZ reads an XYZType tag.
func parseXYZ(t []byte) ([3]float64, bool) {
	if len(t) < 20 || string(t[:4]) != "XYZ " {
		return [3]float64{}, false
	}
	return [3]float64{s15f16(t[8:]), s15f16(t[12:]), s15f16(t[16:])}, true
}

// parseCurve reads a curveType (identity, gamma or table) or a
// parametricCurveType (functions 0 to 4) tag; nil when malformed.
func parseCurve(t []byte) curve {
	if len(t) < 12 {
		return nil
	}
	switch string(t[:4]) {
	case "curv":
		n := int64(binary.BigEndian.Uint32(t[8:]))
		if 12+2*n > int64(len(t)) {
			return nil
		}
		switch n {
		case 0:
			return func(v float64) float64 { return v }
		case 1:
			g := float64(binary.BigEndian.Uint16(t[12:])) / 256
			if g <= 0 {
				return nil
			}
			return gammaCurve(g)
		}
		table := make([]float64, n)
		for i := range table {
			table[i] = float64(binary.BigEndian.Uint16(t[12+2*i:])) / 65535
		}
		return func(v float64) float64 {
			x := v * float64(len(table)-1)
			i := min(int(x), len(table)-2)
			f := x - float64(i)
			return table[i]*(1-f) + table[i+1]*f
		}
	case "para":
		fn := binary.BigEndian.Uint16(t[8:])
		count := map[uint16]int{0: 1, 1: 3, 2: 4, 3: 5, 4: 7}
		k, ok := count[fn]
		if !ok || len(t) < 12+4*k {
			return nil
		}
		var g [7]float64
		for i := range k {
			g[i] = s15f16(t[12+4*i:])
		}
		gamma, a, b, c, d, e, f := g[0], g[1], g[2], g[3], g[4], g[5], g[6]
		pow := func(x float64) float64 { return math.Pow(math.Max(x, 0), gamma) }
		switch fn {
		case 0:
			return pow
		case 1:
			return func(x float64) float64 {
				if a == 0 || x < -b/a {
					return 0
				}
				return pow(a*x + b)
			}
		case 2:
			return func(x float64) float64 {
				if a == 0 || x < -b/a {
					return c
				}
				return pow(a*x+b) + c
			}
		case 3:
			return func(x float64) float64 {
				if x < d {
					return c * x
				}
				return pow(a*x + b)
			}
		default:
			return func(x float64) float64 {
				if x < d {
					return c*x + f
				}
				return pow(a*x+b) + e
			}
		}
	}
	return nil
}

// Fixed point of the conversion: linear light on 16 bits, the matrix on
// 14 fractional bits.
const (
	linearMax  = 65535
	matrixBits = 14
)

// transform converts 8-bit samples of a profile to 8-bit sRGB.
type transform struct {
	dec [3][256]int64 // the samples of each channel to linear light
	m   [9]int64      // linear to linear sRGB
	enc []uint8       // linear sRGB to its 8-bit samples (linearMax+1)
}

// newTransform returns the conversion of a profile to sRGB, nil when
// there is nothing to convert: no profile, or one that gives the same
// samples (an sRGB profile, within one level).
func newTransform(p *profile) *transform {
	if p == nil {
		return nil
	}
	t := &transform{enc: make([]uint8, linearMax+1)}
	for c := range 3 {
		for v := range 256 {
			y := p.trc[c](float64(v) / 255)
			if math.IsNaN(y) {
				y = 0
			}
			t.dec[c][v] = int64(math.Round(math.Max(0, math.Min(1, y)) * linearMax))
		}
	}
	for i, v := range p.toSRGB {
		t.m[i] = int64(math.Round(v * (1 << matrixBits)))
	}
	// enc[l] is the 8-bit sample whose interval (v ± 0.5) holds the sRGB
	// encoding of l: the thresholds are the decoded half levels.
	lo := 0
	for v := 1; v <= 256; v++ {
		hi := linearMax + 1
		if v < 256 {
			hi = int(math.Ceil(srgbDecode((float64(v)-0.5)/255) * linearMax))
		}
		for l := lo; l < hi; l++ {
			t.enc[l] = uint8(v - 1) // #nosec G115 -- 0 to 255
		}
		lo = max(lo, hi)
	}
	if t.isIdentity() {
		return nil
	}
	return t
}

// isIdentity reports whether the conversion changes no sample by more
// than one level: every gray, and a cube of colors.
func (t *transform) isIdentity() bool {
	same := func(r, g, b uint8) bool {
		r2, g2, b2 := t.pixel(r, g, b)
		d := func(x, y uint8) bool { return int(x)-int(y) <= 1 && int(y)-int(x) <= 1 }
		return d(r, r2) && d(g, g2) && d(b, b2)
	}
	for v := range 256 {
		if !same(uint8(v), uint8(v), uint8(v)) { // #nosec G115 -- 0 to 255
			return false
		}
	}
	for r := 0; r < 256; r += 51 {
		for g := 0; g < 256; g += 51 {
			for b := 0; b < 256; b += 51 {
				if !same(uint8(r), uint8(g), uint8(b)) { // #nosec G115 -- 0 to 255
					return false
				}
			}
		}
	}
	return true
}

// pixel converts one color.
func (t *transform) pixel(r, g, b uint8) (uint8, uint8, uint8) {
	lr, lg, lb := t.dec[0][r], t.dec[1][g], t.dec[2][b]
	const half = 1 << (matrixBits - 1)
	clip := func(v int64) int64 { return min(linearMax, max(0, v>>matrixBits)) }
	m := &t.m
	return t.enc[clip(m[0]*lr+m[1]*lg+m[2]*lb+half)],
		t.enc[clip(m[3]*lr+m[4]*lg+m[5]*lb+half)],
		t.enc[clip(m[6]*lr+m[7]*lg+m[8]*lb+half)]
}

// apply converts a premultiplied RGBA picture (tight) in place: the
// colors of its translucent pixels are converted unpremultiplied.
func (t *transform) apply(m *image.RGBA) {
	w, h := m.Rect.Dx(), m.Rect.Dy()
	parallel(h, w*h, func(lo, hi int) {
		pix := m.Pix[lo*m.Stride : hi*m.Stride]
		for i := 0; i+4 <= len(pix); i += 4 {
			p := pix[i : i+4 : i+4]
			switch a := uint32(p[3]); a {
			case 0:
			case 0xff:
				p[0], p[1], p[2] = t.pixel(p[0], p[1], p[2])
			default:
				r, g, b := t.pixel(
					uint8(min(255, (uint32(p[0])*255+a/2)/a)), // #nosec G115 -- at most 255
					uint8(min(255, (uint32(p[1])*255+a/2)/a)), // #nosec G115 -- at most 255
					uint8(min(255, (uint32(p[2])*255+a/2)/a))) // #nosec G115 -- at most 255
				p[0] = uint8((uint32(r)*a + 127) / 255) // #nosec G115 -- at most 255
				p[1] = uint8((uint32(g)*a + 127) / 255) // #nosec G115 -- at most 255
				p[2] = uint8((uint32(b)*a + 127) / 255) // #nosec G115 -- at most 255
			}
		}
	})
}
