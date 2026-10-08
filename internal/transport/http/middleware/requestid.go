package middleware

import (
	"net/http"

	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

type requestID struct {
	ids httpx.IDGenerator
}

var _ Middleware = (*requestID)(nil)

// NewRequestID returns a middleware that assigns x-amz-request-id and
// x-amz-id-2 to every request, stores them in the context and echoes them
// in the response headers.
func NewRequestID(ids httpx.IDGenerator) Middleware {
	if ids == nil {
		ids = httpx.NewIDGenerator()
	}
	return &requestID{ids: ids}
}

// Wrap implements Middleware.
func (m *requestID) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := m.ids.NewRequestID()
		hostID := m.ids.NewHostID()
		w.Header().Set(httpx.HeaderAmzRequestID, reqID)
		w.Header().Set(httpx.HeaderAmzID2, hostID)
		next.ServeHTTP(w, r.WithContext(httpx.WithRequestID(r.Context(), reqID, hostID)))
	})
}
