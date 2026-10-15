package cluster

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/lock"
)

// Defaults of the leader election.
const (
	// LeaderResource is the name of the distributed lock of the leader,
	// in the system volume.
	LeaderResource       = "leader"
	defaultElectionRetry = 5 * time.Second
	defaultLeaderAcquire = 2 * time.Second
)

// LeaderDeps are the dependencies of the leader election.
type LeaderDeps struct {
	// Locker is the distributed locker of the deployment (required). The
	// lease of the leadership is the lease of its lock.
	Locker lock.Locker
	// NodeID identifies this node in the logs.
	NodeID string
	Logger *slog.Logger
}

// LeaderOption configures the leader election.
type LeaderOption func(*leaderOptions)

type leaderOptions struct {
	resource string
	retry    time.Duration
	acquire  time.Duration
}

// WithLeaderResource sets the name of the leader lock (default "leader").
func WithLeaderResource(name string) LeaderOption {
	return func(o *leaderOptions) {
		if name != "" {
			o.resource = name
		}
	}
}

// WithElectionInterval sets the pause between two candidacies of a node
// that is not the leader (default 5s, with jitter).
func WithElectionInterval(d time.Duration) LeaderOption {
	return func(o *leaderOptions) {
		if d > 0 {
			o.retry = d
		}
	}
}

// WithLeaderAcquireTimeout bounds one candidacy (default 2s).
func WithLeaderAcquireTimeout(d time.Duration) LeaderOption {
	return func(o *leaderOptions) {
		if d > 0 {
			o.acquire = d
		}
	}
}

// LeaderFuncs adapts two functions to LeaderListener; either may be nil.
type LeaderFuncs struct {
	Elected func(ctx context.Context)
	Revoked func(ctx context.Context)
}

var _ LeaderListener = LeaderFuncs{}

// OnElected implements LeaderListener.
func (f LeaderFuncs) OnElected(ctx context.Context) {
	if f.Elected != nil {
		f.Elected(ctx)
	}
}

// OnRevoked implements LeaderListener.
func (f LeaderFuncs) OnRevoked(ctx context.Context) {
	if f.Revoked != nil {
		f.Revoked(ctx)
	}
}

// listeners is the registry of the leader listeners shared by the
// implementations.
type listeners struct {
	mu   sync.Mutex
	set  map[*LeaderListener]struct{}
	term context.Context // the current term, nil when not leading
}

func (ls *listeners) add(l LeaderListener) (func(), context.Context) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.set == nil {
		ls.set = make(map[*LeaderListener]struct{})
	}
	key := &l
	ls.set[key] = struct{}{}
	return func() {
		ls.mu.Lock()
		delete(ls.set, key)
		ls.mu.Unlock()
	}, ls.term
}

// begin records the term and returns the listeners to notify.
func (ls *listeners) begin(term context.Context) []LeaderListener {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.term = term
	return ls.snapshotLocked()
}

// end clears the term and returns the listeners to notify.
func (ls *listeners) end() []LeaderListener {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.term = nil
	return ls.snapshotLocked()
}

func (ls *listeners) snapshotLocked() []LeaderListener {
	out := make([]LeaderListener, 0, len(ls.set))
	for l := range ls.set {
		out = append(out, *l)
	}
	return out
}

// leader is the distributed election: candidates take the leader lock in
// turn; the holder leads until it releases it (Stop) or loses it (its
// refresh no longer reaches the quorum, or it crashed and the lock
// expired on the servers).
type leader struct {
	deps LeaderDeps
	opts leaderOptions

	elected   atomic.Bool
	listeners listeners

	lifeMu sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

var _ Leader = (*leader)(nil)

// NewLeader returns the leader election of a distributed deployment.
func NewLeader(deps LeaderDeps, opts ...LeaderOption) (Leader, error) {
	if deps.Locker == nil {
		return nil, fmt.Errorf("%w: cluster: the leader election needs the distributed locker", domain.ErrInvalidArgument)
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	o := leaderOptions{resource: LeaderResource, retry: defaultElectionRetry, acquire: defaultLeaderAcquire}
	for _, opt := range opts {
		opt(&o)
	}
	return &leader{deps: deps, opts: o}, nil
}

// IsLeader implements Leader.
func (l *leader) IsLeader() bool { return l.elected.Load() }

// Subscribe implements Leader.
func (l *leader) Subscribe(ln LeaderListener) func() {
	unsubscribe, term := l.listeners.add(ln)
	if term != nil && term.Err() == nil {
		ln.OnElected(term)
	}
	return unsubscribe
}

// Start implements lifecycle.Runner: the node becomes a candidate.
func (l *leader) Start(context.Context) error {
	l.lifeMu.Lock()
	defer l.lifeMu.Unlock()
	if l.cancel != nil {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	l.cancel, l.done = cancel, make(chan struct{})
	go l.run(ctx, l.done) // #nosec G118 -- the candidacy outlives the context of Start and ends with Stop (cancel)
	return nil
}

// Stop implements lifecycle.Runner: the leader releases the lock (another
// node takes over at its next candidacy).
func (l *leader) Stop(ctx context.Context) error {
	l.lifeMu.Lock()
	cancel, done := l.cancel, l.done
	l.cancel, l.done = nil, nil
	l.lifeMu.Unlock()
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

func (l *leader) run(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	ns := l.deps.Locker.NewNSLock(ctx, domain.SystemBucket, l.opts.resource)
	for {
		lc, err := ns.GetLock(ctx, l.opts.acquire)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if !l.sleep(ctx) {
				return
			}
			continue
		}
		term := lc.Context()
		l.elected.Store(true)
		l.deps.Logger.Info("cluster: this node is the leader", "leader", l.deps.NodeID)
		for _, ln := range l.listeners.begin(term) {
			ln.OnElected(term)
		}
		<-term.Done()
		l.elected.Store(false)
		stopping := ctx.Err() != nil
		if stopping {
			l.deps.Logger.Info("cluster: leadership released", "leader", l.deps.NodeID)
		} else {
			l.deps.Logger.Warn("cluster: leadership lost", "leader", l.deps.NodeID)
		}
		lc.Unlock(context.Background())
		for _, ln := range l.listeners.end() {
			ln.OnRevoked(context.Background())
		}
		if stopping || !l.sleep(ctx) {
			return
		}
	}
}

// sleep waits for the next candidacy, with jitter so that the candidates
// do not compete in lockstep.
func (l *leader) sleep(ctx context.Context) bool {
	d := l.opts.retry/2 + rand.N(l.opts.retry/2+1) // #nosec G404 -- the jitter of the candidacy retries, not a secret
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// localLeader is the election of a single-node deployment: the node always
// leads while it runs.
type localLeader struct {
	logger    *slog.Logger
	elected   atomic.Bool
	listeners listeners

	lifeMu sync.Mutex
	cancel context.CancelFunc
}

var _ Leader = (*localLeader)(nil)

// NewLocalLeader returns the leader of a single-node deployment: elected
// from Start to Stop.
func NewLocalLeader(deps LeaderDeps) Leader {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &localLeader{logger: deps.Logger}
}

// IsLeader implements Leader.
func (l *localLeader) IsLeader() bool { return l.elected.Load() }

// Subscribe implements Leader.
func (l *localLeader) Subscribe(ln LeaderListener) func() {
	unsubscribe, term := l.listeners.add(ln)
	if term != nil && term.Err() == nil {
		ln.OnElected(term)
	}
	return unsubscribe
}

// Start implements lifecycle.Runner.
func (l *localLeader) Start(context.Context) error {
	l.lifeMu.Lock()
	if l.cancel != nil {
		l.lifeMu.Unlock()
		return nil
	}
	term, cancel := context.WithCancel(context.Background())
	l.cancel = cancel
	l.lifeMu.Unlock()
	l.elected.Store(true)
	for _, ln := range l.listeners.begin(term) {
		ln.OnElected(term)
	}
	return nil
}

// Stop implements lifecycle.Runner.
func (l *localLeader) Stop(ctx context.Context) error {
	l.lifeMu.Lock()
	cancel := l.cancel
	l.cancel = nil
	l.lifeMu.Unlock()
	if cancel == nil {
		return nil
	}
	l.elected.Store(false)
	cancel()
	for _, ln := range l.listeners.end() {
		ln.OnRevoked(ctx)
	}
	return nil
}
