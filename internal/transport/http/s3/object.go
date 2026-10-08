package s3

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

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
	authz authorizer
}

var _ ObjectRoutes = (*objectRoutes)(nil)

// NewObjectRoutes returns the object route group.
func NewObjectRoutes(deps ObjectDeps, _ ...Option) ObjectRoutes { return newObjectRoutes(deps) }

// newObjectRoutes builds the concrete group, which the presigned routes
// reuse for the object operations.
func newObjectRoutes(deps ObjectDeps) *objectRoutes {
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &objectRoutes{deps: deps, authz: authorizer{policy: deps.Policy, enc: deps.Enc, clock: deps.Clock}}
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
func putInput(r *http.Request, bucketName, key string) (object.PutObjectInput, error) {
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
	storageClass := r.Header.Get(httpx.HeaderAmzStorageClass)
	if err := checkStorageClass(storageClass); err != nil {
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
	}, nil
}

// writeChecksum echoes the checksum computed for a write.
func writeChecksum(w http.ResponseWriter, c domain.Checksum) {
	if c.Value != "" && c.Algorithm != domain.ChecksumNone {
		w.Header().Set(httpx.HeaderAmzChecksumPrefix+strings.ToLower(string(c.Algorithm)), c.Value)
	}
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
	in, err := putInput(r, bucketName, key)
	if err != nil {
		h.fail(w, r, err)
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
	if !h.authz.authorizeAction(w, r, readAction, srcBucket, srcKey) ||
		!h.authz.authorizeAction(w, r, policy.ActionPutObject, dstBucket, dstKey) {
		return
	}
	in, err := copyInput(r)
	if err != nil {
		h.fail(w, r, err)
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
	h.deps.Enc.WriteXML(w, http.StatusOK, CopyObjectResult{ETag: quoteETag(info.ETag), LastModified: s3Time(info.ModTime)})
}

// copyInput reads the CopyObject headers (source and destination are
// filled in by the caller).
func copyInput(r *http.Request) (object.CopyObjectInput, error) {
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
	in := object.CopyObjectInput{
		SrcConditions:   parseConditions(r.Header, httpx.HeaderAmzCopySourceIfPrefix),
		ReplaceMetadata: metaDirective == directiveReplace,
		ReplaceTags:     tagDirective == directiveReplace,
		StorageClass:    storageClass,
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
func getOptions(r *http.Request) (object.GetObjectOptions, error) {
	q := r.URL.Query()
	opts := object.GetObjectOptions{VersionID: q.Get("versionId"), Range: parseRange(r.Header), Conditions: parseConditions(r.Header, "If-")}
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
	opts, err := getOptions(r)
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
	writeObjectHeaders(w, r, rd.Info, !partial)
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
	opts, err := getOptions(r)
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
	writeObjectHeaders(w, r, info, !partial)
	applyResponseOverrides(w, r.URL.Query())
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	if partial {
		w.Header().Set("Content-Range", domain.ContentRange(offset, length, info.Size))
	}
}

// writeReadError writes the error of a read: a 304 carries the ETag and
// Last-Modified of the object, a delete marker the x-amz-delete-marker
// header.
func (h *objectRoutes) writeReadError(w http.ResponseWriter, r *http.Request, err error, bucketName, key string, opts object.GetObjectOptions) {
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
	case errors.Is(err, domain.ErrMethodNotAllowed), errors.Is(err, domain.ErrObjectNotFound) && isMarkerError(err):
		w.Header().Set(httpx.HeaderAmzDeleteMarker, "true")
	}
	h.fail(w, r, err)
}

// isMarkerError recognizes the NoSuchKey raised by a latest delete marker
// (the object layer reports it with an explicit S3 code).
func isMarkerError(err error) bool {
	var s3err *domain.S3Error
	return errors.As(err, &s3err) && s3err.Code == "NoSuchKey"
}

func (h *objectRoutes) deleteObject(w http.ResponseWriter, r *http.Request) {
	bucketName, key := BucketName(r), ObjectKey(r)
	if !h.authz.authorize(w, r, bucketName, key) {
		return
	}
	del, err := h.deps.Objects.DeleteObject(r.Context(), bucketName, key, object.DeleteObjectOptions{
		VersionID: r.URL.Query().Get("versionId"), BypassGovernance: r.Header.Get(httpx.HeaderAmzBypassGovernance) == "true",
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
	in := object.DeleteObjectsInput{Bucket: bucketName, Quiet: req.Quiet, BypassGovernance: r.Header.Get(httpx.HeaderAmzBypassGovernance) == "true"}
	var out DeleteResult
	for _, o := range req.Objects {
		if err := check(ActionOf("DeleteObject", o.VersionID != ""), o.Key); err != nil {
			code, msg := h.deps.Enc.MapS3Error(err)
			out.Errors = append(out.Errors, DeleteErrorEntry{Key: o.Key, VersionID: o.VersionID, Code: code.Code, Message: msg})
			continue
		}
		in.Objects = append(in.Objects, object.ObjectToDelete{Key: o.Key, VersionID: o.VersionID})
	}
	if len(in.Objects) > 0 {
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

func (h *objectRoutes) putTagging(w http.ResponseWriter, r *http.Request) {
	bucketName, key := BucketName(r), ObjectKey(r)
	if !h.authz.authorize(w, r, bucketName, key) {
		return
	}
	var doc taggingRequest
	if err := decodeXMLBody(r, h.deps.Enc, maxXMLBody, &doc); err != nil {
		h.fail(w, r, err)
		return
	}
	tags, err := tagsFromXML(doc.TagSet)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	info, err := h.deps.Objects.PutObjectTags(r.Context(), bucketName, key, tags, object.TagOptions{VersionID: r.URL.Query().Get("versionId")})
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
