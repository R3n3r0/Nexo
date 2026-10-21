package pools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/sysstore"
)

// Documents of the system store.
const (
	// ControlDoc records the requests: the pools to decommission and the
	// rebalance, with the time of each request.
	ControlDoc = "pools/control.json"
	// ProgressDir holds the progress of the jobs, written by the leader:
	// decommission-<pool id hash>.json and rebalance.json.
	ProgressDir = "pools"
)

// Leader reports whether this node runs the jobs (cluster.Leader).
type Leader interface {
	IsLeader() bool
}

// Deps are the dependencies of the service.
type Deps struct {
	// Objects lists the buckets (required).
	Objects object.Service
	// Mover moves the objects between pools (required).
	Mover object.PoolMover
	// Docs keeps the control and progress documents (required).
	Docs sysstore.Store
	// PoolIDs identify the pools, in order: their command-line arguments
	// (required, one per pool).
	PoolIDs []string
	// Leader runs the jobs on one node; nil runs them here.
	Leader Leader
	Clock  clock.Clock
	Logger *slog.Logger
}

// Option configures the service.
type Option func(*options)

type options struct {
	poll      time.Duration
	batch     int
	maxPasses int
	slack     float64
	retry     time.Duration
	// check is the number of objects a rebalance moves between two
	// checks of the balance.
	check int
}

// WithPollInterval sets how often the control document is read and the
// jobs resume (default 5s).
func WithPollInterval(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.poll = d
		}
	}
}

// WithBatch sets the number of keys listed per page (default 500).
func WithBatch(n int) Option {
	return func(o *options) {
		if n > 0 {
			o.batch = n
		}
	}
}

// WithMaxPasses sets the passes after which a decommission that keeps
// failing to move some objects is marked failed (default 10).
func WithMaxPasses(n int) Option {
	return func(o *options) {
		if n > 0 {
			o.maxPasses = n
		}
	}
}

// WithRetryDelay sets the pause between two passes of a decommission
// that left objects behind (default 1m).
func WithRetryDelay(d time.Duration) Option {
	return func(o *options) {
		if d >= 0 {
			o.retry = d
		}
	}
}

func defaultOptions() options {
	return options{poll: 5 * time.Second, batch: 500, maxPasses: 10, slack: 0.05, retry: time.Minute, check: 8}
}

// control is the control document.
type control struct {
	// Decommission maps a pool id to the time its decommission was
	// requested; a canceled one is removed.
	Decommission map[string]time.Time `json:"decommission,omitempty"`
	// Canceled maps a pool id to the time its decommission was canceled.
	Canceled map[string]time.Time `json:"canceled,omitempty"`
	// Rebalance is the time the running rebalance was requested (zero:
	// none); RebalanceStopped the time the last one was stopped.
	Rebalance        time.Time `json:"rebalance,omitzero"`
	RebalanceStopped time.Time `json:"rebalanceStopped,omitzero"`
}

type service struct {
	deps Deps
	opts options

	// ctlMu serializes the read-modify-write of the control document on
	// this node (the store serializes the writes across nodes).
	ctlMu sync.Mutex

	runMu   sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	started bool
}

var _ Service = (*service)(nil)

// New returns the service.
func New(deps Deps, opts ...Option) (Service, error) {
	if deps.Objects == nil || deps.Mover == nil || deps.Docs == nil {
		return nil, fmt.Errorf("%w: pools: the object layer, the mover and the system store are required", domain.ErrInvalidArgument)
	}
	if len(deps.PoolIDs) == 0 {
		return nil, fmt.Errorf("%w: pools: the pool ids are required", domain.ErrInvalidArgument)
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	o := defaultOptions()
	for _, opt := range opts {
		opt(&o)
	}
	return &service{deps: deps, opts: o}, nil
}

// --- documents -------------------------------------------------------------------

func (s *service) readControl(ctx context.Context) (control, error) {
	var c control
	data, err := s.deps.Docs.Get(ctx, ControlDoc)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return c, nil
	case err != nil:
		return c, fmt.Errorf("pools: read the control document: %w", err)
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("pools: decode the control document: %w", err)
	}
	return c, nil
}

func (s *service) updateControl(ctx context.Context, fn func(c *control) error) error {
	s.ctlMu.Lock()
	defer s.ctlMu.Unlock()
	c, err := s.readControl(ctx)
	if err != nil {
		return err
	}
	if c.Decommission == nil {
		c.Decommission = map[string]time.Time{}
	}
	if c.Canceled == nil {
		c.Canceled = map[string]time.Time{}
	}
	if err := fn(&c); err != nil {
		return err
	}
	data, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("pools: encode the control document: %w", err)
	}
	if err := s.deps.Docs.Put(ctx, ControlDoc, data); err != nil {
		return fmt.Errorf("pools: write the control document: %w", err)
	}
	return nil
}

// progressPath is the progress document of the decommission of a pool:
// named by a hash of its id (ids hold characters the store refuses).
func progressPath(poolID string) string {
	return ProgressDir + "/decommission-" + strconv.FormatUint(fnv64(poolID), 16) + ".json"
}

const rebalancePath = ProgressDir + "/rebalance.json"

func fnv64(s string) uint64 {
	h := uint64(14695981039346656037)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}

func (s *service) readJob(ctx context.Context, path string) (*Job, error) {
	data, err := s.deps.Docs.Get(ctx, path)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("pools: read %s: %w", path, err)
	}
	var j Job
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, fmt.Errorf("pools: decode %s: %w", path, err)
	}
	return &j, nil
}

func (s *service) writeJob(ctx context.Context, path string, j *Job) error {
	j.UpdatedAt = s.deps.Clock.Now().UTC()
	data, err := json.Marshal(j)
	if err != nil {
		return fmt.Errorf("pools: encode %s: %w", path, err)
	}
	if err := s.deps.Docs.Put(ctx, path, data); err != nil {
		return fmt.Errorf("pools: write %s: %w", path, err)
	}
	return nil
}

// saveProgress writes the progress of a job that stops midway, also when
// ctx is over (a stop): the moves done since the last page are counted
// and the next run starts after them.
func (s *service) saveProgress(ctx context.Context, path string, j *Job) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return s.writeJob(ctx, path, j)
}

// --- requests --------------------------------------------------------------------

func (s *service) checkPool(pool int) error {
	if pool < 0 || pool >= len(s.deps.PoolIDs) {
		return fmt.Errorf("%w: no pool %d (the deployment has %d)", domain.ErrInvalidArgument, pool+1, len(s.deps.PoolIDs))
	}
	return nil
}

// Decommission implements Service.
func (s *service) Decommission(ctx context.Context, pool int) error {
	if err := s.checkPool(pool); err != nil {
		return err
	}
	if pool == 0 {
		return fmt.Errorf("%w: the first pool holds the system documents and cannot be decommissioned", domain.ErrInvalidArgument)
	}
	err := s.updateControl(ctx, func(c *control) error {
		if !c.Rebalance.IsZero() {
			return fmt.Errorf("%w: a rebalance is running: stop it first", domain.ErrInvalidArgument)
		}
		remaining := 0
		for i, id := range s.deps.PoolIDs {
			if _, ok := c.Decommission[id]; !ok && i != pool {
				remaining++
			}
		}
		if remaining == 0 {
			return fmt.Errorf("%w: no pool would be left to hold the objects", domain.ErrInvalidArgument)
		}
		id := s.deps.PoolIDs[pool]
		c.Decommission[id] = s.deps.Clock.Now().UTC()
		delete(c.Canceled, id)
		return nil
	})
	if err != nil {
		return err
	}
	s.deps.Mover.SetPoolDraining(pool, true)
	s.deps.Logger.InfoContext(ctx, "pools: decommission requested", "pool", pool+1)
	return nil
}

// CancelDecommission implements Service.
func (s *service) CancelDecommission(ctx context.Context, pool int) error {
	if err := s.checkPool(pool); err != nil {
		return err
	}
	err := s.updateControl(ctx, func(c *control) error {
		id := s.deps.PoolIDs[pool]
		if _, ok := c.Decommission[id]; !ok {
			return fmt.Errorf("%w: pool %d is not being decommissioned", domain.ErrInvalidArgument, pool+1)
		}
		delete(c.Decommission, id)
		c.Canceled[id] = s.deps.Clock.Now().UTC()
		return nil
	})
	if err != nil {
		return err
	}
	s.deps.Mover.SetPoolDraining(pool, false)
	s.deps.Logger.InfoContext(ctx, "pools: decommission canceled", "pool", pool+1)
	return nil
}

// StartRebalance implements Service.
func (s *service) StartRebalance(ctx context.Context) error {
	if len(s.deps.PoolIDs) < 2 {
		return fmt.Errorf("%w: a rebalance needs at least two pools", domain.ErrInvalidArgument)
	}
	return s.updateControl(ctx, func(c *control) error {
		if len(c.Decommission) > 0 {
			return fmt.Errorf("%w: a decommission is running", domain.ErrInvalidArgument)
		}
		c.Rebalance = s.deps.Clock.Now().UTC()
		return nil
	})
}

// StopRebalance implements Service.
func (s *service) StopRebalance(ctx context.Context) error {
	return s.updateControl(ctx, func(c *control) error {
		if c.Rebalance.IsZero() {
			return fmt.Errorf("%w: no rebalance is running", domain.ErrInvalidArgument)
		}
		c.Rebalance = time.Time{}
		c.RebalanceStopped = s.deps.Clock.Now().UTC()
		return nil
	})
}

// --- status ----------------------------------------------------------------------

// Status implements Service. A job requested but not yet picked up by the
// leader is reported running with no progress; a canceled one keeps its
// last progress.
func (s *service) Status(ctx context.Context) (Status, error) {
	c, err := s.readControl(ctx)
	if err != nil {
		return Status{}, err
	}
	usage := s.deps.Mover.PoolUsage(ctx)
	out := Status{Pools: make([]Pool, len(s.deps.PoolIDs))}
	for i, id := range s.deps.PoolIDs {
		p := Pool{Index: i, ID: id}
		if i < len(usage) {
			p.Total, p.Free, p.Draining = usage[i].Total, usage[i].Free, usage[i].Draining
		}
		job, err := s.readJob(ctx, progressPath(id))
		if err != nil {
			return Status{}, err
		}
		requested, active := c.Decommission[id]
		switch {
		case active && (job == nil || !job.StartedAt.Equal(requested)):
			job = &Job{State: JobRunning, StartedAt: requested, Pool: i}
		case !active && job != nil && job.State == JobRunning:
			job.State = JobCanceled
			job.FinishedAt = c.Canceled[id]
		}
		p.Decommission = job
		out.Pools[i] = p
	}
	reb, err := s.readJob(ctx, rebalancePath)
	if err != nil {
		return Status{}, err
	}
	switch {
	case !c.Rebalance.IsZero() && (reb == nil || !reb.StartedAt.Equal(c.Rebalance)):
		reb = &Job{State: JobRunning, StartedAt: c.Rebalance}
	case c.Rebalance.IsZero() && reb != nil && reb.State == JobRunning:
		reb.State = JobCanceled
		reb.FinishedAt = c.RebalanceStopped
	}
	out.Rebalance = reb
	return out, nil
}

// --- runner ----------------------------------------------------------------------

// Start implements lifecycle.Runner.
func (s *service) Start(ctx context.Context) error {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if s.started {
		return nil
	}
	// The draining pools apply before the first request is served.
	if c, err := s.readControl(ctx); err == nil {
		s.applyDraining(c)
	} else {
		s.deps.Logger.WarnContext(ctx, "pools: cannot read the control document yet", "err", err)
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.cancel, s.done, s.started = cancel, make(chan struct{}), true
	go s.loop(runCtx, s.done)
	return nil
}

// Stop implements lifecycle.Runner: the job in progress stops after the
// object it is moving (its progress is saved).
func (s *service) Stop(ctx context.Context) error {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if !s.started {
		return nil
	}
	s.cancel()
	select {
	case <-s.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	s.started = false
	return nil
}

func (s *service) loop(ctx context.Context, done chan struct{}) {
	defer close(done)
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		s.tick(ctx)
		timer.Reset(s.opts.poll)
	}
}

// tick applies the control document and, on the leader, advances the jobs.
func (s *service) tick(ctx context.Context) {
	c, err := s.readControl(ctx)
	if err != nil {
		s.deps.Logger.WarnContext(ctx, "pools: read the control document", "err", err)
		return
	}
	s.applyDraining(c)
	if s.deps.Leader != nil && !s.deps.Leader.IsLeader() {
		return
	}
	for i, id := range s.deps.PoolIDs {
		if requested, ok := c.Decommission[id]; ok {
			s.runDecommission(ctx, i, requested)
		}
	}
	if !c.Rebalance.IsZero() {
		s.runRebalance(ctx, c.Rebalance)
	}
}

// applyDraining marks the pools being (or done being) decommissioned as
// draining, and the others as not.
func (s *service) applyDraining(c control) {
	for i, id := range s.deps.PoolIDs {
		_, draining := c.Decommission[id]
		s.deps.Mover.SetPoolDraining(i, draining)
	}
}

// stillRequested re-reads the control document: a job stops as soon as
// its request is gone or replaced.
func (s *service) stillRequested(ctx context.Context, check func(c control) bool) bool {
	c, err := s.readControl(ctx)
	if err != nil {
		return true // an unreadable document stops nothing
	}
	return check(c)
}

// leading reports whether this node still runs the jobs.
func (s *service) leading() bool { return s.deps.Leader == nil || s.deps.Leader.IsLeader() }
