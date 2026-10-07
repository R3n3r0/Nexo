// Package lifecycle defines the start/stop contract of long-running
// components and a Group that manages them in order.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Runner is a component with a start and a stop phase. Start must return
// once the component is running (listening, scheduled, ...), not when it
// terminates. Stop must be idempotent and honour ctx for its deadline.
type Runner interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

// Funcs adapts two functions to a Runner. Either may be nil.
type Funcs struct {
	OnStart func(ctx context.Context) error
	OnStop  func(ctx context.Context) error
}

// Start implements Runner.
func (f Funcs) Start(ctx context.Context) error {
	if f.OnStart == nil {
		return nil
	}
	return f.OnStart(ctx)
}

// Stop implements Runner.
func (f Funcs) Stop(ctx context.Context) error {
	if f.OnStop == nil {
		return nil
	}
	return f.OnStop(ctx)
}

// Named gives a Runner a name for logs and error messages.
type Named struct {
	Name string
	Runner
}

// Option configures a Group.
type Option func(*Group)

// WithStopTimeout bounds the Stop call of each runner. Zero disables the
// per-runner timeout (the parent context still applies).
func WithStopTimeout(d time.Duration) Option {
	return func(g *Group) { g.stopTimeout = d }
}

// WithLogger sets the logger used to report start/stop progress.
func WithLogger(l *slog.Logger) Option {
	return func(g *Group) {
		if l != nil {
			g.logger = l
		}
	}
}

// Group starts runners in the order they were added and stops them in
// reverse order. If a runner fails to start, the ones already started are
// stopped before the error is returned.
type Group struct {
	mu          sync.Mutex
	runners     []Named
	started     []Named
	stopTimeout time.Duration
	logger      *slog.Logger
}

// NewGroup creates an empty group.
func NewGroup(opts ...Option) *Group {
	g := &Group{stopTimeout: 10 * time.Second, logger: slog.Default()}
	for _, o := range opts {
		o(g)
	}
	return g
}

// Add appends a runner. Name is used for diagnostics only.
func (g *Group) Add(name string, r Runner) *Group {
	if r == nil {
		return g
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.runners = append(g.runners, Named{Name: name, Runner: r})
	return g
}

// Len returns the number of runners in the group.
func (g *Group) Len() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.runners)
}

// Start starts every runner in order. On failure it stops the runners that
// were started (reverse order) and returns the start error, joined with any
// stop errors.
func (g *Group) Start(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.started) > 0 {
		return errors.New("lifecycle: group already started")
	}
	for _, r := range g.runners {
		if err := ctx.Err(); err != nil {
			stopErr := g.stopLocked(context.WithoutCancel(ctx))
			return errors.Join(fmt.Errorf("lifecycle: start %s: %w", r.Name, err), stopErr)
		}
		g.logger.Debug("starting", "runner", r.Name)
		if err := r.Start(ctx); err != nil {
			stopErr := g.stopLocked(context.WithoutCancel(ctx))
			return errors.Join(fmt.Errorf("lifecycle: start %s: %w", r.Name, err), stopErr)
		}
		g.started = append(g.started, r)
	}
	return nil
}

// Stop stops the started runners in reverse order, bounding each Stop by
// the configured timeout. All errors are collected and joined.
func (g *Group) Stop(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.stopLocked(ctx)
}

func (g *Group) stopLocked(ctx context.Context) error {
	var errs []error
	for i := len(g.started) - 1; i >= 0; i-- {
		r := g.started[i]
		g.logger.Debug("stopping", "runner", r.Name)
		if err := g.stopOne(ctx, r); err != nil {
			errs = append(errs, fmt.Errorf("lifecycle: stop %s: %w", r.Name, err))
		}
	}
	g.started = nil
	return errors.Join(errs...)
}

func (g *Group) stopOne(ctx context.Context, r Named) error {
	if g.stopTimeout <= 0 {
		return r.Stop(ctx)
	}
	stopCtx, cancel := context.WithTimeout(ctx, g.stopTimeout)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Stop(stopCtx) }()
	select {
	case err := <-done:
		return err
	case <-stopCtx.Done():
		return fmt.Errorf("timed out after %s: %w", g.stopTimeout, stopCtx.Err())
	}
}

var _ Runner = (*Group)(nil)
