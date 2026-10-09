package admin_test

import (
	"context"
	"encoding/xml"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/admin"
	bucketmock "github.com/r3n3r0/nexo/internal/service/bucket/mock"
)

func newCORSManager(t *testing.T) (admin.CORSManager, *bucketmock.Service) {
	t.Helper()
	buckets := bucketmock.NewService(t)
	mgr, err := admin.NewCORSManager(admin.CORSManagerDeps{Buckets: buckets})
	require.NoError(t, err)
	return mgr, buckets
}

func TestNewCORSManagerRequiresDeps(t *testing.T) {
	_, err := admin.NewCORSManager(admin.CORSManagerDeps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

func TestCORSSetStoresTheS3Document(t *testing.T) {
	ctx := context.Background()
	mgr, buckets := newCORSManager(t)
	maxAge := 600
	rule := domain.CORSRule{ID: " web ", AllowedOrigins: []string{" https://app.example.com "}, AllowedMethods: []string{"GET", "PUT"},
		AllowedHeaders: []string{"*"}, ExposeHeaders: []string{"ETag"}, MaxAgeSeconds: &maxAge}
	want := domain.CORSRule{ID: "web", AllowedOrigins: []string{"https://app.example.com"}, AllowedMethods: []string{"GET", "PUT"},
		AllowedHeaders: []string{"*"}, ExposeHeaders: []string{"ETag"}, MaxAgeSeconds: &maxAge}
	buckets.EXPECT().GetVersioning(mock.Anything, "bkt").Return(domain.VersioningOff, nil)
	buckets.EXPECT().SetCORS(mock.Anything, "bkt", mock.Anything).RunAndReturn(func(_ context.Context, _ string, doc []byte) error {
		var parsed struct {
			XMLName xml.Name `xml:"CORSConfiguration"`
			Rules   []struct {
				ID      string   `xml:"ID"`
				Origins []string `xml:"AllowedOrigin"`
				Methods []string `xml:"AllowedMethod"`
				MaxAge  int      `xml:"MaxAgeSeconds"`
			} `xml:"CORSRule"`
		}
		require.NoError(t, xml.Unmarshal(doc, &parsed))
		require.Len(t, parsed.Rules, 1)
		assert.Equal(t, "web", parsed.Rules[0].ID)
		assert.Equal(t, []string{"https://app.example.com"}, parsed.Rules[0].Origins)
		assert.Equal(t, 600, parsed.Rules[0].MaxAge)
		return nil
	})
	buckets.EXPECT().GetCORSConfig(mock.Anything, "bkt").Return(domain.CORSConfiguration{Rules: []domain.CORSRule{want}}, nil)
	got, err := mgr.Set(ctx, "bkt", admin.CORSConfig{Rules: []domain.CORSRule{rule}})
	require.NoError(t, err)
	assert.Equal(t, []domain.CORSRule{want}, got.Rules)
}

func TestCORSValidationPerRule(t *testing.T) {
	ctx := context.Background()
	mgr, buckets := newCORSManager(t)
	negative := -1
	buckets.EXPECT().GetVersioning(mock.Anything, "bkt").Return(domain.VersioningOff, nil)
	_, err := mgr.Set(ctx, "bkt", admin.CORSConfig{Rules: []domain.CORSRule{
		{AllowedOrigins: []string{"*"}, AllowedMethods: []string{"GET"}},
		{ID: "verb", AllowedOrigins: []string{"*"}, AllowedMethods: []string{"PATCH"}},
		{ID: "empty", AllowedMethods: []string{"GET"}},
		{ID: "age", AllowedOrigins: []string{"*"}, AllowedMethods: []string{"GET"}, MaxAgeSeconds: &negative},
	}})
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
	problems, ok := admin.RuleProblemsOf(err)
	require.True(t, ok)
	assert.Equal(t, []admin.RuleProblem{
		{Rule: 2, ID: "verb", Code: "InvalidRequest", Message: "Found unsupported HTTP method in CORS config. Unsupported method is PATCH"},
		{Rule: 3, ID: "empty", Code: "MalformedXML", Message: "the rule needs an AllowedOrigin and an AllowedMethod"},
		{Rule: 4, ID: "age", Code: "InvalidArgument", Message: "MaxAgeSeconds must not be negative"},
	}, problems)
}

func TestCORSGetAndDelete(t *testing.T) {
	ctx := context.Background()
	mgr, buckets := newCORSManager(t)
	buckets.EXPECT().GetCORSConfig(mock.Anything, "bkt").Return(domain.CORSConfiguration{},
		domain.NewS3Error(domain.ErrPolicyNotFound, "NoSuchCORSConfiguration", "none"))
	got, err := mgr.Get(ctx, "bkt")
	require.NoError(t, err)
	assert.Equal(t, &admin.CORSConfig{Rules: []domain.CORSRule{}}, got, "no configuration is no rule")

	buckets.EXPECT().GetVersioning(mock.Anything, "bkt").Return(domain.VersioningOff, nil)
	buckets.EXPECT().DeleteCORS(mock.Anything, "bkt").Return(nil).Twice()
	got, err = mgr.Set(ctx, "bkt", admin.CORSConfig{})
	require.NoError(t, err)
	assert.Empty(t, got.Rules)
	require.NoError(t, mgr.Delete(ctx, "bkt"))

	buckets.EXPECT().GetCORSConfig(mock.Anything, "nope").Return(domain.CORSConfiguration{}, domain.ErrBucketNotFound)
	_, err = mgr.Get(ctx, "nope")
	assert.ErrorIs(t, err, domain.ErrBucketNotFound)
}
