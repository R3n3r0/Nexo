package s3

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// ObjectDeps are the dependencies of the object routes. Objects and Policy
// (which resolves the bucket policies itself) are required: without them
// every operation answers NotImplemented. Clock and Logger default to the
// wall clock and slog.Default().
type ObjectDeps struct {
	Objects object.Service
	Policy  policy.Service
	Enc     httpx.Codec
	Clock   clock.Clock
	Logger  *slog.Logger
}

type objectRoutes struct {
	deps  ObjectDeps
	opts  options
	authz authorizer
}

var _ ObjectRoutes = (*objectRoutes)(nil)

// NewObjectRoutes returns the object route group (WithInsecureSSEC).
func NewObjectRoutes(deps ObjectDeps, opts ...Option) ObjectRoutes {
	return newObjectRoutes(deps, opts...)
}

// newObjectRoutes builds the concrete group, which the presigned routes
// reuse for the object operations.
func newObjectRoutes(deps ObjectDeps, opts ...Option) *objectRoutes {
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &objectRoutes{deps: deps, opts: newOptions(opts), authz: authorizer{policy: deps.Policy, enc: deps.Enc, clock: deps.Clock, objects: deps.Objects}}
}

// available reports whether the services the handlers need are wired.
func (h *objectRoutes) available() bool {
	return h.deps.Objects != nil && h.deps.Policy != nil
}

// objectOperations returns the object-level operations and their handlers.
func (h *objectRoutes) objectOperations() map[Operation]http.HandlerFunc {
	return map[Operation]http.HandlerFunc{
		{Name: "PutObject", Method: http.MethodPut, Scope: ScopeObject}:                                h.putObject,
		{Name: "GetObject", Method: http.MethodGet, Scope: ScopeObject}:                                h.getObject,
		{Name: "HeadObject", Method: http.MethodHead, Scope: ScopeObject}:                              h.headObject,
		{Name: "DeleteObject", Method: http.MethodDelete, Scope: ScopeObject}:                          h.deleteObject,
		{Name: "DeleteObjects", Method: http.MethodPost, Scope: ScopeBucket, Query: "delete"}:          h.deleteObjects,
		{Name: "GetObjectTagging", Method: http.MethodGet, Scope: ScopeObject, Query: "tagging"}:       h.getTagging,
		{Name: "PutObjectTagging", Method: http.MethodPut, Scope: ScopeObject, Query: "tagging"}:       h.putTagging,
		{Name: "DeleteObjectTagging", Method: http.MethodDelete, Scope: ScopeObject, Query: "tagging"}: h.deleteTagging,
		{Name: "GetObjectAcl", Method: http.MethodGet, Scope: ScopeObject, Query: "acl"}:               h.getACL,
		{Name: "PutObjectAcl", Method: http.MethodPut, Scope: ScopeObject, Query: "acl"}:               h.putACL,
		{Name: "GetObjectRetention", Method: http.MethodGet, Scope: ScopeObject, Query: "retention"}:   h.getRetention,
		{Name: "PutObjectRetention", Method: http.MethodPut, Scope: ScopeObject, Query: "retention"}:   h.putRetention,
		{Name: "GetObjectLegalHold", Method: http.MethodGet, Scope: ScopeObject, Query: "legal-hold"}:  h.getLegalHold,
		{Name: "PutObjectLegalHold", Method: http.MethodPut, Scope: ScopeObject, Query: "legal-hold"}:  h.putLegalHold,
		{Name: "GetObjectAttributes", Method: http.MethodGet, Scope: ScopeObject, Query: "attributes"}: h.getAttributes,
	}
}

// Mount implements Group.
func (h *objectRoutes) Mount(d Dispatcher) {
	mountAll(d, h.deps.Enc, h.objectOperations(), h.available())
	mountNotImplemented(d, h.deps.Enc, unsupportedObjectOperations())
}

// Register implements Routes.
func (h *objectRoutes) Register(r chi.Router) { registerStandalone(r, h.deps.Enc, h) }

func (h *objectRoutes) fail(w http.ResponseWriter, r *http.Request, err error) {
	h.deps.Enc.WriteS3Error(w, r, err)
}

// putInput assembles the PutObject input from the request headers; the
// body streams to the object layer.
func putInput(r *http.Request, bucketName, key string, allowInsecureSSEC bool) (object.PutObjectInput, error) {
	if err := checkUnsupportedHeaders(r.Header); err != nil {
		return object.PutObjectInput{}, err
	}
	size, err := contentLength(r)
	if err != nil {
		return object.PutObjectInput{}, err
	}
	meta, err := metadataFromHeaders(r.Header)
	if err != nil {
		return object.PutObjectInput{}, err
	}
	md5Hex, err := contentMD5Hex(r.Header)
	if err != nil {
		return object.PutObjectInput{}, err
	}
	tags, err := parseTaggingHeader(r.Header)
	if err != nil {
		return object.PutObjectInput{}, err
	}
	checksum, err := checksumFromHeaders(r.Header)
	if err != nil {
		return object.PutObjectInput{}, err
	}
	retention, hold, err := lockFromHeaders(r.Header)
	if err != nil {
		return object.PutObjectInput{}, err
	}
	storageClass := r.Header.Get(httpx.HeaderAmzStorageClass)
	if err := checkStorageClass(storageClass); err != nil {
		return object.PutObjectInput{}, err
	}
	sse, err := sseFromHeaders(r, allowInsecureSSEC)
	if err != nil {
		return object.PutObjectInput{}, err
	}
	var body io.Reader = http.NoBody
	if r.Body != nil {
		body = r.Body
	}
	return object.PutObjectInput{
		Bucket: bucketName, Key: key, Reader: body, Size: size,
		MD5Hex: md5Hex, SHA256Hex: unsignedPayloadSHA256(r), Checksum: checksum,
		ContentType: meta.contentType, ContentEncoding: meta.contentEncoding, ContentDisposition: meta.contentDisposition,
		ContentLanguage: meta.contentLanguage, CacheControl: meta.cacheControl, Expires: meta.expires,
		UserMetadata: meta.user, Tags: tags, StorageClass: storageClass,
		Conditions: parseConditions(r.Header, "If-"),
		Retention:  retention, LegalHold: hold,
		SSE: sse,
	}, nil
}

// putObject stores a new version; with x-amz-copy-source it is a CopyObject.
func (h *objectRoutes) putObject(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(httpx.HeaderAmzCopySource) != "" {
		h.copyObject(w, r)
		return
	}
	bucketName, key := BucketName(r), ObjectKey(r)
	if !h.authz.authorize(w, r, bucketName, key) {
		return
	}
	in, err := putInput(r, bucketName, key, h.opts.insecureSSEC)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if !h.authz.authorizeLockHeaders(w, r, bucketName, key, in.Retention, in.LegalHold) {
		return
	}
	info, err := h.deps.Objects.PutObject(r.Context(), in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("ETag", quoteETag(info.ETag))
	if info.VersionID != "" {
		w.Header().Set(httpx.HeaderAmzVersionID, info.VersionID)
	}
	writeChecksum(w, info.Checksum)
	writeSSEHeaders(w, info.Encryption)
	w.WriteHeader(http.StatusOK)
}

func (h *objectRoutes) copyObject(w http.ResponseWriter, r *http.Request) {
	httpx.SetOperationName(r.Context(), "CopyObject")
	if err := requireSignedCopySource(r); err != nil {
		h.fail(w, r, err)
		return
	}
	dstBucket, dstKey := BucketName(r), ObjectKey(r)
	srcBucket, srcKey, srcVersion, err := parseCopySource(r.Header.Get(httpx.HeaderAmzCopySource))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	readAction := ActionOf("GetObject", srcVersion != "")
	if !h.authz.authorizeVersion(w, r, readAction, srcBucket, srcKey, srcVersion, nil) ||
		!h.authz.authorizeAction(w, r, policy.ActionPutObject, dstBucket, dstKey) {
		return
	}
	in, err := copyInput(r, h.opts.insecureSSEC)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if !h.authz.authorizeLockHeaders(w, r, dstBucket, dstKey, in.Retention, in.LegalHold) {
		return
	}
	in.SrcBucket, in.SrcKey, in.SrcVersionID = srcBucket, srcKey, srcVersion
	in.DstBucket, in.DstKey = dstBucket, dstKey
	info, err := h.deps.Objects.CopyObject(r.Context(), in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if info.VersionID != "" {
		w.Header().Set(httpx.HeaderAmzVersionID, info.VersionID)
	}
	if srcVersion != "" {
		w.Header().Set(httpx.HeaderAmzCopySourceVersionID, srcVersion)
	}
	writeSSEHeaders(w, info.Encryption)
	out := CopyObjectResult{ETag: quoteETag(info.ETag), LastModified: s3Time(info.ModTime), ChecksumFields: checksumFields(info.Checksum)}
	if !info.Checksum.IsZero() {
		out.ChecksumType = string(info.Checksum.Type)
	}
	h.deps.Enc.WriteXML(w, http.StatusOK, out)
}

// copyInput reads the CopyObject headers (source and destination are
// filled in by the caller).
func copyInput(r *http.Request, allowInsecureSSEC bool) (object.CopyObjectInput, error) {
	if err := checkUnsupportedHeaders(r.Header); err != nil {
		return object.CopyObjectInput{}, err
	}
	metaDirective, err := directive(r.Header, httpx.HeaderAmzMetadataDirective)
	if err != nil {
		return object.CopyObjectInput{}, err
	}
	tagDirective, err := directive(r.Header, httpx.HeaderAmzTaggingDirective)
	if err != nil {
		return object.CopyObjectInput{}, err
	}
	storageClass := r.Header.Get(httpx.HeaderAmzStorageClass)
	if err := checkStorageClass(storageClass); err != nil {
		return object.CopyObjectInput{}, err
	}
	algo, err := checksumAlgorithmHeader(r.Header)
	if err != nil {
		return object.CopyObjectInput{}, err
	}
	retention, hold, err := lockFromHeaders(r.Header)
	if err != nil {
		return object.CopyObjectInput{}, err
	}
	sse, err := sseFromHeaders(r, allowInsecureSSEC)
	if err != nil {
		return object.CopyObjectInput{}, err
	}
	srcKey, err := customerKeyFromHeaders(r, headerCopySSECustomerPrefix, allowInsecureSSEC)
	if err != nil {
		return object.CopyObjectInput{}, err
	}
	in := object.CopyObjectInput{
		SSE:               sse,
		SrcSSECustomerKey: srcKey,
		SrcConditions:     parseConditions(r.Header, httpx.HeaderAmzCopySourceIfPrefix),
		ReplaceMetadata:   metaDirective == directiveReplace,
		ReplaceTags:       tagDirective == directiveReplace,
		StorageClass:      storageClass,
		ChecksumAlgorithm: algo,
		Retention:         retention,
		LegalHold:         hold,
	}
	if in.ReplaceMetadata {
		meta, err := metadataFromHeaders(r.Header)
		if err != nil {
			return object.CopyObjectInput{}, err
		}
		in.ContentType, in.ContentEncoding, in.ContentDisposition = meta.contentType, meta.contentEncoding, meta.contentDisposition
		in.ContentLanguage, in.CacheControl, in.Expires, in.UserMetadata = meta.contentLanguage, meta.cacheControl, meta.expires, meta.user
	}
	if in.ReplaceTags {
		tags, err := parseTaggingHeader(r.Header)
		if err != nil {
			return object.CopyObjectInput{}, err
		}
		in.Tags = tags
	}
	return in, nil
}

// getOptions reads the GetObject/HeadObject parameters.
func getOptions(r *http.Request, allowInsecureSSEC bool) (object.GetObjectOptions, error) {
	q := r.URL.Query()
	opts := object.GetObjectOptions{VersionID: q.Get("versionId"), Range: parseRange(r.Header), Conditions: parseConditions(r.Header, "If-")}
	ckey, err := customerKeyFromHeaders(r, headerSSECustomerPrefix, allowInsecureSSEC)
	if err != nil {
		return object.GetObjectOptions{}, err
	}
	opts.SSECustomerKey = ckey
	if q.Has("partNumber") {
		n, err := strconv.Atoi(q.Get("partNumber"))
		if err != nil || n < minPartNumber || n > maxPartNumber {
			return object.GetObjectOptions{}, invalidArgument("Part number must be an integer between 1 and 10000, inclusive")
		}
		opts.PartNumber = n
	}
	return opts, nil
}

func (h *objectRoutes) getObject(w http.ResponseWriter, r *http.Request) {
	bucketName, key := BucketName(r), ObjectKey(r)
	if !h.authz.authorize(w, r, bucketName, key) {
		return
	}
	opts, err := getOptions(r, h.opts.insecureSSEC)
	if err == nil {
		err = checkResponseOverrides(r)
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	rd, err := h.deps.Objects.GetObject(r.Context(), bucketName, key, opts)
	if err != nil {
		h.writeReadError(w, r, err, bucketName, key, opts)
		return
	}
	defer func() { _ = rd.Close() }()
	// A 206 describes the bytes it carries: an empty part is answered 200
	// without Content-Range ("bytes */N" belongs to the 416).
	partial := (opts.Range != nil || opts.PartNumber > 0) && rd.Length > 0
	writeObjectHeaders(w, r, rd.Info, opts.Range == nil)
	h.authz.writeLockHeaders(w, r, rd.Info)
	applyResponseOverrides(w, r.URL.Query())
	w.Header().Set("Content-Length", strconv.FormatInt(rd.Length, 10))
	if opts.PartNumber > 0 && rd.Info.Parts > 0 {
		w.Header().Set(httpx.HeaderAmzMPPartsCount, strconv.Itoa(rd.Info.Parts))
	}
	status := http.StatusOK
	if partial {
		w.Header().Set("Content-Range", domain.ContentRange(rd.Offset, rd.Length, rd.Info.Size))
		status = http.StatusPartialContent
	}
	w.WriteHeader(status)
	if n, err := io.Copy(w, rd); err != nil {
		// The status is sent: the client sees a truncated body.
		h.deps.Logger.DebugContext(r.Context(), "s3: GetObject body interrupted", "bucket", bucketName, "key", key, "written", n, "err", err)
	}
}

func (h *objectRoutes) headObject(w http.ResponseWriter, r *http.Request) {
	bucketName, key := BucketName(r), ObjectKey(r)
	if !h.authz.authorize(w, r, bucketName, key) {
		return
	}
	opts, err := getOptions(r, h.opts.insecureSSEC)
	if err == nil {
		err = checkResponseOverrides(r)
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if opts.PartNumber > 0 {
		// The size of a part is known to the reader only: open it, send
		// the headers and close it unread.
		rd, err := h.deps.Objects.GetObject(r.Context(), bucketName, key, opts)
		if err != nil {
			h.writeReadError(w, r, err, bucketName, key, opts)
			return
		}
		_ = rd.Close()
		partial := rd.Length > 0 // as GetObject: an empty part is a 200
		h.writeHeadResponse(w, r, rd.Info, rd.Offset, rd.Length, partial)
		if rd.Info.Parts > 0 {
			w.Header().Set(httpx.HeaderAmzMPPartsCount, strconv.Itoa(rd.Info.Parts))
		}
		if partial {
			w.WriteHeader(http.StatusPartialContent)
		} else {
			w.WriteHeader(http.StatusOK)
		}
		return
	}
	info, err := h.deps.Objects.GetObjectInfo(r.Context(), bucketName, key, opts)
	if err != nil {
		h.writeReadError(w, r, err, bucketName, key, opts)
		return
	}
	offset, length, partial := int64(0), info.Size, false
	if opts.Range != nil {
		offset, length, err = opts.Range.Resolve(info.Size)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		partial = true
	}
	h.writeHeadResponse(w, r, info, offset, length, partial)
	if partial {
		w.WriteHeader(http.StatusPartialContent)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// writeHeadResponse sets the headers of a HEAD answer.
func (h *objectRoutes) writeHeadResponse(w http.ResponseWriter, r *http.Request, info domain.ObjectInfo, offset, length int64, partial bool) {
	writeObjectHeaders(w, r, info, !partial || r.URL.Query().Has("partNumber"))
	h.authz.writeLockHeaders(w, r, info)
	applyResponseOverrides(w, r.URL.Query())
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	if partial {
		w.Header().Set("Content-Range", domain.ContentRange(offset, length, info.Size))
	}
}

// writeReadError writes the error of a read: a 304 carries the ETag and
// Last-Modified of the object, a delete marker (404 NoSuchKey for the
// latest version, 405 MethodNotAllowed for an explicit one) its
// x-amz-delete-marker, x-amz-version-id and Last-Modified headers.
func (h *objectRoutes) writeReadError(w http.ResponseWriter, r *http.Request, err error, bucketName, key string, opts object.GetObjectOptions) {
	var marker *domain.DeleteMarkerError
	if errors.As(err, &marker) {
		w.Header().Set(httpx.HeaderAmzDeleteMarker, "true")
		if marker.VersionID != "" {
			w.Header().Set(httpx.HeaderAmzVersionID, marker.VersionID)
		}
		if !marker.ModTime.IsZero() {
			w.Header().Set("Last-Modified", marker.ModTime.UTC().Format(http.TimeFormat))
		}
		if errors.Is(err, domain.ErrMethodNotAllowed) {
			w.Header().Set("Allow", "DELETE")
		}
		h.fail(w, r, err)
		return
	}
	switch {
	case errors.Is(err, domain.ErrNotModified):
		info, ierr := h.deps.Objects.GetObjectInfo(r.Context(), bucketName, key, opts)
		if ierr == nil || errors.Is(ierr, domain.ErrNotModified) {
			if info.ETag != "" {
				w.Header().Set("ETag", quoteETag(info.ETag))
			}
			if !info.ModTime.IsZero() {
				w.Header().Set("Last-Modified", info.ModTime.UTC().Format(http.TimeFormat))
			}
		}
	}
	h.fail(w, r, err)
}

func (h *objectRoutes) deleteObject(w http.ResponseWriter, r *http.Request) {
	bucketName, key := BucketName(r), ObjectKey(r)
	if !h.authz.authorize(w, r, bucketName, key) {
		return
	}
	versionID := r.URL.Query().Get("versionId")
	del, err := h.deps.Objects.DeleteObject(r.Context(), bucketName, key, object.DeleteObjectOptions{
		VersionID: versionID, BypassGovernance: h.authz.bypassGovernance(r, bucketName, key, versionID),
	})
	if err != nil {
		h.fail(w, r, err)
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

// deleteObjects removes up to 1000 objects. Every object is authorized on
// its own: a denied one is reported in the result, not as a failure of
// the request.
func (h *objectRoutes) deleteObjects(w http.ResponseWriter, r *http.Request) {
	bucketName := BucketName(r)
	var req DeleteRequest
	if err := decodeXMLBody(r, h.deps.Enc, maxXMLBody, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	if len(req.Objects) == 0 || len(req.Objects) > maxDeleteObjects {
		h.fail(w, r, domain.NewS3Error(domain.ErrMalformedXML, "MalformedXML", "The request must contain between 1 and 1000 objects to delete."))
		return
	}
	check := h.authz.prepare(r, bucketName)
	// The governance bypass is decided object by object
	// (s3:BypassGovernanceRetention on each key): the objects allowed it
	// and the others are deleted in two batches.
	batches := [2]object.DeleteObjectsInput{
		{Bucket: bucketName, Quiet: req.Quiet},
		{Bucket: bucketName, Quiet: req.Quiet, BypassGovernance: true},
	}
	var out DeleteResult
	for _, o := range req.Objects {
		if err := check(ActionOf("DeleteObject", o.VersionID != ""), o.Key); err != nil {
			code, msg := h.deps.Enc.MapS3Error(err)
			out.Errors = append(out.Errors, DeleteErrorEntry{Key: o.Key, VersionID: o.VersionID, Code: code.Code, Message: msg})
			continue
		}
		b := 0
		if o.VersionID != "" && h.authz.bypassGovernance(r, bucketName, o.Key, o.VersionID) {
			b = 1
		}
		batches[b].Objects = append(batches[b].Objects, object.ObjectToDelete{Key: o.Key, VersionID: o.VersionID})
	}
	for _, in := range batches {
		if len(in.Objects) == 0 {
			continue
		}
		res, err := h.deps.Objects.DeleteObjects(r.Context(), in)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		if !req.Quiet {
			for _, d := range res.Deleted {
				out.Deleted = append(out.Deleted, DeletedEntry{Key: d.Key, VersionID: d.VersionID, DeleteMarker: d.DeleteMarker, DeleteMarkerVersionID: d.DeleteMarkerVersionID})
			}
		}
		for _, e := range res.Errors {
			code, msg := h.deps.Enc.MapS3Error(e.Err)
			out.Errors = append(out.Errors, DeleteErrorEntry{Key: e.Key, VersionID: e.VersionID, Code: code.Code, Message: msg})
		}
	}
	h.deps.Enc.WriteXML(w, http.StatusOK, out)
}

func (h *objectRoutes) getTagging(w http.ResponseWriter, r *http.Request) {
	bucketName, key := BucketName(r), ObjectKey(r)
	if !h.authz.authorize(w, r, bucketName, key) {
		return
	}
	versionID := r.URL.Query().Get("versionId")
	tags, err := h.deps.Objects.GetObjectTags(r.Context(), bucketName, key, object.TagOptions{VersionID: versionID})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if versionID != "" {
		w.Header().Set(httpx.HeaderAmzVersionID, versionID)
	}
	h.deps.Enc.WriteXML(w, http.StatusOK, tagsToXML(tags))
}

// putTagging replaces the tags of a version. The tags of the body are the
// s3:RequestObjectTag/<key> and s3:RequestObjectTagKeys condition values
// of the request, as S3 evaluates PutObjectTagging: the body is read
// before the authorization.
func (h *objectRoutes) putTagging(w http.ResponseWriter, r *http.Request) {
	bucketName, key := BucketName(r), ObjectKey(r)
	versionID := r.URL.Query().Get("versionId")
	var doc taggingRequest
	if err := decodeXMLBody(r, h.deps.Enc, maxXMLBody, &doc); err != nil {
		if h.authz.authorizeVersion(w, r, ActionOf("PutObjectTagging", versionID != ""), bucketName, key, versionID, nil) {
			h.fail(w, r, err)
		}
		return
	}
	tags, err := tagsFromXML(doc.TagSet)
	if err != nil {
		if h.authz.authorizeVersion(w, r, ActionOf("PutObjectTagging", versionID != ""), bucketName, key, versionID, nil) {
			h.fail(w, r, err)
		}
		return
	}
	if !h.authz.authorizeVersion(w, r, ActionOf("PutObjectTagging", versionID != ""), bucketName, key, versionID, requestTagConditions(tags)) {
		return
	}
	info, err := h.deps.Objects.PutObjectTags(r.Context(), bucketName, key, tags, object.TagOptions{VersionID: versionID})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if info.VersionID != "" {
		w.Header().Set(httpx.HeaderAmzVersionID, info.VersionID)
	}
	w.WriteHeader(http.StatusOK)
}

func (h *objectRoutes) deleteTagging(w http.ResponseWriter, r *http.Request) {
	bucketName, key := BucketName(r), ObjectKey(r)
	if !h.authz.authorize(w, r, bucketName, key) {
		return
	}
	info, err := h.deps.Objects.DeleteObjectTags(r.Context(), bucketName, key, object.TagOptions{VersionID: r.URL.Query().Get("versionId")})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if info.VersionID != "" {
		w.Header().Set(httpx.HeaderAmzVersionID, info.VersionID)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *objectRoutes) getACL(w http.ResponseWriter, r *http.Request) {
	bucketName, key := BucketName(r), ObjectKey(r)
	if !h.authz.authorize(w, r, bucketName, key) {
		return
	}
	if _, err := h.deps.Objects.GetObjectInfo(r.Context(), bucketName, key, object.GetObjectOptions{VersionID: r.URL.Query().Get("versionId")}); err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteXML(w, http.StatusOK, cannedPrivateACL(ownerOf(r)))
}

func (h *objectRoutes) putACL(w http.ResponseWriter, r *http.Request) {
	bucketName, key := BucketName(r), ObjectKey(r)
	if !h.authz.authorize(w, r, bucketName, key) {
		return
	}
	if err := checkPrivateACL(r); err != nil {
		h.fail(w, r, err)
		return
	}
	if _, err := h.deps.Objects.GetObjectInfo(r.Context(), bucketName, key, object.GetObjectOptions{VersionID: r.URL.Query().Get("versionId")}); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// requestTagConditions returns the s3:RequestObjectTag/<key> and
// s3:RequestObjectTagKeys condition values of a tag set sent in a request
// body (PutObjectTagging); the x-amz-tagging header gives them for the
// uploads (policy.RequestConditions).
func requestTagConditions(tags map[string]string) map[string][]string {
	conds := make(map[string][]string, len(tags)+1)
	keys := make([]string, 0, len(tags))
	for k, v := range tags {
		conds[policy.KeyRequestObjectTagPrefix+k] = []string{v}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	conds[policy.KeyRequestObjectTagKeys] = keys
	return conds
}
