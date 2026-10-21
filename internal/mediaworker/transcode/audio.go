package transcode

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// ErrNoAudio reports a video without an audio track.
var ErrNoAudio = errors.New("transcode: the video has no audio track")

// AudioChunk is a part of the audio track of a video, an M4A file.
type AudioChunk struct {
	Path string
	// Start is the time of the chunk in the video, in seconds.
	Start float64
	// Seconds is the duration of the chunk.
	Seconds float64
}

// AudioChunks copies the audio track of the source (as is, no decoding)
// into M4A files of at most maxSeconds and maxBytes each, cut between
// audio frames: the parts a transcription service accepts. The files are
// written in dir.
func AudioChunks(src *Source, dir string, maxSeconds float64, maxBytes int64) ([]AudioChunk, error) {
	a := audioTrack(src)
	if a == nil {
		return nil, ErrNoAudio
	}
	if maxSeconds <= 0 || maxBytes <= 0 {
		return nil, errors.New("transcode: the bounds of the audio chunks must be positive")
	}
	ts := float64(a.timescale)
	var (
		out   []AudioChunk
		first int
		size  int64
		dur   uint64
		start uint64
	)
	flush := func(end int) error {
		if end == first {
			return nil
		}
		path := filepath.Join(dir, "audio-"+strconv.Itoa(len(out))+".m4a")
		t := *a
		t.id = 1
		t.samples = a.samples[first:end]
		f, err := os.Create(path) // #nosec G304 -- a file of the job directory
		if err != nil {
			return err
		}
		err = writeMP4(f, []*muxTrack{&t})
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fmt.Errorf("transcode: write %s: %w", path, err)
		}
		out = append(out, AudioChunk{Path: path, Start: float64(start) / ts, Seconds: float64(dur) / ts})
		start += dur
		first, size, dur = end, 0, 0
		return nil
	}
	for i, s := range a.samples {
		if i > first && (float64(dur+uint64(s.dur))/ts > maxSeconds || size+int64(s.size) > maxBytes-64<<10) {
			if err := flush(i); err != nil {
				return nil, err
			}
		}
		size += int64(s.size)
		dur += uint64(s.dur)
	}
	if err := flush(len(a.samples)); err != nil {
		return nil, err
	}
	return out, nil
}
