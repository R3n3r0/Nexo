package openh264

import (
	"image"
	"math"
	"os"
	"testing"

	"github.com/Eyevinn/mp4ff/avc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// library opens the library named by NEXO_OPENH264 (the tests that need
// it are skipped without it).
func library(t *testing.T) *Library {
	t.Helper()
	path := os.Getenv("NEXO_OPENH264")
	if path == "" {
		t.Skip("NEXO_OPENH264 is not set (the path of libopenh264 " + Version + ")")
	}
	l, err := Open(path)
	require.NoError(t, err)
	return l
}

// pattern draws a moving gradient.
func pattern(w, h, n int) *image.YCbCr {
	img := image.NewYCbCr(image.Rect(0, 0, w, h), image.YCbCrSubsampleRatio420)
	for y := range h {
		for x := range w {
			img.Y[y*img.YStride+x] = uint8((x + y + 4*n) % 256) // #nosec G115 -- modulo
		}
	}
	for y := range h / 2 {
		for x := range w / 2 {
			img.Cb[y*img.CStride+x] = uint8(64 + (x+n)%128)  // #nosec G115 -- bounded
			img.Cr[y*img.CStride+x] = uint8(192 - (y+n)%128) // #nosec G115 -- bounded
		}
	}
	return img
}

// psnr compares the luma of two pictures.
func psnr(a, b *image.YCbCr) float64 {
	var sum float64
	w, h := a.Rect.Dx(), a.Rect.Dy()
	for y := range h {
		for x := range w {
			d := float64(a.Y[y*a.YStride+x]) - float64(b.Y[y*b.YStride+x])
			sum += d * d
		}
	}
	mse := sum / float64(w*h)
	if mse == 0 {
		return 99
	}
	return 10 * math.Log10(255*255/mse)
}

func TestVersion(t *testing.T) {
	l := library(t)
	assert.Equal(t, Version, l.CodecVersion())
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	l := library(t)
	const w, h, frames = 320, 180, 30
	enc, err := l.NewEncoder(EncoderParams{Width: w, Height: h, Bitrate: 1_500_000, FrameRate: 25})
	require.NoError(t, err)
	defer enc.Close()
	dec, err := l.NewDecoder()
	require.NoError(t, err)
	defer dec.Close()

	var decoded []*image.YCbCr
	var firstSPS, firstPPS [][]byte
	keys := 0
	for i := range frames {
		nalus, key, err := enc.Encode(pattern(w, h, i), int64(i*40), i%10 == 0)
		require.NoError(t, err)
		require.NotEmpty(t, nalus, "frame %d: no frame is skipped", i)
		if key {
			keys++
			sps, pps := avc.GetParameterSets(avc.ConvertByteStreamToNaluSample(annexB(nalus)))
			assert.NotEmpty(t, sps, "an IDR picture carries its parameter sets")
			assert.NotEmpty(t, pps)
			if firstSPS == nil {
				firstSPS, firstPPS = sps, pps
			}
			assert.Equal(t, firstSPS, sps, "one SPS for the whole stream (frame %d)", i)
			assert.Equal(t, firstPPS, pps, "one PPS for the whole stream (frame %d)", i)
		}
		pic, err := dec.Decode(annexB(nalus))
		require.NoError(t, err)
		if pic != nil {
			decoded = append(decoded, pic)
		}
	}
	rest, err := dec.Flush()
	require.NoError(t, err)
	decoded = append(decoded, rest...)
	assert.Equal(t, 3, keys, "an IDR picture where asked")
	require.Len(t, decoded, frames)
	for i, pic := range decoded {
		assert.Equal(t, image.Rect(0, 0, w, h), pic.Rect)
		assert.Greater(t, psnr(pattern(w, h, i), pic), 30.0, "frame %d", i)
	}
}

// TestDecodeHighProfile decodes a High profile stream with B-frames
// (libx264): the pictures come out in display order.
func TestDecodeHighProfile(t *testing.T) {
	l := library(t)
	data, err := os.ReadFile("testdata/high_bframes.h264")
	require.NoError(t, err)
	dec, err := l.NewDecoder()
	require.NoError(t, err)
	defer dec.Close()
	var pics []*image.YCbCr
	for _, au := range accessUnits(data) {
		pic, err := dec.Decode(au)
		require.NoError(t, err)
		if pic != nil {
			pics = append(pics, pic)
		}
	}
	rest, err := dec.Flush()
	require.NoError(t, err)
	pics = append(pics, rest...)
	require.Len(t, pics, 50, "2 s at 25 fps")
	assert.Equal(t, image.Rect(0, 0, 320, 180), pics[0].Rect)
}

func annexB(nalus [][]byte) []byte {
	var out []byte
	for _, n := range nalus {
		out = append(out, 0, 0, 0, 1)
		out = append(out, n...)
	}
	return out
}

// accessUnits splits an Annex B stream into access units: a new one starts
// at an access unit delimiter, a parameter set or the first slice of a
// picture (first_mb_in_slice 0, the first bit after the NAL header set).
func accessUnits(stream []byte) [][]byte {
	var aus [][]byte
	var cur [][]byte
	slice := false
	for _, n := range avc.ExtractNalusFromByteStream(stream) {
		typ := n[0] & 0x1f
		starts := typ == 9 || typ == 7 || typ == 8 || typ == 6
		isSlice := typ == 1 || typ == 5
		if (starts && slice) || (isSlice && slice && len(n) > 1 && n[1]&0x80 != 0) {
			aus = append(aus, annexB(cur))
			cur, slice = nil, false
		}
		cur = append(cur, n)
		slice = slice || isSlice
	}
	if len(cur) > 0 {
		aus = append(aus, annexB(cur))
	}
	return aus
}
