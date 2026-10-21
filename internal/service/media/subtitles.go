package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
)

// subtitleGroup is the GROUP-ID of the subtitle renditions of a master
// playlist.
const subtitleGroup = "subs"

// manifestAttempts bounds the read-modify-write cycles of a manifest
// racing with another writer.
const manifestAttempts = 8

// version resolves the version of src: its ID and the ETag of its
// content.
func (s *service) version(ctx context.Context, src Source) (domain.ObjectInfo, error) {
	info, err := s.deps.Objects.GetObjectInfo(ctx, src.Bucket, src.Key, object.GetObjectOptions{VersionID: src.VersionID})
	if err != nil {
		return domain.ObjectInfo{}, err
	}
	if info.IsDeleteMarker {
		return domain.ObjectInfo{}, fmt.Errorf("%w: %s/%s is a delete marker", domain.ErrNotFound, src.Bucket, src.Key)
	}
	return info, nil
}

// manifest reads the subtitle manifest of a version and its ETag (empty
// when there is none).
func (s *service) manifest(ctx context.Context, bucket, key, versionID string) (domain.MediaSubtitlesManifestDoc, string, error) {
	var m domain.MediaSubtitlesManifestDoc
	mkey := domain.MediaSidecarDir(key, versionID) + domain.MediaSubtitlesManifest
	info, err := s.deps.Objects.GetObjectInfo(ctx, bucket, mkey, object.GetObjectOptions{})
	if errors.Is(err, domain.ErrObjectNotFound) || errors.Is(err, domain.ErrNotFound) {
		return m, "", nil
	}
	if err != nil {
		return m, "", err
	}
	data, err := s.readSmall(ctx, bucket, mkey)
	if err != nil {
		return m, "", err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, "", fmt.Errorf("media: subtitle manifest of %s/%s: %w", bucket, key, err)
	}
	return m, info.ETag, nil
}

// Subtitles implements Service.
func (s *service) Subtitles(ctx context.Context, src Source) ([]domain.MediaSubtitleTrack, error) {
	info, err := s.version(ctx, src)
	if err != nil {
		return nil, err
	}
	return s.currentTracks(ctx, src.Bucket, src.Key, info)
}

// currentTracks returns the tracks of a version that apply to its
// content.
func (s *service) currentTracks(ctx context.Context, bucket, key string, info domain.ObjectInfo) ([]domain.MediaSubtitleTrack, error) {
	m, _, err := s.manifest(ctx, bucket, key, info.VersionID)
	if err != nil {
		return nil, err
	}
	out := make([]domain.MediaSubtitleTrack, 0, len(m.Tracks))
	for _, t := range m.Tracks {
		if t.Current(info.ETag) {
			out = append(out, t)
		}
	}
	return out, nil
}

// track returns the current track of a language.
func (s *service) track(ctx context.Context, src Source, lang string) (domain.ObjectInfo, domain.MediaSubtitleTrack, error) {
	tag, err := domain.CanonicalLanguage(lang)
	if err != nil {
		return domain.ObjectInfo{}, domain.MediaSubtitleTrack{}, err
	}
	info, err := s.version(ctx, src)
	if err != nil {
		return domain.ObjectInfo{}, domain.MediaSubtitleTrack{}, err
	}
	tracks, err := s.currentTracks(ctx, src.Bucket, src.Key, info)
	if err != nil {
		return domain.ObjectInfo{}, domain.MediaSubtitleTrack{}, err
	}
	for _, t := range tracks {
		if t.Lang == tag {
			return info, t, nil
		}
	}
	return domain.ObjectInfo{}, domain.MediaSubtitleTrack{}, fmt.Errorf("%w: no %s subtitles for %s/%s", domain.ErrNotFound, tag, src.Bucket, src.Key)
}

// subtitleMedia writes the EXT-X-MEDIA entries of the subtitle tracks.
func subtitleMedia(b *bytes.Buffer, tracks []domain.MediaSubtitleTrack, links Links) {
	names := map[string]bool{}
	for _, t := range tracks {
		name := domain.LanguageName(t.Lang)
		if t.Auto {
			name += " (auto)"
		}
		for names[name] {
			name += " " + t.Lang
		}
		names[name] = true
		fmt.Fprintf(b, "#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID=%q,NAME=%q,LANGUAGE=%q,DEFAULT=NO,AUTOSELECT=YES,URI=%q\n",
			subtitleGroup, name, t.Lang, links.Subtitles(t.Lang))
	}
}

// SubtitlePlaylist implements Service: one WebVTT segment spanning the
// whole video.
func (s *service) SubtitlePlaylist(ctx context.Context, src Source, lang string, links Links) ([]byte, error) {
	info, t, err := s.track(ctx, src, lang)
	if err != nil {
		return nil, err
	}
	src.VersionID, src.Revision = info.VersionID, ""
	c, err := s.load(ctx, src)
	if err != nil {
		return nil, err
	}
	d := c.p.describe(src)
	secs := max(d.Duration.Seconds(), 1)
	var b bytes.Buffer
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n", int64(math.Ceil(secs)))
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n#EXT-X-PLAYLIST-TYPE:VOD\n")
	fmt.Fprintf(&b, "#EXTINF:%.3f,\n%s\n#EXT-X-ENDLIST\n", secs, links.SubtitleFile(t.Lang))
	return b.Bytes(), nil
}

// SubtitleFile implements Service: the stored WebVTT with the
// X-TIMESTAMP-MAP that anchors its cues at the start of the presentation.
func (s *service) SubtitleFile(ctx context.Context, src Source, lang string) ([]byte, error) {
	_, cues, err := s.SubtitleCues(ctx, src, lang)
	if err != nil {
		return nil, err
	}
	doc := domain.FormatWebVTT(cues)
	header, rest, _ := bytes.Cut(doc, []byte("\n"))
	out := append(append([]byte(nil), header...), []byte("\nX-TIMESTAMP-MAP=MPEGTS:0,LOCAL:00:00:00.000\n")...)
	return append(out, rest...), nil
}

// SubtitleCues implements Service.
func (s *service) SubtitleCues(ctx context.Context, src Source, lang string) (domain.MediaSubtitleTrack, []domain.SubtitleCue, error) {
	info, t, err := s.track(ctx, src, lang)
	if err != nil {
		return domain.MediaSubtitleTrack{}, nil, err
	}
	data, err := s.readSmall(ctx, src.Bucket, domain.MediaSidecarDir(src.Key, info.VersionID)+t.File())
	if err != nil {
		if errors.Is(err, domain.ErrObjectNotFound) {
			return domain.MediaSubtitleTrack{}, nil, fmt.Errorf("%w: the %s subtitles of %s/%s are missing", domain.ErrNotFound, t.Lang, src.Bucket, src.Key)
		}
		return domain.MediaSubtitleTrack{}, nil, err
	}
	cues, err := domain.ParseSubtitles(data)
	if err != nil {
		return domain.MediaSubtitleTrack{}, nil, fmt.Errorf("media: the %s subtitles of %s/%s: %w", t.Lang, src.Bucket, src.Key, err)
	}
	return t, cues, nil
}

// PutSubtitles implements Service.
func (s *service) PutSubtitles(ctx context.Context, src Source, lang string, cues []domain.SubtitleCue, by string) (domain.MediaSubtitleTrack, error) {
	tag, err := domain.CanonicalLanguage(lang)
	if err != nil {
		return domain.MediaSubtitleTrack{}, err
	}
	if len(cues) == 0 {
		return domain.MediaSubtitleTrack{}, fmt.Errorf("%w: subtitles without cues", domain.ErrInvalidArgument)
	}
	// Validated as a document (bounds, timings, sanitized text).
	doc := domain.FormatWebVTT(cues)
	if cues, err = domain.ParseSubtitles(doc); err != nil {
		return domain.MediaSubtitleTrack{}, err
	}
	doc = domain.FormatWebVTT(cues)
	info, err := s.version(ctx, src)
	if err != nil {
		return domain.MediaSubtitleTrack{}, err
	}
	dir := domain.MediaSidecarDir(src.Key, info.VersionID)
	if _, err := s.deps.Objects.PutObject(ctx, object.PutObjectInput{
		Bucket: src.Bucket, Key: dir + domain.MediaSubtitleFile(tag), Reader: bytes.NewReader(doc), Size: int64(len(doc)),
		ContentType: "text/vtt; charset=utf-8",
	}); err != nil {
		return domain.MediaSubtitleTrack{}, err
	}
	t := domain.MediaSubtitleTrack{Lang: tag, Cues: len(cues), Size: int64(len(doc)), Updated: time.Now().UTC(), UpdatedBy: by}
	err = s.updateManifest(ctx, src.Bucket, src.Key, info.VersionID, func(m *domain.MediaSubtitlesManifestDoc) bool {
		m.Put(t)
		return true
	})
	return t, err
}

// DeleteSubtitles implements Service.
func (s *service) DeleteSubtitles(ctx context.Context, src Source, lang string) error {
	tag, err := domain.CanonicalLanguage(lang)
	if err != nil {
		return err
	}
	info, err := s.version(ctx, src)
	if err != nil {
		return err
	}
	var removed domain.MediaSubtitleTrack
	found := false
	if err := s.updateManifest(ctx, src.Bucket, src.Key, info.VersionID, func(m *domain.MediaSubtitlesManifestDoc) bool {
		removed, found = m.Track(tag)
		return m.Remove(tag)
	}); err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: no %s subtitles for %s/%s", domain.ErrNotFound, tag, src.Bucket, src.Key)
	}
	key := domain.MediaSidecarDir(src.Key, info.VersionID) + removed.File()
	if _, err := s.deps.Objects.DeleteObject(ctx, src.Bucket, key, object.DeleteObjectOptions{}); err != nil && !errors.Is(err, domain.ErrObjectNotFound) {
		s.deps.Logger.WarnContext(ctx, "media: subtitle file not removed", "bucket", src.Bucket, "key", key, "err", err)
	}
	return nil
}

// updateManifest applies change to the manifest of a version and writes
// it back when change reports a change, conditionally on the ETag it read
// (retried when another writer, the media worker or a user, wrote in
// between).
func (s *service) updateManifest(ctx context.Context, bucket, key, versionID string, change func(*domain.MediaSubtitlesManifestDoc) bool) error {
	mkey := domain.MediaSidecarDir(key, versionID) + domain.MediaSubtitlesManifest
	for range manifestAttempts {
		m, etag, err := s.manifest(ctx, bucket, key, versionID)
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
		cond := domain.Conditions{IfNoneMatch: "*"}
		if etag != "" {
			cond = domain.Conditions{IfMatch: etag}
		}
		_, err = s.deps.Objects.PutObject(ctx, object.PutObjectInput{
			Bucket: bucket, Key: mkey, Reader: bytes.NewReader(data), Size: int64(len(data)),
			ContentType: "application/json", Conditions: cond,
		})
		if errors.Is(err, domain.ErrPreconditionFailed) {
			continue
		}
		return err
	}
	return fmt.Errorf("%w: the subtitle manifest of %s/%s keeps changing", domain.ErrConflict, bucket, key)
}

// ParseLanguageList splits a comma-separated list of language tags.
func ParseLanguageList(s string) ([]string, error) {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		tag, err := domain.CanonicalLanguage(f)
		if err != nil {
			return nil, err
		}
		out = append(out, tag)
	}
	return out, nil
}
