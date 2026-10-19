package transcode

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"math"
	"runtime"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/mediaworker/openh264"
)

const (
	// jpegQuality of the poster and of the sprite.
	jpegQuality = 85
	// maxThumbnails bounds the tiles of a sprite: a longer video gets a
	// longer interval.
	maxThumbnails = 500
)

// keyframeAt returns the index of the last sync sample shown at or before
// t seconds (the first sync sample when none).
func (s *Source) keyframeAt(t float64) int {
	samples := s.Video.Samples
	if len(samples) == 0 {
		return -1
	}
	base := s.firstPTS()
	limit := base + int64(t*float64(s.Video.Timescale))
	best, first := -1, -1
	for i, sm := range samples {
		if !sm.Sync {
			continue
		}
		if first < 0 {
			first = i
		}
		if sm.PTS() <= limit && (best < 0 || sm.PTS() > samples[best].PTS()) {
			best = i
		}
	}
	if best < 0 {
		return first
	}
	return best
}

func (s *Source) firstPTS() int64 {
	first := int64(math.MaxInt64)
	for _, sm := range s.Video.Samples {
		first = min(first, sm.PTS())
	}
	return first
}

// decodeKeyframe decodes the sync sample i alone.
func (s *Source) decodeKeyframe(lib *openh264.Library, i int) (*image.YCbCr, error) {
	if i < 0 {
		return nil, fmt.Errorf("%w: no sync sample", ErrUnsupported)
	}
	dec, err := lib.NewDecoder()
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	au, err := s.accessUnit(s.Video.Samples[i])
	if err != nil {
		return nil, err
	}
	pic, err := dec.Decode(au)
	if err != nil {
		return nil, err
	}
	if pic == nil {
		rest, err := dec.Flush()
		if err != nil {
			return nil, err
		}
		if len(rest) == 0 {
			return nil, fmt.Errorf("%w: the keyframe at sample %d could not be decoded", ErrUnsupported, i+1)
		}
		pic = rest[0]
	}
	return pic, nil
}

// Poster writes a JPEG of the keyframe shown at a tenth of the video (at
// most 10 seconds in), at most maxWidth wide, and returns its size.
func Poster(ctx context.Context, lib *openh264.Library, src *Source, maxWidth int, w io.Writer) (width, height int, err error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	pic, err := src.decodeKeyframe(lib, src.keyframeAt(min(src.Seconds()/10, 10)))
	if err != nil {
		return 0, 0, err
	}
	width, height = pic.Rect.Dx(), pic.Rect.Dy()
	if width > maxWidth {
		width, height = FitWidth(width, height, maxWidth)
		pic = Scale(pic, width, height)
	}
	if err := jpeg.Encode(w, pic, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return 0, 0, err
	}
	return width, height, nil
}

// SpriteResult describes a thumbnail sprite: Count tiles of Width x
// Height in Columns columns, one every Interval seconds.
type SpriteResult struct {
	Interval float64
	Width    int
	Height   int
	Columns  int
	Count    int
}

// Sprite writes a JPEG grid of thumbnails, one every interval (the
// keyframe shown at that time), and the WebVTT map of the tiles that
// refers to the sprite as spriteName.
func Sprite(ctx context.Context, lib *openh264.Library, src *Source, interval time.Duration, tileWidth, columns int,
	spriteName string, sprite, vtt io.Writer, progress Progress) (SpriteResult, error) {
	dur := src.Seconds()
	step := max(interval.Seconds(), 0.1)
	count := max(1, int(math.Ceil(dur/step)))
	if count > maxThumbnails {
		count = maxThumbnails
		step = dur / maxThumbnails
	}
	tw, th := FitWidth(src.Width, src.Height, tileWidth)
	columns = min(columns, count)
	rows := (count + columns - 1) / columns
	grid := image.NewYCbCr(image.Rect(0, 0, tw*columns, th*rows), image.YCbCrSubsampleRatio420)
	fillBlack(grid)
	var cue strings.Builder
	cue.WriteString("WEBVTT\n")
	// Each tile shows the keyframe at its time: the distinct keyframes are
	// decoded in parallel, each with its own decoder, and pasted in order.
	keys := make([]int, count)
	var distinct []int
	for i := range count {
		keys[i] = src.keyframeAt(float64(i) * step)
		if i == 0 || keys[i] != keys[i-1] {
			distinct = append(distinct, i)
		}
	}
	err := inOrder(ctx, len(distinct), runtime.GOMAXPROCS(0), func() struct{} { return struct{}{} },
		func(_ context.Context, _ struct{}, d int) (*image.YCbCr, error) {
			pic, err := src.decodeKeyframe(lib, keys[distinct[d]])
			if err != nil {
				return nil, err
			}
			return Scale(pic, tw, th), nil
		}, func(d int, tile *image.YCbCr) error {
			last := count
			if d+1 < len(distinct) {
				last = distinct[d+1]
			}
			for i := distinct[d]; i < last; i++ {
				t := float64(i) * step
				x, y := (i%columns)*tw, (i/columns)*th
				paste(grid, tile, x, y)
				fmt.Fprintf(&cue, "\n%s --> %s\n%s#xywh=%d,%d,%d,%d\n", vttTime(t), vttTime(min(t+step, dur)), spriteName, x, y, tw, th)
				if progress != nil {
					progress(float64(i+1) / float64(count))
				}
			}
			return nil
		})
	if err != nil {
		return SpriteResult{}, err
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, grid, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return SpriteResult{}, err
	}
	if _, err := sprite.Write(buf.Bytes()); err != nil {
		return SpriteResult{}, err
	}
	if _, err := io.WriteString(vtt, cue.String()); err != nil {
		return SpriteResult{}, err
	}
	return SpriteResult{Interval: step, Width: tw, Height: th, Columns: columns, Count: count}, nil
}

func fillBlack(img *image.YCbCr) {
	for i := range img.Cb {
		img.Cb[i], img.Cr[i] = 128, 128
	}
	for i := range img.Y {
		img.Y[i] = 16
	}
}

// paste copies tile into dst at (x, y) (even coordinates).
func paste(dst, tile *image.YCbCr, x, y int) {
	w, h := tile.Rect.Dx(), tile.Rect.Dy()
	for r := range h {
		copy(dst.Y[(y+r)*dst.YStride+x:], tile.Y[r*tile.YStride:r*tile.YStride+w])
	}
	cw, ch := w/2, h/2
	for r := range ch {
		do := (y/2+r)*dst.CStride + x/2
		so := r * tile.CStride
		copy(dst.Cb[do:], tile.Cb[so:so+cw])
		copy(dst.Cr[do:], tile.Cr[so:so+cw])
	}
}

// vttTime formats seconds as a WebVTT timestamp.
func vttTime(s float64) string {
	ms := int64(math.Round(s * 1000))
	return fmt.Sprintf("%02d:%02d:%02d.%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}
