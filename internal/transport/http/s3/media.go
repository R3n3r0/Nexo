package s3

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strconv"
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

// The content types of HLS (RFC 8216 §4 and the fMP4 segments).
const (
	contentTypePlaylist = "application/vnd.apple.mpegurl"
	contentTypeInit     = "video/mp4"
	contentTypeSegment  = "video/iso.segment"
)

// MediaDeps are the dependencies of the media routes; all but Objects,
// Clock and Logger are required.
type MediaDeps struct {
	// Media packages the videos as HLS.
	Media media.Service
	// Policy authorizes s3:GetObject (s3:GetObjectVersion) as GetObject
	// does; Objects provides the tags of the objects for the
	// s3:ExistingObjectTag/<key> condition keys.
	Policy  policy.Service
	Objects object.Service
	// Signer and Creds sign the URIs of the playlists of a presigned
	// request (a share link) with the credentials that signed it.
	Signer auth.Signer
	Creds  auth.CredentialLookup
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
// video, readable by whoever may read the object (s3:GetObject, or
// s3:GetObjectVersion with versionId) and signed by the auth middleware
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
}

// serve authorizes and serves one resource of a presentation; pinned
// requires the revision (every URI but the master playlist).
func (h *mediaRoutes) serve(pinned bool, build func(r *http.Request, src media.Source, l media.Links) ([]byte, string, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		src := media.Source{Bucket: chi.URLParam(r, "bucket"), Key: q.Get("key"), VersionID: q.Get("versionId"), Revision: q.Get("rev")}
		if err := domain.ValidateObjectKey(src.Key); err != nil {
			h.deps.Enc.WriteS3Error(w, r, err)
			return
		}
		if pinned && src.Revision == "" {
			h.deps.Enc.WriteS3Error(w, r, fmt.Errorf("%w: rev is required", domain.ErrInvalidArgument))
			return
		}
		action := policy.ActionGetObject
		if src.VersionID != "" {
			action = policy.ActionGetObjectVersion
		}
		if !h.authz.authorizeVersion(w, r, action, src.Bucket, src.Key, src.VersionID, nil) {
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

// links returns the URIs of the playlists of a request, relative to it.
// The URIs of a presigned request are presigned by its signer until the
// request expires; the others carry no signature (an anonymous reader of a
// public object needs none, a request signed in its headers signs each of
// its requests).
func (h *mediaRoutes) links(r *http.Request, src media.Source) (media.Links, error) {
	l := mediaLinks{key: src.Key, versionID: src.VersionID}
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
			return name + "?" + query.Encode()
		}
		return name + "?" + signed.RawQuery
	}
	return l, nil
}

// mediaLinks are the URIs of the playlists, relative to the master
// playlist, optionally presigned.
type mediaLinks struct {
	key, versionID string
	sign           func(name string, query url.Values) string
}

var _ media.Links = mediaLinks{}

func (l mediaLinks) uri(name, rev string, extra url.Values) string {
	q := url.Values{"key": {l.key}, "rev": {rev}}
	if l.versionID != "" {
		q.Set("versionId", l.versionID)
	}
	for k, v := range extra {
		q[k] = v
	}
	if l.sign != nil {
		return l.sign(name, q)
	}
	return name + "?" + q.Encode()
}

func (l mediaLinks) Media(rev string) string { return l.uri("media.m3u8", rev, nil) }

func (l mediaLinks) Init(rev string) string { return l.uri("init.mp4", rev, nil) }

func (l mediaLinks) Segment(rev string, n int) string {
	return l.uri("segment.m4s", rev, url.Values{"n": {strconv.Itoa(n)}})
}
