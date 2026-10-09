package s3_test

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
	"github.com/r3n3r0/nexo/internal/transport/http/s3"
)

func contentMD5(body string) string {
	sum := md5.Sum([]byte(body))
	return base64.StdEncoding.EncodeToString(sum[:])
}

func hexMD5(body string) string {
	sum := md5.Sum([]byte(body))
	return hex.EncodeToString(sum[:])
}

func TestListBuckets(t *testing.T) {
	f := newFixture(t, asIdentity(&domain.Identity{AccessKey: "svc", Parent: "alice"}))
	f.buckets.EXPECT().ListPage(mock.Anything, bucket.ListPageInput{Prefix: "ph"}).Return(bucket.ListPageResult{Buckets: []domain.BucketInfo{
		{Bucket: domain.Bucket{Name: "photos", Created: now, Region: "us-east-1"}},
		{Bucket: domain.Bucket{Name: "phones", Created: now.Add(time.Hour)}},
	}}, nil).Once()
	rec := f.do(http.MethodGet, "/?prefix=ph", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var list s3.ListAllMyBucketsResult
	decodeXML(t, rec, &list)
	require.Len(t, list.Buckets.Bucket, 2)
	assert.Equal(t, s3.BucketEntry{Name: "photos", CreationDate: "2026-10-08T12:00:00.000Z", BucketRegion: "us-east-1"}, list.Buckets.Bucket[0])
	assert.Equal(t, "phones", list.Buckets.Bucket[1].Name)
	assert.Equal(t, s3.Owner{ID: "alice", DisplayName: "alice"}, list.Owner, "derived credentials act as their parent")
	assert.Equal(t, "ph", list.Prefix)
	assert.Contains(t, rec.Body.String(), `xmlns="http://s3.amazonaws.com/doc/2006-03-01/"`)

	assert.NotContains(t, rec.Body.String(), "<ContinuationToken>", "a single page")

	// max-buckets and continuation-token page the listing.
	f.buckets.EXPECT().ListPage(mock.Anything, bucket.ListPageInput{MaxBuckets: 1, ContinuationToken: "tok"}).Return(bucket.ListPageResult{
		Buckets: []domain.BucketInfo{{Bucket: domain.Bucket{Name: "photos", Created: now}}}, ContinuationToken: "next",
	}, nil).Once()
	rec = f.do(http.MethodGet, "/?max-buckets=1&continuation-token=tok", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	list = s3.ListAllMyBucketsResult{}
	decodeXML(t, rec, &list)
	assert.Equal(t, "next", list.ContinuationToken)
	for _, bad := range []string{"0", "-1", "x"} {
		requireError(t, f.do(http.MethodGet, "/?max-buckets="+bad, ""), http.StatusBadRequest, "InvalidArgument")
	}

	f.buckets.EXPECT().ListPage(mock.Anything, bucket.ListPageInput{}).Return(bucket.ListPageResult{}, domain.ErrReadQuorum).Once()
	requireError(t, f.do(http.MethodGet, "/", ""), http.StatusServiceUnavailable, "SlowDown")
}

func TestCreateBucket(t *testing.T) {
	f := newFixture(t, withOptions(s3.WithRegion("eu-south-1")))
	cfg := `<CreateBucketConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><LocationConstraint>eu-south-1</LocationConstraint></CreateBucketConfiguration>`
	f.buckets.EXPECT().Create(mock.Anything, bucket.CreateInput{Name: "photos", Region: "eu-south-1"}).Return(&domain.Bucket{Name: "photos"}, nil).Once()
	rec := f.do(http.MethodPut, "/photos", cfg, "Content-MD5", contentMD5(cfg))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "/photos", rec.Header().Get("Location"))

	f.buckets.EXPECT().Create(mock.Anything, bucket.CreateInput{Name: "photos"}).Return(nil, domain.ErrBucketAlreadyExists).Once()
	requireError(t, f.do(http.MethodPut, "/photos", "", "x-amz-acl", "private"), http.StatusConflict, "BucketAlreadyOwnedByYou")
	f.buckets.EXPECT().Create(mock.Anything, bucket.CreateInput{Name: "Bad_Name"}).Return(nil, domain.ErrInvalidBucketName).Once()
	requireError(t, f.do(http.MethodPut, "/Bad_Name", ""), http.StatusBadRequest, "InvalidBucketName")

	tests := []struct {
		name, body string
		headers    []string
		status     int
		code       string
	}{
		{"other region", `<CreateBucketConfiguration><LocationConstraint>us-west-2</LocationConstraint></CreateBucketConfiguration>`, nil, 400, "InvalidLocationConstraint"},
		{"malformed", "<not xml", nil, 400, "MalformedXML"},
		{"bad digest", cfg, []string{"Content-MD5", contentMD5("other")}, 400, "BadDigest"},
		{"invalid digest", cfg, []string{"Content-MD5", "not-base64!"}, 400, "InvalidDigest"},
		{"object lock flag", "", []string{"x-amz-bucket-object-lock-enabled", "maybe"}, 400, "InvalidArgument"},
		{"public ACL", "", []string{"x-amz-acl", "public-read"}, 501, "NotImplemented"},
		{"ACL grant", "", []string{"x-amz-grant-read", "uri=everyone"}, 501, "NotImplemented"},
		{"oversized", "<CreateBucketConfiguration>" + strings.Repeat(" ", 1<<20) + "</CreateBucketConfiguration>", nil, 400, "MaxMessageLengthExceeded"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			requireError(t, f.do(http.MethodPut, "/photos", tc.body, tc.headers...), tc.status, tc.code)
		})
	}
}

func TestCreateBucketWithObjectLock(t *testing.T) {
	f := newFixture(t)
	f.buckets.EXPECT().Create(mock.Anything, bucket.CreateInput{Name: "vault", ObjectLock: true}).Return(&domain.Bucket{Name: "vault"}, nil).Once()
	rec := f.do(http.MethodPut, "/vault", "", "x-amz-bucket-object-lock-enabled", "True")
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Object locking needs s3:PutBucketObjectLockConfiguration too.
	alice := &domain.Identity{AccessKey: "alice"}
	f = newFixture(t, asIdentity(alice), withStrictPolicy())
	f.policy.EXPECT().Evaluate(mock.Anything, mock.MatchedBy(func(a policy.Args) bool { return a.Action == policy.ActionCreateBucket })).
		Return(policy.Decision{Allowed: true}, nil).Once()
	f.policy.EXPECT().Evaluate(mock.Anything, mock.MatchedBy(func(a policy.Args) bool { return a.Action == "s3:PutBucketObjectLockConfiguration" })).
		Return(policy.Decision{Allowed: false}, nil).Once()
	requireError(t, f.do(http.MethodPut, "/vault", "", "x-amz-bucket-object-lock-enabled", "true"), http.StatusForbidden, "AccessDenied")
}

func TestCreateBucketWithoutRegionAcceptsAnyConstraint(t *testing.T) {
	f := newFixture(t)
	f.buckets.EXPECT().Create(mock.Anything, bucket.CreateInput{Name: "b", Region: "ap-south-1"}).Return(&domain.Bucket{Name: "b"}, nil).Once()
	rec := f.do(http.MethodPut, "/b", `<CreateBucketConfiguration><LocationConstraint>ap-south-1</LocationConstraint></CreateBucketConfiguration>`)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestHeadAndDeleteBucket(t *testing.T) {
	f := newFixture(t, withOptions(s3.WithRegion("eu-south-1")))
	f.buckets.EXPECT().Get(mock.Anything, "photos").Return(&domain.BucketInfo{Bucket: domain.Bucket{Name: "photos", Region: "eu-west-1"}}, nil).Once()
	rec := f.do(http.MethodHead, "/photos", "")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "eu-west-1", rec.Header().Get("x-amz-bucket-region"))
	f.buckets.EXPECT().Get(mock.Anything, "legacy").Return(&domain.BucketInfo{Bucket: domain.Bucket{Name: "legacy"}}, nil).Once()
	rec = f.do(http.MethodHead, "/legacy", "")
	assert.Equal(t, "eu-south-1", rec.Header().Get("x-amz-bucket-region"), "buckets without region are in the deployment's")
	f.buckets.EXPECT().Get(mock.Anything, "missing").Return(nil, domain.ErrBucketNotFound).Once()
	rec = f.do(http.MethodHead, "/missing", "")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Empty(t, rec.Body.String(), "HEAD errors have no body")

	f.buckets.EXPECT().Delete(mock.Anything, "photos", bucket.DeleteOptions{}).Return(nil).Once()
	assert.Equal(t, http.StatusNoContent, f.do(http.MethodDelete, "/photos", "").Code)
	f.buckets.EXPECT().Delete(mock.Anything, "full", bucket.DeleteOptions{}).Return(domain.ErrBucketNotEmpty).Once()
	requireError(t, f.do(http.MethodDelete, "/full", ""), http.StatusConflict, "BucketNotEmpty")
	f.buckets.EXPECT().Delete(mock.Anything, "full", bucket.DeleteOptions{Force: true}).Return(nil).Once()
	assert.Equal(t, http.StatusNoContent, f.do(http.MethodDelete, "/full", "", "x-nexo-force-delete", "true").Code)
}

func TestListObjectsV1(t *testing.T) {
	f := newFixture(t)
	f.objects.EXPECT().ListObjects(mock.Anything, domain.ListObjectsInput{Bucket: "photos", Prefix: "a b/", Marker: "a b/0", MaxKeys: 2}).
		Return(domain.ListObjectsResult{
			Objects:     []domain.ObjectInfo{{Key: "a b/1+1.jpg", Size: 10, ETag: "e1", ModTime: now}, {Key: "a b/ü.jpg", Size: 3, ETag: "e2", ModTime: now, StorageClass: "REDUCED_REDUNDANCY"}},
			IsTruncated: true,
		}, nil).Once()
	rec := f.do(http.MethodGet, "/photos?prefix=a+b/&marker=a+b/0&max-keys=2&encoding-type=url", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var v1 s3.ListBucketResultV1
	decodeXML(t, rec, &v1)
	assert.Equal(t, "url", v1.EncodingType)
	assert.Equal(t, "a b/", v1.Prefix, "the V1 prefix is not encoded, as in S3")
	assert.Equal(t, "a%20b/0", v1.Marker)
	assert.Equal(t, "a%20b/1%2B1.jpg", v1.Contents[0].Key)
	assert.Equal(t, "a%20b/%C3%BC.jpg", v1.Contents[1].Key)
	assert.Equal(t, "a%20b/%C3%BC.jpg", v1.NextMarker, "the last key resumes a truncated listing")
	assert.Equal(t, "STANDARD", v1.Contents[0].StorageClass)
	assert.Equal(t, "REDUCED_REDUNDANCY", v1.Contents[1].StorageClass)
	assert.Equal(t, `"e1"`, v1.Contents[0].ETag)
	require.NotNil(t, v1.Contents[0].Owner, "v1 always reports the owner")
	assert.Equal(t, 2, v1.MaxKeys)

	f.objects.EXPECT().ListObjects(mock.Anything, domain.ListObjectsInput{Bucket: "photos", Delimiter: "/", MaxKeys: 1000}).
		Return(domain.ListObjectsResult{CommonPrefixes: []string{"x/"}, IsTruncated: true, NextMarker: "x/"}, nil).Once()
	rec = f.do(http.MethodGet, "/photos?delimiter=/&max-keys=5000", "")
	v1 = s3.ListBucketResultV1{}
	decodeXML(t, rec, &v1)
	assert.Equal(t, 1000, v1.MaxKeys, "max-keys is capped at 1000")
	assert.Equal(t, "x/", v1.NextMarker)
	assert.Equal(t, "x/", v1.CommonPrefixes[0].Prefix)

	requireError(t, f.do(http.MethodGet, "/photos?max-keys=-1", ""), http.StatusBadRequest, "InvalidArgument")
	requireError(t, f.do(http.MethodGet, "/photos?max-keys=abc", ""), http.StatusBadRequest, "InvalidArgument")
	requireError(t, f.do(http.MethodGet, "/photos?encoding-type=base64", ""), http.StatusBadRequest, "InvalidArgument")
	f.objects.EXPECT().ListObjects(mock.Anything, mock.Anything).Return(domain.ListObjectsResult{}, domain.ErrBucketNotFound).Once()
	requireError(t, f.do(http.MethodGet, "/nope", ""), http.StatusNotFound, "NoSuchBucket")
}

func TestListObjectsV2(t *testing.T) {
	f := newFixture(t)
	f.objects.EXPECT().ListObjects(mock.Anything, domain.ListObjectsInput{
		Bucket: "photos", Prefix: "2024/", Delimiter: "/", MaxKeys: 2, ContinuationToken: "tok", StartAfter: "2024/0", FetchOwner: true,
	}).Return(domain.ListObjectsResult{
		Objects:        []domain.ObjectInfo{{Key: "2024/a.jpg", Size: 10, ETag: "e1", ModTime: now}},
		CommonPrefixes: []string{"2024/raw dir/"}, IsTruncated: true, NextContinuationToken: "next",
	}, nil).Once()
	rec := f.do(http.MethodGet, "/photos?list-type=2&prefix=2024/&delimiter=/&max-keys=2&continuation-token=tok&start-after=2024/0&fetch-owner=true&encoding-type=url", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var v2 s3.ListBucketResultV2
	decodeXML(t, rec, &v2)
	assert.Equal(t, 2, v2.KeyCount)
	assert.True(t, v2.IsTruncated)
	require.NotNil(t, v2.ContinuationToken)
	assert.Equal(t, "tok", *v2.ContinuationToken)
	assert.Equal(t, "next", v2.NextContinuationToken)
	assert.Equal(t, "2024/0", v2.StartAfter)
	assert.Equal(t, "2024/raw%20dir/", v2.CommonPrefixes[0].Prefix)
	require.NotNil(t, v2.Contents[0].Owner)
	assert.Equal(t, "root", v2.Contents[0].Owner.ID)

	f.objects.EXPECT().ListObjects(mock.Anything, domain.ListObjectsInput{Bucket: "photos", MaxKeys: 1000}).Return(domain.ListObjectsResult{
		Objects: []domain.ObjectInfo{{Key: "k", ModTime: now}},
	}, nil).Once()
	rec = f.do(http.MethodGet, "/photos?list-type=2", "")
	v2 = s3.ListBucketResultV2{}
	decodeXML(t, rec, &v2)
	assert.Nil(t, v2.Contents[0].Owner, "the owner is reported with fetch-owner only")
	assert.NotContains(t, rec.Body.String(), "<Owner>")
	assert.NotContains(t, rec.Body.String(), "EncodingType")
	assert.NotContains(t, rec.Body.String(), "<ContinuationToken>", "no token, none echoed")

	// An empty token is echoed (S3 behaves the same).
	f.objects.EXPECT().ListObjects(mock.Anything, domain.ListObjectsInput{Bucket: "photos", MaxKeys: 1000}).Return(domain.ListObjectsResult{}, nil).Once()
	rec = f.do(http.MethodGet, "/photos?list-type=2&continuation-token=", "")
	assert.Contains(t, rec.Body.String(), "<ContinuationToken></ContinuationToken>")
}

func TestListObjectVersions(t *testing.T) {
	f := newFixture(t)
	f.objects.EXPECT().ListObjectVersions(mock.Anything, domain.ListObjectsInput{
		Bucket: "photos", Marker: "k", VersionIDMarker: "v0", MaxKeys: 1000, IncludeVersions: true,
	}).Return(domain.ListObjectsResult{Objects: []domain.ObjectInfo{
		{Key: "k", VersionID: "v2", IsLatest: true, IsDeleteMarker: true, ModTime: now},
		{Key: "k", VersionID: "", Size: 3, ETag: "e", ModTime: now},
	}, CommonPrefixes: []string{"d/"}, IsTruncated: true, NextMarker: "k", NextVersionIDMarker: "null"}, nil).Once()
	rec := f.do(http.MethodGet, "/photos?versions&key-marker=k&version-id-marker=v0", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var versions s3.ListVersionsResult
	decodeXML(t, rec, &versions)
	require.Len(t, versions.Versions, 1)
	require.Len(t, versions.DeleteMarkers, 1)
	assert.Equal(t, "null", versions.Versions[0].VersionID)
	assert.Equal(t, `"e"`, versions.Versions[0].ETag)
	assert.Equal(t, "STANDARD", versions.Versions[0].StorageClass)
	assert.True(t, versions.DeleteMarkers[0].IsLatest)
	assert.Equal(t, "v2", versions.DeleteMarkers[0].VersionID)
	assert.Equal(t, "k", versions.NextKeyMarker)
	assert.Equal(t, "null", versions.NextVersionIDMarker)
	assert.Equal(t, "v0", versions.VersionIDMarker)
	assert.Equal(t, "d/", versions.CommonPrefixes[0].Prefix)

	requireError(t, f.do(http.MethodGet, "/photos?versions&version-id-marker=v0", ""), http.StatusBadRequest, "InvalidArgument")
	requireError(t, f.do(http.MethodGet, "/photos?versions&encoding-type=x", ""), http.StatusBadRequest, "InvalidArgument")
	f.objects.EXPECT().ListObjectVersions(mock.Anything, mock.Anything).Return(domain.ListObjectsResult{}, domain.ErrBucketNotFound).Once()
	requireError(t, f.do(http.MethodGet, "/photos?versions", ""), http.StatusNotFound, "NoSuchBucket")
}

func TestGetBucketLocation(t *testing.T) {
	tests := []struct {
		name, configured, bucketRegion, want string
	}{
		{"default region is empty", "", "us-east-1", ""},
		{"bucket region", "", "eu-south-1", "eu-south-1"},
		{"deployment region", "eu-west-3", "", "eu-west-3"},
		{"no region at all", "", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, withOptions(s3.WithRegion(tc.configured)))
			f.buckets.EXPECT().Get(mock.Anything, "b").Return(&domain.BucketInfo{Bucket: domain.Bucket{Name: "b", Region: tc.bucketRegion}}, nil).Once()
			rec := f.do(http.MethodGet, "/b?location", "")
			var loc s3.LocationConstraint
			decodeXML(t, rec, &loc)
			assert.Equal(t, tc.want, loc.Location)
		})
	}
	f := newFixture(t)
	f.buckets.EXPECT().Get(mock.Anything, "b").Return(nil, domain.ErrBucketNotFound).Once()
	requireError(t, f.do(http.MethodGet, "/b?location", ""), http.StatusNotFound, "NoSuchBucket")
}

func TestBucketPolicy(t *testing.T) {
	f := newFixture(t)
	doc := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::photos/*"}]}`
	f.buckets.EXPECT().SetPolicy(mock.Anything, "photos", json.RawMessage(doc)).Return(nil).Once()
	assert.Equal(t, http.StatusNoContent, f.do(http.MethodPut, "/photos?policy", doc).Code)
	f.buckets.EXPECT().SetPolicy(mock.Anything, "photos", mock.Anything).
		Return(domain.NewS3Error(domain.ErrInvalidArgument, "MalformedPolicy", "Policy has invalid action")).Once()
	requireError(t, f.do(http.MethodPut, "/photos?policy", `{"Statement":[]}`), http.StatusBadRequest, "MalformedPolicy")
	requireError(t, f.do(http.MethodPut, "/photos?policy", ""), http.StatusBadRequest, "MalformedPolicy")
	requireError(t, f.do(http.MethodPut, "/photos?policy", strings.Repeat("x", 20<<10+1)), http.StatusBadRequest, "PolicyTooLarge")
	requireError(t, f.do(http.MethodPut, "/photos?policy", doc, "Content-MD5", contentMD5("x")), http.StatusBadRequest, "BadDigest")

	f.buckets.EXPECT().GetPolicy(mock.Anything, "photos").Return(json.RawMessage(doc), nil).Once()
	rec := f.do(http.MethodGet, "/photos?policy", "")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, httpx.ContentTypeJSON, rec.Header().Get("Content-Type"))
	assert.JSONEq(t, doc, rec.Body.String())
	f.buckets.EXPECT().GetPolicy(mock.Anything, "photos").Return(nil, domain.NewS3Error(domain.ErrPolicyNotFound, "NoSuchBucketPolicy", "")).Once()
	requireError(t, f.do(http.MethodGet, "/photos?policy", ""), http.StatusNotFound, "NoSuchBucketPolicy")

	f.buckets.EXPECT().DeletePolicy(mock.Anything, "photos").Return(nil).Once()
	assert.Equal(t, http.StatusNoContent, f.do(http.MethodDelete, "/photos?policy", "").Code)
	f.buckets.EXPECT().DeletePolicy(mock.Anything, "nope").Return(domain.ErrBucketNotFound).Once()
	requireError(t, f.do(http.MethodDelete, "/nope?policy", ""), http.StatusNotFound, "NoSuchBucket")

	f.buckets.EXPECT().IsPublic(mock.Anything, "photos").Return(true, nil).Once()
	rec = f.do(http.MethodGet, "/photos?policyStatus", "")
	var st s3.PolicyStatus
	decodeXML(t, rec, &st)
	assert.True(t, st.IsPublic)
	f.buckets.EXPECT().IsPublic(mock.Anything, "nopolicy").Return(false, domain.NewS3Error(domain.ErrPolicyNotFound, "NoSuchBucketPolicy", "")).Once()
	requireError(t, f.do(http.MethodGet, "/nopolicy?policyStatus", ""), http.StatusNotFound, "NoSuchBucketPolicy")
}

func TestBucketVersioning(t *testing.T) {
	f := newFixture(t)
	f.buckets.EXPECT().SetVersioning(mock.Anything, "photos", domain.VersioningEnabled).Return(nil).Once()
	rec := f.do(http.MethodPut, "/photos?versioning", `<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Status>Enabled</Status></VersioningConfiguration>`)
	assert.Equal(t, http.StatusOK, rec.Code)
	f.buckets.EXPECT().SetVersioning(mock.Anything, "locked", domain.VersioningSuspended).
		Return(domain.NewS3Error(domain.ErrConflict, "InvalidBucketState", "")).Once()
	requireError(t, f.do(http.MethodPut, "/locked?versioning", `<VersioningConfiguration><Status>Suspended</Status></VersioningConfiguration>`), http.StatusConflict, "InvalidBucketState")
	for _, body := range []string{
		`<VersioningConfiguration><Status>Maybe</Status></VersioningConfiguration>`,
		`<VersioningConfiguration></VersioningConfiguration>`,
	} {
		requireError(t, f.do(http.MethodPut, "/photos?versioning", body), http.StatusBadRequest, "IllegalVersioningConfigurationException")
	}
	requireError(t, f.do(http.MethodPut, "/photos?versioning", `<VersioningConfiguration><Status>Enabled</Status><MfaDelete>Enabled</MfaDelete></VersioningConfiguration>`), http.StatusNotImplemented, "NotImplemented")
	requireError(t, f.do(http.MethodPut, "/photos?versioning", ""), http.StatusBadRequest, "MalformedXML")

	f.buckets.EXPECT().GetVersioning(mock.Anything, "photos").Return(domain.VersioningSuspended, nil).Once()
	var cfg s3.VersioningConfiguration
	decodeXML(t, f.do(http.MethodGet, "/photos?versioning", ""), &cfg)
	assert.Equal(t, "Suspended", cfg.Status)
	f.buckets.EXPECT().GetVersioning(mock.Anything, "new").Return(domain.VersioningOff, nil).Once()
	rec = f.do(http.MethodGet, "/new?versioning", "")
	assert.NotContains(t, rec.Body.String(), "<Status>", "a bucket never versioned has no status")
	f.buckets.EXPECT().GetVersioning(mock.Anything, "nope").Return(domain.VersioningOff, domain.ErrBucketNotFound).Once()
	requireError(t, f.do(http.MethodGet, "/nope?versioning", ""), http.StatusNotFound, "NoSuchBucket")
}

func TestBucketTagging(t *testing.T) {
	f := newFixture(t)
	doc := `<Tagging><TagSet><Tag><Key>a</Key><Value>1</Value></Tag><Tag><Key>b</Key><Value>2</Value></Tag></TagSet></Tagging>`
	f.buckets.EXPECT().SetTags(mock.Anything, "photos", map[string]string{"a": "1", "b": "2"}).Return(nil).Once()
	assert.Equal(t, http.StatusNoContent, f.do(http.MethodPut, "/photos?tagging", doc, "Content-MD5", contentMD5(doc)).Code)
	requireError(t, f.do(http.MethodPut, "/photos?tagging", `<Tagging><TagSet><Tag><Key>a</Key><Value>1</Value></Tag><Tag><Key>a</Key><Value>2</Value></Tag></TagSet></Tagging>`),
		http.StatusBadRequest, "InvalidTag")
	requireError(t, f.do(http.MethodPut, "/photos?tagging", "<Tagging>"), http.StatusBadRequest, "MalformedXML")
	f.buckets.EXPECT().SetTags(mock.Anything, "photos", mock.Anything).Return(domain.NewS3Error(domain.ErrInvalidArgument, "InvalidTag", "too many tags")).Once()
	requireError(t, f.do(http.MethodPut, "/photos?tagging", doc), http.StatusBadRequest, "InvalidTag")

	f.buckets.EXPECT().GetTags(mock.Anything, "photos").Return(map[string]string{"b": "2", "a": "1"}, nil).Once()
	rec := f.do(http.MethodGet, "/photos?tagging", "")
	assert.Contains(t, rec.Body.String(), "<TagSet><Tag><Key>a</Key><Value>1</Value></Tag><Tag><Key>b</Key><Value>2</Value></Tag></TagSet>", "tags are sorted")
	f.buckets.EXPECT().GetTags(mock.Anything, "none").Return(nil, domain.NewS3Error(domain.ErrPolicyNotFound, "NoSuchTagSet", "")).Once()
	requireError(t, f.do(http.MethodGet, "/none?tagging", ""), http.StatusNotFound, "NoSuchTagSet")
	f.buckets.EXPECT().DeleteTags(mock.Anything, "photos").Return(nil).Once()
	assert.Equal(t, http.StatusNoContent, f.do(http.MethodDelete, "/photos?tagging", "").Code)
}

func TestBucketConfigurationDocuments(t *testing.T) {
	f := newFixture(t)
	lc := []byte(`<LifecycleConfiguration><Rule><ID>r</ID><Status>Enabled</Status></Rule></LifecycleConfiguration>`)
	f.buckets.EXPECT().GetLifecycle(mock.Anything, "photos").Return(lc, nil).Once()
	rec := f.do(http.MethodGet, "/photos?lifecycle", "")
	assert.Equal(t, string(lc), rec.Body.String())
	assert.Equal(t, httpx.ContentTypeXML, rec.Header().Get("Content-Type"))
	f.buckets.EXPECT().GetLifecycle(mock.Anything, "none").Return(nil, domain.NewS3Error(domain.ErrPolicyNotFound, "NoSuchLifecycleConfiguration", "")).Once()
	requireError(t, f.do(http.MethodGet, "/none?lifecycle", ""), http.StatusNotFound, "NoSuchLifecycleConfiguration")
	f.buckets.EXPECT().DeleteLifecycle(mock.Anything, "photos").Return(nil).Once()
	assert.Equal(t, http.StatusNoContent, f.do(http.MethodDelete, "/photos?lifecycle", "").Code)

	f.buckets.EXPECT().GetCORS(mock.Anything, "none").Return(nil, domain.NewS3Error(domain.ErrPolicyNotFound, "NoSuchCORSConfiguration", "")).Once()
	requireError(t, f.do(http.MethodGet, "/none?cors", ""), http.StatusNotFound, "NoSuchCORSConfiguration")
	f.buckets.EXPECT().DeleteCORS(mock.Anything, "photos").Return(nil).Once()
	assert.Equal(t, http.StatusNoContent, f.do(http.MethodDelete, "/photos?cors", "").Code)

	notif := []byte(`<NotificationConfiguration></NotificationConfiguration>`)
	f.buckets.EXPECT().GetNotification(mock.Anything, "photos").Return(notif, nil).Once()
	assert.Equal(t, string(notif), f.do(http.MethodGet, "/photos?notification", "").Body.String())

	f.buckets.EXPECT().GetObjectLock(mock.Anything, "photos").Return(bucket.ObjectLockConfig{Enabled: true, Mode: bucket.RetentionCompliance, Years: 1}, nil).Once()
	rec = f.do(http.MethodGet, "/photos?object-lock", "")
	assert.Contains(t, rec.Body.String(), "<ObjectLockEnabled>Enabled</ObjectLockEnabled><Rule><DefaultRetention><Mode>COMPLIANCE</Mode><Years>1</Years></DefaultRetention></Rule>")
	f.buckets.EXPECT().GetObjectLock(mock.Anything, "plain").Return(bucket.ObjectLockConfig{Enabled: true}, nil).Once()
	assert.NotContains(t, f.do(http.MethodGet, "/plain?object-lock", "").Body.String(), "<Rule>")
	f.buckets.EXPECT().GetObjectLock(mock.Anything, "none").Return(bucket.ObjectLockConfig{}, domain.NewS3Error(domain.ErrPolicyNotFound, "ObjectLockConfigurationNotFoundError", "")).Once()
	requireError(t, f.do(http.MethodGet, "/none?object-lock", ""), http.StatusNotFound, "ObjectLockConfigurationNotFoundError")

	f.buckets.EXPECT().GetEncryption(mock.Anything, "photos").Return(bucket.EncryptionConfig{Algorithm: "aws:kms", KMSKeyID: "k1"}, nil).Once()
	rec = f.do(http.MethodGet, "/photos?encryption", "")
	assert.Contains(t, rec.Body.String(), "<SSEAlgorithm>aws:kms</SSEAlgorithm><KMSMasterKeyID>k1</KMSMasterKeyID>")
	f.buckets.EXPECT().GetEncryption(mock.Anything, "none").Return(bucket.EncryptionConfig{}, domain.NewS3Error(domain.ErrPolicyNotFound, "ServerSideEncryptionConfigurationNotFoundError", "")).Once()
	requireError(t, f.do(http.MethodGet, "/none?encryption", ""), http.StatusNotFound, "ServerSideEncryptionConfigurationNotFoundError")
	f.buckets.EXPECT().DeleteEncryption(mock.Anything, "photos").Return(nil).Once()
	assert.Equal(t, http.StatusNoContent, f.do(http.MethodDelete, "/photos?encryption", "").Code)
}

func TestBucketACL(t *testing.T) {
	f := newFixture(t)
	f.buckets.EXPECT().Get(mock.Anything, "photos").Return(&domain.BucketInfo{}, nil).Times(3)
	rec := f.do(http.MethodGet, "/photos?acl", "")
	require.Equal(t, http.StatusOK, rec.Code)
	var acl s3.AccessControlPolicy
	decodeXML(t, rec, &acl)
	require.Len(t, acl.AccessControlList.Grants, 1)
	assert.Equal(t, "FULL_CONTROL", acl.AccessControlList.Grants[0].Permission)
	assert.Equal(t, "root", acl.AccessControlList.Grants[0].Grantee.ID)
	assert.Equal(t, http.StatusOK, f.do(http.MethodPut, "/photos?acl", "").Code)
	assert.Equal(t, http.StatusOK, f.do(http.MethodPut, "/photos?acl", "", "x-amz-acl", "private").Code)
	requireError(t, f.do(http.MethodPut, "/photos?acl", "", "x-amz-acl", "public-read"), http.StatusNotImplemented, "NotImplemented")
	requireError(t, f.do(http.MethodPut, "/photos?acl", "<AccessControlPolicy/>"), http.StatusNotImplemented, "NotImplemented")
	f.buckets.EXPECT().Get(mock.Anything, "nope").Return(nil, domain.ErrBucketNotFound).Twice()
	requireError(t, f.do(http.MethodGet, "/nope?acl", ""), http.StatusNotFound, "NoSuchBucket")
	requireError(t, f.do(http.MethodPut, "/nope?acl", ""), http.StatusNotFound, "NoSuchBucket")
}

// Regression: max-keys=0 (max-uploads=0, max-parts=0) reached the services
// as 0, the default, and listed up to 1000 entries. An explicit 0 now asks
// for none (domain.ListLimitNone) and is echoed as 0; an absent parameter
// is the default 1000.
func TestListingZeroLimits(t *testing.T) {
	tests := []struct {
		name        string
		target      string
		wantService int
		wantEcho    int
	}{
		{name: "v1 explicit zero", target: "/photos?max-keys=0", wantService: domain.ListLimitNone, wantEcho: 0},
		{name: "v2 explicit zero", target: "/photos?list-type=2&max-keys=0", wantService: domain.ListLimitNone, wantEcho: 0},
		{name: "v1 absent", target: "/photos", wantService: 1000, wantEcho: 1000},
		{name: "v2 one", target: "/photos?list-type=2&max-keys=1", wantService: 1, wantEcho: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.objects.EXPECT().ListObjects(mock.Anything, mock.MatchedBy(func(in domain.ListObjectsInput) bool { return in.MaxKeys == tt.wantService })).
				Return(domain.ListObjectsResult{}, nil).Once()
			rec := f.do(http.MethodGet, tt.target, "")
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "<MaxKeys>"+strconv.Itoa(tt.wantEcho)+"</MaxKeys>")
		})
	}
	t.Run("versions", func(t *testing.T) {
		f := newFixture(t)
		f.objects.EXPECT().ListObjectVersions(mock.Anything, mock.MatchedBy(func(in domain.ListObjectsInput) bool { return in.MaxKeys == domain.ListLimitNone })).
			Return(domain.ListObjectsResult{}, nil).Once()
		rec := f.do(http.MethodGet, "/photos?versions&max-keys=0", "")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "<MaxKeys>0</MaxKeys>")
	})
	t.Run("uploads", func(t *testing.T) {
		f := newFixture(t)
		f.objects.EXPECT().ListMultipartUploads(mock.Anything, mock.MatchedBy(func(in domain.ListMultipartUploadsInput) bool { return in.MaxUploads == domain.ListLimitNone })).
			Return(domain.ListMultipartUploadsResult{}, nil).Once()
		rec := f.do(http.MethodGet, "/photos?uploads&max-uploads=0", "")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "<MaxUploads>0</MaxUploads>")
	})
	t.Run("parts", func(t *testing.T) {
		f := newFixture(t)
		f.objects.EXPECT().ListObjectParts(mock.Anything, mock.MatchedBy(func(in domain.ListPartsInput) bool { return in.MaxParts == domain.ListLimitNone })).
			Return(domain.ListPartsResult{}, nil).Once()
		rec := f.do(http.MethodGet, "/photos/k?uploadId=u1&max-parts=0", "")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "<MaxParts>0</MaxParts>")
	})
}
