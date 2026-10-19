package transcode

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/mediaworker/openh264"
)

const fixture = "testdata/h264_aac_360p.mp4" // 640x360, 25 fps, 3 s, H.264 High with B-frames, AAC

func library(t testing.TB) *openh264.Library {
	t.Helper()
	path := os.Getenv("NEXO_OPENH264")
	if path == "" {
		t.Skip("NEXO_OPENH264 is not set (the path of libopenh264 " + openh264.Version + ")")
	}
	l, err := openh264.Open(path)
	require.NoError(t, err)
	return l
}

func openFixture(t testing.TB) *Source {
	t.Helper()
	src, err := Open(fixture)
	require.NoError(t, err)
	t.Cleanup(func() { _ = src.Close() })
	return src
}

func TestOpenSource(t *testing.T) {
	src := openFixture(t)
	assert.Equal(t, 640, src.Width)
	assert.Equal(t, 360, src.Height)
	assert.Len(t, src.Video.Samples, 75)
	assert.InDelta(t, 3.0, src.Seconds(), 0.05)
	require.NotNil(t, src.Audio)
	assert.NotEmpty(t, src.Audio.Samples)
	assert.True(t, src.Video.Samples[0].Sync)
	assert.Equal(t, 0, src.keyframeAt(0))
}

func TestOpenUnsupported(t *testing.T) {
	_, err := Open("../../service/media/testdata/hevc_aac.mp4")
	assert.ErrorIs(t, err, ErrUnsupported)
	_, err = Open("../../service/media/testdata/fragmented.mp4")
	assert.ErrorIs(t, err, ErrUnsupported)
	bad := filepath.Join(t.TempDir(), "x.mp4")
	require.NoError(t, os.WriteFile(bad, []byte("not a video"), 0o600))
	_, err = Open(bad)
	assert.ErrorIs(t, err, ErrUnsupported)
}

func TestScaleKeepsFlatColor(t *testing.T) {
	src := image.NewYCbCr(image.Rect(0, 0, 640, 360), image.YCbCrSubsampleRatio420)
	for i := range src.Y {
		src.Y[i] = 200
	}
	for i := range src.Cb {
		src.Cb[i], src.Cr[i] = 90, 160
	}
	for _, size := range [][2]int{{426, 240}, {160, 90}, {1280, 720}} {
		dst := Scale(src, size[0], size[1])
		assert.Equal(t, image.Rect(0, 0, size[0], size[1]), dst.Rect)
		for _, v := range dst.Y {
			require.Equal(t, uint8(200), v)
		}
		for i := range dst.Cb {
			require.Equal(t, uint8(90), dst.Cb[i])
			require.Equal(t, uint8(160), dst.Cr[i])
		}
	}
}

func TestFit(t *testing.T) {
	w, h := FitHeight(1920, 1080, 720)
	assert.Equal(t, [2]int{1280, 720}, [2]int{w, h})
	w, h = FitHeight(640, 360, 240)
	assert.Equal(t, [2]int{426, 240}, [2]int{w, h})
	w, h = FitWidth(1920, 800, 160)
	assert.Equal(t, [2]int{160, 66}, [2]int{w, h})
}

func TestVTTTime(t *testing.T) {
	assert.Equal(t, "00:00:00.000", vttTime(0))
	assert.Equal(t, "01:02:03.500", vttTime(3723.5))
}

// TestMuxCopy remuxes the source tracks unchanged and parses the result.
func TestMuxCopy(t *testing.T) {
	src := openFixture(t)
	v := src.Video
	samples := make([]muxSample, len(v.Samples))
	for i, s := range v.Samples {
		dur := uint32(512)
		if i+1 < len(v.Samples) {
			dur = uint32(v.Samples[i+1].DTS - s.DTS)
		}
		samples[i] = muxSample{size: s.Size, dur: dur, cto: s.CTO, sync: s.Sync, src: s.Offset}
	}
	video := &muxTrack{id: 1, handler: "vide", timescale: v.Timescale, language: "und", stsd: v.Stsd,
		width: src.Width, height: src.Height, samples: samples, data: src.f}
	var out bytes.Buffer
	require.NoError(t, writeMP4(&out, []*muxTrack{video, audioTrack(src)}))
	path := filepath.Join(t.TempDir(), "copy.mp4")
	require.NoError(t, os.WriteFile(path, out.Bytes(), 0o600))
	copied, err := Open(path)
	require.NoError(t, err)
	defer copied.Close()
	require.Len(t, copied.Video.Samples, len(v.Samples))
	for i, s := range copied.Video.Samples {
		require.Equal(t, v.Samples[i].Size, s.Size)
		require.Equal(t, v.Samples[i].Sync, s.Sync)
		require.Equal(t, v.Samples[i].CTO, s.CTO)
		a, err := src.ReadSample(v.Samples[i])
		require.NoError(t, err)
		b, err := copied.ReadSample(s)
		require.NoError(t, err)
		require.Equal(t, a, b, "sample %d", i)
	}
	require.NotNil(t, copied.Audio)
	assert.Len(t, copied.Audio.Samples, len(src.Audio.Samples))
	ffprobe(t, path)
}

func TestRenditions(t *testing.T) {
	lib := library(t)
	src := openFixture(t)
	dir := t.TempDir()
	var last float64
	res, err := Renditions(context.Background(), lib, src, []RenditionSpec{
		{Name: "240p", Height: 240, Bitrate: 300_000},
		{Name: "180p", Height: 180, Bitrate: 150_000},
	}, dir, func(d float64) { last = d })
	require.NoError(t, err)
	require.Len(t, res, 2)
	assert.InDelta(t, 1.0, last, 0.01)
	assert.Equal(t, RenditionResult{Name: "240p", Width: 426, Height: 240, Bandwidth: res[0].Bandwidth, Path: filepath.Join(dir, "240p.mp4")}, res[0])
	assert.Positive(t, res[0].Bandwidth)
	assert.Greater(t, res[0].Bandwidth, res[1].Bandwidth)

	out, err := Open(res[0].Path)
	require.NoError(t, err)
	defer out.Close()
	assert.Equal(t, 426, out.Width)
	assert.Equal(t, 240, out.Height)
	require.Len(t, out.Video.Samples, 75)
	var keys []int
	for i, s := range out.Video.Samples {
		if s.Sync {
			keys = append(keys, i)
		}
		assert.Zero(t, s.CTO, "no reordering in Constrained Baseline")
	}
	assert.Equal(t, []int{0, 50}, keys, "an IDR picture every 2 s")
	assert.InDelta(t, 3.0, out.Seconds(), 0.05)
	require.NotNil(t, out.Audio)
	assert.Len(t, out.Audio.Samples, len(src.Audio.Samples), "the audio is copied")

	// The rendition decodes back to its 75 pictures.
	dec, err := lib.NewDecoder()
	require.NoError(t, err)
	defer dec.Close()
	pics := 0
	for _, s := range out.Video.Samples {
		au, err := out.accessUnit(s)
		require.NoError(t, err)
		pic, err := dec.Decode(au)
		require.NoError(t, err)
		if pic != nil {
			pics++
		}
	}
	rest, err := dec.Flush()
	require.NoError(t, err)
	assert.Equal(t, 75, pics+len(rest))
	ffprobe(t, res[0].Path)

	// The parsed file has the expected boxes.
	f, err := os.Open(res[1].Path)
	require.NoError(t, err)
	defer f.Close()
	parsed, err := mp4.DecodeFile(f)
	require.NoError(t, err)
	assert.Equal(t, "isom", parsed.Ftyp.MajorBrand())
	assert.Len(t, parsed.Moov.Traks, 2)
}

func TestPosterAndSprite(t *testing.T) {
	lib := library(t)
	src := openFixture(t)
	var poster bytes.Buffer
	w, h, err := Poster(context.Background(), lib, src, 320, &poster)
	require.NoError(t, err)
	assert.Equal(t, [2]int{320, 180}, [2]int{w, h})
	img, err := jpeg.Decode(&poster)
	require.NoError(t, err)
	assert.Equal(t, image.Rect(0, 0, 320, 180), img.Bounds())

	var sprite, vtt bytes.Buffer
	res, err := Sprite(context.Background(), lib, src, time.Second, 160, 10, "sprite.jpg", &sprite, &vtt, nil)
	require.NoError(t, err)
	assert.Equal(t, SpriteResult{Interval: 1, Width: 160, Height: 90, Columns: 3, Count: 3}, res)
	img, err = jpeg.Decode(&sprite)
	require.NoError(t, err)
	assert.Equal(t, image.Rect(0, 0, 480, 90), img.Bounds())
	assert.Equal(t, "WEBVTT\n\n00:00:00.000 --> 00:00:01.000\nsprite.jpg#xywh=0,0,160,90\n"+
		"\n00:00:01.000 --> 00:00:02.000\nsprite.jpg#xywh=160,0,160,90\n"+
		"\n00:00:02.000 --> 00:00:03.000\nsprite.jpg#xywh=320,0,160,90\n", vtt.String())
}

// ffprobe checks a file with ffprobe when it is installed (a check made
// with a third-party demuxer and decoder, not a dependency): every
// frame decodes without an error.
func ffprobe(t *testing.T, path string) {
	t.Helper()
	bin, err := exec.LookPath("ffprobe")
	if err != nil {
		return
	}
	out, err := exec.Command(bin, "-v", "error", "-count_frames", "-show_entries", "stream=codec_name", "-of", "csv=p=0", path).CombinedOutput() // #nosec G204 -- test tool
	require.NoError(t, err, string(out))
	assert.Equal(t, "h264\naac", strings.TrimSpace(string(out)))
}
