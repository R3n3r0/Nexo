package admin

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	adminsvc "github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// upload stores the raw body as the object ?key= in one request: the
// upload manager streams it within its limit.
func (h *objectRoutes) upload(w http.ResponseWriter, r *http.Request) {
	// The tags are the ones the guard authorized (objectRequestOf).
	tags, err := adminsvc.ParseTagging(r.Header.Get(adminsvc.HeaderTagging))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	// The stored type is the Content-Type of the request: the browser
	// sends the type of the file (the service defaults an empty one).
	info, err := h.deps.Uploads.Upload(r.Context(), object.PutObjectInput{
		Bucket:             bucketName(r),
		Key:                r.URL.Query().Get("key"),
		Reader:             r.Body,
		Size:               r.ContentLength,
		ContentType:        r.Header.Get("Content-Type"),
		ContentDisposition: r.Header.Get("Content-Disposition"),
		UserMetadata:       httpx.UserMetadata(r.Header),
		Tags:               tags,
		Conditions:         domain.Conditions{IfNoneMatch: r.Header.Get("If-None-Match"), IfMatch: r.Header.Get("If-Match")},
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusCreated, newObjectResponse(info))
}

// CreateUploadRequest is the optional JSON body of POST
// /buckets/{bucket}/uploads.
type CreateUploadRequest struct {
	// ContentType is the type of the object (default
	// application/octet-stream, adminsvc.DefaultContentType).
	ContentType  string            `json:"contentType,omitempty"`
	UserMetadata map[string]string `json:"userMetadata,omitempty"`
}

// UploadResponse is a multipart upload proxied by the console, with the
// limits its parts must respect.
type UploadResponse struct {
	Bucket    string    `json:"bucket"`
	Key       string    `json:"key"`
	UploadID  string    `json:"uploadId"`
	Initiated time.Time `json:"initiated"`
	// PartSize is the recommended part size.
	PartSize int64 `json:"partSize"`
	// MinPartSize applies to every part but the last.
	MinPartSize int64 `json:"minPartSize"`
	MaxPartSize int64 `json:"maxPartSize"`
	MaxParts    int   `json:"maxParts"`
}

func (h *objectRoutes) createUpload(w http.ResponseWriter, r *http.Request) {
	var in CreateUploadRequest
	if err := decodeJSON(h.deps.Enc, r, &in, true); err != nil {
		h.fail(w, r, err)
		return
	}
	// The tags are the ones the guard authorized (objectRequestOf).
	tags, err := adminsvc.ParseTagging(r.Header.Get(adminsvc.HeaderTagging))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	up, err := h.deps.Uploads.Create(r.Context(), object.NewMultipartInput{
		Bucket: bucketName(r), Key: r.URL.Query().Get("key"), ContentType: in.ContentType, UserMetadata: in.UserMetadata, Tags: tags,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	l := up.Limits
	h.deps.Enc.WriteJSON(w, http.StatusCreated, UploadResponse{
		Bucket: up.Upload.Bucket, Key: up.Upload.Key, UploadID: up.Upload.UploadID, Initiated: up.Upload.Initiated.UTC(),
		PartSize: l.PartSize, MinPartSize: l.MinPartSize, MaxPartSize: l.MaxPartSize, MaxParts: l.MaxParts,
	})
}

// PendingUploadResponse is a pending multipart upload.
type PendingUploadResponse struct {
	Key       string    `json:"key"`
	UploadID  string    `json:"uploadId"`
	Initiated time.Time `json:"initiated"`
}

// UploadListResponse is a page of pending multipart uploads.
type UploadListResponse struct {
	Items      []PendingUploadResponse `json:"items"`
	NextCursor string                  `json:"nextCursor,omitempty"`
}

func (h *objectRoutes) listUploads(w http.ResponseWriter, r *http.Request) {
	p, err := readPage(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	res, err := h.deps.Uploads.List(r.Context(), adminsvc.ListUploadsInput{Bucket: bucketName(r), Prefix: r.URL.Query().Get("prefix"), Page: p})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := UploadListResponse{Items: make([]PendingUploadResponse, 0, len(res.Items)), NextCursor: res.NextCursor}
	for _, u := range res.Items {
		out.Items = append(out.Items, PendingUploadResponse{Key: u.Key, UploadID: u.UploadID, Initiated: u.Initiated.UTC()})
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, out)
}

// PartResponse is an uploaded part.
type PartResponse struct {
	PartNumber   int        `json:"partNumber"`
	ETag         string     `json:"etag"`
	Size         int64      `json:"size"`
	LastModified *time.Time `json:"lastModified,omitempty"`
}

func newPartResponse(p domain.Part) PartResponse {
	return PartResponse{PartNumber: p.Number, ETag: p.ETag, Size: p.Size, LastModified: optTime(p.ModTime)}
}

// PartListResponse is a page of the parts of an upload.
type PartListResponse struct {
	Items      []PartResponse `json:"items"`
	NextCursor string         `json:"nextCursor,omitempty"`
}

// uploadOf names the upload of the path ({uploadId}) and of ?key=.
func uploadOf(r *http.Request) (adminsvc.UploadRef, error) {
	id, err := pathParam(r, "uploadId")
	if err != nil {
		return adminsvc.UploadRef{}, err
	}
	return adminsvc.UploadRef{Bucket: bucketName(r), Key: r.URL.Query().Get("key"), UploadID: id}, nil
}

func (h *objectRoutes) listParts(w http.ResponseWriter, r *http.Request) {
	up, err := uploadOf(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	p, err := readPage(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	res, err := h.deps.Uploads.Parts(r.Context(), adminsvc.ListPartsInput{Upload: up, Page: p})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := PartListResponse{Items: make([]PartResponse, 0, len(res.Items)), NextCursor: res.NextCursor}
	for _, p := range res.Items {
		out.Items = append(out.Items, newPartResponse(p))
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, out)
}

// uploadPart stores the raw body as one part: the upload manager streams
// it within its limit.
func (h *objectRoutes) uploadPart(w http.ResponseWriter, r *http.Request) {
	up, err := uploadOf(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	raw, err := pathParam(r, "partNumber")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		h.fail(w, r, fmt.Errorf("%w: the part number must be an integer", domain.ErrInvalidArgument))
		return
	}
	part, err := h.deps.Uploads.UploadPart(r.Context(), object.PutObjectPartInput{
		Bucket: up.Bucket, Key: up.Key, UploadID: up.UploadID, PartNumber: n, Reader: r.Body, Size: r.ContentLength,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newPartResponse(part))
}

// CompletePartRequest is a part of CompleteUploadRequest.
type CompletePartRequest struct {
	PartNumber int    `json:"partNumber"`
	ETag       string `json:"etag"`
}

// CompleteUploadRequest is the JSON body of POST
// /buckets/{bucket}/uploads/{uploadId}/complete.
type CompleteUploadRequest struct {
	Parts []CompletePartRequest `json:"parts"`
}

func (h *objectRoutes) completeUpload(w http.ResponseWriter, r *http.Request) {
	up, err := uploadOf(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var in CompleteUploadRequest
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	parts := make([]domain.CompletePart, 0, len(in.Parts))
	for _, p := range in.Parts {
		parts = append(parts, domain.CompletePart{Number: p.PartNumber, ETag: p.ETag})
	}
	info, err := h.deps.Uploads.Complete(r.Context(), object.CompleteMultipartInput{Bucket: up.Bucket, Key: up.Key, UploadID: up.UploadID, Parts: parts})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newObjectResponse(info))
}

func (h *objectRoutes) abortUpload(w http.ResponseWriter, r *http.Request) {
	up, err := uploadOf(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if err := h.deps.Uploads.Abort(r.Context(), up); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
