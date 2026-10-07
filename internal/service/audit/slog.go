package audit

import (
	"context"
	"log/slog"

	"github.com/r3n3r0/nexo/internal/clock"
)

// Deps are the dependencies of the slog audit sink.
type Deps struct {
	Logger *slog.Logger
	Clock  clock.Clock
}

// Option configures the slog sink.
type Option func(*slogService)

// WithLevel sets the level audit records are logged at (default Info).
func WithLevel(l slog.Level) Option {
	return func(s *slogService) { s.level = l }
}

// slogService writes audit entries to a structured logger.
type slogService struct {
	deps  Deps
	level slog.Level
}

var _ Service = (*slogService)(nil)

// NewSlog returns an audit service that logs entries with slog.
func NewSlog(deps Deps, opts ...Option) Service {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	s := &slogService{deps: deps, level: slog.LevelInfo}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Log implements Service.
func (s *slogService) Log(ctx context.Context, e Entry) error {
	if e.Version == "" {
		e.Version = EntryVersion
	}
	if e.Time.IsZero() {
		e.Time = s.deps.Clock.Now()
	}
	attrs := []slog.Attr{
		slog.String("version", e.Version),
		slog.Time("time", e.Time),
		slog.String("request_id", e.RequestID),
		slog.String("api", e.API.Name),
		slog.String("bucket", e.API.Bucket),
		slog.String("object", e.API.Object),
		slog.Int("status_code", e.API.StatusCode),
		slog.Int64("rx", e.API.InputBytes),
		slog.Int64("tx", e.API.OutputBytes),
		slog.Duration("time_to_response", e.API.TimeToResponse),
		slog.String("remote_host", e.RemoteHost),
		slog.String("user_agent", e.UserAgent),
		slog.String("access_key", e.AccessKey),
	}
	if e.ParentUser != "" {
		attrs = append(attrs, slog.String("parent_user", e.ParentUser))
	}
	if e.Error != "" {
		attrs = append(attrs, slog.String("error", e.Error))
	}
	if len(e.Tags) > 0 {
		attrs = append(attrs, slog.Any("tags", e.Tags))
	}
	s.deps.Logger.LogAttrs(ctx, s.level, "audit", attrs...)
	return nil
}
