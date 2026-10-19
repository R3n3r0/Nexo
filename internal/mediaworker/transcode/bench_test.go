package transcode

import (
	"context"
	"image"
	"io"
	"os"
	"testing"
	"time"
)

// Benchmarks of the steps of a job, on the 640x360 fixture (3 s, 25 fps) or
// on the video named by NEXO_BENCH_VIDEO (an MP4 H.264 file, e.g. 1080p),
// which gives the cost of real footage at the sizes of production.
// BenchmarkRenditions reports the processing speed as "x" (seconds of video
// per second of processing: 2x processes a 10-minute video in 5 minutes).

func benchSource(b *testing.B) *Source {
	b.Helper()
	path := os.Getenv("NEXO_BENCH_VIDEO")
	if path == "" {
		return openFixture(b)
	}
	src, err := Open(path)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = src.Close() })
	return src
}

func BenchmarkOpen(b *testing.B) {
	path := os.Getenv("NEXO_BENCH_VIDEO")
	if path == "" {
		path = fixture
	}
	for b.Loop() {
		src, err := Open(path)
		if err != nil {
			b.Fatal(err)
		}
		_ = src.Close()
	}
}

func BenchmarkScale(b *testing.B) {
	for _, c := range []struct {
		name         string
		w, h, tw, th int
	}{
		{"1080p-720p", 1920, 1080, 1280, 720},
		{"1080p-360p", 1920, 1080, 640, 360},
		{"720p-480p", 1280, 720, 854, 480},
		{"360p-240p", 640, 360, 426, 240},
		{"1080p-tile", 1920, 1080, 160, 90},
	} {
		b.Run(c.name, func(b *testing.B) {
			src := image.NewYCbCr(image.Rect(0, 0, c.w, c.h), image.YCbCrSubsampleRatio420)
			for i := range src.Y {
				src.Y[i] = byte(i)
			}
			b.SetBytes(int64(c.w * c.h * 3 / 2))
			var s Scaler // as a rendition: the weights and buffers are kept
			for b.Loop() {
				s.Scale(src, c.tw, c.th)
			}
			b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "fps")
		})
	}
}

// renditionSets are the rendition sets of BenchmarkRenditions, the
// taller than the source skipped.
var renditionSets = []struct {
	name  string
	specs []RenditionSpec
}{
	{"240p", []RenditionSpec{{Name: "240p", Height: 240, Bitrate: 400_000}}},
	{"360p", []RenditionSpec{{Name: "360p", Height: 360, Bitrate: 800_000}}},
	{"720p", []RenditionSpec{{Name: "720p", Height: 720, Bitrate: 2_800_000}}},
	{"720p+360p+240p", []RenditionSpec{
		{Name: "720p", Height: 720, Bitrate: 2_800_000},
		{Name: "360p", Height: 360, Bitrate: 800_000},
		{Name: "240p", Height: 240, Bitrate: 400_000},
	}},
}

func BenchmarkRenditions(b *testing.B) {
	lib := library(b)
	src := benchSource(b)
	for _, set := range renditionSets {
		var specs []RenditionSpec
		for _, s := range set.specs {
			if s.Height <= src.Height {
				specs = append(specs, s)
			}
		}
		if len(specs) != len(set.specs) {
			continue
		}
		b.Run(set.name, func(b *testing.B) {
			dir := b.TempDir()
			for b.Loop() {
				res, err := Renditions(context.Background(), lib, src, specs, dir, nil)
				if err != nil {
					b.Fatal(err)
				}
				for _, r := range res {
					_ = os.Remove(r.Path)
				}
			}
			b.ReportMetric(src.Seconds()*float64(b.N)/b.Elapsed().Seconds(), "x")
		})
	}
}

func BenchmarkPoster(b *testing.B) {
	lib := library(b)
	src := benchSource(b)
	for b.Loop() {
		if _, _, err := Poster(context.Background(), lib, src, 1280, io.Discard); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSprite(b *testing.B) {
	lib := library(b)
	src := benchSource(b)
	interval := 10 * time.Second
	if src.Seconds() < 30 {
		interval = time.Second
	}
	for b.Loop() {
		if _, err := Sprite(context.Background(), lib, src, interval, 160, 10, "sprite.jpg", io.Discard, io.Discard, nil); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(src.Seconds()*float64(b.N)/b.Elapsed().Seconds(), "x")
}
