package logs

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/config"
)

// SubsystemLogger is the configuration subsystem of the server logs.
const SubsystemLogger = "logger"

// Keys of the logger subsystem.
const (
	KeyLevel  = "level"
	KeyFormat = "format"
)

// ConfiguratorDeps are the dependencies of the configurator.
type ConfiguratorDeps struct {
	// Config is the dynamic configuration (required).
	Config config.Service
	// Logs is the log stream to configure (required).
	Logs   Service
	Logger *slog.Logger
}

type configurator struct {
	deps ConfiguratorDeps

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

var _ Configurator = (*configurator)(nil)

// NewConfigurator returns the runner applying the logger subsystem. A
// missing dependency is a wiring error (domain.ErrInvalidArgument).
func NewConfigurator(deps ConfiguratorDeps) (Configurator, error) {
	if deps.Config == nil || deps.Logs == nil {
		return nil, fmt.Errorf("%w: logs: the configurator needs the configuration and the log stream", domain.ErrInvalidArgument)
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &configurator{deps: deps}, nil
}

// Start implements lifecycle.Runner: the settings are applied now, then
// at every change of the subsystem.
func (c *configurator) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel != nil {
		return nil
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	changes := c.deps.Config.Watch(runCtx)
	c.apply(ctx)
	c.cancel, c.done = cancel, make(chan struct{})
	go func(done chan<- struct{}) {
		defer close(done)
		for ch := range changes {
			if ch.Subsystem == "" || ch.Subsystem == SubsystemLogger {
				c.apply(runCtx)
			}
		}
	}(c.done)
	return nil
}

// Stop implements lifecycle.Runner.
func (c *configurator) Stop(ctx context.Context) error {
	c.mu.Lock()
	cancel, done := c.cancel, c.done
	c.cancel, c.done = nil, nil
	c.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// apply reads the subsystem and configures the log stream; an invalid
// value (written around the validation of the Admin API) is logged and
// the previous settings stay.
func (c *configurator) apply(ctx context.Context) {
	kv, err := c.deps.Config.Get(ctx, SubsystemLogger)
	if err != nil {
		c.deps.Logger.WarnContext(ctx, "logs: cannot read the logger configuration", "err", err)
		return
	}
	if err := c.deps.Logs.Configure(ctx, Settings{Level: kv[KeyLevel], Format: kv[KeyFormat]}); err != nil {
		c.deps.Logger.WarnContext(ctx, "logs: invalid logger configuration ignored", "err", err)
	}
}
