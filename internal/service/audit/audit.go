package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/config"
	"github.com/r3n3r0/nexo/internal/service/event"
)

// Deps are the dependencies of the audit log.
type Deps struct {
	// Logger receives one structured record per entry (default
	// slog.Default()).
	Logger *slog.Logger
	Clock  clock.Clock
	// NodeID names this node in the entries.
	NodeID string
	// DeploymentID is set in the entries.
	DeploymentID string
	// Peers are the other nodes of a cluster (Search).
	Peers []Peer
	// Targets delivers the entries to the audit targets with their
	// persistent queues: an event dispatcher built with NewTargetFactory
	// (nil: no target).
	Targets event.Dispatcher
	// Config provides the audit_webhook and audit_kafka subsystems (nil:
	// no target is configured).
	Config config.Service
}

// Option configures the audit log.
type Option func(*options)

type options struct {
	level       slog.Level
	memory      int
	peerTimeout time.Duration
}

// WithLevel sets the level audit records are logged at (default Info).
func WithLevel(l slog.Level) Option {
	return func(o *options) { o.level = l }
}

// WithMemory sets how many recent entries the node keeps for the
// searches (default 10000).
func WithMemory(n int) Option {
	return func(o *options) {
		if n > 0 {
			o.memory = n
		}
	}
}

// WithPeerTimeout bounds the search on a peer (default 5s).
func WithPeerTimeout(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.peerTimeout = d
		}
	}
}

// Limits of a search.
const (
	defaultLimit = 100
	maxLimit     = 1000
)

// Configuration subsystems of the audit targets: audit_<kind> configures
// the target "_", audit_<kind>:<id> the target <id>; the keys are those of
// the notification targets of the same kind (endpoint, auth_token,
// client_cert, client_key, tls_skip_verify; brokers, topic, sasl*, tls*;
// enable, queue_dir, queue_limit, comment).
const (
	SubsystemPrefix  = "audit_"
	SubsystemWebhook = SubsystemPrefix + "webhook"
	SubsystemKafka   = SubsystemPrefix + "kafka"
)

// IsTargetSubsystem reports whether a configuration subsystem configures
// an audit target.
func IsTargetSubsystem(subsystem string) bool {
	_, ok := notifySubsystem(subsystem)
	return ok
}

// notifySubsystem maps an audit subsystem to the notification subsystem
// of the same target kind and id.
func notifySubsystem(subsystem string) (string, bool) {
	rest, ok := strings.CutPrefix(subsystem, SubsystemPrefix)
	if !ok {
		return "", false
	}
	kind, _, _ := strings.Cut(rest, ":")
	if kind != "webhook" && kind != "kafka" {
		return "", false
	}
	return event.SubsystemPrefix + rest, true
}

type service struct {
	deps Deps
	opts options

	mu   sync.RWMutex
	ring []Entry
	next int
	full bool

	// targets reports an enabled audit target: the entries are encoded
	// for them only then.
	targets atomic.Bool

	runMu      sync.Mutex
	cancel     context.CancelFunc
	done       chan struct{}
	configMu   sync.Mutex
	fromConfig map[event.TargetID]event.TargetConfig
	// delivering reports the targets started (configMu held).
	delivering bool
}

var _ Dispatcher = (*service)(nil)

// New returns the audit log of the node.
func New(deps Deps, opts ...Option) (Dispatcher, error) {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.NodeID == "" {
		deps.NodeID = "local"
	}
	o := options{level: slog.LevelInfo, memory: 10000, peerTimeout: 5 * time.Second}
	for _, opt := range opts {
		opt(&o)
	}
	// The ring grows with the entries up to its size: a node that serves
	// few requests keeps little.
	return &service{deps: deps, opts: o, fromConfig: map[event.TargetID]event.TargetConfig{}}, nil
}

// NewSlog returns an audit log that writes the entries to a structured
// logger and keeps them in memory, without targets.
func NewSlog(deps Deps, opts ...Option) Service {
	deps.Targets, deps.Config = nil, nil
	s, _ := New(deps, opts...)
	return s
}

// Log implements Service.
func (s *service) Log(ctx context.Context, e Entry) error {
	if e.Version == "" {
		e.Version = EntryVersion
	}
	if e.Time.IsZero() {
		e.Time = s.deps.Clock.Now()
	}
	if e.Node == "" {
		e.Node = s.deps.NodeID
	}
	if e.DeploymentID == "" {
		e.DeploymentID = s.deps.DeploymentID
	}
	s.logRecord(ctx, e)
	s.mu.Lock()
	if s.full {
		s.ring[s.next] = e
		s.next = (s.next + 1) % len(s.ring)
	} else {
		s.ring = append(s.ring, e)
		s.next = len(s.ring)
		if s.next == s.opts.memory {
			s.next, s.full = 0, true
		}
	}
	s.mu.Unlock()
	if s.targets.Load() && s.deps.Targets != nil {
		return s.publish(ctx, e)
	}
	return nil
}

// logRecord writes the structured record of an entry.
func (s *service) logRecord(ctx context.Context, e Entry) {
	attrs := []slog.Attr{
		slog.String("version", e.Version),
		slog.Time("time", e.Time),
		slog.String("node", e.Node),
		slog.String("type", e.Type),
		slog.String("request_id", e.RequestID),
		slog.String("api", e.API.Name),
		slog.String("method", e.API.Method),
		slog.String("bucket", e.API.Bucket),
		slog.String("object", e.API.Object),
		slog.Int("status_code", e.API.StatusCode),
		slog.Int64("rx", e.API.InputBytes),
		slog.Int64("tx", e.API.OutputBytes),
		slog.Duration("time_to_response", e.API.TimeToResponse),
		slog.String("remote_host", e.RemoteHost),
		slog.String("user_agent", e.UserAgent),
		slog.String("access_key", e.AccessKey),
	}
	if e.ParentUser != "" {
		attrs = append(attrs, slog.String("parent_user", e.ParentUser))
	}
	if e.Error != "" {
		attrs = append(attrs, slog.String("error", e.Error))
	}
	if len(e.Tags) > 0 {
		attrs = append(attrs, slog.Any("tags", e.Tags))
	}
	s.deps.Logger.LogAttrs(ctx, s.opts.level, "audit", attrs...)
}

// publish hands an entry to the audit targets (their queues).
func (s *service) publish(ctx context.Context, e Entry) error {
	// #nosec G117 -- AccessKey is the access key ID of the caller, never its secret key.
	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("audit: encode entry: %w", err)
	}
	ev := event.NewEvent(EventName, "", e.Time)
	ev.ResponseElements[envelopeKey] = string(data)
	if err := s.deps.Targets.Publish(context.WithoutCancel(ctx), ev); err != nil {
		return fmt.Errorf("audit: queue entry: %w", err)
	}
	return nil
}

// Search implements Service.
func (s *service) Search(ctx context.Context, q Query) ([]Entry, error) {
	if err := validate(&q); err != nil {
		return nil, err
	}
	var out []Entry
	if len(q.Nodes) == 0 || slices.Contains(q.Nodes, s.deps.NodeID) {
		out = s.local(q)
	}
	if !q.Local {
		out = append(out, s.peers(ctx, q)...)
		sort.SliceStable(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
		if len(out) > q.Limit {
			out = out[:q.Limit]
		}
	}
	return out, nil
}

// validate applies the defaults and bounds of a query.
func validate(q *Query) error {
	switch {
	case q.Limit == 0:
		q.Limit = defaultLimit
	case q.Limit < 0 || q.Limit > maxLimit:
		return fmt.Errorf("%w: audit: the limit is between 1 and %d", domain.ErrInvalidArgument, maxLimit)
	}
	for _, c := range q.StatusCodes {
		if c < 100 || c > 599 {
			return fmt.Errorf("%w: audit: invalid status %d", domain.ErrInvalidArgument, c)
		}
	}
	for _, c := range q.StatusClasses {
		if c < 1 || c > 5 {
			return fmt.Errorf("%w: audit: invalid status class %dxx", domain.ErrInvalidArgument, c)
		}
	}
	if !q.Since.IsZero() && !q.Until.IsZero() && q.Until.Before(q.Since) {
		return fmt.Errorf("%w: audit: until precedes since", domain.ErrInvalidArgument)
	}
	return nil
}

// local returns the matching entries of this node, newest first.
func (s *service) local(q Query) []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := s.next
	if s.full {
		n = len(s.ring)
	}
	var out []Entry
	for i := 0; i < n && len(out) < q.Limit; i++ {
		idx := (s.next - 1 - i + len(s.ring)) % len(s.ring)
		if match(q, s.ring[idx]) {
			out = append(out, s.ring[idx])
		}
	}
	return out
}

// peers searches the peers selected by the query.
func (s *service) peers(ctx context.Context, q Query) []Entry {
	remote := q
	remote.Local, remote.Nodes = true, nil
	var mu sync.Mutex
	var wg sync.WaitGroup
	var out []Entry
	for _, p := range s.deps.Peers {
		if len(q.Nodes) > 0 && !slices.Contains(q.Nodes, p.NodeID()) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			callCtx, cancel := context.WithTimeout(ctx, s.opts.peerTimeout)
			defer cancel()
			entries, err := p.SearchAudit(callCtx, remote)
			if err != nil {
				s.deps.Logger.DebugContext(ctx, "audit: search of a peer failed", "peer", p.NodeID(), "err", err)
				return
			}
			mu.Lock()
			out = append(out, entries...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

// match reports whether e matches q.
func match(q Query, e Entry) bool {
	if q.API != "" && !strings.EqualFold(q.API, e.API.Name) {
		return false
	}
	if q.Bucket != "" && q.Bucket != e.API.Bucket {
		return false
	}
	if q.User != "" && q.User != e.AccessKey && q.User != e.ParentUser {
		return false
	}
	if q.Type != "" && q.Type != e.Type {
		return false
	}
	if q.RequestID != "" && q.RequestID != e.RequestID {
		return false
	}
	if len(q.StatusCodes) > 0 || len(q.StatusClasses) > 0 {
		if !slices.Contains(q.StatusCodes, e.API.StatusCode) && !slices.Contains(q.StatusClasses, e.API.StatusCode/100) {
			return false
		}
	}
	if q.ErrorsOnly && e.API.StatusCode < 400 && e.Error == "" {
		return false
	}
	if !q.Since.IsZero() && e.Time.Before(q.Since) {
		return false
	}
	return q.Until.IsZero() || !e.Time.After(q.Until)
}

// Start implements lifecycle.Runner: the configured targets are loaded,
// then reloaded at every change; the delivery to the targets starts with
// the first one (a node without audit target spends nothing on them).
func (s *service) Start(ctx context.Context) error {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if s.cancel != nil || s.deps.Targets == nil {
		return nil
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.cancel, s.done = cancel, make(chan struct{})
	if s.deps.Config == nil {
		close(s.done)
		return nil
	}
	changes := s.deps.Config.Watch(runCtx)
	s.reload(ctx)
	go func(done chan<- struct{}) {
		defer close(done)
		for c := range changes {
			if c.Subsystem == "" || IsTargetSubsystem(c.Subsystem) {
				s.reload(runCtx)
			}
		}
	}(s.done)
	return nil
}

// Stop implements lifecycle.Runner: the queued entries stay in the queues
// of the targets for the next start.
func (s *service) Stop(ctx context.Context) error {
	s.runMu.Lock()
	cancel, done := s.cancel, s.done
	s.cancel, s.done = nil, nil
	s.runMu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
	case <-ctx.Done():
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()
	if !s.delivering {
		return nil
	}
	s.delivering = false
	s.targets.Store(false)
	return s.deps.Targets.Stop(ctx)
}

// reload applies the audit_* subsystems: the enabled targets are created
// or replaced when their configuration changed, the others are removed
// (their queue is kept).
func (s *service) reload(ctx context.Context) {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	subsystems, err := s.deps.Config.List(ctx)
	if err != nil {
		s.deps.Logger.WarnContext(ctx, "audit: cannot list the configured targets", "err", err)
		return
	}
	want := map[event.TargetID]event.TargetConfig{}
	for _, sub := range subsystems {
		notify, ok := notifySubsystem(sub)
		if !ok {
			continue
		}
		kv, err := s.deps.Config.Get(ctx, sub)
		if err != nil {
			s.deps.Logger.WarnContext(ctx, "audit: cannot read a target configuration", "subsystem", sub, "err", err)
			continue
		}
		cfg, err := event.ParseTargetConfig(notify, kv)
		if err != nil {
			s.deps.Logger.WarnContext(ctx, "audit: invalid target configuration ignored", "subsystem", sub,
				"err", strings.Replace(err.Error(), notify, sub, 1))
			continue
		}
		if cfg.Enabled {
			cfg.Filter = event.Filter{Events: []event.Name{EventName}}
			want[cfg.TargetID()] = cfg
		}
	}
	if len(want) > 0 && !s.delivering {
		if err := s.deps.Targets.Start(ctx); err != nil {
			s.deps.Logger.WarnContext(ctx, "audit: cannot start the delivery to the targets", "err", err)
			return
		}
		s.delivering = true
	}
	for id, cfg := range want {
		if cur, ok := s.fromConfig[id]; ok && reflect.DeepEqual(cur, cfg) {
			continue
		}
		if err := s.deps.Targets.SetTarget(ctx, cfg); err != nil {
			s.deps.Logger.WarnContext(ctx, "audit: cannot configure a target", "target", id.String(), "err", err)
			continue
		}
		s.fromConfig[id] = cfg
		s.deps.Logger.InfoContext(ctx, "audit: target configured", "target", id.String())
	}
	for id := range s.fromConfig {
		if _, ok := want[id]; ok {
			continue
		}
		delete(s.fromConfig, id)
		if err := s.deps.Targets.RemoveTarget(ctx, id); err != nil && !errors.Is(err, domain.ErrNotFound) {
			s.deps.Logger.WarnContext(ctx, "audit: cannot remove a target", "target", id.String(), "err", err)
		}
	}
	s.targets.Store(len(s.fromConfig) > 0)
}
