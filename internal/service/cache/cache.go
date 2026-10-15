package cache

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/cluster"
	"github.com/r3n3r0/nexo/internal/service/distribution"
	"github.com/r3n3r0/nexo/internal/service/match"
)

// service implements Service.
type service struct {
	deps Deps
	opts options
	// epoch identifies this run of the service in its invalidation
	// sequence.
	epoch string
	crc   *crc32.Table

	store   *store
	flights flightGroup
	snap    atomic.Pointer[snapshot]
	// reloadMu serializes the reloads of the configuration.
	reloadMu sync.Mutex
	// distETags are the ETags of the distributions seen by the requests.
	distETags sync.Map
	dists     atomic.Pointer[distIndex]
	distMu    sync.Mutex
	// distRefreshed is when the index was last refreshed (unix
	// nanoseconds), distMissed the last version a request found missing.
	distRefreshed atomic.Int64
	distMissed    atomic.Pointer[string]

	// Sending side of the invalidations: the sequence, the order of the
	// queues, a link per peer.
	seq     atomic.Uint64
	orderMu sync.Mutex
	links   []*link
	// Receiving side: what each peer sent, the trust in the entries (the
	// end of the lease of the last complete comparison of the sequences,
	// nil when revoked; the peers this node is behind).
	recvMu     sync.Mutex
	origins    map[string]*originState
	trustUntil atomic.Pointer[time.Time]
	lagging    atomic.Int32
	// polled is when each peer last compared this node's sequence.
	pollMu sync.Mutex
	polled map[string]time.Time
	// syncNow asks the background loop for a comparison of the sequences
	// at once (a node that lost the trust in its entries), at most one
	// per syncKickMin (syncKicked, unix nanoseconds).
	syncNow    chan struct{}
	syncKicked atomic.Int64

	histState

	webhooks  *webhookQueue
	egress    *egress
	warmSlots chan struct{}

	stats         statsState
	metrics       *cacheMetrics
	evictions     atomic.Int64
	invalidations atomic.Int64
	flushes       atomic.Int64
	originReads   atomic.Int64

	// bg tracks the background goroutines; life is canceled by Stop.
	bg          sync.WaitGroup
	lifeMu      sync.Mutex
	life        context.Context
	cancel      context.CancelFunc
	unsubscribe []func()
}

var _ Service = (*service)(nil)

// New returns the cache service. The origin, the system store, the
// locker, the notifier and the distributions are required
// (domain.ErrInvalidArgument). The configuration is loaded by Start.
func New(deps Deps, opts ...Option) (Service, error) {
	switch {
	case deps.Origin == nil:
		return nil, fmt.Errorf("%w: cache: the origin is required", domain.ErrInvalidArgument)
	case deps.Store == nil:
		return nil, fmt.Errorf("%w: cache: the system store is required", domain.ErrInvalidArgument)
	case deps.Locks == nil:
		return nil, fmt.Errorf("%w: cache: the locker is required", domain.ErrInvalidArgument)
	case deps.Notifier == nil:
		return nil, fmt.Errorf("%w: cache: the notifier is required", domain.ErrInvalidArgument)
	case deps.Distributions == nil:
		return nil, fmt.Errorf("%w: cache: the distributions are required", domain.ErrInvalidArgument)
	}
	if deps.Matcher == nil {
		deps.Matcher = match.New(match.Deps{})
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.NodeID == "" {
		deps.NodeID = cluster.LocalNodeID
	}
	o := defaultOptions()
	for _, opt := range opts {
		opt(&o)
	}
	eg := &egress{allow: o.webhookAllow, nodes: []string{deps.NodeID}}
	for _, p := range deps.Peers {
		eg.nodes = append(eg.nodes, p.NodeID())
	}
	if deps.HTTP == nil {
		deps.HTTP = eg.client()
	}
	switch o.mode {
	case ModeLocal, ModeShared, ModeSharedLocal:
	default:
		return nil, fmt.Errorf("%w: cache mode %q must be local, shared or shared+local", domain.ErrInvalidArgument, o.mode)
	}
	if len(deps.Peers) == 0 {
		o.mode = ModeLocal
	}
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return nil, err
	}
	s := &service{
		deps: deps, opts: o, egress: eg, epoch: hex.EncodeToString(rnd[:]), crc: crcTable(), origins: map[string]*originState{}, polled: map[string]time.Time{},
		warmSlots: make(chan struct{}, warmOnWriteSlots), syncNow: make(chan struct{}, 1),
		histState: histState{history: map[string]*Invalidation{}, saved: map[string]bool{}},
		stats:     statsState{hosts: map[[2]string]*hostCounters{}, objects: map[[3]string]*objectCounters{}},
	}
	s.life, s.cancel = context.WithCancel(context.Background())
	var d *disk
	if o.dir != "" {
		var err error
		if d, err = newDisk(o.dir, o.diskSize, deps.Logger); err != nil {
			return nil, err
		}
	}
	memory := o.memorySize
	if o.mode == ModeSharedLocal {
		// The local tier of the shared+local mode only keeps the hottest
		// objects.
		memory = max(memory/4, 1<<20)
	}
	entryMax := min(memory/defaultMemoryEntryRatio, int64(maxBlockSize))
	st, err := newStore(storeDeps{memorySize: memory, memoryEntryMax: entryMax, disk: d, now: s.now, logger: deps.Logger, onEvict: s.countEviction})
	if err != nil {
		return nil, err
	}
	s.store = st
	s.flights.st = st
	queueDir := o.webhookDir
	if queueDir == "" && o.dir != "" {
		queueDir = filepath.Join(o.dir, "webhooks")
	}
	if s.webhooks, err = newWebhookQueue(queueDir); err != nil {
		return nil, err
	}
	for _, p := range deps.Peers {
		s.links = append(s.links, &link{peer: p})
	}
	if s.metrics, err = newMetrics(context.Background(), deps.Registry); err != nil {
		return nil, err
	}
	s.snap.Store(&snapshot{policies: builtinPolicies(), dists: map[string]*compiledDist{}})
	return s, nil
}

// now is the time of the service clock.
func (s *service) now() time.Time { return s.deps.Clock.Now() }

// lifeContext is the context of the background work (canceled by Stop).
func (s *service) lifeContext() context.Context {
	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()
	return s.life
}

// Start implements lifecycle.Runner: it loads the configuration,
// subscribes to the changes of the peers, indexes the disk tier and starts
// the background work (retries, synchronization, webhooks).
func (s *service) Start(ctx context.Context) error {
	s.lifeMu.Lock()
	if len(s.unsubscribe) > 0 {
		s.lifeMu.Unlock()
		return nil
	}
	s.unsubscribe = append(s.unsubscribe,
		s.deps.Notifier.Subscribe(EventConfig, cluster.HandlerFunc(s.handleConfigEvent)),
		s.deps.Notifier.Subscribe(distribution.EventDistribution, cluster.HandlerFunc(func(ctx context.Context, _ cluster.Event) error {
			s.refreshDistributions(ctx)
			return nil
		})))
	life := s.life
	s.lifeMu.Unlock()
	if err := s.Reload(ctx); err != nil {
		s.deps.Logger.WarnContext(ctx, "cache: configuration not loaded, retried on the next resynchronization", "err", err)
	}
	s.refreshDistributions(ctx)
	if d := s.store.deps.disk; d != nil {
		s.bg.Add(1)
		go func() {
			defer s.bg.Done()
			n := 0
			if err := d.load(func(m *entryMeta, size int64) {
				s.store.adopt(m, size)
				n++
			}); err != nil {
				s.deps.Logger.WarnContext(life, "cache: disk tier not indexed", "err", err)
			}
			s.deps.Logger.InfoContext(life, "cache: disk tier indexed", "entries", n)
		}()
	}
	s.bg.Add(2)
	go s.backgroundLoop(life)
	go s.webhookLoop(life)
	s.deps.Logger.InfoContext(ctx, "cache started", "mode", string(s.mode()), "memory", Size(s.opts.memorySize).String(),
		"dir", s.opts.dir, "disk", Size(s.opts.diskSize).String())
	return nil
}

// Stop implements lifecycle.Runner.
func (s *service) Stop(ctx context.Context) error {
	s.lifeMu.Lock()
	for _, u := range s.unsubscribe {
		u()
	}
	s.unsubscribe = nil
	s.cancel()
	s.lifeMu.Unlock()
	done := make(chan struct{})
	go func() {
		s.bg.Wait()
		close(done)
	}()
	var err error
	select {
	case <-done:
	case <-ctx.Done():
		err = errors.Join(errors.New("cache: background work still running at the stop"), ctx.Err())
	}
	s.store.close()
	return err
}
