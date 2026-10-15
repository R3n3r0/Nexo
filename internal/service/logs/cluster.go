package logs

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
)

// ClusterDeps are the dependencies of the cluster view of the log stream.
type ClusterDeps struct {
	// Local is the log stream of this node (required).
	Local Service
	// Peers are the other nodes of the cluster.
	Peers []Peer
	// Logger reports the broken streams of the peers.
	Logger *slog.Logger
}

type clusterService struct {
	deps ClusterDeps
	opts options
	red  *redactor
}

var _ Service = (*clusterService)(nil)

// NewCluster returns the log stream of the cluster: the subscriptions
// (without Filter.Local) receive the entries of this node and of the
// peers, whose streams are opened again (without replay) when they break;
// Handler and Configure are the ones of the local stream. The options
// WithBuffer and WithReconnect apply.
func NewCluster(deps ClusterDeps, opts ...Option) (Service, error) {
	if deps.Local == nil {
		return nil, fmt.Errorf("%w: logs: the cluster view needs the local stream", domain.ErrInvalidArgument)
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	o := options{buffer: 1024, retryMin: time.Second, retryMax: 30 * time.Second}
	for _, opt := range opts {
		opt(&o)
	}
	return &clusterService{deps: deps, opts: o, red: newRedactor()}, nil
}

// Handler implements Service.
func (c *clusterService) Handler() slog.Handler { return c.deps.Local.Handler() }

// Configure implements Service.
func (c *clusterService) Configure(ctx context.Context, s Settings) error {
	return c.deps.Local.Configure(ctx, s)
}

// Subscribe implements Service.
func (c *clusterService) Subscribe(ctx context.Context, f Filter) (Subscription, error) {
	if f.Local || len(c.deps.Peers) == 0 {
		return c.deps.Local.Subscribe(ctx, f)
	}
	cf, err := compile(f)
	if err != nil {
		return nil, err
	}
	local, err := c.deps.Local.Subscribe(ctx, f)
	if err != nil {
		return nil, err
	}
	sub := &subscriber{filter: cf, ch: make(chan Entry, max(c.opts.buffer, f.Last))}
	merged := &mergedSubscription{subscriber: sub, local: local}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for e := range local.Entries() {
			sub.send(e)
		}
	}()
	for _, p := range c.deps.Peers {
		if !cf.wants(p.NodeID()) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.follow(ctx, p, f, sub)
		}()
	}
	go func() {
		// The local subscription ends with ctx, the streams of the peers
		// too: the merged one closes after them.
		wg.Wait()
		sub.close()
	}()
	return merged, nil
}

// follow relays the entries of a peer to sub until ctx ends, opening the
// stream again (without replay) when it breaks.
func (c *clusterService) follow(ctx context.Context, p Peer, f Filter, sub *subscriber) {
	remote := f
	remote.Local, remote.Nodes = true, nil
	delay := c.opts.retryMin
	for {
		err := p.Logs(ctx, remote, func(d Delivery) error {
			delay = c.opts.retryMin
			if d.Entry != nil {
				e := *d.Entry
				e.Attrs = c.red.attrs(e.Attrs)
				e.Message = c.red.value(e.Message)
				if sub.filter.match(e) {
					sub.send(e)
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
		remote.Last = 0
		c.deps.Logger.DebugContext(ctx, "logs: stream of a peer broken, reconnecting", "peer", p.NodeID(), "err", err, "delay", delay)
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		delay = min(2*delay, c.opts.retryMax)
	}
}

// mergedSubscription counts the entries lost by the local subscription
// with its own.
type mergedSubscription struct {
	*subscriber
	local Subscription
}

// Dropped implements Subscription.
func (m *mergedSubscription) Dropped() uint64 { return m.subscriber.Dropped() + m.local.Dropped() }
