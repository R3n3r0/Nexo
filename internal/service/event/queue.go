package event

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/r3n3r0/nexo/internal/domain"
)

// Files of a directory queue: one "<sequence>.event" JSON file per event,
// written to "<name>.tmp" and renamed into place.
const (
	queueFileSuffix = ".event"
	queueTmpSuffix  = ".tmp"
	queueSeqDigits  = 20
)

// queueStore is the queue of the events of one target, oldest first. The
// dispatcher appends, the sender of the target reads, delivers and
// deletes: the events stay queued until they are delivered.
type queueStore interface {
	// Put appends an event; it fails with domain.ErrStorageFull when the
	// queue holds its limit.
	Put(ctx context.Context, e Event) error
	// Keys returns the keys of the queued events, oldest first.
	Keys(ctx context.Context) []string
	// Get reads a queued event.
	Get(ctx context.Context, key string) (Event, error)
	// Delete removes a delivered (or unreadable) event.
	Delete(ctx context.Context, key string) error
}

// dirQueue keeps the events in files of a local directory: they survive a
// restart of the process.
type dirQueue struct {
	dir   string
	limit int

	mu   sync.Mutex
	keys []string
	next uint64
}

var _ queueStore = (*dirQueue)(nil)

// openDirQueue opens (creating it) the queue directory dir, resuming the
// events it holds; the temporary files of interrupted writes are removed.
func openDirQueue(dir string, limit int) (*dirQueue, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("event: queue directory %s: %w", dir, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("event: queue directory %s: %w", dir, err)
	}
	q := &dirQueue{dir: dir, limit: limit, next: 1}
	for _, e := range entries {
		name := e.Name()
		switch {
		case e.IsDir():
		case strings.HasSuffix(name, queueTmpSuffix):
			_ = os.Remove(filepath.Join(dir, name))
		case strings.HasSuffix(name, queueFileSuffix):
			seq, err := strconv.ParseUint(strings.TrimSuffix(name, queueFileSuffix), 10, 64)
			if err != nil {
				continue
			}
			q.keys = append(q.keys, name)
			if seq >= q.next {
				q.next = seq + 1
			}
		}
	}
	slices.Sort(q.keys)
	return q, nil
}

// Put implements queueStore.
func (q *dirQueue) Put(_ context.Context, e Event) error {
	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("event: encode queued event: %w", err)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.limit > 0 && len(q.keys) >= q.limit {
		return fmt.Errorf("event: queue %s holds %d events: %w", q.dir, q.limit, domain.ErrStorageFull)
	}
	name := fmt.Sprintf("%0*d%s", queueSeqDigits, q.next, queueFileSuffix)
	tmp := filepath.Join(q.dir, name+queueTmpSuffix)
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("event: queue event: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(q.dir, name)); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("event: queue event: %w", err)
	}
	q.next++
	q.keys = append(q.keys, name)
	return nil
}

// Keys implements queueStore.
func (q *dirQueue) Keys(context.Context) []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return slices.Clone(q.keys)
}

// Get implements queueStore.
func (q *dirQueue) Get(_ context.Context, key string) (Event, error) {
	data, err := os.ReadFile(filepath.Join(q.dir, filepath.Base(key)))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Event{}, fmt.Errorf("event: queued event %s: %w", key, domain.ErrNotFound)
		}
		return Event{}, fmt.Errorf("event: read queued event %s: %w", key, err)
	}
	var e Event
	if err := json.Unmarshal(data, &e); err != nil {
		return Event{}, fmt.Errorf("event: queued event %s: %w: %v", key, domain.ErrInvalidFormat, err)
	}
	return e, nil
}

// Delete implements queueStore.
func (q *dirQueue) Delete(_ context.Context, key string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if err := os.Remove(filepath.Join(q.dir, filepath.Base(key))); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("event: remove queued event %s: %w", key, err)
	}
	if i := slices.Index(q.keys, key); i >= 0 {
		q.keys = slices.Delete(q.keys, i, i+1)
	}
	return nil
}

// memQueue keeps the events in memory: they are lost with the process.
type memQueue struct {
	limit int

	mu    sync.Mutex
	keys  []string
	items map[string]Event
	next  uint64
}

var _ queueStore = (*memQueue)(nil)

func newMemQueue(limit int) *memQueue {
	return &memQueue{limit: limit, items: map[string]Event{}, next: 1}
}

// Put implements queueStore.
func (q *memQueue) Put(_ context.Context, e Event) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.limit > 0 && len(q.keys) >= q.limit {
		return fmt.Errorf("event: memory queue holds %d events: %w", q.limit, domain.ErrStorageFull)
	}
	key := fmt.Sprintf("%0*d", queueSeqDigits, q.next)
	q.next++
	q.keys = append(q.keys, key)
	q.items[key] = e
	return nil
}

// Keys implements queueStore.
func (q *memQueue) Keys(context.Context) []string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return slices.Clone(q.keys)
}

// Get implements queueStore.
func (q *memQueue) Get(_ context.Context, key string) (Event, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	e, ok := q.items[key]
	if !ok {
		return Event{}, fmt.Errorf("event: queued event %s: %w", key, domain.ErrNotFound)
	}
	return e, nil
}

// Delete implements queueStore.
func (q *memQueue) Delete(_ context.Context, key string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.items, key)
	if i := slices.Index(q.keys, key); i >= 0 {
		q.keys = slices.Delete(q.keys, i, i+1)
	}
	return nil
}
