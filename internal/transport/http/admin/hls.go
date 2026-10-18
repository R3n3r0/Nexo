package admin

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/media"
)

// The content types of HLS (RFC 8216 §4 and the fMP4 segments).
const (
	contentTypePlaylist = "application/vnd.apple.mpegurl"
	contentTypeInit     = "video/mp4"
	contentTypeSegment  = "video/iso.segment"
)

// hlsPinned is the cache policy of the responses pinned to a revision of
// the object: they never change, but they stay private to the browser.
const hlsPinned = "private, max-age=86400, immutable"

// hlsLinks are the URIs of the playlists, relative to the master playlist
// (/buckets/{bucket}/objects/hls/master.m3u8): they carry the key, the
// revision and, when the master was asked for one, the version, so that
// the guard of every route authorizes the same object.
type hlsLinks struct{ key, versionID string }

var _ media.Links = hlsLinks{}

func (l hlsLinks) query(rev string) url.Values {
	q := url.Values{"key": {l.key}, "rev": {rev}}
	if l.versionID != "" {
		q.Set("versionId", l.versionID)
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

// hlsSource returns the object of an HLS request; pinned requires the
// revision (every URI but the master playlist).
func hlsSource(r *http.Request, pinned bool) (media.Source, error) {
	q := r.URL.Query()
	src := media.Source{Bucket: bucketName(r), Key: q.Get("key"), VersionID: q.Get("versionId"), Revision: q.Get("rev")}
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
		b, err := h.deps.Media.MediaPlaylist(r.Context(), src, hlsLinks{key: src.Key, versionID: src.VersionID})
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
