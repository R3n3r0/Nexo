package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
)

// queued is a job of the queue with its delivery state.
type queued struct {
	Job      domain.MediaJob `json:"job"`
	Attempts int             `json:"attempts"`
	// NotBefore delays a retry.
	NotBefore time.Time `json:"notBefore,omitzero"`
	Error     string    `json:"error,omitempty"`

	name string // file name
	id   string
}

// queue keeps the jobs in a directory, one file each, until they are done:
// a job acknowledged to Nexo survives a restart of the worker. The jobs of
// one version of a video are serialized; a newer job replaces the pending
// one of the same version (the latest wins).
type queue struct {
	dir string
	mu  sync.Mutex
	// running are the ids of the versions being processed.
	running map[string]bool
	wake    chan struct{}
	seq     uint64
	now     func() time.Time
}

func newQueue(dir string) (*queue, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("worker: queue directory: %w", err)
	}
	return &queue{dir: dir, running: map[string]bool{}, wake: make(chan struct{}, 1), now: time.Now}, nil
}

// jobID identifies the version a job is about.
func jobID(j domain.MediaJob) string {
	sum := sha256.Sum256([]byte(j.Bucket + "\x00" + j.Key + "\x00" + j.VersionID))
	return hex.EncodeToString(sum[:12])
}

// push stores a job durably, replacing a pending job of the same version.
func (q *queue) push(j domain.MediaJob) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	id := jobID(j)
	q.seq++
	name := fmt.Sprintf("%s-%020d-%d.json", id, q.now().UnixNano(), q.seq)
	if err := q.write(name, queued{Job: j}); err != nil {
		return err
	}
	if !q.running[id] {
		for _, old := range q.files(id) {
			if old != name {
				_ = os.Remove(filepath.Join(q.dir, old))
			}
		}
	}
	syncDir(q.dir)
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return nil
}

// write writes an entry atomically and durably; mu is held.
func (q *queue) write(name string, item queued) error {
	data, err := json.Marshal(item)
	if err != nil {
		return err
	}
	tmp := filepath.Join(q.dir, "."+name+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) // #nosec G304 -- a file of the queue
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("worker: queue a job: %w", err)
	}
	return os.Rename(tmp, filepath.Join(q.dir, name))
}

// files lists the entries of a version, oldest first; mu is held.
func (q *queue) files(id string) []string {
	entries, _ := os.ReadDir(q.dir)
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), id+"-") && strings.HasSuffix(e.Name(), ".json") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// next takes the oldest job ready to run whose version is not being
// processed; false when none.
func (q *queue) next() (*queued, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	entries, err := os.ReadDir(q.dir)
	if err != nil {
		return nil, false
	}
	var best *queued
	var bestAt int64
	now := q.now()
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			continue
		}
		id, rest, ok := strings.Cut(strings.TrimSuffix(name, ".json"), "-")
		if !ok || q.running[id] {
			continue
		}
		at, _ := strconv.ParseInt(strings.SplitN(rest, "-", 2)[0], 10, 64)
		if best != nil && at >= bestAt {
			continue
		}
		data, err := os.ReadFile(filepath.Join(q.dir, name)) // #nosec G304 -- a file of the queue
		if err != nil {
			continue
		}
		var item queued
		if json.Unmarshal(data, &item) != nil {
			_ = os.Remove(filepath.Join(q.dir, name)) // unreadable: dropped
			continue
		}
		if item.NotBefore.After(now) {
			continue
		}
		item.name, item.id = name, id
		best, bestAt = &item, at
	}
	if best != nil {
		q.running[best.id] = true
	}
	return best, best != nil
}

// done removes a processed job.
func (q *queue) done(item *queued) {
	q.mu.Lock()
	defer q.mu.Unlock()
	_ = os.Remove(filepath.Join(q.dir, item.name))
	syncDir(q.dir)
	delete(q.running, item.id)
	q.signal()
}

// release leaves a job queued as it was, for the next start.
func (q *queue) release(item *queued) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.running, item.id)
}

// retry keeps a failed job for another attempt after delay.
func (q *queue) retry(item *queued, delay time.Duration, cause error) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	defer delete(q.running, item.id)
	item.Attempts++
	item.NotBefore = q.now().Add(delay)
	item.Error = cause.Error()
	return q.write(item.name, *item)
}

// pending counts the jobs of the queue.
func (q *queue) pending() int {
	entries, _ := os.ReadDir(q.dir)
	n := 0
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".") && strings.HasSuffix(e.Name(), ".json") {
			n++
		}
	}
	return n
}

// signal wakes a waiting runner; mu is held or not needed.
func (q *queue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// syncDir makes the entries of a directory durable (best effort).
func syncDir(dir string) {
	d, err := os.Open(dir) // #nosec G304 -- the queue directory
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
