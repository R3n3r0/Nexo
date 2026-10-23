package update

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/sysstore"
	"github.com/r3n3r0/nexo/internal/version"
)

// jobPath is the document of the last job in the system store.
const jobPath = "update/job.json"

// Deps are the dependencies of the service.
type Deps struct {
	// NodeID is the "host:port" of this node ("local" on a single node).
	NodeID string
	// Peers are the agents of the other nodes (none on a single node).
	Peers []Agent
	// Process controls the process of this node; nil: the server cannot
	// update, restart or stop itself.
	Process Process
	// Health tells whether a node can stop (required with peers).
	Health Health
	// Docs keeps the last job (required).
	Docs sysstore.Store
	// HTTP downloads the binaries (default: a client honoring the proxy
	// of the environment).
	HTTP   *http.Client
	Clock  clock.Clock
	Logger *slog.Logger
}

// Option configures the service.
type Option func(*options)

type options struct {
	nodeTimeout  time.Duration
	pollInterval time.Duration
	stageTimeout time.Duration
	maxSize      int64
}

// WithNodeTimeout bounds the wait for a node: back after its restart, or
// able to stop (default 5 minutes).
func WithNodeTimeout(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.nodeTimeout = d
		}
	}
}

// WithPollInterval sets the interval of the checks while waiting for a
// node (default 2 seconds).
func WithPollInterval(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.pollInterval = d
		}
	}
}

// WithStageTimeout bounds the download and the check of the binary
// (default 15 minutes).
func WithStageTimeout(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.stageTimeout = d
		}
	}
}

// WithMaxBinarySize caps the size of a binary (default 1 GiB).
func WithMaxBinarySize(n int64) Option {
	return func(o *options) {
		if n > 0 {
			o.maxSize = n
		}
	}
}

type service struct {
	deps Deps
	opts options
	// boot identifies this run of the server, started its start.
	boot    string
	started time.Time

	mu     sync.Mutex
	staged *stagedBinary
	// job is the job this run orchestrates, while it runs; cancel
	// interrupts it and done is closed when it returns.
	job    *Job
	cancel context.CancelFunc
	done   chan struct{}
}

var _ Service = (*service)(nil)

// New returns the update service of the node.
func New(deps Deps, opts ...Option) (Service, error) {
	if deps.NodeID == "" || deps.Docs == nil {
		return nil, fmt.Errorf("%w: update: the node id and the system store are required", domain.ErrInvalidArgument)
	}
	if len(deps.Peers) > 0 && deps.Health == nil {
		return nil, fmt.Errorf("%w: update: the health of the storage is required in a cluster", domain.ErrInvalidArgument)
	}
	o := options{nodeTimeout: 5 * time.Minute, pollInterval: 2 * time.Second, stageTimeout: 15 * time.Minute, maxSize: 1 << 30}
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.New(slog.DiscardHandler)
	}
	if deps.HTTP == nil {
		deps.HTTP = &http.Client{Transport: http.DefaultTransport}
	}
	peers := slices.Clone(deps.Peers)
	slices.SortFunc(peers, func(a, b Agent) int { return strings.Compare(a.NodeID(), b.NodeID()) })
	deps.Peers = peers
	boot, err := randomID()
	if err != nil {
		return nil, err
	}
	return &service{deps: deps, opts: o, boot: boot, started: deps.Clock.Now()}, nil
}

// Start implements lifecycle.Runner.
func (s *service) Start(context.Context) error { return nil }

// Stop implements lifecycle.Runner: a job this node orchestrates is
// interrupted (it fails, unless this node already restarts itself).
func (s *service) Stop(ctx context.Context) error {
	s.mu.Lock()
	cancel, done := s.cancel, s.done
	s.mu.Unlock()
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

// NodeID implements Agent.
func (s *service) NodeID() string { return s.deps.NodeID }

// State implements Agent.
func (s *service) State(context.Context) (NodeState, error) {
	st := NodeState{Node: s.deps.NodeID, Version: version.Get().Version, Boot: s.boot, StartedAt: s.started}
	s.mu.Lock()
	if s.staged != nil {
		st.Staged = &Staged{SHA256: s.staged.sha256, Version: s.staged.version}
	}
	s.mu.Unlock()
	return st, nil
}

// RestartNode implements Agent.
func (s *service) RestartNode(context.Context) error {
	p, err := s.process()
	if err != nil {
		return err
	}
	return p.Restart()
}

// StopNode implements Agent.
func (s *service) StopNode(context.Context) error {
	p, err := s.process()
	if err != nil {
		return err
	}
	return p.Stop()
}

// process returns the process control, if the server is run as a process.
func (s *service) process() (Process, error) {
	if s.deps.Process == nil {
		return nil, fmt.Errorf("%w: update: only a server run as a process can update, restart or stop itself", domain.ErrNotImplemented)
	}
	if _, err := s.deps.Process.Executable(); err != nil {
		return nil, err
	}
	return s.deps.Process, nil
}

// StopAll implements Service.
func (s *service) StopAll(ctx context.Context) error {
	p, err := s.process()
	if err != nil {
		return err
	}
	errs := s.eachPeer(ctx, func(ctx context.Context, a Agent) error { return a.StopNode(ctx) })
	if err := p.Stop(); err != nil {
		errs = append(errs, err)
	}
	if err := errors.Join(errs...); err != nil {
		s.deps.Logger.WarnContext(ctx, "update: coordinated stop incomplete", "err", err)
		return fmt.Errorf("update: stop: %w", err)
	}
	s.deps.Logger.InfoContext(ctx, "update: coordinated stop of every node")
	return nil
}

// eachPeer runs fn on every peer in parallel and returns the errors,
// naming their node.
func (s *service) eachPeer(ctx context.Context, fn func(context.Context, Agent) error) []error {
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for _, a := range s.deps.Peers {
		wg.Go(func() {
			if err := fn(ctx, a); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("node %s: %w", a.NodeID(), err))
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return errs
}

// Update implements Service.
func (s *service) Update(ctx context.Context, in UpdateInput) (Job, error) {
	u, err := url.Parse(in.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return Job{}, fmt.Errorf("%w: update: the URL of the binary must be http(s)://host/path", domain.ErrInvalidArgument)
	}
	sum := strings.ToLower(strings.TrimSpace(in.SHA256))
	if b, err := hex.DecodeString(sum); err != nil || len(b) != 32 {
		return Job{}, fmt.Errorf("%w: update: the SHA-256 of the binary must be 64 hex digits", domain.ErrInvalidArgument)
	}
	job := Job{Kind: KindUpdate, URL: in.URL, SHA256: sum}
	return s.begin(ctx, job, in.Mode)
}

// Restart implements Service.
func (s *service) Restart(ctx context.Context, in RestartInput) (Job, error) {
	return s.begin(ctx, Job{Kind: KindRestart}, in.Mode)
}

// begin checks that a job can start, records it and runs it.
func (s *service) begin(ctx context.Context, job Job, mode Mode) (Job, error) {
	switch mode {
	case "":
		mode = ModeRolling
	case ModeRolling, ModeAll:
	default:
		return Job{}, fmt.Errorf("%w: update: unknown mode %q (rolling or all)", domain.ErrInvalidArgument, mode)
	}
	if _, err := s.process(); err != nil {
		return Job{}, err
	}
	if last, err := s.Job(ctx); err == nil && last.State == JobRunning {
		return Job{}, fmt.Errorf("%w: update: job %s (%s) is running on %s", domain.ErrConflict, last.ID, last.Kind, last.Node)
	} else if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return Job{}, err
	}
	nodes := make([]NodeProgress, 0, len(s.deps.Peers)+1)
	for _, a := range s.agents() {
		st, err := a.State(ctx)
		if err != nil {
			return Job{}, fmt.Errorf("%w: update: node %s is unreachable: %v", domain.ErrPreconditionFailed, a.NodeID(), err)
		}
		nodes = append(nodes, NodeProgress{Node: a.NodeID(), Phase: PhasePending, Version: st.Version, Boot: st.Boot})
	}
	if mode == ModeRolling && len(s.deps.Peers) > 0 {
		if h := s.deps.Health.Health(ctx, object.HealthOptions{Maintenance: true}); !h.Healthy {
			return Job{}, fmt.Errorf("%w: update: a node cannot stop without losing the quorum (%s): repair the deployment first, or restart every node at once",
				domain.ErrPreconditionFailed, h.Message)
		}
	}
	id, err := randomID()
	if err != nil {
		return Job{}, err
	}
	job.ID, job.Mode, job.State, job.Nodes = id, mode, JobRunning, nodes
	job.Node, job.Boot, job.Started = s.deps.NodeID, s.boot, s.deps.Clock.Now()
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.mu.Lock()
	if s.job != nil {
		s.mu.Unlock()
		cancel()
		return Job{}, fmt.Errorf("%w: update: job %s is running", domain.ErrConflict, s.job.ID)
	}
	s.job, s.cancel, s.done = &job, cancel, make(chan struct{})
	out := cloneJob(job)
	done := s.done
	s.mu.Unlock()
	if err := s.save(ctx, out); err != nil {
		s.mu.Lock()
		s.job, s.cancel, s.done = nil, nil, nil
		s.mu.Unlock()
		cancel()
		close(done)
		return Job{}, err
	}
	s.deps.Logger.InfoContext(ctx, "update: job started", "job", id, "kind", job.Kind, "mode", mode, "url", job.URL)
	go s.run(runCtx, done)
	return out, nil
}

// errSelfRestart reports a job whose last step is the restart of this
// node: the next run completes it.
var errSelfRestart = errors.New("update: this node restarts")

// run runs the job of this node.
func (s *service) run(ctx context.Context, done chan struct{}) {
	defer close(done)
	s.mu.Lock()
	job := cloneJob(*s.job)
	s.mu.Unlock()
	var err error
	if job.Kind == KindUpdate {
		err = s.runUpdate(ctx, job)
	} else {
		err = s.activate(ctx, job, "")
	}
	if errors.Is(err, errSelfRestart) {
		// The job stays running: the process is about to be replaced.
		return
	}
	s.edit(func(j *Job) {
		j.Finished = s.deps.Clock.Now()
		if err == nil {
			j.State = JobCompleted
			return
		}
		if ctx.Err() != nil {
			err = fmt.Errorf("interrupted: the server stopped: %w", err)
		}
		j.State, j.Error = JobFailed, err.Error()
	})
	if err != nil {
		s.deps.Logger.ErrorContext(ctx, "update: job failed", "job", job.ID, "err", err)
	}
	s.mu.Lock()
	s.job, s.cancel = nil, nil
	s.mu.Unlock()
}

// runUpdate stages the binary on every node, then activates it.
func (s *service) runUpdate(ctx context.Context, job Job) error {
	stageCtx, cancel := context.WithTimeout(ctx, s.opts.stageTimeout)
	defer cancel()
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		errs     []error
		versions = map[string]string{}
	)
	in := StageInput{URL: job.URL, SHA256: job.SHA256}
	for _, a := range s.agents() {
		wg.Go(func() {
			st, err := a.Stage(stageCtx, in)
			if err == nil && (st.Staged == nil || st.Staged.SHA256 != job.SHA256) {
				err = errors.New("the binary is not staged")
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, fmt.Errorf("node %s: %w", a.NodeID(), err))
				s.setNode(a.NodeID(), PhaseFailed, "", err)
				return
			}
			versions[a.NodeID()] = st.Staged.Version
			s.setNode(a.NodeID(), PhaseStaged, "", nil)
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("staging: %w", err)
	}
	target := versions[s.deps.NodeID]
	for node, v := range versions {
		if v != target {
			return fmt.Errorf("staging: node %s reports version %s, node %s %s", node, v, s.deps.NodeID, target)
		}
	}
	s.edit(func(j *Job) { j.Version = target })
	if job.Mode == ModeRolling {
		for _, n := range job.Nodes {
			if !domain.CompatibleVersions(n.Version, target) {
				return fmt.Errorf("%w: node %s runs %s: a change of major version to %s restarts every node at once (mode all)",
					domain.ErrPreconditionFailed, n.Node, n.Version, target)
			}
		}
	}
	return s.activate(ctx, job, target)
}

// activate restarts the nodes, the other ones first: with the staged
// binary when target is set (an update), as they are otherwise.
func (s *service) activate(ctx context.Context, job Job, target string) error {
	act := func(ctx context.Context, a Agent) error {
		if target != "" {
			return a.Activate(ctx, job.SHA256)
		}
		return a.RestartNode(ctx)
	}
	if job.Mode == ModeAll {
		for _, a := range s.deps.Peers {
			s.setNode(a.NodeID(), PhaseRestarting, "", nil)
		}
		errs := s.eachPeer(ctx, act)
		if err := errors.Join(errs...); err != nil {
			return err
		}
	} else {
		for _, a := range s.deps.Peers {
			if err := s.rollNode(ctx, a, act, target); err != nil {
				s.setNode(a.NodeID(), PhaseFailed, "", err)
				return fmt.Errorf("node %s: %w", a.NodeID(), err)
			}
		}
		if len(s.deps.Peers) > 0 {
			if err := s.waitStoppable(ctx); err != nil {
				return fmt.Errorf("node %s: %w", s.deps.NodeID, err)
			}
		}
	}
	// This node restarts last; the job is recorded first, the next run
	// completes it.
	s.setNode(s.deps.NodeID, PhaseRestarting, "", nil)
	if err := act(ctx, s); err != nil {
		s.setNode(s.deps.NodeID, PhaseFailed, "", err)
		return fmt.Errorf("node %s: %w", s.deps.NodeID, err)
	}
	s.deps.Logger.InfoContext(ctx, "update: restarting this node, the last one", "job", job.ID)
	return errSelfRestart
}

// rollNode restarts one node of a rolling job and waits for it.
func (s *service) rollNode(ctx context.Context, a Agent, act func(context.Context, Agent) error, target string) error {
	if err := s.waitStoppable(ctx); err != nil {
		return err
	}
	before, err := a.State(ctx)
	if err != nil {
		return err
	}
	s.setNodeBoot(a.NodeID(), before.Boot)
	s.setNode(a.NodeID(), PhaseRestarting, "", nil)
	if err := act(ctx, a); err != nil {
		return err
	}
	st, err := s.waitBack(ctx, a, before.Boot)
	if err != nil {
		return err
	}
	if target != "" && st.Version != target {
		return fmt.Errorf("restarted with version %s instead of %s", st.Version, target)
	}
	s.setNode(a.NodeID(), PhaseDone, st.Version, nil)
	s.deps.Logger.InfoContext(ctx, "update: node back", "node", a.NodeID(), "version", st.Version)
	return nil
}

// waitStoppable waits until one more node can stop without losing the
// write quorum of any erasure set (its drives back after a restart).
func (s *service) waitStoppable(ctx context.Context) error {
	var last object.HealthResult
	err := s.poll(ctx, func(ctx context.Context) bool {
		last = s.deps.Health.Health(ctx, object.HealthOptions{Maintenance: true})
		return last.Healthy
	})
	if err != nil {
		return fmt.Errorf("the deployment cannot lose a node (%s): %w", last.Message, err)
	}
	return nil
}

// waitBack waits for a node to run again after a restart: a new run of
// its server.
func (s *service) waitBack(ctx context.Context, a Agent, boot string) (NodeState, error) {
	var st NodeState
	err := s.poll(ctx, func(ctx context.Context) bool {
		cur, err := a.State(ctx)
		if err != nil || cur.Boot == boot {
			return false
		}
		st = cur
		return true
	})
	if err != nil {
		return st, fmt.Errorf("not back after its restart: %w", err)
	}
	return st, nil
}

// poll calls ok until it reports true, up to the node timeout.
func (s *service) poll(parent context.Context, ok func(context.Context) bool) error {
	ctx, cancel := context.WithTimeout(parent, s.opts.nodeTimeout)
	defer cancel()
	for {
		callCtx, stop := context.WithTimeout(ctx, max(s.opts.pollInterval, 5*time.Second))
		done := ok(callCtx)
		stop()
		if done {
			return nil
		}
		t := time.NewTimer(s.opts.pollInterval)
		select {
		case <-ctx.Done():
			t.Stop()
			if err := parent.Err(); err != nil {
				return err
			}
			return fmt.Errorf("timed out after %s", s.opts.nodeTimeout)
		case <-t.C:
		}
	}
}

// Job implements Service.
func (s *service) Job(ctx context.Context) (Job, error) {
	s.mu.Lock()
	if s.job != nil {
		j := cloneJob(*s.job)
		s.mu.Unlock()
		return j, nil
	}
	s.mu.Unlock()
	j, err := s.load(ctx)
	if err != nil || j.State != JobRunning {
		return j, err
	}
	// A running job of another run: completed once its orchestrator is
	// in a new run (or gone).
	if j.Node == s.deps.NodeID && j.Boot == s.boot {
		return j, nil
	}
	orch := s.agent(j.Node)
	if orch != nil {
		st, err := orch.State(ctx)
		if err != nil || st.Boot == j.Boot {
			return j, nil
		}
	}
	return s.complete(ctx, j)
}

// complete ends a job whose orchestrator restarted: done if every node is
// back (with the version of an update), failed otherwise. A node not back
// yet leaves it running.
func (s *service) complete(ctx context.Context, j Job) (Job, error) {
	failed := false
	for i := range j.Nodes {
		n := &j.Nodes[i]
		switch n.Phase {
		case PhaseDone:
			continue
		case PhaseRestarting:
		default:
			n.Phase, n.Error, failed = PhaseFailed, "interrupted before its restart", true
			continue
		}
		a := s.agent(n.Node)
		if a == nil {
			n.Phase, n.Error, failed = PhaseFailed, "no longer in the deployment", true
			continue
		}
		st, err := a.State(ctx)
		if err != nil || st.Boot == n.Boot {
			// Not back yet.
			return j, nil
		}
		n.Version = st.Version
		if j.Kind == KindUpdate && st.Version != j.Version {
			n.Phase, n.Error, failed = PhaseFailed, fmt.Sprintf("restarted with version %s instead of %s", st.Version, j.Version), true
			continue
		}
		n.Phase = PhaseDone
	}
	j.Finished = s.deps.Clock.Now()
	j.State = JobCompleted
	if failed {
		j.State = JobFailed
		if j.Error == "" {
			j.Error = "interrupted: the orchestrating node restarted before every node was done"
		}
	}
	if err := s.save(ctx, j); err != nil {
		return j, err
	}
	s.deps.Logger.InfoContext(ctx, "update: job ended", "job", j.ID, "state", j.State)
	return j, nil
}

// agents returns the agents of every node, this one last.
func (s *service) agents() []Agent {
	return append(slices.Clone(s.deps.Peers), Agent(s))
}

// agent returns the agent of a node, nil if it is not in the deployment.
func (s *service) agent(node string) Agent {
	for _, a := range s.agents() {
		if a.NodeID() == node {
			return a
		}
	}
	return nil
}

// edit changes the running job and saves it.
func (s *service) edit(fn func(j *Job)) {
	s.mu.Lock()
	if s.job == nil {
		s.mu.Unlock()
		return
	}
	fn(s.job)
	j := cloneJob(*s.job)
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.save(ctx, j); err != nil {
		s.deps.Logger.WarnContext(ctx, "update: job not saved", "job", j.ID, "err", err)
	}
}

// setNode records the progress of a node.
func (s *service) setNode(node string, phase Phase, ver string, err error) {
	s.edit(func(j *Job) {
		for i := range j.Nodes {
			if j.Nodes[i].Node != node {
				continue
			}
			j.Nodes[i].Phase = phase
			if ver != "" {
				j.Nodes[i].Version = ver
			}
			if err != nil {
				j.Nodes[i].Error = err.Error()
			}
		}
	})
}

// setNodeBoot records the run of a node before its restart.
func (s *service) setNodeBoot(node, boot string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.job == nil {
		return
	}
	for i := range s.job.Nodes {
		if s.job.Nodes[i].Node == node {
			s.job.Nodes[i].Boot = boot
		}
	}
}

func (s *service) save(ctx context.Context, j Job) error {
	data, err := json.Marshal(j)
	if err != nil {
		return err
	}
	if err := s.deps.Docs.Put(ctx, jobPath, data); err != nil {
		return fmt.Errorf("update: save the job: %w", err)
	}
	return nil
}

func (s *service) load(ctx context.Context) (Job, error) {
	data, err := s.deps.Docs.Get(ctx, jobPath)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return Job{}, fmt.Errorf("%w: no update or coordinated restart ran", domain.ErrNotFound)
		}
		return Job{}, fmt.Errorf("update: read the job: %w", err)
	}
	var j Job
	if err := json.Unmarshal(data, &j); err != nil {
		return Job{}, fmt.Errorf("update: corrupt job: %w", err)
	}
	return j, nil
}

func cloneJob(j Job) Job {
	j.Nodes = slices.Clone(j.Nodes)
	return j
}

func randomID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
