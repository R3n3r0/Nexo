package transcode

import (
	"context"
	"image"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// decodeAll decodes the pictures of a file, in display order.
func decodeAll(t *testing.T, path string) (*Source, []*image.YCbCr) {
	t.Helper()
	lib := library(t)
	src, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = src.Close() })
	dec, err := lib.NewDecoder()
	require.NoError(t, err)
	defer dec.Close()
	var pics []*image.YCbCr
	for _, s := range src.Video.Samples {
		au, err := src.accessUnit(s)
		require.NoError(t, err)
		pic, err := dec.Decode(au)
		require.NoError(t, err)
		if pic != nil {
			pics = append(pics, clonePicture(pic))
		}
	}
	rest, err := dec.Flush()
	require.NoError(t, err)
	for _, p := range rest {
		pics = append(pics, clonePicture(p))
	}
	return src, pics
}

func clonePicture(p *image.YCbCr) *image.YCbCr {
	c := image.NewYCbCr(p.Rect, p.SubsampleRatio)
	copy(c.Y, p.Y)
	copy(c.Cb, p.Cb)
	copy(c.Cr, p.Cr)
	return c
}

// lumaPSNR is the PSNR of the luma of b against a, in dB.
func lumaPSNR(a, b *image.YCbCr) float64 {
	var se float64
	w, h := a.Rect.Dx(), a.Rect.Dy()
	for y := range h {
		for x := range w {
			d := float64(a.Y[y*a.YStride+x]) - float64(b.Y[y*b.YStride+x])
			se += d * d
		}
	}
	mse := se / float64(w*h)
	if mse == 0 {
		return math.Inf(1)
	}
	return 10 * math.Log10(255*255/mse)
}

// TestForensicPattern: the pattern is smooth, faint, balanced and the
// same at every size.
//
// covers: S-OBJ-19
func TestForensicPattern(t *testing.T) {
	m := newForensicMark(42, 640, 360, ForensicStrength)
	var pos, neg, maxStep int
	for y := range 360 {
		for x := range 640 {
			v := int(m.field[y*640+x])
			require.LessOrEqual(t, math.Abs(float64(v)), ForensicStrength)
			if v > 0 {
				pos++
			} else if v < 0 {
				neg++
			}
			if x > 0 {
				maxStep = max(maxStep, int(math.Abs(float64(v-int(m.field[y*640+x-1])))))
			}
		}
	}
	assert.LessOrEqual(t, maxStep, 1, "smooth: neighbours differ by a level at most")
	assert.InDelta(t, 1.0, float64(pos)/float64(neg), 0.6, "about as many raised as lowered pixels")
	small := newForensicMark(42, 320, 180, ForensicStrength)
	assert.Equal(t, m.field[180*640+320], small.field[90*320+160], "the same pattern at another size")
	other := newForensicMark(43, 640, 360, ForensicStrength)
	assert.NotEqual(t, m.field, other.field)

	pic := image.NewYCbCr(image.Rect(0, 0, 640, 360), image.YCbCrSubsampleRatio420)
	for i := range pic.Y {
		pic.Y[i] = 128
	}
	m.apply(pic, -1)
	assert.Equal(t, uint8(128-int(m.field[0])), pic.Y[0])
}

// TestForensicRenditions: a forensic rendition comes in two variants with
// the same samples, sync samples and timing, whose pictures differ by the
// faint pattern only: invisible (high PSNR), yet A minus B correlates
// with the pattern in every picture.
//
// covers: S-OBJ-19
func TestForensicRenditions(t *testing.T) {
	lib := library(t)
	src := openFixture(t)
	dir := t.TempDir()
	res, err := Renditions(context.Background(), lib, src, []RenditionSpec{
		{Name: "360p", Height: 360, Bitrate: 800_000, Forensic: true},
		{Name: "180p", Height: 180, Bitrate: 150_000},
	}, dir, nil)
	require.NoError(t, err)
	require.Len(t, res, 2)
	require.NotEmpty(t, res[0].PathB)
	assert.Empty(t, res[1].PathB)

	a, picsA := decodeAll(t, res[0].Path)
	b, picsB := decodeAll(t, res[0].PathB)
	require.Len(t, b.Video.Samples, len(a.Video.Samples))
	for i := range a.Video.Samples {
		sa, sb := a.Video.Samples[i], b.Video.Samples[i]
		assert.Equal(t, sa.Sync, sb.Sync, "sync sample %d", i)
		assert.Equal(t, sa.DTS, sb.DTS, "timing of sample %d", i)
	}
	require.Len(t, picsB, len(picsA))
	for i := range picsA {
		assert.Greater(t, lumaPSNR(picsA[i], picsB[i]), 38.0, "picture %d: the variants look the same", i)
		// The sign of the projection of A-B on (A-B) of the first
		// picture: positive in every picture (the mark is static).
		var corr float64
		pa, pb := picsA[i], picsB[i]
		for y := range 360 {
			for x := range 640 {
				d := float64(pa.Y[y*pa.YStride+x]) - float64(pb.Y[y*pb.YStride+x])
				m := float64(picsA[0].Y[y*640+x]) - float64(picsB[0].Y[y*640+x])
				corr += d * m
			}
		}
		assert.Positive(t, corr, "picture %d", i)
	}
}
