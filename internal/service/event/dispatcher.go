package event

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
)

// Deps are the dependencies of the dispatcher.
type Deps struct {
	// Factory builds targets from configurations (default: webhook factory).
	Factory TargetFactory
	Logger  *slog.Logger
}

// Option configures the dispatcher.
type Option func(*dispatcher)

// WithQueueSize sets the in-memory queue capacity (default 10000).
func WithQueueSize(n int) Option {
	return func(d *dispatcher) {
		if n > 0 {
			d.queueSize = n
		}
	}
}

// WithSendTimeout bounds each delivery attempt (default 10s).
func WithSendTimeout(t time.Duration) Option {
	return func(d *dispatcher) {
		if t > 0 {
			d.sendTimeout = t
		}
	}
}

type targetEntry struct {
	cfg    TargetConfig
	target Target
}

// dispatcher is an in-memory event dispatcher with one delivery goroutine.
type dispatcher struct {
	deps        Deps
	queueSize   int
	sendTimeout time.Duration

	mu      sync.RWMutex
	targets map[string]*targetEntry
	queue   chan Event
	closed  bool
	started bool
	done    chan struct{}
}

var _ Dispatcher = (*dispatcher)(nil)

// NewDispatcher returns an in-memory dispatcher. Call Start before publishing.
func NewDispatcher(deps Deps, opts ...Option) Dispatcher {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Factory == nil {
		deps.Factory = NewWebhookFactory(nil)
	}
	d := &dispatcher{deps: deps, queueSize: 10000, sendTimeout: 10 * time.Second, targets: make(map[string]*targetEntry)}
	for _, o := range opts {
		o(d)
	}
	return d
}

// Start implements lifecycle.Runner.
func (d *dispatcher) Start(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.started {
		return errors.New("event: dispatcher already started")
	}
	d.queue = make(chan Event, d.queueSize)
	d.done = make(chan struct{})
	d.started = true
	d.closed = false
	go d.run(d.queue, d.done)
	return nil
}

// Stop implements lifecycle.Runner: it stops accepting events, drains the
// queue (bounded by ctx) and closes the targets.
func (d *dispatcher) Stop(ctx context.Context) error {
	d.mu.Lock()
	if !d.started || d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	close(d.queue)
	done := d.done
	d.mu.Unlock()

	var err error
	select {
	case <-done:
	case <-ctx.Done():
		err = fmt.Errorf("event: drain queue: %w", ctx.Err())
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	for id, t := range d.targets {
		if cerr := t.target.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("event: close target %s: %w", id, cerr))
		}
	}
	d.started = false
	return err
}

// Publish implements Service.
func (d *dispatcher) Publish(_ context.Context, e Event) error {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if !d.started || d.closed {
		return fmt.Errorf("event: dispatcher not running: %w", domain.ErrConflict)
	}
	select {
	case d.queue <- e:
		return nil
	default:
		d.deps.Logger.Warn("event: queue full, event dropped", "event", e.EventName, "bucket", e.S3.Bucket.Name, "key", e.S3.Object.Key)
		return fmt.Errorf("event: queue full: %w", domain.ErrStorageFull)
	}
}

// SetTarget implements Service.
func (d *dispatcher) SetTarget(_ context.Context, cfg TargetConfig) error {
	if cfg.ID == "" {
		return fmt.Errorf("%w: target id is required", domain.ErrInvalidArgument)
	}
	t, err := d.deps.Factory.New(cfg)
	if err != nil {
		return fmt.Errorf("event: build target %s: %w", cfg.ID, err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if old, ok := d.targets[cfg.ID]; ok {
		if cerr := old.target.Close(); cerr != nil {
			d.deps.Logger.Warn("event: close replaced target", "id", cfg.ID, "err", cerr)
		}
	}
	d.targets[cfg.ID] = &targetEntry{cfg: cfg, target: t}
	return nil
}

// GetTarget implements Service.
func (d *dispatcher) GetTarget(_ context.Context, id string) (TargetConfig, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	t, ok := d.targets[id]
	if !ok {
		return TargetConfig{}, fmt.Errorf("%w: event target %q", domain.ErrPolicyNotFound, id)
	}
	return t.cfg, nil
}

// ListTargets implements Service.
func (d *dispatcher) ListTargets(context.Context) ([]TargetConfig, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]TargetConfig, 0, len(d.targets))
	for _, t := range d.targets {
		out = append(out, t.cfg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// RemoveTarget implements Service.
func (d *dispatcher) RemoveTarget(_ context.Context, id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	t, ok := d.targets[id]
	if !ok {
		return fmt.Errorf("%w: event target %q", domain.ErrPolicyNotFound, id)
	}
	delete(d.targets, id)
	if err := t.target.Close(); err != nil {
		return fmt.Errorf("event: close target %s: %w", id, err)
	}
	return nil
}

func (d *dispatcher) run(queue <-chan Event, done chan<- struct{}) {
	defer close(done)
	for e := range queue {
		d.deliver(e)
	}
}

func (d *dispatcher) deliver(e Event) {
	d.mu.RLock()
	matching := make([]*targetEntry, 0, len(d.targets))
	for _, t := range d.targets {
		if t.cfg.Enabled && t.cfg.Filter.Matches(e) {
			matching = append(matching, t)
		}
	}
	d.mu.RUnlock()
	for _, t := range matching {
		ctx, cancel := context.WithTimeout(context.Background(), d.sendTimeout)
		if err := t.target.Send(ctx, e); err != nil {
			d.deps.Logger.Error("event: delivery failed", "target", t.cfg.ID, "event", e.EventName, "err", err)
		}
		cancel()
	}
}
