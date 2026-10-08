package s3

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// ObjectDeps are the dependencies of the object routes. Buckets provides the
// bucket policies to the authorizer; Policy is optional.
type ObjectDeps struct {
	Objects object.Service
	Buckets bucket.Service
	Policy  policy.Service
	Enc     httpx.Codec
}

type objectRoutes struct {
	deps  ObjectDeps
	authz authorizer
}

var _ ObjectRoutes = (*objectRoutes)(nil)

// NewObjectRoutes returns the object route group.
func NewObjectRoutes(deps ObjectDeps) ObjectRoutes {
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	return &objectRoutes{deps: deps, authz: authorizer{policy: deps.Policy, buckets: deps.Buckets, enc: deps.Enc}}
}

// objectOperations returns the object-level operations and their handlers.
func (h *objectRoutes) objectOperations() map[Operation]http.HandlerFunc {
	return map[Operation]http.HandlerFunc{
		{Name: "PutObject", Method: "PUT", Scope: ScopeObject}:                                h.putObject,
		{Name: "GetObject", Method: "GET", Scope: ScopeObject}:                                h.getObject,
		{Name: "HeadObject", Method: "HEAD", Scope: ScopeObject}:                              h.headObject,
		{Name: "DeleteObject", Method: "DELETE", Scope: ScopeObject}:                          h.deleteObject,
		{Name: "DeleteObjects", Method: "POST", Scope: ScopeBucket, Query: "delete"}:          h.deleteObjects,
		{Name: "GetObjectTagging", Method: "GET", Scope: ScopeObject, Query: "tagging"}:       h.getTagging,
		{Name: "PutObjectTagging", Method: "PUT", Scope: ScopeObject, Query: "tagging"}:       h.putTagging,
		{Name: "DeleteObjectTagging", Method: "DELETE", Scope: ScopeObject, Query: "tagging"}: h.deleteTagging,
		{Name: "GetObjectAcl", Method: "GET", Scope: ScopeObject, Query: "acl"}:               h.getACL,
		{Name: "PutObjectAcl", Method: "PUT", Scope: ScopeObject, Query: "acl"}:               h.putACL,
	}
}

// unimplementedObjectOperations are advertised but not served yet (M7).
func unimplementedObjectOperations() []Operation {
	return []Operation{
		{Name: "GetObjectRetention", Method: "GET", Scope: ScopeObject, Query: "retention"},
		{Name: "PutObjectRetention", Method: "PUT", Scope: ScopeObject, Query: "retention"},
		{Name: "GetObjectLegalHold", Method: "GET", Scope: ScopeObject, Query: "legal-hold"},
		{Name: "PutObjectLegalHold", Method: "PUT", Scope: ScopeObject, Query: "legal-hold"},
		{Name: "GetObjectAttributes", Method: "GET", Scope: ScopeObject, Query: "attributes"},
	}
}

// Mount implements Group.
func (h *objectRoutes) Mount(d Dispatcher) {
	mountAll(d, h.deps.Enc, h.objectOperations(), h.deps.Objects != nil)
	mountNotImplemented(d, h.deps.Enc, unimplementedObjectOperations())
}

// Register implements Routes.
func (h *objectRoutes) Register(r chi.Router) { registerStandalone(r, h.deps.Enc, h) }

// putObject stores a new version; with x-amz-copy-source it is a CopyObject.
func (h *objectRoutes) putObject(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(httpx.HeaderAmzCopySource) != "" {
		h.copyObject(w, r)
		return
	}
	bucketName, key := BucketName(r), ObjectKey(r)
	if !h.authz.authorize(w, r, policy.ActionPutObject, bucketName, key) {
		return
	}
	in, err := objectInput(r, bucketName, key)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	info, err := h.deps.Objects.PutObject(r.Context(), in)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	w.Header().Set("ETag", quoteETag(info.ETag))
	if info.VersionID != "" {
		w.Header().Set(httpx.HeaderAmzVersionID, info.VersionID)
	}
	if !info.Checksum.IsZero() && info.Checksum.Value != "" {
		w.Header().Set("x-amz-checksum-"+toLower(string(info.Checksum.Algorithm)), info.Checksum.Value)
	}
	w.WriteHeader(http.StatusOK)
}

func (h *objectRoutes) copyObject(w http.ResponseWriter, r *http.Request) {
	dstBucket, dstKey := BucketName(r), ObjectKey(r)
	srcBucket, srcKey, srcVersion, err := parseCopySource(r.Header.Get(httpx.HeaderAmzCopySource))
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	if !h.authz.authorize(w, r, policy.ActionGetObject, srcBucket, srcKey) || !h.authz.authorize(w, r, policy.ActionPutObject, dstBucket, dstKey) {
		return
	}
	tags, err := parseTaggingHeader(r.Header)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	in := object.CopyObjectInput{
		SrcBucket: srcBucket, SrcKey: srcKey, SrcVersionID: srcVersion,
		SrcConditions:   parseConditions(r.Header, "X-Amz-Copy-Source-If-"),
		DstBucket:       dstBucket,
		DstKey:          dstKey,
		ReplaceMetadata: r.Header.Get("x-amz-metadata-directive") == "REPLACE",
		ContentType:     r.Header.Get("Content-Type"),
		UserMetadata:    userMetadata(r.Header),
		ReplaceTags:     r.Header.Get("x-amz-tagging-directive") == "REPLACE",
		Tags:            tags,
		StorageClass:    r.Header.Get(httpx.HeaderAmzStorageClass),
	}
	info, err := h.deps.Objects.CopyObject(r.Context(), in)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	if info.VersionID != "" {
		w.Header().Set(httpx.HeaderAmzVersionID, info.VersionID)
	}
	if srcVersion != "" {
		w.Header().Set("x-amz-copy-source-version-id", srcVersion)
	}
	h.deps.Enc.WriteXML(w, http.StatusOK, CopyObjectResult{ETag: quoteETag(info.ETag), LastModified: s3Time(info.ModTime)})
}

// getOptions reads the GetObject/HeadObject parameters.
func getOptions(r *http.Request) (object.GetObjectOptions, error) {
	q := r.URL.Query()
	rg, err := parseRange(r.Header)
	if err != nil {
		return object.GetObjectOptions{}, err
	}
	partNumber, err := queryInt(q, "partNumber", 0)
	if err != nil {
		return object.GetObjectOptions{}, err
	}
	return object.GetObjectOptions{VersionID: q.Get("versionId"), Range: rg, Conditions: parseConditions(r.Header, "If-"), PartNumber: partNumber}, nil
}

func (h *objectRoutes) readAction(r *http.Request) policy.Action {
	if r.URL.Query().Get("versionId") != "" {
		return policy.ActionGetObjectVersion
	}
	return policy.ActionGetObject
}

func (h *objectRoutes) getObject(w http.ResponseWriter, r *http.Request) {
	bucketName, key := BucketName(r), ObjectKey(r)
	if !h.authz.authorize(w, r, h.readAction(r), bucketName, key) {
		return
	}
	opts, err := getOptions(r)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	rd, err := h.deps.Objects.GetObject(r.Context(), bucketName, key, opts)
	if err != nil {
		h.writeReadError(w, r, err)
		return
	}
	defer func() { _ = rd.Close() }()
	writeObjectHeaders(w, rd.Info)
	applyResponseOverrides(w, r.URL.Query())
	w.Header().Set("Content-Length", strconv.FormatInt(rd.Length, 10))
	status := http.StatusOK
	if opts.Range != nil || opts.PartNumber > 0 {
		w.Header().Set("Content-Range", domain.ContentRange(rd.Offset, rd.Length, rd.Info.Size))
		status = http.StatusPartialContent
	}
	w.WriteHeader(status)
	_, _ = io.Copy(w, rd)
}

func (h *objectRoutes) headObject(w http.ResponseWriter, r *http.Request) {
	bucketName, key := BucketName(r), ObjectKey(r)
	if !h.authz.authorize(w, r, h.readAction(r), bucketName, key) {
		return
	}
	opts, err := getOptions(r)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	info, err := h.deps.Objects.GetObjectInfo(r.Context(), bucketName, key, opts)
	if err != nil {
		h.writeReadError(w, r, err)
		return
	}
	writeObjectHeaders(w, info)
	applyResponseOverrides(w, r.URL.Query())
	length := info.Size
	status := http.StatusOK
	if opts.Range != nil {
		offset, l, rerr := opts.Range.Resolve(info.Size)
		if rerr != nil {
			h.deps.Enc.WriteS3Error(w, r, rerr)
			return
		}
		w.Header().Set("Content-Range", domain.ContentRange(offset, l, info.Size))
		length = l
		status = http.StatusPartialContent
	}
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	w.WriteHeader(status)
}

// writeReadError adds the delete-marker header to not-found errors caused
// by a marker and writes the S3 error.
func (h *objectRoutes) writeReadError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, domain.ErrMethodNotAllowed) || (errors.Is(err, domain.ErrObjectNotFound) && isMarkerError(err)) {
		w.Header().Set(httpx.HeaderAmzDeleteMarker, "true")
	}
	h.deps.Enc.WriteS3Error(w, r, err)
}

// isMarkerError recognizes the NoSuchKey raised by a latest delete marker.
func isMarkerError(err error) bool {
	var s3err *domain.S3Error
	return errors.As(err, &s3err) && s3err.Code == "NoSuchKey"
}

func (h *objectRoutes) deleteObject(w http.ResponseWriter, r *http.Request) {
	bucketName, key := BucketName(r), ObjectKey(r)
	versionID := r.URL.Query().Get("versionId")
	action := policy.ActionDeleteObject
	if versionID != "" {
		action = policy.ActionDeleteObjectVersion
	}
	if !h.authz.authorize(w, r, action, bucketName, key) {
		return
	}
	del, err := h.deps.Objects.DeleteObject(r.Context(), bucketName, key, object.DeleteObjectOptions{
		VersionID: versionID, BypassGovernance: r.Header.Get("x-amz-bypass-governance-retention") == "true",
	})
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	writeDeleted(w, del)
	w.WriteHeader(http.StatusNoContent)
}

func writeDeleted(w http.ResponseWriter, del object.DeletedObject) {
	if del.DeleteMarker {
		w.Header().Set(httpx.HeaderAmzDeleteMarker, "true")
	}
	switch {
	case del.DeleteMarkerVersionID != "":
		w.Header().Set(httpx.HeaderAmzVersionID, del.DeleteMarkerVersionID)
	case del.VersionID != "":
		w.Header().Set(httpx.HeaderAmzVersionID, del.VersionID)
	}
}

func (h *objectRoutes) deleteObjects(w http.ResponseWriter, r *http.Request) {
	bucketName := BucketName(r)
	var req DeleteRequest
	if err := h.deps.Enc.DecodeXML(r.Body, maxXMLBody, &req); err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	if len(req.Objects) == 0 {
		h.deps.Enc.WriteS3Error(w, r, fmt.Errorf("%w: no objects to delete", domain.ErrMalformedXML))
		return
	}
	in := object.DeleteObjectsInput{Bucket: bucketName, Quiet: req.Quiet, BypassGovernance: r.Header.Get("x-amz-bypass-governance-retention") == "true"}
	var out DeleteResult
	for _, o := range req.Objects {
		action := policy.ActionDeleteObject
		if o.VersionID != "" {
			action = policy.ActionDeleteObjectVersion
		}
		if !h.authz.allowed(r, action, bucketName, o.Key) {
			code, msg := h.deps.Enc.MapS3Error(domain.ErrAccessDenied)
			out.Errors = append(out.Errors, deleteError(o.Key, o.VersionID, code, msg))
			continue
		}
		in.Objects = append(in.Objects, object.ObjectToDelete{Key: o.Key, VersionID: o.VersionID})
	}
	if len(in.Objects) > 0 {
		res, err := h.deps.Objects.DeleteObjects(r.Context(), in)
		if err != nil {
			h.deps.Enc.WriteS3Error(w, r, err)
			return
		}
		if !req.Quiet {
			for _, d := range res.Deleted {
				out.Deleted = append(out.Deleted, struct {
					Key                   string `xml:"Key"`
					VersionID             string `xml:"VersionId,omitempty"`
					DeleteMarker          bool   `xml:"DeleteMarker,omitempty"`
					DeleteMarkerVersionID string `xml:"DeleteMarkerVersionId,omitempty"`
				}{Key: d.Key, VersionID: d.VersionID, DeleteMarker: d.DeleteMarker, DeleteMarkerVersionID: d.DeleteMarkerVersionID})
			}
		}
		for _, e := range res.Errors {
			code, msg := h.deps.Enc.MapS3Error(e.Err)
			out.Errors = append(out.Errors, deleteError(e.Key, e.VersionID, code, msg))
		}
	}
	h.deps.Enc.WriteXML(w, http.StatusOK, out)
}

func deleteError(key, versionID string, code httpx.S3Code, msg string) struct {
	Key       string `xml:"Key"`
	VersionID string `xml:"VersionId,omitempty"`
	Code      string `xml:"Code"`
	Message   string `xml:"Message"`
} {
	return struct {
		Key       string `xml:"Key"`
		VersionID string `xml:"VersionId,omitempty"`
		Code      string `xml:"Code"`
		Message   string `xml:"Message"`
	}{Key: key, VersionID: versionID, Code: code.Code, Message: msg}
}

func (h *objectRoutes) getTagging(w http.ResponseWriter, r *http.Request) {
	bucketName, key := BucketName(r), ObjectKey(r)
	if !h.authz.authorize(w, r, policy.ActionGetObjectTagging, bucketName, key) {
		return
	}
	tags, err := h.deps.Objects.GetObjectTags(r.Context(), bucketName, key, object.TagOptions{VersionID: r.URL.Query().Get("versionId")})
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	h.deps.Enc.WriteXML(w, http.StatusOK, tagsToXML(tags))
}

func (h *objectRoutes) putTagging(w http.ResponseWriter, r *http.Request) {
	bucketName, key := BucketName(r), ObjectKey(r)
	if !h.authz.authorize(w, r, policy.ActionPutObjectTagging, bucketName, key) {
		return
	}
	var doc taggingRequest
	if err := h.deps.Enc.DecodeXML(r.Body, maxXMLBody, &doc); err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	tags, err := tagsFromXML(doc.TagSet)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	info, err := h.deps.Objects.PutObjectTags(r.Context(), bucketName, key, tags, object.TagOptions{VersionID: r.URL.Query().Get("versionId")})
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	if info.VersionID != "" {
		w.Header().Set(httpx.HeaderAmzVersionID, info.VersionID)
	}
	w.WriteHeader(http.StatusOK)
}

func (h *objectRoutes) deleteTagging(w http.ResponseWriter, r *http.Request) {
	bucketName, key := BucketName(r), ObjectKey(r)
	if !h.authz.authorize(w, r, policy.ActionDeleteObjectTagging, bucketName, key) {
		return
	}
	info, err := h.deps.Objects.DeleteObjectTags(r.Context(), bucketName, key, object.TagOptions{VersionID: r.URL.Query().Get("versionId")})
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	if info.VersionID != "" {
		w.Header().Set(httpx.HeaderAmzVersionID, info.VersionID)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *objectRoutes) getACL(w http.ResponseWriter, r *http.Request) {
	bucketName, key := BucketName(r), ObjectKey(r)
	if !h.authz.authorize(w, r, "s3:GetObjectAcl", bucketName, key) {
		return
	}
	if _, err := h.deps.Objects.GetObjectInfo(r.Context(), bucketName, key, object.GetObjectOptions{VersionID: r.URL.Query().Get("versionId")}); err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	h.deps.Enc.WriteXML(w, http.StatusOK, cannedPrivateACL(ownerOf(r)))
}

func (h *objectRoutes) putACL(w http.ResponseWriter, r *http.Request) {
	bucketName, key := BucketName(r), ObjectKey(r)
	if !h.authz.authorize(w, r, "s3:PutObjectAcl", bucketName, key) {
		return
	}
	if acl := r.Header.Get("x-amz-acl"); acl != "" && acl != "private" {
		h.deps.Enc.WriteS3Error(w, r, fmt.Errorf("%w: only the private canned ACL is supported, use bucket policies", domain.ErrNotImplemented))
		return
	}
	if _, err := h.deps.Objects.GetObjectInfo(r.Context(), bucketName, key, object.GetObjectOptions{VersionID: r.URL.Query().Get("versionId")}); err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}
