package lock

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
)

// Default lease of the lock server entries (docs/03 §3.6): an entry that is
// not refreshed for a minute is an orphan whose holder died.
const (
	DefaultStaleAfter   = time.Minute
	defaultReapInterval = 15 * time.Second
)

// ServerDeps are the dependencies of the in-memory lock server. Every field
// is optional.
type ServerDeps struct {
	// Clock dates the entries and decides their expiry (default wall clock).
	Clock  clock.Clock
	Logger *slog.Logger
}

// ServerOption configures the in-memory lock server.
type ServerOption func(*serverOptions)

type serverOptions struct {
	staleAfter   time.Duration
	reapInterval time.Duration
}

// WithStaleAfter sets how long an entry survives without refresh before it
// expires (default DefaultStaleAfter).
func WithStaleAfter(d time.Duration) ServerOption {
	return func(o *serverOptions) {
		if d > 0 {
			o.staleAfter = d
		}
	}
}

// WithReapInterval sets how often the server sweeps every entry for orphans
// (default 15s). Entries of a resource are also checked whenever the
// resource is locked, so the sweep only bounds the memory of abandoned
// resources.
func WithReapInterval(d time.Duration) ServerOption {
	return func(o *serverOptions) {
		if d > 0 {
			o.reapInterval = d
		}
	}
}

// server is the in-memory Server: resource name → granted entries (one
// writer, or any number of readers). It runs no goroutine: orphans are
// expired lazily, on the resources an operation touches and by a sweep of
// the whole table at most every reapInterval.
type server struct {
	deps ServerDeps
	opts serverOptions

	mu        sync.Mutex
	resources map[string][]*serverEntry
	lastReap  time.Time
}

type serverEntry struct {
	uid       string
	owner     string
	source    string
	writer    bool
	quorum    int
	since     time.Time
	refreshed time.Time
}

var _ Server = (*server)(nil)

// NewServer returns the in-memory lock server of a node.
func NewServer(deps ServerDeps, opts ...ServerOption) Server {
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	o := serverOptions{staleAfter: DefaultStaleAfter, reapInterval: defaultReapInterval}
	for _, opt := range opts {
		opt(&o)
	}
	return &server{deps: deps, opts: o, resources: make(map[string][]*serverEntry), lastReap: deps.Clock.Now()}
}

// Lock implements Server.
func (s *server) Lock(ctx context.Context, args Args) (bool, error) {
	return s.grant(ctx, args, true)
}

// RLock implements Server.
func (s *server) RLock(ctx context.Context, args Args) (bool, error) {
	return s.grant(ctx, args, false)
}

func (s *server) grant(ctx context.Context, args Args, writer bool) (bool, error) {
	if err := validateArgs(ctx, args, true); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.deps.Clock.Now()
	s.reapLocked(ctx, now)
	held := 0
	for _, res := range args.Resources {
		entries := s.expireLocked(ctx, res, now)
		switch {
		case hasUID(entries, args.UID, writer):
			held++ // retried call of an acquisition already granted
		case writer && len(entries) > 0, !writer && hasWriter(entries):
			return false, nil
		}
	}
	if held == len(args.Resources) {
		s.touchLocked(args, now)
		return true, nil
	}
	for _, res := range args.Resources {
		if hasUID(s.resources[res], args.UID, writer) {
			continue
		}
		s.resources[res] = append(s.resources[res], &serverEntry{
			uid: args.UID, owner: args.Owner, source: args.Source, writer: writer,
			quorum: args.Quorum, since: now, refreshed: now,
		})
	}
	return true, nil
}

// Unlock implements Server.
func (s *server) Unlock(ctx context.Context, args Args) (bool, error) {
	return s.release(ctx, args, true)
}

// RUnlock implements Server.
func (s *server) RUnlock(ctx context.Context, args Args) (bool, error) {
	return s.release(ctx, args, false)
}

func (s *server) release(ctx context.Context, args Args, writer bool) (bool, error) {
	if err := validateArgs(ctx, args, true); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	released := false
	for _, res := range args.Resources {
		if s.removeLocked(res, func(e *serverEntry) bool { return e.uid == args.UID && e.writer == writer }) {
			released = true
		}
	}
	return released, nil
}

// Refresh implements Server.
func (s *server) Refresh(ctx context.Context, args Args) (bool, error) {
	if err := validateArgs(ctx, args, false); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.deps.Clock.Now()
	resources := args.Resources
	if len(resources) == 0 {
		resources = s.namesLocked()
	}
	found := false
	for _, res := range resources {
		for _, e := range s.expireLocked(ctx, res, now) {
			if e.uid == args.UID {
				e.refreshed = now
				found = true
			}
		}
	}
	return found, nil
}

// ForceUnlock implements Server.
func (s *server) ForceUnlock(ctx context.Context, args Args) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if len(args.Resources) == 0 && args.UID == "" {
		return false, fmt.Errorf("%w: lock: force unlock needs resources or a uid", domain.ErrInvalidArgument)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := false
	if len(args.Resources) > 0 {
		for _, res := range args.Resources {
			if len(s.resources[res]) > 0 {
				removed = true
			}
			delete(s.resources, res)
		}
	} else {
		for _, res := range s.namesLocked() {
			if s.removeLocked(res, func(e *serverEntry) bool { return e.uid == args.UID }) {
				removed = true
			}
		}
	}
	if removed {
		s.deps.Logger.LogAttrs(ctx, slog.LevelWarn, "lock: entries force-unlocked",
			slog.Any("resources", args.Resources), slog.String("uid", args.UID))
	}
	return removed, nil
}

// List implements Server.
func (s *server) List(ctx context.Context) ([]Info, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.deps.Clock.Now()
	var out []Info
	for _, res := range s.namesLocked() {
		for _, e := range s.expireLocked(ctx, res, now) {
			out = append(out, Info{
				Resource: res, UID: e.uid, Owner: e.owner, Source: e.source, Writer: e.writer,
				Quorum: e.quorum, Since: e.since, Refreshed: e.refreshed,
			})
		}
	}
	slices.SortFunc(out, func(a, b Info) int {
		return cmp.Or(cmp.Compare(a.Resource, b.Resource), cmp.Compare(a.UID, b.UID))
	})
	return out, nil
}

// touchLocked refreshes the entries of an acquisition granted again.
func (s *server) touchLocked(args Args, now time.Time) {
	for _, res := range args.Resources {
		for _, e := range s.resources[res] {
			if e.uid == args.UID {
				e.refreshed = now
			}
		}
	}
}

// expireLocked drops the orphan entries of res and returns the others.
func (s *server) expireLocked(ctx context.Context, res string, now time.Time) []*serverEntry {
	s.removeLocked(res, func(e *serverEntry) bool {
		if now.Sub(e.refreshed) <= s.opts.staleAfter {
			return false
		}
		s.deps.Logger.LogAttrs(ctx, slog.LevelWarn, "lock: orphan lock expired",
			slog.String("resource", res), slog.String("uid", e.uid), slog.String("owner", e.owner),
			slog.Bool("writer", e.writer), slog.Duration("idle", now.Sub(e.refreshed)))
		return true
	})
	return s.resources[res]
}

// reapLocked sweeps every resource for orphans at most every reapInterval.
func (s *server) reapLocked(ctx context.Context, now time.Time) {
	if now.Sub(s.lastReap) < s.opts.reapInterval {
		return
	}
	s.lastReap = now
	for _, res := range s.namesLocked() {
		s.expireLocked(ctx, res, now)
	}
}

// removeLocked removes the entries of res matching drop and reports whether
// any was removed. Resources without entries leave the table.
func (s *server) removeLocked(res string, drop func(*serverEntry) bool) bool {
	entries, ok := s.resources[res]
	if !ok {
		return false
	}
	kept := entries[:0]
	for _, e := range entries {
		if !drop(e) {
			kept = append(kept, e)
		}
	}
	removed := len(kept) != len(entries)
	clear(entries[len(kept):])
	if len(kept) == 0 {
		delete(s.resources, res)
	} else {
		s.resources[res] = kept
	}
	return removed
}

func (s *server) namesLocked() []string {
	names := make([]string, 0, len(s.resources))
	for res := range s.resources {
		names = append(names, res)
	}
	slices.Sort(names)
	return names
}

func hasUID(entries []*serverEntry, uid string, writer bool) bool {
	for _, e := range entries {
		if e.uid == uid && e.writer == writer {
			return true
		}
	}
	return false
}

func hasWriter(entries []*serverEntry) bool {
	for _, e := range entries {
		if e.writer {
			return true
		}
	}
	return false
}

// validateArgs checks the context and the identity of an acquisition.
func validateArgs(ctx context.Context, args Args, needResources bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if args.UID == "" {
		return fmt.Errorf("%w: lock: empty uid", domain.ErrInvalidArgument)
	}
	if needResources && len(args.Resources) == 0 {
		return fmt.Errorf("%w: lock: no resource", domain.ErrInvalidArgument)
	}
	for _, res := range args.Resources {
		if res == "" {
			return fmt.Errorf("%w: lock: empty resource name", domain.ErrInvalidArgument)
		}
	}
	return nil
}
