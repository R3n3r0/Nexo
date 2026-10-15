package s3_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	authmock "github.com/r3n3r0/nexo/internal/service/auth/mock"
	"github.com/r3n3r0/nexo/internal/service/health"
	"github.com/r3n3r0/nexo/internal/service/metrics"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/policy"
	policymock "github.com/r3n3r0/nexo/internal/service/policy/mock"
	"github.com/r3n3r0/nexo/internal/transport/http/s3"
)

const presignedQuery = "X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Date=20261008T115000Z&X-Amz-Signature=0000"

func TestPresignedVerification(t *testing.T) {
	f := newFixture(t, withoutIdentity())
	// Without an identity in the context the group verifies the signature itself.
	f.verifier.EXPECT().VerifyPresigned(mock.Anything, mock.Anything).Return(nil, domain.ErrSignatureMismatch).Once()
	requireError(t, f.do(http.MethodGet, "/b/k?"+presignedQuery, ""), http.StatusForbidden, "SignatureDoesNotMatch")
	f.verifier.EXPECT().VerifyPresigned(mock.Anything, mock.Anything).
		Return(nil, domain.NewS3Error(domain.ErrRequestExpired, "AccessDenied", "Request has expired")).Once()
	rec := f.do(http.MethodGet, "/b/k?"+presignedQuery, "")
	requireError(t, rec, http.StatusForbidden, "AccessDenied")
	assert.Equal(t, "Request has expired", s3Error(t, rec).Message)

	f.verifier.EXPECT().VerifyPresigned(mock.Anything, mock.Anything).Return(&domain.Identity{AccessKey: "alice"}, nil).Once()
	f.objects.EXPECT().GetObject(mock.Anything, "b", "k", object.GetObjectOptions{}).Return(body("data", domain.ObjectInfo{Size: 4, ModTime: now}, 0, 4), nil).Once()
	rec = f.do(http.MethodGet, "/b/k?"+presignedQuery+"&response-content-disposition=attachment", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "data", rec.Body.String())
	assert.Equal(t, "attachment", rec.Header().Get("Content-Disposition"), "a signed request may override the response headers")
}

func TestPresignedOperations(t *testing.T) {
	f := newFixture(t, asIdentity(&domain.Identity{AccessKey: "alice"}))
	// With an identity already established (auth middleware) no verification happens.
	f.objects.EXPECT().ListObjects(mock.Anything, mock.MatchedBy(func(in domain.ListObjectsInput) bool { return in.Bucket == "b" })).
		Return(domain.ListObjectsResult{}, nil).Twice()
	rec := f.do(http.MethodGet, "/b?list-type=2&"+presignedQuery, "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "<KeyCount>0</KeyCount>")
	rec = f.do(http.MethodGet, "/b?"+presignedQuery, "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "<Marker></Marker>", "v1 listing")

	f.objects.EXPECT().GetObjectInfo(mock.Anything, "b", "k", mock.Anything).Return(domain.ObjectInfo{Size: 2}, nil).Once()
	assert.Equal(t, http.StatusOK, f.do(http.MethodHead, "/b/k?"+presignedQuery, "").Code)
	f.objects.EXPECT().PutObject(mock.Anything, mock.MatchedBy(func(in object.PutObjectInput) bool { return in.Key == "k" && in.Size == 3 })).
		Return(domain.ObjectInfo{ETag: "e"}, nil).Once()
	assert.Equal(t, http.StatusOK, f.do(http.MethodPut, "/b/k?"+presignedQuery, "abc").Code)
	f.objects.EXPECT().DeleteObject(mock.Anything, "b", "k", object.DeleteObjectOptions{VersionID: "v1"}).Return(object.DeletedObject{VersionID: "v1"}, nil).Once()
	assert.Equal(t, http.StatusNoContent, f.do(http.MethodDelete, "/b/k?versionId=v1&"+presignedQuery, "").Code)

	// Presigned multipart requests reach the multipart operations.
	f.objects.EXPECT().PutObjectPart(mock.Anything, mock.MatchedBy(func(in object.PutObjectPartInput) bool { return in.UploadID == "u" && in.PartNumber == 2 })).
		Return(domain.Part{ETag: "pe"}, nil).Once()
	rec = f.do(http.MethodPut, "/b/k?partNumber=2&uploadId=u&"+presignedQuery, "part")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, `"pe"`, rec.Header().Get("ETag"))
	f.objects.EXPECT().GetObjectTags(mock.Anything, "b", "k", object.TagOptions{}).Return(map[string]string{}, nil).Once()
	assert.Equal(t, http.StatusOK, f.do(http.MethodGet, "/b/k?tagging&"+presignedQuery, "").Code)
}

func TestPresignedAuthorizationUsesTheSigner(t *testing.T) {
	f := newFixture(t, withoutIdentity(), withStrictPolicy())
	f.verifier.EXPECT().VerifyPresigned(mock.Anything, mock.Anything).Return(&domain.Identity{AccessKey: "alice"}, nil).Once()
	f.policy.EXPECT().Evaluate(mock.Anything, mock.MatchedBy(func(a policy.Args) bool {
		return a.Identity.AccessKey == "alice" && a.Conditions["s3:authType"][0] == "REST-QUERY-STRING" && a.Conditions["s3:signatureAge"][0] == "600000"
	})).Return(policy.Decision{Allowed: false}, nil).Once()
	requireError(t, f.do(http.MethodGet, "/b/k?"+presignedQuery, ""), http.StatusForbidden, "AccessDenied")
}

// covers: S-OPS-03
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
	rec := serve(r, request(http.MethodGet, domain.HealthLivePath, ""))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	var st health.Status
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &st))
	assert.True(t, st.OK)
	rec = serve(r, request(http.MethodHead, domain.HealthLivePath, ""))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, rec.Body.String())
	ready = false
	rec = serve(r, request(http.MethodGet, domain.HealthReadyPath, ""))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "no quorum")
	rec = serve(r, request(http.MethodHead, domain.HealthReadyPath, ""))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	rec = serve(r, request(http.MethodGet, domain.HealthClusterPath, ""))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	var cs health.ClusterStatus
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &cs))
	assert.Equal(t, 1, cs.Nodes)
	rec = serve(r, request(http.MethodHead, domain.HealthClusterPath, ""))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Empty(t, strings.TrimSpace(rec.Body.String()))
}

// TestPresignedCopySourceMustBeSigned is the transport defense in depth of
// the copy-source injection: a presigned request whose signature does not
// cover x-amz-copy-source never reaches CopyObject or UploadPartCopy, even
// if the verifier let it through.
func TestPresignedCopySourceMustBeSigned(t *testing.T) {
	tests := []struct {
		name     string
		identity *domain.Identity
		target   string
		allowed  bool
	}{
		{name: "presigned PUT, copy source not signed", target: "/b/k?" + presignedQuery,
			identity: &domain.Identity{AccessKey: "alice", AuthType: domain.AuthTypeQuery, SignedHeaders: []string{"host"}}},
		{name: "presigned UploadPart, copy source not signed", target: "/b/k?partNumber=1&uploadId=u&" + presignedQuery,
			identity: &domain.Identity{AccessKey: "alice", AuthType: domain.AuthTypeQuery, SignedHeaders: []string{"host"}}},
		{name: "presigned PUT, copy source signed", target: "/b/k?" + presignedQuery, allowed: true,
			identity: &domain.Identity{AccessKey: "alice", AuthType: domain.AuthTypeQuery, SignedHeaders: []string{"host", "x-amz-copy-source"}}},
		{name: "header-signed copy", target: "/b/k", allowed: true,
			identity: &domain.Identity{AccessKey: "alice", AuthType: domain.AuthTypeHeader}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, asIdentity(tt.identity))
			if tt.allowed {
				f.objects.EXPECT().CopyObject(mock.Anything, mock.MatchedBy(func(in object.CopyObjectInput) bool {
					return in.SrcBucket == "src" && in.SrcKey == "secret" && in.DstKey == "k"
				})).Return(domain.ObjectInfo{ETag: "e", ModTime: now}, nil).Once()
			}
			rec := f.do(http.MethodPut, tt.target, "", "x-amz-copy-source", "/src/secret")
			if tt.allowed {
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				return
			}
			requireError(t, rec, http.StatusForbidden, "AccessDenied")
			assert.Equal(t, "There were headers present in the request which were not signed", s3Error(t, rec).Message)
		})
	}
}

// Regression: the metrics endpoints were served to anyone. By default
// they require a bearer token whose identity is allowed admin:Prometheus;
// WithPublicMetrics opens them.
func TestMetricsRoutesAuthentication(t *testing.T) {
	registry := metrics.New(metrics.Deps{}, metrics.WithoutRuntimeCollectors())
	_, err := s3.NewMetricsRoutes(s3.MetricsDeps{Registry: registry})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument, "protected routes need a verifier and a policy engine")
	_, err = s3.NewMetricsRoutes(s3.MetricsDeps{Registry: registry, Verifier: authmock.NewVerifier(t)})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)

	scraper := &domain.Identity{AccessKey: "prom", AuthType: domain.AuthTypeBearer}
	tests := []struct {
		name   string
		header string
		setup  func(v *authmock.Verifier, p *policymock.Service)
		want   int
	}{
		{name: "no token", want: http.StatusForbidden},
		{name: "other scheme", header: "AWS4-HMAC-SHA256 Credential=x", want: http.StatusForbidden},
		{name: "empty bearer", header: "Bearer ", want: http.StatusForbidden},
		{name: "invalid token", header: "Bearer bad", want: http.StatusForbidden, setup: func(v *authmock.Verifier, _ *policymock.Service) {
			v.EXPECT().VerifyBearerToken(mock.Anything, "bad").Return(nil, domain.NewS3Error(domain.ErrAccessDenied, "AccessDenied", "The bearer token is not valid.")).Once()
		}},
		{name: "not allowed", header: "Bearer tok", want: http.StatusForbidden, setup: func(v *authmock.Verifier, p *policymock.Service) {
			v.EXPECT().VerifyBearerToken(mock.Anything, "tok").Return(scraper, nil).Once()
			p.EXPECT().Evaluate(mock.Anything, mock.MatchedBy(func(a policy.Args) bool {
				return a.Identity == scraper && a.Action == policy.ActionAdminPrometheus
			})).Return(policy.Decision{Allowed: false, Reason: "no statement"}, nil).Once()
		}},
		{name: "engine failure", header: "Bearer tok", want: http.StatusInternalServerError, setup: func(v *authmock.Verifier, p *policymock.Service) {
			v.EXPECT().VerifyBearerToken(mock.Anything, "tok").Return(scraper, nil).Once()
			p.EXPECT().Evaluate(mock.Anything, mock.Anything).Return(policy.Decision{}, errors.New("engine down")).Once()
		}},
		{name: "allowed, scheme case-insensitive", header: "bearer tok", want: http.StatusOK, setup: func(v *authmock.Verifier, p *policymock.Service) {
			v.EXPECT().VerifyBearerToken(mock.Anything, "tok").Return(scraper, nil).Once()
			p.EXPECT().Evaluate(mock.Anything, mock.Anything).Return(policy.Decision{Allowed: true}, nil).Once()
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, p := authmock.NewVerifier(t), policymock.NewService(t)
			if tt.setup != nil {
				tt.setup(v, p)
			}
			mr, err := s3.NewMetricsRoutes(s3.MetricsDeps{Registry: registry, Verifier: v, Policy: p, Clock: clock.NewFake(now)})
			require.NoError(t, err)
			r := chi.NewRouter()
			mr.Register(r)
			req := request(http.MethodGet, s3.PathMetricsCluster, "")
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			rec := serve(r, req)
			require.Equal(t, tt.want, rec.Code, rec.Body.String())
			if tt.want == http.StatusOK {
				assert.Contains(t, rec.Header().Get("Content-Type"), "text/plain")
			} else {
				assert.Contains(t, rec.Header().Get("Content-Type"), "xml")
			}
		})
	}
	// Public routes need nothing but the registry.
	mr, err := s3.NewMetricsRoutes(s3.MetricsDeps{Registry: registry}, s3.WithPublicMetrics(true))
	require.NoError(t, err)
	r := chi.NewRouter()
	mr.Register(r)
	assert.Equal(t, http.StatusOK, serve(r, request(http.MethodGet, s3.PathMetricsBucket, "")).Code)
}
