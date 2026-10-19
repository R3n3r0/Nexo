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

// rendition is the state of a rendition being encoded.
type rendition struct {
	spec    RenditionSpec
	width   int
	height  int
	enc     *openh264.Encoder
	scaler  Scaler
	data    *os.File // the video samples
	written int64
	samples []muxSample
	sps     []byte
	pps     []byte
}

// Renditions decodes the video of src once and encodes every spec from
// it, then writes each rendition, with the source audio copied, to
// dir/<name>.mp4.
func Renditions(ctx context.Context, lib *openh264.Library, src *Source, specs []RenditionSpec, dir string, progress Progress) (results []RenditionResult, err error) {
	if len(specs) == 0 {
		return nil, nil
	}
	samples := src.Video.Samples
	if len(samples) == 0 {
		return nil, fmt.Errorf("%w: the video track has no samples", ErrUnsupported)
	}
	fps := float64(len(samples)) / max(src.Seconds(), 0.001)
	var rs []*rendition
	defer func() {
		for _, r := range rs {
			r.enc.Close()
			_ = r.data.Close()
			_ = os.Remove(r.data.Name())
		}
	}()
	for _, spec := range specs {
		w, h := FitHeight(src.Width, src.Height, spec.Height)
		enc, err := lib.NewEncoder(openh264.EncoderParams{Width: w, Height: h, Bitrate: spec.Bitrate, FrameRate: min(fps, 60)})
		if err != nil {
			return nil, err
		}
		f, err := os.CreateTemp(dir, spec.Name+"-*.h264")
		if err != nil {
			enc.Close()
			return nil, err
		}
		rs = append(rs, &rendition{spec: spec, width: w, height: h, enc: enc, data: f})
	}
	dec, err := lib.NewDecoder()
	if err != nil {
		return nil, err
	}
	defer dec.Close()

	pts := src.presentationTimes()
	scale := float64(src.Video.Timescale)
	n := 0
	nextKey := 0.0
	emit := func(pic *image.YCbCr) error {
		if n >= len(pts) {
			return nil // more pictures than samples: never seen, ignored
		}
		t := float64(pts[n]-pts[0]) / scale
		idr := n == 0 || t >= nextKey
		if idr {
			nextKey = math.Floor(t/keyInterval+1) * keyInterval
		}
		if err := encodeAll(rs, pic, t, idr); err != nil {
			return err
		}
		n++
		if progress != nil && n%25 == 0 {
			progress(float64(n) / float64(len(pts)))
		}
		return nil
	}
	// The decoder runs ahead of the encoders, on its own goroutine: decoding
	// the next pictures overlaps with scaling and encoding this one.
	decodeCtx, stop := context.WithCancel(ctx)
	defer stop()
	pics, decoded := decodeAhead(decodeCtx, dec, src, samples)
	for pic := range pics {
		if err := emit(pic); err != nil {
			stop()
			<-decoded
			return nil, err
		}
	}
	if err := <-decoded; err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, fmt.Errorf("%w: no picture could be decoded", ErrUnsupported)
	}
	durations := frameDurations(pts[:n], src.Video)
	for _, r := range rs {
		res, err := r.finish(src, durations, dir)
		if err != nil {
			return nil, err
		}
		results = append(results, res)
	}
	return results, nil
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
func (r *rendition) encode(pic *image.YCbCr, t float64, idr bool) error {
	scaled := r.scaler.Scale(pic, r.width, r.height)
	nalus, key, err := r.enc.Encode(scaled, int64(t*1000), idr)
	if err != nil {
		return err
	}
	var sample []byte
	for _, nal := range nalus {
		if len(nal) == 0 {
			continue
		}
		switch nal[0] & 0x1f {
		case 7: // SPS: in the sample entry
			if r.sps == nil {
				r.sps = nal
			}
			continue
		case 8: // PPS
			if r.pps == nil {
				r.pps = nal
			}
			continue
		}
		sample = binary.BigEndian.AppendUint32(sample, uint32(len(nal))) // #nosec G115 -- NAL sizes
		sample = append(sample, nal...)
	}
	if len(sample) == 0 {
		return errors.New("transcode: the encoder produced an empty picture")
	}
	if _, err := r.data.Write(sample); err != nil {
		return err
	}
	r.samples = append(r.samples, muxSample{size: uint32(len(sample)), sync: key, src: r.written}) // #nosec G115 -- sample sizes
	r.written += int64(len(sample))
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
func encodeAll(rs []*rendition, pic *image.YCbCr, t float64, idr bool) error {
	if len(rs) == 1 {
		return rs[0].encode(pic, t, idr)
	}
	errs := make([]error, len(rs))
	var wg sync.WaitGroup
	for i, r := range rs {
		wg.Go(func() { errs[i] = r.encode(pic, t, idr) })
	}
	wg.Wait()
	return errors.Join(errs...)
}
