package middleware

import (
	"net/http"

	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// RequestIDDeps are the dependencies of the request-id middleware.
type RequestIDDeps struct {
	// IDs generates the request and host ids (default httpx.NewIDGenerator()).
	IDs httpx.IDGenerator
}

type requestID struct {
	deps RequestIDDeps
}

var _ Middleware = (*requestID)(nil)

// NewRequestID returns a middleware that assigns x-amz-request-id and
// x-amz-id-2 to every request, stores them in the context and echoes them
// in the response headers.
func NewRequestID(deps RequestIDDeps) Middleware {
	if deps.IDs == nil {
		deps.IDs = httpx.NewIDGenerator()
	}
	return &requestID{deps: deps}
}

// Wrap implements Middleware.
func (m *requestID) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := m.deps.IDs.NewRequestID()
		hostID := m.deps.IDs.NewHostID()
		w.Header().Set(httpx.HeaderAmzRequestID, reqID)
		w.Header().Set(httpx.HeaderAmzID2, hostID)
		next.ServeHTTP(w, r.WithContext(httpx.WithRequestID(r.Context(), reqID, hostID)))
	})
}
