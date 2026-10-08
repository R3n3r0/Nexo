package settings

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
)

// Subsystems and keys of the dynamic configuration applied by the server.
const (
	SubsystemAPI  = "api"
	KeyRootAccess = "root_access"
)

// Applied reports whether the server applies a key of the dynamic
// configuration; the other keys of the schema are stored and validated
// but have no effect yet. The scanner reads scanner.speed and
// scanner.cycle at the start of every cycle (internal/service/scanner).
func Applied(subsystem, key string) bool {
	switch subsystem {
	case SubsystemAPI:
		return key == KeyRootAccess
	case "scanner":
		return key == "speed" || key == "cycle"
	}
	return false
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
	return API{RootAccess: s.flag(ctx, SubsystemAPI, KeyRootAccess, kv[KeyRootAccess], true)}, nil
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
