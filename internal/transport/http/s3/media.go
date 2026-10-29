package s3

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/auth"
	"github.com/r3n3r0/nexo/internal/service/media"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
)

// MediaPrefix is the path of the HLS presentations of a bucket on the S3
// listener (domain.MediaPathPrefix, version 1): /nexo/media/v1/{bucket}/.
const MediaPrefix = domain.MediaPathPrefix + "v1/"

// PlayerPath is the public player page of the stream shares; the master
// playlist goes in its fragment (/nexo/media/player/#/nexo/media/v1/...).
const PlayerPath = domain.MediaPlayerPath

// playerCSP confines the player page to its own scripts, styles and
// streams: Media Source Extensions feed the video from blob: URLs.
const playerCSP = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; " +
	"media-src 'self' blob:; worker-src 'self' blob:; img-src 'self' data:; base-uri 'none'; form-action 'none'"

// playerAssetName reports whether name is a script or a stylesheet of the
// build output of the console (a file name: letters, digits, ".", "_",
// "-").
func playerAssetName(name string) bool {
	if ext := path.Ext(name); ext != ".js" && ext != ".css" {
		return false
	}
	valid := func(c rune) bool {
		return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-'
	}
	return !strings.HasPrefix(name, ".") && strings.IndexFunc(name, func(c rune) bool { return !valid(c) }) < 0
}

// The content types of HLS (RFC 8216 §4 and the fMP4 segments).
const (
	contentTypePlaylist = "application/vnd.apple.mpegurl"
	contentTypeInit     = "video/mp4"
	contentTypeSegment  = "video/iso.segment"
	contentTypeVTT      = "text/vtt; charset=utf-8"
	contentTypeJPEG     = "image/jpeg"
)

// MediaDeps are the dependencies of the media routes; all but Objects,
// Clock and Logger are required.
type MediaDeps struct {
	// Media packages the videos as HLS.
	Media media.Service
	// Policy authorizes nexo:GetMediaStream, else s3:GetObject
	// (s3:GetObjectVersion) as GetObject does; Objects provides the tags of the objects for the
	// s3:ExistingObjectTag/<key> condition keys.
	Policy  policy.Service
	Objects object.Service
	// Signer and Creds sign the URIs of the playlists of a presigned
	// request (a share link) with the credentials that signed it.
	Signer auth.Signer
	Creds  auth.CredentialLookup
	// Player holds the build of the web console (player.html and its
	// assets); nil serves no player page.
	Player fs.FS
	Enc    httpx.Codec
	Clock  clock.Clock
	Logger *slog.Logger
}

type mediaRoutes struct {
	deps  MediaDeps
	opts  options
	authz *authorizer
}

var _ MediaRoutes = (*mediaRoutes)(nil)

// NewMediaRoutes returns the media routes: the HLS presentation of an MP4
// video, readable by whoever may stream it (nexo:GetMediaStream) or read
// the object (s3:GetObject, or s3:GetObjectVersion with versionId) and signed by the auth middleware
// like any S3 request. A presigned master playlist (a share link) signs
// the URIs of its playlists with the same credentials until the same
// expiry, so the player needs nothing but the link. Missing dependencies
// are a wiring error (domain.ErrInvalidArgument).
func NewMediaRoutes(deps MediaDeps, opts ...Option) (MediaRoutes, error) {
	if deps.Media == nil || deps.Policy == nil || deps.Signer == nil || deps.Creds == nil {
		return nil, fmt.Errorf("%w: media routes require the media, policy, signer and credential services", domain.ErrInvalidArgument)
	}
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	h := &mediaRoutes{deps: deps, opts: newOptions(opts)}
	h.authz = &authorizer{policy: deps.Policy, enc: deps.Enc, clock: deps.Clock, objects: deps.Objects}
	return h, nil
}

// Register implements Routes.
func (h *mediaRoutes) Register(r chi.Router) {
	r.Get(PlayerPath, h.player)
	r.Get(PlayerPath+"assets/{name}", h.playerAsset)
	r.Get(MediaPrefix+"{bucket}/master.m3u8", h.serve(false, func(r *http.Request, src media.Source, l media.Links) ([]byte, string, error) {
		b, err := h.deps.Media.MasterPlaylist(r.Context(), src, l)
		return b, contentTypePlaylist, err
	}))
	r.Get(MediaPrefix+"{bucket}/media.m3u8", h.serve(true, func(r *http.Request, src media.Source, l media.Links) ([]byte, string, error) {
		b, err := h.deps.Media.MediaPlaylist(r.Context(), src, l)
		return b, contentTypePlaylist, err
	}))
	r.Get(MediaPrefix+"{bucket}/init.mp4", h.serve(true, func(r *http.Request, src media.Source, _ media.Links) ([]byte, string, error) {
		b, err := h.deps.Media.Init(r.Context(), src)
		return b, contentTypeInit, err
	}))
	r.Get(MediaPrefix+"{bucket}/segment.m4s", h.serve(true, func(r *http.Request, src media.Source, _ media.Links) ([]byte, string, error) {
		n, err := strconv.Atoi(r.URL.Query().Get("n"))
		if err != nil || n < 0 {
			return nil, "", fmt.Errorf("%w: n must be a non-negative integer", domain.ErrInvalidArgument)
		}
		b, err := h.deps.Media.Segment(r.Context(), src, n)
		return b, contentTypeSegment, err
	}))
	r.Get(MediaPrefix+"{bucket}/key", h.key)
	// What the media worker derived from the video.
	for name, file := range map[string]string{"poster.jpg": domain.MediaPosterFile, "sprite.jpg": domain.MediaSpriteFile} {
		r.Get(MediaPrefix+"{bucket}/"+name, h.serve(false, func(r *http.Request, src media.Source, _ media.Links) ([]byte, string, error) {
			b, err := h.deps.Media.Sidecar(r.Context(), src, file)
			return b, contentTypeJPEG, err
		}))
	}
	r.Get(MediaPrefix+"{bucket}/thumbnails.vtt", h.serve(false, func(r *http.Request, src media.Source, l media.Links) ([]byte, string, error) {
		b, err := h.deps.Media.Sidecar(r.Context(), src, domain.MediaThumbnailsFile)
		if err != nil {
			return nil, "", err
		}
		return media.RewriteSprite(b, l.(mediaLinks).file("sprite.jpg")), contentTypeVTT, nil
	}))
	r.Get(MediaPrefix+"{bucket}/subtitles.m3u8", h.serve(false, func(r *http.Request, src media.Source, l media.Links) ([]byte, string, error) {
		b, err := h.deps.Media.SubtitlePlaylist(r.Context(), src, r.URL.Query().Get("lang"), l)
		return b, contentTypePlaylist, err
	}))
	r.Get(MediaPrefix+"{bucket}/subtitles.vtt", h.serve(false, func(r *http.Request, src media.Source, _ media.Links) ([]byte, string, error) {
		b, err := h.deps.Media.SubtitleFile(r.Context(), src, r.URL.Query().Get("lang"))
		return b, contentTypeVTT, err
	}))
	r.Get(MediaPrefix+"{bucket}/status.json", h.serve(false, func(r *http.Request, src media.Source, l media.Links) ([]byte, string, error) {
		st, err := h.deps.Media.Status(r.Context(), src)
		if err != nil {
			return nil, "", err
		}
		b, err := json.Marshal(newMediaStatus(st, l.(mediaLinks)))
		return b, "application/json", err
	}))
	// The variants of an image, the key in the path (an <img src> or a
	// CDN keeps the extension of the key).
	r.Get(MediaPrefix+"{bucket}/image/*", h.image)
	r.Head(MediaPrefix+"{bucket}/image/*", h.image)
}

// mediaStatus is the processing state of a video or an image by the
// media worker (status.json of the media routes).
type mediaStatus struct {
	// State is processing, done or failed; none when the worker never
	// processed the version.
	State      string                        `json:"state"`
	Current    bool                          `json:"current"`
	Error      string                        `json:"error,omitempty"`
	Duration   float64                       `json:"duration,omitempty"`
	Width      int                           `json:"width,omitempty"`
	Height     int                           `json:"height,omitempty"`
	Renditions []domain.MediaRenditionOutput `json:"renditions,omitempty"`
	Poster     bool                          `json:"poster,omitempty"`
	Thumbnails *domain.MediaThumbnailsOutput `json:"thumbnails,omitempty"`
	Images     []mediaImage                  `json:"images,omitempty"`
	Alpha      bool                          `json:"alpha,omitempty"`
}

// mediaImage is a variant of an image with its URI, relative to
// status.json (presigned like the request).
type mediaImage struct {
	domain.MediaImageOutput
	ContentType string `json:"contentType"`
	URL         string `json:"url"`
}

func newMediaStatus(st *media.OutputStatus, l mediaLinks) mediaStatus {
	if st == nil {
		return mediaStatus{State: "none"}
	}
	out := mediaStatus{State: string(st.State), Current: st.Current, Error: st.Error, Duration: st.Duration,
		Width: st.Width, Height: st.Height, Renditions: st.Renditions, Poster: st.Poster, Thumbnails: st.Thumbnails, Alpha: st.Alpha}
	for _, v := range st.Images {
		out.Images = append(out.Images, mediaImage{MediaImageOutput: v, ContentType: domain.MediaImageContentType(v.Format), URL: l.image(v.Width, v.Format)})
	}
	return out
}

// imageKey returns the bucket and the key of an image request
// (/nexo/media/v1/{bucket}/image/{key}): the key is the rest of the
// decoded path, so that it may hold any character.
func imageKey(r *http.Request) (bucket, key string) {
	bucket = chi.URLParam(r, "bucket")
	return bucket, strings.TrimPrefix(r.URL.Path, MediaPrefix+bucket+"/image/")
}

// image serves a variant of an image the media worker derived from the
// current content of the object: ?w= the width the client displays,
// ?format= auto (the default: the most compact format of the Accept
// header) or a format, ?versionId= a version. It answers ranges and
// conditional requests (the ETag names the variant), and varies on Accept
// with format auto.
func (h *mediaRoutes) image(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	bucket, key := imageKey(r)
	src := media.Source{Bucket: bucket, Key: key, VersionID: q.Get("versionId")}
	if err := domain.ValidateObjectKey(src.Key); err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	req, err := imageRequest(r)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	action := policy.ActionGetObject
	if src.VersionID != "" {
		action = policy.ActionGetObjectVersion
	}
	if !h.authz.authorizeVersion(w, r, action, src.Bucket, src.Key, src.VersionID, nil) {
		return
	}
	img, err := h.deps.Media.Image(r.Context(), src, req)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	serveImage(w, r, img, req)
}

// imageRequest reads the variant a request asks for: ?w=, ?format= and
// the Accept header.
func imageRequest(r *http.Request) (media.ImageRequest, error) {
	q := r.URL.Query()
	width, err := media.ParseImageWidth(q.Get("w"))
	if err != nil {
		return media.ImageRequest{}, err
	}
	format, err := media.ParseImageFormat(q.Get("format"))
	if err != nil {
		return media.ImageRequest{}, err
	}
	return media.ImageRequest{Width: width, Format: format, Accept: r.Header.Get("Accept")}, nil
}

// serveImage writes a variant of an image: its type, its ETag and
// modification time for the conditional requests, the ranges
// (http.ServeContent), and Vary: Accept when the format was negotiated.
// Private and revalidated like the other files of the media worker: the
// variants follow the current content of the object.
func serveImage(w http.ResponseWriter, r *http.Request, img *media.Image, req media.ImageRequest) {
	defer func() { _ = img.Content.Close() }()
	hdr := w.Header()
	hdr.Set("Content-Type", img.ContentType)
	hdr.Set("ETag", img.ETag)
	hdr.Set("Cache-Control", "private, no-cache")
	hdr.Set("X-Content-Type-Options", "nosniff")
	if req.Format == media.ImageFormatAuto {
		hdr.Add("Vary", "Accept")
	}
	http.ServeContent(w, r, "", img.Modified, img.Content)
}

// authorizeStream authorizes the stream of a video: nexo:GetMediaStream
// (the presentation without the original), else s3:GetObject
// (s3:GetObjectVersion with versionId), whose refusal is answered.
func (h *mediaRoutes) authorizeStream(w http.ResponseWriter, r *http.Request, src media.Source) bool {
	if h.authz.allowed(r, policy.ActionGetMediaStream, src.Bucket, src.Key, src.VersionID) {
		return true
	}
	action := policy.ActionGetObject
	if src.VersionID != "" {
		action = policy.ActionGetObjectVersion
	}
	return h.authz.authorizeVersion(w, r, action, src.Bucket, src.Key, src.VersionID, nil)
}

// serve authorizes and serves one resource of a presentation; pinned
// requires the revision (every URI but the master playlist).
func (h *mediaRoutes) serve(pinned bool, build func(r *http.Request, src media.Source, l media.Links) ([]byte, string, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		src := media.Source{Bucket: chi.URLParam(r, "bucket"), Key: q.Get("key"), VersionID: q.Get("versionId"), Revision: q.Get("rev"), Rendition: q.Get("r")}
		if err := domain.ValidateObjectKey(src.Key); err != nil {
			h.deps.Enc.WriteS3Error(w, r, err)
			return
		}
		if pinned && src.Revision == "" {
			h.deps.Enc.WriteS3Error(w, r, fmt.Errorf("%w: rev is required", domain.ErrInvalidArgument))
			return
		}
		if !h.authorizeStream(w, r, src) {
			return
		}
		links, err := h.links(r, src)
		if err != nil {
			h.deps.Enc.WriteS3Error(w, r, err)
			return
		}
		body, contentType, err := build(r, src, links)
		if err != nil {
			h.deps.Enc.WriteS3Error(w, r, err)
			return
		}
		hdr := w.Header()
		hdr.Set("Content-Type", contentType)
		hdr.Set("Content-Length", strconv.Itoa(len(body)))
		hdr.Set("X-Content-Type-Options", "nosniff")
		// Private: a shared cache would keep serving a revoked link.
		if pinned {
			hdr.Set("Cache-Control", "private, max-age=86400, immutable")
		} else {
			hdr.Set("Cache-Control", "private, no-cache")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}
}

// key serves the AES-128 key of a period (?i=) of a protected video, never
// cached. It answers a request signed in its headers by an identity
// allowed to stream the video (owners previewing their videos), a
// presigned one (a share link) when the protection lets share links play,
// and no anonymous request; the Origin of a browser must be allowed.
func (h *mediaRoutes) key(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	src := media.Source{Bucket: chi.URLParam(r, "bucket"), Key: q.Get("key"), VersionID: q.Get("versionId"), Revision: q.Get("rev"), Rendition: q.Get("r")}
	if err := domain.ValidateObjectKey(src.Key); err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	period, err := strconv.Atoi(q.Get("i"))
	if err != nil || period < 0 || src.Revision == "" {
		h.deps.Enc.WriteS3Error(w, r, fmt.Errorf("%w: rev and a non-negative i are required", domain.ErrInvalidArgument))
		return
	}
	if !h.authorizeStream(w, r, src) {
		return
	}
	prot, err := h.deps.Media.Protection(r.Context(), src)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	if prot == nil {
		h.deps.Enc.WriteS3Error(w, r, fmt.Errorf("%w: %s is not a protected video", domain.ErrNotFound, src.Key))
		return
	}
	id, ok := middleware.IdentityFromContext(r.Context())
	switch {
	case !ok || id.IsAnonymous:
		h.deps.Enc.WriteS3Error(w, r, fmt.Errorf("%w: a protected video plays with a playback token", domain.ErrAccessDenied))
		return
	case auth.IsPresigned(q) && !prot.ShareLinks:
		h.deps.Enc.WriteS3Error(w, r, fmt.Errorf("%w: the share links of this bucket do not play protected videos", domain.ErrAccessDenied))
		return
	case !prot.AllowsOrigin(r.Header.Get("Origin")):
		h.deps.Enc.WriteS3Error(w, r, fmt.Errorf("%w: origin not allowed", domain.ErrAccessDenied))
		return
	}
	key, err := h.deps.Media.Key(r.Context(), src, period)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	hdr := w.Header()
	hdr.Set("Content-Type", "application/octet-stream")
	hdr.Set("Content-Length", strconv.Itoa(len(key)))
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(key)
}

// player serves the player page: static, it reads the playlist from its
// fragment and holds no data.
func (h *mediaRoutes) player(w http.ResponseWriter, r *http.Request) {
	if h.deps.Player == nil {
		h.deps.Enc.WriteS3Error(w, r, fmt.Errorf("%w: the player is not embedded in this build", domain.ErrNotFound))
		return
	}
	page, err := fs.ReadFile(h.deps.Player, "player.html")
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, fmt.Errorf("%w: player page: %v", domain.ErrNotFound, err))
		return
	}
	hdr := w.Header()
	hdr.Set("Content-Type", "text/html; charset=utf-8")
	hdr.Set("Content-Length", strconv.Itoa(len(page)))
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("Content-Security-Policy", playerCSP)
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(page)
}

// playerAsset serves a script or a stylesheet of the build: fingerprinted,
// cached for good.
func (h *mediaRoutes) playerAsset(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if h.deps.Player == nil || !playerAssetName(name) {
		h.deps.Enc.WriteS3Error(w, r, fmt.Errorf("%w: %s", domain.ErrNotFound, name))
		return
	}
	data, err := fs.ReadFile(h.deps.Player, "assets/"+name)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, fmt.Errorf("%w: %s", domain.ErrNotFound, name))
		return
	}
	hdr := w.Header()
	hdr.Set("Content-Type", mime.TypeByExtension(path.Ext(name)))
	hdr.Set("Content-Length", strconv.Itoa(len(data)))
	hdr.Set("Cache-Control", "public, max-age=31536000, immutable")
	hdr.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// links returns the URIs of the playlists of a request, relative to it.
// The URIs of a presigned request are presigned by its signer until the
// request expires; the others carry no signature (an anonymous reader of a
// public object needs none, a request signed in its headers signs each of
// its requests).
func (h *mediaRoutes) links(r *http.Request, src media.Source) (media.Links, error) {
	l := mediaLinks{key: src.Key, versionID: src.VersionID, rendition: src.Rendition}
	q := r.URL.Query()
	id, ok := middleware.IdentityFromContext(r.Context())
	if !auth.IsPresigned(q) || !ok || id.IsAnonymous || id.AccessKey == "" {
		return l, nil
	}
	signedAt := parseAmzDate(q.Get("X-Amz-Date"))
	validity, err := strconv.ParseInt(q.Get("X-Amz-Expires"), 10, 64)
	if signedAt.IsZero() || err != nil || validity <= 0 {
		return nil, fmt.Errorf("%w: malformed presigned request", domain.ErrInvalidArgument)
	}
	now := h.deps.Clock.Now()
	left := signedAt.Add(time.Duration(validity) * time.Second).Sub(now).Truncate(time.Second)
	if left < time.Second {
		return nil, fmt.Errorf("%w: the link has expired", domain.ErrRequestExpired)
	}
	creds, _, err := h.deps.Creds.LookupCredentials(r.Context(), id.AccessKey)
	if err != nil {
		return nil, err
	}
	base := url.URL{Scheme: "http", Host: r.Host, Path: path.Dir(r.URL.Path) + "/"}
	l.sign = func(name string, query url.Values) string {
		u := base
		u.Path += name
		u.RawQuery = query.Encode()
		signed, err := h.deps.Signer.Presign(r.Context(), auth.PresignInput{URL: &u, Expires: left, Creds: creds, Region: h.opts.region, Now: now})
		if err != nil {
			// The signer refuses only malformed input, which the URL
			// built here is not: an unsigned URI is denied when used.
			h.deps.Logger.ErrorContext(r.Context(), "s3: media: presign", "err", err)
			return query.Encode()
		}
		return signed.RawQuery
	}
	return l, nil
}

// mediaLinks are the URIs of the playlists, relative to the master
// playlist, optionally presigned.
type mediaLinks struct {
	key, versionID, rendition string
	// sign returns the presigned query of the URI name (a decoded path
	// relative to the request) with query.
	sign func(name string, query url.Values) string
}

// link returns the relative URI name?query, presigned when the request
// was: name is a decoded path, escaped in the URI.
func (l mediaLinks) link(name string, q url.Values) string {
	escaped := (&url.URL{Path: name}).EscapedPath()
	if l.sign != nil {
		return escaped + "?" + l.sign(name, q)
	}
	return escaped + "?" + q.Encode()
}

// image is the URI of a variant of the image of the object (width 0: its
// own size): the image route, the key in its path.
func (l mediaLinks) image(width int, format string) string {
	q := url.Values{"format": {format}}
	if width > 0 {
		q.Set("w", strconv.Itoa(width))
	}
	if l.versionID != "" {
		q.Set("versionId", l.versionID)
	}
	return l.link("image/"+l.key, q)
}

var _ media.Links = mediaLinks{}

func (l mediaLinks) uri(name, rev string, extra url.Values) string {
	q := url.Values{"key": {l.key}, "rev": {rev}}
	if l.versionID != "" {
		q.Set("versionId", l.versionID)
	}
	if l.rendition != "" {
		q.Set("r", l.rendition)
	}
	for k, v := range extra {
		q[k] = v
	}
	return l.link(name, q)
}

func (l mediaLinks) Media(rev string) string { return l.uri("media.m3u8", rev, nil) }

func (l mediaLinks) Init(rev string) string { return l.uri("init.mp4", rev, nil) }

func (l mediaLinks) Segment(rev string, n int) string {
	return l.uri("segment.m4s", rev, url.Values{"n": {strconv.Itoa(n)}})
}

func (l mediaLinks) Key(rev string, period int) string {
	return l.uri("key", rev, url.Values{"i": {strconv.Itoa(period)}})
}

// file is the URI of a file of the media worker of the object (no
// revision: the files follow the current content).
func (l mediaLinks) file(name string) string {
	q := url.Values{"key": {l.key}}
	if l.versionID != "" {
		q.Set("versionId", l.versionID)
	}
	return l.link(name, q)
}

func (l mediaLinks) Subtitles(lang string) string { return l.subtitle("subtitles.m3u8", lang) }

func (l mediaLinks) SubtitleFile(lang string) string { return l.subtitle("subtitles.vtt", lang) }

// subtitle is the URI of a subtitle file of a language (no revision: the
// tracks follow the current content).
func (l mediaLinks) subtitle(name, lang string) string {
	q := url.Values{"key": {l.key}, "lang": {lang}}
	if l.versionID != "" {
		q.Set("versionId", l.versionID)
	}
	return l.link(name, q)
}

func (l mediaLinks) Rendition(name, rev string) string {
	l.rendition = name
	return l.uri("media.m3u8", rev, nil)
}
