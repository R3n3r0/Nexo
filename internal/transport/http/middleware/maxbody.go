package middleware

import "net/http"

type maxBody struct {
	limit int64
}

var _ Middleware = (*maxBody)(nil)

// NewMaxBody returns a middleware limiting request bodies to limit bytes
// with http.MaxBytesReader. Reads beyond the limit fail and the connection
// is closed after the response. A non-positive limit disables the check.
func NewMaxBody(limit int64) Middleware {
	return &maxBody{limit: limit}
}

// Wrap implements Middleware.
func (m *maxBody) Wrap(next http.Handler) http.Handler {
	if m.limit <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && r.Body != http.NoBody {
			r.Body = http.MaxBytesReader(w, r.Body, m.limit)
		}
		next.ServeHTTP(w, r)
	})
}
