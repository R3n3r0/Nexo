package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/mediaworker/transcode"
)

// The audio sent to the transcription service in one request: what the
// OpenAI API accepts (25 MB), with a margin.
const (
	audioChunkSeconds = 10 * 60
	audioChunkBytes   = 24 << 20
	// maxManifest bounds the subtitle manifest read back.
	maxManifest = 1 << 20
	// manifestAttempts bounds the read-modify-write cycles of the
	// manifest racing with a user.
	manifestAttempts = 8
)

// subtitleTrack is a track the run made or reused: its cues and its entry
// of the manifest.
type subtitleTrack struct {
	track domain.MediaSubtitleTrack
	cues  []domain.SubtitleCue
	// written reports a file the run wrote (else reused).
	written bool
}

// subtitles makes the automatic subtitles of the rule: the speech
// transcribed (or a manual track of the spoken language used instead),
// then translated into the languages of the rule. A manual track is never
// overwritten: the automatic files live apart (MediaAutoSubtitleFile), and
// the manifest is updated conditionally. Without subtitles in the rule,
// the automatic tracks of earlier runs are removed. A failure is recorded
// in the status (SubtitlesError) and does not fail the job; only a
// cancellation does.
func (j *job) subtitles(ctx context.Context, dir string, src *transcode.Source) error {
	rule := j.job.Rule.Subtitles
	var made map[string]subtitleTrack
	if rule != nil {
		if err := j.step(ctx, "subtitles"); err != nil {
			return err
		}
		var err error
		made, err = j.makeSubtitles(ctx, dir, src, rule)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			j.status.SubtitlesError = err.Error()
			j.w.logger.Warn("worker: subtitles failed", "bucket", j.job.Bucket, "key", j.job.Key, "err", err)
		}
	}
	// The automatic tracks of earlier runs not made again: kept while
	// they apply to this content and the rule asks for their language (a
	// failed run keeps what it could not redo).
	wanted := func(t domain.MediaSubtitleTrack) bool {
		return rule != nil && t.Current(j.status.ETag) && (t.From == "" || slices.Contains(rule.Languages, t.Lang))
	}
	kept, err := j.publishSubtitles(ctx, made, wanted)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil && j.status.SubtitlesError == "" {
		j.status.SubtitlesError = err.Error()
	}
	j.status.Subtitles = kept
	return nil
}

// makeSubtitles writes the automatic tracks of the rule and returns every
// track the rule asks for, by language.
func (j *job) makeSubtitles(ctx context.Context, dir string, src *transcode.Source, rule *domain.MediaSubtitles) (map[string]subtitleTrack, error) {
	m, _, err := j.readManifest(ctx)
	if err != nil {
		return nil, err
	}
	made := map[string]subtitleTrack{}
	source, err := j.sourceTrack(ctx, dir, src, rule, m)
	if err != nil {
		return nil, err
	}
	j.status.Language = source.track.Lang
	if source.track.Auto {
		made[source.track.Lang] = source
	}
	var errs []error
	for _, lang := range rule.Languages {
		if lang == source.track.Lang {
			continue
		}
		if t, ok := m.Track(lang); ok && !t.Auto {
			continue // a manual track of that language: left alone
		}
		if t, ok := m.Track(lang); ok && t.Current(j.status.ETag) && t.From == source.track.Lang && !source.written {
			if cues, err := j.readCues(ctx, t); err == nil {
				made[lang] = subtitleTrack{track: t, cues: cues}
				continue
			}
		}
		tr, err := j.translate(ctx, source, lang)
		if err != nil {
			if ctx.Err() != nil {
				return made, ctx.Err()
			}
			errs = append(errs, fmt.Errorf("translation to %s: %w", lang, err))
			continue
		}
		made[lang] = tr
	}
	return made, errors.Join(errs...)
}

// sourceTrack returns the track of the spoken language: a manual track of
// the language of the rule, a current automatic one, or a new
// transcription.
func (j *job) sourceTrack(ctx context.Context, dir string, src *transcode.Source, rule *domain.MediaSubtitles, m domain.MediaSubtitlesManifestDoc) (subtitleTrack, error) {
	for _, t := range m.Tracks {
		// The track of the language of the rule (manual, or transcribed
		// from this content), else, the language detected, the
		// transcription of this content.
		reuse := t.Current(j.status.ETag) && (t.Lang == rule.Source || (rule.Source == "" && t.Auto && t.From == ""))
		if !reuse {
			continue
		}
		if cues, err := j.readCues(ctx, t); err == nil {
			return subtitleTrack{track: t, cues: cues}, nil
		}
	}
	tr := j.w.cfg.Transcriber
	if tr == nil {
		return subtitleTrack{}, errors.New("no transcription service is configured (nexo-media-worker --transcribe-url)")
	}
	chunks, err := transcode.AudioChunks(src, dir, audioChunkSeconds, audioChunkBytes)
	if err != nil {
		return subtitleTrack{}, err
	}
	lang := rule.Source
	var cues []domain.SubtitleCue
	for _, c := range chunks {
		t, err := tr.Transcribe(ctx, c.Path, lang)
		if err != nil {
			return subtitleTrack{}, fmt.Errorf("transcription: %w", err)
		}
		if lang == "" && t.Language != "" {
			lang = t.Language
		}
		for _, s := range t.Segments {
			cues = append(cues, segmentCue(c.Start+s.Start, c.Start+s.End, s.Text))
		}
	}
	if lang == "" {
		return subtitleTrack{}, errors.New("transcription: the service reported no language: set the source language of the rule")
	}
	if len(cues) == 0 {
		return subtitleTrack{}, errors.New("transcription: no speech")
	}
	// A manual track of the detected language wins over the transcription.
	if t, ok := m.Track(lang); ok && !t.Auto {
		if cues, err := j.readCues(ctx, t); err == nil {
			return subtitleTrack{track: t, cues: cues}, nil
		}
	}
	return j.writeAuto(ctx, domain.MediaSubtitleTrack{Lang: lang, Engine: tr.Engine()}, cues)
}

// translate writes the automatic translation of the source track.
func (j *job) translate(ctx context.Context, source subtitleTrack, lang string) (subtitleTrack, error) {
	tl := j.w.cfg.Translator
	if tl == nil {
		return subtitleTrack{}, errors.New("no translation service is configured (nexo-media-worker --translate-url)")
	}
	texts := make([]string, len(source.cues))
	for i, c := range source.cues {
		texts[i] = c.Text
	}
	out, err := tl.Translate(ctx, texts, source.track.Lang, lang)
	if err != nil {
		return subtitleTrack{}, err
	}
	cues := make([]domain.SubtitleCue, 0, len(out))
	for i, text := range out {
		c := source.cues[i]
		c.Text = truncate(strings.TrimSpace(text))
		if c.Text != "" {
			cues = append(cues, c)
		}
	}
	return j.writeAuto(ctx, domain.MediaSubtitleTrack{Lang: lang, From: source.track.Lang, Engine: tl.Engine()}, cues)
}

// writeAuto writes the file of an automatic track.
func (j *job) writeAuto(ctx context.Context, t domain.MediaSubtitleTrack, cues []domain.SubtitleCue) (subtitleTrack, error) {
	doc := domain.FormatWebVTT(cues)
	cues, err := domain.ParseSubtitles(doc) // sanitized and bounded as an upload
	if err != nil {
		return subtitleTrack{}, err
	}
	doc = domain.FormatWebVTT(cues)
	if err := j.put(ctx, domain.MediaAutoSubtitleFile(t.Lang), doc, "text/vtt; charset=utf-8"); err != nil {
		return subtitleTrack{}, err
	}
	t.Auto, t.ETag, t.Cues, t.Size, t.Updated = true, j.status.ETag, len(cues), int64(len(doc)), time.Now().UTC()
	return subtitleTrack{track: t, cues: cues, written: true}, nil
}

// publishSubtitles updates the manifest with the automatic tracks made
// and removes the other automatic tracks unless wanted; then the automatic
// files the manifest no longer names, and the older versions of the
// others. It returns the languages of the automatic tracks of the
// manifest.
func (j *job) publishSubtitles(ctx context.Context, made map[string]subtitleTrack, wanted func(domain.MediaSubtitleTrack) bool) ([]string, error) {
	var kept []string
	err := j.updateManifest(ctx, func(m *domain.MediaSubtitlesManifestDoc) bool {
		changed := false
		for _, t := range slices.Clone(m.Tracks) {
			if _, ok := made[t.Lang]; t.Auto && !ok && !wanted(t) {
				m.Remove(t.Lang)
				changed = true
			}
		}
		for lang, t := range made {
			cur, ok := m.Track(lang)
			if ok && !cur.Auto {
				continue // a manual track arrived meanwhile: it wins
			}
			if !ok || cur != t.track {
				m.Put(t.track)
				changed = true
			}
		}
		kept = kept[:0]
		for _, t := range m.Tracks {
			if t.Auto {
				kept = append(kept, t.Lang)
			}
		}
		return changed
	})
	if err != nil {
		return nil, err
	}
	// The automatic files: those the manifest names keep their latest
	// version; the others go.
	versions, err := j.w.store.List(ctx, j.job.Bucket, j.sidecar+domain.MediaAutoSubtitlesDir)
	if err != nil {
		return kept, err
	}
	var stale []ObjectVersion
	for _, v := range versions {
		lang := strings.TrimSuffix(strings.TrimPrefix(v.Key, j.sidecar+domain.MediaAutoSubtitlesDir), ".vtt")
		if !v.Latest || !slices.Contains(kept, lang) {
			stale = append(stale, v)
		}
	}
	if len(stale) > 0 {
		if err := j.w.store.Delete(ctx, j.job.Bucket, stale); err != nil {
			j.w.logger.Warn("worker: stale subtitles not removed", "bucket", j.job.Bucket, "key", j.job.Key, "err", err)
		}
	}
	return kept, nil
}

// readManifest reads the subtitle manifest and its ETag ("" when there is
// none).
func (j *job) readManifest(ctx context.Context) (domain.MediaSubtitlesManifestDoc, string, error) {
	var m domain.MediaSubtitlesManifestDoc
	data, etag, err := j.w.store.Get(ctx, j.job.Bucket, j.sidecar+domain.MediaSubtitlesManifest, maxManifest)
	if errors.Is(err, ErrNotFound) {
		return m, "", nil
	}
	if err != nil {
		return m, "", err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, "", fmt.Errorf("the subtitle manifest is not valid JSON: %w", err)
	}
	return m, etag, nil
}

// updateManifest applies change to the manifest and writes it back when
// change reports a change, conditionally on the version read: a user
// writing meanwhile makes it start over.
func (j *job) updateManifest(ctx context.Context, change func(*domain.MediaSubtitlesManifestDoc) bool) error {
	for range manifestAttempts {
		m, etag, err := j.readManifest(ctx)
		if err != nil {
			return err
		}
		if !change(&m) {
			return nil
		}
		if m.Tracks == nil {
			m.Tracks = []domain.MediaSubtitleTrack{}
		}
		data, err := json.MarshalIndent(m, "", "  ")
		if err != nil {
			return err
		}
		err = j.w.store.PutIf(ctx, j.job.Bucket, j.sidecar+domain.MediaSubtitlesManifest, data, "application/json", etag)
		if errors.Is(err, ErrPrecondition) {
			continue
		}
		return err
	}
	return errors.New("the subtitle manifest keeps changing")
}

// readCues reads the cues of a track of the manifest.
func (j *job) readCues(ctx context.Context, t domain.MediaSubtitleTrack) ([]domain.SubtitleCue, error) {
	data, _, err := j.w.store.Get(ctx, j.job.Bucket, j.sidecar+t.File(), domain.MaxSubtitleBytes)
	if err != nil {
		return nil, err
	}
	return domain.ParseSubtitles(data)
}

// segmentCue is the cue of a transcribed segment, its times rounded to the
// millisecond.
func segmentCue(start, end float64, text string) domain.SubtitleCue {
	ms := func(s float64) time.Duration { return time.Duration(math.Round(max(s, 0)*1000)) * time.Millisecond }
	c := domain.SubtitleCue{Start: ms(start), End: ms(end), Text: truncate(strings.TrimSpace(text))}
	if c.End <= c.Start {
		c.End = c.Start + time.Millisecond
	}
	return c
}

// truncate bounds the text of a cue (domain.MaxSubtitleCueText), at a
// rune boundary.
func truncate(s string) string {
	if len(s) <= domain.MaxSubtitleCueText {
		return s
	}
	s = s[:domain.MaxSubtitleCueText]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
