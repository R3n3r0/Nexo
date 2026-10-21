package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/mediaworker/speech"
)

// fakeTranscriber transcribes every chunk into one English segment.
type fakeTranscriber struct {
	calls atomic.Int32
	err   error
}

func (f *fakeTranscriber) Engine() string { return "fake:whisper" }

func (f *fakeTranscriber) Transcribe(_ context.Context, path, lang string) (*speech.Transcript, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	return &speech.Transcript{Language: "en", Segments: []speech.Segment{{Start: 0.5, End: 2, Text: " Hello <script>x</script>world "}}}, nil
}

// fakeTranslator prefixes the lines with the target language.
type fakeTranslator struct{ calls atomic.Int32 }

func (f *fakeTranslator) Engine() string { return "fake:mt" }

func (f *fakeTranslator) Translate(_ context.Context, texts []string, from, to string) ([]string, error) {
	f.calls.Add(1)
	out := make([]string, len(texts))
	for i, t := range texts {
		out[i] = "[" + from + ">" + to + "] " + t
	}
	return out, nil
}

func manifestOf(t *testing.T, s *memStore, dir string) domain.MediaSubtitlesManifestDoc {
	t.Helper()
	data, ok := s.get("b", dir+domain.MediaSubtitlesManifest)
	require.True(t, ok, "the manifest")
	var m domain.MediaSubtitlesManifestDoc
	require.NoError(t, json.Unmarshal(data, &m))
	return m
}

func subtitlesFixture(t *testing.T) (*memStore, *Worker, []byte, string, *fakeTranscriber, *fakeTranslator) {
	t.Helper()
	video, err := os.ReadFile(fixture)
	require.NoError(t, err)
	s := newMemStore()
	require.NoError(t, s.Put(t.Context(), "b", "talks/a.mp4", video, "video/mp4"))
	w := newWorker(t, s, nil, "")
	tr, tl := &fakeTranscriber{}, &fakeTranslator{}
	w.cfg.Transcriber, w.cfg.Translator = tr, tl
	return s, w, video, domain.MediaSidecarDir("talks/a.mp4", domain.MediaNullVersion), tr, tl
}

// TestSubtitles: the speech transcribed and translated into automatic
// tracks; a manual track is never overwritten, and a run reuses what
// still applies.
func TestSubtitles(t *testing.T) {
	s, w, video, dir, tr, tl := subtitlesFixture(t)
	// A manual Italian track uploaded before the run.
	manual := "WEBVTT\n\n00:00:00.000 --> 00:00:01.000\nCiao (a mano)\n"
	require.NoError(t, s.Put(t.Context(), "b", dir+domain.MediaSubtitleFile("it"), []byte(manual), "text/vtt"))
	m := domain.MediaSubtitlesManifestDoc{Tracks: []domain.MediaSubtitleTrack{{Lang: "it", Cues: 1, UpdatedBy: "alice"}}}
	data, _ := json.Marshal(m)
	require.NoError(t, s.Put(t.Context(), "b", dir+domain.MediaSubtitlesManifest, data, "application/json"))

	rule := &domain.MediaRule{ID: "talks", Subtitles: &domain.MediaSubtitles{Languages: []string{"it", "de"}}}
	job := domain.MediaJob{Action: domain.MediaJobProcess, Bucket: "b", Key: "talks/a.mp4", ETag: etagOf(video), Rule: rule}
	runJob(t, w, job)

	st := s.status(t, "b", dir)
	assert.Equal(t, domain.MediaStateDone, st.State, st.Error)
	assert.Empty(t, st.SubtitlesError)
	assert.Equal(t, "en", st.Language)
	assert.Equal(t, []string{"de", "en"}, st.Subtitles)
	assert.EqualValues(t, 1, tr.calls.Load())
	assert.EqualValues(t, 1, tl.calls.Load(), "Italian is manual: only German is translated")

	got := manifestOf(t, s, dir)
	require.Len(t, got.Tracks, 3)
	de, _ := got.Track("de")
	assert.Equal(t, domain.MediaSubtitleTrack{Lang: "de", Auto: true, ETag: etagOf(video), From: "en", Engine: "fake:mt", Cues: 1, Size: de.Size, Updated: de.Updated}, de)
	en, _ := got.Track("en")
	assert.Equal(t, "fake:whisper", en.Engine)
	it, _ := got.Track("it")
	assert.False(t, it.Auto)
	assert.Equal(t, "alice", it.UpdatedBy)

	vtt, _ := s.get("b", dir+domain.MediaAutoSubtitleFile("en"))
	assert.Equal(t, "WEBVTT\n\n00:00:00.500 --> 00:00:02.000\nHello xworld\n", string(vtt), "sanitized")
	vtt, _ = s.get("b", dir+domain.MediaAutoSubtitleFile("de"))
	assert.Contains(t, string(vtt), "[en>de] Hello xworld")
	kept, _ := s.get("b", dir+domain.MediaSubtitleFile("it"))
	assert.Equal(t, manual, string(kept), "the manual track is untouched")
	assert.False(t, strings.Contains(strings.Join(s.keys("b", dir), ","), "auto/it"), "no automatic Italian file")

	// Again on the same content: nothing to redo.
	runJob(t, w, job)
	assert.EqualValues(t, 1, tr.calls.Load(), "the transcription of this content is reused")
	assert.EqualValues(t, 1, tl.calls.Load())
	assert.Equal(t, []string{"de", "en"}, s.status(t, "b", dir).Subtitles)

	// The rule no longer asks for subtitles: the automatic tracks go, the
	// manual one stays.
	runJob(t, w, domain.MediaJob{Action: domain.MediaJobProcess, Bucket: "b", Key: "talks/a.mp4", ETag: etagOf(video),
		Rule: &domain.MediaRule{ID: "talks", Poster: false, Thumbnails: nil, Renditions: nil, Subtitles: nil}})
	got = manifestOf(t, s, dir)
	require.Len(t, got.Tracks, 1)
	assert.Equal(t, "it", got.Tracks[0].Lang)
	assert.Equal(t, []string{dir + "status.json", dir + "subtitles/it.vtt", dir + "subtitles/manifest.json"}, s.keys("b", dir))
}

// TestSubtitlesManualArrivesMeanwhile: a user uploads a track of a
// language while the worker publishes its own: the manual track wins and
// the automatic file goes.
func TestSubtitlesManualArrivesMeanwhile(t *testing.T) {
	s, w, video, dir, _, _ := subtitlesFixture(t)
	s.putIfRace = func() {
		require.NoError(t, s.Put(context.Background(), "b", dir+domain.MediaSubtitleFile("de"), []byte("WEBVTT\n\n00:00.000 --> 00:01.000\nHallo\n"), "text/vtt"))
		m := domain.MediaSubtitlesManifestDoc{Tracks: []domain.MediaSubtitleTrack{{Lang: "de", Cues: 1, UpdatedBy: "bob"}}}
		data, _ := json.Marshal(m)
		require.NoError(t, s.Put(context.Background(), "b", dir+domain.MediaSubtitlesManifest, data, "application/json"))
	}
	runJob(t, w, domain.MediaJob{Action: domain.MediaJobProcess, Bucket: "b", Key: "talks/a.mp4", ETag: etagOf(video),
		Rule: &domain.MediaRule{ID: "talks", Subtitles: &domain.MediaSubtitles{Source: "en", Languages: []string{"de"}}}})
	got := manifestOf(t, s, dir)
	require.Len(t, got.Tracks, 2)
	de, _ := got.Track("de")
	assert.False(t, de.Auto, "the manual track wins")
	assert.Equal(t, "bob", de.UpdatedBy)
	_, ok := s.get("b", dir+domain.MediaAutoSubtitleFile("de"))
	assert.False(t, ok, "the automatic German file is removed")
	data, _ := s.get("b", dir+domain.MediaSubtitleFile("de"))
	assert.Contains(t, string(data), "Hallo")
	assert.Equal(t, []string{"en"}, s.status(t, "b", dir).Subtitles)
}

// TestSubtitlesFailureKeepsTheJob: without a transcription service, or
// when it fails, the job is done all the same and the error recorded.
func TestSubtitlesFailureKeepsTheJob(t *testing.T) {
	s, w, video, dir, tr, _ := subtitlesFixture(t)
	rule := &domain.MediaRule{ID: "talks", Subtitles: &domain.MediaSubtitles{Source: "en"}}
	job := domain.MediaJob{Action: domain.MediaJobProcess, Bucket: "b", Key: "talks/a.mp4", ETag: etagOf(video), Rule: rule}

	tr.err = errors.New("service down")
	runJob(t, w, job)
	st := s.status(t, "b", dir)
	assert.Equal(t, domain.MediaStateDone, st.State)
	assert.Contains(t, st.SubtitlesError, "service down")

	w.cfg.Transcriber = nil
	runJob(t, w, job)
	st = s.status(t, "b", dir)
	assert.Equal(t, domain.MediaStateDone, st.State)
	assert.Contains(t, st.SubtitlesError, "--transcribe-url")
	assert.Empty(t, st.Subtitles)
}
