package media_test

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	tmock "github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/media"
	"github.com/r3n3r0/nexo/internal/service/object"
	objectmock "github.com/r3n3r0/nexo/internal/service/object/mock"
)

// writable lets the service write and delete the objects of the store,
// honouring the conditions of a write; conflicts counts the writes a
// condition refused.
func writable(s *store, m *objectmock.Service) *int {
	conflicts := new(int)
	m.EXPECT().PutObject(tmock.Anything, tmock.Anything).RunAndReturn(
		func(_ context.Context, in object.PutObjectInput) (domain.ObjectInfo, error) {
			info, exists := s.info[in.Key]
			if (in.Conditions.IfNoneMatch == "*" && exists) || (in.Conditions.IfMatch != "" && (!exists || in.Conditions.IfMatch != info.ETag)) {
				*conflicts++
				return domain.ObjectInfo{}, domain.ErrPreconditionFailed
			}
			data, err := io.ReadAll(in.Reader)
			if err != nil {
				return domain.ObjectInfo{}, err
			}
			s.put(in.Key, data)
			return s.info[in.Key], nil
		}).Maybe()
	m.EXPECT().DeleteObject(tmock.Anything, "videos", tmock.Anything, tmock.Anything).RunAndReturn(
		func(_ context.Context, _, key string, _ object.DeleteObjectOptions) (object.DeletedObject, error) {
			delete(s.objects, key)
			delete(s.info, key)
			return object.DeletedObject{}, nil
		}).Maybe()
	return conflicts
}

func TestSubtitles(t *testing.T) {
	ctx := context.Background()
	s, objects := newStore(t, "h264_aac.mp4")
	writable(s, objects)
	svc := newService(t, objects)
	src := media.Source{Bucket: "videos", Key: "h264_aac.mp4"}

	tracks, err := svc.Subtitles(ctx, src)
	require.NoError(t, err)
	assert.Empty(t, tracks)
	_, err = svc.SubtitlePlaylist(ctx, src, "it", links{})
	require.ErrorIs(t, err, domain.ErrNotFound)

	cues := []domain.SubtitleCue{{Start: 500 * time.Millisecond, End: 2 * time.Second, Text: "Ciao <b>a tutti</b>"}}
	tr, err := svc.PutSubtitles(ctx, src, "IT", cues, "alice")
	require.NoError(t, err)
	assert.Equal(t, "it", tr.Lang)
	assert.False(t, tr.Auto)
	assert.Equal(t, 1, tr.Cues)
	assert.Equal(t, "alice", tr.UpdatedBy)
	_, err = svc.PutSubtitles(ctx, src, "en", []domain.SubtitleCue{{Start: 0, End: time.Second, Text: "Hello"}}, "bob")
	require.NoError(t, err)

	tracks, err = svc.Subtitles(ctx, src)
	require.NoError(t, err)
	require.Len(t, tracks, 2)
	assert.Equal(t, []string{"en", "it"}, []string{tracks[0].Lang, tracks[1].Lang})

	// The master playlist names the tracks, the variants the group.
	master, err := svc.MasterPlaylist(ctx, src, links{})
	require.NoError(t, err)
	assert.Contains(t, string(master), `#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",NAME="Italiano",LANGUAGE="it",DEFAULT=NO,AUTOSELECT=YES,URI="subtitles.m3u8?lang=it"`)
	assert.Contains(t, string(master), `NAME="English"`)
	assert.Contains(t, string(master), `,SUBTITLES="subs"`)

	pl, err := svc.SubtitlePlaylist(ctx, src, "it", links{})
	require.NoError(t, err)
	assert.Contains(t, string(pl), "#EXT-X-TARGETDURATION:4\n")
	assert.Contains(t, string(pl), "subtitles.vtt?lang=it\n#EXT-X-ENDLIST")
	vtt, err := svc.SubtitleFile(ctx, src, "it")
	require.NoError(t, err)
	assert.Equal(t, "WEBVTT\nX-TIMESTAMP-MAP=MPEGTS:0,LOCAL:00:00:00.000\n\n00:00:00.500 --> 00:00:02.000\nCiao <b>a tutti</b>\n", string(vtt))
	_, got, err := svc.SubtitleCues(ctx, src, "it")
	require.NoError(t, err)
	assert.Equal(t, cues, got)

	// Invalid input.
	_, err = svc.PutSubtitles(ctx, src, "xx-!!", cues, "")
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = svc.PutSubtitles(ctx, src, "it", nil, "")
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = svc.PutSubtitles(ctx, src, "it", []domain.SubtitleCue{{Start: 2 * time.Second, End: time.Second, Text: "x"}}, "")
	require.ErrorIs(t, err, domain.ErrInvalidArgument)

	// Removal.
	require.NoError(t, svc.DeleteSubtitles(ctx, src, "en"))
	require.ErrorIs(t, svc.DeleteSubtitles(ctx, src, "en"), domain.ErrNotFound)
	_, ok := s.objects[domain.MediaSidecarDir(src.Key, "")+domain.MediaSubtitleFile("en")]
	assert.False(t, ok, "the file of the track is removed")
	master, err = svc.MasterPlaylist(ctx, src, links{})
	require.NoError(t, err)
	assert.NotContains(t, string(master), "English")
}

func TestSubtitlesAutomaticFollowContent(t *testing.T) {
	ctx := context.Background()
	s, objects := newStore(t, "h264_aac.mp4")
	writable(s, objects)
	svc := newService(t, objects)
	src := media.Source{Bucket: "videos", Key: "h264_aac.mp4"}
	dir := domain.MediaSidecarDir(src.Key, "")

	// A track the worker transcribed from the current content.
	s.put(dir+domain.MediaSubtitleFile("en"), []byte("WEBVTT\n\n00:00.000 --> 00:01.000\nauto\n"))
	m := domain.MediaSubtitlesManifestDoc{Tracks: []domain.MediaSubtitleTrack{{Lang: "en", Auto: true, ETag: s.info[src.Key].ETag, Cues: 1}}}
	data, err := json.Marshal(m)
	require.NoError(t, err)
	s.put(dir+domain.MediaSubtitlesManifest, data)

	master, err := svc.MasterPlaylist(ctx, src, links{})
	require.NoError(t, err)
	assert.Contains(t, string(master), `NAME="English (auto)"`)

	// The video is overwritten: the automatic track no longer applies,
	// and the playlist has no subtitle group.
	s.put(src.Key, append(append([]byte(nil), s.objects[src.Key]...), 0))
	master, err = svc.MasterPlaylist(ctx, src, links{})
	require.NoError(t, err)
	assert.NotContains(t, string(master), "SUBTITLES")
	_, err = svc.SubtitleFile(ctx, src, "en")
	require.ErrorIs(t, err, domain.ErrNotFound)
}

func TestSubtitlesManifestConflict(t *testing.T) {
	ctx := context.Background()
	s, objects := newStore(t, "h264_aac.mp4")
	conflicts := writable(s, objects)
	svc := newService(t, objects)
	src := media.Source{Bucket: "videos", Key: "h264_aac.mp4"}
	mkey := domain.MediaSidecarDir(src.Key, "") + domain.MediaSubtitlesManifest

	// Another writer (the worker) changes the manifest once between the
	// read and the write of the service: the update is retried on top.
	raced := false
	objects.EXPECT().GetObjectInfo(tmock.Anything, "videos", mkey, tmock.Anything).RunAndReturn(
		func(_ context.Context, _, key string, _ object.GetObjectOptions) (domain.ObjectInfo, error) {
			info, ok := s.info[key]
			if !ok {
				return domain.ObjectInfo{}, domain.ErrObjectNotFound
			}
			if !raced {
				raced = true
				defer func() {
					var m domain.MediaSubtitlesManifestDoc
					require.NoError(t, json.Unmarshal(s.objects[key], &m))
					m.Put(domain.MediaSubtitleTrack{Lang: "de", Auto: true, ETag: s.info[src.Key].ETag})
					data, _ := json.Marshal(m)
					s.put(key, data)
				}()
			}
			return info, nil
		})
	// Unordered expectations: the specific one above must win.
	objects.ExpectedCalls = append(objects.ExpectedCalls[len(objects.ExpectedCalls)-1:], objects.ExpectedCalls[:len(objects.ExpectedCalls)-1]...)

	s.put(mkey, []byte(`{"tracks":[]}`))
	_, err := svc.PutSubtitles(ctx, src, "it", []domain.SubtitleCue{{End: time.Second, Text: "uno"}}, "")
	require.NoError(t, err)
	assert.Equal(t, 1, *conflicts)
	tracks, err := svc.Subtitles(ctx, src)
	require.NoError(t, err)
	langs := make([]string, 0, len(tracks))
	for _, tr := range tracks {
		langs = append(langs, tr.Lang)
	}
	assert.Equal(t, "de,it", strings.Join(langs, ","), "no update is lost")
}
