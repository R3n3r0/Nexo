package trace

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/metrics"
)

// Deps are the dependencies of the live trace.
type Deps struct {
	// NodeID names this node in the events ("host:port", "local" on a
	// single node).
	NodeID string
	// Peers are the other nodes of a cluster.
	Peers []Peer
	// Registry, when set, receives nexo_trace_subscribers and
	// nexo_trace_dropped_total.
	Registry metrics.Registry
	Logger   *slog.Logger
}

// Option configures the live trace.
type Option func(*options)

type options struct {
	buffer   int
	retryMin time.Duration
	retryMax time.Duration
}

// WithBuffer sets the events a subscriber may lag behind before it loses
// them (default 1024).
func WithBuffer(n int) Option {
	return func(o *options) {
		if n > 0 {
			o.buffer = n
		}
	}
}

// WithReconnect sets the delays before a broken stream of a peer is
// opened again: min doubles up to max (default 1s to 30s).
func WithReconnect(minDelay, maxDelay time.Duration) Option {
	return func(o *options) {
		if minDelay > 0 {
			o.retryMin = minDelay
		}
		if maxDelay >= o.retryMin {
			o.retryMax = maxDelay
		}
	}
}

type service struct {
	deps Deps
	opts options

	mu    sync.RWMutex
	subs  map[*subscriber]struct{}
	count atomic.Int64

	subscribers metrics.Gauge
	dropped     metrics.Counter
}

var _ Service = (*service)(nil)

// New returns the live trace of the node.
func New(deps Deps, opts ...Option) (Service, error) {
	if deps.NodeID == "" {
		deps.NodeID = "local"
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	o := options{buffer: 1024, retryMin: time.Second, retryMax: 30 * time.Second}
	for _, opt := range opts {
		opt(&o)
	}
	s := &service{deps: deps, opts: o, subs: map[*subscriber]struct{}{}}
	if deps.Registry != nil {
		ctx := context.Background()
		var err error
		if s.subscribers, err = deps.Registry.Gauge(ctx, metrics.Definition{Subsystem: "trace", Name: "subscribers",
			Help: "Live trace subscriptions on this node (the console, the CLI and the peers of their nodes)."}); err != nil {
			return nil, fmt.Errorf("trace: %w", err)
		}
		if s.dropped, err = deps.Registry.Counter(ctx, metrics.Definition{Subsystem: "trace", Name: "dropped_total",
			Help: "Trace events lost by subscribers that did not keep up."}); err != nil {
			return nil, fmt.Errorf("trace: %w", err)
		}
	}
	return s, nil
}

// Enabled implements Service.
func (s *service) Enabled() bool { return s.count.Load() > 0 }

// Publish implements Service.
func (s *service) Publish(ctx context.Context, e Event) {
	if !s.Enabled() {
		return
	}
	if e.Node == "" {
		e.Node = s.deps.NodeID
	}
	e.Query = redactQuery(e.Query)
	var lost float64
	s.mu.RLock()
	for sub := range s.subs {
		if sub.filter.match(e) && !sub.send(e) {
			lost++
		}
	}
	s.mu.RUnlock()
	if lost > 0 && s.dropped != nil {
		s.dropped.Add(ctx, lost)
	}
}

// Subscribe implements Service.
func (s *service) Subscribe(ctx context.Context, f Filter) (Subscription, error) {
	cf, err := compile(f)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sub := &subscriber{filter: cf, ch: make(chan Event, s.opts.buffer)}
	s.mu.Lock()
	s.subs[sub] = struct{}{}
	s.mu.Unlock()
	s.count.Add(1)
	if s.subscribers != nil {
		s.subscribers.Add(ctx, 1)
	}
	var wg sync.WaitGroup
	if !f.Local {
		for _, p := range s.deps.Peers {
			if len(f.Nodes) > 0 && !slices.Contains(f.Nodes, p.NodeID()) {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				s.follow(ctx, p, f, sub)
			}()
		}
	}
	context.AfterFunc(ctx, func() {
		s.mu.Lock()
		delete(s.subs, sub)
		s.mu.Unlock()
		s.count.Add(-1)
		if s.subscribers != nil {
			s.subscribers.Add(context.WithoutCancel(ctx), -1)
		}
		wg.Wait()
		sub.close()
	})
	return sub, nil
}

// follow relays the events of a peer to sub until ctx ends, opening the
// stream again when it breaks.
func (s *service) follow(ctx context.Context, p Peer, f Filter, sub *subscriber) {
	remote := f
	remote.Local, remote.Nodes = true, nil
	delay := s.opts.retryMin
	for {
		err := p.Trace(ctx, remote, func(d Delivery) error {
			delay = s.opts.retryMin
			if d.Event != nil {
				e := *d.Event
				e.Query = redactQuery(e.Query)
				if !sub.filter.match(e) {
					return nil
				}
				if !sub.send(e) && s.dropped != nil {
					s.dropped.Add(ctx, 1)
				}
			}
			if d.Dropped > 0 {
				sub.dropped.Add(d.Dropped)
			}
			return nil
		})
		if ctx.Err() != nil {
			return
		}
		s.deps.Logger.DebugContext(ctx, "trace: stream of a peer broken, reconnecting", "peer", p.NodeID(), "err", err, "delay", delay)
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		delay = min(2*delay, s.opts.retryMax)
	}
}

// subscriber is a subscription: a bounded buffer, closed once.
type subscriber struct {
	filter  filter
	ch      chan Event
	dropped atomic.Uint64

	mu     sync.Mutex
	closed bool
}

var _ Subscription = (*subscriber)(nil)

// Events implements Subscription.
func (s *subscriber) Events() <-chan Event { return s.ch }

// Dropped implements Subscription.
func (s *subscriber) Dropped() uint64 { return s.dropped.Load() }

// send queues e without blocking; false when it was dropped.
func (s *subscriber) send(e Event) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return true
	}
	select {
	case s.ch <- e:
		return true
	default:
		s.dropped.Add(1)
		return false
	}
}

func (s *subscriber) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.ch)
	}
}

// filter is a compiled Filter.
type filter struct {
	nodes       map[string]bool
	types       map[Type]bool
	apis        map[string]bool
	buckets     map[string]bool
	codes       map[int]bool
	classes     map[int]bool
	errorsOnly  bool
	minDuration time.Duration
}

// compile validates f.
func compile(f Filter) (filter, error) {
	out := filter{errorsOnly: f.ErrorsOnly, minDuration: f.MinDuration}
	if f.MinDuration < 0 {
		return out, fmt.Errorf("%w: trace: negative duration threshold", domain.ErrInvalidArgument)
	}
	set := func(values []string, fold bool) map[string]bool {
		if len(values) == 0 {
			return nil
		}
		m := make(map[string]bool, len(values))
		for _, v := range values {
			if fold {
				v = strings.ToLower(v)
			}
			m[v] = true
		}
		return m
	}
	out.nodes, out.apis, out.buckets = set(f.Nodes, false), set(f.APIs, true), set(f.Buckets, false)
	for _, t := range f.Types {
		switch t {
		case TypeS3, TypeAdmin, TypeWeb, TypeRPC:
		default:
			return out, fmt.Errorf("%w: trace: unknown request type %q", domain.ErrInvalidArgument, t)
		}
		if out.types == nil {
			out.types = map[Type]bool{}
		}
		out.types[t] = true
	}
	for _, c := range f.StatusCodes {
		if c < 100 || c > 599 {
			return out, fmt.Errorf("%w: trace: invalid status %d", domain.ErrInvalidArgument, c)
		}
		if out.codes == nil {
			out.codes = map[int]bool{}
		}
		out.codes[c] = true
	}
	for _, c := range f.StatusClasses {
		if c < 1 || c > 5 {
			return out, fmt.Errorf("%w: trace: invalid status class %dxx", domain.ErrInvalidArgument, c)
		}
		if out.classes == nil {
			out.classes = map[int]bool{}
		}
		out.classes[c] = true
	}
	return out, nil
}

// match reports whether e passes the filter.
func (f filter) match(e Event) bool {
	if f.nodes != nil && !f.nodes[e.Node] {
		return false
	}
	if f.types == nil {
		if e.Type == TypeRPC {
			return false
		}
	} else if !f.types[e.Type] {
		return false
	}
	if f.apis != nil && !f.apis[strings.ToLower(e.API)] {
		return false
	}
	if f.buckets != nil && !f.buckets[e.Bucket] {
		return false
	}
	if f.codes != nil || f.classes != nil {
		if !f.codes[e.Status] && !f.classes[e.Status/100] {
			return false
		}
	}
	if f.errorsOnly && e.Status < 400 && e.Error == "" {
		return false
	}
	return e.Duration >= f.minDuration
}
