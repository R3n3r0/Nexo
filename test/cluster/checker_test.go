//go:build cluster

package cluster_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/require"
)

// The workload of the chaos tests is deterministic: a seed (logged, and
// replayed with NEXO_CLUSTER_SEED) gives every worker the same sequence
// of operations and contents. Every key belongs to one worker, which runs
// its operations one after the other: the ledger knows the exact state a
// confirmed operation leaves, so that every read is checked at once
// (read-after-write) and the final state of every key is checked against
// it (key → sha256 of the content, or absent).

// Sizes of the objects of the workload: inline in xl.meta, one block,
// several blocks of the erasure coding.
var workloadSizes = []int{0, 1, 1 << 10, 100 << 10, 300 << 10, 1<<20 + 7, 2<<20 + 513}

// seedOf returns the seed of a test: NEXO_CLUSTER_SEED, or a random one.
// It is logged so that a failure can be replayed.
func seedOf(t *testing.T) uint64 {
	t.Helper()
	seed := rand.Uint64()
	if v := os.Getenv("NEXO_CLUSTER_SEED"); v != "" {
		s, err := strconv.ParseUint(v, 10, 64)
		require.NoError(t, err, "NEXO_CLUSTER_SEED")
		seed = s
	}
	t.Logf("workload seed %d (replay with NEXO_CLUSTER_SEED=%d)", seed, seed)
	return seed
}

// content returns the deterministic content of an operation.
func content(rng *rand.Rand) []byte {
	size := workloadSizes[rng.IntN(len(workloadSizes))]
	data := make([]byte, size)
	for i := 0; i < size; i += 8 {
		v := rng.Uint64()
		for j := 0; j < 8 && i+j < size; j++ {
			data[i+j] = byte(v >> (8 * j))
		}
	}
	return data
}

// sum is the sha256 of data, hex.
func sum(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// state is the state of a key: absent, or present with the sha256 of its
// content.
type state struct {
	present bool
	sum     string
}

func (s state) String() string {
	if !s.present {
		return "absent"
	}
	return s.sum[:12]
}

// keyLedger is what the confirmed operations say about a key: its state
// after the last confirmed operation, plus the states of the operations
// that failed since then (their outcome is unknown: the cluster may or may
// not have applied them).
type keyLedger struct {
	confirmed state
	// since is when the operation that left the confirmed state started:
	// a drive holding the key since then holds that state (or a later
	// one).
	since     time.Time
	uncertain []state
}

// allowed returns the states the key may be in.
func (k *keyLedger) allowed() []state {
	return append([]state{k.confirmed}, k.uncertain...)
}

// ledger records the operations of a workload and checks the reads.
type ledger struct {
	bucket string
	mu     sync.Mutex
	keys   map[string]*keyLedger
	ops    int
	// errs are the operations that failed for the client (after its
	// retries); violations the reads that contradicted the ledger.
	errs       []string
	violations []string
}

func newLedger(bucket string) *ledger {
	return &ledger{bucket: bucket, keys: map[string]*keyLedger{}}
}

func (l *ledger) key(k string) *keyLedger {
	kl, ok := l.keys[k]
	if !ok {
		kl = &keyLedger{}
		l.keys[k] = kl
	}
	return kl
}

// write records a put (present) or a delete (absent) started at start
// and its outcome.
func (l *ledger) write(op, key string, s state, start time.Time, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ops++
	kl := l.key(key)
	if err != nil {
		l.errs = append(l.errs, fmt.Sprintf("%s %s: %v", op, key, err))
		kl.uncertain = append(kl.uncertain, s)
		return
	}
	kl.confirmed, kl.since, kl.uncertain = s, start, nil
}

// read checks a read against the ledger.
func (l *ledger) read(key string, got state, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ops++
	if err != nil {
		l.errs = append(l.errs, fmt.Sprintf("get %s: %v", key, err))
		return
	}
	kl := l.key(key)
	if !slices.Contains(kl.allowed(), got) {
		l.violations = append(l.violations, fmt.Sprintf("get %s: read %s, the ledger allows %v", key, got, kl.allowed()))
	}
}

// report returns the number of operations, the client errors and the
// consistency violations.
func (l *ledger) report() (ops int, errs, violations []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ops, slices.Clone(l.errs), slices.Clone(l.violations)
}

// verify reads every key of the ledger through c and checks its final
// state (the checker of docs/06 §6.3); it returns the mismatches.
func (l *ledger) verify(ctx context.Context, c *s3.Client) []string {
	l.mu.Lock()
	keys := make(map[string][]state, len(l.keys))
	for k, kl := range l.keys {
		keys[k] = kl.allowed()
	}
	l.mu.Unlock()
	var out []string
	for k, allowed := range keys {
		data, found, err := get(ctx, c, l.bucket, k)
		if err != nil {
			out = append(out, fmt.Sprintf("%s: %v", k, err))
			continue
		}
		got := state{present: found}
		if found {
			got.sum = sum(data)
		}
		if !slices.Contains(allowed, got) {
			out = append(out, fmt.Sprintf("%s: final state %s, the ledger allows %v", k, got, allowed))
		}
	}
	return out
}

// present returns the keys the ledger knows to exist (confirmed, with no
// uncertain operation since) with their sums.
func (l *ledger) present() map[string]string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string]string{}
	for k, kl := range l.keys {
		if kl.confirmed.present && len(kl.uncertain) == 0 {
			out[k] = kl.confirmed.sum
		}
	}
	return out
}

// stale returns the keys of present whose metadata on drive is missing
// or older than their last confirmed write: the writes the drive missed
// and that are not healed yet.
func (l *ledger) stale(drive string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for k, kl := range l.keys {
		if !kl.confirmed.present || len(kl.uncertain) > 0 {
			continue
		}
		st, err := os.Stat(filepath.Join(objectPath(drive, l.bucket, k), "xl.meta"))
		if err != nil || st.ModTime().Before(kl.since) {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// load is a running mixed workload: workers putting, reading and
// deleting their own keys through a client until stopped.
type load struct {
	ledger *ledger
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// startLoad starts workers workers on bucket (each with keysPerWorker
// keys): 50% puts, 35% gets, 15% deletes, in the order the seed gives.
func startLoad(c *s3.Client, bucket string, seed uint64, workers, keysPerWorker int) *load {
	ctx, cancel := context.WithCancel(context.Background())
	l := &load{ledger: newLedger(bucket), cancel: cancel}
	for w := range workers {
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			rng := rand.New(rand.NewPCG(seed, uint64(w)))
			for ctx.Err() == nil {
				key := fmt.Sprintf("w%02d/k%03d", w, rng.IntN(keysPerWorker))
				// Each operation runs to its end (with the retries of
				// the client): stopping the load never cuts one.
				opCtx := context.WithoutCancel(ctx)
				switch p := rng.IntN(100); {
				case p < 50:
					data := content(rng)
					start := time.Now()
					err := put(opCtx, c, bucket, key, data)
					l.ledger.write("put", key, state{present: true, sum: sum(data)}, start, err)
				case p < 85:
					data, found, err := get(opCtx, c, bucket, key)
					got := state{present: found}
					if found {
						got.sum = sum(data)
					}
					l.ledger.read(key, got, err)
				default:
					start := time.Now()
					err := deleteKey(opCtx, c, bucket, key)
					l.ledger.write("delete", key, state{}, start, err)
				}
			}
		}()
	}
	return l
}

// stop stops the workers and waits for their last operations.
func (l *load) stop() {
	l.cancel()
	l.wg.Wait()
}

// deleteKey deletes key.
func deleteKey(ctx context.Context, c *s3.Client, bucket, key string) error {
	_, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &bucket, Key: &key})
	return err
}

// dataset writes n objects of deterministic contents under prefix and
// returns key → sha256.
func dataset(t *testing.T, c *s3.Client, bucket, prefix string, n int, seed uint64) map[string]string {
	t.Helper()
	rng := rand.New(rand.NewPCG(seed, 0xda7a))
	out := make(map[string]string, n)
	ctx := context.Background()
	for i := range n {
		key := fmt.Sprintf("%s%04d", prefix, i)
		data := content(rng)
		require.NoError(t, put(ctx, c, bucket, key, data), "put %s", key)
		out[key] = sum(data)
	}
	return out
}

// checkDataset reads every object of want through c and compares the
// contents; it returns the mismatches.
func checkDataset(ctx context.Context, c *s3.Client, bucket string, want map[string]string) []string {
	var out []string
	for key, s := range want {
		data, found, err := get(ctx, c, bucket, key)
		switch {
		case err != nil:
			out = append(out, fmt.Sprintf("%s: %v", key, err))
		case !found:
			out = append(out, key+": not found")
		case sum(data) != s:
			out = append(out, fmt.Sprintf("%s: sha256 %s, want %s", key, sum(data)[:12], s[:12]))
		}
	}
	slices.Sort(out)
	return out
}

// runFor runs the load for d.
func runFor(d time.Duration) { time.Sleep(d) }
