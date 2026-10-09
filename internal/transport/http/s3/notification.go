package s3

import (
	"log/slog"
	"net"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/event"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
)

// NotificationRoutes exposes Put/GetBucketNotificationConfiguration
// (S-BKT-10). As a middleware of the S3 listener (innermost, after the
// authentication) it puts the request in the context of every S3 request,
// so that the events the services publish carry its identity, client and
// request ids, and it publishes s3:ObjectAccessed:Head for the successful
// HeadObject (the object layer publishes the other events).
type NotificationRoutes interface {
	Routes
	Group
	middleware.Middleware
}

// NotificationDeps are the dependencies of the notification routes.
// Buckets stores the documents, Events validates them (the destinations
// must be configured targets) and receives the HEAD events; without
// Buckets, Events or Policy the operations answer NotImplemented and the
// middleware passes the requests through.
type NotificationDeps struct {
	Buckets bucket.Service
	Events  event.Service
	Policy  policy.Service
	Enc     httpx.Codec
	Clock   clock.Clock
	Logger  *slog.Logger
}

type notificationRoutes struct {
	deps  NotificationDeps
	opts  options
	authz authorizer
}

var _ NotificationRoutes = (*notificationRoutes)(nil)

// NewNotificationRoutes returns the notification route group. WithRegion
// sets the region of the events, WithDomains resolves the virtual-host
// requests of the middleware.
func NewNotificationRoutes(deps NotificationDeps, opts ...Option) NotificationRoutes {
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &notificationRoutes{deps: deps, opts: newOptions(opts), authz: authorizer{policy: deps.Policy, enc: deps.Enc, clock: deps.Clock}}
}

func (h *notificationRoutes) available() bool {
	return h.deps.Buckets != nil && h.deps.Events != nil && h.deps.Policy != nil
}

// Mount implements Group.
func (h *notificationRoutes) Mount(d Dispatcher) {
	mountAll(d, h.deps.Enc, map[Operation]http.HandlerFunc{
		{Name: "GetBucketNotification", Method: http.MethodGet, Scope: ScopeBucket, Query: "notification"}: h.get,
		{Name: "PutBucketNotification", Method: http.MethodPut, Scope: ScopeBucket, Query: "notification"}: h.put,
	}, h.available())
}

// Register implements Routes.
func (h *notificationRoutes) Register(r chi.Router) { registerStandalone(r, h.deps.Enc, h) }

func (h *notificationRoutes) get(w http.ResponseWriter, r *http.Request) {
	name := BucketName(r)
	if !h.authz.authorize(w, r, name, "") {
		return
	}
	doc, err := h.deps.Buckets.GetNotification(r.Context(), name)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	writeRawXML(w, doc)
}

func (h *notificationRoutes) put(w http.ResponseWriter, r *http.Request) {
	name := BucketName(r)
	if !h.authz.authorize(w, r, name, "") {
		return
	}
	body, err := readConfigBody(r)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	doc, err := h.deps.Events.ValidateBucketConfig(r.Context(), body)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	if err := h.deps.Buckets.SetNotification(r.Context(), name, doc); err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// Wrap implements middleware.Middleware.
func (h *notificationRoutes) Wrap(next http.Handler) http.Handler {
	if h.deps.Events == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		r = r.WithContext(event.WithRequest(ctx, requestOf(r)))
		t := resolveTarget(h.opts, r)
		if r.Method != http.MethodHead || !objectRequest(t) {
			next.ServeHTTP(w, r)
			return
		}
		hw := &headerWriter{ResponseWriter: w}
		next.ServeHTTP(hw, r)
		if hw.statusCode() == http.StatusOK && headOperation(r) {
			h.publishHead(r, t, hw.Header())
		}
	})
}

// requestOf describes the request for the events it causes.
func requestOf(r *http.Request) event.Request {
	ctx := r.Context()
	client := middleware.ClientOf(r)
	req := event.Request{
		SourceIP:  client.IP,
		RequestID: httpx.RequestIDFrom(ctx),
		HostID:    httpx.HostIDFrom(ctx),
		UserAgent: r.UserAgent(),
		Operation: func() string { return httpx.OperationNameFrom(ctx) },
	}
	if host, port, err := net.SplitHostPort(r.RemoteAddr); err == nil && host == client.IP {
		req.Port = port
	}
	if id, ok := middleware.IdentityFromContext(ctx); ok && !id.IsAnonymous {
		req.PrincipalID = id.AccessKey
	}
	return req
}

// headOperation reports a HeadObject (plain or presigned), by the name the
// Dispatcher recorded when an operation holder is installed.
func headOperation(r *http.Request) bool {
	switch httpx.OperationNameFrom(r.Context()) {
	case "", "HeadObject", presignedPrefix + "HeadObject":
		return true
	}
	return false
}

// publishHead publishes s3:ObjectAccessed:Head from the response headers.
func (h *notificationRoutes) publishHead(r *http.Request, t target, hdr http.Header) {
	now := h.deps.Clock.Now()
	e := event.NewEvent(event.ObjectAccessedHead, h.region(), now)
	e.S3.Bucket = event.Bucket{Name: t.bucket}
	size, _ := strconv.ParseInt(hdr.Get("Content-Length"), 10, 64)
	e.S3.Object = event.Object{
		Key:          t.key,
		Size:         size,
		ETag:         trimETag(hdr.Get("ETag")),
		ContentType:  hdr.Get(headerContentType),
		UserMetadata: httpx.UserMetadata(hdr),
		VersionID:    hdr.Get(httpx.HeaderAmzVersionID),
		Sequencer:    strconv.FormatInt(now.UnixNano(), 16),
	}
	if len(e.S3.Object.UserMetadata) == 0 {
		e.S3.Object.UserMetadata = nil
	}
	if err := h.deps.Events.Publish(r.Context(), e); err != nil {
		h.deps.Logger.DebugContext(r.Context(), "s3: head event not published", "bucket", t.bucket, "key", t.key, "err", err)
	}
}

// region is the region of the events.
func (h *notificationRoutes) region() string {
	if h.opts.region == "" {
		return defaultRegion
	}
	return h.opts.region
}
