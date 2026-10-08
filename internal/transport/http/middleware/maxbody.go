package middleware

import "net/http"

// DefaultMaxBody is the default request body limit of NewMaxBody (64 MiB).
const DefaultMaxBody = 64 << 20

// MaxBodyDeps are the dependencies of the body-limit middleware (none: the
// struct keeps the constructor shape of every middleware).
type MaxBodyDeps struct{}

// MaxBodyOption configures the body-limit middleware.
type MaxBodyOption func(*maxBody)

// WithMaxBodyLimit sets the limit in bytes; a non-positive limit disables
// the check.
func WithMaxBodyLimit(limit int64) MaxBodyOption {
	return func(m *maxBody) { m.limit = limit }
}

type maxBody struct {
	limit int64
}

var _ Middleware = (*maxBody)(nil)

// NewMaxBody returns a middleware limiting request bodies (DefaultMaxBody
// unless WithMaxBodyLimit says otherwise) with http.MaxBytesReader. Reads
// beyond the limit fail and the connection is closed after the response.
func NewMaxBody(_ MaxBodyDeps, opts ...MaxBodyOption) Middleware {
	m := &maxBody{limit: DefaultMaxBody}
	for _, o := range opts {
		o(m)
	}
	return m
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
