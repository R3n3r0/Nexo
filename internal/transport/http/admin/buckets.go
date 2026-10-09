package admin

import (
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	adminsvc "github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// maxPolicyBody bounds a bucket policy document (S3: 20 KiB; the policy
// engine reports PolicyTooLarge beyond).
const maxPolicyBody = 64 << 10

// BucketDeps are the dependencies of the admin bucket routes.
type BucketDeps struct {
	// Manager lists, creates and deletes buckets and manages the
	// anonymous access presets (required).
	Manager adminsvc.BucketManager
	// Buckets serves the bucket settings (required).
	Buckets bucket.Service
	// Authz authorizes the routes (required).
	Authz adminsvc.Authorizer
	Enc   httpx.Codec
}

// BucketOption configures the bucket routes (none yet: the type keeps the
// constructor shape of every route group, docs/12 §12.3).
type BucketOption func(*bucketRoutes)

type bucketRoutes struct {
	deps  BucketDeps
	guard guard
}

var _ BucketRoutes = (*bucketRoutes)(nil)

// NewBucketRoutes returns the admin bucket routes.
func NewBucketRoutes(deps BucketDeps, opts ...BucketOption) (BucketRoutes, error) {
	if deps.Manager == nil || deps.Buckets == nil || deps.Authz == nil {
		return nil, fmt.Errorf("%w: bucket routes require the bucket manager, the bucket service and the authorizer", domain.ErrInvalidArgument)
	}
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	h := &bucketRoutes{deps: deps, guard: guard{authz: deps.Authz, enc: deps.Enc}}
	for _, o := range opts {
		o(h)
	}
	return h, nil
}

// Register implements Routes.
func (h *bucketRoutes) Register(r chi.Router) { h.guard.register(r, h.routes()) }

func (h *bucketRoutes) routes() []route {
	ni := notImplemented(h.deps.Enc)
	force := func(r *http.Request) []policy.Action {
		if f, _ := queryBool(r, "force"); f {
			return []policy.Action{policy.ActionForceDeleteBucket}
		}
		return nil
	}
	return []route{
		{http.MethodGet, "/buckets", byService(policy.ActionListAllMyBuckets, policy.ActionListBucket), h.list},
		{http.MethodPost, "/buckets", byService(policy.ActionCreateBucket, policy.ActionPutBucketVersioning,
			policy.ActionPutBucketObjectLock, policy.ActionAdminSetBucketQuota), h.create},
		// The page of a bucket opens for a requester that may list part of
		// it (a home directory): the listing itself checks s3:prefix and
		// the service shows such a requester only the name, creation date
		// and region of the bucket.
		{http.MethodGet, "/buckets/{bucket}", onBucket(policy.ActionListBucket).inPart(), h.get},
		{http.MethodDelete, "/buckets/{bucket}", onBucket(policy.ActionDeleteBucket).plus(force, policy.ActionForceDeleteBucket), h.remove},

		{http.MethodGet, "/buckets/{bucket}/versioning", onBucket(policy.ActionGetBucketVersioning), h.getVersioning},
		{http.MethodPut, "/buckets/{bucket}/versioning", onBucket(policy.ActionPutBucketVersioning), h.putVersioning},
		{http.MethodGet, "/buckets/{bucket}/policy", onBucket(policy.ActionGetBucketPolicy), h.getPolicy},
		{http.MethodPut, "/buckets/{bucket}/policy", onBucket(policy.ActionPutBucketPolicy), h.putPolicy},
		{http.MethodDelete, "/buckets/{bucket}/policy", onBucket(policy.ActionDeleteBucketPolicy), h.deletePolicy},
		{http.MethodGet, "/buckets/{bucket}/quota", onBucket(policy.ActionAdminGetBucketQuota), h.getQuota},
		{http.MethodPut, "/buckets/{bucket}/quota", onBucket(policy.ActionAdminSetBucketQuota), h.putQuota},
		{http.MethodGet, "/buckets/{bucket}/tags", onBucket(policy.ActionGetBucketTagging), h.getTags},
		{http.MethodPut, "/buckets/{bucket}/tags", onBucket(policy.ActionPutBucketTagging), h.putTags},
		{http.MethodGet, "/buckets/{bucket}/objectlock", onBucket(policy.ActionGetBucketObjectLock), h.getObjectLock},
		{http.MethodPut, "/buckets/{bucket}/objectlock", onBucket(policy.ActionPutBucketObjectLock), h.putObjectLock},
		{http.MethodGet, "/buckets/{bucket}/encryption", onBucket(policy.ActionGetEncryption), h.getEncryption},
		{http.MethodPut, "/buckets/{bucket}/encryption", onBucket(policy.ActionPutEncryption), h.putEncryption},
		{http.MethodDelete, "/buckets/{bucket}/encryption", onBucket(policy.ActionPutEncryption), h.deleteEncryption},
		{http.MethodGet, "/buckets/{bucket}/access", onBucket(policy.ActionGetBucketPolicy), h.getAccess},
		{http.MethodPut, "/buckets/{bucket}/access", onBucket(policy.ActionPutBucketPolicy), h.putAccess},
		{http.MethodDelete, "/buckets/{bucket}/access", onBucket(policy.ActionPutBucketPolicy), h.deleteAccess},

		// The lifecycle, notification and CORS configurations have their
		// own route groups (LifecycleRoutes, NotificationRoutes,
		// CORSRoutes); bucket replication is P2.
		{http.MethodGet, "/buckets/{bucket}/replication", onBucket(policy.ActionGetReplication), ni},
		{http.MethodPut, "/buckets/{bucket}/replication", onBucket(policy.ActionPutReplication), ni},
	}
}

// BucketResponse is a bucket with its settings and usage. It mirrors the
// Bucket DTO of the web console.
type BucketResponse struct {
	Name              string            `json:"name"`
	Created           time.Time         `json:"created"`
	Region            string            `json:"region"`
	Versioning        string            `json:"versioning"`
	ObjectLockEnabled bool              `json:"objectLockEnabled"`
	Objects           uint64            `json:"objects"`
	Versions          uint64            `json:"versions"`
	DeleteMarkers     uint64            `json:"deleteMarkers"`
	Size              uint64            `json:"size"`
	QuotaBytes        uint64            `json:"quotaBytes"`
	Public            bool              `json:"public"`
	Tags              map[string]string `json:"tags"`
	// Partial reports a summary restricted to the name, creation date and
	// region: the caller may list only a part of the bucket.
	Partial bool `json:"partial,omitempty"`
}

func newBucketResponse(b *adminsvc.BucketSummary) BucketResponse {
	return BucketResponse{
		Name:              b.Info.Name,
		Created:           b.Info.Created,
		Region:            b.Info.Region,
		Versioning:        b.Info.Versioning.State(),
		ObjectLockEnabled: b.Info.ObjectLockEnabled,
		Objects:           b.Info.Objects,
		Versions:          b.Info.Versions,
		DeleteMarkers:     b.Info.DeleteMarkers,
		Size:              b.Info.Size,
		QuotaBytes:        b.Info.QuotaBytes,
		Public:            b.Public,
		Tags:              nonNilMap(b.Info.Tags),
		Partial:           b.Partial,
	}
}

// BucketListResponse is a page of buckets.
type BucketListResponse struct {
	Items      []BucketResponse `json:"items"`
	NextCursor string           `json:"nextCursor,omitempty"`
}

// CreateBucketRequest is the JSON body of POST /buckets.
type CreateBucketRequest struct {
	Name   string `json:"name"`
	Region string `json:"region,omitempty"`
	// Versioning enables versioning (implied by ObjectLock).
	Versioning bool `json:"versioning,omitempty"`
	ObjectLock bool `json:"objectLock,omitempty"`
	// QuotaBytes sets a hard quota in bytes (0: none).
	QuotaBytes uint64 `json:"quotaBytes,omitempty"`
}

// VersioningResponse is the body of the versioning endpoints.
type VersioningResponse struct {
	// Status is "off", "enabled" or "suspended".
	Status string `json:"status"`
}

// QuotaResponse is the body of the quota endpoints; a zero Size disables
// the quota.
type QuotaResponse struct {
	Type string `json:"type"`
	Size uint64 `json:"size"`
}

// TagsResponse is the body of the tags endpoints.
type TagsResponse struct {
	Tags map[string]string `json:"tags"`
}

// ObjectLockResponse is the body of the object lock endpoints.
type ObjectLockResponse struct {
	Enabled bool   `json:"enabled"`
	Mode    string `json:"mode,omitempty"`
	Days    int    `json:"days,omitempty"`
	Years   int    `json:"years,omitempty"`
}

// EncryptionResponse is the body of the encryption endpoints.
type EncryptionResponse struct {
	Algorithm string `json:"algorithm"`
	KMSKeyID  string `json:"kmsKeyId,omitempty"`
}

// AccessRuleResponse is the anonymous access of one prefix.
type AccessRuleResponse struct {
	// Prefix is "" for the whole bucket.
	Prefix string `json:"prefix"`
	// Preset is private, download, upload, public or custom.
	Preset string `json:"preset"`
}

// BucketAccessResponse is the body of the access endpoints.
type BucketAccessResponse struct {
	Bucket    string               `json:"bucket"`
	Rules     []AccessRuleResponse `json:"rules"`
	Public    bool                 `json:"public"`
	HasPolicy bool                 `json:"hasPolicy"`
}

// SetAccessRequest is the JSON body of PUT /buckets/{bucket}/access.
type SetAccessRequest struct {
	Prefix string `json:"prefix"`
	Preset string `json:"preset"`
}

func newBucketAccessResponse(a *adminsvc.BucketAccess) BucketAccessResponse {
	out := BucketAccessResponse{Bucket: a.Bucket, Rules: make([]AccessRuleResponse, 0, len(a.Rules)), Public: a.Public, HasPolicy: a.HasPolicy}
	for _, rule := range a.Rules {
		out.Rules = append(out.Rules, AccessRuleResponse{Prefix: rule.Prefix, Preset: string(rule.Preset)})
	}
	return out
}

func bucketName(r *http.Request) string { return chi.URLParam(r, "bucket") }

func (h *bucketRoutes) fail(w http.ResponseWriter, r *http.Request, err error) {
	writeError(h.deps.Enc, w, r, err)
}

func (h *bucketRoutes) list(w http.ResponseWriter, r *http.Request) {
	p, err := readPage(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	buckets, err := h.deps.Manager.List(r.Context(), adminsvc.ListBucketsInput{Requester: requesterOf(r), Prefix: r.URL.Query().Get("prefix"), Page: p})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := BucketListResponse{Items: make([]BucketResponse, 0, len(buckets.Items)), NextCursor: buckets.NextCursor}
	for i := range buckets.Items {
		out.Items = append(out.Items, newBucketResponse(&buckets.Items[i]))
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, out)
}

func (h *bucketRoutes) create(w http.ResponseWriter, r *http.Request) {
	var in CreateBucketRequest
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	b, err := h.deps.Manager.Create(r.Context(), adminsvc.CreateBucketInput{
		Requester: requesterOf(r), Name: in.Name, Region: in.Region, Versioning: in.Versioning, ObjectLock: in.ObjectLock, QuotaBytes: in.QuotaBytes,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusCreated, newBucketResponse(b))
}

func (h *bucketRoutes) get(w http.ResponseWriter, r *http.Request) {
	b, err := h.deps.Manager.Get(r.Context(), requesterOf(r), bucketName(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newBucketResponse(b))
}

func (h *bucketRoutes) remove(w http.ResponseWriter, r *http.Request) {
	force, err := queryBool(r, "force")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.deps.Manager.Delete(r.Context(), adminsvc.DeleteBucketInput{Name: bucketName(r), Force: force}); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *bucketRoutes) getVersioning(w http.ResponseWriter, r *http.Request) {
	v, err := h.deps.Buckets.GetVersioning(r.Context(), bucketName(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, VersioningResponse{Status: v.State()})
}

func (h *bucketRoutes) putVersioning(w http.ResponseWriter, r *http.Request) {
	var in VersioningResponse
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	v, err := domain.ParseVersioningState(in.Status)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.deps.Buckets.SetVersioning(r.Context(), bucketName(r), v); err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, VersioningResponse{Status: v.State()})
}

func (h *bucketRoutes) getPolicy(w http.ResponseWriter, r *http.Request) {
	raw, err := h.deps.Buckets.GetPolicy(r.Context(), bucketName(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, raw)
}

// putPolicy stores the body as the bucket policy; the raw text is passed
// on so that errors are located by line and column.
func (h *bucketRoutes) putPolicy(w http.ResponseWriter, r *http.Request) {
	raw, err := readBody(r, maxPolicyBody)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.deps.Buckets.SetPolicy(r.Context(), bucketName(r), raw); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *bucketRoutes) deletePolicy(w http.ResponseWriter, r *http.Request) {
	if err := h.deps.Buckets.DeletePolicy(r.Context(), bucketName(r)); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *bucketRoutes) getQuota(w http.ResponseWriter, r *http.Request) {
	q, err := h.deps.Buckets.GetQuota(r.Context(), bucketName(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, QuotaResponse{Type: string(q.Type), Size: q.Size})
}

func (h *bucketRoutes) putQuota(w http.ResponseWriter, r *http.Request) {
	var in QuotaResponse
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	q, err := h.deps.Buckets.SetQuota(r.Context(), bucketName(r), bucket.Quota{Type: bucket.QuotaType(in.Type), Size: in.Size})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, QuotaResponse{Type: string(q.Type), Size: q.Size})
}

// getTags answers the tags of the bucket (an empty set when it has none,
// see adminsvc.BucketManager.Tags).
func (h *bucketRoutes) getTags(w http.ResponseWriter, r *http.Request) {
	tags, err := h.deps.Manager.Tags(r.Context(), bucketName(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, TagsResponse{Tags: nonNilMap(tags)})
}

// putTags replaces the tags of the bucket; an empty set removes them.
func (h *bucketRoutes) putTags(w http.ResponseWriter, r *http.Request) {
	var in TagsResponse
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.deps.Buckets.SetTags(r.Context(), bucketName(r), in.Tags); err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, TagsResponse{Tags: nonNilMap(in.Tags)})
}

// getObjectLock answers the object lock configuration ({"enabled":
// false} when the bucket has none, see adminsvc.BucketManager.ObjectLock).
func (h *bucketRoutes) getObjectLock(w http.ResponseWriter, r *http.Request) {
	cfg, err := h.deps.Manager.ObjectLock(r.Context(), bucketName(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, ObjectLockResponse{Enabled: cfg.Enabled, Mode: string(cfg.Mode), Days: cfg.Days, Years: cfg.Years})
}

func (h *bucketRoutes) putObjectLock(w http.ResponseWriter, r *http.Request) {
	var in ObjectLockResponse
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	cfg := bucket.ObjectLockConfig{Enabled: in.Enabled, Mode: bucket.RetentionMode(in.Mode), Days: in.Days, Years: in.Years}
	if err := h.deps.Buckets.SetObjectLock(r.Context(), bucketName(r), cfg); err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, in)
}

func (h *bucketRoutes) getEncryption(w http.ResponseWriter, r *http.Request) {
	cfg, err := h.deps.Buckets.GetEncryption(r.Context(), bucketName(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, EncryptionResponse{Algorithm: cfg.Algorithm, KMSKeyID: cfg.KMSKeyID})
}

func (h *bucketRoutes) putEncryption(w http.ResponseWriter, r *http.Request) {
	var in EncryptionResponse
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.deps.Buckets.SetEncryption(r.Context(), bucketName(r), bucket.EncryptionConfig{Algorithm: in.Algorithm, KMSKeyID: in.KMSKeyID}); err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, in)
}

func (h *bucketRoutes) deleteEncryption(w http.ResponseWriter, r *http.Request) {
	if err := h.deps.Buckets.DeleteEncryption(r.Context(), bucketName(r)); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *bucketRoutes) getAccess(w http.ResponseWriter, r *http.Request) {
	a, err := h.deps.Manager.Access(r.Context(), bucketName(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newBucketAccessResponse(a))
}

func (h *bucketRoutes) putAccess(w http.ResponseWriter, r *http.Request) {
	var in SetAccessRequest
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	h.setAccess(w, r, in.Prefix, policy.Preset(in.Preset))
}

// deleteAccess removes the anonymous access of ?prefix= (the whole bucket
// when absent).
func (h *bucketRoutes) deleteAccess(w http.ResponseWriter, r *http.Request) {
	h.setAccess(w, r, r.URL.Query().Get("prefix"), policy.PresetPrivate)
}

func (h *bucketRoutes) setAccess(w http.ResponseWriter, r *http.Request, prefix string, preset policy.Preset) {
	a, err := h.deps.Manager.SetAccess(r.Context(), adminsvc.SetAccessInput{Bucket: bucketName(r), Prefix: prefix, Preset: preset})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newBucketAccessResponse(a))
}
