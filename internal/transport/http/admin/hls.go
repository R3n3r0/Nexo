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

func (l hlsLinks) Rendition(name, rev string) string {
	l.rendition = name
	return "media.m3u8?" + l.query(rev).Encode()
}

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

// MediaStatusResponse is the processing state of a video by the media
// worker (GET /buckets/{bucket}/objects/media/status).
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
	Updated    *time.Time                    `json:"updated,omitempty"`
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
	h.deps.Enc.WriteJSON(w, http.StatusOK, newMediaStatusResponse(st))
}

func newMediaStatusResponse(st *media.OutputStatus) MediaStatusResponse {
	if st == nil {
		return MediaStatusResponse{State: "none"}
	}
	out := MediaStatusResponse{
		State: string(st.State), Current: st.Current, VersionID: st.VersionID, Error: st.Error, Step: st.Step, Rule: st.Rule,
		Duration: st.Duration, Width: st.Width, Height: st.Height, Renditions: st.Renditions, Poster: st.Poster, Thumbnails: st.Thumbnails,
	}
	if !st.Updated.IsZero() {
		u := st.Updated.UTC()
		out.Updated = &u
	}
	return out
}
