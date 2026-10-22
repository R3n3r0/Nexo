package replication

import (
	"container/heap"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// op is the kind of a task.
type op string

// Tasks: a version to send (its targets from its replication state, or
// Target), a delete marker, a permanent deletion of a version, the tags
// of a version.
const (
	opObject       op = "object"
	opDeleteMarker op = "marker"
	opDelete       op = "delete"
	opTags         op = "tags"
)

// task is one replication task. The tasks of a key run one at a time, in
// the order they were queued.
type task struct {
	seq       uint64
	Op        op        `json:"op"`
	Bucket    string    `json:"bucket"`
	Key       string    `json:"key"`
	VersionID string    `json:"versionId,omitempty"`
	ModTime   time.Time `json:"modTime,omitzero"`
	// Target is the only destination of the task ("" for the targets the
	// version records).
	Target string `json:"target,omitempty"`
	// Resync tasks first check whether the destination has the version.
	Resync    bool      `json:"resync,omitempty"`
	Attempts  int       `json:"attempts,omitempty"`
	NotBefore time.Time `json:"notBefore,omitzero"`
	Queued    time.Time `json:"queued"`
}

func (t *task) keyID() string { return t.Bucket + "/" + t.Key }

// errQueueFull is a task refused because the queue holds its maximum.
var errQueueFull = errors.New("replication: the queue is full")

// queue holds the tasks of this node in memory, and in one file each in
// dir when dir is set (they survive a restart). Every method is safe for
// concurrent use.
type queue struct {
	dir   string
	limit int

	mu    sync.Mutex
	seq   uint64
	tasks map[uint64]*task
	// keys are the tasks of every key, oldest first; ready holds the keys
	// whose oldest task is not running, by its NotBefore.
	keys    map[string][]uint64
	ready   readyHeap
	running map[string]bool
	wake    chan struct{}
}

// openQueue opens the queue, resuming the tasks of dir ("" keeps them in
// memory only).
func openQueue(dir string, limit int) (*queue, error) {
	q := &queue{dir: dir, limit: limit, tasks: map[uint64]*task{}, keys: map[string][]uint64{}, running: map[string]bool{}, wake: make(chan struct{}, 1)}
	if dir == "" {
		return q, nil
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("replication: queue: %w", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("replication: queue: %w", err)
	}
	var names []string
	for _, e := range entries {
		name := e.Name()
		switch {
		case strings.HasSuffix(name, ".tmp"):
			_ = os.Remove(filepath.Join(dir, name))
		case strings.HasSuffix(name, ".json"):
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		seq, err := strconv.ParseUint(strings.TrimSuffix(name, ".json"), 10, 64)
		if err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name)) // #nosec G304 -- a file of the queue directory
		if err != nil {
			return nil, fmt.Errorf("replication: queue: %w", err)
		}
		t := &task{}
		if err := json.Unmarshal(data, t); err != nil {
			// A damaged task is dropped: the version stays PENDING for a
			// resync.
			_ = os.Remove(filepath.Join(dir, name))
			continue
		}
		t.seq = seq
		if seq > q.seq {
			q.seq = seq
		}
		q.add(t)
	}
	return q, nil
}

func (q *queue) path(seq uint64) string {
	return filepath.Join(q.dir, fmt.Sprintf("%020d.json", seq))
}

// write persists t (atomically: a temporary file renamed).
func (q *queue) write(t *task) error {
	if q.dir == "" {
		return nil
	}
	data, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("replication: queue: %w", err)
	}
	p := q.path(t.seq)
	if err := os.WriteFile(p+".tmp", data, 0o600); err != nil {
		return fmt.Errorf("replication: queue: %w", err)
	}
	if err := os.Rename(p+".tmp", p); err != nil {
		_ = os.Remove(p + ".tmp")
		return fmt.Errorf("replication: queue: %w", err)
	}
	return nil
}

// add indexes t; q.mu is held (or the queue is being opened).
func (q *queue) add(t *task) {
	q.tasks[t.seq] = t
	id := t.keyID()
	q.keys[id] = append(q.keys[id], t.seq)
	if len(q.keys[id]) == 1 && !q.running[id] {
		heap.Push(&q.ready, readyKey{id: id, at: t.NotBefore, seq: t.seq})
	}
}

// Put queues a task.
func (q *queue) Put(t task) error {
	q.mu.Lock()
	if q.limit > 0 && len(q.tasks) >= q.limit {
		q.mu.Unlock()
		return errQueueFull
	}
	q.seq++
	n := t
	n.seq = q.seq
	if err := q.write(&n); err != nil {
		q.seq--
		q.mu.Unlock()
		return err
	}
	q.add(&n)
	q.mu.Unlock()
	q.signal()
	return nil
}

func (q *queue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// Next returns the oldest task of a key that is due at now and whose key
// has no task running, marking its key running; ok is false when none is
// due, with the time the next one is (zero: none queued).
func (q *queue) Next(now time.Time) (t task, ok bool, next time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for q.ready.Len() > 0 {
		head := q.ready[0]
		seqs := q.keys[head.id]
		if len(seqs) == 0 || seqs[0] != head.seq || q.running[head.id] {
			heap.Pop(&q.ready) // stale entry
			continue
		}
		if head.at.After(now) {
			return task{}, false, head.at
		}
		heap.Pop(&q.ready)
		q.running[head.id] = true
		return *q.tasks[head.seq], true, time.Time{}
	}
	return task{}, false, time.Time{}
}

// Done removes a finished task and frees its key.
func (q *queue) Done(t task) {
	q.mu.Lock()
	id := t.keyID()
	delete(q.tasks, t.seq)
	if seqs := q.keys[id]; len(seqs) > 0 && seqs[0] == t.seq {
		q.keys[id] = seqs[1:]
	}
	if q.dir != "" {
		_ = os.Remove(q.path(t.seq))
	}
	q.release(id)
	q.mu.Unlock()
	q.signal()
}

// Retry keeps a failed task at the head of its key, due at notBefore.
func (q *queue) Retry(t task, notBefore time.Time) {
	q.mu.Lock()
	if cur, ok := q.tasks[t.seq]; ok {
		cur.Attempts = t.Attempts
		cur.NotBefore = notBefore
		_ = q.write(cur)
	}
	q.release(t.keyID())
	q.mu.Unlock()
	q.signal()
}

// release frees a key and readies its next task; q.mu is held.
func (q *queue) release(id string) {
	delete(q.running, id)
	seqs := q.keys[id]
	if len(seqs) == 0 {
		delete(q.keys, id)
		return
	}
	head := q.tasks[seqs[0]]
	heap.Push(&q.ready, readyKey{id: id, at: head.NotBefore, seq: head.seq})
}

// Count returns the tasks queued for bucket: in all and per target (""
// for the tasks without one).
func (q *queue) Count(bucket string) (int, map[string]int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := 0
	per := map[string]int{}
	for _, t := range q.tasks {
		if t.Bucket == bucket {
			n++
			per[t.Target]++
		}
	}
	return n, per
}

// Len returns the number of queued tasks.
func (q *queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.tasks)
}

// DropBucket removes the tasks of bucket that are not running.
func (q *queue) DropBucket(bucket string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for seq, t := range q.tasks {
		if t.Bucket != bucket || q.running[t.keyID()] {
			continue
		}
		delete(q.tasks, seq)
		if q.dir != "" {
			_ = os.Remove(q.path(seq))
		}
		id := t.keyID()
		q.keys[id] = removeSeq(q.keys[id], seq)
		if len(q.keys[id]) == 0 {
			delete(q.keys, id)
		}
	}
}

func removeSeq(seqs []uint64, seq uint64) []uint64 {
	for i, s := range seqs {
		if s == seq {
			return append(seqs[:i:i], seqs[i+1:]...)
		}
	}
	return seqs
}

// readyKey is a key whose oldest task can run from at.
type readyKey struct {
	id  string
	at  time.Time
	seq uint64
}

// readyHeap orders the keys by the time their task is due, then by age.
type readyHeap []readyKey

func (h readyHeap) Len() int { return len(h) }
func (h readyHeap) Less(i, j int) bool {
	if !h[i].at.Equal(h[j].at) {
		return h[i].at.Before(h[j].at)
	}
	return h[i].seq < h[j].seq
}
func (h readyHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *readyHeap) Push(x any)   { *h = append(*h, x.(readyKey)) }
func (h *readyHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}
