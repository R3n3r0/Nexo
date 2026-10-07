package config

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"sync"

	"github.com/r3n3r0/nexo/internal/domain"
)

// Deps are the dependencies of the configuration service.
type Deps struct {
	Store  Store
	Logger *slog.Logger
}

// Option configures the service.
type Option func(*service)

// WithWatchBuffer sets the buffer size of Watch channels (default 16).
func WithWatchBuffer(n int) Option {
	return func(s *service) {
		if n > 0 {
			s.watchBuffer = n
		}
	}
}

// service caches the configuration in memory and writes through the Store.
type service struct {
	deps        Deps
	watchBuffer int

	mu       sync.RWMutex
	loaded   bool
	data     map[string]KV
	watchers map[chan Change]struct{}
}

var _ Service = (*service)(nil)

// New returns a configuration service backed by deps.Store.
func New(deps Deps, opts ...Option) Service {
	if deps.Store == nil {
		deps.Store = NewMemoryStore(nil)
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	s := &service{deps: deps, watchBuffer: 16, watchers: make(map[chan Change]struct{})}
	for _, o := range opts {
		o(s)
	}
	return s
}

func (s *service) ensureLoaded(ctx context.Context) error {
	s.mu.RLock()
	loaded := s.loaded
	s.mu.RUnlock()
	if loaded {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loaded {
		return nil
	}
	data, err := s.deps.Store.Load(ctx)
	if err != nil {
		return fmt.Errorf("config: load: %w", err)
	}
	if data == nil {
		data = make(map[string]KV)
	}
	s.data = data
	s.loaded = true
	return nil
}

// Get implements Service.
func (s *service) Get(ctx context.Context, subsystem string) (KV, error) {
	if err := s.ensureLoaded(ctx); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	kv := s.data[subsystem].Clone()
	if kv == nil {
		kv = KV{}
	}
	return kv, nil
}

// Set implements Service.
func (s *service) Set(ctx context.Context, subsystem string, kv KV) error {
	if subsystem == "" {
		return fmt.Errorf("%w: empty subsystem", domain.ErrInvalidArgument)
	}
	if err := s.ensureLoaded(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := cloneAll(s.data)
	cur := next[subsystem]
	if cur == nil {
		cur = KV{}
	}
	keys := make([]string, 0, len(kv))
	for k, v := range kv {
		cur[k] = v
		keys = append(keys, k)
	}
	sort.Strings(keys)
	next[subsystem] = cur
	if err := s.deps.Store.Save(ctx, next); err != nil {
		return fmt.Errorf("config: save %s: %w", subsystem, err)
	}
	s.data = next
	s.notifyLocked(Change{Subsystem: subsystem, Keys: keys})
	return nil
}

// Delete implements Service.
func (s *service) Delete(ctx context.Context, subsystem string, keys ...string) error {
	if err := s.ensureLoaded(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[subsystem]; !ok {
		return nil
	}
	next := cloneAll(s.data)
	if len(keys) == 0 {
		delete(next, subsystem)
	} else {
		for _, k := range keys {
			delete(next[subsystem], k)
		}
		if len(next[subsystem]) == 0 {
			delete(next, subsystem)
		}
	}
	if err := s.deps.Store.Save(ctx, next); err != nil {
		return fmt.Errorf("config: delete %s: %w", subsystem, err)
	}
	s.data = next
	s.notifyLocked(Change{Subsystem: subsystem, Deleted: true, Keys: keys})
	return nil
}

// List implements Service.
func (s *service) List(ctx context.Context) ([]string, error) {
	if err := s.ensureLoaded(ctx); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := make([]string, 0, len(s.data))
	for k := range s.data {
		names = append(names, k)
	}
	sort.Strings(names)
	return names, nil
}

// Export implements Service.
func (s *service) Export(ctx context.Context) ([]byte, error) {
	if err := s.ensureLoaded(ctx); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("config: export: %w", err)
	}
	return b, nil
}

// Import implements Service.
func (s *service) Import(ctx context.Context, data []byte) error {
	var all map[string]KV
	if err := json.Unmarshal(data, &all); err != nil {
		return fmt.Errorf("%w: config import: %v", domain.ErrInvalidArgument, err)
	}
	if all == nil {
		all = make(map[string]KV)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.deps.Store.Save(ctx, all); err != nil {
		return fmt.Errorf("config: import: %w", err)
	}
	s.data = cloneAll(all)
	s.loaded = true
	for name := range all {
		s.notifyLocked(Change{Subsystem: name})
	}
	return nil
}

// Watch implements Service.
func (s *service) Watch(ctx context.Context) <-chan Change {
	ch := make(chan Change, s.watchBuffer)
	s.mu.Lock()
	s.watchers[ch] = struct{}{}
	s.mu.Unlock()
	go func() {
		<-ctx.Done()
		s.mu.Lock()
		delete(s.watchers, ch)
		s.mu.Unlock()
		close(ch)
	}()
	return ch
}

// notifyLocked delivers a change to every watcher without blocking.
func (s *service) notifyLocked(c Change) {
	for ch := range s.watchers {
		select {
		case ch <- c:
		default:
			s.deps.Logger.Warn("config: watcher is slow, change dropped", "subsystem", c.Subsystem)
		}
	}
}
