package s3

import (
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// MultipartDeps are the dependencies of the multipart routes. Objects and
// Policy (which resolves the bucket policies itself) are required: without
// them every operation answers NotImplemented. Clock and Logger default to
// the wall clock and slog.Default().
type MultipartDeps struct {
	Objects object.Service
	Policy  policy.Service
	Enc     httpx.Codec
	Clock   clock.Clock
	Logger  *slog.Logger
}

type multipartRoutes struct {
	deps  MultipartDeps
	opts  options
	authz authorizer
}

var _ MultipartRoutes = (*multipartRoutes)(nil)

// NewMultipartRoutes returns the multipart route group (WithInsecureSSEC).
func NewMultipartRoutes(deps MultipartDeps, opts ...Option) MultipartRoutes {
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &multipartRoutes{deps: deps, opts: newOptions(opts), authz: authorizer{policy: deps.Policy, enc: deps.Enc, clock: deps.Clock, objects: deps.Objects}}
}

func (h *multipartRoutes) operations() map[Operation]http.HandlerFunc {
	return map[Operation]http.HandlerFunc{
		{Name: "ListMultipartUploads", Method: http.MethodGet, Scope: ScopeBucket, Query: "uploads"}:      h.listUploads,
		{Name: "CreateMultipartUpload", Method: http.MethodPost, Scope: ScopeObject, Query: "uploads"}:    h.createUpload,
		{Name: "UploadPart", Method: http.MethodPut, Scope: ScopeObject, Query: "uploadId"}:               h.uploadPart,
		{Name: "CompleteMultipartUpload", Method: http.MethodPost, Scope: ScopeObject, Query: "uploadId"}: h.completeUpload,
		{Name: "AbortMultipartUpload", Method: http.MethodDelete, Scope: ScopeObject, Query: "uploadId"}:  h.abortUpload,
		{Name: "ListParts", Method: http.MethodGet, Scope: ScopeObject, Query: "uploadId"}:                h.listParts,
	}
}

// Mount implements Group.
func (h *multipartRoutes) Mount(d Dispatcher) {
	mountAll(d, h.deps.Enc, h.operations(), h.deps.Objects != nil && h.deps.Policy != nil)
}

// Register implements Routes.
func (h *multipartRoutes) Register(r chi.Router) { registerStandalone(r, h.deps.Enc, h) }

func (h *multipartRoutes) fail(w http.ResponseWriter, r *http.Request, err error) {
	writeError(h.deps.Enc, h.deps.Logger, w, r, err)
}

func (h *multipartRoutes) createUpload(w http.ResponseWriter, r *http.Request) {
	bucketName, key := BucketName(r), ObjectKey(r)
	if !h.authz.authorize(w, r, bucketName, key) {
		return
	}
	in, err := newMultipartInput(r, bucketName, key, h.opts.insecureSSEC)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if !h.authz.authorizeLockHeaders(w, r, bucketName, key, in.Retention, in.LegalHold) {
		return
	}
	up, err := h.deps.Objects.NewMultipartUpload(r.Context(), in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if up.Checksum != domain.ChecksumNone {
		w.Header().Set(httpx.HeaderAmzChecksumAlgorithm, string(up.Checksum))
	}
	if up.ChecksumType != domain.ChecksumTypeNone {
		w.Header().Set(headerAmzChecksumType, string(up.ChecksumType))
	}
	writeSSEHeaders(w, up.Encryption)
	h.deps.Enc.WriteXML(w, http.StatusOK, InitiateMultipartUploadResult{Bucket: bucketName, Key: key, UploadID: up.UploadID})
}

// newMultipartInput reads the CreateMultipartUpload headers.
func newMultipartInput(r *http.Request, bucketName, key string, allowInsecureSSEC bool) (object.NewMultipartInput, error) {
	if err := checkUnsupportedHeaders(r.Header); err != nil {
		return object.NewMultipartInput{}, err
	}
	meta, err := metadataFromHeaders(r.Header)
	if err != nil {
		return object.NewMultipartInput{}, err
	}
	tags, err := parseTaggingHeader(r.Header)
	if err != nil {
		return object.NewMultipartInput{}, err
	}
	storageClass := r.Header.Get(httpx.HeaderAmzStorageClass)
	if err := checkStorageClass(storageClass); err != nil {
		return object.NewMultipartInput{}, err
	}
	algo, err := checksumAlgorithmHeader(r.Header)
	if err != nil {
		return object.NewMultipartInput{}, err
	}
	typ, err := checksumTypeHeader(r.Header)
	if err != nil {
		return object.NewMultipartInput{}, err
	}
	retention, hold, err := lockFromHeaders(r.Header)
	if err != nil {
		return object.NewMultipartInput{}, err
	}
	sse, err := sseFromHeaders(r, allowInsecureSSEC)
	if err != nil {
		return object.NewMultipartInput{}, err
	}
	return object.NewMultipartInput{
		SSE:    sse,
		Bucket: bucketName, Key: key,
		ContentType: meta.contentType, ContentEncoding: meta.contentEncoding, ContentDisposition: meta.contentDisposition,
		ContentLanguage: meta.contentLanguage, CacheControl: meta.cacheControl, Expires: meta.expires,
		UserMetadata: meta.user, Tags: tags, StorageClass: storageClass,
		Checksum: algo, ChecksumType: typ, Retention: retention, LegalHold: hold,
	}, nil
}

// partNumber reads the partNumber query parameter (1-10000).
func partNumber(r *http.Request) (int, error) {
	n, err := strconv.Atoi(r.URL.Query().Get("partNumber"))
	if err != nil || n < minPartNumber || n > maxPartNumber {
		return 0, invalidArgument("Part number must be an integer between 1 and 10000, inclusive")
	}
	return n, nil
}

// uploadPart stores a part; with x-amz-copy-source it is an UploadPartCopy.
func (h *multipartRoutes) uploadPart(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(httpx.HeaderAmzCopySource) != "" {
		h.copyPart(w, r)
		return
	}
	bucketName, key := BucketName(r), ObjectKey(r)
	if !h.authz.authorize(w, r, bucketName, key) {
		return
	}
	number, err := partNumber(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	size, err := contentLength(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	md5Hex, err := contentMD5Hex(r.Header)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	checksum, err := checksumFromHeaders(r.Header)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	ckey, err := customerKeyFromHeaders(r, headerSSECustomerPrefix, h.opts.insecureSSEC)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	part, err := h.deps.Objects.PutObjectPart(r.Context(), object.PutObjectPartInput{
		Bucket: bucketName, Key: key, UploadID: r.URL.Query().Get("uploadId"), PartNumber: number,
		Reader: r.Body, Size: size, MD5Hex: md5Hex, SHA256Hex: unsignedPayloadSHA256(r), Checksum: checksum,
		SSECustomerKey: ckey,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("ETag", quoteETag(part.ETag))
	writeChecksum(w, part.Checksum)
	writeSSEHeaders(w, part.Encryption)
	w.WriteHeader(http.StatusOK)
}

func (h *multipartRoutes) copyPart(w http.ResponseWriter, r *http.Request) {
	httpx.SetOperationName(r.Context(), "UploadPartCopy")
	if err := requireSignedCopySource(r); err != nil {
		h.fail(w, r, err)
		return
	}
	bucketName, key := BucketName(r), ObjectKey(r)
	srcBucket, srcKey, srcVersion, err := parseCopySource(r.Header.Get(httpx.HeaderAmzCopySource))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if !h.authz.authorizeVersion(w, r, ActionOf("GetObject", srcVersion != ""), srcBucket, srcKey, srcVersion, nil) ||
		!h.authz.authorizeAction(w, r, policy.ActionPutObject, bucketName, key) {
		return
	}
	number, err := partNumber(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	rg, err := parseCopyRange(r.Header)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	srcCKey, err := customerKeyFromHeaders(r, headerCopySSECustomerPrefix, h.opts.insecureSSEC)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	ckey, err := customerKeyFromHeaders(r, headerSSECustomerPrefix, h.opts.insecureSSEC)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	part, err := h.deps.Objects.CopyObjectPart(r.Context(), object.CopyObjectPartInput{
		SrcBucket: srcBucket, SrcKey: srcKey, SrcVersionID: srcVersion, SrcRange: rg,
		SrcConditions: parseConditions(r.Header, httpx.HeaderAmzCopySourceIfPrefix),
		DstBucket:     bucketName, DstKey: key, UploadID: r.URL.Query().Get("uploadId"), PartNumber: number,
		SrcSSECustomerKey: srcCKey, SSECustomerKey: ckey,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if srcVersion != "" {
		w.Header().Set(httpx.HeaderAmzCopySourceVersionID, srcVersion)
	}
	writeSSEHeaders(w, part.Encryption)
	h.deps.Enc.WriteXML(w, http.StatusOK, CopyPartResult{ETag: quoteETag(part.ETag), LastModified: s3Time(part.ModTime), ChecksumFields: checksumFields(part.Checksum)})
}

func (h *multipartRoutes) completeUpload(w http.ResponseWriter, r *http.Request) {
	bucketName, key := BucketName(r), ObjectKey(r)
	if !h.authz.authorize(w, r, bucketName, key) {
		return
	}
	var req CompleteMultipartUploadRequest
	if err := decodeXMLBody(r, h.deps.Enc, maxCompleteBody, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	if len(req.Parts) == 0 {
		h.fail(w, r, domain.NewS3Error(domain.ErrMalformedXML, "MalformedXML", "You must specify at least one part"))
		return
	}
	checksum, err := checksumFromHeaders(r.Header)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	typ, err := checksumTypeHeader(r.Header)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	// The SSE-C key of an SSE-C upload opens the sealed checksums of its
	// parts (needed when the completion computes or verifies a checksum).
	ckey, err := customerKeyFromHeaders(r, headerSSECustomerPrefix, h.opts.insecureSSEC)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	size, err := objectSizeHeader(r.Header)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	in := object.CompleteMultipartInput{
		Bucket: bucketName, Key: key, UploadID: r.URL.Query().Get("uploadId"),
		Conditions: parseConditions(r.Header, "If-"), Checksum: checksum, ChecksumType: typ, SSECustomerKey: ckey,
		ObjectSize: size,
	}
	for _, p := range req.Parts {
		in.Parts = append(in.Parts, domain.CompletePart{Number: p.PartNumber, ETag: trimETag(p.ETag), Checksum: p.checksum()})
	}
	info, err := h.deps.Objects.CompleteMultipartUpload(r.Context(), in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	setWrittenVersion(w, info.VersionID)
	writeSSEHeaders(w, info.Encryption)
	out := CompleteMultipartUploadResult{Location: objectURL(r, bucketName, key), Bucket: bucketName, Key: key, ETag: quoteETag(info.ETag),
		ChecksumFields: checksumFields(info.Checksum)}
	if !info.Checksum.IsZero() {
		out.ChecksumType = string(info.Checksum.Type)
	}
	h.deps.Enc.WriteXML(w, http.StatusOK, out)
}

// headerMpObjectSize is the size of the object a completion expects.
const headerMpObjectSize = "x-amz-mp-object-size"

// objectSizeHeader returns the x-amz-mp-object-size of a completion (nil
// when absent): a non-negative number of bytes.
func objectSizeHeader(h http.Header) (*int64, error) {
	raw := h.Get(headerMpObjectSize)
	if raw == "" {
		return nil, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return nil, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidArgument",
			fmt.Sprintf("The value of the %s header must be a non-negative integer: %q", headerMpObjectSize, raw))
	}
	return &n, nil
}

func (h *multipartRoutes) abortUpload(w http.ResponseWriter, r *http.Request) {
	bucketName, key := BucketName(r), ObjectKey(r)
	if !h.authz.authorize(w, r, bucketName, key) {
		return
	}
	if err := h.deps.Objects.AbortMultipartUpload(r.Context(), bucketName, key, r.URL.Query().Get("uploadId")); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *multipartRoutes) listParts(w http.ResponseWriter, r *http.Request) {
	bucketName, key := BucketName(r), ObjectKey(r)
	if !h.authz.authorize(w, r, bucketName, key) {
		return
	}
	q := r.URL.Query()
	maxParts, err := queryLimit(q, "max-parts")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	marker, err := queryInt(q, "part-number-marker", 0)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	ckey, err := customerKeyFromHeaders(r, headerSSECustomerPrefix, h.opts.insecureSSEC)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	res, err := h.deps.Objects.ListObjectParts(r.Context(), domain.ListPartsInput{
		Bucket: bucketName, Key: key, UploadID: q.Get("uploadId"), PartNumberMarker: marker, MaxParts: serviceLimit(maxParts),
		SSECustomerKey: ckey,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	owner := ownerOf(r)
	out := ListPartsResult{
		Bucket: bucketName, Key: key, UploadID: q.Get("uploadId"), Initiator: owner, Owner: owner,
		StorageClass: storageClassOrStandard(res.Upload.StorageClass), PartNumberMarker: marker,
		NextPartNumberMarker: res.NextPartNumberMarker, MaxParts: maxParts, IsTruncated: res.IsTruncated,
		ChecksumAlgorithm: string(res.Upload.Checksum), ChecksumType: string(res.Upload.ChecksumType),
	}
	for _, p := range res.Parts {
		out.Parts = append(out.Parts, PartEntry{PartNumber: p.Number, LastModified: s3Time(p.ModTime), ETag: quoteETag(p.ETag), Size: p.Size,
			ChecksumFields: checksumFields(p.Checksum)})
	}
	h.deps.Enc.WriteXML(w, http.StatusOK, out)
}

func (h *multipartRoutes) listUploads(w http.ResponseWriter, r *http.Request) {
	bucketName := BucketName(r)
	if !h.authz.authorize(w, r, bucketName, "") {
		return
	}
	q := r.URL.Query()
	maxUploads, err := queryLimit(q, "max-uploads")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	enc, err := newListEncoder(q)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	in := domain.ListMultipartUploadsInput{
		Bucket: bucketName, Prefix: q.Get("prefix"), Delimiter: q.Get("delimiter"),
		KeyMarker: q.Get("key-marker"), UploadIDMarker: q.Get("upload-id-marker"), MaxUploads: serviceLimit(maxUploads),
	}
	res, err := h.deps.Objects.ListMultipartUploads(r.Context(), in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	owner := ownerOf(r)
	out := ListMultipartUploadsResult{
		Bucket: bucketName, KeyMarker: enc.Encode(in.KeyMarker), UploadIDMarker: in.UploadIDMarker,
		Prefix: enc.Encode(in.Prefix), Delimiter: enc.Encode(in.Delimiter), MaxUploads: maxUploads, EncodingType: enc.Type(),
		IsTruncated: res.IsTruncated, CommonPrefixes: commonPrefixes(res.CommonPrefixes, enc),
	}
	if res.IsTruncated {
		out.NextKeyMarker = enc.Encode(res.NextKeyMarker)
		out.NextUploadIDMarker = res.NextUploadIDMarker
	}
	for _, u := range res.Uploads {
		out.Uploads = append(out.Uploads, UploadEntry{
			Key: enc.Encode(u.Key), UploadID: u.UploadID, Initiator: owner, Owner: owner,
			StorageClass: storageClassOrStandard(u.StorageClass), Initiated: s3Time(u.Initiated),
		})
	}
	h.deps.Enc.WriteXML(w, http.StatusOK, out)
}
