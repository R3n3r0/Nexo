package s3

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/ilm"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
)

// headerAmzExpiration is the expiration of an object under the lifecycle
// of its bucket: expiry-date="<RFC 1123 date>", rule-id="<rule>".
const headerAmzExpiration = "x-amz-expiration"

// LifecycleRoutes exposes Put/Get/DeleteBucketLifecycleConfiguration
// (S-BKT-07). As a middleware of the S3 listener (innermost) it adds the
// x-amz-expiration header to the PUT, copy, complete-multipart, GET and
// HEAD responses of the objects a lifecycle rule expires.
type LifecycleRoutes interface {
	Routes
	Group
	middleware.Middleware
}

// LifecycleDeps are the dependencies of the lifecycle routes. Buckets
// stores the documents, Lifecycle validates them and computes the
// expirations; without Buckets, Lifecycle or Policy the operations answer
// NotImplemented and no header is added.
type LifecycleDeps struct {
	Buckets   bucket.Service
	Lifecycle ilm.Service
	Policy    policy.Service
	Enc       httpx.Codec
	Clock     clock.Clock
	Logger    *slog.Logger
}

type lifecycleRoutes struct {
	deps  LifecycleDeps
	opts  options
	authz authorizer
}

var _ LifecycleRoutes = (*lifecycleRoutes)(nil)

// NewLifecycleRoutes returns the lifecycle route group. WithDomains
// resolves the virtual-host requests of the middleware.
func NewLifecycleRoutes(deps LifecycleDeps, opts ...Option) LifecycleRoutes {
	if deps.Enc == nil {
		deps.Enc = httpx.New()
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &lifecycleRoutes{deps: deps, opts: newOptions(opts), authz: authorizer{policy: deps.Policy, enc: deps.Enc, clock: deps.Clock}}
}

func (h *lifecycleRoutes) available() bool {
	return h.deps.Buckets != nil && h.deps.Lifecycle != nil && h.deps.Policy != nil
}

// Mount implements Group.
func (h *lifecycleRoutes) Mount(d Dispatcher) {
	get, put, del := http.MethodGet, http.MethodPut, http.MethodDelete
	mountAll(d, h.deps.Enc, map[Operation]http.HandlerFunc{
		{Name: "GetBucketLifecycle", Method: get, Scope: ScopeBucket, Query: "lifecycle"}:    h.get,
		{Name: "PutBucketLifecycle", Method: put, Scope: ScopeBucket, Query: "lifecycle"}:    h.put,
		{Name: "DeleteBucketLifecycle", Method: del, Scope: ScopeBucket, Query: "lifecycle"}: h.delete,
	}, h.available())
}

// Register implements Routes.
func (h *lifecycleRoutes) Register(r chi.Router) { registerStandalone(r, h.deps.Enc, h) }

func (h *lifecycleRoutes) get(w http.ResponseWriter, r *http.Request) {
	name := BucketName(r)
	if !h.authz.authorize(w, r, name, "") {
		return
	}
	doc, err := h.deps.Buckets.GetLifecycle(r.Context(), name)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	writeRawXML(w, doc)
}

func (h *lifecycleRoutes) put(w http.ResponseWriter, r *http.Request) {
	name := BucketName(r)
	if !h.authz.authorize(w, r, name, "") {
		return
	}
	body, err := readConfigBody(r)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	doc, err := h.deps.Lifecycle.Validate(r.Context(), body)
	if err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	if err := h.deps.Buckets.SetLifecycle(r.Context(), name, doc); err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *lifecycleRoutes) delete(w http.ResponseWriter, r *http.Request) {
	name := BucketName(r)
	if !h.authz.authorize(w, r, name, "") {
		return
	}
	if err := h.deps.Buckets.DeleteLifecycle(r.Context(), name); err != nil {
		h.deps.Enc.WriteS3Error(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Wrap implements middleware.Middleware: the object responses get their
// x-amz-expiration header.
func (h *lifecycleRoutes) Wrap(next http.Handler) http.Handler {
	if !h.available() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t := resolveTarget(h.opts, r)
		if !objectRequest(t) || !expirationMethod(r.Method) {
			next.ServeHTTP(w, r)
			return
		}
		ew := &headerWriter{ResponseWriter: w}
		ew.before = func(status int) { h.addExpiration(ew, r, t, status) }
		next.ServeHTTP(ew, r)
	})
}

// expirationMethod reports the methods of the object operations that
// report an expiration.
func expirationMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPost:
		return true
	}
	return false
}

// expirationOperations are the operations whose response carries
// x-amz-expiration.
func expirationOperations() []string {
	return []string{
		"PutObject", "CopyObject", "CompleteMultipartUpload", "GetObject", "HeadObject",
		presignedPrefix + "PutObject", presignedPrefix + "GetObject", presignedPrefix + "HeadObject",
	}
}

// expirationOperation reports whether the request is one of the
// expirationOperations: by the name the Dispatcher recorded, or by its
// shape when no operation holder is installed.
func expirationOperation(r *http.Request) bool {
	if op := httpx.OperationNameFrom(r.Context()); op != "" {
		for _, name := range expirationOperations() {
			if op == name {
				return true
			}
		}
		return false
	}
	q := r.URL.Query()
	switch r.Method {
	case http.MethodPost:
		return q.Has("uploadId")
	case http.MethodPut:
		return !q.Has("uploadId") && !q.Has("tagging") && !q.Has("acl") && !q.Has("retention") && !q.Has("legal-hold")
	default:
		for name := range q {
			if strings.HasPrefix(strings.ToLower(name), "x-amz-") || strings.HasPrefix(name, "response-") {
				continue
			}
			if name != "versionId" && name != "partNumber" {
				return false
			}
		}
		return true
	}
}

// addExpiration sets the header on a successful object response.
func (h *lifecycleRoutes) addExpiration(w http.ResponseWriter, r *http.Request, t target, status int) {
	if (status != http.StatusOK && status != http.StatusPartialContent) || !expirationOperation(r) {
		return
	}
	versionID := w.Header().Get(httpx.HeaderAmzVersionID)
	if versionID == "" {
		versionID = r.URL.Query().Get("versionId")
	}
	exp, err := h.deps.Lifecycle.Expiration(r.Context(), t.bucket, t.key, versionID)
	if err != nil {
		h.deps.Logger.DebugContext(r.Context(), "s3: expiration not computed", "bucket", t.bucket, "key", t.key, "err", err)
		return
	}
	if !exp.Date.IsZero() {
		w.Header().Set(headerAmzExpiration, fmt.Sprintf(`expiry-date="%s", rule-id="%s"`, exp.Date.UTC().Format(http.TimeFormat), exp.RuleID))
	}
}

// --- helpers shared with the notification routes ---------------------------------

// resolveTarget resolves the bucket and the key of a request like the
// Dispatcher does (the middlewares run before it).
func resolveTarget(o options, r *http.Request) target {
	return (&dispatcher{domains: o.domains, hostBuckets: o.hostBuckets}).resolve(r)
}

// objectRequest reports a request on an object, not on the reserved
// system paths of the listener.
func objectRequest(t target) bool {
	return t.scope == ScopeObject && (t.virtualHost || t.bucket != domain.ReservedBucket)
}

// readConfigBody reads a configuration document (at most maxXMLBody,
// Content-MD5 verified).
func readConfigBody(r *http.Request) ([]byte, error) {
	body, err := readBody(r, maxXMLBody)
	if errors.Is(err, errBodyTooLarge) {
		return nil, domain.NewS3Error(domain.ErrMalformedXML, "MaxMessageLengthExceeded", "Your request was too big.")
	}
	return body, err
}

// writeRawXML writes a stored XML document.
func writeRawXML(w http.ResponseWriter, doc []byte) {
	w.Header().Set(headerContentType, httpx.ContentTypeXML)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(doc)
}

// headerWriter calls before with the status right before the response
// header is written, so that a middleware can add headers that depend on
// what the handler did. It keeps http.Flusher and http.ResponseController
// working (Unwrap).
type headerWriter struct {
	http.ResponseWriter
	before func(status int)
	wrote  bool
	status int
}

func (w *headerWriter) WriteHeader(status int) {
	if !w.wrote {
		w.wrote, w.status = true, status
		if w.before != nil {
			w.before(status)
		}
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *headerWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// ReadFrom implements io.ReaderFrom: a file section (httpx.FileSection)
// goes on to the ReadFrom of the wrapped writer (sendfile, see httpx); any
// other reader is copied through Write as io.Copy does.
func (w *headerWriter) ReadFrom(src io.Reader) (int64, error) {
	if _, ok := httpx.FileSection(src); !ok {
		return httpx.CopyPooled(struct{ io.Writer }{w}, src)
	}
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return httpx.CopyPooled(w.ResponseWriter, src)
}

// Flush implements http.Flusher.
func (w *headerWriter) Flush() {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack implements http.Hijacker when the wrapped writer does.
func (w *headerWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

// Unwrap returns the wrapped writer (http.ResponseController).
func (w *headerWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// statusCode returns the status written (200 when the handler wrote
// nothing).
func (w *headerWriter) statusCode() int {
	if !w.wrote {
		return http.StatusOK
	}
	return w.status
}
