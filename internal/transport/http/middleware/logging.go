package middleware

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

type logging struct {
	logger *slog.Logger
	clock  clock.Clock
	level  slog.Level
}

var _ Middleware = (*logging)(nil)

// LoggingOption configures the logging middleware.
type LoggingOption func(*logging)

// WithLogLevel sets the level of access log records (default Info).
func WithLogLevel(l slog.Level) LoggingOption {
	return func(m *logging) { m.level = l }
}

// WithLogClock sets the clock used to measure durations.
func WithLogClock(c clock.Clock) LoggingOption {
	return func(m *logging) {
		if c != nil {
			m.clock = c
		}
	}
}

// NewLogging returns an access-log middleware writing one slog record per
// request: method, path, status, bytes, duration, remote address, request id.
func NewLogging(logger *slog.Logger, opts ...LoggingOption) Middleware {
	if logger == nil {
		logger = slog.Default()
	}
	m := &logging{logger: logger, clock: clock.NewReal(), level: slog.LevelInfo}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Wrap implements Middleware.
func (m *logging) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := m.clock.Now()
		rw := wrapWriter(w)
		next.ServeHTTP(rw, r)
		m.logger.LogAttrs(r.Context(), m.level, "http request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.String("query", r.URL.RawQuery),
			slog.Int("status", rw.Status()),
			slog.Int64("bytes", rw.bytes),
			slog.Duration("duration", m.clock.Now().Sub(start).Round(time.Microsecond)),
			slog.String("remote", r.RemoteAddr),
			slog.String("user_agent", r.UserAgent()),
			slog.String("request_id", httpx.RequestIDFrom(r.Context())),
		)
	})
}
