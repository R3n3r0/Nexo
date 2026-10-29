package transcode

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// splice writes a copy made of the samples of a or b: the GOP g (from a
// sync sample to the next) from b when pick(g), else from a; with the
// audio of a. As a stream the server marked, remuxed by a downloader.
func splice(t *testing.T, a, b *Source, pick func(gop int) bool, path string) {
	t.Helper()
	data, err := os.CreateTemp(t.TempDir(), "splice-*.h264")
	require.NoError(t, err)
	defer data.Close()
	var samples []muxSample
	var off int64
	gop := -1
	for i, s := range a.Video.Samples {
		if s.Sync {
			gop++
		}
		from := a
		if pick(gop) {
			from = b
		}
		src := from.Video.Samples[i]
		buf := make([]byte, src.Size)
		_, err := from.f.ReadAt(buf, src.Offset)
		require.NoError(t, err)
		_, err = data.Write(buf)
		require.NoError(t, err)
		var dur uint32
		if i+1 < len(a.Video.Samples) {
			dur = uint32(a.Video.Samples[i+1].DTS - s.DTS) // #nosec G115 -- test durations
		} else {
			dur = uint32(a.Video.Samples[i].DTS - a.Video.Samples[i-1].DTS) // #nosec G115 -- test durations
		}
		samples = append(samples, muxSample{size: src.Size, dur: dur, cto: src.CTO, sync: s.Sync, src: off})
		off += int64(src.Size)
	}
	video := &muxTrack{id: 1, handler: "vide", timescale: a.Video.Timescale, language: "und", stsd: a.Video.Stsd,
		width: a.Width, height: a.Height, samples: samples, data: data}
	out, err := os.Create(path)
	require.NoError(t, err)
	defer out.Close()
	require.NoError(t, writeMP4(out, []*muxTrack{video}))
}

// gopVotes sums the scores of the votes by GOP (keyInterval seconds).
func gopVotes(votes []TraceVote) map[int]float64 {
	out := map[int]float64{}
	for _, v := range votes {
		out[int(v.T/keyInterval)] += v.Score
	}
	return out
}

// TestTrace: the votes of a copy spliced from the two variants tell, GOP
// by GOP, which variant it came from; a copy re-encoded at another size
// and a lower bitrate too.
//
// covers: S-OBJ-19
func TestTrace(t *testing.T) {
	lib := library(t)
	src := openFixture(t)
	dir := t.TempDir()
	res, err := Renditions(context.Background(), lib, src, []RenditionSpec{{Name: "360p", Height: 360, Bitrate: 800_000, Forensic: true}}, dir, nil)
	require.NoError(t, err)
	a, err := Open(res[0].Path)
	require.NoError(t, err)
	defer a.Close()
	b, err := Open(res[0].PathB)
	require.NoError(t, err)
	defer b.Close()
	renditions := []TraceRendition{{Name: "360p", A: a, B: b}}

	// GOP 0 from A, GOP 1 from B (the fixture is 3 s: two GOPs).
	pick := func(g int) bool { return g%2 == 1 }
	leakPath := filepath.Join(dir, "leak.mp4")
	splice(t, a, b, pick, leakPath)
	leak, err := Open(leakPath)
	require.NoError(t, err)
	defer leak.Close()
	votes, err := Trace(context.Background(), lib, leak, renditions, TraceOptions{})
	require.NoError(t, err)
	require.NotEmpty(t, votes)
	for _, v := range votes {
		assert.Equal(t, "360p", v.Rendition)
		if pick(int(v.T / keyInterval)) {
			assert.Less(t, v.Score, -0.5, "t=%.2f from B", v.T)
		} else {
			assert.Greater(t, v.Score, 0.5, "t=%.2f from A", v.T)
		}
	}

	// Re-encoded at 240p and a third of the bitrate: every picture still
	// on the right side (about ±0.75).
	re, err := Renditions(context.Background(), lib, leak, []RenditionSpec{{Name: "240p", Height: 240, Bitrate: 250_000}}, t.TempDir(), nil)
	require.NoError(t, err)
	reenc, err := Open(re[0].Path)
	require.NoError(t, err)
	defer reenc.Close()
	votes, err = Trace(context.Background(), lib, reenc, renditions, TraceOptions{})
	require.NoError(t, err)
	for _, v := range votes {
		if pick(int(v.T / keyInterval)) {
			assert.Less(t, v.Score, -0.3, "re-encoded, t=%.2f from B", v.T)
		} else {
			assert.Greater(t, v.Score, 0.3, "re-encoded, t=%.2f from A", v.T)
		}
	}
	gops := gopVotes(votes)
	require.Len(t, gops, 2)
	for g, sum := range gops {
		if pick(g) {
			assert.Negative(t, sum, "GOP %d from B", g)
		} else {
			assert.Positive(t, sum, "GOP %d from A", g)
		}
	}
}
