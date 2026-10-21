package transcode

import (
	"testing"

	"github.com/Eyevinn/mp4ff/mp4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAudioChunks(t *testing.T) {
	src, err := Open("testdata/h264_aac_360p.mp4")
	require.NoError(t, err)
	defer func() { _ = src.Close() }()
	dir := t.TempDir()

	chunks, err := AudioChunks(src, dir, 1, 1<<20)
	require.NoError(t, err)
	require.Len(t, chunks, 4, "3.03 s of audio in chunks of at most 1 s")
	var total float64
	for i, c := range chunks {
		assert.InDelta(t, total, c.Start, 1e-9, "chunk %d starts where the previous ends", i)
		assert.LessOrEqual(t, c.Seconds, 1.0)
		total += c.Seconds
		// Each chunk is an MP4 file with only an audio track.
		f, err := mp4.ReadMP4File(c.Path)
		require.NoError(t, err)
		require.Len(t, f.Moov.Traks, 1)
		assert.Equal(t, "soun", f.Moov.Traks[0].Mdia.Hdlr.HandlerType)
	}
	assert.InDelta(t, src.Seconds(), total, 0.1)

	one, err := AudioChunks(src, dir, 3600, 25<<20)
	require.NoError(t, err)
	require.Len(t, one, 1)

	src.Audio = nil
	_, err = AudioChunks(src, dir, 60, 1<<20)
	require.ErrorIs(t, err, ErrNoAudio)
}
