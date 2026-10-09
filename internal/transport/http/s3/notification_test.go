package s3_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	bucketmock "github.com/r3n3r0/nexo/internal/service/bucket/mock"
	"github.com/r3n3r0/nexo/internal/service/event"
	eventmock "github.com/r3n3r0/nexo/internal/service/event/mock"
	objectmock "github.com/r3n3r0/nexo/internal/service/object/mock"
	"github.com/r3n3r0/nexo/internal/service/policy"
	policymock "github.com/r3n3r0/nexo/internal/service/policy/mock"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
	"github.com/r3n3r0/nexo/internal/transport/http/s3"
)

const notificationDoc = `<NotificationConfiguration><QueueConfiguration><Queue>arn:nexo:sqs:us-east-1:_:webhook</Queue><Event>s3:ObjectCreated:*</Event></QueueConfiguration></NotificationConfiguration>`

func newNotificationAPI(t *testing.T, allow bool) (http.Handler, *bucketmock.Service, *eventmock.Service) {
	t.Helper()
	buckets, events, pol := bucketmock.NewService(t), eventmock.NewService(t), policymock.NewService(t)
	pol.EXPECT().Evaluate(mock.Anything, mock.Anything).Return(policy.Decision{Allowed: allow, Reason: "test"}, nil).Maybe()
	pol.EXPECT().Simulate(mock.Anything, mock.Anything).Return(policy.Decision{}, nil).Maybe()
	enc := httpx.New()
	api := s3.NewAPI(s3.APIDeps{Enc: enc},
		s3.NewBucketRoutes(s3.BucketDeps{Buckets: buckets, Objects: objectmock.NewService(t), Policy: pol, Enc: enc}),
		s3.NewNotificationRoutes(s3.NotificationDeps{Buckets: buckets, Events: events, Policy: pol, Enc: enc}),
	)
	r := chi.NewRouter()
	api.Register(r)
	return withRoot(r), buckets, events
}

func TestNotificationRoutes(t *testing.T) {
	h, buckets, events := newNotificationAPI(t, true)
	canonical := []byte(`<?xml version="1.0" encoding="UTF-8"?>` + "\n" + notificationDoc)

	events.EXPECT().ValidateBucketConfig(mock.Anything, []byte(notificationDoc)).Return(canonical, nil).Once()
	buckets.EXPECT().SetNotification(mock.Anything, "photos", canonical).Return(nil).Once()
	rec := serve(h, request(http.MethodPut, "/photos?notification", notificationDoc))
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	buckets.EXPECT().GetNotification(mock.Anything, "photos").Return(canonical, nil).Once()
	rec = serve(h, request(http.MethodGet, "/photos?notification", ""))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, string(canonical), rec.Body.String())

	events.EXPECT().ValidateBucketConfig(mock.Anything, mock.Anything).Return(nil,
		domain.NewS3Error(domain.ErrInvalidArgument, "InvalidArgument", "A specified destination ARN does not exist")).Once()
	requireError(t, serve(h, request(http.MethodPut, "/photos?notification", notificationDoc)), http.StatusBadRequest, "InvalidArgument")

	events.EXPECT().ValidateBucketConfig(mock.Anything, mock.Anything).Return([]byte("<NotificationConfiguration/>"), nil).Once()
	buckets.EXPECT().SetNotification(mock.Anything, "gone", mock.Anything).Return(domain.ErrBucketNotFound).Once()
	requireError(t, serve(h, request(http.MethodPut, "/gone?notification", "<NotificationConfiguration/>")), http.StatusNotFound, "NoSuchBucket")
}

func TestNotificationRoutesAuthorization(t *testing.T) {
	h, _, _ := newNotificationAPI(t, false)
	requireError(t, serve(h, request(http.MethodGet, "/photos?notification", "")), http.StatusForbidden, "AccessDenied")
	requireError(t, serve(h, request(http.MethodPut, "/photos?notification", notificationDoc)), http.StatusForbidden, "AccessDenied")
}

func TestNotificationRoutesUnavailable(t *testing.T) {
	r := chi.NewRouter()
	s3.NewNotificationRoutes(s3.NotificationDeps{}).Register(r)
	requireError(t, serve(r, request(http.MethodGet, "/b?notification", "")), http.StatusNotImplemented, "NotImplemented")
}

// TestNotificationMiddleware: every request carries its description for
// the events, and a successful HeadObject publishes s3:ObjectAccessed:Head.
func TestNotificationMiddleware(t *testing.T) {
	events := eventmock.NewService(t)
	clk := clock.NewFake(time.Date(2026, 10, 9, 10, 0, 0, 0, time.UTC))
	routes := s3.NewNotificationRoutes(s3.NotificationDeps{
		Buckets: bucketmock.NewService(t), Events: events, Policy: policymock.NewService(t), Clock: clk,
	}, s3.WithRegion("eu-south-1"))

	var seen event.Request
	handler := func(op string, status int) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			httpx.SetOperationName(r.Context(), op)
			req, ok := event.RequestFrom(r.Context())
			require.True(t, ok, "the request is in the context")
			seen = req
			w.Header().Set("Content-Length", "42")
			w.Header().Set("ETag", `"abc"`)
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("x-amz-meta-color", "blue")
			w.Header().Set(httpx.HeaderAmzVersionID, "v1")
			w.WriteHeader(status)
		})
	}
	chain := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, _ := httpx.WithOperationHolder(r.Context())
			ctx = httpx.WithRequestID(ctx, "REQ1", "HOST1")
			ctx = middleware.WithIdentity(ctx, &domain.Identity{AccessKey: "alice"})
			r.RemoteAddr = "192.0.2.7:4321"
			r.Header.Set("User-Agent", "test-agent")
			routes.Wrap(next).ServeHTTP(w, r.WithContext(ctx))
		})
	}

	var got event.Event
	var gotReq event.Request
	events.EXPECT().Publish(mock.Anything, mock.Anything).RunAndReturn(func(ctx context.Context, e event.Event) error {
		got = e
		gotReq, _ = event.RequestFrom(ctx)
		return nil
	}).Once()
	rec := serve(chain(handler("HeadObject", http.StatusOK)), request(http.MethodHead, "/photos/a/b.txt", ""))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, event.ObjectAccessedHead, got.EventName)
	assert.Equal(t, "eu-south-1", got.AwsRegion)
	assert.Equal(t, "2026-10-09T10:00:00.000Z", got.EventTime)
	assert.Equal(t, "photos", got.S3.Bucket.Name)
	assert.Equal(t, event.Object{Key: "a/b.txt", Size: 42, ETag: "abc", ContentType: "text/plain",
		UserMetadata: map[string]string{"color": "blue"}, VersionID: "v1", Sequencer: got.S3.Object.Sequencer}, got.S3.Object)
	assert.NotEmpty(t, got.S3.Object.Sequencer)
	assert.Equal(t, "alice", gotReq.PrincipalID)
	assert.Equal(t, "192.0.2.7", gotReq.SourceIP)
	assert.Equal(t, "4321", gotReq.Port)
	assert.Equal(t, "REQ1", gotReq.RequestID)
	assert.Equal(t, "HOST1", gotReq.HostID)
	assert.Equal(t, "test-agent", gotReq.UserAgent)
	require.NotNil(t, gotReq.Operation)
	assert.Equal(t, "HeadObject", gotReq.Operation(), "the operation the Dispatcher routed to")

	// No HEAD event for a failed HEAD, a HEAD of a bucket or another method.
	serve(chain(handler("HeadObject", http.StatusNotFound)), request(http.MethodHead, "/photos/missing", ""))
	serve(chain(handler("HeadBucket", http.StatusOK)), request(http.MethodHead, "/photos", ""))
	serve(chain(handler("GetObject", http.StatusOK)), request(http.MethodGet, "/photos/a", ""))
	assert.Equal(t, "GetObject", seen.Operation())
	assert.Equal(t, "alice", seen.PrincipalID)

	// An anonymous request has no principal.
	anon := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, _ := httpx.WithOperationHolder(r.Context())
		routes.Wrap(handler("GetObject", http.StatusOK)).ServeHTTP(w, r.WithContext(middleware.WithIdentity(ctx, domain.Anonymous())))
	})
	serve(anon, request(http.MethodGet, "/photos/a", ""))
	assert.Empty(t, seen.PrincipalID)
}
