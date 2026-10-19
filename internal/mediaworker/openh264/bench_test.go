package openh264

import (
	"fmt"
	"image"
	"testing"
)

// The benchmarks run on generated pictures with texture and motion (a
// gradient, moving blocks and film grain): an encoder spends on them about
// what it spends on camera footage, unlike flat test patterns. They report
// the pictures per second of one encoder or decoder, on one core.

// sizes are the rendition presets the benchmarks measure.
var sizes = []struct {
	name          string
	width, height int
	bitrate       int
}{
	{"240p", 426, 240, 400_000},
	{"360p", 640, 360, 800_000},
	{"720p", 1280, 720, 2_800_000},
	{"1080p", 1920, 1080, 5_000_000},
}

// natural draws picture n of a moving scene with grain.
func natural(w, h, n int) *image.YCbCr {
	img := image.NewYCbCr(image.Rect(0, 0, w, h), image.YCbCrSubsampleRatio420)
	seed := uint32(n*2654435761 + 1) // #nosec G115 -- a PRNG seed
	grain := func() int {
		seed = seed*1664525 + 1013904223
		return int(seed>>27) - 16 // -16..15
	}
	bx, by := (n*7)%w, (n*3)%h
	for y := range h {
		for x := range w {
			v := (x*160)/w + (y*64)/h + 16
			if (x-bx+w)%w < w/6 && (y-by+h)%h < h/5 {
				v = 220 - (x+y)%32
			}
			img.Y[y*img.YStride+x] = uint8(min(max(v+grain()/2, 0), 255)) // #nosec G115 -- clamped
		}
	}
	for y := range h / 2 {
		for x := range w / 2 {
			img.Cb[y*img.CStride+x] = uint8(100 + (x*40)/w + grain()/4) // #nosec G115 -- bounded
			img.Cr[y*img.CStride+x] = uint8(150 - (y*40)/h + grain()/4) // #nosec G115 -- bounded
		}
	}
	return img
}

const benchFrames = 50 // 2 s at 25 fps: one IDR picture, as the renditions

func BenchmarkEncode(b *testing.B) {
	l := library(b)
	for _, s := range sizes {
		b.Run(s.name, func(b *testing.B) {
			pics := make([]*image.YCbCr, benchFrames)
			for i := range pics {
				pics[i] = natural(s.width&^1, s.height, i)
			}
			enc, err := l.NewEncoder(EncoderParams{Width: s.width &^ 1, Height: s.height, Bitrate: s.bitrate, FrameRate: 25})
			if err != nil {
				b.Fatal(err)
			}
			defer enc.Close()
			n := 0
			for b.Loop() {
				if _, _, err := enc.Encode(pics[n%benchFrames], int64(n*40), n%benchFrames == 0); err != nil {
					b.Fatal(err)
				}
				n++
			}
			b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "fps")
		})
	}
}

func BenchmarkDecode(b *testing.B) {
	l := library(b)
	for _, s := range sizes {
		b.Run(s.name, func(b *testing.B) {
			enc, err := l.NewEncoder(EncoderParams{Width: s.width &^ 1, Height: s.height, Bitrate: s.bitrate, FrameRate: 25})
			if err != nil {
				b.Fatal(err)
			}
			stream := make([][]byte, benchFrames)
			for i := range stream {
				nalus, _, err := enc.Encode(natural(s.width&^1, s.height, i), int64(i*40), i == 0)
				if err != nil {
					b.Fatal(err)
				}
				stream[i] = annexB(nalus)
			}
			enc.Close()
			dec, err := l.NewDecoder()
			if err != nil {
				b.Fatal(err)
			}
			defer dec.Close()
			n := 0
			for b.Loop() {
				if _, err := dec.Decode(stream[n%benchFrames]); err != nil {
					b.Fatal(fmt.Errorf("frame %d: %w", n, err))
				}
				n++
			}
			b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "fps")
		})
	}
}
