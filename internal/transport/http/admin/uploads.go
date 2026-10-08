package admin

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	adminsvc "github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// limitedBody streams a request body and fails, without buffering, once
// it exceeds limit bytes: the object layer then commits nothing.
type limitedBody struct {
	r        io.Reader
	left     int64
	exceeded bool
}

func newLimitedBody(r io.Reader, limit int64) *limitedBody { return &limitedBody{r: r, left: limit} }

// Read implements io.Reader.
func (l *limitedBody) Read(p []byte) (int, error) {
	if l.left <= 0 {
		var probe [1]byte
		n, err := l.r.Read(probe[:])
		if n > 0 {
			l.exceeded = true
			return 0, domain.ErrEntityTooLarge
		}
		return 0, err
	}
	if int64(len(p)) > l.left {
		p = p[:l.left]
	}
	n, err := l.r.Read(p)
	l.left -= int64(n)
	return n, err
}

// tooLarge is the error of a body over limit bytes.
func tooLarge(limit int64) error {
	return fmt.Errorf("%w: the body exceeds %d bytes", domain.ErrEntityTooLarge, limit)
}

// errorsIsNotModified reports a failed If-None-Match / If-Modified-Since.
func errorsIsNotModified(err error) bool { return errors.Is(err, domain.ErrNotModified) }

// upload stores the raw body as the object ?key= in one request.
func (h *objectRoutes) upload(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength > h.maxUpload {
		h.fail(w, r, tooLarge(h.maxUpload))
		return
	}
	// The tags are the ones the guard authorized (objectRequestOf).
	tags, err := adminsvc.ParseTagging(r.Header.Get(adminsvc.HeaderTagging))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	body := newLimitedBody(r.Body, h.maxUpload)
	// The stored type is the Content-Type of the request: the browser
	// sends the type of the file (the service defaults an empty one).
	info, err := h.deps.Browser.Upload(r.Context(), object.PutObjectInput{
		Bucket:             bucketName(r),
		Key:                r.URL.Query().Get("key"),
		Reader:             body,
		Size:               r.ContentLength,
		ContentType:        r.Header.Get("Content-Type"),
		ContentDisposition: r.Header.Get("Content-Disposition"),
		UserMetadata:       httpx.UserMetadata(r.Header),
		Tags:               tags,
		Conditions:         domain.Conditions{IfNoneMatch: r.Header.Get("If-None-Match"), IfMatch: r.Header.Get("If-Match")},
	})
	if body.exceeded {
		err = tooLarge(h.maxUpload)
	}
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
	up, err := h.deps.Browser.CreateUpload(r.Context(), object.NewMultipartInput{
		Bucket: bucketName(r), Key: r.URL.Query().Get("key"), ContentType: in.ContentType, UserMetadata: in.UserMetadata, Tags: tags,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusCreated, UploadResponse{
		Bucket: up.Bucket, Key: up.Key, UploadID: up.UploadID, Initiated: up.Initiated.UTC(),
		PartSize: min(recommendedPartSize, h.maxPart), MinPartSize: minPartSize, MaxPartSize: h.maxPart, MaxParts: maxParts,
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

// uploadCursor is the decoded cursor of the upload listing.
type uploadCursor struct {
	Key      string `json:"k"`
	UploadID string `json:"u"`
}

func encodeCursor(v any) string {
	raw, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeCursor(s string, v any) error {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err == nil {
		err = json.Unmarshal(raw, v)
	}
	if err != nil {
		return fmt.Errorf("%w: invalid cursor", domain.ErrInvalidArgument)
	}
	return nil
}

func (h *objectRoutes) listUploads(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, err := queryInt(r, "limit")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var cur uploadCursor
	if c := q.Get("cursor"); c != "" {
		if err := decodeCursor(c, &cur); err != nil {
			h.fail(w, r, err)
			return
		}
	}
	res, err := h.deps.Objects.ListMultipartUploads(r.Context(), domain.ListMultipartUploadsInput{
		Bucket: bucketName(r), Prefix: q.Get("prefix"), KeyMarker: cur.Key, UploadIDMarker: cur.UploadID, MaxUploads: limit,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := UploadListResponse{Items: make([]PendingUploadResponse, 0, len(res.Uploads))}
	for _, u := range res.Uploads {
		out.Items = append(out.Items, PendingUploadResponse{Key: u.Key, UploadID: u.UploadID, Initiated: u.Initiated.UTC()})
	}
	if res.IsTruncated {
		out.NextCursor = encodeCursor(uploadCursor{Key: res.NextKeyMarker, UploadID: res.NextUploadIDMarker})
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

// PartListResponse is a page of the parts of an upload.
type PartListResponse struct {
	Items      []PartResponse `json:"items"`
	NextCursor string         `json:"nextCursor,omitempty"`
}

func (h *objectRoutes) listParts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, err := queryInt(r, "limit")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	marker := 0
	if c := q.Get("cursor"); c != "" {
		if err := decodeCursor(c, &marker); err != nil {
			h.fail(w, r, err)
			return
		}
	}
	res, err := h.deps.Objects.ListObjectParts(r.Context(), domain.ListPartsInput{
		Bucket: bucketName(r), Key: q.Get("key"), UploadID: chi.URLParam(r, "uploadId"), PartNumberMarker: marker, MaxParts: limit,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := PartListResponse{Items: make([]PartResponse, 0, len(res.Parts))}
	for _, p := range res.Parts {
		out.Items = append(out.Items, PartResponse{PartNumber: p.Number, ETag: p.ETag, Size: p.Size, LastModified: optTime(p.ModTime)})
	}
	if res.IsTruncated {
		out.NextCursor = encodeCursor(res.NextPartNumberMarker)
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, out)
}

// uploadPart stores the raw body as one part, streaming.
func (h *objectRoutes) uploadPart(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(chi.URLParam(r, "partNumber"))
	if err != nil || n < 1 || n > maxParts {
		h.fail(w, r, fmt.Errorf("%w: the part number must be between 1 and %d", domain.ErrInvalidArgument, maxParts))
		return
	}
	if r.ContentLength < 0 {
		h.fail(w, r, fmt.Errorf("%w: Content-Length is required", domain.ErrInvalidArgument))
		return
	}
	if r.ContentLength > h.maxPart {
		h.fail(w, r, tooLarge(h.maxPart))
		return
	}
	body := newLimitedBody(r.Body, h.maxPart)
	part, err := h.deps.Objects.PutObjectPart(r.Context(), object.PutObjectPartInput{
		Bucket: bucketName(r), Key: r.URL.Query().Get("key"), UploadID: chi.URLParam(r, "uploadId"), PartNumber: n,
		Reader: body, Size: r.ContentLength,
	})
	if body.exceeded {
		err = tooLarge(h.maxPart)
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, PartResponse{PartNumber: part.Number, ETag: part.ETag, Size: part.Size, LastModified: optTime(part.ModTime)})
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
	var in CompleteUploadRequest
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	parts := make([]domain.CompletePart, 0, len(in.Parts))
	for _, p := range in.Parts {
		parts = append(parts, domain.CompletePart{Number: p.PartNumber, ETag: p.ETag})
	}
	info, err := h.deps.Objects.CompleteMultipartUpload(r.Context(), object.CompleteMultipartInput{
		Bucket: bucketName(r), Key: r.URL.Query().Get("key"), UploadID: chi.URLParam(r, "uploadId"), Parts: parts,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, newObjectResponse(info))
}

func (h *objectRoutes) abortUpload(w http.ResponseWriter, r *http.Request) {
	if err := h.deps.Objects.AbortMultipartUpload(r.Context(), bucketName(r), r.URL.Query().Get("key"), chi.URLParam(r, "uploadId")); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
