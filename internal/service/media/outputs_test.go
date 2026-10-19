package media_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/media"
)

// withOutputs stores the outputs of the worker for h264_aac.mp4: one
// rendition (a copy of the faststart file), a poster and thumbnails.
func withOutputs(t *testing.T, s *store, state domain.MediaJobState, etag string) {
	t.Helper()
	dir := domain.MediaSidecarDir("h264_aac.mp4", "")
	s.put(dir+domain.MediaRenditionsDir+"240p.mp4", s.objects["h264_aac_faststart.mp4"])
	s.put(dir+domain.MediaPosterFile, []byte("jpeg poster"))
	s.put(dir+domain.MediaSpriteFile, []byte("jpeg sprite"))
	s.put(dir+domain.MediaThumbnailsFile, []byte("WEBVTT\n\n00:00:00.000 --> 00:00:01.000\nsprite.jpg#xywh=0,0,160,90\n"))
	st := domain.MediaStatus{State: state, ETag: etag, Renditions: []domain.MediaRenditionOutput{{Name: "240p", Width: 160, Height: 90, Bandwidth: 1}},
		Poster: true, Thumbnails: &domain.MediaThumbnailsOutput{Interval: 1, Width: 160, Height: 90, Columns: 1, Count: 1}}
	data, err := json.Marshal(st)
	require.NoError(t, err)
	s.put(dir+domain.MediaStatusFile, data)
}

func TestMasterPlaylistWithRenditions(t *testing.T) {
	ctx := context.Background()
	s, objects := newStore(t, "h264_aac.mp4", "h264_aac_faststart.mp4")
	svc := newService(t, objects)
	src := media.Source{Bucket: "videos", Key: "h264_aac.mp4"}
	etag := s.info["h264_aac.mp4"].ETag

	master, err := svc.MasterPlaylist(ctx, src, links{})
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(string(master), "#EXT-X-STREAM-INF"), "the source alone without outputs")

	withOutputs(t, s, domain.MediaStateProcessing, etag)
	master, err = svc.MasterPlaylist(ctx, src, links{})
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(string(master), "#EXT-X-STREAM-INF"), "outputs still processing are not served")

	withOutputs(t, s, domain.MediaStateDone, `"`+etag+`"`)
	master, err = svc.MasterPlaylist(ctx, src, links{})
	require.NoError(t, err)
	assert.Equal(t, 2, strings.Count(string(master), "#EXT-X-STREAM-INF"), string(master))
	rev := s.info[domain.MediaSidecarDir("h264_aac.mp4", "")+"renditions/240p.mp4"].ETag
	assert.Contains(t, string(master), "media.m3u8?r=240p&rev="+rev)

	// The playlists and segments of the rendition, pinned to its ETag.
	rsrc := media.Source{Bucket: "videos", Key: "h264_aac.mp4", Rendition: "240p", Revision: rev}
	pl, err := svc.MediaPlaylist(ctx, rsrc, links{})
	require.NoError(t, err)
	assert.Contains(t, string(pl), "#EXT-X-ENDLIST")
	_, err = svc.Init(ctx, rsrc)
	require.NoError(t, err)
	_, err = svc.Segment(ctx, rsrc, 0)
	require.NoError(t, err)
	_, err = svc.MediaPlaylist(ctx, media.Source{Bucket: "videos", Key: "h264_aac.mp4", Rendition: "1080p", Revision: rev}, links{})
	assert.ErrorIs(t, err, domain.ErrNotFound, "a rendition the worker did not produce")
	_, err = svc.MediaPlaylist(ctx, media.Source{Bucket: "videos", Key: "h264_aac.mp4", Rendition: "bogus", Revision: rev}, links{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = svc.MasterPlaylist(ctx, rsrc, links{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)

	// The video is overwritten: the outputs no longer apply.
	s.put("h264_aac.mp4", append(append([]byte(nil), s.objects["h264_aac.mp4"]...), 0))
	master, err = svc.MasterPlaylist(ctx, src, links{})
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(string(master), "#EXT-X-STREAM-INF"), "stale outputs are not served")
	_, err = svc.MediaPlaylist(ctx, rsrc, links{})
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

func TestStatusAndSidecars(t *testing.T) {
	ctx := context.Background()
	s, objects := newStore(t, "h264_aac.mp4", "h264_aac_faststart.mp4")
	svc := newService(t, objects)
	src := media.Source{Bucket: "videos", Key: "h264_aac.mp4"}

	st, err := svc.Status(ctx, src)
	require.NoError(t, err)
	assert.Nil(t, st, "never processed")
	_, err = svc.Sidecar(ctx, src, domain.MediaPosterFile)
	assert.ErrorIs(t, err, domain.ErrNotFound)

	withOutputs(t, s, domain.MediaStateDone, s.info["h264_aac.mp4"].ETag)
	st, err = svc.Status(ctx, src)
	require.NoError(t, err)
	require.NotNil(t, st)
	assert.True(t, st.Current)
	assert.Equal(t, domain.MediaStateDone, st.State)
	poster, err := svc.Sidecar(ctx, src, domain.MediaPosterFile)
	require.NoError(t, err)
	assert.Equal(t, "jpeg poster", string(poster))
	vtt, err := svc.Sidecar(ctx, src, domain.MediaThumbnailsFile)
	require.NoError(t, err)
	assert.Equal(t, "WEBVTT\n\n00:00:00.000 --> 00:00:01.000\nsprite.jpg?key=k#xywh=0,0,160,90\n",
		string(media.RewriteSprite(vtt, "sprite.jpg?key=k")))
	_, err = svc.Sidecar(ctx, src, "status.json")
	assert.ErrorIs(t, err, domain.ErrInvalidArgument, "only the worker outputs")
}
