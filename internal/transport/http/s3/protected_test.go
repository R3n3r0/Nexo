package s3_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/media"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/policy"
)

// protectVideos makes the videos under courses/ of every bucket
// protected, and allows every action but nexo:DownloadProtectedMedia
// unless download.
func protectVideos(f *fixture, download bool) {
	calls := f.media.ExpectedCalls[:0]
	for _, c := range f.media.ExpectedCalls {
		if c.Method != "Protection" {
			calls = append(calls, c)
		}
	}
	f.media.ExpectedCalls = calls
	f.media.EXPECT().Protection(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, src media.Source) (*domain.MediaProtection, error) {
		p := &domain.MediaProtection{Enabled: true, Prefix: "courses/"}
		if !p.Protects(src.Key) {
			return nil, nil
		}
		return p, nil
	}).Maybe()
	f.policy.EXPECT().Evaluate(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, a policy.Args) (policy.Decision, error) {
		return policy.Decision{Allowed: download || a.Action != policy.ActionDownloadProtectedMedia, Reason: "no"}, nil
	}).Maybe()
}

// TestProtectedVideoDownload: the original of a protected video is not
// read, copied or queried without nexo:DownloadProtectedMedia; the other
// objects, and an identity allowed it, read as usual.
//
// covers: S-OBJ-17
func TestProtectedVideoDownload(t *testing.T) {
	alice := &domain.Identity{AccessKey: "alice", AuthType: domain.AuthTypeHeader}
	f := newFixture(t, withStrictPolicy(), asIdentity(alice))
	f.policy.EXPECT().Simulate(mock.Anything, mock.Anything).Return(policy.Decision{}, nil).Maybe()
	protectVideos(f, false)

	requireError(t, f.do(http.MethodGet, "/b/courses/intro.mp4", ""), http.StatusForbidden, "AccessDenied")
	requireError(t, f.do(http.MethodPut, "/b/copy.mp4", "", "x-amz-copy-source", "/b/courses/intro.mp4"), http.StatusForbidden, "AccessDenied")
	requireError(t, f.do(http.MethodPut, "/b/copy.mp4?partNumber=1&uploadId=u", "", "x-amz-copy-source", "/b/courses/intro.mp4"), http.StatusForbidden, "AccessDenied")

	f.objects.EXPECT().GetObject(mock.Anything, "b", "public/intro.mp4", mock.Anything).Return(&object.Reader{
		ReadCloser: io.NopCloser(bytes.NewReader([]byte("mp4"))), Info: domain.ObjectInfo{Bucket: "b", Key: "public/intro.mp4", Size: 3}, Length: 3,
	}, nil)
	rec := f.do(http.MethodGet, "/b/public/intro.mp4", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	g := newFixture(t, withStrictPolicy(), asIdentity(alice))
	g.policy.EXPECT().Simulate(mock.Anything, mock.Anything).Return(policy.Decision{}, nil).Maybe()
	protectVideos(g, true)
	g.objects.EXPECT().GetObject(mock.Anything, "b", "courses/intro.mp4", mock.Anything).Return(&object.Reader{
		ReadCloser: io.NopCloser(bytes.NewReader([]byte("mp4"))), Info: domain.ObjectInfo{Bucket: "b", Key: "courses/intro.mp4", Size: 3}, Length: 3,
	}, nil)
	rec = g.do(http.MethodGet, "/b/courses/intro.mp4", "")
	assert.Equal(t, http.StatusOK, rec.Code, "allowed nexo:DownloadProtectedMedia")
}
