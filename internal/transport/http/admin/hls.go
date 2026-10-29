package admin

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/media"
)

// The content types of HLS (RFC 8216 §4 and the fMP4 segments).
const (
	contentTypePlaylist = "application/vnd.apple.mpegurl"
	contentTypeInit     = "video/mp4"
	contentTypeSegment  = "video/iso.segment"
	contentTypeVTT      = "text/vtt; charset=utf-8"
	contentTypeJPEG     = "image/jpeg"
)

// hlsPinned is the cache policy of the responses pinned to a revision of
// the object: they never change, but they stay private to the browser.
const hlsPinned = "private, max-age=86400, immutable"

// hlsLinks are the URIs of the playlists, relative to the master playlist
// (/buckets/{bucket}/objects/hls/master.m3u8): they carry the key, the
// revision and, when the master was asked for one, the version, so that
// the guard of every route authorizes the same object; the URIs of a
// rendition playlist name the rendition.
type hlsLinks struct{ key, versionID, rendition string }

var _ media.Links = hlsLinks{}

func (l hlsLinks) query(rev string) url.Values {
	q := url.Values{"key": {l.key}, "rev": {rev}}
	if l.versionID != "" {
		q.Set("versionId", l.versionID)
	}
	if l.rendition != "" {
		q.Set("r", l.rendition)
	}
	return q
}

func (l hlsLinks) Media(rev string) string { return "media.m3u8?" + l.query(rev).Encode() }

func (l hlsLinks) Init(rev string) string { return "init.mp4?" + l.query(rev).Encode() }

func (l hlsLinks) Segment(rev string, n int) string {
	q := l.query(rev)
	q.Set("n", strconv.Itoa(n))
	return "segment.m4s?" + q.Encode()
}

func (l hlsLinks) Key(rev string, period int) string {
	q := l.query(rev)
	q.Set("i", strconv.Itoa(period))
	return "key?" + q.Encode()
}

func (l hlsLinks) Rendition(name, rev string) string {
	l.rendition = name
	return "media.m3u8?" + l.query(rev).Encode()
}

func (l hlsLinks) subtitleQuery(lang string) string {
	q := l.query("")
	q.Del("rev")
	q.Del("r")
	q.Set("lang", lang)
	return q.Encode()
}

func (l hlsLinks) Subtitles(lang string) string { return "subtitles.m3u8?" + l.subtitleQuery(lang) }

func (l hlsLinks) SubtitleFile(lang string) string { return "subtitles.vtt?" + l.subtitleQuery(lang) }

// hlsSource returns the object of an HLS request; pinned requires the
// revision (every URI but the master playlist).
func hlsSource(r *http.Request, pinned bool) (media.Source, error) {
	q := r.URL.Query()
	src := media.Source{Bucket: bucketName(r), Key: q.Get("key"), VersionID: q.Get("versionId"), Revision: q.Get("rev"), Rendition: q.Get("r")}
	if src.Key == "" {
		return src, fmt.Errorf("%w: key is required", domain.ErrInvalidArgument)
	}
	if pinned && src.Revision == "" {
		return src, fmt.Errorf("%w: rev is required", domain.ErrInvalidArgument)
	}
	return src, nil
}

// hls serves one resource of the HLS presentation of a video object; 404
// when the server has no media service.
func (h *objectRoutes) hls(pinned bool, build func(r *http.Request, src media.Source) ([]byte, string, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.deps.Media == nil {
			h.fail(w, r, fmt.Errorf("%w: video streaming is not enabled", domain.ErrNotFound))
			return
		}
		src, err := hlsSource(r, pinned)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		body, contentType, err := build(r, src)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		hdr := w.Header()
		hdr.Set("Content-Type", contentType)
		hdr.Set("Content-Length", strconv.Itoa(len(body)))
		hdr.Set("X-Content-Type-Options", "nosniff")
		if pinned {
			hdr.Set("Cache-Control", hlsPinned)
		} else {
			hdr.Set("Cache-Control", "private, no-cache")
		}
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(body)
		}
	}
}

func (h *objectRoutes) hlsMaster() http.HandlerFunc {
	return h.hls(false, func(r *http.Request, src media.Source) ([]byte, string, error) {
		b, err := h.deps.Media.MasterPlaylist(r.Context(), src, hlsLinks{key: src.Key, versionID: src.VersionID})
		return b, contentTypePlaylist, err
	})
}

func (h *objectRoutes) hlsMedia() http.HandlerFunc {
	return h.hls(true, func(r *http.Request, src media.Source) ([]byte, string, error) {
		b, err := h.deps.Media.MediaPlaylist(r.Context(), src, hlsLinks{key: src.Key, versionID: src.VersionID, rendition: src.Rendition})
		return b, contentTypePlaylist, err
	})
}

func (h *objectRoutes) hlsInit() http.HandlerFunc {
	return h.hls(true, func(r *http.Request, src media.Source) ([]byte, string, error) {
		b, err := h.deps.Media.Init(r.Context(), src)
		return b, contentTypeInit, err
	})
}

func (h *objectRoutes) hlsSegment() http.HandlerFunc {
	return h.hls(true, func(r *http.Request, src media.Source) ([]byte, string, error) {
		n, err := strconv.Atoi(r.URL.Query().Get("n"))
		if err != nil || n < 0 {
			return nil, "", fmt.Errorf("%w: n must be a non-negative integer", domain.ErrInvalidArgument)
		}
		b, err := h.deps.Media.Segment(r.Context(), src, n)
		return b, contentTypeSegment, err
	})
}

// hlsKey serves the AES-128 key of a period (?i=) of a protected video to
// a console session allowed to stream it; never cached.
func (h *objectRoutes) hlsKey(w http.ResponseWriter, r *http.Request) {
	if h.deps.Media == nil {
		h.fail(w, r, fmt.Errorf("%w: video streaming is not enabled", domain.ErrNotFound))
		return
	}
	src, err := hlsSource(r, true)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	period, err := strconv.Atoi(r.URL.Query().Get("i"))
	if err != nil || period < 0 {
		h.fail(w, r, fmt.Errorf("%w: i must be a non-negative integer", domain.ErrInvalidArgument))
		return
	}
	key, err := h.deps.Media.Key(r.Context(), src, period)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeMediaKey(w, key)
}

// writeMediaKey writes the 16 bytes of a segment key: never stored by a
// cache nor the browser.
func writeMediaKey(w http.ResponseWriter, key []byte) {
	hdr := w.Header()
	hdr.Set("Content-Type", "application/octet-stream")
	hdr.Set("Content-Length", strconv.Itoa(len(key)))
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(key)
}

// hlsSubtitles serves the playlist of the subtitles of a language
// (?lang=).
func (h *objectRoutes) hlsSubtitles() http.HandlerFunc {
	return h.hls(false, func(r *http.Request, src media.Source) ([]byte, string, error) {
		b, err := h.deps.Media.SubtitlePlaylist(r.Context(), src, r.URL.Query().Get("lang"), hlsLinks{key: src.Key, versionID: src.VersionID})
		return b, contentTypePlaylist, err
	})
}

// hlsSubtitleFile serves the WebVTT of the subtitles of a language.
func (h *objectRoutes) hlsSubtitleFile() http.HandlerFunc {
	return h.hls(false, func(r *http.Request, src media.Source) ([]byte, string, error) {
		b, err := h.deps.Media.SubtitleFile(r.Context(), src, r.URL.Query().Get("lang"))
		return b, contentTypeVTT, err
	})
}

// hlsFile serves a file the media worker derived from the video (the
// poster, the thumbnail sprite); 404 until the worker produced it from the
// current content.
func (h *objectRoutes) hlsFile(file, contentType string) http.HandlerFunc {
	return h.hls(false, func(r *http.Request, src media.Source) ([]byte, string, error) {
		b, err := h.deps.Media.Sidecar(r.Context(), src, file)
		return b, contentType, err
	})
}

// hlsThumbnails serves the WebVTT map of the thumbnails, its sprite
// references rewritten to the sprite route of the same object.
func (h *objectRoutes) hlsThumbnails() http.HandlerFunc {
	return h.hls(false, func(r *http.Request, src media.Source) ([]byte, string, error) {
		b, err := h.deps.Media.Sidecar(r.Context(), src, domain.MediaThumbnailsFile)
		if err != nil {
			return nil, "", err
		}
		q := hlsLinks{key: src.Key, versionID: src.VersionID}.query("")
		q.Del("rev")
		return media.RewriteSprite(b, "sprite.jpg?"+q.Encode()), contentTypeVTT, nil
	})
}

// image serves a variant of an image the media worker derived from the
// current content of the object (404 otherwise): ?w= the width the client
// displays, ?format= auto (the default: the most compact format of the
// Accept header) or a format. It answers ranges and conditional requests
// (the ETag names the variant).
func (h *objectRoutes) image(w http.ResponseWriter, r *http.Request) {
	if h.deps.Media == nil {
		h.fail(w, r, fmt.Errorf("%w: the media outputs are not enabled", domain.ErrNotFound))
		return
	}
	src, err := hlsSource(r, false)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	q := r.URL.Query()
	width, err := media.ParseImageWidth(q.Get("w"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	format, err := media.ParseImageFormat(q.Get("format"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	img, err := h.deps.Media.Image(r.Context(), media.Source{Bucket: src.Bucket, Key: src.Key, VersionID: src.VersionID},
		media.ImageRequest{Width: width, Format: format, Accept: r.Header.Get("Accept")})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	defer func() { _ = img.Content.Close() }()
	hdr := w.Header()
	hdr.Set("Content-Type", img.ContentType)
	hdr.Set("ETag", img.ETag)
	hdr.Set("Cache-Control", "private, no-cache")
	hdr.Set("X-Content-Type-Options", "nosniff")
	if format == media.ImageFormatAuto {
		hdr.Add("Vary", "Accept")
	}
	http.ServeContent(w, r, "", img.Modified, img.Content)
}

// imageURL is the URL of a variant of an image on the Admin API (width 0:
// the image at its own size).
func imageURL(bucket, key, versionID string, width int, format string) string {
	q := url.Values{"key": {key}, "format": {format}}
	if width > 0 {
		q.Set("w", strconv.Itoa(width))
	}
	if versionID != "" {
		q.Set("versionId", versionID)
	}
	return Prefix + "/buckets/" + url.PathEscape(bucket) + "/objects/image?" + q.Encode()
}

// MediaImageResponse is a variant of an image produced by the media
// worker.
type MediaImageResponse struct {
	domain.MediaImageOutput
	// ContentType is the media type of the format.
	ContentType string `json:"contentType"`
	// URL serves the variant (GET /buckets/{bucket}/objects/image), a path
	// of the server.
	URL string `json:"url"`
}

// MediaStatusResponse is the processing state of a video or an image by
// the media worker (GET /buckets/{bucket}/objects/media/status).
type MediaStatusResponse struct {
	// State is processing, done or failed; none when the worker never
	// processed the version.
	State string `json:"state"`
	// Current reports outputs derived from the current content of the
	// version (only they are served).
	Current    bool                          `json:"current"`
	VersionID  string                        `json:"versionId,omitempty"`
	Error      string                        `json:"error,omitempty"`
	Step       string                        `json:"step,omitempty"`
	Rule       string                        `json:"rule,omitempty"`
	Duration   float64                       `json:"duration,omitempty"`
	Width      int                           `json:"width,omitempty"`
	Height     int                           `json:"height,omitempty"`
	Renditions []domain.MediaRenditionOutput `json:"renditions,omitempty"`
	Poster     bool                          `json:"poster,omitempty"`
	Thumbnails *domain.MediaThumbnailsOutput `json:"thumbnails,omitempty"`
	// Images are the variants of an image (Width and Height are the size
	// of the image).
	Images []MediaImageResponse `json:"images,omitempty"`
	// Alpha reports an image with transparency.
	Alpha bool `json:"alpha,omitempty"`
	// Subtitles are the languages of the automatic subtitle tracks the
	// worker made; Language is the spoken language it detected.
	Subtitles []string `json:"subtitles,omitempty"`
	Language  string   `json:"language,omitempty"`
	// SubtitlesError is why the automatic subtitles failed (the other
	// outputs are served nonetheless).
	SubtitlesError string     `json:"subtitlesError,omitempty"`
	Updated        *time.Time `json:"updated,omitempty"`
}

// mediaStatus serves the processing state of a video.
func (h *objectRoutes) mediaStatus(w http.ResponseWriter, r *http.Request) {
	if h.deps.Media == nil {
		h.fail(w, r, fmt.Errorf("%w: video streaming is not enabled", domain.ErrNotFound))
		return
	}
	src, err := hlsSource(r, false)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	st, err := h.deps.Media.Status(r.Context(), src)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newMediaStatusResponse(src, st))
}

func newMediaStatusResponse(src media.Source, st *media.OutputStatus) MediaStatusResponse {
	if st == nil {
		return MediaStatusResponse{State: "none"}
	}
	out := MediaStatusResponse{
		State: string(st.State), Current: st.Current, VersionID: st.VersionID, Error: st.Error, Step: st.Step, Rule: st.Rule,
		Duration: st.Duration, Width: st.Width, Height: st.Height, Renditions: st.Renditions, Poster: st.Poster, Thumbnails: st.Thumbnails,
		Alpha: st.Alpha, Subtitles: st.Subtitles, Language: st.Language, SubtitlesError: st.SubtitlesError,
	}
	for _, v := range st.Images {
		out.Images = append(out.Images, MediaImageResponse{MediaImageOutput: v, ContentType: domain.MediaImageContentType(v.Format),
			URL: imageURL(src.Bucket, src.Key, src.VersionID, v.Width, v.Format)})
	}
	if !st.Updated.IsZero() {
		u := st.Updated.UTC()
		out.Updated = &u
	}
	return out
}
