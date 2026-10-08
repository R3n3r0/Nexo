package s3_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/auth"
	authmock "github.com/r3n3r0/nexo/internal/service/auth/mock"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	bucketmock "github.com/r3n3r0/nexo/internal/service/bucket/mock"
	"github.com/r3n3r0/nexo/internal/service/health"
	"github.com/r3n3r0/nexo/internal/service/metrics"
	"github.com/r3n3r0/nexo/internal/service/object"
	objectmock "github.com/r3n3r0/nexo/internal/service/object/mock"
	"github.com/r3n3r0/nexo/internal/service/policy"
	policymock "github.com/r3n3r0/nexo/internal/service/policy/mock"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
	"github.com/r3n3r0/nexo/internal/transport/http/s3"
)

var now = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

// deps bundles the mocks behind an API under test.
type deps struct {
	objects  *objectmock.Service
	buckets  *bucketmock.Service
	policy   *policymock.Service
	verifier *authmock.Verifier
	enc      httpx.Codec
}

func newDeps(t *testing.T) *deps {
	t.Helper()
	return &deps{
		objects:  objectmock.NewService(t),
		buckets:  bucketmock.NewService(t),
		policy:   policymock.NewService(t),
		verifier: authmock.NewVerifier(t),
		enc:      httpx.New(),
	}
}

// api builds the full S3 API; withPolicy enables the authorizer.
func (d *deps) api(withPolicy bool) http.Handler {
	var pol policy.Service
	if withPolicy {
		pol = d.policy
	}
	api := s3.NewAPI(d.enc,
		s3.NewBucketRoutes(s3.BucketDeps{Buckets: d.buckets, Objects: d.objects, Policy: pol, Enc: d.enc}),
		s3.NewObjectRoutes(s3.ObjectDeps{Objects: d.objects, Buckets: d.buckets, Policy: pol, Enc: d.enc}),
		s3.NewMultipartRoutes(s3.MultipartDeps{Objects: d.objects, Buckets: d.buckets, Policy: pol, Enc: d.enc}),
		s3.NewPresignedRoutes(s3.PresignedDeps{Verifier: d.verifier, Objects: d.objects, Buckets: d.buckets, Policy: pol, Enc: d.enc}),
		s3.NewPostPolicyRoutes(s3.PostPolicyDeps{Verifier: d.verifier, Objects: d.objects, Buckets: d.buckets, Policy: pol, Enc: d.enc}),
	)
	r := chi.NewRouter()
	api.Register(r)
	return r
}

func do(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func s3Error(t *testing.T, rec *httptest.ResponseRecorder) httpx.S3ErrorResponse {
	t.Helper()
	var body httpx.S3ErrorResponse
	if rec.Body.Len() > 0 && strings.Contains(rec.Header().Get("Content-Type"), "xml") {
		require.NoError(t, xml.Unmarshal(rec.Body.Bytes(), &body))
	}
	return body
}

func TestAPIWithoutServicesAnswersNotImplemented(t *testing.T) {
	enc := httpx.New()
	api := s3.NewAPI(enc,
		s3.NewBucketRoutes(s3.BucketDeps{Enc: enc}),
		s3.NewObjectRoutes(s3.ObjectDeps{Enc: enc}),
		s3.NewMultipartRoutes(s3.MultipartDeps{Enc: enc}),
		s3.NewPresignedRoutes(s3.PresignedDeps{Enc: enc}),
		s3.NewPostPolicyRoutes(s3.PostPolicyDeps{Enc: enc}),
		nil,
	)
	r := chi.NewRouter()
	api.Register(r)
	s3.NewHealthRoutes(s3.HealthDeps{Health: health.New(health.Deps{}), Enc: enc}).Register(r)
	mr, err := s3.NewMetricsRoutes(s3.MetricsDeps{Registry: metrics.New(metrics.Deps{}, metrics.WithoutRuntimeCollectors())})
	require.NoError(t, err)
	mr.Register(r)
	for _, tc := range []struct{ method, target string }{
		{"GET", "/"}, {"PUT", "/bucket"}, {"HEAD", "/bucket"}, {"GET", "/bucket?list-type=2&prefix=a"},
		{"GET", "/bucket?versions"}, {"GET", "/bucket?uploads"}, {"POST", "/bucket?delete"}, {"POST", "/bucket"},
		{"PUT", "/bucket/key"}, {"GET", "/bucket/dir/key.txt"}, {"POST", "/bucket/key?uploads"},
		{"PUT", "/bucket/key?uploadId=1&partNumber=2"}, {"GET", "/bucket/key?tagging"},
		{"GET", "/bucket/key?X-Amz-Signature=abc&X-Amz-Algorithm=AWS4-HMAC-SHA256"}, {"GET", "/bucket/key?retention"},
	} {
		rec := do(r, httptest.NewRequest(tc.method, tc.target, nil))
		assert.Equal(t, http.StatusNotImplemented, rec.Code, "%s %s", tc.method, tc.target)
		if tc.method != http.MethodHead {
			assert.Equal(t, "NotImplemented", s3Error(t, rec).Code, "%s %s", tc.method, tc.target)
		}
	}
	// Health and metrics paths win over the /{bucket}/* pattern.
	rec := do(r, httptest.NewRequest(http.MethodGet, s3.PathHealthLive, nil))
	assert.Equal(t, http.StatusOK, rec.Code)
	rec = do(r, httptest.NewRequest(http.MethodGet, s3.PathMetricsNode, nil))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "text/plain")
	rec = do(r, httptest.NewRequest(http.MethodGet, "/nexo", nil))
	assert.Equal(t, http.StatusNotImplemented, rec.Code, "a bucket literally named nexo still routes to S3")
	_, err = s3.NewMetricsRoutes(s3.MetricsDeps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument, "the registry is wired by app, never created here")
}

func TestDispatcherSelectsOperation(t *testing.T) {
	enc := httpx.New()
	d := s3.NewDispatcher(enc)
	var seen []string
	record := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			op, ok := s3.OperationFromContext(r.Context())
			require.True(t, ok)
			assert.Equal(t, name, op.Name)
			seen = append(seen, name+":"+s3.BucketName(r)+":"+s3.ObjectKey(r))
			w.WriteHeader(http.StatusOK)
		}
	}
	d.Handle(s3.Operation{Name: "ListObjects", Method: "GET", Scope: s3.ScopeBucket}, record("ListObjects"))
	d.Handle(s3.Operation{Name: "ListObjectsV2", Method: "GET", Scope: s3.ScopeBucket, Query: "list-type", QueryValue: "2"}, record("ListObjectsV2"))
	d.Handle(s3.Operation{Name: "ListMultipartUploads", Method: "GET", Scope: s3.ScopeBucket, Query: "uploads"}, record("ListMultipartUploads"))
	d.Handle(s3.Operation{Name: "GetObject", Method: "GET", Scope: s3.ScopeObject}, record("GetObject"))
	d.Handle(s3.Operation{Name: "ListParts", Method: "GET", Scope: s3.ScopeObject, Query: "uploadId"}, record("ListParts"))
	d.Handle(s3.Operation{Name: "Uploads", Method: "POST", Scope: s3.ScopeObject, Query: "uploads"}, record("Uploads"))
	r := chi.NewRouter()
	d.Register(r)

	for _, target := range []string{"/b", "/b/", "/b?list-type=2", "/b?list-type=1", "/b?uploads", "/b/k", "/b/dir/sub/k%20space.txt", "/b/k?uploadId=x"} {
		rec := do(r, httptest.NewRequest(http.MethodGet, target, nil))
		assert.Equal(t, http.StatusOK, rec.Code, target)
	}
	assert.Equal(t, []string{
		"ListObjects:b:", "ListObjects:b:", "ListObjectsV2:b:", "ListObjects:b:", "ListMultipartUploads:b:",
		"GetObject:b:k", "GetObject:b:dir/sub/k space.txt", "ListParts:b:k",
	}, seen)

	rec := do(r, httptest.NewRequest(http.MethodPost, "/b/k", nil))
	assert.Equal(t, http.StatusNotImplemented, rec.Code, "POST on an object without ?uploads has no default operation")
	rec = do(r, httptest.NewRequest(http.MethodDelete, "/b/k", nil))
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	assert.Equal(t, "MethodNotAllowed", s3Error(t, rec).Code)
	rec = do(r, httptest.NewRequest("PROPFIND", "/b/k", nil))
	assert.Equal(t, "MethodNotAllowed", s3Error(t, rec).Code)
	assert.Panics(t, func() {
		d.Handle(s3.Operation{Name: "Dup", Method: "GET", Scope: s3.ScopeBucket, Query: "uploads"}, record("Dup"))
	})
	assert.Panics(t, func() { d.Handle(s3.Operation{Name: "Nil", Method: "GET", Scope: s3.ScopeService}, nil) })
}

func TestGroupsRegisterStandalone(t *testing.T) {
	enc := httpx.New()
	for name, g := range map[string]s3.Routes{
		"bucket":     s3.NewBucketRoutes(s3.BucketDeps{}),
		"object":     s3.NewObjectRoutes(s3.ObjectDeps{}),
		"multipart":  s3.NewMultipartRoutes(s3.MultipartDeps{}),
		"presigned":  s3.NewPresignedRoutes(s3.PresignedDeps{}),
		"postpolicy": s3.NewPostPolicyRoutes(s3.PostPolicyDeps{Enc: enc}),
	} {
		r := chi.NewRouter()
		g.Register(r)
		rec := do(r, httptest.NewRequest(http.MethodGet, "/b/k", nil))
		assert.Contains(t, []int{http.StatusNotImplemented, http.StatusMethodNotAllowed}, rec.Code, name)
		assert.Equal(t, httpx.ContentTypeXML, rec.Header().Get("Content-Type"), name)
	}
}

func TestBucketHandlers(t *testing.T) {
	d := newDeps(t)
	h := d.api(false)

	d.buckets.EXPECT().List(mock.Anything, bucket.ListInput{}).Return([]domain.BucketInfo{
		{Bucket: domain.Bucket{Name: "docs", Created: now, Region: "us-east-1"}},
		{Bucket: domain.Bucket{Name: "photos", Created: now.Add(time.Hour), Region: "eu-south-1"}},
	}, nil).Once()
	rec := do(h, httptest.NewRequest(http.MethodGet, "/", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var list s3.ListAllMyBucketsResult
	require.NoError(t, xml.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list.Buckets.Bucket, 2)
	assert.Equal(t, "docs", list.Buckets.Bucket[0].Name)
	assert.Equal(t, "2026-10-08T12:00:00.000Z", list.Buckets.Bucket[0].CreationDate)
	assert.Equal(t, "nexo", list.Owner.ID)
	assert.Contains(t, rec.Body.String(), `xmlns="http://s3.amazonaws.com/doc/2006-03-01/"`)

	// CreateBucket with a location constraint and object lock.
	d.buckets.EXPECT().Create(mock.Anything, bucket.CreateInput{Name: "photos", Region: "eu-south-1", ObjectLock: true}).
		Return(&domain.Bucket{Name: "photos"}, nil).Once()
	req := httptest.NewRequest(http.MethodPut, "/photos", strings.NewReader(`<CreateBucketConfiguration><LocationConstraint>eu-south-1</LocationConstraint></CreateBucketConfiguration>`))
	req.Header.Set("x-amz-bucket-object-lock-enabled", "true")
	rec = do(h, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "/photos", rec.Header().Get("Location"))
	d.buckets.EXPECT().Create(mock.Anything, bucket.CreateInput{Name: "photos"}).Return(nil, domain.ErrBucketAlreadyExists).Once()
	rec = do(h, httptest.NewRequest(http.MethodPut, "/photos", nil))
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, "BucketAlreadyOwnedByYou", s3Error(t, rec).Code)
	rec = do(h, httptest.NewRequest(http.MethodPut, "/photos", strings.NewReader("<not xml")))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "MalformedXML", s3Error(t, rec).Code)

	// HeadBucket and DeleteBucket.
	d.buckets.EXPECT().Get(mock.Anything, "photos").Return(&domain.BucketInfo{Bucket: domain.Bucket{Name: "photos", Region: "eu-south-1"}}, nil).Once()
	rec = do(h, httptest.NewRequest(http.MethodHead, "/photos", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "eu-south-1", rec.Header().Get("x-amz-bucket-region"))
	d.buckets.EXPECT().Get(mock.Anything, "missing").Return(nil, domain.ErrBucketNotFound).Once()
	rec = do(h, httptest.NewRequest(http.MethodHead, "/missing", nil))
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Empty(t, rec.Body.String(), "HEAD errors have no body")
	d.buckets.EXPECT().Delete(mock.Anything, "photos", bucket.DeleteOptions{}).Return(nil).Once()
	rec = do(h, httptest.NewRequest(http.MethodDelete, "/photos", nil))
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// ListObjectsV2 and ListObjects.
	d.objects.EXPECT().ListObjects(mock.Anything, domain.ListObjectsInput{Bucket: "photos", Prefix: "2024/", Delimiter: "/", MaxKeys: 2, ContinuationToken: "tok", FetchOwner: true}).
		Return(domain.ListObjectsResult{
			Objects:        []domain.ObjectInfo{{Key: "2024/a.jpg", Size: 10, ETag: "e1", ModTime: now, StorageClass: "STANDARD"}},
			CommonPrefixes: []string{"2024/raw/"}, IsTruncated: true, NextContinuationToken: "next",
		}, nil).Once()
	rec = do(h, httptest.NewRequest(http.MethodGet, "/photos?list-type=2&prefix=2024/&delimiter=/&max-keys=2&continuation-token=tok&fetch-owner=true", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var v2 s3.ListBucketResultV2
	require.NoError(t, xml.Unmarshal(rec.Body.Bytes(), &v2))
	assert.Equal(t, 2, v2.KeyCount)
	assert.True(t, v2.IsTruncated)
	assert.Equal(t, "next", v2.NextContinuationToken)
	assert.Equal(t, `"e1"`, v2.Contents[0].ETag)
	assert.Equal(t, "2024/raw/", v2.CommonPrefixes[0].Prefix)
	require.NotNil(t, v2.Contents[0].Owner)
	d.objects.EXPECT().ListObjects(mock.Anything, domain.ListObjectsInput{Bucket: "photos", Marker: "m", MaxKeys: 1000}).
		Return(domain.ListObjectsResult{Objects: []domain.ObjectInfo{{Key: "z", ModTime: now}}}, nil).Once()
	rec = do(h, httptest.NewRequest(http.MethodGet, "/photos?marker=m", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	var v1 s3.ListBucketResultV1
	require.NoError(t, xml.Unmarshal(rec.Body.Bytes(), &v1))
	assert.Equal(t, "m", v1.Marker)
	assert.Equal(t, 1000, v1.MaxKeys)
	rec = do(h, httptest.NewRequest(http.MethodGet, "/photos?max-keys=-1", nil))
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	// ListObjectVersions splits versions and delete markers.
	d.objects.EXPECT().ListObjectVersions(mock.Anything, mock.MatchedBy(func(in domain.ListObjectsInput) bool {
		return in.Bucket == "photos" && in.IncludeVersions && in.Marker == "k" && in.VersionIDMarker == "v0"
	})).Return(domain.ListObjectsResult{Objects: []domain.ObjectInfo{
		{Key: "k", VersionID: "v2", IsLatest: true, IsDeleteMarker: true, ModTime: now},
		{Key: "k", VersionID: "", Size: 3, ETag: "e", ModTime: now},
	}, IsTruncated: true, NextMarker: "k", NextVersionIDMarker: "null"}, nil).Once()
	rec = do(h, httptest.NewRequest(http.MethodGet, "/photos?versions&key-marker=k&version-id-marker=v0", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var versions s3.ListVersionsResult
	require.NoError(t, xml.Unmarshal(rec.Body.Bytes(), &versions))
	require.Len(t, versions.Versions, 1)
	require.Len(t, versions.DeleteMarkers, 1)
	assert.Equal(t, "null", versions.Versions[0].VersionID)
	assert.True(t, versions.DeleteMarkers[0].IsLatest)
	assert.Equal(t, "null", versions.NextVersionIDMarker)

	// Location: the default region is an empty constraint.
	d.buckets.EXPECT().Get(mock.Anything, "photos").Return(&domain.BucketInfo{Bucket: domain.Bucket{Name: "photos", Region: "us-east-1"}}, nil).Once()
	rec = do(h, httptest.NewRequest(http.MethodGet, "/photos?location", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "<LocationConstraint xmlns=\"http://s3.amazonaws.com/doc/2006-03-01/\"></LocationConstraint>")

	// Policy round trip and validation through the policy engine.
	doc := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::photos/*"}]}`
	d.buckets.EXPECT().SetPolicy(mock.Anything, "photos", json.RawMessage(doc)).Return(nil).Once()
	rec = do(h, httptest.NewRequest(http.MethodPut, "/photos?policy", strings.NewReader(doc)))
	assert.Equal(t, http.StatusNoContent, rec.Code)
	d.buckets.EXPECT().GetPolicy(mock.Anything, "photos").Return(json.RawMessage(doc), nil).Twice()
	rec = do(h, httptest.NewRequest(http.MethodGet, "/photos?policy", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, httpx.ContentTypeJSON, rec.Header().Get("Content-Type"))
	assert.JSONEq(t, doc, rec.Body.String())
	rec = do(h, httptest.NewRequest(http.MethodGet, "/photos?policyStatus", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "<IsPublic>true</IsPublic>")
	d.buckets.EXPECT().DeletePolicy(mock.Anything, "photos").Return(nil).Once()
	rec = do(h, httptest.NewRequest(http.MethodDelete, "/photos?policy", nil))
	assert.Equal(t, http.StatusNoContent, rec.Code)
	d.buckets.EXPECT().GetPolicy(mock.Anything, "photos").Return(nil, domain.NewS3Error(domain.ErrPolicyNotFound, "NoSuchBucketPolicy", "The bucket policy does not exist")).Once()
	rec = do(h, httptest.NewRequest(http.MethodGet, "/photos?policy", nil))
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "NoSuchBucketPolicy", s3Error(t, rec).Code)

	// Versioning.
	d.buckets.EXPECT().SetVersioning(mock.Anything, "photos", domain.VersioningEnabled).Return(nil).Once()
	rec = do(h, httptest.NewRequest(http.MethodPut, "/photos?versioning", strings.NewReader(`<VersioningConfiguration><Status>Enabled</Status></VersioningConfiguration>`)))
	assert.Equal(t, http.StatusOK, rec.Code)
	rec = do(h, httptest.NewRequest(http.MethodPut, "/photos?versioning", strings.NewReader(`<VersioningConfiguration><Status>Maybe</Status></VersioningConfiguration>`)))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "IllegalVersioningConfigurationException", s3Error(t, rec).Code)
	d.buckets.EXPECT().GetVersioning(mock.Anything, "photos").Return(domain.VersioningSuspended, nil).Once()
	rec = do(h, httptest.NewRequest(http.MethodGet, "/photos?versioning", nil))
	assert.Contains(t, rec.Body.String(), "<Status>Suspended</Status>")

	// Tagging, lifecycle (raw), object lock, encryption, ACL.
	d.buckets.EXPECT().SetTags(mock.Anything, "photos", map[string]string{"a": "1", "b": "2"}).Return(nil).Once()
	rec = do(h, httptest.NewRequest(http.MethodPut, "/photos?tagging", strings.NewReader(`<Tagging><TagSet><Tag><Key>a</Key><Value>1</Value></Tag><Tag><Key>b</Key><Value>2</Value></Tag></TagSet></Tagging>`)))
	assert.Equal(t, http.StatusNoContent, rec.Code)
	rec = do(h, httptest.NewRequest(http.MethodPut, "/photos?tagging", strings.NewReader(`<Tagging><TagSet><Tag><Key>a</Key><Value>1</Value></Tag><Tag><Key>a</Key><Value>2</Value></Tag></TagSet></Tagging>`)))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	d.buckets.EXPECT().GetTags(mock.Anything, "photos").Return(map[string]string{"b": "2", "a": "1"}, nil).Once()
	rec = do(h, httptest.NewRequest(http.MethodGet, "/photos?tagging", nil))
	assert.Contains(t, rec.Body.String(), "<Tag><Key>a</Key><Value>1</Value></Tag><Tag><Key>b</Key><Value>2</Value></Tag>")
	d.buckets.EXPECT().DeleteTags(mock.Anything, "photos").Return(nil).Once()
	rec = do(h, httptest.NewRequest(http.MethodDelete, "/photos?tagging", nil))
	assert.Equal(t, http.StatusNoContent, rec.Code)
	lc := []byte(`<LifecycleConfiguration><Rule><ID>r</ID><Status>Enabled</Status></Rule></LifecycleConfiguration>`)
	d.buckets.EXPECT().SetLifecycle(mock.Anything, "photos", lc).Return(nil).Once()
	rec = do(h, httptest.NewRequest(http.MethodPut, "/photos?lifecycle", bytes.NewReader(lc)))
	assert.Equal(t, http.StatusOK, rec.Code)
	d.buckets.EXPECT().GetLifecycle(mock.Anything, "photos").Return(lc, nil).Once()
	rec = do(h, httptest.NewRequest(http.MethodGet, "/photos?lifecycle", nil))
	assert.Equal(t, string(lc), rec.Body.String())
	d.buckets.EXPECT().DeleteLifecycle(mock.Anything, "photos").Return(nil).Once()
	rec = do(h, httptest.NewRequest(http.MethodDelete, "/photos?lifecycle", nil))
	assert.Equal(t, http.StatusNoContent, rec.Code)
	d.buckets.EXPECT().SetObjectLock(mock.Anything, "photos", bucket.ObjectLockConfig{Enabled: true, Mode: bucket.RetentionGovernance, Days: 30}).Return(nil).Once()
	rec = do(h, httptest.NewRequest(http.MethodPut, "/photos?object-lock", strings.NewReader(`<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled><Rule><DefaultRetention><Mode>GOVERNANCE</Mode><Days>30</Days></DefaultRetention></Rule></ObjectLockConfiguration>`)))
	assert.Equal(t, http.StatusOK, rec.Code)
	d.buckets.EXPECT().GetObjectLock(mock.Anything, "photos").Return(bucket.ObjectLockConfig{Enabled: true, Mode: bucket.RetentionCompliance, Years: 1}, nil).Once()
	rec = do(h, httptest.NewRequest(http.MethodGet, "/photos?object-lock", nil))
	assert.Contains(t, rec.Body.String(), "<Mode>COMPLIANCE</Mode><Years>1</Years>")
	d.buckets.EXPECT().SetEncryption(mock.Anything, "photos", bucket.EncryptionConfig{Algorithm: "aws:kms", KMSKeyID: "k1"}).Return(nil).Once()
	rec = do(h, httptest.NewRequest(http.MethodPut, "/photos?encryption", strings.NewReader(`<ServerSideEncryptionConfiguration><Rule><ApplyServerSideEncryptionByDefault><SSEAlgorithm>aws:kms</SSEAlgorithm><KMSMasterKeyID>k1</KMSMasterKeyID></ApplyServerSideEncryptionByDefault></Rule></ServerSideEncryptionConfiguration>`)))
	assert.Equal(t, http.StatusOK, rec.Code)
	d.buckets.EXPECT().GetEncryption(mock.Anything, "photos").Return(bucket.EncryptionConfig{Algorithm: "AES256"}, nil).Once()
	rec = do(h, httptest.NewRequest(http.MethodGet, "/photos?encryption", nil))
	assert.Contains(t, rec.Body.String(), "<SSEAlgorithm>AES256</SSEAlgorithm>")
	d.buckets.EXPECT().DeleteEncryption(mock.Anything, "photos").Return(nil).Once()
	rec = do(h, httptest.NewRequest(http.MethodDelete, "/photos?encryption", nil))
	assert.Equal(t, http.StatusNoContent, rec.Code)
	d.buckets.EXPECT().Get(mock.Anything, "photos").Return(&domain.BucketInfo{}, nil).Twice()
	rec = do(h, httptest.NewRequest(http.MethodGet, "/photos?acl", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "<Permission>FULL_CONTROL</Permission>")
	rec = do(h, httptest.NewRequest(http.MethodPut, "/photos?acl", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
	req = httptest.NewRequest(http.MethodPut, "/photos?acl", nil)
	req.Header.Set("x-amz-acl", "public-read")
	rec = do(h, req)
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
	notif := []byte(`<NotificationConfiguration></NotificationConfiguration>`)
	d.buckets.EXPECT().GetNotification(mock.Anything, "photos").Return(notif, nil).Once()
	rec = do(h, httptest.NewRequest(http.MethodGet, "/photos?notification", nil))
	assert.Equal(t, string(notif), rec.Body.String())
}

func TestObjectHandlers(t *testing.T) {
	d := newDeps(t)
	h := d.api(false)
	body := []byte("hello world")

	// PutObject carries metadata, tags, checksums and conditions.
	d.objects.EXPECT().PutObject(mock.Anything, mock.MatchedBy(func(in object.PutObjectInput) bool {
		data, _ := io.ReadAll(in.Reader)
		return in.Bucket == "photos" && in.Key == "dir/a b.txt" && string(data) == "hello world" && in.Size == 11 &&
			in.ContentType == "text/plain" && in.UserMetadata["owner"] == "alice" && in.UserMetadata["cache-control"] == "max-age=1" &&
			in.Tags["k"] == "v" && in.MD5Hex == "5eb63bbbe01eeed093cb22bb8f5acdc3" && in.Checksum.Algorithm == domain.ChecksumCRC32C &&
			in.StorageClass == "REDUCED_REDUNDANCY" && in.Conditions.IfNoneMatch == "*"
	})).Return(domain.ObjectInfo{ETag: "5eb63bbbe01eeed093cb22bb8f5acdc3", VersionID: "v1", Checksum: domain.Checksum{Algorithm: domain.ChecksumCRC32C, Value: "crc"}}, nil).Once()
	req := httptest.NewRequest(http.MethodPut, "/photos/dir/a%20b.txt", bytes.NewReader(body))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("x-amz-meta-Owner", "alice")
	req.Header.Set("Cache-Control", "max-age=1")
	req.Header.Set("x-amz-tagging", "k=v")
	req.Header.Set("Content-MD5", base64.StdEncoding.EncodeToString(mustHex("5eb63bbbe01eeed093cb22bb8f5acdc3")))
	req.Header.Set("x-amz-checksum-crc32c", "crc")
	req.Header.Set("x-amz-storage-class", "REDUCED_REDUNDANCY")
	req.Header.Set("If-None-Match", "*")
	rec := do(h, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, `"5eb63bbbe01eeed093cb22bb8f5acdc3"`, rec.Header().Get("ETag"))
	assert.Equal(t, "v1", rec.Header().Get("x-amz-version-id"))
	assert.Equal(t, "crc", rec.Header().Get("x-amz-checksum-crc32c"))
	req = httptest.NewRequest(http.MethodPut, "/photos/k", bytes.NewReader(body))
	req.Header.Set("Content-MD5", "not base64!")
	rec = do(h, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "InvalidDigest", s3Error(t, rec).Code)

	// GetObject: full, ranged, overrides, errors.
	info := domain.ObjectInfo{Bucket: "photos", Key: "k", Size: 11, ETag: "e", ModTime: now, ContentType: "text/plain", UserMetadata: map[string]string{"owner": "alice"}, Tags: map[string]string{"a": "1"}, Parts: 2, VersionID: "v1"}
	d.objects.EXPECT().GetObject(mock.Anything, "photos", "k", object.GetObjectOptions{}).
		Return(&object.Reader{ReadCloser: io.NopCloser(bytes.NewReader(body)), Info: info, Offset: 0, Length: 11}, nil).Once()
	rec = do(h, httptest.NewRequest(http.MethodGet, "/photos/k?response-content-disposition=attachment", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "hello world", rec.Body.String())
	assert.Equal(t, "11", rec.Header().Get("Content-Length"))
	assert.Equal(t, `"e"`, rec.Header().Get("ETag"))
	assert.Equal(t, "Thu, 08 Oct 2026 12:00:00 GMT", rec.Header().Get("Last-Modified"))
	assert.Equal(t, "alice", rec.Header().Get("x-amz-meta-owner"))
	assert.Equal(t, "1", rec.Header().Get("x-amz-tagging-count"))
	assert.Equal(t, "2", rec.Header().Get("x-amz-mp-parts-count"))
	assert.Equal(t, "attachment", rec.Header().Get("Content-Disposition"))
	assert.Equal(t, "v1", rec.Header().Get("x-amz-version-id"))
	assert.Equal(t, "bytes", rec.Header().Get("Accept-Ranges"))
	rg, _ := domain.ParseRange("bytes=6-10")
	d.objects.EXPECT().GetObject(mock.Anything, "photos", "k", mock.MatchedBy(func(o object.GetObjectOptions) bool {
		return o.Range != nil && o.Range.Start == 6 && o.Range.End == 10 && o.Conditions.IfMatch == `"e"` && o.VersionID == "v1"
	})).Return(&object.Reader{ReadCloser: io.NopCloser(bytes.NewReader(body[6:])), Info: info, Offset: 6, Length: 5}, nil).Once()
	req = httptest.NewRequest(http.MethodGet, "/photos/k?versionId=v1", nil)
	req.Header.Set("Range", "bytes=6-10")
	req.Header.Set("If-Match", `"e"`)
	rec = do(h, req)
	assert.Equal(t, http.StatusPartialContent, rec.Code)
	assert.Equal(t, "bytes 6-10/11", rec.Header().Get("Content-Range"))
	assert.Equal(t, "world", rec.Body.String())
	_ = rg
	d.objects.EXPECT().GetObject(mock.Anything, "photos", "gone", mock.Anything).
		Return(nil, domain.NewS3Error(domain.ErrObjectNotFound, "NoSuchKey", "The specified key does not exist.")).Once()
	rec = do(h, httptest.NewRequest(http.MethodGet, "/photos/gone", nil))
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "true", rec.Header().Get("x-amz-delete-marker"), "a latest delete marker is signalled")
	d.objects.EXPECT().GetObject(mock.Anything, "photos", "k", mock.Anything).Return(nil, domain.ErrNotModified).Once()
	req = httptest.NewRequest(http.MethodGet, "/photos/k", nil)
	req.Header.Set("If-None-Match", `"e"`)
	rec = do(h, req)
	assert.Equal(t, http.StatusNotModified, rec.Code)
	assert.Empty(t, rec.Body.String())
	req = httptest.NewRequest(http.MethodGet, "/photos/k", nil)
	req.Header.Set("Range", "bytes=a-b")
	rec = do(h, req)
	assert.Equal(t, http.StatusRequestedRangeNotSatisfiable, rec.Code)

	// HeadObject with a range reports the partial length without a body.
	d.objects.EXPECT().GetObjectInfo(mock.Anything, "photos", "k", mock.Anything).Return(info, nil).Once()
	req = httptest.NewRequest(http.MethodHead, "/photos/k", nil)
	req.Header.Set("Range", "bytes=0-3")
	rec = do(h, req)
	assert.Equal(t, http.StatusPartialContent, rec.Code)
	assert.Equal(t, "4", rec.Header().Get("Content-Length"))
	assert.Equal(t, "bytes 0-3/11", rec.Header().Get("Content-Range"))
	assert.Empty(t, rec.Body.String())
	d.objects.EXPECT().GetObjectInfo(mock.Anything, "photos", "k", mock.Anything).Return(domain.ObjectInfo{}, domain.ErrObjectNotFound).Once()
	rec = do(h, httptest.NewRequest(http.MethodHead, "/photos/k", nil))
	assert.Equal(t, http.StatusNotFound, rec.Code)

	// DeleteObject and DeleteObjects.
	d.objects.EXPECT().DeleteObject(mock.Anything, "photos", "k", object.DeleteObjectOptions{VersionID: "v1", BypassGovernance: true}).
		Return(object.DeletedObject{Key: "k", VersionID: "v1", DeleteMarker: true, DeleteMarkerVersionID: "v1"}, nil).Once()
	req = httptest.NewRequest(http.MethodDelete, "/photos/k?versionId=v1", nil)
	req.Header.Set("x-amz-bypass-governance-retention", "true")
	rec = do(h, req)
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, "true", rec.Header().Get("x-amz-delete-marker"))
	assert.Equal(t, "v1", rec.Header().Get("x-amz-version-id"))
	d.objects.EXPECT().DeleteObjects(mock.Anything, object.DeleteObjectsInput{Bucket: "photos", Objects: []object.ObjectToDelete{{Key: "a"}, {Key: "b", VersionID: "v"}}}).
		Return(object.DeleteObjectsResult{
			Deleted: []object.DeletedObject{{Key: "a"}},
			Errors:  []object.DeleteError{{Key: "b", VersionID: "v", Err: domain.ErrNoSuchVersion}},
		}, nil).Once()
	rec = do(h, httptest.NewRequest(http.MethodPost, "/photos?delete", strings.NewReader(`<Delete><Object><Key>a</Key></Object><Object><Key>b</Key><VersionId>v</VersionId></Object></Delete>`)))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var dr s3.DeleteResult
	require.NoError(t, xml.Unmarshal(rec.Body.Bytes(), &dr))
	require.Len(t, dr.Deleted, 1)
	require.Len(t, dr.Errors, 1)
	assert.Equal(t, "NoSuchVersion", dr.Errors[0].Code)
	rec = do(h, httptest.NewRequest(http.MethodPost, "/photos?delete", strings.NewReader(`<Delete></Delete>`)))
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	// Tagging.
	d.objects.EXPECT().PutObjectTags(mock.Anything, "photos", "k", map[string]string{"a": "1"}, object.TagOptions{VersionID: "v1"}).Return(domain.ObjectInfo{VersionID: "v1"}, nil).Once()
	rec = do(h, httptest.NewRequest(http.MethodPut, "/photos/k?tagging&versionId=v1", strings.NewReader(`<Tagging><TagSet><Tag><Key>a</Key><Value>1</Value></Tag></TagSet></Tagging>`)))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "v1", rec.Header().Get("x-amz-version-id"))
	d.objects.EXPECT().GetObjectTags(mock.Anything, "photos", "k", object.TagOptions{}).Return(map[string]string{"a": "1"}, nil).Once()
	rec = do(h, httptest.NewRequest(http.MethodGet, "/photos/k?tagging", nil))
	assert.Contains(t, rec.Body.String(), "<Key>a</Key><Value>1</Value>")
	d.objects.EXPECT().DeleteObjectTags(mock.Anything, "photos", "k", object.TagOptions{}).Return(domain.ObjectInfo{}, nil).Once()
	rec = do(h, httptest.NewRequest(http.MethodDelete, "/photos/k?tagging", nil))
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// CopyObject through PUT + x-amz-copy-source.
	d.objects.EXPECT().CopyObject(mock.Anything, object.CopyObjectInput{
		SrcBucket: "src", SrcKey: "dir/a b.txt", SrcVersionID: "v9", SrcConditions: domain.Conditions{IfMatch: `"x"`},
		DstBucket: "photos", DstKey: "copy", ReplaceMetadata: true, ContentType: "image/png", UserMetadata: map[string]string{"x": "y"},
		ReplaceTags: true, Tags: map[string]string{"t": "1"}, StorageClass: "",
	}).Return(domain.ObjectInfo{ETag: "ce", ModTime: now, VersionID: "v10"}, nil).Once()
	req = httptest.NewRequest(http.MethodPut, "/photos/copy", nil)
	req.Header.Set("x-amz-copy-source", "/src/dir/a%20b.txt?versionId=v9")
	req.Header.Set("x-amz-copy-source-if-match", `"x"`)
	req.Header.Set("x-amz-metadata-directive", "REPLACE")
	req.Header.Set("x-amz-tagging-directive", "REPLACE")
	req.Header.Set("x-amz-tagging", "t=1")
	req.Header.Set("Content-Type", "image/png")
	req.Header.Set("x-amz-meta-x", "y")
	rec = do(h, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `<ETag>&#34;ce&#34;</ETag>`)
	assert.Equal(t, "v10", rec.Header().Get("x-amz-version-id"))
	assert.Equal(t, "v9", rec.Header().Get("x-amz-copy-source-version-id"))
	req = httptest.NewRequest(http.MethodPut, "/photos/copy", nil)
	req.Header.Set("x-amz-copy-source", "nokey")
	rec = do(h, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	// ACL: private only.
	d.objects.EXPECT().GetObjectInfo(mock.Anything, "photos", "k", mock.Anything).Return(info, nil).Twice()
	rec = do(h, httptest.NewRequest(http.MethodGet, "/photos/k?acl", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
	rec = do(h, httptest.NewRequest(http.MethodPut, "/photos/k?acl", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
	rec = do(h, httptest.NewRequest(http.MethodGet, "/photos/k?retention", nil))
	assert.Equal(t, http.StatusNotImplemented, rec.Code)
}

func mustHex(s string) []byte {
	out := make([]byte, len(s)/2)
	for i := 0; i < len(out); i++ {
		var b byte
		for _, c := range s[2*i : 2*i+2] {
			b <<= 4
			switch {
			case c >= '0' && c <= '9':
				b |= byte(c - '0')
			case c >= 'a' && c <= 'f':
				b |= byte(c-'a') + 10
			}
		}
		out[i] = b
	}
	return out
}

func TestMultipartHandlers(t *testing.T) {
	d := newDeps(t)
	h := d.api(false)

	d.objects.EXPECT().NewMultipartUpload(mock.Anything, object.NewMultipartInput{Bucket: "b", Key: "big", ContentType: "video/mp4", UserMetadata: map[string]string{}, Checksum: domain.ChecksumSHA256}).
		Return(domain.MultipartUpload{Bucket: "b", Key: "big", UploadID: "up1"}, nil).Once()
	req := httptest.NewRequest(http.MethodPost, "/b/big?uploads", nil)
	req.Header.Set("Content-Type", "video/mp4")
	req.Header.Set("x-amz-checksum-algorithm", "sha256")
	rec := do(h, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "<UploadId>up1</UploadId>")

	d.objects.EXPECT().PutObjectPart(mock.Anything, mock.MatchedBy(func(in object.PutObjectPartInput) bool {
		data, _ := io.ReadAll(in.Reader)
		return in.Bucket == "b" && in.Key == "big" && in.UploadID == "up1" && in.PartNumber == 2 && string(data) == "part" && in.Size == 4
	})).Return(domain.Part{Number: 2, ETag: "pe", Size: 4}, nil).Once()
	rec = do(h, httptest.NewRequest(http.MethodPut, "/b/big?partNumber=2&uploadId=up1", strings.NewReader("part")))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, `"pe"`, rec.Header().Get("ETag"))
	rec = do(h, httptest.NewRequest(http.MethodPut, "/b/big?partNumber=x&uploadId=up1", strings.NewReader("part")))
	assert.Equal(t, http.StatusBadRequest, rec.Code)

	d.objects.EXPECT().CopyObjectPart(mock.Anything, mock.MatchedBy(func(in object.CopyObjectPartInput) bool {
		return in.SrcBucket == "src" && in.SrcKey == "k" && in.SrcRange != nil && in.SrcRange.End == 99 && in.DstKey == "big" && in.PartNumber == 3
	})).Return(domain.Part{Number: 3, ETag: "ce", ModTime: now}, nil).Once()
	req = httptest.NewRequest(http.MethodPut, "/b/big?partNumber=3&uploadId=up1", nil)
	req.Header.Set("x-amz-copy-source", "src/k")
	req.Header.Set("x-amz-copy-source-range", "bytes=0-99")
	rec = do(h, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "<CopyPartResult")

	d.objects.EXPECT().ListObjectParts(mock.Anything, domain.ListPartsInput{Bucket: "b", Key: "big", UploadID: "up1", PartNumberMarker: 1, MaxParts: 5}).
		Return(domain.ListPartsResult{Upload: domain.MultipartUpload{UploadID: "up1"}, Parts: []domain.Part{{Number: 2, ETag: "pe", Size: 4, ModTime: now}}, IsTruncated: true, NextPartNumberMarker: 2}, nil).Once()
	rec = do(h, httptest.NewRequest(http.MethodGet, "/b/big?uploadId=up1&max-parts=5&part-number-marker=1", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var parts s3.ListPartsResult
	require.NoError(t, xml.Unmarshal(rec.Body.Bytes(), &parts))
	assert.Equal(t, 2, parts.NextPartNumberMarker)
	assert.Equal(t, "STANDARD", parts.StorageClass)
	require.Len(t, parts.Parts, 1)

	d.objects.EXPECT().ListMultipartUploads(mock.Anything, domain.ListMultipartUploadsInput{Bucket: "b", Prefix: "bi", MaxUploads: 1000}).
		Return(domain.ListMultipartUploadsResult{Uploads: []domain.MultipartUpload{{Key: "big", UploadID: "up1", Initiated: now}}}, nil).Once()
	rec = do(h, httptest.NewRequest(http.MethodGet, "/b?uploads&prefix=bi", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "<UploadId>up1</UploadId>")

	d.objects.EXPECT().CompleteMultipartUpload(mock.Anything, object.CompleteMultipartInput{
		Bucket: "b", Key: "big", UploadID: "up1",
		Parts: []domain.CompletePart{{Number: 2, ETag: `"pe"`}, {Number: 3, ETag: "ce", Checksum: domain.Checksum{Algorithm: domain.ChecksumSHA256, Value: "abc="}}},
	}).Return(domain.ObjectInfo{ETag: "final-2", VersionID: "v1"}, nil).Once()
	req = httptest.NewRequest(http.MethodPost, "/b/big?uploadId=up1", strings.NewReader(`<CompleteMultipartUpload><Part><PartNumber>2</PartNumber><ETag>"pe"</ETag></Part><Part><PartNumber>3</PartNumber><ETag>ce</ETag><ChecksumSHA256>abc=</ChecksumSHA256></Part></CompleteMultipartUpload>`))
	req.Host = "s3.local:9000"
	rec = do(h, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "<Location>http://s3.local:9000/b/big</Location>")
	assert.Contains(t, rec.Body.String(), `<ETag>&#34;final-2&#34;</ETag>`)
	assert.Equal(t, "v1", rec.Header().Get("x-amz-version-id"))

	d.objects.EXPECT().AbortMultipartUpload(mock.Anything, "b", "big", "up1").Return(nil).Once()
	rec = do(h, httptest.NewRequest(http.MethodDelete, "/b/big?uploadId=up1", nil))
	assert.Equal(t, http.StatusNoContent, rec.Code)
	d.objects.EXPECT().AbortMultipartUpload(mock.Anything, "b", "big", "nope").Return(domain.ErrNoSuchUpload).Once()
	rec = do(h, httptest.NewRequest(http.MethodDelete, "/b/big?uploadId=nope", nil))
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Equal(t, "NoSuchUpload", s3Error(t, rec).Code)
}

func TestAuthorization(t *testing.T) {
	d := newDeps(t)
	h := d.api(true)
	alice := &domain.Identity{AccessKey: "alice"}
	withIdentity := func(req *http.Request, id *domain.Identity) *http.Request {
		return req.WithContext(middleware.WithIdentity(req.Context(), id))
	}
	bucketPolicy := json.RawMessage(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::pub/*"}]}`)

	// The bucket policy is loaded and handed to the engine; a denial is a 403.
	d.buckets.EXPECT().GetPolicy(mock.Anything, "pub").Return(bucketPolicy, nil).Once()
	d.policy.EXPECT().Evaluate(mock.Anything, mock.MatchedBy(func(a policy.Args) bool {
		return a.Identity.AccessKey == "alice" && a.Action == policy.ActionGetObject && a.Bucket == "pub" && a.Object == "k" &&
			a.BucketPolicy != nil && len(a.Conditions["aws:SourceIp"]) == 1 && a.Conditions["aws:SecureTransport"][0] == "false"
	})).Return(policy.Decision{Allowed: false, Reason: "nope"}, nil).Once()
	rec := do(h, withIdentity(httptest.NewRequest(http.MethodGet, "/pub/k", nil), alice))
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, "AccessDenied", s3Error(t, rec).Code)

	// Anonymous requests reach the engine as anonymous; allowed → served.
	d.buckets.EXPECT().GetPolicy(mock.Anything, "pub").Return(bucketPolicy, nil).Once()
	d.policy.EXPECT().Evaluate(mock.Anything, mock.MatchedBy(func(a policy.Args) bool { return a.Identity.IsAnonymous })).Return(policy.Decision{Allowed: true}, nil).Once()
	d.objects.EXPECT().GetObjectInfo(mock.Anything, "pub", "k", mock.Anything).Return(domain.ObjectInfo{Size: 1, ModTime: now}, nil).Once()
	rec = do(h, httptest.NewRequest(http.MethodHead, "/pub/k", nil))
	assert.Equal(t, http.StatusOK, rec.Code)

	// Root skips the bucket policy lookup; errors from the engine propagate.
	d.policy.EXPECT().Evaluate(mock.Anything, mock.Anything).Return(policy.Decision{}, errors.New("engine down")).Once()
	rec = do(h, withIdentity(httptest.NewRequest(http.MethodGet, "/", nil), &domain.Identity{IsRoot: true}))
	assert.Equal(t, http.StatusInternalServerError, rec.Code)

	// A missing bucket policy is fine; ListBuckets has no bucket.
	d.buckets.EXPECT().GetPolicy(mock.Anything, "b").Return(nil, domain.ErrPolicyNotFound).Once()
	d.policy.EXPECT().Evaluate(mock.Anything, mock.MatchedBy(func(a policy.Args) bool { return a.BucketPolicy == nil && a.Action == policy.ActionDeleteBucket })).Return(policy.Decision{Allowed: true}, nil).Once()
	d.buckets.EXPECT().Delete(mock.Anything, "b", bucket.DeleteOptions{}).Return(nil).Once()
	rec = do(h, withIdentity(httptest.NewRequest(http.MethodDelete, "/b", nil), alice))
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// DeleteObjects reports per-key denials instead of failing the batch.
	d.buckets.EXPECT().GetPolicy(mock.Anything, "b").Return(nil, domain.ErrPolicyNotFound).Twice()
	d.policy.EXPECT().Evaluate(mock.Anything, mock.MatchedBy(func(a policy.Args) bool { return a.Object == "ok" })).Return(policy.Decision{Allowed: true}, nil).Once()
	d.policy.EXPECT().Evaluate(mock.Anything, mock.MatchedBy(func(a policy.Args) bool { return a.Object == "denied" })).Return(policy.Decision{Allowed: false}, nil).Once()
	d.objects.EXPECT().DeleteObjects(mock.Anything, object.DeleteObjectsInput{Bucket: "b", Objects: []object.ObjectToDelete{{Key: "ok"}}}).
		Return(object.DeleteObjectsResult{Deleted: []object.DeletedObject{{Key: "ok"}}}, nil).Once()
	rec = do(h, withIdentity(httptest.NewRequest(http.MethodPost, "/b?delete", strings.NewReader(`<Delete><Object><Key>ok</Key></Object><Object><Key>denied</Key></Object></Delete>`)), alice))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var dr s3.DeleteResult
	require.NoError(t, xml.Unmarshal(rec.Body.Bytes(), &dr))
	require.Len(t, dr.Deleted, 1)
	require.Len(t, dr.Errors, 1)
	assert.Equal(t, "AccessDenied", dr.Errors[0].Code)
}

func TestPresignedDelegation(t *testing.T) {
	d := newDeps(t)
	h := d.api(false)
	target := "/b/k?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=" + strings.Repeat("0", 64)

	// Without an identity in the context the group verifies the signature itself.
	d.verifier.EXPECT().VerifyPresigned(mock.Anything, mock.Anything).Return(nil, domain.ErrSignatureMismatch).Once()
	rec := do(h, httptest.NewRequest(http.MethodGet, target, nil))
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, "SignatureDoesNotMatch", s3Error(t, rec).Code)

	d.verifier.EXPECT().VerifyPresigned(mock.Anything, mock.Anything).Return(&domain.Identity{AccessKey: "alice"}, nil).Once()
	d.objects.EXPECT().GetObject(mock.Anything, "b", "k", mock.Anything).
		Return(&object.Reader{ReadCloser: io.NopCloser(strings.NewReader("data")), Info: domain.ObjectInfo{Size: 4, ModTime: now}, Length: 4}, nil).Once()
	rec = do(h, httptest.NewRequest(http.MethodGet, target, nil))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "data", rec.Body.String())

	// With an identity already established (auth middleware) no verification happens.
	d.objects.EXPECT().ListObjects(mock.Anything, mock.MatchedBy(func(in domain.ListObjectsInput) bool { return in.Bucket == "b" })).Return(domain.ListObjectsResult{}, nil).Once()
	req := httptest.NewRequest(http.MethodGet, "/b?list-type=2&X-Amz-Signature="+strings.Repeat("0", 64), nil)
	req = req.WithContext(middleware.WithIdentity(req.Context(), &domain.Identity{AccessKey: "alice"}))
	rec = do(h, req)
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "<KeyCount>0</KeyCount>")
}

func TestPostObject(t *testing.T) {
	d := newDeps(t)
	h := d.api(false)
	build := func(fields map[string]string, file string) (*bytes.Buffer, string) {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		for k, v := range fields {
			_ = mw.WriteField(k, v)
		}
		fw, _ := mw.CreateFormFile("file", "photo.png")
		_, _ = fw.Write([]byte(file))
		_ = mw.Close()
		return &body, mw.FormDataContentType()
	}
	fields := map[string]string{"key": "uploads/${filename}", "policy": "cG9saWN5", "x-amz-signature": "sig", "success_action_status": "201", "x-amz-meta-tag": "a", "Content-Type": "image/png"}
	postPolicy := &auth.PostPolicy{Conditions: []auth.PostCondition{{Operator: auth.ConditionLengthRange, Min: 1, Max: 100}}}

	d.verifier.EXPECT().VerifyPostPolicy(mock.Anything, mock.MatchedBy(func(f auth.PostForm) bool {
		return f.Bucket == "b" && f.FileName == "photo.png" && f.Fields["key"] == "uploads/${filename}" && f.Fields["content-type"] == "image/png"
	})).Return(&domain.Identity{AccessKey: "alice"}, postPolicy, nil).Once()
	d.objects.EXPECT().PutObject(mock.Anything, mock.MatchedBy(func(in object.PutObjectInput) bool {
		data, _ := io.ReadAll(in.Reader)
		return in.Bucket == "b" && in.Key == "uploads/photo.png" && string(data) == "png-bytes" && in.Size == -1 && in.ContentType == "image/png" && in.UserMetadata["tag"] == "a"
	})).Return(domain.ObjectInfo{ETag: "pe", Size: 9}, nil).Once()
	body, ct := build(fields, "png-bytes")
	req := httptest.NewRequest(http.MethodPost, "/b", body)
	req.Header.Set("Content-Type", ct)
	req.Host = "s3.local"
	rec := do(h, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "<Location>http://s3.local/b/uploads/photo.png</Location>")
	assert.Equal(t, `"pe"`, rec.Header().Get("ETag"))

	// Oversized uploads are removed again.
	d.verifier.EXPECT().VerifyPostPolicy(mock.Anything, mock.Anything).Return(&domain.Identity{AccessKey: "alice"}, postPolicy, nil).Once()
	d.objects.EXPECT().PutObject(mock.Anything, mock.Anything).Return(domain.ObjectInfo{ETag: "pe", Size: 101}, nil).Once()
	d.objects.EXPECT().DeleteObject(mock.Anything, "b", "uploads/photo.png", mock.Anything).Return(object.DeletedObject{}, nil).Once()
	body, ct = build(fields, strings.Repeat("x", 101))
	req = httptest.NewRequest(http.MethodPost, "/b", body)
	req.Header.Set("Content-Type", ct)
	rec = do(h, req)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "EntityTooLarge", s3Error(t, rec).Code)

	// Verification failures and malformed forms.
	d.verifier.EXPECT().VerifyPostPolicy(mock.Anything, mock.Anything).Return(nil, nil, domain.ErrAccessDenied).Once()
	body, ct = build(fields, "x")
	req = httptest.NewRequest(http.MethodPost, "/b", body)
	req.Header.Set("Content-Type", ct)
	rec = do(h, req)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	rec = do(h, httptest.NewRequest(http.MethodPost, "/b", strings.NewReader("plain")))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "MalformedPOSTRequest", s3Error(t, rec).Code)
	// Redirect on success.
	d.verifier.EXPECT().VerifyPostPolicy(mock.Anything, mock.Anything).Return(&domain.Identity{AccessKey: "alice"}, &auth.PostPolicy{}, nil).Once()
	d.objects.EXPECT().PutObject(mock.Anything, mock.Anything).Return(domain.ObjectInfo{ETag: "pe", Size: 1}, nil).Once()
	redirectFields := map[string]string{"key": "k", "policy": "p", "x-amz-signature": "s", "success_action_redirect": "https://app.example.com/done"}
	body, ct = build(redirectFields, "x")
	req = httptest.NewRequest(http.MethodPost, "/b", body)
	req.Header.Set("Content-Type", ct)
	rec = do(h, req)
	assert.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, "https://app.example.com/done", rec.Header().Get("Location"))
}

func TestHealthRoutes(t *testing.T) {
	ready := true
	svc := health.New(health.Deps{Readiness: map[string]health.Check{"storage": func(context.Context) error {
		if ready {
			return nil
		}
		return errors.New("no quorum")
	}}})
	r := chi.NewRouter()
	s3.NewHealthRoutes(s3.HealthDeps{Health: svc}).Register(r)
	rec := do(r, httptest.NewRequest(http.MethodGet, s3.PathHealthLive, nil))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	var st health.Status
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &st))
	assert.True(t, st.OK)
	rec = do(r, httptest.NewRequest(http.MethodHead, s3.PathHealthLive, nil))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Body.String())
	ready = false
	rec = do(r, httptest.NewRequest(http.MethodGet, s3.PathHealthReady, nil))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "no quorum")
	rec = do(r, httptest.NewRequest(http.MethodGet, s3.PathHealthCluster, nil))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	var cs health.ClusterStatus
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &cs))
	assert.Equal(t, 1, cs.Nodes)
}
