package settings

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
)

// Subsystems and keys of the dynamic configuration applied by the server.
const (
	SubsystemAPI        = "api"
	KeyRootAccess       = "root_access"
	KeyCORSAllowOrigin  = "cors_allow_origin"
	KeyRequestsMax      = "requests_max"
	KeyRequestsDeadline = "requests_deadline"

	// SubsystemRateLimit holds the token buckets of the requests
	// (S-OPS-10): every key is a rate in requests per second (0: no
	// limit), with a "<key>_burst" companion.
	SubsystemRateLimit = "ratelimit"
	KeyS3IP            = "s3_ip"
	KeyS3AccessKey     = "s3_access_key"
	KeyS3Bucket        = "s3_bucket"
	KeyAdminIP         = "admin_ip"
	KeyAdminUser       = "admin_user"
	// BurstSuffix names the burst of a rate key ("s3_ip_burst").
	BurstSuffix = "_burst"

	// SubsystemStorage tunes the metadata log of the local drives
	// (applied at once to the drives that use it).
	SubsystemStorage = "storage"
	// KeyMetadataLogSegmentMiB is the size of the next segments, in MiB.
	KeyMetadataLogSegmentMiB = "metadata_log_segment_mib"
	// KeyMetadataLogMemoryPercent is the share of the RAM the index of a
	// drive may use before a warning.
	KeyMetadataLogMemoryPercent = "metadata_log_memory_percent"

	// SubsystemCompression selects the new versions stored compressed
	// (S-OBJ-14): by key extension or content type.
	SubsystemCompression = "compression"
	KeyCompressionEnable = "enable"
	KeyExtensions        = "extensions"
	KeyMimeTypes         = "mime_types"
	// DefaultCompressionExtensions and DefaultCompressionMimeTypes are
	// the defaults of compression.extensions and compression.mime_types.
	DefaultCompressionExtensions = ".txt,.log,.csv,.json,.tar,.xml,.bin"
	DefaultCompressionMimeTypes  = "text/*,application/json,application/xml,binary/octet-stream"

	// DefaultRequestsDeadline is the default of api.requests_deadline.
	DefaultRequestsDeadline = 10 * time.Second
)

// Applied reports whether the server applies a key of the dynamic
// configuration; the other keys of the schema are stored and validated
// but have no effect yet. The scanner reads scanner.speed and
// scanner.cycle at the start of every cycle (internal/service/scanner),
// the event dispatcher the notify_* targets (internal/service/event).
func Applied(subsystem, key string) bool {
	switch subsystem {
	case SubsystemAPI:
		return key == KeyRootAccess || key == KeyCORSAllowOrigin || key == KeyRequestsMax || key == KeyRequestsDeadline
	case SubsystemRateLimit:
		// The request limits, read by the rate limiter at every request.
		return true
	case "scanner":
		return key == "speed" || key == "cycle"
	case "access":
		// The block public access, read by the access rules
		// (internal/service/accessrule) at every evaluation.
		return key == "block_public" || key == "block_public_mode"
	case SubsystemStorage:
		// The metadata log of the local drives (internal/app/metalog.go).
		return key == KeyMetadataLogSegmentMiB || key == KeyMetadataLogMemoryPercent
	case SubsystemCompression:
		// Read by the object layer at every write.
		return key == KeyCompressionEnable || key == KeyExtensions || key == KeyMimeTypes
	case "logger":
		// The level and the format of the server logs, applied at once by
		// the log stream (internal/service/logs).
		return key == "level" || key == "format"
	}
	// The notification targets (notify_<kind>[:<id>]) are applied by the
	// event dispatcher, which reloads them when they change; the audit
	// targets (audit_webhook, audit_kafka) by the audit log
	// (internal/service/audit).
	return (strings.HasPrefix(subsystem, "notify_") || strings.HasPrefix(subsystem, "audit_")) && key != "comment"
}

// Option configures the settings service.
type Option func(*service)

type service struct {
	deps Deps
}

var _ Service = (*service)(nil)

// New returns the settings service. A missing configuration service is a
// wiring error (domain.ErrInvalidArgument).
func New(deps Deps, opts ...Option) (Service, error) {
	if deps.Config == nil {
		return nil, fmt.Errorf("%w: settings require the config service", domain.ErrInvalidArgument)
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	s := &service{deps: deps}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// API implements Service.
func (s *service) API(ctx context.Context) (API, error) {
	kv, err := s.deps.Config.Get(ctx, SubsystemAPI)
	if err != nil {
		return API{}, fmt.Errorf("settings: %s: %w", SubsystemAPI, err)
	}
	return API{
		RootAccess:      s.flag(ctx, SubsystemAPI, KeyRootAccess, kv[KeyRootAccess], true),
		CORSAllowOrigin: list(kv[KeyCORSAllowOrigin]),
	}, nil
}

// Compression implements Service.
func (s *service) Compression(ctx context.Context) (Compression, error) {
	kv, err := s.deps.Config.Get(ctx, SubsystemCompression)
	if err != nil {
		return Compression{}, fmt.Errorf("settings: %s: %w", SubsystemCompression, err)
	}
	ext, mime := kv[KeyExtensions], kv[KeyMimeTypes]
	if _, ok := kv[KeyExtensions]; !ok {
		ext = DefaultCompressionExtensions
	}
	if _, ok := kv[KeyMimeTypes]; !ok {
		mime = DefaultCompressionMimeTypes
	}
	return Compression{
		Enabled:    s.flag(ctx, SubsystemCompression, KeyCompressionEnable, kv[KeyCompressionEnable], false),
		Extensions: list(ext),
		MimeTypes:  list(mime),
	}, nil
}

// Limits implements Service.
func (s *service) Limits(ctx context.Context) (Limits, error) {
	api, err := s.deps.Config.Get(ctx, SubsystemAPI)
	if err != nil {
		return Limits{}, fmt.Errorf("settings: %s: %w", SubsystemAPI, err)
	}
	rl, err := s.deps.Config.Get(ctx, SubsystemRateLimit)
	if err != nil {
		return Limits{}, fmt.Errorf("settings: %s: %w", SubsystemRateLimit, err)
	}
	rate := func(key string) Rate {
		r := Rate{PerSecond: s.count(ctx, SubsystemRateLimit, key, rl[key])}
		if r.Burst = s.count(ctx, SubsystemRateLimit, key+BurstSuffix, rl[key+BurstSuffix]); r.Burst == 0 {
			r.Burst = r.PerSecond
		}
		return r
	}
	return Limits{
		RequestsMax:      s.count(ctx, SubsystemAPI, KeyRequestsMax, api[KeyRequestsMax]),
		RequestsDeadline: s.duration(ctx, SubsystemAPI, KeyRequestsDeadline, api[KeyRequestsDeadline], DefaultRequestsDeadline),
		S3IP:             rate(KeyS3IP),
		S3AccessKey:      rate(KeyS3AccessKey),
		S3Bucket:         rate(KeyS3Bucket),
		AdminIP:          rate(KeyAdminIP),
		AdminUser:        rate(KeyAdminUser),
	}, nil
}

// count parses a non-negative integer setting; empty is 0, anything else
// is 0 and logged.
func (s *service) count(ctx context.Context, subsystem, key, value string) int {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 0 {
		s.deps.Logger.WarnContext(ctx, "settings: invalid count, using no limit", "subsystem", subsystem, "key", key, "value", value)
		return 0
	}
	return n
}

// duration parses a non-negative duration setting; empty is the default,
// anything else keeps the default and is logged.
func (s *service) duration(ctx context.Context, subsystem, key, value string, def time.Duration) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return def
	}
	d, err := time.ParseDuration(value)
	if err != nil || d < 0 {
		s.deps.Logger.WarnContext(ctx, "settings: invalid duration, using the default", "subsystem", subsystem, "key", key, "value", value, "default", def)
		return def
	}
	return d
}

// list parses a comma-separated setting; nil when empty.
func list(value string) []string {
	var out []string
	for _, v := range strings.Split(value, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// flag parses a boolean setting ("on", "off", "true", "false"); empty is
// the default, anything else keeps the default and is logged.
func (s *service) flag(ctx context.Context, subsystem, key, value string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "":
		return def
	case "on", "true":
		return true
	case "off", "false":
		return false
	}
	s.deps.Logger.WarnContext(ctx, "settings: invalid boolean, using the default", "subsystem", subsystem, "key", key, "value", value, "default", def)
	return def
}
