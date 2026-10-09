package admin

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	adminsvc "github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
)

// previewCSP confines the inline previews: no script, no plugin, no
// navigation, only the document itself.
const previewCSP = "default-src 'none'; img-src 'self' data:; media-src 'self'; style-src 'unsafe-inline'; sandbox"

// errorsIsNotModified reports a failed If-None-Match / If-Modified-Since.
func errorsIsNotModified(err error) bool { return errors.Is(err, domain.ErrNotModified) }

// ObjectDeps are the dependencies of the object routes.
type ObjectDeps struct {
	// Browser implements the object browser operations (required).
	Browser adminsvc.ObjectBrowser
	// Uploads serves the proxied uploads, single and multipart (required).
	Uploads adminsvc.UploadManager
	// Authz authorizes the routes (required).
	Authz  adminsvc.Authorizer
	Enc    httpx.Codec
	Logger *slog.Logger
}

// ObjectOption configures the object routes.
type ObjectOption func(*objectRoutes)

type objectRoutes struct {
	deps  ObjectDeps
	guard guard
}

var _ ObjectRoutes = (*objectRoutes)(nil)

// NewObjectRoutes returns the object browser routes.
func NewObjectRoutes(deps ObjectDeps, opts ...ObjectOption) (ObjectRoutes, error) {
	if deps.Browser == nil || deps.Uploads == nil || deps.Authz == nil {
		return nil, fmt.Errorf("%w: object routes require the object browser, the upload manager and the authorizer", domain.ErrInvalidArgument)
	}
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	h := &objectRoutes{deps: deps, guard: guard{authz: deps.Authz, enc: deps.Enc}}
	for _, o := range opts {
		o(h)
	}
	return h, nil
}

// Register implements Routes.
func (h *objectRoutes) Register(r chi.Router) { h.guard.register(r, h.routes()) }

func (h *objectRoutes) routes() []route {
	read := onObject(policy.ActionGetObject).orVersion(policy.ActionGetObjectVersion)
	return []route{
		{http.MethodGet, "/buckets/{bucket}/objects", byService(policy.ActionListBucket), h.list},
		{http.MethodDelete, "/buckets/{bucket}/objects", byService(policy.ActionDeleteObject, policy.ActionDeleteObjectVersion, policy.ActionListBucket,
			policy.ActionListBucketVersions, policy.ActionBypassGovernanceRetention), h.remove},
		{http.MethodGet, "/buckets/{bucket}/objects/stat", read, h.stat},
		{http.MethodGet, "/buckets/{bucket}/objects/versions", onBucket(policy.ActionListBucketVersions).when(prefixConditions("key")), h.versions},
		{http.MethodPost, "/buckets/{bucket}/objects/folder", byService(policy.ActionPutObject), h.folder},
		{http.MethodPost, "/buckets/{bucket}/objects/rename", byService(policy.ActionGetObject, policy.ActionPutObject, policy.ActionDeleteObject, policy.ActionListBucket), h.rename},
		{http.MethodPost, "/buckets/{bucket}/objects/restore", byService(policy.ActionGetObjectVersion, policy.ActionPutObject), h.restore},
		{http.MethodPut, "/buckets/{bucket}/objects/tags", onObject(policy.ActionPutObjectTagging).orVersion(policy.ActionPutObjectVersionTagging), h.putTags},
		{http.MethodGet, "/buckets/{bucket}/objects/download", read, h.download},
		{http.MethodGet, "/buckets/{bucket}/objects/preview", read, h.preview},
		{http.MethodPost, zipDownloadPattern, byService(policy.ActionGetObject, policy.ActionListBucket), h.zip},
		{http.MethodPost, "/buckets/{bucket}/objects/upload", onObject(policy.ActionPutObject), h.upload},
		// Object Lock and attributes of a version (?key=&versionId=). The
		// governance bypass of a retention change is asked in the body:
		// the service authorizes it.
		{http.MethodGet, "/buckets/{bucket}/objects/retention", onObject(policy.ActionGetObjectRetention), h.getRetention},
		{http.MethodPut, "/buckets/{bucket}/objects/retention", byService(policy.ActionPutObjectRetention, policy.ActionBypassGovernanceRetention), h.putRetention},
		{http.MethodGet, "/buckets/{bucket}/objects/legal-hold", onObject(policy.ActionGetObjectLegalHold), h.getLegalHold},
		{http.MethodPut, "/buckets/{bucket}/objects/legal-hold", onObject(policy.ActionPutObjectLegalHold), h.putLegalHold},
		{http.MethodGet, "/buckets/{bucket}/objects/attributes",
			onObject(policy.ActionGetObjectAttributes).orVersion(policy.ActionGetObjectVersionAttributes), h.attributes},

		{http.MethodGet, "/buckets/{bucket}/uploads", onBucket(policy.ActionListMultipart).when(prefixConditions("prefix")), h.listUploads},
		{http.MethodPost, "/buckets/{bucket}/uploads", onObject(policy.ActionPutObject), h.createUpload},
		{http.MethodGet, "/buckets/{bucket}/uploads/{uploadId}/parts", onObject(policy.ActionListMultipartParts), h.listParts},
		{http.MethodPut, "/buckets/{bucket}/uploads/{uploadId}/parts/{partNumber}", onObject(policy.ActionPutObject), h.uploadPart},
		{http.MethodPost, "/buckets/{bucket}/uploads/{uploadId}/complete", onObject(policy.ActionPutObject), h.completeUpload},
		{http.MethodDelete, "/buckets/{bucket}/uploads/{uploadId}", onObject(policy.ActionAbortMultipart), h.abortUpload},
	}
}

// optionalQuery returns the query parameter name, nil when the request
// does not set it (an empty value is set).
func optionalQuery(r *http.Request, name string) *string {
	q := r.URL.Query()
	if !q.Has(name) {
		return nil
	}
	v := q.Get(name)
	return &v
}

func (h *objectRoutes) fail(w http.ResponseWriter, r *http.Request, err error) {
	writeError(h.deps.Enc, w, r, err)
}

// ObjectResponse is an object (version) of the object browser.
type ObjectResponse struct {
	Key            string    `json:"key"`
	VersionID      string    `json:"versionId,omitempty"`
	Size           int64     `json:"size"`
	ETag           string    `json:"etag"`
	LastModified   time.Time `json:"lastModified"`
	ContentType    string    `json:"contentType,omitempty"`
	IsDeleteMarker bool      `json:"isDeleteMarker"`
	IsLatest       bool      `json:"isLatest"`
	StorageClass   string    `json:"storageClass,omitempty"`
}

func newObjectResponse(o domain.ObjectInfo) ObjectResponse {
	return ObjectResponse{
		Key: o.Key, VersionID: o.VersionID, Size: o.Size, ETag: o.ETag, LastModified: o.ModTime.UTC(),
		ContentType: o.ContentType, IsDeleteMarker: o.IsDeleteMarker, IsLatest: o.IsLatest, StorageClass: o.StorageClass,
	}
}

func newObjectResponses(infos []domain.ObjectInfo) []ObjectResponse {
	out := make([]ObjectResponse, 0, len(infos))
	for _, o := range infos {
		out = append(out, newObjectResponse(o))
	}
	return out
}

// ObjectListResponse is the body of GET /buckets/{bucket}/objects.
type ObjectListResponse struct {
	Objects []ObjectResponse `json:"objects"`
	// Prefixes are the "folders" directly under the prefix.
	Prefixes    []string `json:"prefixes"`
	IsTruncated bool     `json:"isTruncated"`
	// NextCursor resumes a truncated listing (query parameter "cursor").
	NextCursor string `json:"nextCursor,omitempty"`
}

func (h *objectRoutes) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, err := queryInt(r, "limit")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	res, err := h.deps.Browser.List(r.Context(), adminsvc.ListObjectsInput{
		Requester: requesterOf(r), Bucket: bucketName(r), Prefix: q.Get("prefix"), Delimiter: optionalQuery(r, "delimiter"), Cursor: q.Get("cursor"), Limit: limit,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, ObjectListResponse{
		Objects: newObjectResponses(res.Objects), Prefixes: nonNil(res.Prefixes), IsTruncated: res.NextCursor != "", NextCursor: res.NextCursor,
	})
}

// ObjectEncryptionResponse is the server-side encryption of a version.
type ObjectEncryptionResponse struct {
	// Type is SSE-S3, SSE-KMS or SSE-C.
	Type string `json:"type"`
	// KMSKeyID is the KMS key of SSE-S3 and SSE-KMS.
	KMSKeyID string `json:"kmsKeyId,omitempty"`
}

// ObjectDetailsResponse is the body of GET /buckets/{bucket}/objects/stat.
type ObjectDetailsResponse struct {
	ObjectResponse
	ContentEncoding    string            `json:"contentEncoding,omitempty"`
	ContentDisposition string            `json:"contentDisposition,omitempty"`
	ContentLanguage    string            `json:"contentLanguage,omitempty"`
	CacheControl       string            `json:"cacheControl,omitempty"`
	Expires            *time.Time        `json:"expires,omitempty"`
	UserMetadata       map[string]string `json:"userMetadata"`
	// Parts is the number of parts of a multipart object.
	Parts             int    `json:"parts,omitempty"`
	ChecksumAlgorithm string `json:"checksumAlgorithm,omitempty"`
	Checksum          string `json:"checksum,omitempty"`
	// Encryption is the server-side encryption of the version, absent
	// for an unencrypted one.
	Encryption *ObjectEncryptionResponse `json:"encryption,omitempty"`
	// Tags is absent when the requester may not read them.
	Tags *map[string]string `json:"tags,omitempty"`
	// Versions are the first versions of the key, absent when the
	// requester may not list them.
	Versions          *[]ObjectResponse `json:"versions,omitempty"`
	VersionsTruncated bool              `json:"versionsTruncated,omitempty"`
}

func (h *objectRoutes) stat(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	d, err := h.deps.Browser.Stat(r.Context(), adminsvc.StatInput{Requester: requesterOf(r), Bucket: bucketName(r), Key: q.Get("key"), VersionID: q.Get("versionId")})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := ObjectDetailsResponse{
		ObjectResponse:     newObjectResponse(d.Info),
		ContentEncoding:    d.Info.ContentEncoding,
		ContentDisposition: d.Info.ContentDisposition,
		ContentLanguage:    d.Info.ContentLanguage,
		CacheControl:       d.Info.CacheControl,
		Expires:            optTime(d.Info.Expires),
		UserMetadata:       nonNilMap(d.Info.UserMetadata),
		Parts:              d.Info.Parts,
		ChecksumAlgorithm:  string(d.Info.Checksum.Algorithm),
		Checksum:           d.Info.Checksum.Value,
		VersionsTruncated:  d.VersionsTruncated,
	}
	if e := d.Info.Encryption; !e.IsZero() {
		out.Encryption = &ObjectEncryptionResponse{Type: string(e.Type), KMSKeyID: e.KMSKeyID}
	}
	if d.Tags != nil {
		out.Tags = &d.Tags
	}
	if d.Versions != nil {
		versions := newObjectResponses(d.Versions)
		out.Versions = &versions
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, out)
}

// VersionListResponse is a page of versions of one key.
type VersionListResponse struct {
	Items      []ObjectResponse `json:"items"`
	NextCursor string           `json:"nextCursor,omitempty"`
}

func (h *objectRoutes) versions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, err := queryInt(r, "limit")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	res, err := h.deps.Browser.Versions(r.Context(), adminsvc.VersionsInput{Bucket: bucketName(r), Key: q.Get("key"), Cursor: q.Get("cursor"), Limit: limit})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, VersionListResponse{Items: newObjectResponses(res.Versions), NextCursor: res.NextCursor})
}

// FolderRequest is the JSON body of POST /buckets/{bucket}/objects/folder.
type FolderRequest struct {
	// Prefix is the folder key; a trailing "/" is added when missing.
	Prefix string `json:"prefix"`
}

func (h *objectRoutes) folder(w http.ResponseWriter, r *http.Request) {
	var in FolderRequest
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	info, err := h.deps.Browser.CreateFolder(r.Context(), adminsvc.FolderInput{Requester: requesterOf(r), Bucket: bucketName(r), Prefix: in.Prefix})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusCreated, newObjectResponse(info))
}

// restore is POST /buckets/{bucket}/objects/restore?key=&versionId=: the
// version becomes the latest again (a copy onto its own key).
func (h *objectRoutes) restore(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	info, err := h.deps.Browser.RestoreVersion(r.Context(), adminsvc.RestoreVersionInput{
		Requester: requesterOf(r), Bucket: bucketName(r), Key: q.Get("key"), VersionID: q.Get("versionId"),
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusCreated, newObjectResponse(info))
}

// ObjectRefRequest names an object or one of its versions.
type ObjectRefRequest struct {
	Key       string `json:"key"`
	VersionID string `json:"versionId,omitempty"`
}

// DeleteObjectsRequest is the JSON body of DELETE
// /buckets/{bucket}/objects.
type DeleteObjectsRequest struct {
	Objects []ObjectRefRequest `json:"objects,omitempty"`
	// Prefixes are folders deleted recursively ("photos" is "photos/").
	Prefixes []string `json:"prefixes,omitempty"`
	// AllVersions deletes every version under the prefixes permanently.
	AllVersions bool `json:"allVersions,omitempty"`
	// BypassGovernance deletes the versions a GOVERNANCE retention
	// protects, where the caller may (s3:BypassGovernanceRetention).
	BypassGovernance bool `json:"bypassGovernance,omitempty"`
}

// DeletedObjectResponse is a successful deletion.
type DeletedObjectResponse struct {
	Key                   string `json:"key"`
	VersionID             string `json:"versionId,omitempty"`
	DeleteMarker          bool   `json:"deleteMarker"`
	DeleteMarkerVersionID string `json:"deleteMarkerVersionId,omitempty"`
}

// ObjectErrorResponse is a per-object failure with the problem code of
// its error.
type ObjectErrorResponse struct {
	Key       string `json:"key"`
	VersionID string `json:"versionId,omitempty"`
	Code      string `json:"code"`
	Message   string `json:"message"`
}

// DeleteObjectsResponse reports every object of a deletion.
type DeleteObjectsResponse struct {
	Deleted []DeletedObjectResponse `json:"deleted"`
	Errors  []ObjectErrorResponse   `json:"errors"`
}

func (h *objectRoutes) objectError(key, versionID string, err error) ObjectErrorResponse {
	p := h.deps.Enc.ProblemFromError(err)
	return ObjectErrorResponse{Key: key, VersionID: versionID, Code: p.Code, Message: err.Error()}
}

func (h *objectRoutes) remove(w http.ResponseWriter, r *http.Request) {
	var in DeleteObjectsRequest
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	refs := make([]adminsvc.ObjectRef, 0, len(in.Objects))
	for _, o := range in.Objects {
		refs = append(refs, adminsvc.ObjectRef{Key: o.Key, VersionID: o.VersionID})
	}
	res, err := h.deps.Browser.Delete(r.Context(), adminsvc.DeleteObjectsInput{
		Requester: requesterOf(r), Bucket: bucketName(r), Objects: refs, Prefixes: in.Prefixes, AllVersions: in.AllVersions,
		BypassGovernance: in.BypassGovernance,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := DeleteObjectsResponse{Deleted: make([]DeletedObjectResponse, 0, len(res.Deleted)), Errors: make([]ObjectErrorResponse, 0, len(res.Errors))}
	for _, d := range res.Deleted {
		out.Deleted = append(out.Deleted, DeletedObjectResponse(d))
	}
	for _, e := range res.Errors {
		out.Errors = append(out.Errors, h.objectError(e.Key, e.VersionID, e.Err))
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, out)
}

// RenameRequest is the JSON body of POST /buckets/{bucket}/objects/rename.
type RenameRequest struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	// Prefix moves every object of the folder Source to the folder
	// Destination ("photos" is "photos/").
	Prefix bool `json:"prefix,omitempty"`
	// Overwrite replaces existing destination objects.
	Overwrite bool `json:"overwrite,omitempty"`
}

// RenamedObjectResponse is a successful move.
type RenamedObjectResponse struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
}

// RenameErrorResponse is a failed move.
type RenameErrorResponse struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Code        string `json:"code"`
	Message     string `json:"message"`
}

// RenameResponse reports every object of a rename.
type RenameResponse struct {
	Renamed []RenamedObjectResponse `json:"renamed"`
	Errors  []RenameErrorResponse   `json:"errors"`
}

func (h *objectRoutes) rename(w http.ResponseWriter, r *http.Request) {
	var in RenameRequest
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	res, err := h.deps.Browser.Rename(r.Context(), adminsvc.RenameInput{
		Requester: requesterOf(r), Bucket: bucketName(r), Source: in.Source, Destination: in.Destination, Prefix: in.Prefix, Overwrite: in.Overwrite,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := RenameResponse{Renamed: make([]RenamedObjectResponse, 0, len(res.Renamed)), Errors: make([]RenameErrorResponse, 0, len(res.Errors))}
	for _, m := range res.Renamed {
		out.Renamed = append(out.Renamed, RenamedObjectResponse(m))
	}
	for _, e := range res.Errors {
		p := h.deps.Enc.ProblemFromError(e.Err)
		out.Errors = append(out.Errors, RenameErrorResponse{Source: e.Source, Destination: e.Destination, Code: p.Code, Message: e.Err.Error()})
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, out)
}

func (h *objectRoutes) putTags(w http.ResponseWriter, r *http.Request) {
	var in TagsResponse
	if err := decodeJSON(h.deps.Enc, r, &in, false); err != nil {
		h.fail(w, r, err)
		return
	}
	q := r.URL.Query()
	tags, err := h.deps.Browser.SetTags(r.Context(), adminsvc.SetTagsInput{Bucket: bucketName(r), Key: q.Get("key"), VersionID: q.Get("versionId"), Tags: in.Tags})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteJSON(w, http.StatusOK, TagsResponse{Tags: nonNilMap(tags)})
}

func (h *objectRoutes) download(w http.ResponseWriter, r *http.Request) { h.serve(w, r, false) }

func (h *objectRoutes) preview(w http.ResponseWriter, r *http.Request) { h.serve(w, r, true) }

// serve streams an object: as an attachment for downloads, inline for the
// previews of safe types (adminsvc.ObjectBrowser.Open decides).
func (h *objectRoutes) serve(w http.ResponseWriter, r *http.Request, preview bool) {
	q := r.URL.Query()
	in := adminsvc.OpenInput{Bucket: bucketName(r), Key: q.Get("key"), VersionID: q.Get("versionId"), Preview: preview}
	if spec := r.Header.Get("Range"); spec != "" {
		rng, err := domain.ParseRange(spec)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		in.Range = rng
	}
	in.Conditions = domain.Conditions{IfMatch: r.Header.Get("If-Match"), IfNoneMatch: r.Header.Get("If-None-Match")}
	d, err := h.deps.Browser.Open(r.Context(), in)
	if err != nil {
		if errorsIsNotModified(err) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		h.fail(w, r, err)
		return
	}
	defer func() { _ = d.Reader.Close() }()
	hdr := w.Header()
	hdr.Set("Content-Type", d.ContentType)
	hdr.Set("Content-Disposition", contentDisposition(d.Disposition, d.FileName))
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("Cache-Control", "private, no-cache")
	hdr.Set("Accept-Ranges", "bytes")
	hdr.Set("Content-Length", strconv.FormatInt(d.Reader.Length, 10))
	if d.Reader.Info.ETag != "" {
		hdr.Set("ETag", `"`+d.Reader.Info.ETag+`"`)
	}
	if !d.Reader.Info.ModTime.IsZero() {
		hdr.Set("Last-Modified", d.Reader.Info.ModTime.UTC().Format(http.TimeFormat))
	}
	if d.Reader.Info.VersionID != "" {
		hdr.Set(httpx.HeaderAmzVersionID, d.Reader.Info.VersionID)
	}
	// Attachments are sandboxed too, should a browser render one anyway;
	// only the inline PDF viewer needs to run unconfined.
	if d.Sandbox || d.Disposition == adminsvc.DispositionAttachment {
		hdr.Set("Content-Security-Policy", previewCSP)
	}
	status := http.StatusOK
	if d.Partial {
		hdr.Set("Content-Range", domain.ContentRange(d.Reader.Offset, d.Reader.Length, d.Reader.Info.Size))
		status = http.StatusPartialContent
	}
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	if n, err := io.Copy(w, d.Reader); err != nil {
		// The status is sent: the client sees a truncated body.
		h.deps.Logger.DebugContext(r.Context(), "admin: download interrupted", "bucket", in.Bucket, "key", in.Key, "written", n, "err", err)
	}
}

// ZipRequest is the JSON body of POST
// /buckets/{bucket}/objects/download-zip. The console submits it instead
// as a form (application/x-www-form-urlencoded) whose field ZipFormField
// holds the JSON document, next to the CSRF token, so that the browser
// streams the archive to disk.
type ZipRequest struct {
	Keys     []string `json:"keys,omitempty"`
	Prefixes []string `json:"prefixes,omitempty"`
	// Base is the folder the archive names are relative to.
	Base string `json:"base,omitempty"`
}

// zipDownloadPattern is the route of the zip archives: the only route
// that accepts the CSRF token in a form field (see formTokenRoute).
const zipDownloadPattern = "/buckets/{bucket}/objects/download-zip"

// Fields of a download-zip form.
const (
	// ZipFormField holds the JSON ZipRequest.
	ZipFormField = "request"
	// DownloadTokenField is an optional token (16 to 64 ASCII letters and
	// digits) the console picks to learn that the archive started: the
	// answer then sets the cookie DownloadCookiePrefix+token, which the
	// console reads (a download never loads the frame of the form).
	DownloadTokenField = "downloadToken"
	// DownloadCookiePrefix prefixes the name of the download cookie.
	DownloadCookiePrefix = "nexo_dl_"
)

// downloadCookie returns the cookie signalling that the download of a
// form started, nil without a valid token.
func downloadCookie(r *http.Request) *http.Cookie {
	if r.PostForm == nil {
		return nil
	}
	token := r.PostForm.Get(DownloadTokenField)
	if len(token) < 16 || len(token) > 64 {
		return nil
	}
	for _, c := range token {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return nil
		}
	}
	return &http.Cookie{
		Name: DownloadCookiePrefix + token, Value: "1", Path: "/", MaxAge: 60,
		SameSite: http.SameSiteStrictMode, Secure: middleware.ClientOf(r).Secure,
	}
}

// decodeZip decodes the JSON body, or the JSON field of a form.
func (h *objectRoutes) decodeZip(w http.ResponseWriter, r *http.Request) (ZipRequest, error) {
	var in ZipRequest
	if !isForm(r) {
		return in, decodeJSON(h.deps.Enc, r, &in, false)
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBody)
	if err := r.ParseForm(); err != nil {
		return in, fmt.Errorf("%w: invalid form: %v", domain.ErrInvalidArgument, err)
	}
	if !r.PostForm.Has(ZipFormField) {
		return in, fmt.Errorf("%w: the form field %q is required", domain.ErrInvalidArgument, ZipFormField)
	}
	return in, h.deps.Enc.DecodeJSON(strings.NewReader(r.PostForm.Get(ZipFormField)), maxJSONBody, &in)
}

// zip streams a zip archive: the headers are sent once the request is
// validated, then every entry as it is read.
func (h *objectRoutes) zip(w http.ResponseWriter, r *http.Request) {
	in, err := h.decodeZip(w, r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	started := false
	res, err := h.deps.Browser.Zip(r.Context(), adminsvc.ZipInput{Requester: requesterOf(r), Bucket: bucketName(r), Base: in.Base, Keys: in.Keys, Prefixes: in.Prefixes},
		func(filename string) (io.Writer, error) {
			hdr := w.Header()
			hdr.Set("Content-Type", "application/zip")
			hdr.Set("Content-Disposition", contentDisposition(adminsvc.DispositionAttachment, filename))
			hdr.Set("X-Content-Type-Options", "nosniff")
			hdr.Set("Cache-Control", "no-store")
			if c := downloadCookie(r); c != nil {
				http.SetCookie(w, c)
			}
			w.WriteHeader(http.StatusOK)
			started = true
			return w, nil
		})
	if err != nil {
		if !started {
			h.fail(w, r, err)
			return
		}
		// The archive is truncated: the client sees an invalid zip.
		h.deps.Logger.WarnContext(r.Context(), "admin: zip archive interrupted", "bucket", bucketName(r), "err", err)
		return
	}
	h.deps.Logger.DebugContext(r.Context(), "admin: zip archive sent", "bucket", bucketName(r), "files", res.Files, "bytes", res.Bytes)
}

// contentDisposition formats a Content-Disposition with the file name
// (RFC 6266, RFC 2231 for non-ASCII names). A name that cannot be encoded
// is left out: the disposition itself is never lost.
func contentDisposition(disposition, filename string) string {
	if v := mime.FormatMediaType(disposition, map[string]string{"filename": filename}); v != "" {
		return v
	}
	return disposition
}
