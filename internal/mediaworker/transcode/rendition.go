package transcode

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"

	"github.com/Eyevinn/mp4ff/mp4"

	"github.com/r3n3r0/nexo/internal/mediaworker/openh264"
)

// keyInterval is the distance between two IDR pictures of a rendition:
// every rendition has an IDR picture at the same times, so their segments
// line up.
const keyInterval = 2.0

// RenditionSpec is a rendition to produce.
type RenditionSpec struct {
	Name    string
	Height  int
	Bitrate int // video, bits per second
}

// RenditionResult is a produced rendition.
type RenditionResult struct {
	Name   string
	Width  int
	Height int
	// Bandwidth is the peak bitrate over keyInterval windows (video and
	// audio), as HLS expects.
	Bandwidth int
	// Path of the MP4 file.
	Path string
}

// Progress reports the fraction of the work done.
type Progress func(done float64)

// rendition is a rendition file being written: the spans of the video
// are encoded apart and appended to it in order.
type rendition struct {
	spec    RenditionSpec
	width   int
	height  int
	data    *os.File // the video samples
	written int64
	samples []muxSample
	sps     []byte
	pps     []byte
}

// Renditions decodes the video of src once and encodes every spec from
// it, then writes each rendition, with the source audio copied, to
// dir/<name>.mp4. The video is split into spans that decode alone (they
// start at an IDR picture), processed in parallel on every core.
func Renditions(ctx context.Context, lib *openh264.Library, src *Source, specs []RenditionSpec, dir string, progress Progress) ([]RenditionResult, error) {
	return renditions(ctx, lib, src, specs, dir, progress, workers(src))
}

// decodeMemory bounds the memory of the pictures held by the spans
// decoded at once (decoders, pictures decoded ahead, scaled copies): a
// larger video gets fewer workers.
const decodeMemory = 1 << 30

// picturesPerWorker estimates the pictures a worker holds: the references
// and reordering of its decoder, the pictures it decodes ahead and the
// scaled copies of the renditions.
const picturesPerWorker = 24

// workers returns how many spans of src to process at once: one per core,
// within decodeMemory.
func workers(src *Source) int {
	perWorker := max(1, src.Width*src.Height*3/2*picturesPerWorker)
	return max(1, min(runtime.GOMAXPROCS(0), decodeMemory/perWorker))
}

func renditions(ctx context.Context, lib *openh264.Library, src *Source, specs []RenditionSpec, dir string, progress Progress, nworkers int) (results []RenditionResult, err error) {
	if len(specs) == 0 {
		return nil, nil
	}
	samples := src.Video.Samples
	if len(samples) == 0 {
		return nil, fmt.Errorf("%w: the video track has no samples", ErrUnsupported)
	}
	spans, err := src.spans()
	if err != nil {
		return nil, err
	}
	var rs []*rendition
	defer func() {
		for _, r := range rs {
			_ = r.data.Close()
			_ = os.Remove(r.data.Name())
		}
	}()
	for _, spec := range specs {
		w, h := FitHeight(src.Width, src.Height, spec.Height)
		f, err := os.CreateTemp(dir, spec.Name+"-*.h264")
		if err != nil {
			return nil, err
		}
		rs = append(rs, &rendition{spec: spec, width: w, height: h, data: f})
	}
	j := &spanJob{lib: lib, src: src, rs: rs, fps: float64(len(samples)) / max(src.Seconds(), 0.001)}
	var shown []int64 // the presentation times of the encoded pictures
	err = inOrder(ctx, len(spans), max(1, nworkers), j.newWorker, func(ctx context.Context, w *spanWorker, k int) (*spanOutput, error) {
		return w.encode(ctx, spans[k])
	}, func(k int, out *spanOutput) error {
		for i, r := range rs {
			if err := r.append(out.encodings[i]); err != nil {
				return err
			}
		}
		shown = append(shown, out.shown...)
		if progress != nil {
			progress(float64(spans[k].end) / float64(len(samples)))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(shown) == 0 {
		return nil, fmt.Errorf("%w: no picture could be decoded", ErrUnsupported)
	}
	durations := frameDurations(shown, src.Video)
	for _, r := range rs {
		res, err := r.finish(src, durations, dir)
		if err != nil {
			return nil, err
		}
		results = append(results, res)
	}
	return results, nil
}

// inOrder runs process over the items 0..n-1 on workers goroutines (each
// with the state newWorker gives it) and passes the outputs to deliver in
// item order, on the calling goroutine. At most 2*workers items are
// processed or waiting to be delivered: the outputs of the items ahead of
// a slow one stay bounded. The first error stops the work and is returned.
func inOrder[W, T any](ctx context.Context, n, workers int, newWorker func() W,
	process func(context.Context, W, int) (T, error), deliver func(int, T) error) error {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
	}()
	type result struct {
		k   int
		out T
		err error
	}
	slots := make(chan struct{}, 2*workers)
	next := make(chan int)
	done := make(chan result, n) // never blocks a worker
	wg.Go(func() {
		defer close(next)
		for k := range n {
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			select {
			case next <- k:
			case <-ctx.Done():
				return
			}
		}
	})
	for range min(workers, n) {
		wg.Go(func() {
			w := newWorker()
			for k := range next {
				out, err := process(ctx, w, k)
				done <- result{k, out, err}
			}
		})
	}
	pending := map[int]T{}
	for delivered := 0; delivered < n; {
		var res result
		select {
		case res = <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
		if res.err != nil {
			return res.err
		}
		pending[res.k] = res.out
		for {
			out, ok := pending[delivered]
			if !ok {
				break
			}
			delete(pending, delivered)
			if err := deliver(delivered, out); err != nil {
				return err
			}
			delivered++
			<-slots
		}
	}
	return nil
}

// spanJob is what the workers of Renditions share.
type spanJob struct {
	lib *openh264.Library
	src *Source
	rs  []*rendition
	fps float64
}

// spanWorker encodes spans; its scalers keep their weights from one
// span to the next.
type spanWorker struct {
	*spanJob
	scalers []Scaler
}

func (j *spanJob) newWorker() *spanWorker {
	return &spanWorker{spanJob: j, scalers: make([]Scaler, len(j.rs))}
}

// spanOutput is a span encoded into every rendition.
type spanOutput struct {
	encodings []*encoding
	shown     []int64
}

// encoding is a span being encoded into one rendition, in memory.
type encoding struct {
	width, height int
	enc           *openh264.Encoder
	scaler        *Scaler
	data          []byte
	samples       []muxSample // src: offset in data
	sps, pps      []byte
}

// encode decodes a span with its own decoder and encodes it with new
// encoders: the first picture of every rendition is an IDR picture, then
// one every keyInterval as in a single pass.
func (w *spanWorker) encode(ctx context.Context, c span) (out *spanOutput, err error) {
	out = &spanOutput{}
	defer func() {
		for _, e := range out.encodings {
			if e.enc != nil {
				e.enc.Close()
			}
		}
	}()
	for i, r := range w.rs {
		enc, err := w.lib.NewEncoder(openh264.EncoderParams{Width: r.width, Height: r.height, Bitrate: r.spec.Bitrate, FrameRate: min(w.fps, 60)})
		if err != nil {
			return nil, err
		}
		out.encodings = append(out.encodings, &encoding{width: r.width, height: r.height, enc: enc, scaler: &w.scalers[i]})
	}
	dec, err := w.lib.NewDecoder()
	if err != nil {
		return nil, err
	}
	defer dec.Close()

	samples := w.src.Video.Samples[c.first:c.end]
	pts := make([]int64, len(samples))
	for i, s := range samples {
		pts[i] = s.PTS()
	}
	slices.Sort(pts)
	base := w.src.firstPTS()
	scale := float64(w.src.Video.Timescale)
	nextKey := 0.0
	// The decoder runs ahead of the encoders, on its own goroutine: decoding
	// the next pictures overlaps with scaling and encoding this one.
	decodeCtx, stop := context.WithCancel(ctx)
	defer stop()
	pics, decoded := decodeAhead(decodeCtx, dec, w.src, samples)
	for pic := range pics {
		n := len(out.shown)
		if n >= len(pts) {
			continue // more pictures than samples: never seen, ignored
		}
		t := float64(pts[n]-base) / scale
		idr := n == 0 || t >= nextKey
		if idr {
			nextKey = math.Floor(t/keyInterval+1) * keyInterval
		}
		if err := encodeAll(out.encodings, pic, t, idr); err != nil {
			stop()
			<-decoded
			return nil, err
		}
		out.shown = append(out.shown, pts[n])
	}
	if err := <-decoded; err != nil {
		return nil, err
	}
	return out, nil
}

// decodeAhead decodes the samples on a goroutine and sends the pictures
// in display order, flushing the decoder at the end. done gives the outcome
// of the decoding once pics is closed; cancelling ctx stops it.
func decodeAhead(ctx context.Context, dec *openh264.Decoder, src *Source, samples []Sample) (pics <-chan *image.YCbCr, done <-chan error) {
	out := make(chan *image.YCbCr, 4)
	result := make(chan error, 1)
	go func() {
		defer close(out)
		result <- func() error {
			send := func(pic *image.YCbCr) error {
				select {
				case out <- pic:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			for _, sm := range samples {
				if err := ctx.Err(); err != nil {
					return err
				}
				au, err := src.accessUnit(sm)
				if err != nil {
					return err
				}
				pic, err := dec.Decode(au)
				if err != nil {
					return err
				}
				if pic != nil {
					if err := send(pic); err != nil {
						return err
					}
				}
			}
			rest, err := dec.Flush()
			if err != nil {
				return err
			}
			for _, pic := range rest {
				if err := send(pic); err != nil {
					return err
				}
			}
			return nil
		}()
	}()
	return out, result
}

// frameDurations returns the duration of each displayed picture.
func frameDurations(pts []int64, video *Track) []uint32 {
	d := make([]uint32, len(pts))
	for i := range len(pts) - 1 {
		d[i] = uint32(max(pts[i+1]-pts[i], 1)) // #nosec G115 -- frame durations
	}
	last := uint32(1)
	if len(pts) > 1 {
		last = d[len(pts)-2]
	} else if video.Duration > 0 {
		last = uint32(min(video.Duration, math.MaxUint32)) // #nosec G115 -- clamped
	}
	d[len(pts)-1] = last
	return d
}

// encode scales and encodes a picture, appending its sample.
func (e *encoding) encode(pic *image.YCbCr, t float64, idr bool) error {
	scaled := e.scaler.Scale(pic, e.width, e.height)
	nalus, key, err := e.enc.Encode(scaled, int64(t*1000), idr)
	if err != nil {
		return err
	}
	start := len(e.data)
	for _, nal := range nalus {
		if len(nal) == 0 {
			continue
		}
		switch nal[0] & 0x1f {
		case 7: // SPS: in the sample entry
			if e.sps == nil {
				e.sps = nal
			}
			continue
		case 8: // PPS
			if e.pps == nil {
				e.pps = nal
			}
			continue
		}
		e.data = binary.BigEndian.AppendUint32(e.data, uint32(len(nal))) // #nosec G115 -- NAL sizes
		e.data = append(e.data, nal...)
	}
	if len(e.data) == start {
		return errors.New("transcode: the encoder produced an empty picture")
	}
	e.samples = append(e.samples, muxSample{size: uint32(len(e.data) - start), sync: key, src: int64(start)}) // #nosec G115 -- sample sizes
	return nil
}

// append writes an encoded span to the rendition. Every span comes from
// an encoder with the same parameters: their parameter sets must match,
// the file has one sample entry.
func (r *rendition) append(e *encoding) error {
	if len(e.samples) == 0 {
		return nil
	}
	if r.sps == nil {
		r.sps, r.pps = e.sps, e.pps
	} else if !bytes.Equal(r.sps, e.sps) || !bytes.Equal(r.pps, e.pps) {
		return errors.New("transcode: the encoders of two spans produced different parameter sets")
	}
	if _, err := r.data.Write(e.data); err != nil {
		return err
	}
	for _, s := range e.samples {
		s.src += r.written
		r.samples = append(r.samples, s)
	}
	r.written += int64(len(e.data))
	return nil
}

// finish writes the rendition file.
func (r *rendition) finish(src *Source, durations []uint32, dir string) (RenditionResult, error) {
	if r.sps == nil || r.pps == nil {
		return RenditionResult{}, errors.New("transcode: the encoder produced no parameter sets")
	}
	for i := range r.samples {
		r.samples[i].dur = durations[min(i, len(durations)-1)]
	}
	stsd, err := avcSampleEntry(r.sps, r.pps)
	if err != nil {
		return RenditionResult{}, err
	}
	video := &muxTrack{id: 1, handler: "vide", timescale: src.Video.Timescale, language: "und", stsd: stsd,
		width: r.width, height: r.height, samples: r.samples, data: r.data}
	tracks := []*muxTrack{video}
	if a := audioTrack(src); a != nil {
		tracks = append(tracks, a)
	}
	path := filepath.Join(dir, r.spec.Name+".mp4")
	out, err := os.Create(path) // #nosec G304 -- a file of the job directory
	if err != nil {
		return RenditionResult{}, err
	}
	err = writeMP4(out, tracks)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return RenditionResult{}, fmt.Errorf("transcode: write %s: %w", path, err)
	}
	return RenditionResult{Name: r.spec.Name, Width: r.width, Height: r.height, Bandwidth: peakBandwidth(tracks), Path: path}, nil
}

// audioTrack copies the audio track of the source.
func audioTrack(src *Source) *muxTrack {
	a := src.Audio
	if a == nil || len(a.Samples) == 0 {
		return nil
	}
	samples := make([]muxSample, len(a.Samples))
	for i, s := range a.Samples {
		var dur uint64
		if i+1 < len(a.Samples) {
			dur = a.Samples[i+1].DTS - s.DTS
		} else if a.Duration > s.DTS {
			dur = a.Duration - s.DTS
		} else if i > 0 {
			dur = s.DTS - a.Samples[i-1].DTS
		}
		samples[i] = muxSample{size: s.Size, dur: uint32(min(dur, math.MaxUint32)), cto: s.CTO, sync: s.Sync, src: s.Offset} // #nosec G115 -- clamped
	}
	return &muxTrack{id: 2, handler: "soun", timescale: a.Timescale, language: a.Language, stsd: a.Stsd, samples: samples, data: src.f}
}

// avcSampleEntry builds the stsd box of an H.264 track.
func avcSampleEntry(sps, pps []byte) ([]byte, error) {
	trak := mp4.CreateEmptyTrak(1, 90000, "video", "und")
	if err := trak.SetAVCDescriptor("avc1", [][]byte{sps}, [][]byte{pps}, true); err != nil {
		return nil, fmt.Errorf("transcode: avc descriptor: %w", err)
	}
	var b bytes.Buffer
	if err := trak.Mdia.Minf.Stbl.Stsd.Encode(&b); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// peakBandwidth returns the highest bitrate of the file over keyInterval
// windows.
func peakBandwidth(tracks []*muxTrack) int {
	bits := map[int]float64{}
	for _, t := range tracks {
		var dts uint64
		for _, s := range t.samples {
			w := int(float64(dts) / float64(t.timescale) / keyInterval)
			bits[w] += float64(s.size) * 8
			dts += uint64(s.dur)
		}
	}
	peak := 0.0
	for _, b := range bits {
		peak = max(peak, b/keyInterval)
	}
	return int(math.Ceil(peak))
}

// encodeAll scales and encodes a picture into every rendition, each on its
// own goroutine: the encoders are independent.
func encodeAll(es []*encoding, pic *image.YCbCr, t float64, idr bool) error {
	if len(es) == 1 {
		return es[0].encode(pic, t, idr)
	}
	errs := make([]error, len(es))
	var wg sync.WaitGroup
	for i, e := range es {
		wg.Go(func() { errs[i] = e.encode(pic, t, idr) })
	}
	wg.Wait()
	return errors.Join(errs...)
}
