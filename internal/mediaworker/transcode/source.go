// Package transcode derives the outputs of a video for the media worker:
// lower-quality renditions (H.264 Constrained Baseline with the source
// audio copied), a poster frame and a timeline thumbnail sprite. It reads
// progressive MP4 files with H.264 video and decodes and encodes through
// OpenH264; scaling and JPEG are pure Go.
package transcode

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/Eyevinn/mp4ff/avc"
	"github.com/Eyevinn/mp4ff/mp4"
)

// ErrUnsupported marks a source the worker cannot process (not an MP4
// with H.264 video in a progressive layout).
var ErrUnsupported = errors.New("unsupported video")

// Sample is a sample of a track, in decoding order.
type Sample struct {
	Offset int64
	Size   uint32
	// DTS is the decoding time in the track timescale.
	DTS uint64
	// CTO is the composition time offset (PTS - DTS).
	CTO  int32
	Sync bool
}

// PTS is the presentation time of the sample.
func (s Sample) PTS() int64 { return int64(s.DTS) + int64(s.CTO) } // #nosec G115 -- media times

// Track is a track of the source.
type Track struct {
	ID        uint32
	Timescale uint32
	Samples   []Sample
	// Duration of the track in its timescale.
	Duration uint64
	// Stsd is the encoded sample description box (copied as is for the
	// audio of the renditions).
	Stsd     []byte
	Language string
	trak     *mp4.TrakBox
}

// Source is an opened MP4 file.
type Source struct {
	f      *os.File
	Video  *Track
	Audio  *Track
	Width  int
	Height int
	// SPS and PPS of the video (from avcC).
	SPS, PPS [][]byte
}

// Open reads the index of an MP4 file: its moov box and the sample tables
// of its first H.264 video track and first audio track.
func Open(path string) (*Source, error) {
	f, err := os.Open(path) // #nosec G304 -- a temporary file of the worker
	if err != nil {
		return nil, err
	}
	src, err := open(f)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return src, nil
}

func open(f *os.File) (*Source, error) {
	file, err := mp4.DecodeFile(f, mp4.WithDecodeMode(mp4.DecModeLazyMdat))
	if err != nil {
		return nil, fmt.Errorf("%w: not an MP4 file: %w", ErrUnsupported, err)
	}
	if file.Moov == nil || file.IsFragmented() {
		return nil, fmt.Errorf("%w: a progressive MP4 file is needed (no moov box or fragmented)", ErrUnsupported)
	}
	src := &Source{f: f}
	for _, trak := range file.Moov.Traks {
		if trak.Mdia == nil || trak.Mdia.Hdlr == nil || trak.Mdia.Minf == nil || trak.Mdia.Minf.Stbl == nil {
			continue
		}
		switch trak.Mdia.Hdlr.HandlerType {
		case "vide":
			if src.Video != nil {
				continue
			}
			if err := src.openVideo(trak); err != nil {
				return nil, err
			}
		case "soun":
			if src.Audio != nil {
				continue
			}
			t, err := newTrack(trak)
			if err != nil {
				return nil, err
			}
			src.Audio = t
		}
	}
	if src.Video == nil {
		return nil, fmt.Errorf("%w: no H.264 video track", ErrUnsupported)
	}
	return src, nil
}

func (s *Source) openVideo(trak *mp4.TrakBox) error {
	stsd := trak.Mdia.Minf.Stbl.Stsd
	if stsd == nil || len(stsd.Children) == 0 {
		return fmt.Errorf("%w: video track without sample description", ErrUnsupported)
	}
	entry, ok := stsd.Children[0].(*mp4.VisualSampleEntryBox)
	if !ok || entry.AvcC == nil {
		name := "unknown"
		if len(stsd.Children) > 0 {
			name = stsd.Children[0].Type()
		}
		return fmt.Errorf("%w: video codec %s (H.264 is needed)", ErrUnsupported, name)
	}
	t, err := newTrack(trak)
	if err != nil {
		return err
	}
	s.Video = t
	s.Width, s.Height = int(entry.Width), int(entry.Height)
	s.SPS, s.PPS = entry.AvcC.SPSnalus, entry.AvcC.PPSnalus
	if len(s.SPS) > 0 {
		if sps, err := avc.ParseSPSNALUnit(s.SPS[0], false); err == nil && sps.Width > 0 {
			s.Width, s.Height = int(sps.Width), int(sps.Height)
		}
	}
	if s.Width <= 0 || s.Height <= 0 || len(s.SPS) == 0 || len(s.PPS) == 0 {
		return fmt.Errorf("%w: video track without size or parameter sets", ErrUnsupported)
	}
	return nil
}

// newTrack builds the sample table of a track.
func newTrack(trak *mp4.TrakBox) (*Track, error) {
	stbl := trak.Mdia.Minf.Stbl
	if trak.Mdia.Mdhd == nil || trak.Mdia.Mdhd.Timescale == 0 || stbl.Stsz == nil || stbl.Stsc == nil || stbl.Stts == nil ||
		(stbl.Stco == nil && stbl.Co64 == nil) || stbl.Stsd == nil {
		return nil, fmt.Errorf("%w: incomplete sample table", ErrUnsupported)
	}
	var stsd bytes.Buffer
	if err := stbl.Stsd.Encode(&stsd); err != nil {
		return nil, fmt.Errorf("transcode: encode stsd: %w", err)
	}
	t := &Track{
		ID: trak.Tkhd.TrackID, Timescale: trak.Mdia.Mdhd.Timescale, Duration: trak.Mdia.Mdhd.Duration,
		Stsd: stsd.Bytes(), Language: trak.Mdia.Mdhd.GetLanguage(), trak: trak,
	}
	n := stbl.Stsz.GetNrSamples()
	if n == 0 {
		return t, nil
	}
	durs, err := stbl.Stts.SampleDurations(n)
	if err != nil {
		return nil, fmt.Errorf("%w: stts: %w", ErrUnsupported, err)
	}
	var ctos []int32
	if stbl.Ctts != nil {
		if ctos, err = stbl.Ctts.CompositionTimeOffsets(n); err != nil {
			return nil, fmt.Errorf("%w: ctts: %w", ErrUnsupported, err)
		}
	}
	t.Samples = make([]Sample, n)
	var dts uint64
	chunk, next := 0, 0 // the current chunk (1-based) and its first sample (1-based)
	var off int64
	for i := range int(n) {
		nr := i + 1
		if nr >= next {
			c, first, err := stbl.Stsc.ChunkNrFromSampleNr(nr)
			if err != nil {
				return nil, fmt.Errorf("%w: stsc: %w", ErrUnsupported, err)
			}
			if c != chunk {
				chunk = c
				o, err := chunkOffset(stbl, c)
				if err != nil {
					return nil, err
				}
				off = int64(o) // #nosec G115 -- file offsets
				// Skip the samples of the chunk before this one.
				for j := first; j < nr; j++ {
					off += int64(stbl.Stsz.GetSampleSize(j))
				}
			}
			next = nr + 1
		}
		size := stbl.Stsz.GetSampleSize(nr)
		s := Sample{Offset: off, Size: size, DTS: dts, Sync: stbl.Stss == nil || stbl.Stss.IsSyncSample(uint32(nr))} // #nosec G115 -- sample numbers
		if ctos != nil {
			s.CTO = ctos[i]
		}
		t.Samples[i] = s
		off += int64(size)
		dts += uint64(durs[i])
	}
	if t.Duration == 0 {
		t.Duration = dts
	}
	return t, nil
}

func chunkOffset(stbl *mp4.StblBox, chunkNr int) (uint64, error) {
	var (
		o   uint64
		err error
	)
	if stbl.Co64 != nil {
		o, err = stbl.Co64.GetOffset(chunkNr)
	} else {
		o, err = stbl.Stco.GetOffset(chunkNr)
	}
	if err != nil {
		return 0, fmt.Errorf("%w: chunk offset: %w", ErrUnsupported, err)
	}
	return o, nil
}

// Seconds is the duration of the video in seconds.
func (s *Source) Seconds() float64 {
	return float64(s.Video.Duration) / float64(s.Video.Timescale)
}

// ReadSample reads the data of a sample.
func (s *Source) ReadSample(sm Sample) ([]byte, error) {
	buf := make([]byte, sm.Size)
	if _, err := s.f.ReadAt(buf, sm.Offset); err != nil {
		return nil, fmt.Errorf("transcode: read sample at %d: %w", sm.Offset, err)
	}
	return buf, nil
}

// accessUnit returns a video sample as an Annex B access unit; the
// parameter sets are prepended to a sync sample.
func (s *Source) accessUnit(sm Sample) ([]byte, error) {
	data, err := s.ReadSample(sm)
	if err != nil {
		return nil, err
	}
	var out []byte
	if sm.Sync {
		for _, ps := range slices.Concat(s.SPS, s.PPS) {
			out = append(out, 0, 0, 0, 1)
			out = append(out, ps...)
		}
	}
	return append(out, avc.ConvertSampleToByteStream(data)...), nil
}

// presentationTimes returns the presentation times of the video samples
// in display order (the order of the decoded pictures).
func (s *Source) presentationTimes() []int64 {
	pts := make([]int64, len(s.Video.Samples))
	for i, sm := range s.Video.Samples {
		pts[i] = sm.PTS()
	}
	slices.Sort(pts)
	return pts
}

// Close closes the file.
func (s *Source) Close() error { return s.f.Close() }
