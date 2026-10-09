package s3

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// defaultRegion is the S3 region reported when none is configured.
const defaultRegion = "us-east-1"

// BucketDeps are the dependencies of the bucket routes. Buckets, Objects
// and Policy are required: without them every operation answers
// NotImplemented (authorization never fails open). Clock and Logger
// default to the wall clock and slog.Default().
type BucketDeps struct {
	Buckets bucket.Service
	Objects object.Service
	Policy  policy.Service
	Enc     httpx.Codec
	Clock   clock.Clock
	Logger  *slog.Logger
}

type bucketRoutes struct {
	deps  BucketDeps
	opts  options
	authz authorizer
}

var _ BucketRoutes = (*bucketRoutes)(nil)

// NewBucketRoutes returns the bucket route group. WithRegion sets the
// region of the deployment.
func NewBucketRoutes(deps BucketDeps, opts ...Option) BucketRoutes {
	return newBucketRoutes(deps, opts...)
}

// newBucketRoutes builds the concrete group, which the presigned routes
// reuse for their listings.
func newBucketRoutes(deps BucketDeps, opts ...Option) *bucketRoutes {
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &bucketRoutes{
		deps:  deps,
		opts:  newOptions(opts),
		authz: authorizer{policy: deps.Policy, enc: deps.Enc, clock: deps.Clock, objects: deps.Objects},
	}
}

// available reports whether the services the handlers need are wired.
func (h *bucketRoutes) available() bool {
	return h.deps.Buckets != nil && h.deps.Objects != nil && h.deps.Policy != nil
}

// bucketOperations returns the bucket-level operations and their handlers.
func (h *bucketRoutes) bucketOperations() map[Operation]http.HandlerFunc {
	get, put, del := http.MethodGet, http.MethodPut, http.MethodDelete
	return map[Operation]http.HandlerFunc{
		{Name: "ListBuckets", Method: get, Scope: ScopeService}:                                       h.listBuckets,
		{Name: "CreateBucket", Method: put, Scope: ScopeBucket}:                                       h.createBucket,
		{Name: "HeadBucket", Method: http.MethodHead, Scope: ScopeBucket}:                             h.headBucket,
		{Name: "DeleteBucket", Method: del, Scope: ScopeBucket}:                                       h.deleteBucket,
		{Name: "ListObjects", Method: get, Scope: ScopeBucket}:                                        h.listObjects,
		{Name: "ListObjectsV2", Method: get, Scope: ScopeBucket, Query: "list-type", QueryValue: "2"}: h.listObjectsV2,
		{Name: "ListObjectVersions", Method: get, Scope: ScopeBucket, Query: "versions"}:              h.listObjectVersions,
		{Name: "GetBucketLocation", Method: get, Scope: ScopeBucket, Query: "location"}:               h.getLocation,
		{Name: "GetBucketPolicy", Method: get, Scope: ScopeBucket, Query: "policy"}:                   h.getPolicy,
		{Name: "PutBucketPolicy", Method: put, Scope: ScopeBucket, Query: "policy"}:                   h.putPolicy,
		{Name: "DeleteBucketPolicy", Method: del, Scope: ScopeBucket, Query: "policy"}:                h.deletePolicy,
		{Name: "GetBucketPolicyStatus", Method: get, Scope: ScopeBucket, Query: "policyStatus"}:       h.getPolicyStatus,
		{Name: "GetBucketVersioning", Method: get, Scope: ScopeBucket, Query: "versioning"}:           h.getVersioning,
		{Name: "PutBucketVersioning", Method: put, Scope: ScopeBucket, Query: "versioning"}:           h.putVersioning,
		{Name: "GetBucketTagging", Method: get, Scope: ScopeBucket, Query: "tagging"}:                 h.getTagging,
		{Name: "PutBucketTagging", Method: put, Scope: ScopeBucket, Query: "tagging"}:                 h.putTagging,
		{Name: "DeleteBucketTagging", Method: del, Scope: ScopeBucket, Query: "tagging"}:              h.deleteTagging,
		{Name: "GetBucketCors", Method: get, Scope: ScopeBucket, Query: "cors"}:                       h.rawGetter(h.getCORS),
		{Name: "DeleteBucketCors", Method: del, Scope: ScopeBucket, Query: "cors"}:                    h.deleter(h.deleteCORS),
		{Name: "GetObjectLockConfiguration", Method: get, Scope: ScopeBucket, Query: "object-lock"}:   h.getObjectLock,
		{Name: "PutObjectLockConfiguration", Method: put, Scope: ScopeBucket, Query: "object-lock"}:   h.putObjectLock,
		{Name: "PutBucketCors", Method: put, Scope: ScopeBucket, Query: "cors"}:                       h.putCORS,
		{Name: "GetBucketEncryption", Method: get, Scope: ScopeBucket, Query: "encryption"}:           h.getEncryption,
		{Name: "PutBucketEncryption", Method: put, Scope: ScopeBucket, Query: "encryption"}:           h.putEncryption,
		{Name: "DeleteBucketEncryption", Method: del, Scope: ScopeBucket, Query: "encryption"}:        h.deleter(h.deleteEncryption),
		{Name: "GetBucketAcl", Method: get, Scope: ScopeBucket, Query: "acl"}:                         h.getACL,
		{Name: "PutBucketAcl", Method: put, Scope: ScopeBucket, Query: "acl"}:                         h.putACL,
	}
}

// Mount implements Group. Without the services the operations are
// advertised but answer NotImplemented.
func (h *bucketRoutes) Mount(d Dispatcher) {
	mountAll(d, h.deps.Enc, h.bucketOperations(), h.available())
	mountNotImplemented(d, h.deps.Enc, unsupportedBucketOperations())
}

// Register implements Routes.
func (h *bucketRoutes) Register(r chi.Router) { registerStandalone(r, h.deps.Enc, h) }

func (h *bucketRoutes) fail(w http.ResponseWriter, r *http.Request, err error) {
	writeError(h.deps.Enc, h.deps.Logger, w, r, err)
}

// region is the region of the deployment.
func (h *bucketRoutes) region() string {
	if h.opts.region == "" {
		return defaultRegion
	}
	return h.opts.region
}

func (h *bucketRoutes) listBuckets(w http.ResponseWriter, r *http.Request) {
	if !h.authz.authorize(w, r, "", "") {
		return
	}
	q := r.URL.Query()
	in := bucket.ListPageInput{Prefix: q.Get("prefix"), ContinuationToken: q.Get("continuation-token")}
	if v := q.Get("max-buckets"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			h.fail(w, r, invalidArgument("max-buckets must be an integer between 1 and "+strconv.Itoa(bucket.MaxListBuckets)))
			return
		}
		in.MaxBuckets = n
	}
	page, err := h.deps.Buckets.ListPage(r.Context(), in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	res := ListAllMyBucketsResult{Owner: ownerOf(r), Prefix: in.Prefix, ContinuationToken: page.ContinuationToken}
	res.Buckets.Bucket = make([]BucketEntry, 0, len(page.Buckets))
	for _, b := range page.Buckets {
		res.Buckets.Bucket = append(res.Buckets.Bucket, BucketEntry{Name: b.Name, CreationDate: s3Time(b.Created), BucketRegion: b.Region})
	}
	h.deps.Enc.WriteXML(w, http.StatusOK, res)
}

func (h *bucketRoutes) createBucket(w http.ResponseWriter, r *http.Request) {
	name := BucketName(r)
	if !h.authz.authorize(w, r, name, "") {
		return
	}
	if err := checkUnsupportedHeaders(r.Header); err != nil {
		h.fail(w, r, err)
		return
	}
	in := bucket.CreateInput{Name: name}
	if v := strings.TrimSpace(r.Header.Get(httpx.HeaderAmzBucketObjectLock)); strings.EqualFold(v, "true") {
		// Object locking needs the right to configure it (and implies
		// versioning), as S3 requires.
		if !h.authz.authorizeAction(w, r, actionPutBucketObjectLock, name, "") {
			return
		}
		in.ObjectLock = true
	} else if v != "" && !strings.EqualFold(v, "false") {
		h.fail(w, r, invalidArgument("x-amz-bucket-object-lock-enabled must be true or false"))
		return
	}
	if r.ContentLength != 0 && r.Body != nil && r.Body != http.NoBody {
		var cfg CreateBucketConfiguration
		if err := decodeXMLBody(r, h.deps.Enc, maxXMLBody, &cfg); err != nil {
			h.fail(w, r, err)
			return
		}
		if err := h.checkLocation(cfg.LocationConstraint); err != nil {
			h.fail(w, r, err)
			return
		}
		in.Region = cfg.LocationConstraint
	}
	if _, err := h.deps.Buckets.Create(r.Context(), in); err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/"+name)
	w.WriteHeader(http.StatusOK)
}

// checkLocation accepts the location constraint of the deployment's
// region (any constraint when no region is configured).
func (h *bucketRoutes) checkLocation(constraint string) error {
	if constraint == "" || h.opts.region == "" || constraint == h.opts.region {
		return nil
	}
	return domain.NewS3Error(domain.ErrInvalidArgument, "InvalidLocationConstraint",
		"The specified location constraint "+constraint+" is not valid: this deployment serves "+h.opts.region)
}

func (h *bucketRoutes) headBucket(w http.ResponseWriter, r *http.Request) {
	name := BucketName(r)
	if !h.authz.authorize(w, r, name, "") {
		return
	}
	info, err := h.deps.Buckets.Get(r.Context(), name)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	region := info.Region
	if region == "" {
		region = h.region()
	}
	w.Header().Set(httpx.HeaderAmzBucketRegion, region)
	w.WriteHeader(http.StatusOK)
}

func (h *bucketRoutes) deleteBucket(w http.ResponseWriter, r *http.Request) {
	name := BucketName(r)
	if !h.authz.authorize(w, r, name, "") {
		return
	}
	force := r.Header.Get(headerForceDelete) == "true" || r.Header.Get(headerForceDeleteLegacy) == "true"
	if force && !h.authz.authorizeAction(w, r, actionForceDeleteBucket, name, "") {
		return
	}
	if err := h.deps.Buckets.Delete(r.Context(), name, bucket.DeleteOptions{Force: force}); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listParams are the parameters shared by the listings.
type listParams struct {
	in  domain.ListObjectsInput
	enc listEncoder
	// maxKeys is the max-keys echoed in the response (in.MaxKeys is the
	// service form, see serviceLimit).
	maxKeys int
}

// readListParams reads the common listing parameters.
func readListParams(r *http.Request) (listParams, error) {
	q := r.URL.Query()
	maxKeys, err := queryLimit(q, "max-keys")
	if err != nil {
		return listParams{}, err
	}
	enc, err := newListEncoder(q)
	if err != nil {
		return listParams{}, err
	}
	return listParams{
		in:      domain.ListObjectsInput{Bucket: BucketName(r), Prefix: q.Get("prefix"), Delimiter: q.Get("delimiter"), MaxKeys: serviceLimit(maxKeys)},
		enc:     enc,
		maxKeys: maxKeys,
	}, nil
}

func objectEntries(objects []domain.ObjectInfo, owner *Owner, enc listEncoder) []ObjectEntry {
	out := make([]ObjectEntry, 0, len(objects))
	for _, o := range objects {
		out = append(out, ObjectEntry{
			Key: enc.Encode(o.Key), LastModified: s3Time(o.ModTime), ETag: quoteETag(o.ETag), Size: o.Size,
			ChecksumAlgorithm: string(o.Checksum.Algorithm), ChecksumType: string(o.Checksum.Type),
			StorageClass: storageClassOrStandard(o.StorageClass), Owner: owner,
		})
	}
	return out
}

func commonPrefixes(prefixes []string, enc listEncoder) []CommonPrefix {
	out := make([]CommonPrefix, 0, len(prefixes))
	for _, p := range prefixes {
		out = append(out, CommonPrefix{Prefix: enc.Encode(p)})
	}
	return out
}

func (h *bucketRoutes) listObjects(w http.ResponseWriter, r *http.Request) {
	bucketName := BucketName(r)
	if !h.authz.authorize(w, r, bucketName, "") {
		return
	}
	p, err := readListParams(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	p.in.Marker = r.URL.Query().Get("marker")
	res, err := h.deps.Objects.ListObjects(r.Context(), p.in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	owner := ownerOf(r)
	// S3 leaves the Prefix of a V1 listing as sent, even with
	// encoding-type=url (the SDKs decode it in V2 listings only).
	out := ListBucketResultV1{
		Name: bucketName, Prefix: p.in.Prefix, Marker: p.enc.Encode(p.in.Marker), MaxKeys: p.maxKeys,
		Delimiter: p.enc.Encode(p.in.Delimiter), EncodingType: p.enc.Type(), IsTruncated: res.IsTruncated,
		Contents: objectEntries(res.Objects, &owner, p.enc), CommonPrefixes: commonPrefixes(res.CommonPrefixes, p.enc),
	}
	if res.IsTruncated {
		next := res.NextMarker
		if next == "" && len(res.Objects) > 0 {
			next = res.Objects[len(res.Objects)-1].Key
		}
		out.NextMarker = p.enc.Encode(next)
	}
	h.deps.Enc.WriteXML(w, http.StatusOK, out)
}

func (h *bucketRoutes) listObjectsV2(w http.ResponseWriter, r *http.Request) {
	bucketName := BucketName(r)
	if !h.authz.authorize(w, r, bucketName, "") {
		return
	}
	p, err := readListParams(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	q := r.URL.Query()
	p.in.ContinuationToken = q.Get("continuation-token")
	p.in.StartAfter = q.Get("start-after")
	p.in.FetchOwner = q.Get("fetch-owner") == "true"
	res, err := h.deps.Objects.ListObjects(r.Context(), p.in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var owner *Owner
	if p.in.FetchOwner {
		o := ownerOf(r)
		owner = &o
	}
	var token *string
	if q.Has("continuation-token") {
		token = &p.in.ContinuationToken
	}
	out := ListBucketResultV2{
		Name: bucketName, Prefix: p.enc.Encode(p.in.Prefix), StartAfter: p.enc.Encode(p.in.StartAfter),
		ContinuationToken: token, NextContinuationToken: res.NextContinuationToken, MaxKeys: p.maxKeys,
		Delimiter: p.enc.Encode(p.in.Delimiter), EncodingType: p.enc.Type(), IsTruncated: res.IsTruncated,
		Contents: objectEntries(res.Objects, owner, p.enc), CommonPrefixes: commonPrefixes(res.CommonPrefixes, p.enc),
	}
	out.KeyCount = len(out.Contents) + len(out.CommonPrefixes)
	h.deps.Enc.WriteXML(w, http.StatusOK, out)
}

func (h *bucketRoutes) listObjectVersions(w http.ResponseWriter, r *http.Request) {
	bucketName := BucketName(r)
	if !h.authz.authorize(w, r, bucketName, "") {
		return
	}
	p, err := readListParams(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	q := r.URL.Query()
	p.in.Marker = q.Get("key-marker")
	p.in.VersionIDMarker = q.Get("version-id-marker")
	p.in.IncludeVersions = true
	if p.in.VersionIDMarker != "" && p.in.Marker == "" {
		h.fail(w, r, invalidArgument("A version-id marker cannot be specified without a key marker."))
		return
	}
	res, err := h.deps.Objects.ListObjectVersions(r.Context(), p.in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	owner := ownerOf(r)
	out := ListVersionsResult{
		Name: bucketName, Prefix: p.enc.Encode(p.in.Prefix), KeyMarker: p.enc.Encode(p.in.Marker), VersionIDMarker: p.in.VersionIDMarker,
		MaxKeys: p.maxKeys, Delimiter: p.enc.Encode(p.in.Delimiter), EncodingType: p.enc.Type(), IsTruncated: res.IsTruncated,
		CommonPrefixes: commonPrefixes(res.CommonPrefixes, p.enc),
	}
	if res.IsTruncated {
		out.NextKeyMarker = p.enc.Encode(res.NextMarker)
		out.NextVersionIDMarker = res.NextVersionIDMarker
	}
	for _, o := range res.Objects {
		if o.IsDeleteMarker {
			out.DeleteMarkers = append(out.DeleteMarkers, DeleteMarkerEntry{
				Key: p.enc.Encode(o.Key), VersionID: versionIDOrNull(o.VersionID), IsLatest: o.IsLatest, LastModified: s3Time(o.ModTime), Owner: owner,
			})
			continue
		}
		out.Versions = append(out.Versions, VersionEntry{
			Key: p.enc.Encode(o.Key), VersionID: versionIDOrNull(o.VersionID), IsLatest: o.IsLatest, LastModified: s3Time(o.ModTime),
			ETag: quoteETag(o.ETag), ChecksumAlgorithm: string(o.Checksum.Algorithm), ChecksumType: string(o.Checksum.Type),
			Size: o.Size, StorageClass: storageClassOrStandard(o.StorageClass), Owner: owner,
		})
	}
	h.deps.Enc.WriteXML(w, http.StatusOK, out)
}

func (h *bucketRoutes) getLocation(w http.ResponseWriter, r *http.Request) {
	name := BucketName(r)
	if !h.authz.authorize(w, r, name, "") {
		return
	}
	info, err := h.deps.Buckets.Get(r.Context(), name)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	region := info.Region
	if region == "" {
		region = h.region()
	}
	if region == defaultRegion {
		region = "" // S3 reports the default region as an empty constraint
	}
	h.deps.Enc.WriteXML(w, http.StatusOK, LocationConstraint{Location: region})
}

func (h *bucketRoutes) getPolicy(w http.ResponseWriter, r *http.Request) {
	name := BucketName(r)
	if !h.authz.authorize(w, r, name, "") {
		return
	}
	raw, err := h.deps.Buckets.GetPolicy(r.Context(), name)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set(headerContentType, httpx.ContentTypeJSON)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

func (h *bucketRoutes) putPolicy(w http.ResponseWriter, r *http.Request) {
	name := BucketName(r)
	if !h.authz.authorize(w, r, name, "") {
		return
	}
	raw, err := readBody(r, maxPolicyBody)
	switch {
	case errors.Is(err, errBodyTooLarge):
		h.fail(w, r, domain.NewS3Error(domain.ErrInvalidArgument, "PolicyTooLarge", "Policy exceeds the maximum allowed document size."))
		return
	case err != nil:
		h.fail(w, r, err)
		return
	case len(raw) == 0:
		h.fail(w, r, domain.NewS3Error(domain.ErrInvalidArgument, "MalformedPolicy", "Policies must be valid JSON and the first byte must be '{'"))
		return
	}
	if err := h.deps.Buckets.SetPolicy(r.Context(), name, json.RawMessage(raw)); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *bucketRoutes) deletePolicy(w http.ResponseWriter, r *http.Request) {
	h.deleter(h.deps.Buckets.DeletePolicy)(w, r)
}

func (h *bucketRoutes) getPolicyStatus(w http.ResponseWriter, r *http.Request) {
	name := BucketName(r)
	if !h.authz.authorize(w, r, name, "") {
		return
	}
	isPublic, err := h.deps.Buckets.IsPublic(r.Context(), name)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteXML(w, http.StatusOK, PolicyStatus{IsPublic: isPublic})
}

func (h *bucketRoutes) getVersioning(w http.ResponseWriter, r *http.Request) {
	name := BucketName(r)
	if !h.authz.authorize(w, r, name, "") {
		return
	}
	v, err := h.deps.Buckets.GetVersioning(r.Context(), name)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteXML(w, http.StatusOK, VersioningConfiguration{Status: v.String()})
}

func (h *bucketRoutes) putVersioning(w http.ResponseWriter, r *http.Request) {
	name := BucketName(r)
	if !h.authz.authorize(w, r, name, "") {
		return
	}
	var cfg versioningRequest
	if err := decodeXMLBody(r, h.deps.Enc, maxXMLBody, &cfg); err != nil {
		h.fail(w, r, err)
		return
	}
	if cfg.MFADelete == "Enabled" {
		h.fail(w, r, notImplemented("MFA delete is not supported"))
		return
	}
	v, err := domain.ParseVersioning(cfg.Status)
	if err != nil || v == domain.VersioningOff {
		h.fail(w, r, domain.NewS3Error(domain.ErrMalformedXML, "IllegalVersioningConfigurationException", "The Versioning element must be Enabled or Suspended"))
		return
	}
	if err := h.deps.Buckets.SetVersioning(r.Context(), name, v); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *bucketRoutes) getTagging(w http.ResponseWriter, r *http.Request) {
	name := BucketName(r)
	if !h.authz.authorize(w, r, name, "") {
		return
	}
	tags, err := h.deps.Buckets.GetTags(r.Context(), name)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteXML(w, http.StatusOK, tagsToXML(tags))
}

func (h *bucketRoutes) putTagging(w http.ResponseWriter, r *http.Request) {
	name := BucketName(r)
	if !h.authz.authorize(w, r, name, "") {
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
	if err := h.deps.Buckets.SetTags(r.Context(), name, tags); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *bucketRoutes) deleteTagging(w http.ResponseWriter, r *http.Request) {
	h.deleter(h.deps.Buckets.DeleteTags)(w, r)
}

// The sub-resource accessors resolve the bucket service when the request
// is served (the group is built before its services may be checked).
func (h *bucketRoutes) getCORS(ctx context.Context, name string) ([]byte, error) {
	return h.deps.Buckets.GetCORS(ctx, name)
}

func (h *bucketRoutes) deleteCORS(ctx context.Context, name string) error {
	return h.deps.Buckets.DeleteCORS(ctx, name)
}

func (h *bucketRoutes) deleteEncryption(ctx context.Context, name string) error {
	return h.deps.Buckets.DeleteEncryption(ctx, name)
}

// rawGetter serves a stored XML configuration document.
func (h *bucketRoutes) rawGetter(get func(ctx context.Context, name string) ([]byte, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := BucketName(r)
		if !h.authz.authorize(w, r, name, "") {
			return
		}
		doc, err := get(r.Context(), name)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		w.Header().Set(headerContentType, httpx.ContentTypeXML)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(doc)
	}
}

// deleter removes a bucket sub-resource.
func (h *bucketRoutes) deleter(del func(ctx context.Context, name string) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := BucketName(r)
		if !h.authz.authorize(w, r, name, "") {
			return
		}
		if err := del(r.Context(), name); err != nil {
			h.fail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (h *bucketRoutes) getObjectLock(w http.ResponseWriter, r *http.Request) {
	name := BucketName(r)
	if !h.authz.authorize(w, r, name, "") {
		return
	}
	cfg, err := h.deps.Buckets.GetObjectLock(r.Context(), name)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := ObjectLockConfiguration{ObjectLockEnabled: "Enabled"}
	if cfg.Mode != "" {
		out.Rule = &ObjectLockRule{DefaultRetention: DefaultRetention{Mode: string(cfg.Mode), Days: cfg.Days, Years: cfg.Years}}
	}
	h.deps.Enc.WriteXML(w, http.StatusOK, out)
}

func (h *bucketRoutes) getEncryption(w http.ResponseWriter, r *http.Request) {
	name := BucketName(r)
	if !h.authz.authorize(w, r, name, "") {
		return
	}
	cfg, err := h.deps.Buckets.GetEncryption(r.Context(), name)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := ServerSideEncryptionConfiguration{Rules: []EncryptionRule{{
		ApplyServerSideEncryptionByDefault: EncryptionByDefault{SSEAlgorithm: cfg.Algorithm, KMSMasterKeyID: cfg.KMSKeyID},
	}}}
	h.deps.Enc.WriteXML(w, http.StatusOK, out)
}

// putEncryption sets the default encryption of a bucket (S-BKT-11): one
// rule, SSE-S3 (AES256) or SSE-KMS (aws:kms, with an optional key).
func (h *bucketRoutes) putEncryption(w http.ResponseWriter, r *http.Request) {
	name := BucketName(r)
	if !h.authz.authorize(w, r, name, "") {
		return
	}
	var req encryptionRequest
	if err := decodeXMLBody(r, h.deps.Enc, maxXMLBody, &req); err != nil {
		h.fail(w, r, err)
		return
	}
	if len(req.Rules) != 1 || req.Rules[0].ApplyServerSideEncryptionByDefault == nil {
		h.fail(w, r, domain.NewS3Error(domain.ErrMalformedXML, "MalformedXML",
			"The XML you provided was not well-formed or did not validate against our published schema: one Rule with ApplyServerSideEncryptionByDefault is required."))
		return
	}
	def := req.Rules[0].ApplyServerSideEncryptionByDefault
	cfg := bucket.EncryptionConfig{Algorithm: strings.TrimSpace(def.SSEAlgorithm), KMSKeyID: kmsKeyID(def.KMSMasterKeyID)}
	if err := h.deps.Buckets.SetEncryption(r.Context(), name, cfg); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// cannedPrivateACL is the only ACL Nexo exposes: full control to the owner.
func cannedPrivateACL(owner Owner) AccessControlPolicy {
	var acl AccessControlPolicy
	acl.Owner = owner
	acl.AccessControlList.Grants = []Grant{{
		Grantee: Grantee{
			XMLNS: "http://www.w3.org/2001/XMLSchema-instance", Type: "CanonicalUser",
			ID: owner.ID, DisplayName: owner.DisplayName,
		},
		Permission: "FULL_CONTROL",
	}}
	return acl
}

// checkPrivateACL accepts a PutAcl that only restates the private canned
// ACL (access is governed by policies): no body, no grant, x-amz-acl
// private or absent.
func checkPrivateACL(r *http.Request) error {
	if err := checkUnsupportedHeaders(r.Header); err != nil {
		return err
	}
	if r.ContentLength > 0 {
		return notImplemented("Access control lists are not supported: use bucket policies or anonymous access rules")
	}
	return nil
}

func (h *bucketRoutes) getACL(w http.ResponseWriter, r *http.Request) {
	name := BucketName(r)
	if !h.authz.authorize(w, r, name, "") {
		return
	}
	if _, err := h.deps.Buckets.Get(r.Context(), name); err != nil {
		h.fail(w, r, err)
		return
	}
	h.deps.Enc.WriteXML(w, http.StatusOK, cannedPrivateACL(ownerOf(r)))
}

func (h *bucketRoutes) putACL(w http.ResponseWriter, r *http.Request) {
	name := BucketName(r)
	if !h.authz.authorize(w, r, name, "") {
		return
	}
	if err := checkPrivateACL(r); err != nil {
		h.fail(w, r, err)
		return
	}
	if _, err := h.deps.Buckets.Get(r.Context(), name); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}
