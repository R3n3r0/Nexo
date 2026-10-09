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

// LifecycleRoutes exposes the lifecycle configuration of the buckets
// (/buckets/{bucket}/lifecycle) and its immediate application.
type LifecycleRoutes interface{ Routes }

// LifecycleDeps are the dependencies of the lifecycle routes.
type LifecycleDeps struct {
	// Lifecycle converts, validates, stores and applies the
	// configurations (required).
	Lifecycle adminsvc.LifecycleManager
	// Authz authorizes the routes (required).
	Authz adminsvc.Authorizer
	Enc   httpx.Codec
}

// LifecycleOption configures the lifecycle routes.
type LifecycleOption func(*lifecycleRoutes)

type lifecycleRoutes struct {
	deps  LifecycleDeps
	guard guard
}

var _ LifecycleRoutes = (*lifecycleRoutes)(nil)

// NewLifecycleRoutes returns the lifecycle routes.
func NewLifecycleRoutes(deps LifecycleDeps, opts ...LifecycleOption) (LifecycleRoutes, error) {
	if deps.Lifecycle == nil || deps.Authz == nil {
		return nil, fmt.Errorf("%w: lifecycle routes require the lifecycle manager and the authorizer", domain.ErrInvalidArgument)
	}
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	h := &lifecycleRoutes{deps: deps, guard: guard{authz: deps.Authz, enc: deps.Enc}}
	for _, o := range opts {
		o(h)
	}
	return h, nil
}

// Register implements Routes.
func (h *lifecycleRoutes) Register(r chi.Router) { h.guard.register(r, h.routes()) }

func (h *lifecycleRoutes) routes() []route {
	return []route{
		{http.MethodGet, "/buckets/{bucket}/lifecycle", onBucket(policy.ActionGetLifecycle), h.get},
		{http.MethodPut, "/buckets/{bucket}/lifecycle", onBucket(policy.ActionPutLifecycle), h.put},
		// S3 authorizes DeleteBucketLifecycle as s3:PutLifecycleConfiguration.
		{http.MethodDelete, "/buckets/{bucket}/lifecycle", onBucket(policy.ActionPutLifecycle), h.remove},
		// Applying the rules at once is an administrative action on top of
		// the right to configure them.
		{http.MethodPost, "/buckets/{bucket}/lifecycle/apply", onBucket(policy.ActionPutLifecycle, policy.ActionAdminApplyLifecycle), h.apply},
	}
}

// LifecycleResponse is the lifecycle configuration of a bucket, the
// body of GET and PUT /buckets/{bucket}/lifecycle.
type LifecycleResponse struct {
	Rules []LifecycleRuleResponse `json:"rules"`
}

// LifecycleRuleResponse is one lifecycle rule (S3 Rule). A number sent as 0
// is validated as such.
type LifecycleRuleResponse struct {
	// ID is generated when empty.
	ID string `json:"id,omitempty"`
	// Enabled defaults to true when absent.
	Enabled               *bool                                   `json:"enabled,omitempty"`
	Filter                *LifecycleFilterResponse                `json:"filter,omitempty"`
	Expiration            *LifecycleExpirationResponse            `json:"expiration,omitempty"`
	NoncurrentExpiration  *LifecycleNoncurrentExpirationResponse  `json:"noncurrentExpiration,omitempty"`
	AbortIncompleteUpload *LifecycleAbortResponse                 `json:"abortIncompleteUpload,omitempty"`
	Transitions           []LifecycleTransitionResponse           `json:"transitions,omitempty"`
	NoncurrentTransitions []LifecycleNoncurrentTransitionResponse `json:"noncurrentTransitions,omitempty"`
}

// LifecycleFilterResponse selects the objects of a rule.
type LifecycleFilterResponse struct {
	Prefix          string            `json:"prefix,omitempty"`
	Tags            map[string]string `json:"tags,omitempty"`
	SizeGreaterThan *int64            `json:"sizeGreaterThan,omitempty"`
	SizeLessThan    *int64            `json:"sizeLessThan,omitempty"`
}

// LifecycleExpirationResponse expires the current versions: exactly one of
// days, date or expiredObjectDeleteMarker.
type LifecycleExpirationResponse struct {
	Days *int `json:"days,omitempty"`
	// Date is an ISO 8601 date at midnight UTC.
	Date                      string `json:"date,omitempty"`
	ExpiredObjectDeleteMarker bool   `json:"expiredObjectDeleteMarker,omitempty"`
}

// LifecycleNoncurrentExpirationResponse removes the noncurrent versions.
type LifecycleNoncurrentExpirationResponse struct {
	NoncurrentDays          *int `json:"noncurrentDays,omitempty"`
	NewerNoncurrentVersions *int `json:"newerNoncurrentVersions,omitempty"`
}

// LifecycleAbortResponse aborts the incomplete multipart uploads.
type LifecycleAbortResponse struct {
	DaysAfterInitiation *int `json:"daysAfterInitiation,omitempty"`
}

// LifecycleTransitionResponse moves the current versions to a tier.
type LifecycleTransitionResponse struct {
	Days         *int   `json:"days,omitempty"`
	Date         string `json:"date,omitempty"`
	StorageClass string `json:"storageClass"`
}

// LifecycleNoncurrentTransitionResponse moves the noncurrent versions to a tier.
type LifecycleNoncurrentTransitionResponse struct {
	NoncurrentDays          *int   `json:"noncurrentDays,omitempty"`
	NewerNoncurrentVersions *int   `json:"newerNoncurrentVersions,omitempty"`
	StorageClass            string `json:"storageClass"`
}

// LifecycleApplyResponse is the body of POST
// /buckets/{bucket}/lifecycle/apply.
type LifecycleApplyResponse struct {
	// Buckets is 1 when the bucket has a configuration, Versions the
	// versions evaluated.
	Buckets  int `json:"buckets"`
	Versions int `json:"versions"`
	// Deleted counts the versions and delete markers removed,
	// DeleteMarkers those created, UploadsAborted the aborted uploads and
	// Locked the expirations Object Lock prevented.
	Deleted        int `json:"deleted"`
	DeleteMarkers  int `json:"deleteMarkers"`
	UploadsAborted int `json:"uploadsAborted"`
	Locked         int `json:"locked"`
}

// lifecycleToService converts the request to the console form of the
// service.
func lifecycleToService(in LifecycleResponse) adminsvc.LifecycleConfig {
	out := adminsvc.LifecycleConfig{Rules: make([]adminsvc.LifecycleRule, 0, len(in.Rules))}
	for _, r := range in.Rules {
		rule := adminsvc.LifecycleRule{ID: r.ID, Enabled: r.Enabled == nil || *r.Enabled}
		if f := r.Filter; f != nil {
			rule.Filter = adminsvc.LifecycleFilter{Prefix: f.Prefix, Tags: f.Tags, SizeGreaterThan: f.SizeGreaterThan, SizeLessThan: f.SizeLessThan}
		}
		if e := r.Expiration; e != nil {
			rule.Expiration = &adminsvc.LifecycleExpiration{Days: e.Days, Date: e.Date, ExpiredObjectDeleteMarker: e.ExpiredObjectDeleteMarker}
		}
		if n := r.NoncurrentExpiration; n != nil {
			rule.NoncurrentExpiration = &adminsvc.LifecycleNoncurrentExpiration{NoncurrentDays: n.NoncurrentDays, NewerNoncurrentVersions: n.NewerNoncurrentVersions}
		}
		if a := r.AbortIncompleteUpload; a != nil {
			rule.AbortIncompleteUpload = &adminsvc.LifecycleAbortUpload{DaysAfterInitiation: a.DaysAfterInitiation}
		}
		for _, t := range r.Transitions {
			rule.Transitions = append(rule.Transitions, adminsvc.LifecycleTransition(t))
		}
		for _, t := range r.NoncurrentTransitions {
			rule.NoncurrentTransitions = append(rule.NoncurrentTransitions, adminsvc.LifecycleNoncurrentTransition(t))
		}
		out.Rules = append(out.Rules, rule)
	}
	return out
}

// newLifecycleResponse converts the console form of the service.
func newLifecycleResponse(in *adminsvc.LifecycleConfig) LifecycleResponse {
	out := LifecycleResponse{Rules: make([]LifecycleRuleResponse, 0, len(in.Rules))}
	for _, r := range in.Rules {
		enabled := r.Enabled
		rule := LifecycleRuleResponse{ID: r.ID, Enabled: &enabled}
		if f := r.Filter; f.Prefix != "" || len(f.Tags) > 0 || f.SizeGreaterThan != nil || f.SizeLessThan != nil {
			rule.Filter = &LifecycleFilterResponse{Prefix: f.Prefix, Tags: f.Tags, SizeGreaterThan: f.SizeGreaterThan, SizeLessThan: f.SizeLessThan}
		}
		if e := r.Expiration; e != nil {
			rule.Expiration = &LifecycleExpirationResponse{Days: e.Days, Date: e.Date, ExpiredObjectDeleteMarker: e.ExpiredObjectDeleteMarker}
		}
		if n := r.NoncurrentExpiration; n != nil {
			rule.NoncurrentExpiration = &LifecycleNoncurrentExpirationResponse{NoncurrentDays: n.NoncurrentDays, NewerNoncurrentVersions: n.NewerNoncurrentVersions}
		}
		if a := r.AbortIncompleteUpload; a != nil {
			rule.AbortIncompleteUpload = &LifecycleAbortResponse{DaysAfterInitiation: a.DaysAfterInitiation}
		}
		for _, t := range r.Transitions {
			rule.Transitions = append(rule.Transitions, LifecycleTransitionResponse(t))
		}
		for _, t := range r.NoncurrentTransitions {
			rule.NoncurrentTransitions = append(rule.NoncurrentTransitions, LifecycleNoncurrentTransitionResponse(t))
		}
		out.Rules = append(out.Rules, rule)
	}
	return out
}

func (h *lifecycleRoutes) fail(w http.ResponseWriter, r *http.Request, err error) {
	writeError(h.deps.Enc, w, r, err)
}

func (h *lifecycleRoutes) get(w http.ResponseWriter, r *http.Request) {
	cfg, err := h.deps.Lifecycle.Get(r.Context(), bucketName(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newLifecycleResponse(cfg))
}

func (h *lifecycleRoutes) put(w http.ResponseWriter, r *http.Request) {
	var in LifecycleResponse
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	cfg, err := h.deps.Lifecycle.Set(r.Context(), bucketName(r), lifecycleToService(in))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newLifecycleResponse(cfg))
}

func (h *lifecycleRoutes) remove(w http.ResponseWriter, r *http.Request) {
	if err := h.deps.Lifecycle.Delete(r.Context(), bucketName(r)); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *lifecycleRoutes) apply(w http.ResponseWriter, r *http.Request) {
	res, err := h.deps.Lifecycle.Apply(r.Context(), bucketName(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, LifecycleApplyResponse{
		Buckets: res.Buckets, Versions: res.Versions, Deleted: res.Deleted, DeleteMarkers: res.DeleteMarkers,
		UploadsAborted: res.UploadsAborted, Locked: res.Locked,
	})
}
