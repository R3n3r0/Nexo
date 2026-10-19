package transcode

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/mediaworker/openh264"
)

// moving returns picture i of a test video: a gradient and a square that
// move, so that a picture out of order stands out.
func moving(w, h, i int) *image.YCbCr {
	img := image.NewYCbCr(image.Rect(0, 0, w, h), image.YCbCrSubsampleRatio420)
	for y := range h {
		for x := range w {
			v := (x + y + 4*i) % 256
			if x >= 6*i%w && x < 6*i%w+w/8 && y >= h/3 && y < h/3+h/8 {
				v = 255 - v
			}
			img.Y[y*img.YStride+x] = uint8(v) // #nosec G115 -- modulo 256
		}
	}
	for j := range img.Cb {
		img.Cb[j], img.Cr[j] = uint8(100+i%50), uint8(150-i%50) // #nosec G115 -- small
	}
	return img
}

// writeSource encodes n pictures of moving at 25 fps with an IDR picture
// every idrEvery pictures into an MP4 file and opens it.
func writeSource(t *testing.T, lib *openh264.Library, w, h, n, idrEvery int) *Source {
	t.Helper()
	enc, err := lib.NewEncoder(openh264.EncoderParams{Width: w, Height: h, Bitrate: 1_000_000, FrameRate: 25})
	require.NoError(t, err)
	defer enc.Close()
	var data bytes.Buffer
	var samples []muxSample
	var sps, pps []byte
	for i := range n {
		nalus, key, err := enc.Encode(moving(w, h, i), int64(i*40), i%idrEvery == 0)
		require.NoError(t, err)
		start := data.Len()
		for _, nal := range nalus {
			switch nal[0] & 0x1f {
			case 7:
				sps = nal
				continue
			case 8:
				pps = nal
				continue
			}
			_ = binary.Write(&data, binary.BigEndian, uint32(len(nal))) // #nosec G115 -- test
			data.Write(nal)
		}
		samples = append(samples, muxSample{size: uint32(data.Len() - start), dur: 512, sync: key, src: int64(start)}) // #nosec G115 -- test
	}
	stsd, err := avcSampleEntry(sps, pps)
	require.NoError(t, err)
	video := &muxTrack{id: 1, handler: "vide", timescale: 12800, language: "und", stsd: stsd, width: w, height: h,
		samples: samples, data: bytes.NewReader(data.Bytes())}
	var out bytes.Buffer
	require.NoError(t, writeMP4(&out, []*muxTrack{video}))
	path := filepath.Join(t.TempDir(), "source.mp4")
	require.NoError(t, os.WriteFile(path, out.Bytes(), 0o600))
	src, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = src.Close() })
	return src
}

func TestSpans(t *testing.T) {
	lib := library(t)
	// An IDR picture every second: spans of keyInterval (2 s).
	src := writeSource(t, lib, 160, 90, 130, 25)
	spans, err := src.spans()
	require.NoError(t, err)
	assert.Equal(t, []span{{0, 50}, {50, 100}, {100, 130}}, spans)

	// An IDR picture every 2.6 s (65 pictures) is never the first picture
	// of a 2 s period after the first: split after maxSpan (8 s), at the
	// IDR picture of 10.4 s.
	src = writeSource(t, lib, 160, 90, 300, 65)
	spans, err = src.spans()
	require.NoError(t, err)
	assert.Equal(t, []span{{0, 260}, {260, 300}}, spans)

	// The fixture (B-frames) has an IDR picture at 0 and 2 s.
	spans, err = openFixture(t).spans()
	require.NoError(t, err)
	assert.Equal(t, []span{{0, 50}, {50, 75}}, spans)
}

func TestIsIDR(t *testing.T) {
	src := openFixture(t)
	idr, err := src.isIDR(src.Video.Samples[0])
	require.NoError(t, err)
	assert.True(t, idr)
	idr, err = src.isIDR(src.Video.Samples[1])
	require.NoError(t, err)
	assert.False(t, idr)
}

// TestRenditionsInParallel encodes the spans of a video on several workers:
// the files are the same as with one worker, their pictures are those of
// the source in order, with an IDR picture every 2 s.
func TestRenditionsInParallel(t *testing.T) {
	lib := library(t)
	src := writeSource(t, lib, 320, 180, 200, 25) // 8 s, 4 spans
	specs := []RenditionSpec{{Name: "180p", Height: 180, Bitrate: 600_000}, {Name: "90p", Height: 90, Bitrate: 200_000}}
	files := map[int][][]byte{}
	decode := func(workers int) [][]*image.YCbCr {
		dir := t.TempDir()
		res, err := renditions(context.Background(), lib, src, specs, dir, nil, workers)
		require.NoError(t, err)
		var all [][]*image.YCbCr
		for _, r := range res {
			data, err := os.ReadFile(r.Path)
			require.NoError(t, err)
			files[workers] = append(files[workers], data)
			out, err := Open(r.Path)
			require.NoError(t, err)
			defer out.Close()
			var keys []int
			for i, s := range out.Video.Samples {
				if s.Sync {
					keys = append(keys, i)
				}
			}
			assert.Equal(t, []int{0, 50, 100, 150}, keys, "workers %d %s", workers, r.Name)
			assert.InDelta(t, 8.0, out.Seconds(), 0.05)
			dec, err := lib.NewDecoder()
			require.NoError(t, err)
			defer dec.Close()
			var pics []*image.YCbCr
			for _, s := range out.Video.Samples {
				au, err := out.accessUnit(s)
				require.NoError(t, err)
				pic, err := dec.Decode(au)
				require.NoError(t, err)
				if pic != nil {
					pics = append(pics, pic)
				}
			}
			rest, err := dec.Flush()
			require.NoError(t, err)
			all = append(all, append(pics, rest...))
		}
		return all
	}
	one, four := decode(1), decode(4)
	for r := range specs {
		assert.True(t, bytes.Equal(files[1][r], files[4][r]), "%s: the same file with 1 and 4 workers", specs[r].Name)
	}
	for r := range specs {
		require.Len(t, one[r], 200)
		require.Len(t, four[r], 200)
		for i := range 200 {
			want := Scale(moving(320, 180, i), one[r][i].Rect.Dx(), one[r][i].Rect.Dy())
			assert.Greater(t, psnr(want.Y, four[r][i].Y), 25.0, "%s picture %d of 4 workers", specs[r].Name, i)
			assert.Greater(t, psnr(want.Y, one[r][i].Y), 25.0, "%s picture %d of 1 worker", specs[r].Name, i)
		}
	}
}

func psnr(a, b []byte) float64 {
	var sum float64
	for i := range a {
		d := float64(a[i]) - float64(b[i])
		sum += d * d
	}
	if sum == 0 {
		return math.Inf(1)
	}
	return 10 * math.Log10(255*255*float64(len(a))/sum)
}

func TestInOrder(t *testing.T) {
	var got []int
	err := inOrder(context.Background(), 50, 4, func() int { return 0 },
		func(_ context.Context, _ int, k int) (int, error) {
			time.Sleep(time.Duration(rand.IntN(3)) * time.Millisecond) // #nosec G404 -- test jitter
			return k * k, nil
		}, func(k, v int) error {
			assert.Equal(t, k*k, v)
			got = append(got, k)
			return nil
		})
	require.NoError(t, err)
	assert.Len(t, got, 50)
	assert.IsIncreasing(t, got)

	boom := errors.New("boom")
	err = inOrder(context.Background(), 50, 4, func() int { return 0 },
		func(_ context.Context, _ int, k int) (int, error) {
			if k == 20 {
				return 0, boom
			}
			return k, nil
		}, func(int, int) error { return nil })
	require.ErrorIs(t, err, boom)

	err = inOrder(context.Background(), 10, 2, func() int { return 0 },
		func(_ context.Context, _ int, k int) (int, error) { return k, nil },
		func(k, _ int) error {
			if k == 3 {
				return boom
			}
			return nil
		})
	require.ErrorIs(t, err, boom)
}
