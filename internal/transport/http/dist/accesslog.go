package dist

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/r3n3r0/nexo/internal/service/distribution"
)

// AccessLogDeps are the dependencies of the access log.
type AccessLogDeps struct {
	// Logger receives the lines (default slog.Default()).
	Logger *slog.Logger
}

type accessLog struct {
	logger *slog.Logger
}

var _ AccessLogger = (*accessLog)(nil)

// NewAccessLog returns the access log of the distributions: a JSON record
// (the attributes of the entry; event_time is the start of the request,
// the time key being the one of the record) or a line in the combined log
// format, as the distribution asks.
func NewAccessLog(deps AccessLogDeps) AccessLogger {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &accessLog{logger: deps.Logger}
}

// Log implements AccessLogger.
func (l *accessLog) Log(ctx context.Context, e AccessEntry) {
	if e.Format == distribution.LogFormatCombined {
		l.logger.InfoContext(ctx, combinedLine(e), "distribution", e.Distribution)
		return
	}
	l.logger.LogAttrs(ctx, slog.LevelInfo, "distribution access",
		slog.String("distribution", e.Distribution), slog.String("host", e.Host), slog.String("alias", e.Alias),
		slog.String("method", e.Method), slog.String("uri", e.URI), slog.String("proto", e.Protocol),
		slog.Int("status", e.Status), slog.Int64("bytes", e.Bytes), slog.Float64("duration_ms", float64(e.Duration.Microseconds())/1000),
		slog.String("client_ip", e.ClientIP), slog.String("user_agent", e.UserAgent), slog.String("referer", e.Referer),
		slog.String("request_id", e.RequestID), slog.Time("event_time", e.Time))
}

// combinedLine formats an entry in the Apache/nginx combined log format.
func combinedLine(e AccessEntry) string {
	size := "-"
	if e.Bytes > 0 {
		size = strconv.FormatInt(e.Bytes, 10)
	}
	return fmt.Sprintf(`%s - - [%s] "%s %s %s" %d %s "%s" "%s"`,
		dash(e.ClientIP), e.Time.Format("02/Jan/2006:15:04:05 -0700"), e.Method, quote(e.URI), e.Protocol,
		e.Status, size, quote(e.Referer), quote(e.UserAgent))
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// quote escapes the quotes and the control characters of a field.
func quote(s string) string {
	if s == "" {
		return "-"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
