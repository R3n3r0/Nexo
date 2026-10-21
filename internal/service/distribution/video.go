package distribution

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/media"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/policy"
)

// OutcomeVideo is the outcome of an answer serving a resource of the HLS
// presentation of a video (Video).
const OutcomeVideo = "video"

// The files of the HLS presentation of a video, served below the URL of
// the video (Video).
const (
	VideoMaster     = "master.m3u8"
	VideoMedia      = "media.m3u8"
	VideoInit       = "init.mp4"
	VideoSegment    = "segment.m4s"
	VideoPoster     = "poster.jpg"
	VideoSprite     = "sprite.jpg"
	VideoThumbnails = "thumbnails.vtt"
	// VideoSubtitles and VideoSubtitleFile serve the subtitles of a
	// language (?lang=): its playlist and its WebVTT.
	VideoSubtitles    = "subtitles.m3u8"
	VideoSubtitleFile = "subtitles.vtt"
)

// The content types of the HLS presentations (RFC 8216 §4 and the fMP4
// segments).
const (
	contentTypePlaylist = "application/vnd.apple.mpegurl"
	contentTypeMP4      = "video/mp4"
	contentTypeSegment  = "video/iso.segment"
	contentTypeVTT      = "text/vtt; charset=utf-8"
	contentTypeJPEG     = "image/jpeg"
)

// videoFile is a file of the presentation: whether its URI names the
// revision (the immutable ones), and how it is built.
type videoFile struct {
	pinned bool
	build  func(ctx context.Context, m media.Service, src media.Source, l media.Links, q url.Values) ([]byte, string, error)
}

// videoFiles are the files a video behavior serves.
func videoFiles() map[string]videoFile {
	sidecar := func(file string) videoFile {
		return videoFile{build: func(ctx context.Context, m media.Service, src media.Source, _ media.Links, _ url.Values) ([]byte, string, error) {
			b, err := m.Sidecar(ctx, src, file)
			return b, contentTypeJPEG, err
		}}
	}
	return map[string]videoFile{
		VideoMaster: {build: func(ctx context.Context, m media.Service, src media.Source, l media.Links, _ url.Values) ([]byte, string, error) {
			b, err := m.MasterPlaylist(ctx, src, l)
			return b, contentTypePlaylist, err
		}},
		VideoMedia: {pinned: true, build: func(ctx context.Context, m media.Service, src media.Source, l media.Links, _ url.Values) ([]byte, string, error) {
			b, err := m.MediaPlaylist(ctx, src, l)
			return b, contentTypePlaylist, err
		}},
		VideoInit: {pinned: true, build: func(ctx context.Context, m media.Service, src media.Source, _ media.Links, _ url.Values) ([]byte, string, error) {
			b, err := m.Init(ctx, src)
			return b, contentTypeMP4, err
		}},
		VideoSegment: {pinned: true, build: func(ctx context.Context, m media.Service, src media.Source, _ media.Links, q url.Values) ([]byte, string, error) {
			n, err := strconv.Atoi(q.Get("n"))
			if err != nil || n < 0 {
				return nil, "", fmt.Errorf("%w: n must be a non-negative integer", domain.ErrInvalidArgument)
			}
			b, err := m.Segment(ctx, src, n)
			return b, contentTypeSegment, err
		}},
		VideoPoster: sidecar(domain.MediaPosterFile),
		VideoSprite: sidecar(domain.MediaSpriteFile),
		VideoThumbnails: {build: func(ctx context.Context, m media.Service, src media.Source, l media.Links, _ url.Values) ([]byte, string, error) {
			b, err := m.Sidecar(ctx, src, domain.MediaThumbnailsFile)
			if err != nil {
				return nil, "", err
			}
			return media.RewriteSprite(b, l.(videoLinks).with(VideoSprite, nil)), contentTypeVTT, nil
		}},
		VideoSubtitles: {build: func(ctx context.Context, m media.Service, src media.Source, l media.Links, q url.Values) ([]byte, string, error) {
			b, err := m.SubtitlePlaylist(ctx, src, q.Get("lang"), l)
			return b, contentTypePlaylist, err
		}},
		VideoSubtitleFile: {build: func(ctx context.Context, m media.Service, src media.Source, _ media.Links, q url.Values) ([]byte, string, error) {
			b, err := m.SubtitleFile(ctx, src, q.Get("lang"))
			return b, contentTypeVTT, err
		}},
	}
}

// streamsVideo reports a behavior serving the HLS presentations of the
// videos below their URL.
func streamsVideo(b Behavior) bool { return b.Video != nil && b.Video.HLS }

// videoRequest splits the key of a request into the key of the video and
// the file of its presentation: ok is false when the last segment of the
// path names no file of a presentation.
func videoRequest(key string) (video, file string, ok bool) {
	i := strings.LastIndexByte(key, '/')
	if i <= 0 {
		return "", "", false
	}
	video, file = key[:i], key[i+1:]
	if _, known := videoFiles()[file]; !known || strings.HasSuffix(video, "/") {
		return "", "", false
	}
	return video, file, true
}

// videoLinks are the URIs of the files of a presentation, relative to the
// playlist: siblings of it below the URL of the video. They carry the
// revision, the rendition, and the parameters of a signed URL (a custom
// policy whose resource covers the URL of the video with a wildcard:
// https://videos.example.com/intro.mp4/*), so that a player follows
// them with the authorization of the master playlist.
type videoLinks struct {
	rendition string
	signing   url.Values
}

var _ media.Links = videoLinks{}

// with is the URI of file with the parameters q (and those of the
// signature).
func (l videoLinks) with(file string, q url.Values) string {
	out := url.Values{}
	for k, v := range q {
		out[k] = v
	}
	for k, v := range l.signing {
		out[k] = v
	}
	if len(out) == 0 {
		return file
	}
	return file + "?" + out.Encode()
}

func (l videoLinks) query(rev string) url.Values {
	q := url.Values{"rev": {rev}}
	if l.rendition != "" {
		q.Set("r", l.rendition)
	}
	return q
}

func (l videoLinks) Media(rev string) string { return l.with(VideoMedia, l.query(rev)) }

func (l videoLinks) Init(rev string) string { return l.with(VideoInit, l.query(rev)) }

func (l videoLinks) Segment(rev string, n int) string {
	q := l.query(rev)
	q.Set("n", strconv.Itoa(n))
	return l.with(VideoSegment, q)
}

func (l videoLinks) Rendition(name, rev string) string {
	l.rendition = name
	return l.with(VideoMedia, l.query(rev))
}

func (l videoLinks) Subtitles(lang string) string {
	return l.with(VideoSubtitles, url.Values{"lang": {lang}})
}

func (l videoLinks) SubtitleFile(lang string) string {
	return l.with(VideoSubtitleFile, url.Values{"lang": {lang}})
}

// signingParams returns the parameters of a signed URL in rawQuery (none
// for an unsigned one).
func signingParams(rawQuery string) url.Values {
	q, _ := url.ParseQuery(rawQuery)
	out := url.Values{}
	for k, v := range q {
		if signingParam(k) {
			out[k] = v
		}
	}
	return out
}

// authorizeStream decides whether the viewer may stream key: allowed
// nexo:GetMediaStream (the presentation without the original), or else
// s3:GetObject. The refusal of s3:GetObject is the one answered.
func (w *website) authorizeStream(ctx context.Context, c *compiledDistribution, req WebRequest, b Behavior, key string) (int, string) {
	status, reason := w.authorize(ctx, c, req, b, policy.ActionGetMediaStream, key)
	if status == 0 || status == http.StatusServiceUnavailable {
		return status, reason
	}
	return w.authorize(ctx, c, req, b, policy.ActionGetObject, key)
}

// serveVideo answers a GET or HEAD of a file of the HLS presentation of a
// video (Video): <video key>/master.m3u8 and the files its playlists name.
// ok is false when the request is not about a video, so that the object
// of that key is served instead: no video at that key, or not one the
// packager streams.
//
// The files whose URI names the revision of the video (?rev=: the media
// playlists, the init and media segments) never change, so they are
// cacheable for a day, publicly on a public behavior; the master playlist,
// the poster and the thumbnails follow the current content of the video
// and are revalidated.
func (w *website) serveVideo(ctx context.Context, c *compiledDistribution, req WebRequest, b Behavior, key string) (*WebResponse, bool) {
	if w.deps.Media == nil {
		return nil, false
	}
	video, name, ok := videoRequest(key)
	if !ok {
		return nil, false
	}
	bucket := c.d.Origin.Bucket
	info, err := w.deps.Objects.GetObjectInfo(ctx, bucket, video, object.GetObjectOptions{})
	if err != nil || info.IsDeleteMarker {
		if err != nil && unavailable(err) {
			return w.refuse(ctx, c, req, http.StatusServiceUnavailable, "the video cannot be read now"), true
		}
		return nil, false
	}
	if status, reason := w.authorizeStream(ctx, c, req, b, video); status != 0 {
		return w.refuse(ctx, c, req, status, reason), true
	}
	file := videoFiles()[name]
	q, _ := url.ParseQuery(req.RawQuery)
	src := media.Source{Bucket: bucket, Key: video, Revision: q.Get("rev"), Rendition: q.Get("r")}
	if file.pinned && src.Revision == "" {
		return w.plain(http.StatusBadRequest, "rev is required", OutcomeError), true
	}
	links := videoLinks{rendition: src.Rendition, signing: signingParams(req.RawQuery)}
	body, contentType, err := file.build(ctx, w.deps.Media, src, links, q)
	if err != nil {
		if errors.Is(err, media.ErrUnsupported) && name == VideoMaster {
			// Not a video the packager streams: the object of the key
			// (if any) answers.
			return nil, false
		}
		return w.videoError(ctx, c, req, video, err), true
	}
	h := http.Header{}
	for k, v := range c.responseHeaders(b) {
		h.Set(k, v)
	}
	h.Set("Content-Type", contentType)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	h.Set("X-Content-Type-Options", "nosniff")
	scope := "private"
	if b.Access == AccessPublic {
		scope = "public"
	}
	if file.pinned {
		h.Set("Cache-Control", scope+", max-age=86400, immutable")
	} else {
		h.Set("Cache-Control", scope+", no-cache")
	}
	resp := &WebResponse{Status: http.StatusOK, Header: h, Outcome: OutcomeVideo, Bucket: bucket, Key: video}
	if req.Method != http.MethodHead {
		resp.Body = io.NopCloser(strings.NewReader(string(body)))
	}
	return resp, true
}

// videoError answers an error of the packager.
func (w *website) videoError(ctx context.Context, c *compiledDistribution, req WebRequest, video string, err error) *WebResponse {
	switch {
	case errors.Is(err, domain.ErrPreconditionFailed):
		// A link to a revision that is no longer the current content:
		// the player reloads the master playlist.
		return w.plain(http.StatusPreconditionFailed, "the video changed: reload the master playlist", OutcomeError)
	case errors.Is(err, domain.ErrSSECustomerKeyRequired):
		return w.errorPage(ctx, c, req, http.StatusForbidden, "SSE-C videos cannot be streamed")
	case errors.Is(err, media.ErrUnsupported):
		return w.plain(http.StatusUnsupportedMediaType, "the video cannot be streamed: "+err.Error(), OutcomeError)
	case errors.Is(err, domain.ErrInvalidArgument):
		return w.plain(http.StatusBadRequest, err.Error(), OutcomeError)
	case errors.Is(err, domain.ErrNotFound), errors.Is(err, domain.ErrObjectNotFound), errors.Is(err, domain.ErrNoSuchVersion):
		return w.errorPage(ctx, c, req, http.StatusNotFound, "not found")
	case unavailable(err):
		w.deps.Logger.WarnContext(ctx, "distribution: video unavailable", "id", c.d.ID, "key", video, "err", err)
		return w.refuse(ctx, c, req, http.StatusServiceUnavailable, "service unavailable")
	default:
		w.deps.Logger.ErrorContext(ctx, "distribution: video failed", "id", c.d.ID, "key", video, "err", err)
		return w.errorPage(ctx, c, req, http.StatusInternalServerError, "internal error")
	}
}
