package s3

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// MultipartDeps are the dependencies of the multipart routes.
type MultipartDeps struct {
	Objects object.Service
	Buckets bucket.Service
	Policy  policy.Service
	Enc     httpx.Codec
}

type multipartRoutes struct {
	deps  MultipartDeps
	authz authorizer
}

var _ MultipartRoutes = (*multipartRoutes)(nil)

// NewMultipartRoutes returns the multipart route group.
func NewMultipartRoutes(deps MultipartDeps) MultipartRoutes {
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	return &multipartRoutes{deps: deps, authz: authorizer{policy: deps.Policy, buckets: deps.Buckets, enc: deps.Enc}}
}

func (h *multipartRoutes) operations() map[Operation]http.HandlerFunc {
	return map[Operation]http.HandlerFunc{
		{Name: "ListMultipartUploads", Method: "GET", Scope: ScopeBucket, Query: "uploads"}:      h.listUploads,
		{Name: "CreateMultipartUpload", Method: "POST", Scope: ScopeObject, Query: "uploads"}:    h.createUpload,
		{Name: "UploadPart", Method: "PUT", Scope: ScopeObject, Query: "uploadId"}:               h.uploadPart,
		{Name: "CompleteMultipartUpload", Method: "POST", Scope: ScopeObject, Query: "uploadId"}: h.completeUpload,
		{Name: "AbortMultipartUpload", Method: "DELETE", Scope: ScopeObject, Query: "uploadId"}:  h.abortUpload,
		{Name: "ListParts", Method: "GET", Scope: ScopeObject, Query: "uploadId"}:                h.listParts,
	}
}

// Mount implements Group.
func (h *multipartRoutes) Mount(d Dispatcher) {
	mountAll(d, h.deps.Enc, h.operations(), h.deps.Objects != nil)
}

// Register implements Routes.
func (h *multipartRoutes) Register(r chi.Router) { registerStandalone(r, h.deps.Enc, h) }

func (h *multipartRoutes) createUpload(w http.ResponseWriter, r *http.Request) {
	bucketName, key := BucketName(r), ObjectKey(r)
	if !h.authz.authorize(w, r, policy.ActionPutObject, bucketName, key) {
		return
	}
	tags, err := parseTaggingHeader(r.Header)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	up, err := h.deps.Objects.NewMultipartUpload(r.Context(), object.NewMultipartInput{
		Bucket: bucketName, Key: key, ContentType: r.Header.Get("Content-Type"), UserMetadata: userMetadata(r.Header),
		Tags: tags, StorageClass: r.Header.Get(httpx.HeaderAmzStorageClass),
		Checksum: domain.ChecksumAlgorithm(toUpper(r.Header.Get("x-amz-checksum-algorithm"))),
	})
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	h.deps.Enc.WriteXML(w, http.StatusOK, InitiateMultipartUploadResult{Bucket: up.Bucket, Key: up.Key, UploadID: up.UploadID})
}

// uploadPart stores a part; with x-amz-copy-source it is an UploadPartCopy.
func (h *multipartRoutes) uploadPart(w http.ResponseWriter, r *http.Request) {
	bucketName, key := BucketName(r), ObjectKey(r)
	if !h.authz.authorize(w, r, policy.ActionPutObject, bucketName, key) {
		return
	}
	q := r.URL.Query()
	partNumber, err := strconv.Atoi(q.Get("partNumber"))
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidArgument", "Part number must be an integer between 1 and 10000, inclusive"))
		return
	}
	uploadID := q.Get("uploadId")
	if src := r.Header.Get(httpx.HeaderAmzCopySource); src != "" {
		h.copyPart(w, r, bucketName, key, uploadID, partNumber, src)
		return
	}
	in, err := objectInput(r, bucketName, key)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	part, err := h.deps.Objects.PutObjectPart(r.Context(), object.PutObjectPartInput{
		Bucket: bucketName, Key: key, UploadID: uploadID, PartNumber: partNumber,
		Reader: in.Reader, Size: in.Size, MD5Hex: in.MD5Hex, Checksum: in.Checksum,
	})
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	w.Header().Set("ETag", quoteETag(part.ETag))
	if !part.Checksum.IsZero() && part.Checksum.Value != "" {
		w.Header().Set("x-amz-checksum-"+toLower(string(part.Checksum.Algorithm)), part.Checksum.Value)
	}
	w.WriteHeader(http.StatusOK)
}

func (h *multipartRoutes) copyPart(w http.ResponseWriter, r *http.Request, bucketName, key, uploadID string, partNumber int, src string) {
	srcBucket, srcKey, srcVersion, err := parseCopySource(src)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	if !h.authz.authorize(w, r, policy.ActionGetObject, srcBucket, srcKey) {
		return
	}
	var rg *domain.Range
	if v := r.Header.Get("x-amz-copy-source-range"); v != "" {
		if rg, err = domain.ParseRange(v); err != nil {
			h.deps.Enc.WriteS3Error(w, r, err)
			return
		}
	}
	part, err := h.deps.Objects.CopyObjectPart(r.Context(), object.CopyObjectPartInput{
		SrcBucket: srcBucket, SrcKey: srcKey, SrcVersionID: srcVersion, SrcRange: rg,
		SrcConditions: parseConditions(r.Header, "X-Amz-Copy-Source-If-"),
		DstBucket:     bucketName, DstKey: key, UploadID: uploadID, PartNumber: partNumber,
	})
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	if srcVersion != "" {
		w.Header().Set("x-amz-copy-source-version-id", srcVersion)
	}
	h.deps.Enc.WriteXML(w, http.StatusOK, CopyPartResult{ETag: quoteETag(part.ETag), LastModified: s3Time(part.ModTime)})
}

func (h *multipartRoutes) completeUpload(w http.ResponseWriter, r *http.Request) {
	bucketName, key := BucketName(r), ObjectKey(r)
	if !h.authz.authorize(w, r, policy.ActionPutObject, bucketName, key) {
		return
	}
	var req CompleteMultipartUploadRequest
	if err := h.deps.Enc.DecodeXML(r.Body, maxXMLBody, &req); err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	in := object.CompleteMultipartInput{Bucket: bucketName, Key: key, UploadID: r.URL.Query().Get("uploadId"), Conditions: parseConditions(r.Header, "If-")}
	for _, p := range req.Parts {
		cp := domain.CompletePart{Number: p.PartNumber, ETag: p.ETag}
		switch {
		case p.ChecksumCRC32 != "":
			cp.Checksum = domain.Checksum{Algorithm: domain.ChecksumCRC32, Value: p.ChecksumCRC32}
		case p.ChecksumCRC32C != "":
			cp.Checksum = domain.Checksum{Algorithm: domain.ChecksumCRC32C, Value: p.ChecksumCRC32C}
		case p.ChecksumSHA1 != "":
			cp.Checksum = domain.Checksum{Algorithm: domain.ChecksumSHA1, Value: p.ChecksumSHA1}
		case p.ChecksumSHA256 != "":
			cp.Checksum = domain.Checksum{Algorithm: domain.ChecksumSHA256, Value: p.ChecksumSHA256}
		}
		in.Parts = append(in.Parts, cp)
	}
	info, err := h.deps.Objects.CompleteMultipartUpload(r.Context(), in)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	if info.VersionID != "" {
		w.Header().Set(httpx.HeaderAmzVersionID, info.VersionID)
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	h.deps.Enc.WriteXML(w, http.StatusOK, CompleteMultipartUploadResult{
		Location: scheme + "://" + r.Host + "/" + bucketName + "/" + key, Bucket: bucketName, Key: key, ETag: quoteETag(info.ETag),
	})
}

func (h *multipartRoutes) abortUpload(w http.ResponseWriter, r *http.Request) {
	bucketName, key := BucketName(r), ObjectKey(r)
	if !h.authz.authorize(w, r, policy.ActionAbortMultipart, bucketName, key) {
		return
	}
	if err := h.deps.Objects.AbortMultipartUpload(r.Context(), bucketName, key, r.URL.Query().Get("uploadId")); err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *multipartRoutes) listParts(w http.ResponseWriter, r *http.Request) {
	bucketName, key := BucketName(r), ObjectKey(r)
	if !h.authz.authorize(w, r, policy.ActionListMultipartParts, bucketName, key) {
		return
	}
	q := r.URL.Query()
	maxParts, err := queryInt(q, "max-parts", 1000)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	marker, err := queryInt(q, "part-number-marker", 0)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	res, err := h.deps.Objects.ListObjectParts(r.Context(), domain.ListPartsInput{Bucket: bucketName, Key: key, UploadID: q.Get("uploadId"), PartNumberMarker: marker, MaxParts: maxParts})
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	owner := ownerOf(r)
	out := ListPartsResult{
		Bucket: bucketName, Key: key, UploadID: res.Upload.UploadID, Initiator: owner, Owner: owner,
		StorageClass: storageClassOrStandard(res.Upload.StorageClass), PartNumberMarker: marker,
		NextPartNumberMarker: res.NextPartNumberMarker, MaxParts: maxParts, IsTruncated: res.IsTruncated,
	}
	for _, p := range res.Parts {
		out.Parts = append(out.Parts, PartEntry{PartNumber: p.Number, LastModified: s3Time(p.ModTime), ETag: quoteETag(p.ETag), Size: p.Size})
	}
	h.deps.Enc.WriteXML(w, http.StatusOK, out)
}

func (h *multipartRoutes) listUploads(w http.ResponseWriter, r *http.Request) {
	bucketName := BucketName(r)
	if !h.authz.authorize(w, r, policy.ActionListMultipart, bucketName, "") {
		return
	}
	q := r.URL.Query()
	maxUploads, err := queryInt(q, "max-uploads", 1000)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	in := domain.ListMultipartUploadsInput{
		Bucket: bucketName, Prefix: q.Get("prefix"), Delimiter: q.Get("delimiter"),
		KeyMarker: q.Get("key-marker"), UploadIDMarker: q.Get("upload-id-marker"), MaxUploads: maxUploads,
	}
	res, err := h.deps.Objects.ListMultipartUploads(r.Context(), in)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	owner := ownerOf(r)
	out := ListMultipartUploadsResult{
		Bucket: bucketName, KeyMarker: in.KeyMarker, UploadIDMarker: in.UploadIDMarker, NextKeyMarker: res.NextKeyMarker,
		NextUploadIDMarker: res.NextUploadIDMarker, Prefix: in.Prefix, Delimiter: in.Delimiter, MaxUploads: maxUploads,
		IsTruncated: res.IsTruncated, CommonPrefixes: commonPrefixes(res.CommonPrefixes),
	}
	for _, u := range res.Uploads {
		out.Uploads = append(out.Uploads, UploadEntry{Key: u.Key, UploadID: u.UploadID, Initiator: owner, Owner: owner, StorageClass: storageClassOrStandard(u.StorageClass), Initiated: s3Time(u.Initiated)})
	}
	h.deps.Enc.WriteXML(w, http.StatusOK, out)
}

func storageClassOrStandard(sc string) string {
	if sc == "" {
		return domain.StorageClassStandard
	}
	return sc
}
