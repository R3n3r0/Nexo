package cluster

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
)

// Defaults of the notifier.
const (
	defaultNotifyTimeout = 5 * time.Second
	defaultRetryInterval = 2 * time.Second
	defaultMaxPending    = 256
	// DefaultResyncInterval is the period of the full reload of the
	// cached state (docs/03 §3.9: the safety net of the invalidations).
	DefaultResyncInterval = 2 * time.Minute
	// stallFactor: a retry tick that comes more than stallFactor periods
	// after the previous one means this node was frozen meanwhile.
	stallFactor = 3
)

// NotifierDeps are the dependencies of the notifier.
type NotifierDeps struct {
	// NodeID identifies this node as the origin of its events.
	NodeID string
	// Peers are the other nodes of the deployment (none on a single node).
	Peers  []Peer
	Clock  clock.Clock
	Logger *slog.Logger
}

// NotifierOption configures the notifier.
type NotifierOption func(*notifierOptions)

type notifierOptions struct {
	timeout    time.Duration
	retry      time.Duration
	maxPending int
	resync     time.Duration
}

// WithResyncInterval sets how often the node reloads the state it caches
// even without invalidation (default 2m; 0 disables the periodic reload,
// not the resyncs after a peer or this node was away).
func WithResyncInterval(d time.Duration) NotifierOption {
	return func(o *notifierOptions) {
		if d >= 0 {
			o.resync = d
		}
	}
}

// WithNotifyTimeout bounds the delivery to one peer (default 5s).
func WithNotifyTimeout(d time.Duration) NotifierOption {
	return func(o *notifierOptions) {
		if d > 0 {
			o.timeout = d
		}
	}
}

// WithRetryInterval sets how often the failed deliveries are retried
// (default 2s).
func WithRetryInterval(d time.Duration) NotifierOption {
	return func(o *notifierOptions) {
		if d > 0 {
			o.retry = d
		}
	}
}

// WithMaxPending bounds the events kept for an unreachable peer; beyond
// it they collapse into one EventResync (default 256).
func WithMaxPending(n int) NotifierOption {
	return func(o *notifierOptions) {
		if n > 0 {
			o.maxPending = n
		}
	}
}

type subscription struct {
	kind EventKind
	h    Handler
}

// notifier implements Notifier.
type notifier struct {
	deps NotifierDeps
	opts notifierOptions

	mu   sync.RWMutex
	subs map[*subscription]struct{}

	pmu sync.Mutex
	// pending holds, per peer, the events whose delivery failed.
	pending map[string][]Event

	lifeMu sync.Mutex
	stop   chan struct{}
	done   chan struct{}
}

var _ Notifier = (*notifier)(nil)

// NewNotifier returns the notifier of a node of a distributed deployment.
func NewNotifier(deps NotifierDeps, opts ...NotifierOption) Notifier {
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.NodeID == "" {
		deps.NodeID = LocalNodeID
	}
	o := notifierOptions{timeout: defaultNotifyTimeout, retry: defaultRetryInterval, maxPending: defaultMaxPending, resync: DefaultResyncInterval}
	for _, opt := range opts {
		opt(&o)
	}
	return &notifier{deps: deps, opts: o, subs: make(map[*subscription]struct{}), pending: make(map[string][]Event)}
}

// NewLocalNotifier returns the notifier of a single-node deployment:
// Broadcast has nobody to notify, Deliver dispatches to the subscribers
// (the same contract as the distributed notifier without peers).
func NewLocalNotifier(deps NotifierDeps, opts ...NotifierOption) Notifier {
	deps.Peers = nil
	return NewNotifier(deps, opts...)
}

// Subscribe implements Notifier.
func (n *notifier) Subscribe(kind EventKind, h Handler) func() {
	s := &subscription{kind: kind, h: h}
	n.mu.Lock()
	n.subs[s] = struct{}{}
	n.mu.Unlock()
	return func() {
		n.mu.Lock()
		delete(n.subs, s)
		n.mu.Unlock()
	}
}

// Deliver implements Notifier.
func (n *notifier) Deliver(ctx context.Context, e Event) error {
	if e.Kind == "" {
		return fmt.Errorf("%w: cluster: event without kind", domain.ErrInvalidArgument)
	}
	n.mu.RLock()
	var targets []*subscription
	for s := range n.subs {
		if s.kind == e.Kind || e.Kind == EventResync {
			targets = append(targets, s)
		}
	}
	n.mu.RUnlock()
	var errs []error
	for _, s := range targets {
		if err := s.h.HandleEvent(ctx, e); err != nil {
			errs = append(errs, fmt.Errorf("cluster: handle %s event %q: %w", s.kind, e.Name, err))
		}
	}
	if len(errs) > 0 {
		n.deps.Logger.LogAttrs(ctx, slog.LevelWarn, "cluster: invalidation not applied",
			slog.String("kind", string(e.Kind)), slog.String("name", e.Name), slog.String("origin", e.Origin), slog.Any("err", errors.Join(errs...)))
	}
	return errors.Join(errs...)
}

// Broadcast implements Notifier.
func (n *notifier) Broadcast(ctx context.Context, e Event) error {
	if e.Kind == "" {
		return fmt.Errorf("%w: cluster: event without kind", domain.ErrInvalidArgument)
	}
	e.Origin = n.deps.NodeID
	if e.Time.IsZero() {
		e.Time = n.deps.Clock.Now()
	}
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for _, p := range n.deps.Peers {
		if !p.IsOnline() {
			n.enqueue(p.NodeID(), e)
			mu.Lock()
			errs = append(errs, fmt.Errorf("peer %s: %w (delivery queued)", p.NodeID(), domain.ErrNodeOffline))
			mu.Unlock()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			callCtx, cancel := context.WithTimeout(ctx, n.opts.timeout)
			defer cancel()
			if err := p.Notify(callCtx, e); err != nil {
				n.enqueue(p.NodeID(), e)
				mu.Lock()
				errs = append(errs, fmt.Errorf("peer %s: %w (delivery queued)", p.NodeID(), err))
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		n.deps.Logger.LogAttrs(ctx, slog.LevelWarn, "cluster: invalidation not delivered to every peer, retrying in background",
			slog.String("kind", string(e.Kind)), slog.String("name", e.Name), slog.Any("err", err))
		return err
	}
	return nil
}

// enqueue keeps a failed delivery for the retry loop, deduplicated; past
// the bound the queue collapses into one resync.
func (n *notifier) enqueue(peer string, e Event) {
	n.pmu.Lock()
	defer n.pmu.Unlock()
	queue := n.pending[peer]
	for _, q := range queue {
		if q.Kind == EventResync || (q.Kind == e.Kind && q.Name == e.Name) {
			return
		}
	}
	if len(queue) >= n.opts.maxPending {
		n.pending[peer] = []Event{{Kind: EventResync, Origin: e.Origin, Time: e.Time}}
		return
	}
	n.pending[peer] = append(queue, e)
}

// Start implements lifecycle.Runner: it starts the retry of the failed
// deliveries.
func (n *notifier) Start(context.Context) error {
	n.lifeMu.Lock()
	defer n.lifeMu.Unlock()
	if n.stop != nil || len(n.deps.Peers) == 0 {
		return nil
	}
	n.stop, n.done = make(chan struct{}), make(chan struct{})
	go n.retryLoop(n.stop, n.done)
	return nil
}

// Stop implements lifecycle.Runner.
func (n *notifier) Stop(ctx context.Context) error {
	n.lifeMu.Lock()
	stop, done := n.stop, n.done
	n.stop, n.done = nil, nil
	n.lifeMu.Unlock()
	if stop == nil {
		return nil
	}
	close(stop)
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// retryLoop delivers the pending events of the peers back online and keeps
// the cached state of this node fresh, since an invalidation can be lost
// (the node that changed the state crashed with it queued): it reloads
// everything (a local EventResync) periodically, when this node notices it
// was frozen (a tick far behind the previous one: suspended process, long
// GC or VM pause), and asks a peer that comes back online to do the same.
func (n *notifier) retryLoop(stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(n.opts.retry)
	defer ticker.Stop()
	online := make(map[string]bool, len(n.deps.Peers))
	for _, p := range n.deps.Peers {
		online[p.NodeID()] = p.IsOnline()
	}
	// The monotonic clock tells a frozen process (wall clock jumps do not
	// count).
	last := time.Now()
	lastResync := last
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		now := time.Now()
		gap := now.Sub(last)
		last = now
		switch {
		case gap > stallFactor*n.opts.retry:
			n.resync(fmt.Sprintf("this node was frozen for %s", gap.Round(time.Second)))
			lastResync = now
		case n.opts.resync > 0 && now.Sub(lastResync) >= n.opts.resync:
			n.resync("periodic")
			lastResync = now
		}
		for _, p := range n.deps.Peers {
			up := p.IsOnline()
			if up && !online[p.NodeID()] {
				// Back online: whatever it missed, possibly from a node
				// that is gone since, is reloaded.
				n.enqueue(p.NodeID(), Event{Kind: EventResync, Name: "peer back online", Origin: n.deps.NodeID, Time: n.deps.Clock.Now()})
			}
			online[p.NodeID()] = up
			if up {
				n.flush(p, stop)
			}
		}
	}
}

// resync reloads the cached state of this node (local EventResync).
func (n *notifier) resync(why string) {
	ctx, cancel := context.WithTimeout(context.Background(), n.opts.timeout)
	defer cancel()
	n.deps.Logger.LogAttrs(ctx, slog.LevelDebug, "cluster: reloading the cached state", slog.String("why", why))
	_ = n.Deliver(ctx, Event{Kind: EventResync, Name: why, Origin: n.deps.NodeID, Time: n.deps.Clock.Now()})
}

// flush delivers the pending events of a peer, in order, until one fails.
func (n *notifier) flush(p Peer, stop <-chan struct{}) {
	n.pmu.Lock()
	queue := n.pending[p.NodeID()]
	delete(n.pending, p.NodeID())
	n.pmu.Unlock()
	for i, e := range queue {
		ctx, cancel := context.WithTimeout(context.Background(), n.opts.timeout)
		err := p.Notify(ctx, e)
		cancel()
		if err != nil {
			for _, rest := range queue[i:] {
				n.enqueue(p.NodeID(), rest)
			}
			return
		}
		select {
		case <-stop:
			for _, rest := range queue[i+1:] {
				n.enqueue(p.NodeID(), rest)
			}
			return
		default:
		}
	}
	if len(queue) > 0 {
		n.deps.Logger.Info("cluster: missed invalidations delivered", "peer", p.NodeID(), "events", len(queue))
	}
}
