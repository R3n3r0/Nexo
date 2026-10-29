package transcode

import (
	"context"
	"errors"
	"fmt"
	"image"
	"math"
	"slices"

	"github.com/r3n3r0/nexo/internal/mediaworker/openh264"
)

// Trace reads the forensic watermark of a leaked copy: every picture of
// the copy is compared with the pictures of the two variants of the
// renditions at the same time of the video, scaled to their size, and
// tells which variant it is closer to along their difference (the
// pattern). The server groups the votes by segment and reads the code of
// the playback token the copy was streamed with (Admin API, nexoctl media
// trace).

// TraceRendition is a rendition of the video with its two variants.
type TraceRendition struct {
	Name string
	A, B *Source
}

// TraceVote is what a picture of a leaked copy tells: at T seconds of the
// video, Score about +1 for the A variant, about -1 for the B variant,
// near 0 for neither; Rendition is the rendition it matched best.
type TraceVote struct {
	T         float64 `json:"t"`
	Score     float64 `json:"s"`
	Rendition string  `json:"r,omitempty"`
}

// TraceOptions tune Trace.
type TraceOptions struct {
	// Offset is added to the times of the copy: the time of the video its
	// first picture shows (a clip).
	Offset float64
	// Every is the least time between two votes, in seconds (default
	// 0.2): the pictures of a segment carry the same bit.
	Every float64
}

// traceStep samples the pixels of the comparison (every other row and
// column).
const traceStep = 2

// frameStream decodes a video in display order and yields the picture
// showing at a time.
type frameStream struct {
	pics  <-chan *image.YCbCr
	done  <-chan error
	stop  context.CancelFunc
	times []float64
	i     int
	cur   *image.YCbCr
	curT  float64
}

func newFrameStream(ctx context.Context, lib *openh264.Library, src *Source) (*frameStream, error) {
	dec, err := lib.NewDecoder()
	if err != nil {
		return nil, err
	}
	samples := src.Video.Samples
	pts := make([]int64, len(samples))
	for i, s := range samples {
		pts[i] = s.PTS()
	}
	slices.Sort(pts)
	base := src.firstPTS()
	times := make([]float64, len(pts))
	for i, p := range pts {
		times[i] = float64(p-base) / float64(src.Video.Timescale)
	}
	ctx, stop := context.WithCancel(ctx)
	pics, done := decodeAhead(ctx, dec, src, samples)
	f := &frameStream{pics: pics, done: done, times: times, curT: math.Inf(-1),
		stop: func() {
			stop()
			// Drained, so that the decoding goroutine ends.
			for p := range pics {
				_ = p
			}
			<-done
			dec.Close()
		}}
	return f, nil
}

// at returns the picture showing at time t (the last one starting at or
// before it), nil before the first or after the stream ended.
func (f *frameStream) at(t float64) *image.YCbCr {
	for f.i < len(f.times) && f.times[f.i] <= t+1e-6 {
		pic, ok := <-f.pics
		if !ok {
			f.i = len(f.times)
			break
		}
		f.cur, f.curT = pic, f.times[f.i]
		f.i++
	}
	return f.cur
}

// renditionStreams are the variants of a rendition being decoded.
type renditionStreams struct {
	name   string
	a, b   *frameStream
	w, h   int
	scaler Scaler
}

// Trace implements the reading of the forensic watermark of leaked.
func Trace(ctx context.Context, lib *openh264.Library, leaked *Source, renditions []TraceRendition, opts TraceOptions) (votes []TraceVote, err error) {
	if len(renditions) == 0 {
		return nil, errors.New("transcode: trace: no rendition with both variants")
	}
	every := opts.Every
	if every <= 0 {
		every = 0.2
	}
	var streams []*renditionStreams
	defer func() {
		for _, s := range streams {
			if s.a != nil {
				s.a.stop()
			}
			if s.b != nil {
				s.b.stop()
			}
		}
	}()
	for _, r := range renditions {
		if r.A.Width != r.B.Width || r.A.Height != r.B.Height {
			return nil, fmt.Errorf("transcode: trace: the variants of %s differ in size", r.Name)
		}
		s := &renditionStreams{name: r.Name, w: r.A.Width, h: r.A.Height}
		streams = append(streams, s)
		if s.a, err = newFrameStream(ctx, lib, r.A); err != nil {
			return nil, err
		}
		if s.b, err = newFrameStream(ctx, lib, r.B); err != nil {
			return nil, err
		}
	}
	copyStream, err := newFrameStream(ctx, lib, leaked)
	if err != nil {
		return nil, err
	}
	defer copyStream.stop()

	last := math.Inf(-1)
	for i := range copyStream.times {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pic := copyStream.at(copyStream.times[i])
		if pic == nil {
			break
		}
		t := copyStream.times[i] + opts.Offset
		if t < 0 || t-last < every {
			continue
		}
		best := TraceVote{T: t}
		bestDist := math.Inf(1)
		for _, s := range streams {
			a, b := s.a.at(t), s.b.at(t)
			if a == nil || b == nil {
				continue
			}
			score, dist, ok := compareVariants(s.scaler.Scale(pic, s.w, s.h), a, b)
			if ok && dist < bestDist {
				best.Score, best.Rendition, bestDist = score, s.name, dist
			}
		}
		if best.Rendition != "" {
			votes = append(votes, best)
			last = t
		}
	}
	return votes, nil
}

// compareVariants projects the luma of a picture of the copy (at the size
// of the variants) on the difference of the variants a and b around their
// midpoint: score is +1 for a, -1 for b; dist is the mean squared
// distance of the copy from the midpoint (the rendition the copy came
// from is the closest). ok is false when the variants are the same there.
func compareVariants(c, a, b *image.YCbCr) (score, dist float64, ok bool) {
	w, h := a.Rect.Dx(), a.Rect.Dy()
	var num, den, sq float64
	n := 0
	for y := 0; y < h; y += traceStep {
		ra, rb, rc := a.Y[y*a.YStride:], b.Y[y*b.YStride:], c.Y[y*c.YStride:]
		for x := 0; x < w; x += traceStep {
			va, vb, vc := float64(ra[x]), float64(rb[x]), float64(rc[x])
			mid, d := (va+vb)/2, va-vb
			num += (vc - mid) * d
			den += d * d
			sq += (vc - mid) * (vc - mid)
			n++
		}
	}
	if n == 0 || den < float64(n)*0.05 {
		return 0, 0, false
	}
	return 2 * num / den, sq / float64(n), true
}
