package s3_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/service/settings"
	settingsmock "github.com/r3n3r0/nexo/internal/service/settings/mock"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
	"github.com/r3n3r0/nexo/internal/transport/http/s3"
)

func TestObjectRetentionRoutes(t *testing.T) {
	f := newFixture(t)
	until := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	locked := domain.NewS3Error(domain.ErrObjectLocked, "AccessDenied", "Access Denied because object protected by object lock.")

	f.objects.EXPECT().GetObjectRetention(mock.Anything, "b", "k", object.LockOptions{VersionID: "v1"}).
		Return(domain.ObjectRetention{Mode: domain.RetentionCompliance, RetainUntil: until}, nil).Once()
	rec := f.do(http.MethodGet, "/b/k?retention&versionId=v1", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got s3.Retention
	decodeXML(t, rec, &got)
	assert.Equal(t, "COMPLIANCE", got.Mode)
	assert.Equal(t, "2030-01-02T03:04:05.000Z", got.RetainUntilDate)

	f.objects.EXPECT().GetObjectRetention(mock.Anything, "b", "none", object.LockOptions{}).
		Return(domain.ObjectRetention{}, domain.NewS3Error(domain.ErrPolicyNotFound, "NoSuchObjectLockConfiguration", "")).Once()
	requireError(t, f.do(http.MethodGet, "/b/none?retention", ""), http.StatusNotFound, "NoSuchObjectLockConfiguration")

	tests := []struct {
		name      string
		target    string
		body      string
		headers   []string
		retention domain.ObjectRetention
		opts      object.LockOptions
		err       error
		status    int
		code      string
	}{
		{
			name: "set", target: "/b/k?retention",
			body:      `<Retention><Mode>GOVERNANCE</Mode><RetainUntilDate>2030-01-02T03:04:05Z</RetainUntilDate></Retention>`,
			retention: domain.ObjectRetention{Mode: domain.RetentionGovernance, RetainUntil: until},
			status:    http.StatusOK,
		},
		{
			name: "removal with bypass on a version", target: "/b/k?retention&versionId=v1",
			body: `<Retention xmlns="http://s3.amazonaws.com/doc/2006-03-01/"/>`, headers: []string{"x-amz-bypass-governance-retention", "true"},
			opts: object.LockOptions{VersionID: "v1", BypassGovernance: true}, status: http.StatusOK,
		},
		{
			name: "bypass header off", target: "/b/k?retention",
			body:    `<Retention><Mode>GOVERNANCE</Mode><RetainUntilDate>2030-01-02T03:04:05Z</RetainUntilDate></Retention>`,
			headers: []string{"x-amz-bypass-governance-retention", "false"}, retention: domain.ObjectRetention{Mode: domain.RetentionGovernance, RetainUntil: until},
			status: http.StatusOK,
		},
		{
			name: "locked", target: "/b/k?retention",
			body:      `<Retention><Mode>GOVERNANCE</Mode><RetainUntilDate>2030-01-02T03:04:05Z</RetainUntilDate></Retention>`,
			retention: domain.ObjectRetention{Mode: domain.RetentionGovernance, RetainUntil: until},
			err:       locked, status: http.StatusForbidden, code: "AccessDenied",
		},
		{name: "unknown mode", target: "/b/k?retention", body: `<Retention><Mode>LOOSE</Mode><RetainUntilDate>2030-01-02T03:04:05Z</RetainUntilDate></Retention>`, status: http.StatusBadRequest, code: "MalformedXML"},
		{name: "bad date", target: "/b/k?retention", body: `<Retention><Mode>GOVERNANCE</Mode><RetainUntilDate>tomorrow</RetainUntilDate></Retention>`, status: http.StatusBadRequest, code: "MalformedXML"},
		{name: "malformed", target: "/b/k?retention", body: `<Retention>`, status: http.StatusBadRequest, code: "MalformedXML"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.code != "MalformedXML" {
				f.objects.EXPECT().PutObjectRetention(mock.Anything, "b", "k", tc.retention, tc.opts).
					Return(domain.ObjectInfo{VersionID: "v9"}, tc.err).Once()
			}
			rec := f.do(http.MethodPut, tc.target, tc.body, tc.headers...)
			if tc.code != "" {
				requireError(t, rec, tc.status, tc.code)
				return
			}
			require.Equal(t, tc.status, rec.Code, rec.Body.String())
			assert.Equal(t, "v9", rec.Header().Get("x-amz-version-id"))
		})
	}
}

func TestObjectRetentionBypassNeedsPermission(t *testing.T) {
	user := &domain.Identity{AccessKey: "alice"}
	f := newFixture(t, asIdentity(user), withStrictPolicy())
	f.policy.EXPECT().Evaluate(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, req policy.Args) (policy.Decision, error) {
		return policy.Decision{Allowed: req.Action != policy.Action("s3:BypassGovernanceRetention")}, nil
	})
	f.objects.EXPECT().PutObjectRetention(mock.Anything, "b", "k", domain.ObjectRetention{}, object.LockOptions{}).
		Return(domain.ObjectInfo{}, nil).Once()
	rec := f.do(http.MethodPut, "/b/k?retention", `<Retention/>`, "x-amz-bypass-governance-retention", "true")
	assert.Equal(t, http.StatusOK, rec.Code, "without s3:BypassGovernanceRetention the header is ignored")
}

func TestObjectLegalHoldRoutes(t *testing.T) {
	f := newFixture(t)
	f.objects.EXPECT().GetObjectLegalHold(mock.Anything, "b", "k", object.LockOptions{}).Return(domain.LegalHoldOn, nil).Once()
	rec := f.do(http.MethodGet, "/b/k?legal-hold", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got s3.LegalHold
	decodeXML(t, rec, &got)
	assert.Equal(t, "ON", got.Status)

	f.objects.EXPECT().PutObjectLegalHold(mock.Anything, "b", "k", domain.LegalHoldOff, object.LockOptions{VersionID: "v1"}).
		Return(domain.ObjectInfo{VersionID: "v1"}, nil).Once()
	rec = f.do(http.MethodPut, "/b/k?legal-hold&versionId=v1", `<LegalHold><Status>OFF</Status></LegalHold>`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "v1", rec.Header().Get("x-amz-version-id"))

	requireError(t, f.do(http.MethodPut, "/b/k?legal-hold", `<LegalHold><Status>MAYBE</Status></LegalHold>`), http.StatusBadRequest, "MalformedXML")
	f.objects.EXPECT().PutObjectLegalHold(mock.Anything, "plain", "k", domain.LegalHoldOn, object.LockOptions{}).
		Return(domain.ObjectInfo{}, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidRequest", "Bucket is missing Object Lock Configuration")).Once()
	requireError(t, f.do(http.MethodPut, "/plain/k?legal-hold", `<LegalHold><Status>ON</Status></LegalHold>`), http.StatusBadRequest, "InvalidRequest")
}

func TestPutObjectLockConfiguration(t *testing.T) {
	f := newFixture(t)
	f.buckets.EXPECT().SetObjectLock(mock.Anything, "vault", bucket.ObjectLockConfig{Enabled: true, Mode: domain.RetentionGovernance, Days: 7}).Return(nil).Once()
	rec := f.do(http.MethodPut, "/vault?object-lock", `<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled>
		<Rule><DefaultRetention><Mode>GOVERNANCE</Mode><Days>7</Days></DefaultRetention></Rule></ObjectLockConfiguration>`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	f.buckets.EXPECT().SetObjectLock(mock.Anything, "vault", bucket.ObjectLockConfig{Enabled: true}).Return(nil).Once()
	rec = f.do(http.MethodPut, "/vault?object-lock", `<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled></ObjectLockConfiguration>`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	f.buckets.EXPECT().SetObjectLock(mock.Anything, "plain", bucket.ObjectLockConfig{Enabled: true}).
		Return(domain.NewS3Error(domain.ErrConflict, "InvalidBucketState", "Object Lock configuration cannot be enabled on existing buckets")).Once()
	requireError(t, f.do(http.MethodPut, "/plain?object-lock", `<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled></ObjectLockConfiguration>`),
		http.StatusConflict, "InvalidBucketState")

	rule := func(retention string) string {
		return `<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled><Rule><DefaultRetention>` + retention +
			`</DefaultRetention></Rule></ObjectLockConfiguration>`
	}
	for _, tt := range []struct {
		name, body, code string
	}{
		{"disabled", `<ObjectLockConfiguration><ObjectLockEnabled>Disabled</ObjectLockEnabled></ObjectLockConfiguration>`, "MalformedXML"},
		{"rule without period", rule(""), "MalformedXML"},
		{"mode in lowercase", rule(`<Mode>governance</Mode><Days>1</Days>`), "MalformedXML"},
		{"unknown mode", rule(`<Mode>abc</Mode><Days>1</Days>`), "MalformedXML"},
		{"days and years", rule(`<Mode>GOVERNANCE</Mode><Days>1</Days><Years>1</Years>`), "MalformedXML"},
		{"no period", rule(`<Mode>GOVERNANCE</Mode>`), "MalformedXML"},
		{"zero days", rule(`<Mode>GOVERNANCE</Mode><Days>0</Days>`), "InvalidRetentionPeriod"},
		{"negative years", rule(`<Mode>COMPLIANCE</Mode><Years>-1</Years>`), "InvalidRetentionPeriod"},
		{"malformed", `<ObjectLockConfiguration>`, "MalformedXML"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			requireError(t, f.do(http.MethodPut, "/vault?object-lock", tt.body), http.StatusBadRequest, tt.code)
		})
	}
}

func TestPutObjectWithLockHeaders(t *testing.T) {
	user := &domain.Identity{AccessKey: "alice"}
	f := newFixture(t, asIdentity(user), withStrictPolicy())
	var actions []string
	f.policy.EXPECT().Evaluate(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, req policy.Args) (policy.Decision, error) {
		actions = append(actions, string(req.Action))
		return policy.Decision{Allowed: req.Action != policy.Action("s3:PutObjectLegalHold")}, nil
	})
	until := time.Date(2030, 1, 2, 0, 0, 0, 0, time.UTC)
	f.objects.EXPECT().PutObject(mock.Anything, mock.MatchedBy(func(in object.PutObjectInput) bool {
		return in.Retention == domain.ObjectRetention{Mode: domain.RetentionGovernance, RetainUntil: until} && in.LegalHold == ""
	})).Return(domain.ObjectInfo{Bucket: "b", Key: "k", ETag: "e"}, nil).Once()
	rec := f.do(http.MethodPut, "/b/k", "x", "x-amz-object-lock-mode", "GOVERNANCE", "x-amz-object-lock-retain-until-date", "2030-01-02T00:00:00Z")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, actions, "s3:PutObjectRetention", "a retention needs s3:PutObjectRetention")

	requireError(t, f.do(http.MethodPut, "/b/k", "x", "x-amz-object-lock-legal-hold", "ON"), http.StatusForbidden, "AccessDenied")
}

func TestGetObjectLockHeaders(t *testing.T) {
	user := &domain.Identity{AccessKey: "alice"}
	f := newFixture(t, asIdentity(user), withStrictPolicy())
	f.policy.EXPECT().Evaluate(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, req policy.Args) (policy.Decision, error) {
		return policy.Decision{Allowed: req.Action != policy.Action("s3:GetObjectLegalHold")}, nil
	})
	until := time.Date(2030, 1, 2, 0, 0, 0, 0, time.UTC)
	info := domain.ObjectInfo{Bucket: "b", Key: "k", ETag: "e", ModTime: now, Size: 1,
		Retention: domain.ObjectRetention{Mode: domain.RetentionGovernance, RetainUntil: until}, LegalHold: domain.LegalHoldOn}
	f.objects.EXPECT().GetObjectInfo(mock.Anything, "b", "k", mock.Anything).Return(info, nil).Once()
	rec := f.do(http.MethodHead, "/b/k", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "GOVERNANCE", rec.Header().Get("x-amz-object-lock-mode"))
	assert.Equal(t, "2030-01-02T00:00:00.000Z", rec.Header().Get("x-amz-object-lock-retain-until-date"))
	assert.Empty(t, rec.Header().Get("x-amz-object-lock-legal-hold"), "reported only with s3:GetObjectLegalHold")
}

func TestGetObjectAttributes(t *testing.T) {
	f := newFixture(t)
	requireError(t, f.do(http.MethodGet, "/b/k?attributes", ""), http.StatusBadRequest, "InvalidRequest")
	requireError(t, f.do(http.MethodGet, "/b/k?attributes", "", "x-amz-object-attributes", "ETag,Owner"), http.StatusBadRequest, "InvalidArgument")
	requireError(t, f.do(http.MethodGet, "/b/k?attributes", "", "x-amz-object-attributes", "ETag", "x-amz-max-parts", "-1"), http.StatusBadRequest, "InvalidArgument")

	info := domain.ObjectInfo{Bucket: "b", Key: "k", VersionID: "v1", ETag: `"abc-2"`, Size: 30, ModTime: now,
		Checksum: domain.Checksum{Algorithm: domain.ChecksumCRC32, Value: "AAAAAA==-2", Type: domain.ChecksumTypeComposite}}
	f.objects.EXPECT().GetObjectAttributes(mock.Anything, "b", "k", object.AttributesOptions{VersionID: "v1", MaxParts: 2, PartNumberMarker: 0}).
		Return(object.ObjectAttributes{Info: info, PartsCount: 2, Parts: []domain.ObjectPart{
			{Number: 1, Size: 20, Checksum: domain.Checksum{Algorithm: domain.ChecksumCRC32, Value: "AQIDBA=="}},
			{Number: 2, Size: 10, Checksum: domain.Checksum{Algorithm: domain.ChecksumCRC32, Value: "BQYHCA=="}},
		}}, nil).Once()
	rec := f.do(http.MethodGet, "/b/k?attributes&versionId=v1", "", "x-amz-object-attributes", "ETag, Checksum, ObjectParts, StorageClass, ObjectSize", "x-amz-max-parts", "2")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "v1", rec.Header().Get("x-amz-version-id"))
	var got s3.GetObjectAttributesResponse
	decodeXML(t, rec, &got)
	assert.Equal(t, "abc-2", got.ETag)
	require.NotNil(t, got.Checksum)
	assert.Equal(t, "AAAAAA==-2", got.Checksum.ChecksumCRC32)
	assert.Equal(t, "COMPOSITE", got.Checksum.ChecksumType)
	require.NotNil(t, got.ObjectParts)
	assert.Equal(t, 2, got.ObjectParts.PartsCount)
	assert.Equal(t, 2, got.ObjectParts.MaxParts)
	require.Len(t, got.ObjectParts.Parts, 2)
	assert.Equal(t, s3.AttributesPart{ChecksumFields: s3.ChecksumFields{ChecksumCRC32: "BQYHCA=="}, PartNumber: 2, Size: 10}, got.ObjectParts.Parts[1])
	assert.Equal(t, "STANDARD", got.StorageClass)
	require.NotNil(t, got.ObjectSize)
	assert.Equal(t, int64(30), *got.ObjectSize)

	// Only the requested attributes.
	f.objects.EXPECT().GetObjectAttributes(mock.Anything, "b", "k", mock.Anything).Return(object.ObjectAttributes{Info: info, PartsCount: 2}, nil).Once()
	rec = f.do(http.MethodGet, "/b/k?attributes", "", "x-amz-object-attributes", "ObjectSize")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "<ETag>")
	assert.NotContains(t, rec.Body.String(), "<ObjectParts>")

	// A delete marker answers as GET does.
	f.objects.EXPECT().GetObjectAttributes(mock.Anything, "b", "gone", mock.Anything).
		Return(object.ObjectAttributes{}, &domain.DeleteMarkerError{VersionID: "m1", ModTime: now, Err: domain.ErrObjectNotFound}).Once()
	rec = f.do(http.MethodGet, "/b/gone?attributes", "", "x-amz-object-attributes", "ETag")
	requireError(t, rec, http.StatusNotFound, "NoSuchKey")
	assert.Equal(t, "true", rec.Header().Get("x-amz-delete-marker"))
	assert.Equal(t, "m1", rec.Header().Get("x-amz-version-id"))
}

// corsFixture is the S3 API with the CORS rules of the buckets and the
// deployment-wide policy of the settings.
type corsFixture struct {
	*fixture
	settings *settingsmock.Service
}

func newCORSFixture(t *testing.T) *corsFixture {
	t.Helper()
	f := newFixture(t)
	st := settingsmock.NewService(t)
	api := s3.NewAPI(s3.APIDeps{Enc: f.enc, Buckets: f.buckets, Settings: st},
		s3.NewBucketRoutes(s3.BucketDeps{Buckets: f.buckets, Objects: f.objects, Policy: f.policy, Enc: f.enc, Clock: f.clock}),
		s3.NewObjectRoutes(s3.ObjectDeps{Objects: f.objects, Policy: f.policy, Enc: f.enc, Clock: f.clock}),
	)
	r := chi.NewRouter()
	api.Register(r)
	f.handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.ServeHTTP(w, req.WithContext(middleware.WithIdentity(req.Context(), f.identity)))
	})
	return &corsFixture{fixture: f, settings: st}
}

func TestCORSBucketRules(t *testing.T) {
	f := newCORSFixture(t)
	maxAge := 60
	f.buckets.EXPECT().GetCORSConfig(mock.Anything, "web").Return(domain.CORSConfiguration{Rules: []domain.CORSRule{{
		AllowedOrigins: []string{"https://*.example.com"}, AllowedMethods: []string{"GET", "PUT"},
		AllowedHeaders: []string{"content-type"}, ExposeHeaders: []string{"ETag"}, MaxAgeSeconds: &maxAge,
	}}}, nil)

	tests := []struct {
		name, origin, method, headers string
		status                        int
	}{
		{"match", "https://app.example.com", "PUT", "Content-Type", http.StatusOK},
		{"origin", "https://evil.test", "PUT", "", http.StatusForbidden},
		{"method", "https://app.example.com", "DELETE", "", http.StatusForbidden},
		{"header", "https://app.example.com", "PUT", "x-custom", http.StatusForbidden},
		{"no method", "https://app.example.com", "", "", http.StatusBadRequest},
		{"no origin", "", "GET", "", http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var headers []string
			for _, kv := range [][2]string{{"Origin", tc.origin}, {"Access-Control-Request-Method", tc.method}, {"Access-Control-Request-Headers", tc.headers}} {
				if kv[1] != "" {
					headers = append(headers, kv[0], kv[1])
				}
			}
			rec := f.do(http.MethodOptions, "/web/index.html", "", headers...)
			require.Equal(t, tc.status, rec.Code, rec.Body.String())
			if tc.status == http.StatusForbidden {
				assert.Equal(t, "AccessForbidden", s3Error(t, rec).Code)
			}
			if tc.status != http.StatusOK {
				assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
				return
			}
			assert.Equal(t, tc.origin, rec.Header().Get("Access-Control-Allow-Origin"))
			assert.Equal(t, "true", rec.Header().Get("Access-Control-Allow-Credentials"))
			assert.Equal(t, "GET, PUT", rec.Header().Get("Access-Control-Allow-Methods"))
			assert.Equal(t, "Content-Type", rec.Header().Get("Access-Control-Allow-Headers"))
			assert.Equal(t, "60", rec.Header().Get("Access-Control-Max-Age"))
			assert.Equal(t, []string{"Origin", "Access-Control-Request-Headers", "Access-Control-Request-Method"}, rec.Header().Values("Vary"))
		})
	}

	// Actual requests: the headers of the matching rule, errors included.
	f.objects.EXPECT().GetObject(mock.Anything, "web", "missing", mock.Anything).Return(nil, domain.ErrObjectNotFound).Once()
	rec := f.do(http.MethodGet, "/web/missing", "", "Origin", "https://app.example.com")
	requireError(t, rec, http.StatusNotFound, "NoSuchKey")
	assert.Equal(t, "https://app.example.com", rec.Header().Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "ETag", rec.Header().Get("Access-Control-Expose-Headers"))
	assert.Equal(t, []string{"Origin"}, rec.Header().Values("Vary"))
	f.objects.EXPECT().GetObjectInfo(mock.Anything, "web", "missing", mock.Anything).Return(domain.ObjectInfo{}, domain.ErrObjectNotFound).Once()
	rec = f.do(http.MethodHead, "/web/missing", "", "Origin", "https://app.example.com")
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"), "no rule allows HEAD")
}

func TestCORSGlobalPolicy(t *testing.T) {
	f := newCORSFixture(t)
	f.buckets.EXPECT().GetCORSConfig(mock.Anything, "plain").Return(domain.CORSConfiguration{}, domain.ErrPolicyNotFound)
	f.buckets.EXPECT().GetCORSConfig(mock.Anything, "broken").Return(domain.CORSConfiguration{}, errors.New("disk failure"))

	f.settings.EXPECT().API(mock.Anything).Return(settings.API{}, nil).Once()
	rec := f.do(http.MethodOptions, "/plain/k", "", "Origin", "https://any.test", "Access-Control-Request-Method", "DELETE")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "*", rec.Header().Get("Access-Control-Allow-Origin"), "every origin without api.cors_allow_origin")
	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Credentials"))

	f.settings.EXPECT().API(mock.Anything).Return(settings.API{CORSAllowOrigin: []string{"https://only.test"}}, nil).Times(3)
	rec = f.do(http.MethodOptions, "/plain/k", "", "Origin", "https://any.test", "Access-Control-Request-Method", "GET")
	requireError(t, rec, http.StatusForbidden, "AccessForbidden")
	rec = f.do(http.MethodOptions, "/plain/k", "", "Origin", "https://only.test", "Access-Control-Request-Method", "GET")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "https://only.test", rec.Header().Get("Access-Control-Allow-Origin"))
	rec = f.do(http.MethodOptions, "/", "", "Origin", "https://only.test", "Access-Control-Request-Method", "GET")
	assert.Equal(t, http.StatusOK, rec.Code, "service-level requests")

	rec = f.do(http.MethodOptions, "/broken/k", "", "Origin", "https://only.test", "Access-Control-Request-Method", "GET")
	requireError(t, rec, http.StatusForbidden, "AccessForbidden")

	f.settings.EXPECT().API(mock.Anything).Return(settings.API{}, errors.New("unavailable")).Once()
	rec = f.do(http.MethodOptions, "/plain/k", "", "Origin", "https://any.test", "Access-Control-Request-Method", "GET")
	requireError(t, rec, http.StatusForbidden, "AccessForbidden")
}

func TestPutBucketCORS(t *testing.T) {
	f := newFixture(t)
	doc := `<CORSConfiguration><CORSRule><AllowedOrigin>*</AllowedOrigin><AllowedMethod>GET</AllowedMethod></CORSRule></CORSConfiguration>`
	f.buckets.EXPECT().SetCORS(mock.Anything, "web", []byte(doc)).Return(nil).Once()
	require.Equal(t, http.StatusOK, f.do(http.MethodPut, "/web?cors", doc).Code)
	f.buckets.EXPECT().SetCORS(mock.Anything, "web", mock.Anything).
		Return(domain.NewS3Error(domain.ErrInvalidArgument, "InvalidRequest", "Found unsupported HTTP method in CORS config. Unsupported method is PATCH")).Once()
	requireError(t, f.do(http.MethodPut, "/web?cors", doc), http.StatusBadRequest, "InvalidRequest")
}
