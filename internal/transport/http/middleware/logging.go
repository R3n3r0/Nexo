package middleware

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

// LoggingDeps are the dependencies of the access-log middleware.
type LoggingDeps struct {
	// Logger receives the access log (default slog.Default()).
	Logger *slog.Logger
	// Clock measures durations (default wall clock).
	Clock clock.Clock
}

type logging struct {
	deps  LoggingDeps
	level slog.Level
}

var _ Middleware = (*logging)(nil)

// LoggingOption configures the logging middleware.
type LoggingOption func(*logging)

// WithLogLevel sets the level of access log records (default Info).
func WithLogLevel(l slog.Level) LoggingOption {
	return func(m *logging) { m.level = l }
}

// NewLogging returns an access-log middleware writing one slog record per
// request: method, path, status, bytes, duration, remote address, request id.
func NewLogging(deps LoggingDeps, opts ...LoggingOption) Middleware {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	m := &logging{deps: deps, level: slog.LevelInfo}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Wrap implements Middleware.
func (m *logging) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := m.deps.Clock.Now()
		rw := wrapWriter(w)
		next.ServeHTTP(rw, r)
		m.deps.Logger.LogAttrs(r.Context(), m.level, "http request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.String("query", httpx.RedactRawQuery(r.URL.RawQuery)),
			slog.Int("status", rw.Status()),
			slog.Int64("bytes", rw.bytes),
			slog.Duration("duration", m.deps.Clock.Now().Sub(start).Round(time.Microsecond)),
			slog.String("remote", r.RemoteAddr),
			slog.String("user_agent", r.UserAgent()),
			slog.String("request_id", httpx.RequestIDFrom(r.Context())),
		)
	})
}
