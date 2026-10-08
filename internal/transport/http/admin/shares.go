package admin

import (
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	adminsvc "github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/service/share"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// maxExpiresIn is the longest validity, in seconds, of a share link, a
// presigned URL and a POST policy (SigV4: 7 days).
const maxExpiresIn = int64(share.MaxExpiry / time.Second)

// expiresIn decodes a validity in seconds (0 means the default of the
// operation). The range is checked before the conversion, which would
// overflow for huge values and silently yield another validity.
func expiresIn(seconds int64) (time.Duration, error) {
	if seconds < 0 || seconds > maxExpiresIn {
		return 0, fmt.Errorf("%w: expiresIn must be between 1 and %d seconds", domain.ErrInvalidArgument, maxExpiresIn)
	}
	return time.Duration(seconds) * time.Second, nil
}

// ShareDeps are the dependencies of the share routes.
type ShareDeps struct {
	// Shares manages share links, presigned URLs and POST policies
	// (required).
	Shares adminsvc.ShareManager
	// Authz authorizes the routes (required).
	Authz adminsvc.Authorizer
	Enc   httpx.Codec
}

// ShareOption configures the share routes (none yet: the type keeps the
// constructor shape of every route group, docs/12 §12.3).
type ShareOption func(*shareRoutes)

type shareRoutes struct {
	deps  ShareDeps
	guard guard
}

var _ ShareRoutes = (*shareRoutes)(nil)

// NewShareRoutes returns the share link, presign and POST policy routes.
func NewShareRoutes(deps ShareDeps, opts ...ShareOption) (ShareRoutes, error) {
	if deps.Shares == nil || deps.Authz == nil {
		return nil, fmt.Errorf("%w: share routes require the share manager and the authorizer", domain.ErrInvalidArgument)
	}
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	h := &shareRoutes{deps: deps, guard: guard{authz: deps.Authz, enc: deps.Enc}}
	for _, o := range opts {
		o(h)
	}
	return h, nil
}

// Register implements Routes.
func (h *shareRoutes) Register(r chi.Router) { h.guard.register(r, h.routes()) }

func (h *shareRoutes) routes() []route {
	// Every share route is authorized by the share manager: the owner of
	// a link always manages it, admin:ManageShares manages those of others.
	shared := byService(policy.ActionGetObject, policy.ActionPutObject)
	manage := byService(policy.ActionAdminManageShares)
	return []route{
		{http.MethodPost, "/shares", shared, h.create},
		{http.MethodGet, "/shares", manage, h.list},
		{http.MethodDelete, "/shares", manage, h.revokeAll},
		{http.MethodGet, "/shares/{id}", manage, h.get},
		{http.MethodGet, "/shares/{id}/links", byService(policy.ActionAdminManageShares, policy.ActionListBucket), h.links},
		{http.MethodDelete, "/shares/{id}", manage, h.revoke},
		{http.MethodPost, "/buckets/{bucket}/objects/share", shared, h.createInBucket},
		{http.MethodPost, "/presign", byService(policy.ActionGetObject, policy.ActionPutObject, policy.ActionDeleteObject), h.presign},
		{http.MethodPost, "/post-policy", byService(policy.ActionPutObject), h.postPolicy},
	}
}

func (h *shareRoutes) fail(w http.ResponseWriter, r *http.Request, err error) {
	writeError(h.deps.Enc, w, r, err)
}

// ShareResponse is a managed share link.
type ShareResponse struct {
	ID     string `json:"id"`
	Owner  string `json:"owner"`
	Bucket string `json:"bucket"`
	// Key is the object key, or the shared prefix when Prefix is set.
	Key    string `json:"key"`
	Prefix bool   `json:"prefix"`
	// Permission is download or upload.
	Permission         string    `json:"permission"`
	VersionID          string    `json:"versionId,omitempty"`
	ContentType        string    `json:"contentType,omitempty"`
	ContentDisposition string    `json:"contentDisposition,omitempty"`
	ExpiresAt          time.Time `json:"expiresAt"`
	CreatedAt          time.Time `json:"createdAt"`
	// URL is the presigned URL: GET or PUT of the object for object
	// shares, a ListObjectsV2 of the prefix for prefix download shares,
	// empty for prefix upload shares.
	URL string `json:"url,omitempty"`
	// AccessKey is the service account signing the link.
	AccessKey string `json:"accessKey"`
}

func newShareResponse(s *share.Share) ShareResponse {
	return ShareResponse{
		ID: s.ID, Owner: s.Owner, Bucket: s.Bucket, Key: s.Key, Prefix: s.Prefix, Permission: string(s.Permission),
		VersionID: s.VersionID, ContentType: s.ContentType, ContentDisposition: s.ContentDisposition,
		ExpiresAt: s.Expires.UTC(), CreatedAt: s.CreatedAt.UTC(), URL: s.URL, AccessKey: s.AccessKey,
	}
}

// ShareListResponse is a page of share links.
type ShareListResponse struct {
	Items      []ShareResponse `json:"items"`
	NextCursor string          `json:"nextCursor,omitempty"`
}

// CreateShareRequest is the JSON body of POST /shares.
type CreateShareRequest struct {
	Bucket string `json:"bucket,omitempty"`
	// Key is the object key, or the prefix when Prefix is set.
	Key    string `json:"key"`
	Prefix bool   `json:"prefix,omitempty"`
	// Permission is download (default) or upload.
	Permission string `json:"permission,omitempty"`
	// ExpiresIn is the validity in seconds (1 to 604800; default 7 days).
	ExpiresIn          int64  `json:"expiresIn,omitempty"`
	ContentType        string `json:"contentType,omitempty"`
	ContentDisposition string `json:"contentDisposition,omitempty"`
	VersionID          string `json:"versionId,omitempty"`
}

// RevokeAllResponse is the body of DELETE /shares.
type RevokeAllResponse struct {
	Revoked int `json:"revoked"`
}

func (h *shareRoutes) create(w http.ResponseWriter, r *http.Request) { h.createShare(w, r, "") }

// createInBucket is POST /buckets/{bucket}/objects/share: POST /shares
// with the bucket of the path.
func (h *shareRoutes) createInBucket(w http.ResponseWriter, r *http.Request) {
	h.createShare(w, r, bucketName(r))
}

func (h *shareRoutes) createShare(w http.ResponseWriter, r *http.Request, bucket string) {
	var in CreateShareRequest
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	if bucket != "" {
		if in.Bucket != "" && in.Bucket != bucket {
			h.fail(w, r, fmt.Errorf("%w: the bucket of the body differs from the path", domain.ErrInvalidArgument))
			return
		}
		in.Bucket = bucket
	}
	expires, err := expiresIn(in.ExpiresIn)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	s, err := h.deps.Shares.Create(r.Context(), adminsvc.CreateShareInput{
		Requester: requesterOf(r), Bucket: in.Bucket, Key: in.Key, Prefix: in.Prefix, Permission: share.Permission(in.Permission),
		Expires: expires, ContentType: in.ContentType,
		ContentDisposition: in.ContentDisposition, VersionID: in.VersionID,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusCreated, newShareResponse(s))
}

func (h *shareRoutes) list(w http.ResponseWriter, r *http.Request) {
	p, err := readPage(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	q := r.URL.Query()
	shares, err := h.deps.Shares.List(r.Context(), adminsvc.ListSharesInput{Requester: requesterOf(r), Owner: q.Get("owner"), Bucket: q.Get("bucket"), Page: p})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := ShareListResponse{Items: make([]ShareResponse, 0, len(shares.Items)), NextCursor: shares.NextCursor}
	for i := range shares.Items {
		out.Items = append(out.Items, newShareResponse(&shares.Items[i]))
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, out)
}

func (h *shareRoutes) get(w http.ResponseWriter, r *http.Request) {
	id, err := pathParam(r, "id")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	s, err := h.deps.Shares.Get(r.Context(), requesterOf(r), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newShareResponse(s))
}

// ShareLinkResponse is the link of one object of a prefix share.
type ShareLinkResponse struct {
	Key  string `json:"key"`
	Size int64  `json:"size"`
	// URL downloads the object; it is signed by the share and revoked with
	// it.
	URL string `json:"url"`
}

// ShareLinksResponse is a page of the per-object links of a prefix
// download share.
type ShareLinksResponse struct {
	Share      ShareResponse       `json:"share"`
	Items      []ShareLinkResponse `json:"items"`
	NextCursor string              `json:"nextCursor,omitempty"`
}

// links is GET /shares/{id}/links: the objects of a prefix download share,
// one link each.
func (h *shareRoutes) links(w http.ResponseWriter, r *http.Request) {
	id, err := pathParam(r, "id")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	p, err := readPage(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	res, err := h.deps.Shares.Links(r.Context(), adminsvc.ShareLinksInput{Requester: requesterOf(r), ID: id, Page: p})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := ShareLinksResponse{Share: newShareResponse(res.Share), Items: make([]ShareLinkResponse, 0, len(res.Items)), NextCursor: res.NextCursor}
	for _, l := range res.Items {
		out.Items = append(out.Items, ShareLinkResponse(l))
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, out)
}

func (h *shareRoutes) revoke(w http.ResponseWriter, r *http.Request) {
	id, err := pathParam(r, "id")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.deps.Shares.Revoke(r.Context(), requesterOf(r), id); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// revokeAll revokes every share of ?owner= (default: the caller).
func (h *shareRoutes) revokeAll(w http.ResponseWriter, r *http.Request) {
	n, err := h.deps.Shares.RevokeAll(r.Context(), requesterOf(r), r.URL.Query().Get("owner"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, RevokeAllResponse{Revoked: n})
}

// PresignRequest is the JSON body of POST /presign.
type PresignRequest struct {
	// Method is GET (default), PUT, HEAD or DELETE.
	Method    string `json:"method,omitempty"`
	Bucket    string `json:"bucket"`
	Key       string `json:"key"`
	VersionID string `json:"versionId,omitempty"`
	// ExpiresIn is the validity in seconds (1 to 604800; default 7 days).
	ExpiresIn          int64             `json:"expiresIn,omitempty"`
	ContentType        string            `json:"contentType,omitempty"`
	ContentDisposition string            `json:"contentDisposition,omitempty"`
	Headers            map[string]string `json:"headers,omitempty"`
}

// PresignResponse is a raw presigned URL (not revocable).
type PresignResponse struct {
	URL       string    `json:"url"`
	Method    string    `json:"method"`
	ExpiresAt time.Time `json:"expiresAt"`
	// Headers are the signed headers the client must send unchanged.
	Headers map[string]string `json:"headers"`
}

func (h *shareRoutes) presign(w http.ResponseWriter, r *http.Request) {
	var in PresignRequest
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	expires, err := expiresIn(in.ExpiresIn)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	u, err := h.deps.Shares.Presign(r.Context(), adminsvc.PresignInput{
		Requester: requesterOf(r), Method: in.Method, Bucket: in.Bucket, Key: in.Key, VersionID: in.VersionID,
		Expires: expires, ContentType: in.ContentType,
		ContentDisposition: in.ContentDisposition, Headers: in.Headers,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, PresignResponse{URL: u.URL, Method: u.Method, ExpiresAt: u.Expires.UTC(), Headers: nonNilMap(u.Headers)})
}

// PostPolicyRequest is the JSON body of POST /post-policy.
type PostPolicyRequest struct {
	Bucket string `json:"bucket"`
	// Key is the exact key; with KeyPrefix the browser chooses any key
	// under it ("${filename}" is replaced by the file name).
	Key       string `json:"key,omitempty"`
	KeyPrefix string `json:"keyPrefix,omitempty"`
	// ExpiresIn is the validity in seconds (1 to 604800; default 1 hour).
	ExpiresIn int64 `json:"expiresIn,omitempty"`
	// MinSize and MaxSize bound the file size when MaxSize is positive.
	MinSize               int64             `json:"minSize,omitempty"`
	MaxSize               int64             `json:"maxSize,omitempty"`
	ContentType           string            `json:"contentType,omitempty"`
	ContentTypePrefix     string            `json:"contentTypePrefix,omitempty"`
	SuccessActionStatus   int               `json:"successActionStatus,omitempty"`
	SuccessActionRedirect string            `json:"successActionRedirect,omitempty"`
	Fields                map[string]string `json:"fields,omitempty"`
}

// PostConditionResponse is a condition of a signed POST policy.
type PostConditionResponse struct {
	// Operator is eq, starts-with or content-length-range.
	Operator string `json:"operator"`
	Key      string `json:"key,omitempty"`
	Value    string `json:"value,omitempty"`
	Min      int64  `json:"min,omitempty"`
	Max      int64  `json:"max,omitempty"`
}

// PostPolicyResponse is a signed POST policy form.
type PostPolicyResponse struct {
	// URL is where the multipart/form-data is posted.
	URL string `json:"url"`
	// Fields are sent before the "file" field.
	Fields     map[string]string       `json:"fields"`
	ExpiresAt  time.Time               `json:"expiresAt"`
	Conditions []PostConditionResponse `json:"conditions"`
}

func (h *shareRoutes) postPolicy(w http.ResponseWriter, r *http.Request) {
	var in PostPolicyRequest
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	expires, err := expiresIn(in.ExpiresIn)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	form, err := h.deps.Shares.PostPolicy(r.Context(), adminsvc.PostPolicyInput{
		Requester: requesterOf(r), Bucket: in.Bucket, Key: in.Key, KeyPrefix: in.KeyPrefix,
		Expires: expires, MinSize: in.MinSize, MaxSize: in.MaxSize,
		ContentType: in.ContentType, ContentTypePrefix: in.ContentTypePrefix,
		SuccessActionStatus: in.SuccessActionStatus, SuccessActionRedirect: in.SuccessActionRedirect, Fields: in.Fields,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := PostPolicyResponse{URL: form.URL, Fields: nonNilMap(form.Fields), ExpiresAt: form.Expiration.UTC(), Conditions: make([]PostConditionResponse, 0, len(form.Conditions))}
	for _, c := range form.Conditions {
		out.Conditions = append(out.Conditions, PostConditionResponse(c))
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, out)
}
