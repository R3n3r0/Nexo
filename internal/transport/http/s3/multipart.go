package s3

import (
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
	authz authorizer
}

var _ MultipartRoutes = (*multipartRoutes)(nil)

// NewMultipartRoutes returns the multipart route group.
func NewMultipartRoutes(deps MultipartDeps, _ ...Option) MultipartRoutes {
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &multipartRoutes{deps: deps, authz: authorizer{policy: deps.Policy, enc: deps.Enc, clock: deps.Clock}}
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
	h.deps.Enc.WriteS3Error(w, r, err)
}

func (h *multipartRoutes) createUpload(w http.ResponseWriter, r *http.Request) {
	bucketName, key := BucketName(r), ObjectKey(r)
	if !h.authz.authorize(w, r, bucketName, key) {
		return
	}
	in, err := newMultipartInput(r, bucketName, key)
	if err != nil {
		h.fail(w, r, err)
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
	h.deps.Enc.WriteXML(w, http.StatusOK, InitiateMultipartUploadResult{Bucket: bucketName, Key: key, UploadID: up.UploadID})
}

// newMultipartInput reads the CreateMultipartUpload headers.
func newMultipartInput(r *http.Request, bucketName, key string) (object.NewMultipartInput, error) {
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
	return object.NewMultipartInput{
		Bucket: bucketName, Key: key,
		ContentType: meta.contentType, ContentEncoding: meta.contentEncoding, ContentDisposition: meta.contentDisposition,
		ContentLanguage: meta.contentLanguage, CacheControl: meta.cacheControl, Expires: meta.expires,
		UserMetadata: meta.user, Tags: tags, StorageClass: storageClass,
		Checksum: domain.ChecksumAlgorithm(strings.ToUpper(r.Header.Get(httpx.HeaderAmzChecksumAlgorithm))),
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
	part, err := h.deps.Objects.PutObjectPart(r.Context(), object.PutObjectPartInput{
		Bucket: bucketName, Key: key, UploadID: r.URL.Query().Get("uploadId"), PartNumber: number,
		Reader: r.Body, Size: size, MD5Hex: md5Hex, SHA256Hex: unsignedPayloadSHA256(r), Checksum: checksum,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("ETag", quoteETag(part.ETag))
	writeChecksum(w, part.Checksum)
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
	if !h.authz.authorizeAction(w, r, ActionOf("GetObject", srcVersion != ""), srcBucket, srcKey) ||
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
	part, err := h.deps.Objects.CopyObjectPart(r.Context(), object.CopyObjectPartInput{
		SrcBucket: srcBucket, SrcKey: srcKey, SrcVersionID: srcVersion, SrcRange: rg,
		SrcConditions: parseConditions(r.Header, httpx.HeaderAmzCopySourceIfPrefix),
		DstBucket:     bucketName, DstKey: key, UploadID: r.URL.Query().Get("uploadId"), PartNumber: number,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if srcVersion != "" {
		w.Header().Set(httpx.HeaderAmzCopySourceVersionID, srcVersion)
	}
	h.deps.Enc.WriteXML(w, http.StatusOK, CopyPartResult{ETag: quoteETag(part.ETag), LastModified: s3Time(part.ModTime)})
}

// completedPartChecksum returns the checksum declared for a part.
func completedPartChecksum(p CompletedPart) domain.Checksum {
	switch {
	case p.ChecksumCRC32 != "":
		return domain.Checksum{Algorithm: domain.ChecksumCRC32, Value: p.ChecksumCRC32}
	case p.ChecksumCRC32C != "":
		return domain.Checksum{Algorithm: domain.ChecksumCRC32C, Value: p.ChecksumCRC32C}
	case p.ChecksumSHA1 != "":
		return domain.Checksum{Algorithm: domain.ChecksumSHA1, Value: p.ChecksumSHA1}
	case p.ChecksumSHA256 != "":
		return domain.Checksum{Algorithm: domain.ChecksumSHA256, Value: p.ChecksumSHA256}
	default:
		return domain.Checksum{}
	}
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
	in := object.CompleteMultipartInput{
		Bucket: bucketName, Key: key, UploadID: r.URL.Query().Get("uploadId"),
		Conditions: parseConditions(r.Header, "If-"), Checksum: checksum,
	}
	for _, p := range req.Parts {
		in.Parts = append(in.Parts, domain.CompletePart{Number: p.PartNumber, ETag: trimETag(p.ETag), Checksum: completedPartChecksum(p)})
	}
	info, err := h.deps.Objects.CompleteMultipartUpload(r.Context(), in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if info.VersionID != "" {
		w.Header().Set(httpx.HeaderAmzVersionID, info.VersionID)
	}
	out := CompleteMultipartUploadResult{Location: objectURL(r, bucketName, key), Bucket: bucketName, Key: key, ETag: quoteETag(info.ETag)}
	switch info.Checksum.Algorithm {
	case domain.ChecksumCRC32:
		out.ChecksumCRC32 = info.Checksum.Value
	case domain.ChecksumCRC32C:
		out.ChecksumCRC32C = info.Checksum.Value
	case domain.ChecksumSHA1:
		out.ChecksumSHA1 = info.Checksum.Value
	case domain.ChecksumSHA256:
		out.ChecksumSHA256 = info.Checksum.Value
	}
	h.deps.Enc.WriteXML(w, http.StatusOK, out)
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
	res, err := h.deps.Objects.ListObjectParts(r.Context(), domain.ListPartsInput{
		Bucket: bucketName, Key: key, UploadID: q.Get("uploadId"), PartNumberMarker: marker, MaxParts: serviceLimit(maxParts),
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
		ChecksumAlgorithm: string(res.Upload.Checksum),
	}
	for _, p := range res.Parts {
		entry := PartEntry{PartNumber: p.Number, LastModified: s3Time(p.ModTime), ETag: quoteETag(p.ETag), Size: p.Size}
		switch p.Checksum.Algorithm {
		case domain.ChecksumCRC32:
			entry.ChecksumCRC32 = p.Checksum.Value
		case domain.ChecksumCRC32C:
			entry.ChecksumCRC32C = p.Checksum.Value
		case domain.ChecksumSHA1:
			entry.ChecksumSHA1 = p.Checksum.Value
		case domain.ChecksumSHA256:
			entry.ChecksumSHA256 = p.Checksum.Value
		}
		out.Parts = append(out.Parts, entry)
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
