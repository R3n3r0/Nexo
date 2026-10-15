package middleware

import (
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// The observability middlewares (audit, API metrics, live trace,
// distributed tracing) share the operation and identity holders of a
// request: the outermost one installs them, the inner ones reuse them, so
// that each sees the operation the router names and the identity the
// authentication establishes, whatever their order.

type operationHolderKey struct{}

// holdOperation returns ctx carrying the operation holder of the request:
// the one an outer middleware installed, or a new one.
func holdOperation(ctx context.Context) (context.Context, *httpx.OperationHolder) {
	if h, ok := ctx.Value(operationHolderKey{}).(*httpx.OperationHolder); ok {
		return ctx, h
	}
	ctx, h := httpx.WithOperationHolder(ctx)
	return context.WithValue(ctx, operationHolderKey{}, h), h
}

// holdIdentity returns ctx carrying an identity holder (the one of an
// outer middleware when there is one).
func holdIdentity(ctx context.Context) context.Context {
	if _, ok := ctx.Value(identityHolderKey{}).(*identityHolder); ok {
		return ctx
	}
	return WithIdentityHolder(ctx)
}

// observe returns the context of a request with both holders.
func observe(ctx context.Context) (context.Context, *httpx.OperationHolder) {
	ctx, h := holdOperation(ctx)
	return holdIdentity(ctx), h
}

// callerOf returns the identity established while the request was served:
// below the middleware (held) or before it.
func callerOf(ctx context.Context) (*domain.Identity, bool) {
	if id, ok := HeldIdentity(ctx); ok {
		return id, true
	}
	return IdentityFromContext(ctx)
}

// authenticated reports a caller that is not anonymous.
func authenticated(id *domain.Identity, ok bool) bool {
	return ok && id != nil && !id.IsAnonymous && (id.AccessKey != "" || id.IsRoot)
}

// countingBody counts the bytes of a request body read by the handler.
type countingBody struct {
	io.ReadCloser
	n atomic.Int64
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.n.Add(int64(n))
	return n, err
}

// countBody wraps the body of r; the returned function reports the bytes
// read so far (the declared length when nothing was read through it).
func countBody(r *http.Request) func() int64 {
	if r.Body == nil || r.Body == http.NoBody {
		return func() int64 { return 0 }
	}
	cb := &countingBody{ReadCloser: r.Body}
	r.Body = cb
	return func() int64 {
		if n := cb.n.Load(); n > 0 {
			return n
		}
		return max(r.ContentLength, 0)
	}
}

// targetOf returns the bucket and object of an S3 request: from the host
// of a virtual-host request ("<bucket>.<domain>"), from the path
// otherwise.
func targetOf(r *http.Request, domains []string) (bucket, object string) {
	if len(domains) > 0 {
		host := strings.ToLower(r.Host)
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		for _, d := range domains {
			if b, ok := strings.CutSuffix(host, "."+strings.ToLower(d)); ok && b != "" {
				return b, strings.TrimPrefix(r.URL.Path, "/")
			}
		}
	}
	return splitPath(r.URL.Path)
}

// adminBucket returns the bucket a request of the Admin API names
// (/api/v1/buckets/{bucket}/...).
func adminBucket(path string) string {
	rest, ok := strings.CutPrefix(path, "/api/v1/buckets/")
	if !ok {
		return ""
	}
	bucket, _, _ := strings.Cut(rest, "/")
	return bucket
}

// requestIDOf returns the request id of a served request: the one of the
// context, or the one the request-id middleware answered with.
func requestIDOf(ctx context.Context, w http.ResponseWriter) string {
	if id := httpx.RequestIDFrom(ctx); id != "" {
		return id
	}
	return w.Header().Get(httpx.HeaderAmzRequestID)
}

// statusError describes a failed status ("" below 400).
func statusError(status int) string {
	if status < http.StatusBadRequest {
		return ""
	}
	if t := http.StatusText(status); t != "" {
		return t
	}
	return "status " + strconv.Itoa(status)
}
