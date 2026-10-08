package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// BucketDeps are the dependencies of the admin bucket routes.
type BucketDeps struct {
	Buckets bucket.Service
	// Objects serves the object browser listing.
	Objects object.Service
	// Policy authorizes the admin:* actions; nil leaves the routes open (tests).
	Policy policy.Service
	Enc    httpx.Codec
}

type bucketRoutes struct {
	deps  BucketDeps
	authz authorizer
}

var _ BucketRoutes = (*bucketRoutes)(nil)

// NewBucketRoutes returns the admin bucket routes.
func NewBucketRoutes(deps BucketDeps) (BucketRoutes, error) {
	if deps.Buckets == nil || deps.Objects == nil {
		return nil, fmt.Errorf("%w: bucket routes require the bucket and object services", domain.ErrInvalidArgument)
	}
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	return &bucketRoutes{deps: deps, authz: authorizer{policy: deps.Policy, enc: deps.Enc}}, nil
}

// Register implements Routes.
func (h *bucketRoutes) Register(r chi.Router) {
	r.Get("/buckets", h.list)
	r.Post("/buckets", h.create)
	r.Get("/buckets/{bucket}", h.get)
	r.Delete("/buckets/{bucket}", h.remove)

	r.Get("/buckets/{bucket}/versioning", h.getVersioning)
	r.Put("/buckets/{bucket}/versioning", h.putVersioning)
	r.Get("/buckets/{bucket}/policy", h.getPolicy)
	r.Put("/buckets/{bucket}/policy", h.putPolicy)
	r.Delete("/buckets/{bucket}/policy", h.deletePolicy)
	r.Get("/buckets/{bucket}/quota", h.getQuota)
	r.Put("/buckets/{bucket}/quota", h.putQuota)
	r.Get("/buckets/{bucket}/tags", h.getTags)
	r.Put("/buckets/{bucket}/tags", h.putTags)
	r.Get("/buckets/{bucket}/objectlock", h.getObjectLock)
	r.Put("/buckets/{bucket}/objectlock", h.putObjectLock)
	r.Get("/buckets/{bucket}/encryption", h.getEncryption)
	r.Put("/buckets/{bucket}/encryption", h.putEncryption)
	r.Delete("/buckets/{bucket}/encryption", h.deleteEncryption)

	// Lifecycle, notification and replication documents are parsed by the
	// ilm/event services of M7; until then the console cannot edit them.
	ni := notImplemented(h.deps.Enc)
	for _, s := range []string{"lifecycle", "events", "replication"} {
		r.Get("/buckets/{bucket}/"+s, ni)
		r.Put("/buckets/{bucket}/"+s, ni)
	}

	r.Get("/buckets/{bucket}/objects", h.listObjects)
	// Object transfers from the console belong to the object browser (M4).
	r.Post("/buckets/{bucket}/objects/upload", ni)
	r.Get("/buckets/{bucket}/objects/download", ni)
	r.Post("/buckets/{bucket}/objects/download-zip", ni)
	r.Delete("/buckets/{bucket}/objects", ni)
	r.Post("/buckets/{bucket}/objects/share", ni)
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
	Size              uint64            `json:"size"`
	QuotaBytes        uint64            `json:"quotaBytes"`
	Tags              map[string]string `json:"tags"`
}

func newBucketResponse(b *domain.BucketInfo) BucketResponse {
	return BucketResponse{
		Name:              b.Name,
		Created:           b.Created,
		Region:            b.Region,
		Versioning:        versioningState(b.Versioning),
		ObjectLockEnabled: b.ObjectLockEnabled,
		Objects:           b.Objects,
		Versions:          b.Versions,
		Size:              b.Size,
		QuotaBytes:        b.QuotaBytes,
		Tags:              nonNilMap(b.Tags),
	}
}

// versioningState renders a versioning state as the console expects it
// ("off", "enabled", "suspended").
func versioningState(v domain.Versioning) string {
	if s := v.String(); s != "" {
		return strings.ToLower(s)
	}
	return "off"
}

// CreateBucketRequest is the JSON body of POST /buckets.
type CreateBucketRequest struct {
	Name       string `json:"name"`
	Region     string `json:"region,omitempty"`
	ObjectLock bool   `json:"objectLock,omitempty"`
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

// ObjectListResponse is the body of GET /buckets/{bucket}/objects.
type ObjectListResponse struct {
	Objects     []ObjectResponse `json:"objects"`
	Prefixes    []string         `json:"prefixes"`
	IsTruncated bool             `json:"isTruncated"`
	// NextCursor resumes a truncated listing (query parameter "cursor").
	NextCursor string `json:"nextCursor,omitempty"`
}

// ObjectResponse is one object of the object browser listing.
type ObjectResponse struct {
	Key            string    `json:"key"`
	VersionID      string    `json:"versionId,omitempty"`
	Size           int64     `json:"size"`
	ETag           string    `json:"etag"`
	LastModified   time.Time `json:"lastModified"`
	ContentType    string    `json:"contentType,omitempty"`
	IsDeleteMarker bool      `json:"isDeleteMarker"`
	IsLatest       bool      `json:"isLatest"`
}

func bucketName(r *http.Request) string { return chi.URLParam(r, "bucket") }

func (h *bucketRoutes) list(w http.ResponseWriter, r *http.Request) {
	if !h.authz.authorize(w, r, policy.ActionAdminListBuckets, "") {
		return
	}
	infos, err := h.deps.Buckets.List(r.Context(), bucket.ListInput{Prefix: r.URL.Query().Get("prefix"), WithUsage: true})
	if err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	out := make([]BucketResponse, 0, len(infos))
	for i := range infos {
		out = append(out, newBucketResponse(&infos[i]))
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, out)
}

func (h *bucketRoutes) create(w http.ResponseWriter, r *http.Request) {
	var in CreateBucketRequest
	if err := h.deps.Enc.DecodeJSON(r.Body, maxJSONBody, &in); err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	if !h.authz.authorize(w, r, policy.ActionAdminCreateBucket, in.Name) {
		return
	}
	ctx := r.Context()
	if _, err := h.deps.Buckets.Create(ctx, bucket.CreateInput{Name: in.Name, Region: in.Region, ObjectLock: in.ObjectLock}); err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	info, err := h.deps.Buckets.Get(ctx, in.Name)
	if err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusCreated, newBucketResponse(info))
}

func (h *bucketRoutes) get(w http.ResponseWriter, r *http.Request) {
	name := bucketName(r)
	if !h.authz.authorize(w, r, policy.ActionAdminGetBucket, name) {
		return
	}
	info, err := h.deps.Buckets.Get(r.Context(), name)
	if err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newBucketResponse(info))
}

func (h *bucketRoutes) remove(w http.ResponseWriter, r *http.Request) {
	name := bucketName(r)
	if !h.authz.authorize(w, r, policy.ActionAdminDeleteBucket, name) {
		return
	}
	if err := h.deps.Buckets.Delete(r.Context(), name, bucket.DeleteOptions{Force: queryBool(r, "force")}); err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *bucketRoutes) getVersioning(w http.ResponseWriter, r *http.Request) {
	name := bucketName(r)
	if !h.authz.authorize(w, r, policy.ActionAdminGetBucket, name) {
		return
	}
	v, err := h.deps.Buckets.GetVersioning(r.Context(), name)
	if err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, VersioningResponse{Status: versioningState(v)})
}

func (h *bucketRoutes) putVersioning(w http.ResponseWriter, r *http.Request) {
	name := bucketName(r)
	if !h.authz.authorize(w, r, policy.ActionAdminUpdateBucket, name) {
		return
	}
	var in VersioningResponse
	if err := h.deps.Enc.DecodeJSON(r.Body, maxJSONBody, &in); err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	status := in.Status
	if strings.EqualFold(status, "off") {
		status = ""
	}
	v, err := domain.ParseVersioning(status)
	if err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	if err := h.deps.Buckets.SetVersioning(r.Context(), name, v); err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, VersioningResponse{Status: versioningState(v)})
}

func (h *bucketRoutes) getPolicy(w http.ResponseWriter, r *http.Request) {
	name := bucketName(r)
	if !h.authz.authorize(w, r, policy.ActionAdminGetBucket, name) {
		return
	}
	raw, err := h.deps.Buckets.GetPolicy(r.Context(), name)
	if err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, raw)
}

func (h *bucketRoutes) putPolicy(w http.ResponseWriter, r *http.Request) {
	name := bucketName(r)
	if !h.authz.authorize(w, r, policy.ActionAdminUpdateBucket, name) {
		return
	}
	var raw json.RawMessage
	if err := h.deps.Enc.DecodeJSON(r.Body, maxJSONBody, &raw); err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	if err := h.deps.Buckets.SetPolicy(r.Context(), name, raw); err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *bucketRoutes) deletePolicy(w http.ResponseWriter, r *http.Request) {
	name := bucketName(r)
	if !h.authz.authorize(w, r, policy.ActionAdminUpdateBucket, name) {
		return
	}
	if err := h.deps.Buckets.DeletePolicy(r.Context(), name); err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *bucketRoutes) getQuota(w http.ResponseWriter, r *http.Request) {
	name := bucketName(r)
	if !h.authz.authorize(w, r, policy.ActionAdminGetBucket, name) {
		return
	}
	q, err := h.deps.Buckets.GetQuota(r.Context(), name)
	if err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, QuotaResponse{Type: string(q.Type), Size: q.Size})
}

func (h *bucketRoutes) putQuota(w http.ResponseWriter, r *http.Request) {
	name := bucketName(r)
	if !h.authz.authorize(w, r, policy.ActionAdminUpdateBucket, name) {
		return
	}
	var in QuotaResponse
	if err := h.deps.Enc.DecodeJSON(r.Body, maxJSONBody, &in); err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	q := bucket.Quota{Type: bucket.QuotaType(in.Type), Size: in.Size}
	if q.Size > 0 && q.Type == "" {
		q.Type = bucket.QuotaHard
	}
	if err := h.deps.Buckets.SetQuota(r.Context(), name, q); err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, QuotaResponse{Type: string(q.Type), Size: q.Size})
}

func (h *bucketRoutes) getTags(w http.ResponseWriter, r *http.Request) {
	name := bucketName(r)
	if !h.authz.authorize(w, r, policy.ActionAdminGetBucket, name) {
		return
	}
	tags, err := h.deps.Buckets.GetTags(r.Context(), name)
	if err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, TagsResponse{Tags: nonNilMap(tags)})
}

func (h *bucketRoutes) putTags(w http.ResponseWriter, r *http.Request) {
	name := bucketName(r)
	if !h.authz.authorize(w, r, policy.ActionAdminUpdateBucket, name) {
		return
	}
	var in TagsResponse
	if err := h.deps.Enc.DecodeJSON(r.Body, maxJSONBody, &in); err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	ctx := r.Context()
	var err error
	if len(in.Tags) == 0 {
		err = h.deps.Buckets.DeleteTags(ctx, name)
	} else {
		err = h.deps.Buckets.SetTags(ctx, name, in.Tags)
	}
	if err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, TagsResponse{Tags: nonNilMap(in.Tags)})
}

func (h *bucketRoutes) getObjectLock(w http.ResponseWriter, r *http.Request) {
	name := bucketName(r)
	if !h.authz.authorize(w, r, policy.ActionAdminGetBucket, name) {
		return
	}
	cfg, err := h.deps.Buckets.GetObjectLock(r.Context(), name)
	if err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, ObjectLockResponse{Enabled: cfg.Enabled, Mode: string(cfg.Mode), Days: cfg.Days, Years: cfg.Years})
}

func (h *bucketRoutes) putObjectLock(w http.ResponseWriter, r *http.Request) {
	name := bucketName(r)
	if !h.authz.authorize(w, r, policy.ActionAdminUpdateBucket, name) {
		return
	}
	var in ObjectLockResponse
	if err := h.deps.Enc.DecodeJSON(r.Body, maxJSONBody, &in); err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	cfg := bucket.ObjectLockConfig{Enabled: in.Enabled, Mode: bucket.RetentionMode(in.Mode), Days: in.Days, Years: in.Years}
	if err := h.deps.Buckets.SetObjectLock(r.Context(), name, cfg); err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, in)
}

func (h *bucketRoutes) getEncryption(w http.ResponseWriter, r *http.Request) {
	name := bucketName(r)
	if !h.authz.authorize(w, r, policy.ActionAdminGetBucket, name) {
		return
	}
	cfg, err := h.deps.Buckets.GetEncryption(r.Context(), name)
	if err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, EncryptionResponse{Algorithm: cfg.Algorithm, KMSKeyID: cfg.KMSKeyID})
}

func (h *bucketRoutes) putEncryption(w http.ResponseWriter, r *http.Request) {
	name := bucketName(r)
	if !h.authz.authorize(w, r, policy.ActionAdminUpdateBucket, name) {
		return
	}
	var in EncryptionResponse
	if err := h.deps.Enc.DecodeJSON(r.Body, maxJSONBody, &in); err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	if err := h.deps.Buckets.SetEncryption(r.Context(), name, bucket.EncryptionConfig{Algorithm: in.Algorithm, KMSKeyID: in.KMSKeyID}); err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, in)
}

func (h *bucketRoutes) deleteEncryption(w http.ResponseWriter, r *http.Request) {
	name := bucketName(r)
	if !h.authz.authorize(w, r, policy.ActionAdminUpdateBucket, name) {
		return
	}
	if err := h.deps.Buckets.DeleteEncryption(r.Context(), name); err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listObjects serves the object browser: GET
// /buckets/{bucket}/objects?prefix=&delimiter=&cursor=&limit=. The
// console browses with the caller's S3 permissions (s3:ListBucket).
func (h *bucketRoutes) listObjects(w http.ResponseWriter, r *http.Request) {
	name := bucketName(r)
	if !h.authz.authorize(w, r, policy.ActionListBucket, name) {
		return
	}
	q := r.URL.Query()
	in := domain.ListObjectsInput{
		Bucket:            name,
		Prefix:            q.Get("prefix"),
		Delimiter:         q.Get("delimiter"),
		ContinuationToken: q.Get("cursor"),
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			badRequest(h.deps.Enc, w, r, "limit must be a positive integer")
			return
		}
		in.MaxKeys = n
	}
	res, err := h.deps.Objects.ListObjects(r.Context(), in)
	if err != nil {
		h.deps.Enc.WriteProblemError(w, r, err)
		return
	}
	out := ObjectListResponse{
		Objects:     make([]ObjectResponse, 0, len(res.Objects)),
		Prefixes:    nonNil(res.CommonPrefixes),
		IsTruncated: res.IsTruncated,
		NextCursor:  res.NextContinuationToken,
	}
	for _, o := range res.Objects {
		out.Objects = append(out.Objects, ObjectResponse{
			Key:            o.Key,
			VersionID:      o.VersionID,
			Size:           o.Size,
			ETag:           o.ETag,
			LastModified:   o.ModTime,
			ContentType:    o.ContentType,
			IsDeleteMarker: o.IsDeleteMarker,
			IsLatest:       o.IsLatest,
		})
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, out)
}
