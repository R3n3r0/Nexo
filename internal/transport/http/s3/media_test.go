package s3_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/auth"
	authmock "github.com/r3n3r0/nexo/internal/service/auth/mock"
	"github.com/r3n3r0/nexo/internal/service/media"
	mediamock "github.com/r3n3r0/nexo/internal/service/media/mock"
	"github.com/r3n3r0/nexo/internal/service/policy"
	policymock "github.com/r3n3r0/nexo/internal/service/policy/mock"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
	"github.com/r3n3r0/nexo/internal/transport/http/s3"
)

type mediaFixture struct {
	media  *mediamock.Service
	policy *policymock.Service
	signer *authmock.Signer
	creds  *authmock.CredentialLookup
	router chi.Router
	now    time.Time
}

func newMediaFixture(t *testing.T) *mediaFixture {
	t.Helper()
	f := &mediaFixture{
		media: mediamock.NewService(t), policy: policymock.NewService(t), signer: authmock.NewSigner(t), creds: authmock.NewCredentialLookup(t),
		router: chi.NewRouter(), now: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC),
	}
	routes, err := s3.NewMediaRoutes(s3.MediaDeps{Media: f.media, Policy: f.policy, Signer: f.signer, Creds: f.creds, Clock: clock.NewFake(f.now)},
		s3.WithRegion("eu-south-1"))
	require.NoError(t, err)
	routes.Register(f.router)
	return f
}

// serve sends a request as the identity id (nil: no auth middleware).
func (f *mediaFixture) serve(target string, id *domain.Identity) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Host = "s3.example.com"
	if id != nil {
		req = req.WithContext(middleware.WithIdentity(req.Context(), id))
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

func TestNewMediaRoutesRequiresDeps(t *testing.T) {
	_, err := s3.NewMediaRoutes(s3.MediaDeps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

func TestMediaRoutesAnonymous(t *testing.T) {
	f := newMediaFixture(t)
	f.policy.EXPECT().Evaluate(mock.Anything, mock.MatchedBy(func(a policy.Args) bool {
		return a.Action == policy.ActionGetObject && a.Bucket == "films" && a.Identity.IsAnonymous
	})).Return(policy.Decision{Allowed: true}, nil)
	f.media.EXPECT().MasterPlaylist(mock.Anything, media.Source{Bucket: "films", Key: "a b.mp4"}, mock.Anything).
		RunAndReturn(func(_ context.Context, _ media.Source, l media.Links) ([]byte, error) {
			return []byte(l.Media("e1") + "\n" + l.Segment("e1", 2)), nil
		})
	rec := f.serve(s3.MediaPrefix+"films/master.m3u8?key=a+b.mp4", domain.Anonymous())
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "application/vnd.apple.mpegurl", rec.Header().Get("Content-Type"))
	assert.Equal(t, "media.m3u8?key=a+b.mp4&rev=e1\nsegment.m4s?key=a+b.mp4&n=2&rev=e1", rec.Body.String(), "unsigned URIs")

	f.media.EXPECT().Segment(mock.Anything, media.Source{Bucket: "films", Key: "a b.mp4", Revision: "e1"}, 2).Return([]byte("seg"), nil)
	rec = f.serve(s3.MediaPrefix+"films/segment.m4s?key=a+b.mp4&rev=e1&n=2", domain.Anonymous())
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "video/iso.segment", rec.Header().Get("Content-Type"))
	assert.Equal(t, "seg", rec.Body.String())

	for _, target := range []string{"master.m3u8", "segment.m4s?key=a.mp4&n=1", "segment.m4s?key=a.mp4&rev=e1&n=x"} {
		assert.Equal(t, http.StatusBadRequest, f.serve(s3.MediaPrefix+"films/"+target, domain.Anonymous()).Code, target)
	}
}

func TestMediaRoutesDenied(t *testing.T) {
	f := newMediaFixture(t)
	f.policy.EXPECT().Evaluate(mock.Anything, mock.MatchedBy(func(a policy.Args) bool {
		return a.Action == policy.ActionGetObjectVersion
	})).Return(policy.Decision{Reason: "no"}, nil)
	rec := f.serve(s3.MediaPrefix+"films/master.m3u8?key=a.mp4&versionId=v1", domain.Anonymous())
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

// TestMediaRoutesPresigned: the URIs of a presigned playlist are presigned
// with the credentials of its signer until it expires.
func TestMediaRoutesPresigned(t *testing.T) {
	f := newMediaFixture(t)
	share := &domain.Identity{AccessKey: "share-abc", Parent: "alice", AuthType: domain.AuthTypeQuery}
	f.policy.EXPECT().Evaluate(mock.Anything, mock.Anything).Return(policy.Decision{Allowed: true}, nil)
	creds := domain.Credentials{AccessKey: "share-abc", SecretKey: "secret"}
	f.creds.EXPECT().LookupCredentials(mock.Anything, "share-abc").Return(creds, *share, nil)
	var signed []auth.PresignInput
	f.signer.EXPECT().Presign(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, in auth.PresignInput) (*url.URL, error) {
		signed = append(signed, in)
		u := *in.URL
		q := u.Query()
		q.Set("X-Amz-Signature", "sig")
		u.RawQuery = q.Encode()
		return &u, nil
	})
	f.media.EXPECT().MediaPlaylist(mock.Anything, media.Source{Bucket: "films", Key: "a.mp4", Revision: "e1"}, mock.Anything).
		RunAndReturn(func(_ context.Context, _ media.Source, l media.Links) ([]byte, error) {
			return []byte(l.Init("e1") + "\n" + l.Segment("e1", 0)), nil
		})
	// Signed ten minutes ago for an hour.
	query := "key=a.mp4&rev=e1&X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Date=20261018T115000Z&X-Amz-Expires=3600&X-Amz-Signature=x"
	rec := f.serve(s3.MediaPrefix+"films/media.m3u8?"+query, share)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "init.mp4?X-Amz-Signature=sig&key=a.mp4&rev=e1\nsegment.m4s?X-Amz-Signature=sig&key=a.mp4&n=0&rev=e1", rec.Body.String())
	require.Len(t, signed, 2)
	assert.Equal(t, "s3.example.com", signed[0].URL.Host)
	assert.Equal(t, s3.MediaPrefix+"films/init.mp4", signed[0].URL.Path)
	assert.Equal(t, 50*time.Minute, signed[0].Expires)
	assert.Equal(t, creds, signed[0].Creds)
	assert.Equal(t, "eu-south-1", signed[0].Region)

	// Expired: nothing to sign.
	query = "key=a.mp4&rev=e1&X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Date=20261018T100000Z&X-Amz-Expires=3600&X-Amz-Signature=x"
	rec = f.serve(s3.MediaPrefix+"films/media.m3u8?"+query, share)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}
