package s3

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// BucketDeps are the dependencies of the bucket routes. Policy is optional:
// without it no authorization is performed (tests).
type BucketDeps struct {
	Buckets bucket.Service
	Objects object.Service
	Policy  policy.Service
	Enc     httpx.Codec
}

type bucketRoutes struct {
	deps  BucketDeps
	authz authorizer
}

var _ BucketRoutes = (*bucketRoutes)(nil)

// NewBucketRoutes returns the bucket route group.
func NewBucketRoutes(deps BucketDeps) BucketRoutes {
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	return &bucketRoutes{deps: deps, authz: authorizer{policy: deps.Policy, buckets: deps.Buckets, enc: deps.Enc}}
}

// bucketOperations returns the bucket-level operations and their handlers.
func (h *bucketRoutes) bucketOperations() map[Operation]http.HandlerFunc {
	svc := h.deps.Buckets
	if svc == nil {
		svc = noBuckets{}
	}
	return map[Operation]http.HandlerFunc{
		{Name: "ListBuckets", Method: "GET", Scope: ScopeService}:                                       h.listBuckets,
		{Name: "CreateBucket", Method: "PUT", Scope: ScopeBucket}:                                       h.createBucket,
		{Name: "HeadBucket", Method: "HEAD", Scope: ScopeBucket}:                                        h.headBucket,
		{Name: "DeleteBucket", Method: "DELETE", Scope: ScopeBucket}:                                    h.deleteBucket,
		{Name: "ListObjects", Method: "GET", Scope: ScopeBucket}:                                        h.listObjects,
		{Name: "ListObjectsV2", Method: "GET", Scope: ScopeBucket, Query: "list-type", QueryValue: "2"}: h.listObjectsV2,
		{Name: "ListObjectVersions", Method: "GET", Scope: ScopeBucket, Query: "versions"}:              h.listObjectVersions,
		{Name: "GetBucketLocation", Method: "GET", Scope: ScopeBucket, Query: "location"}:               h.getLocation,
		{Name: "GetBucketPolicy", Method: "GET", Scope: ScopeBucket, Query: "policy"}:                   h.getPolicy,
		{Name: "PutBucketPolicy", Method: "PUT", Scope: ScopeBucket, Query: "policy"}:                   h.putPolicy,
		{Name: "DeleteBucketPolicy", Method: "DELETE", Scope: ScopeBucket, Query: "policy"}:             h.deletePolicy,
		{Name: "GetBucketPolicyStatus", Method: "GET", Scope: ScopeBucket, Query: "policyStatus"}:       h.getPolicyStatus,
		{Name: "GetBucketVersioning", Method: "GET", Scope: ScopeBucket, Query: "versioning"}:           h.getVersioning,
		{Name: "PutBucketVersioning", Method: "PUT", Scope: ScopeBucket, Query: "versioning"}:           h.putVersioning,
		{Name: "GetBucketTagging", Method: "GET", Scope: ScopeBucket, Query: "tagging"}:                 h.getTagging,
		{Name: "PutBucketTagging", Method: "PUT", Scope: ScopeBucket, Query: "tagging"}:                 h.putTagging,
		{Name: "DeleteBucketTagging", Method: "DELETE", Scope: ScopeBucket, Query: "tagging"}:           h.deleteTagging,
		{Name: "GetBucketCors", Method: "GET", Scope: ScopeBucket, Query: "cors"}:                       h.rawGetter("s3:GetBucketCORS", svc.GetCORS),
		{Name: "PutBucketCors", Method: "PUT", Scope: ScopeBucket, Query: "cors"}:                       h.rawSetter("s3:PutBucketCORS", svc.SetCORS),
		{Name: "DeleteBucketCors", Method: "DELETE", Scope: ScopeBucket, Query: "cors"}:                 h.deleter("s3:PutBucketCORS", svc.DeleteCORS),
		{Name: "GetBucketLifecycle", Method: "GET", Scope: ScopeBucket, Query: "lifecycle"}:             h.rawGetter("s3:GetLifecycleConfiguration", svc.GetLifecycle),
		{Name: "PutBucketLifecycle", Method: "PUT", Scope: ScopeBucket, Query: "lifecycle"}:             h.rawSetter("s3:PutLifecycleConfiguration", svc.SetLifecycle),
		{Name: "DeleteBucketLifecycle", Method: "DELETE", Scope: ScopeBucket, Query: "lifecycle"}:       h.deleter("s3:PutLifecycleConfiguration", svc.DeleteLifecycle),
		{Name: "GetObjectLockConfiguration", Method: "GET", Scope: ScopeBucket, Query: "object-lock"}:   h.getObjectLock,
		{Name: "PutObjectLockConfiguration", Method: "PUT", Scope: ScopeBucket, Query: "object-lock"}:   h.putObjectLock,
		{Name: "GetBucketEncryption", Method: "GET", Scope: ScopeBucket, Query: "encryption"}:           h.getEncryption,
		{Name: "PutBucketEncryption", Method: "PUT", Scope: ScopeBucket, Query: "encryption"}:           h.putEncryption,
		{Name: "DeleteBucketEncryption", Method: "DELETE", Scope: ScopeBucket, Query: "encryption"}:     h.deleter("s3:PutEncryptionConfiguration", svc.DeleteEncryption),
		{Name: "GetBucketNotification", Method: "GET", Scope: ScopeBucket, Query: "notification"}:       h.rawGetter("s3:GetBucketNotification", svc.GetNotification),
		{Name: "PutBucketNotification", Method: "PUT", Scope: ScopeBucket, Query: "notification"}:       h.rawSetter("s3:PutBucketNotification", svc.SetNotification),
		{Name: "GetBucketAcl", Method: "GET", Scope: ScopeBucket, Query: "acl"}:                         h.getACL,
		{Name: "PutBucketAcl", Method: "PUT", Scope: ScopeBucket, Query: "acl"}:                         h.putACL,
	}
}

// Mount implements Group. Without the services the operations are
// advertised but answer NotImplemented.
func (h *bucketRoutes) Mount(d Dispatcher) {
	mountAll(d, h.deps.Enc, h.bucketOperations(), h.deps.Buckets != nil && h.deps.Objects != nil)
}

// Register implements Routes.
func (h *bucketRoutes) Register(r chi.Router) { registerStandalone(r, h.deps.Enc, h) }

func (h *bucketRoutes) listBuckets(w http.ResponseWriter, r *http.Request) {
	if !h.authz.authorize(w, r, policy.ActionListAllMyBuckets, "", "") {
		return
	}
	list, err := h.deps.Buckets.List(r.Context(), bucket.ListInput{})
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	res := ListAllMyBucketsResult{Owner: ownerOf(r)}
	res.Buckets.Bucket = make([]BucketEntry, 0, len(list))
	for _, b := range list {
		res.Buckets.Bucket = append(res.Buckets.Bucket, BucketEntry{Name: b.Name, CreationDate: s3Time(b.Created), BucketRegion: b.Region})
	}
	h.deps.Enc.WriteXML(w, http.StatusOK, res)
}

func (h *bucketRoutes) createBucket(w http.ResponseWriter, r *http.Request) {
	name := BucketName(r)
	if !h.authz.authorize(w, r, policy.ActionCreateBucket, name, "") {
		return
	}
	in := bucket.CreateInput{Name: name, ObjectLock: r.Header.Get("x-amz-bucket-object-lock-enabled") == "true"}
	if r.ContentLength != 0 && r.Body != nil && r.Body != http.NoBody {
		var cfg CreateBucketConfiguration
		if err := h.deps.Enc.DecodeXML(r.Body, maxXMLBody, &cfg); err != nil {
			h.deps.Enc.WriteS3Error(w, r, err)
			return
		}
		in.Region = cfg.LocationConstraint
	}
	if _, err := h.deps.Buckets.Create(r.Context(), in); err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	w.Header().Set("Location", "/"+name)
	w.WriteHeader(http.StatusOK)
}

func (h *bucketRoutes) headBucket(w http.ResponseWriter, r *http.Request) {
	name := BucketName(r)
	if !h.authz.authorize(w, r, policy.ActionListBucket, name, "") {
		return
	}
	info, err := h.deps.Buckets.Get(r.Context(), name)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	w.Header().Set(httpx.HeaderAmzBucketRegion, info.Region)
	w.WriteHeader(http.StatusOK)
}

func (h *bucketRoutes) deleteBucket(w http.ResponseWriter, r *http.Request) {
	name := BucketName(r)
	if !h.authz.authorize(w, r, policy.ActionDeleteBucket, name, "") {
		return
	}
	if err := h.deps.Buckets.Delete(r.Context(), name, bucket.DeleteOptions{Force: r.Header.Get("x-amz-force-delete") == "true"}); err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listInput reads the common listing parameters.
func listInput(r *http.Request) (domain.ListObjectsInput, error) {
	q := r.URL.Query()
	maxKeys, err := queryInt(q, "max-keys", 1000)
	if err != nil {
		return domain.ListObjectsInput{}, err
	}
	if enc := q.Get("encoding-type"); enc != "" && enc != "url" {
		return domain.ListObjectsInput{}, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidArgument", "Invalid Encoding Method specified in Request")
	}
	return domain.ListObjectsInput{
		Bucket:    BucketName(r),
		Prefix:    q.Get("prefix"),
		Delimiter: q.Get("delimiter"),
		MaxKeys:   maxKeys,
	}, nil
}

func objectEntries(objects []domain.ObjectInfo, owner *Owner) []ObjectEntry {
	out := make([]ObjectEntry, 0, len(objects))
	for _, o := range objects {
		out = append(out, ObjectEntry{Key: o.Key, LastModified: s3Time(o.ModTime), ETag: quoteETag(o.ETag), Size: o.Size, StorageClass: o.StorageClass, Owner: owner})
	}
	return out
}

func commonPrefixes(prefixes []string) []CommonPrefix {
	out := make([]CommonPrefix, 0, len(prefixes))
	for _, p := range prefixes {
		out = append(out, CommonPrefix{Prefix: p})
	}
	return out
}

func (h *bucketRoutes) listObjects(w http.ResponseWriter, r *http.Request) {
	in, err := listInput(r)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	if !h.authz.authorize(w, r, policy.ActionListBucket, in.Bucket, "") {
		return
	}
	in.Marker = r.URL.Query().Get("marker")
	res, err := h.deps.Objects.ListObjects(r.Context(), in)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	owner := ownerOf(r)
	out := ListBucketResultV1{
		Name: in.Bucket, Prefix: in.Prefix, Marker: in.Marker, MaxKeys: in.MaxKeys, Delimiter: in.Delimiter,
		IsTruncated: res.IsTruncated, Contents: objectEntries(res.Objects, &owner), CommonPrefixes: commonPrefixes(res.CommonPrefixes),
	}
	if res.IsTruncated && in.Delimiter != "" {
		out.NextMarker = res.NextMarker
	}
	h.deps.Enc.WriteXML(w, http.StatusOK, out)
}

func (h *bucketRoutes) listObjectsV2(w http.ResponseWriter, r *http.Request) {
	in, err := listInput(r)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	if !h.authz.authorize(w, r, policy.ActionListBucket, in.Bucket, "") {
		return
	}
	q := r.URL.Query()
	in.ContinuationToken = q.Get("continuation-token")
	in.StartAfter = q.Get("start-after")
	in.FetchOwner = q.Get("fetch-owner") == "true"
	res, err := h.deps.Objects.ListObjects(r.Context(), in)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	var owner *Owner
	if in.FetchOwner {
		o := ownerOf(r)
		owner = &o
	}
	out := ListBucketResultV2{
		Name: in.Bucket, Prefix: in.Prefix, StartAfter: in.StartAfter, ContinuationToken: in.ContinuationToken,
		NextContinuationToken: res.NextContinuationToken, MaxKeys: in.MaxKeys, Delimiter: in.Delimiter,
		IsTruncated: res.IsTruncated, Contents: objectEntries(res.Objects, owner), CommonPrefixes: commonPrefixes(res.CommonPrefixes),
	}
	out.KeyCount = len(out.Contents) + len(out.CommonPrefixes)
	h.deps.Enc.WriteXML(w, http.StatusOK, out)
}

func (h *bucketRoutes) listObjectVersions(w http.ResponseWriter, r *http.Request) {
	in, err := listInput(r)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	if !h.authz.authorize(w, r, policy.ActionListBucketVersions, in.Bucket, "") {
		return
	}
	q := r.URL.Query()
	in.Marker = q.Get("key-marker")
	in.VersionIDMarker = q.Get("version-id-marker")
	in.IncludeVersions = true
	res, err := h.deps.Objects.ListObjectVersions(r.Context(), in)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	owner := ownerOf(r)
	out := ListVersionsResult{
		Name: in.Bucket, Prefix: in.Prefix, KeyMarker: in.Marker, VersionIDMarker: in.VersionIDMarker,
		NextKeyMarker: res.NextMarker, NextVersionIDMarker: res.NextVersionIDMarker, MaxKeys: in.MaxKeys,
		Delimiter: in.Delimiter, IsTruncated: res.IsTruncated, CommonPrefixes: commonPrefixes(res.CommonPrefixes),
	}
	for _, o := range res.Objects {
		if o.IsDeleteMarker {
			out.DeleteMarkers = append(out.DeleteMarkers, DeleteMarkerEntry{Key: o.Key, VersionID: versionIDOrNull(o.VersionID), IsLatest: o.IsLatest, LastModified: s3Time(o.ModTime), Owner: owner})
			continue
		}
		out.Versions = append(out.Versions, VersionEntry{Key: o.Key, VersionID: versionIDOrNull(o.VersionID), IsLatest: o.IsLatest, LastModified: s3Time(o.ModTime), ETag: quoteETag(o.ETag), Size: o.Size, StorageClass: o.StorageClass, Owner: owner})
	}
	h.deps.Enc.WriteXML(w, http.StatusOK, out)
}

func (h *bucketRoutes) getLocation(w http.ResponseWriter, r *http.Request) {
	name := BucketName(r)
	if !h.authz.authorize(w, r, policy.ActionGetBucketLocation, name, "") {
		return
	}
	info, err := h.deps.Buckets.Get(r.Context(), name)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	region := info.Region
	if region == "us-east-1" {
		region = "" // S3 reports the default region as an empty constraint
	}
	h.deps.Enc.WriteXML(w, http.StatusOK, LocationConstraint{Location: region})
}

func (h *bucketRoutes) getPolicy(w http.ResponseWriter, r *http.Request) {
	name := BucketName(r)
	if !h.authz.authorize(w, r, policy.ActionGetBucketPolicy, name, "") {
		return
	}
	raw, err := h.deps.Buckets.GetPolicy(r.Context(), name)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	w.Header().Set("Content-Type", httpx.ContentTypeJSON)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

func (h *bucketRoutes) putPolicy(w http.ResponseWriter, r *http.Request) {
	name := BucketName(r)
	if !h.authz.authorize(w, r, policy.ActionPutBucketPolicy, name, "") {
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxPolicyBody+1))
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, fmt.Errorf("%w: read policy: %v", domain.ErrInvalidArgument, err))
		return
	}
	if len(raw) > maxPolicyBody {
		h.deps.Enc.WriteS3Error(w, r, domain.NewS3Error(domain.ErrInvalidArgument, "PolicyTooLarge", "Policy exceeds the maximum allowed document size."))
		return
	}
	if h.deps.Policy != nil {
		if _, err := h.deps.Policy.Parse(r.Context(), raw); err != nil {
			h.deps.Enc.WriteS3Error(w, r, err)
			return
		}
	}
	if err := h.deps.Buckets.SetPolicy(r.Context(), name, json.RawMessage(raw)); err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *bucketRoutes) deletePolicy(w http.ResponseWriter, r *http.Request) {
	name := BucketName(r)
	if !h.authz.authorize(w, r, policy.ActionDeleteBucketPolicy, name, "") {
		return
	}
	if err := h.deps.Buckets.DeletePolicy(r.Context(), name); err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *bucketRoutes) getPolicyStatus(w http.ResponseWriter, r *http.Request) {
	name := BucketName(r)
	if !h.authz.authorize(w, r, "s3:GetBucketPolicyStatus", name, "") {
		return
	}
	raw, err := h.deps.Buckets.GetPolicy(r.Context(), name)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	var p domain.Policy
	isPublic := false
	if json.Unmarshal(raw, &p) == nil {
		for _, st := range p.Statement {
			if st.Effect == domain.Allow && st.Principal.IsWildcard() {
				isPublic = true
			}
		}
	}
	h.deps.Enc.WriteXML(w, http.StatusOK, PolicyStatus{IsPublic: isPublic})
}

func (h *bucketRoutes) getVersioning(w http.ResponseWriter, r *http.Request) {
	name := BucketName(r)
	if !h.authz.authorize(w, r, policy.ActionGetBucketVersioning, name, "") {
		return
	}
	v, err := h.deps.Buckets.GetVersioning(r.Context(), name)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	h.deps.Enc.WriteXML(w, http.StatusOK, VersioningConfiguration{Status: v.String()})
}

func (h *bucketRoutes) putVersioning(w http.ResponseWriter, r *http.Request) {
	name := BucketName(r)
	if !h.authz.authorize(w, r, policy.ActionPutBucketVersioning, name, "") {
		return
	}
	var cfg versioningRequest
	if err := h.deps.Enc.DecodeXML(r.Body, maxXMLBody, &cfg); err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	v, err := domain.ParseVersioning(cfg.Status)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, domain.NewS3Error(domain.ErrMalformedXML, "IllegalVersioningConfigurationException", "The Versioning element must be Enabled or Suspended"))
		return
	}
	if err := h.deps.Buckets.SetVersioning(r.Context(), name, v); err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *bucketRoutes) getTagging(w http.ResponseWriter, r *http.Request) {
	name := BucketName(r)
	if !h.authz.authorize(w, r, "s3:GetBucketTagging", name, "") {
		return
	}
	tags, err := h.deps.Buckets.GetTags(r.Context(), name)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	h.deps.Enc.WriteXML(w, http.StatusOK, tagsToXML(tags))
}

func (h *bucketRoutes) putTagging(w http.ResponseWriter, r *http.Request) {
	name := BucketName(r)
	if !h.authz.authorize(w, r, "s3:PutBucketTagging", name, "") {
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
	if err := h.deps.Buckets.SetTags(r.Context(), name, tags); err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *bucketRoutes) deleteTagging(w http.ResponseWriter, r *http.Request) {
	h.deleter("s3:PutBucketTagging", h.deps.Buckets.DeleteTags)(w, r)
}

// rawGetter serves a stored XML configuration document.
func (h *bucketRoutes) rawGetter(action policy.Action, get func(ctx context.Context, name string) ([]byte, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := BucketName(r)
		if !h.authz.authorize(w, r, action, name, "") {
			return
		}
		doc, err := get(r.Context(), name)
		if err != nil {
			h.deps.Enc.WriteS3Error(w, r, err)
			return
		}
		w.Header().Set("Content-Type", httpx.ContentTypeXML)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(doc)
	}
}

// rawSetter stores an XML configuration document as sent by the client.
func (h *bucketRoutes) rawSetter(action policy.Action, set func(ctx context.Context, name string, doc []byte) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := BucketName(r)
		if !h.authz.authorize(w, r, action, name, "") {
			return
		}
		doc, err := io.ReadAll(io.LimitReader(r.Body, maxXMLBody+1))
		if err != nil || len(doc) > maxXMLBody {
			h.deps.Enc.WriteS3Error(w, r, fmt.Errorf("%w: configuration document too large or unreadable", domain.ErrMalformedXML))
			return
		}
		if err := set(r.Context(), name, doc); err != nil {
			h.deps.Enc.WriteS3Error(w, r, err)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

// deleter removes a bucket sub-resource.
func (h *bucketRoutes) deleter(action policy.Action, del func(ctx context.Context, name string) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := BucketName(r)
		if !h.authz.authorize(w, r, action, name, "") {
			return
		}
		if err := del(r.Context(), name); err != nil {
			h.deps.Enc.WriteS3Error(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (h *bucketRoutes) getObjectLock(w http.ResponseWriter, r *http.Request) {
	name := BucketName(r)
	if !h.authz.authorize(w, r, "s3:GetBucketObjectLockConfiguration", name, "") {
		return
	}
	cfg, err := h.deps.Buckets.GetObjectLock(r.Context(), name)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	out := ObjectLockConfiguration{ObjectLockEnabled: "Enabled"}
	if cfg.Mode != "" {
		out.Rule = &ObjectLockRule{DefaultRetention: DefaultRetention{Mode: string(cfg.Mode), Days: cfg.Days, Years: cfg.Years}}
	}
	h.deps.Enc.WriteXML(w, http.StatusOK, out)
}

func (h *bucketRoutes) putObjectLock(w http.ResponseWriter, r *http.Request) {
	name := BucketName(r)
	if !h.authz.authorize(w, r, "s3:PutBucketObjectLockConfiguration", name, "") {
		return
	}
	var doc objectLockRequest
	if err := h.deps.Enc.DecodeXML(r.Body, maxXMLBody, &doc); err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	cfg := bucket.ObjectLockConfig{Enabled: doc.ObjectLockEnabled == "Enabled"}
	if doc.Rule != nil {
		cfg.Mode = bucket.RetentionMode(doc.Rule.DefaultRetention.Mode)
		cfg.Days = doc.Rule.DefaultRetention.Days
		cfg.Years = doc.Rule.DefaultRetention.Years
	}
	if err := h.deps.Buckets.SetObjectLock(r.Context(), name, cfg); err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *bucketRoutes) getEncryption(w http.ResponseWriter, r *http.Request) {
	name := BucketName(r)
	if !h.authz.authorize(w, r, "s3:GetEncryptionConfiguration", name, "") {
		return
	}
	cfg, err := h.deps.Buckets.GetEncryption(r.Context(), name)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	out := ServerSideEncryptionConfiguration{Rules: []EncryptionRule{{
		ApplyServerSideEncryptionByDefault: EncryptionByDefault{SSEAlgorithm: cfg.Algorithm, KMSMasterKeyID: cfg.KMSKeyID},
	}}}
	h.deps.Enc.WriteXML(w, http.StatusOK, out)
}

func (h *bucketRoutes) putEncryption(w http.ResponseWriter, r *http.Request) {
	name := BucketName(r)
	if !h.authz.authorize(w, r, "s3:PutEncryptionConfiguration", name, "") {
		return
	}
	var doc encryptionRequest
	if err := h.deps.Enc.DecodeXML(r.Body, maxXMLBody, &doc); err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	if len(doc.Rules) != 1 {
		h.deps.Enc.WriteS3Error(w, r, domain.NewS3Error(domain.ErrMalformedXML, "MalformedXML", "Exactly one encryption rule is supported"))
		return
	}
	rule := doc.Rules[0].ApplyServerSideEncryptionByDefault
	if err := h.deps.Buckets.SetEncryption(r.Context(), name, bucket.EncryptionConfig{Algorithm: rule.SSEAlgorithm, KMSKeyID: rule.KMSMasterKeyID}); err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// cannedPrivateACL is the only ACL Nexo exposes: full control to the owner.
func cannedPrivateACL(owner Owner) AccessControlPolicy {
	var acl AccessControlPolicy
	acl.Owner = owner
	var g Grant
	g.Grantee.XMLNS = "http://www.w3.org/2001/XMLSchema-instance"
	g.Grantee.Type = "CanonicalUser"
	g.Grantee.ID = owner.ID
	g.Grantee.DisplayName = owner.DisplayName
	g.Permission = "FULL_CONTROL"
	acl.AccessControlList.Grants = []Grant{g}
	return acl
}

func (h *bucketRoutes) getACL(w http.ResponseWriter, r *http.Request) {
	name := BucketName(r)
	if !h.authz.authorize(w, r, "s3:GetBucketAcl", name, "") {
		return
	}
	if _, err := h.deps.Buckets.Get(r.Context(), name); err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	h.deps.Enc.WriteXML(w, http.StatusOK, cannedPrivateACL(ownerOf(r)))
}

// putACL accepts only the private canned ACL: access is governed by policies.
func (h *bucketRoutes) putACL(w http.ResponseWriter, r *http.Request) {
	name := BucketName(r)
	if !h.authz.authorize(w, r, "s3:PutBucketAcl", name, "") {
		return
	}
	if acl := r.Header.Get("x-amz-acl"); acl != "" && acl != "private" {
		h.deps.Enc.WriteS3Error(w, r, fmt.Errorf("%w: only the private canned ACL is supported, use bucket policies", domain.ErrNotImplemented))
		return
	}
	if _, err := h.deps.Buckets.Get(r.Context(), name); err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// noBuckets stands in for a missing bucket service when building the
// operation table; its handlers are never served (see Mount).
type noBuckets struct{ bucket.Service }
