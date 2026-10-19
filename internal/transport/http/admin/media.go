package admin

import (
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	adminsvc "github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// MediaRoutes exposes the video configuration of the buckets, the rules
// of the media worker (/buckets/{bucket}/media), and the reprocessing of
// a video.
type MediaRoutes interface{ Routes }

// MediaDeps are the dependencies of the media routes.
type MediaDeps struct {
	// Media validates and stores the configurations (required).
	Media adminsvc.MediaManager
	// Authz authorizes the routes (required).
	Authz adminsvc.Authorizer
	Enc   httpx.Codec
}

// MediaOption configures the media routes.
type MediaOption func(*mediaRoutes)

type mediaRoutes struct {
	deps  MediaDeps
	guard guard
}

var _ MediaRoutes = (*mediaRoutes)(nil)

// NewMediaRoutes returns the media routes.
func NewMediaRoutes(deps MediaDeps, opts ...MediaOption) (MediaRoutes, error) {
	if deps.Media == nil || deps.Authz == nil {
		return nil, fmt.Errorf("%w: media routes require the media manager and the authorizer", domain.ErrInvalidArgument)
	}
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	h := &mediaRoutes{deps: deps, guard: guard{authz: deps.Authz, enc: deps.Enc}}
	for _, o := range opts {
		o(h)
	}
	return h, nil
}

// Register implements Routes.
func (h *mediaRoutes) Register(r chi.Router) { h.guard.register(r, h.routes()) }

func (h *mediaRoutes) routes() []route {
	return []route{
		{http.MethodGet, "/buckets/{bucket}/media", onBucket(policy.ActionAdminGetBucketMedia), h.get},
		{http.MethodPut, "/buckets/{bucket}/media", onBucket(policy.ActionAdminSetBucketMedia), h.put},
		{http.MethodDelete, "/buckets/{bucket}/media", onBucket(policy.ActionAdminSetBucketMedia), h.remove},
		{http.MethodPost, "/buckets/{bucket}/media/reprocess", onBucket(policy.ActionAdminSetBucketMedia), h.reprocess},
	}
}

// MediaConfigResponse is the video configuration of a bucket, the body
// of GET and PUT /buckets/{bucket}/media: the first rule selecting a key
// applies.
type MediaConfigResponse struct {
	Rules []MediaRuleResponse `json:"rules"`
}

// MediaRuleResponse is one media rule.
type MediaRuleResponse struct {
	ID         string                   `json:"id"`
	Prefix     string                   `json:"prefix,omitempty"`
	Suffixes   []string                 `json:"suffixes,omitempty"`
	Renditions []string                 `json:"renditions,omitempty"`
	Poster     bool                     `json:"poster,omitempty"`
	Thumbnails *MediaThumbnailsResponse `json:"thumbnails,omitempty"`
	// Images makes an image rule: the variants of every image it selects.
	Images *MediaImagesResponse `json:"images,omitempty"`
}

// MediaImagesResponse configures the variants of an image rule.
type MediaImagesResponse struct {
	// Formats of the variants: avif, webp, jpeg, png (default all).
	Formats []string `json:"formats,omitempty"`
	// Widths of the variants in pixels, 16 to 8192 (default 160, 320,
	// 640, 1280, 1920, 2560); each format also gets the image at its own
	// size.
	Widths []int `json:"widths,omitempty"`
}

// MediaThumbnailsResponse configures the timeline thumbnails of a rule.
type MediaThumbnailsResponse struct {
	// Interval is a Go duration from 1s to 10m ("10s" when empty).
	Interval string `json:"interval,omitempty"`
}

// MediaReprocessRequest is the body of POST
// /buckets/{bucket}/media/reprocess.
type MediaReprocessRequest struct {
	Key string `json:"key"`
	// VersionID is the version to process ("": the latest).
	VersionID string `json:"versionId,omitempty"`
}

func newMediaResponse(cfg *domain.MediaConfig) MediaConfigResponse {
	out := MediaConfigResponse{Rules: make([]MediaRuleResponse, 0, len(cfg.Rules))}
	for _, r := range cfg.Rules {
		rule := MediaRuleResponse{ID: r.ID, Prefix: r.Prefix, Suffixes: r.Suffixes, Renditions: r.Renditions, Poster: r.Poster}
		if r.Thumbnails != nil {
			rule.Thumbnails = &MediaThumbnailsResponse{Interval: r.Thumbnails.Interval}
		}
		if r.Images != nil {
			rule.Images = &MediaImagesResponse{Formats: r.Images.Formats, Widths: r.Images.Widths}
		}
		out.Rules = append(out.Rules, rule)
	}
	return out
}

func (h *mediaRoutes) fail(w http.ResponseWriter, r *http.Request, err error) {
	writeError(h.deps.Enc, w, r, err)
}

func (h *mediaRoutes) get(w http.ResponseWriter, r *http.Request) {
	cfg, err := h.deps.Media.Get(r.Context(), bucketName(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newMediaResponse(cfg))
}

func (h *mediaRoutes) put(w http.ResponseWriter, r *http.Request) {
	var in MediaConfigResponse
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	cfg := domain.MediaConfig{Rules: make([]domain.MediaRule, 0, len(in.Rules))}
	for _, rule := range in.Rules {
		mr := domain.MediaRule{ID: rule.ID, Prefix: rule.Prefix, Suffixes: rule.Suffixes, Renditions: rule.Renditions, Poster: rule.Poster}
		if rule.Thumbnails != nil {
			mr.Thumbnails = &domain.MediaThumbnails{Interval: rule.Thumbnails.Interval}
		}
		if rule.Images != nil {
			mr.Images = &domain.MediaImages{Formats: rule.Images.Formats, Widths: rule.Images.Widths}
		}
		cfg.Rules = append(cfg.Rules, mr)
	}
	out, err := h.deps.Media.Set(r.Context(), bucketName(r), cfg)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newMediaResponse(out))
}

func (h *mediaRoutes) remove(w http.ResponseWriter, r *http.Request) {
	if err := h.deps.Media.Delete(r.Context(), bucketName(r)); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *mediaRoutes) reprocess(w http.ResponseWriter, r *http.Request) {
	var in MediaReprocessRequest
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.deps.Media.Reprocess(r.Context(), bucketName(r), in.Key, in.VersionID); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
