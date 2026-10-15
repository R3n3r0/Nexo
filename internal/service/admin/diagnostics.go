package admin

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime"
	"runtime/pprof"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// Sampling of the mutex and block profiles while they are collected.
const (
	// mutexProfileFraction reports one contention event out of 5.
	mutexProfileFraction = 5
	// blockProfileRate samples the blocking events of 10µs and more.
	blockProfileRate = 10_000
	// speedtestBucketPrefix names the temporary bucket of the object test.
	speedtestBucketPrefix = "nexo-speedtest-"
)

// DiagnosticsDeps are the dependencies of the diagnostics.
type DiagnosticsDeps struct {
	// Drives are the local drives of the node (drive test).
	Drives []storage.StorageAPI
	// Peers are the drives of the other nodes of a cluster, reached over
	// the internode RPC (net test): the first online one of every node is
	// measured. Empty out of a cluster.
	Peers []storage.StorageAPI
	// Objects and Buckets run the object test (optional: without them the
	// object test is domain.ErrNotImplemented).
	Objects object.Service
	Buckets bucket.Service
	Logger  *slog.Logger
}

// DiagnosticsOption configures the diagnostics.
type DiagnosticsOption func(*diagnostics)

// WithDiagnosticsNode names the node in the profiles and the results
// (default "local").
func WithDiagnosticsNode(node string) DiagnosticsOption {
	return func(d *diagnostics) {
		if node != "" {
			d.node = node
		}
	}
}

type diagnostics struct {
	deps DiagnosticsDeps
	node string
	// profiling and testing hold the profile and the speed test running.
	profiling atomic.Bool
	testing   atomic.Bool
}

var _ Diagnostics = (*diagnostics)(nil)

// NewDiagnostics returns the diagnostics of the node. The profiles and the
// speed tests are measured on the wall clock.
func NewDiagnostics(deps DiagnosticsDeps, opts ...DiagnosticsOption) Diagnostics {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	d := &diagnostics{deps: deps, node: "local"}
	for _, o := range opts {
		o(d)
	}
	return d
}

// --- profiling ----------------------------------------------------------------------------

// Profile implements Diagnostics.
func (d *diagnostics) Profile(ctx context.Context, in ProfileInput) (*Profile, error) {
	sampled := in.Type == ProfileCPU || in.Type == ProfileMutex || in.Type == ProfileBlock
	if !sampled && in.Type != ProfileHeap && in.Type != ProfileGoroutine {
		return nil, fmt.Errorf("%w: profile type %q must be cpu, heap, goroutine, mutex or block", domain.ErrInvalidArgument, in.Type)
	}
	duration := in.Duration
	if sampled {
		if duration == 0 {
			duration = DefaultProfileDuration
		}
		if duration < MinProfileDuration || duration > MaxProfileDuration {
			return nil, fmt.Errorf("%w: profile duration %s must be between %s and %s", domain.ErrInvalidArgument,
				duration, MinProfileDuration, MaxProfileDuration)
		}
	} else {
		duration = 0
	}
	// The sampling of the runtime is global to the process: one profile
	// at a time.
	if !d.profiling.CompareAndSwap(false, true) {
		return nil, fmt.Errorf("%w: a profile is being collected", domain.ErrConflict)
	}
	defer d.profiling.Store(false)

	var buf bytes.Buffer
	var err error
	switch in.Type {
	case ProfileCPU:
		err = d.cpuProfile(ctx, &buf, duration)
	case ProfileMutex:
		previous := runtime.SetMutexProfileFraction(mutexProfileFraction)
		err = d.sampled(ctx, &buf, "mutex", duration)
		runtime.SetMutexProfileFraction(previous)
	case ProfileBlock:
		runtime.SetBlockProfileRate(blockProfileRate)
		err = d.sampled(ctx, &buf, "block", duration)
		runtime.SetBlockProfileRate(0)
	default:
		err = writeProfile(&buf, string(in.Type))
	}
	if err != nil {
		return nil, err
	}
	d.deps.Logger.InfoContext(ctx, "diagnostics: profile collected", "type", in.Type, "duration", duration, "bytes", buf.Len())
	return &Profile{Type: in.Type, Node: d.node, Duration: duration, Data: buf.Bytes()}, nil
}

// cpuProfile samples the CPU for duration (or until ctx ends: the profile
// collected so far is returned).
func (d *diagnostics) cpuProfile(ctx context.Context, w io.Writer, duration time.Duration) error {
	if err := pprof.StartCPUProfile(w); err != nil {
		// Another CPU profile of the process (go test -cpuprofile).
		return fmt.Errorf("%w: cpu profile: %w", domain.ErrConflict, err)
	}
	d.wait(ctx, duration)
	pprof.StopCPUProfile()
	return nil
}

// sampled waits for duration while the runtime samples, then writes the
// profile name.
func (d *diagnostics) sampled(ctx context.Context, w io.Writer, name string, duration time.Duration) error {
	d.wait(ctx, duration)
	return writeProfile(w, name)
}

// wait blocks for duration or until ctx ends.
func (d *diagnostics) wait(ctx context.Context, duration time.Duration) {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// writeProfile writes a named profile of the runtime in the protocol
// buffer format.
func writeProfile(w io.Writer, name string) error {
	p := pprof.Lookup(name)
	if p == nil {
		return fmt.Errorf("%w: unknown profile %q", domain.ErrInvalidArgument, name)
	}
	if err := p.WriteTo(w, 0); err != nil {
		return fmt.Errorf("profile %s: %w", name, err)
	}
	return nil
}

// --- speed tests --------------------------------------------------------------------------

// Speedtest implements Diagnostics.
func (d *diagnostics) Speedtest(ctx context.Context, in SpeedtestInput) (*SpeedtestResult, error) {
	in, err := normalizeSpeedtest(in)
	if err != nil {
		return nil, err
	}
	if !d.testing.CompareAndSwap(false, true) {
		return nil, fmt.Errorf("%w: a speed test is running", domain.ErrConflict)
	}
	defer d.testing.Store(false)
	res := &SpeedtestResult{Kind: in.Kind, Node: d.node, Size: in.Size, Duration: in.Duration, Concurrency: in.Concurrency}
	id := randomID()
	switch in.Kind {
	case SpeedtestDrive:
		if len(d.deps.Drives) == 0 {
			return nil, fmt.Errorf("%w: no local drive to test", domain.ErrNotImplemented)
		}
		res.Targets = d.eachDrive(ctx, d.deps.Drives, func(ctx context.Context, drive storage.StorageAPI) SpeedtestTarget {
			return d.driveTest(ctx, drive, id, in)
		})
	case SpeedtestNet:
		peers := firstPerNode(d.deps.Peers)
		if len(peers) == 0 {
			return nil, fmt.Errorf("%w: the network test needs the other nodes of a cluster", domain.ErrNotImplemented)
		}
		res.Targets = d.eachDrive(ctx, peers, func(ctx context.Context, drive storage.StorageAPI) SpeedtestTarget {
			return d.netTest(ctx, drive, id, in)
		})
	case SpeedtestObject:
		if d.deps.Objects == nil || d.deps.Buckets == nil {
			return nil, fmt.Errorf("%w: the object test needs the object layer", domain.ErrNotImplemented)
		}
		t, err := d.objectTest(ctx, id, in)
		if err != nil {
			return nil, err
		}
		res.Targets = []SpeedtestTarget{t}
	}
	d.deps.Logger.InfoContext(ctx, "diagnostics: speed test done", "kind", in.Kind, "size", in.Size, "duration", in.Duration,
		"targets", len(res.Targets))
	return res, nil
}

// normalizeSpeedtest applies the defaults and checks the limits.
func normalizeSpeedtest(in SpeedtestInput) (SpeedtestInput, error) {
	switch in.Kind {
	case SpeedtestDrive, SpeedtestObject, SpeedtestNet:
	default:
		return in, fmt.Errorf("%w: speed test %q must be drive, object or net", domain.ErrInvalidArgument, in.Kind)
	}
	if in.Size == 0 {
		in.Size = DefaultSpeedtestSize
	}
	if in.Duration == 0 {
		in.Duration = DefaultSpeedtestDuration
	}
	if in.Concurrency == 0 {
		in.Concurrency = DefaultSpeedtestConcurrency
	}
	switch {
	case in.Size < 1 || in.Size > MaxSpeedtestSize:
		return in, fmt.Errorf("%w: speed test size %d must be between 1 and %d bytes", domain.ErrInvalidArgument, in.Size, MaxSpeedtestSize)
	case in.Duration < 100*time.Millisecond || in.Duration > MaxSpeedtestDuration:
		return in, fmt.Errorf("%w: speed test duration %s must be between 100ms and %s", domain.ErrInvalidArgument, in.Duration, MaxSpeedtestDuration)
	case in.Concurrency < 1 || in.Concurrency > MaxSpeedtestConcurrency:
		return in, fmt.Errorf("%w: speed test concurrency %d must be between 1 and %d", domain.ErrInvalidArgument, in.Concurrency, MaxSpeedtestConcurrency)
	}
	return in, nil
}

// eachDrive runs test on every drive in parallel, results in drive order.
func (d *diagnostics) eachDrive(ctx context.Context, drives []storage.StorageAPI,
	test func(context.Context, storage.StorageAPI) SpeedtestTarget) []SpeedtestTarget {
	out := make([]SpeedtestTarget, len(drives))
	var wg sync.WaitGroup
	for i, drive := range drives {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if drive == nil || !drive.IsOnline() {
				name := "offline"
				if drive != nil {
					name = drive.Endpoint().String()
				}
				out[i] = SpeedtestTarget{Target: name, Error: domain.ErrDiskNotFound.Error()}
				return
			}
			out[i] = test(ctx, drive)
		}()
	}
	wg.Wait()
	return out
}

// firstPerNode keeps the first online drive of every node.
func firstPerNode(drives []storage.StorageAPI) []storage.StorageAPI {
	seen := map[string]bool{}
	var out []storage.StorageAPI
	for _, dr := range drives {
		if dr == nil || !dr.IsOnline() {
			continue
		}
		host := dr.Endpoint().Host
		if seen[host] {
			continue
		}
		seen[host] = true
		out = append(out, dr)
	}
	return out
}

// testDir is the temporary directory of a test on a drive.
func testDir(id string) string { return storage.TmpDir + "/speedtest-" + id }

// driveTest writes files of in.Size into the temporary area of the drive
// for in.Duration (at most MaxSpeedtestBytes), then reads them back for
// in.Duration, and removes them.
func (d *diagnostics) driveTest(ctx context.Context, drive storage.StorageAPI, id string, in SpeedtestInput) SpeedtestTarget {
	t := SpeedtestTarget{Target: drive.Endpoint().String()}
	dir := testDir(id)
	defer d.cleanup(drive, dir)
	written, w, err := d.writeFiles(ctx, drive, dir, in)
	t.Write = w
	if err == nil && len(written) > 0 {
		t.Read, err = d.readFiles(ctx, drive, written, in.Size, in.Duration, 1)
	}
	if err != nil {
		t.Error = err.Error()
	}
	return t
}

// netTest uploads files to a drive of a peer for in.Duration, then reads
// the first of them back with in.Concurrency readers for in.Duration (the
// peer serves it from its page cache), and removes them.
func (d *diagnostics) netTest(ctx context.Context, drive storage.StorageAPI, id string, in SpeedtestInput) SpeedtestTarget {
	t := SpeedtestTarget{Target: drive.Endpoint().Host}
	dir := testDir(id)
	defer d.cleanup(drive, dir)
	written, w, err := d.writeFiles(ctx, drive, dir, in)
	t.Write = w
	if err == nil && len(written) > 0 {
		t.Read, err = d.readFiles(ctx, drive, written[:1], in.Size, in.Duration, in.Concurrency)
	}
	if err != nil {
		t.Error = err.Error()
	}
	return t
}

// writeFiles writes files of in.Size under dir until in.Duration elapses
// or MaxSpeedtestBytes are written.
func (d *diagnostics) writeFiles(ctx context.Context, drive storage.StorageAPI, dir string, in SpeedtestInput) ([]string, SpeedtestMeasure, error) {
	payload := speedtestPayload(in.Size)
	var rec recorder
	var written []string
	start := time.Now()
	deadline := start.Add(in.Duration)
	for n := 0; time.Now().Before(deadline) && int64(len(written)+1)*in.Size <= MaxSpeedtestBytes; n++ {
		if err := ctx.Err(); err != nil {
			return written, rec.measure(time.Since(start)), err
		}
		path := fmt.Sprintf("%s/%d", dir, n)
		began := time.Now()
		if err := drive.CreateFile(ctx, domain.SystemBucket, path, in.Size, bytes.NewReader(payload)); err != nil {
			return written, rec.measure(time.Since(start)), fmt.Errorf("write: %w", err)
		}
		rec.add(in.Size, time.Since(began))
		written = append(written, path)
	}
	return written, rec.measure(time.Since(start)), nil
}

// readFiles reads the files round-robin with concurrency readers for
// duration.
func (d *diagnostics) readFiles(ctx context.Context, drive storage.StorageAPI, files []string, size int64, duration time.Duration,
	concurrency int) (SpeedtestMeasure, error) {
	var rec recorder
	start := time.Now()
	deadline := start.Add(duration)
	var next atomic.Int64
	var firstErr error
	var errOnce sync.Once
	var wg sync.WaitGroup
	for range concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 128<<10)
			for time.Now().Before(deadline) && ctx.Err() == nil {
				path := files[int(next.Add(1)-1)%len(files)]
				began := time.Now()
				n, err := readAll(ctx, drive, path, size, buf)
				if err != nil {
					errOnce.Do(func() { firstErr = fmt.Errorf("read: %w", err) })
					return
				}
				rec.add(n, time.Since(began))
			}
		}()
	}
	wg.Wait()
	if firstErr == nil {
		firstErr = ctx.Err()
	}
	return rec.measure(time.Since(start)), firstErr
}

// readAll reads a whole file of the drive into the void.
func readAll(ctx context.Context, drive storage.StorageAPI, path string, size int64, buf []byte) (int64, error) {
	rc, err := drive.ReadFileStream(ctx, domain.SystemBucket, path, 0, size)
	if err != nil {
		return 0, err
	}
	defer func() { _ = rc.Close() }()
	return io.CopyBuffer(io.Discard, rc, buf)
}

// cleanup removes the directory of a test, whatever the outcome.
func (d *diagnostics) cleanup(drive storage.StorageAPI, dir string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := drive.Delete(ctx, domain.SystemBucket, dir, storage.DeleteOptions{Recursive: true, Immediate: true}); err != nil &&
		!errors.Is(err, domain.ErrObjectNotFound) && !errors.Is(err, domain.ErrNotFound) {
		d.deps.Logger.Warn("diagnostics: speed test files not removed (the janitor removes them)", "drive", drive.Endpoint().String(), "err", err)
	}
}

// objectTest uploads objects of in.Size in a temporary bucket with
// in.Concurrency writers for in.Duration (at most MaxSpeedtestBytes), then
// downloads them for in.Duration, and removes the bucket.
func (d *diagnostics) objectTest(ctx context.Context, id string, in SpeedtestInput) (SpeedtestTarget, error) {
	name := speedtestBucketPrefix + id
	if _, err := d.deps.Buckets.Create(ctx, bucket.CreateInput{Name: name}); err != nil {
		return SpeedtestTarget{}, fmt.Errorf("speed test bucket: %w", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := d.deps.Buckets.Delete(ctx, name, bucket.DeleteOptions{Force: true}); err != nil {
			d.deps.Logger.Warn("diagnostics: speed test bucket not removed", "bucket", name, "err", err)
		}
	}()
	t := SpeedtestTarget{Target: "objects"}
	payload := speedtestPayload(in.Size)
	var rec recorder
	var mu sync.Mutex
	var keys []string
	var count atomic.Int64
	maxObjects := max(MaxSpeedtestBytes/in.Size, 1)
	var firstErr error
	var errOnce sync.Once
	start := time.Now()
	deadline := start.Add(in.Duration)
	var wg sync.WaitGroup
	for range in.Concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) && ctx.Err() == nil {
				n := count.Add(1)
				if n > maxObjects {
					return
				}
				key := fmt.Sprintf("obj-%d", n)
				began := time.Now()
				_, err := d.deps.Objects.PutObject(ctx, object.PutObjectInput{Bucket: name, Key: key, Reader: bytes.NewReader(payload), Size: in.Size})
				if err != nil {
					errOnce.Do(func() { firstErr = fmt.Errorf("put: %w", err) })
					return
				}
				rec.add(in.Size, time.Since(began))
				mu.Lock()
				keys = append(keys, key)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	t.Write = rec.measure(time.Since(start))
	if firstErr != nil || len(keys) == 0 {
		if firstErr == nil {
			firstErr = ctx.Err()
		}
		if firstErr != nil {
			t.Error = firstErr.Error()
		}
		return t, nil
	}

	var readRec recorder
	var next atomic.Int64
	start = time.Now()
	deadline = start.Add(in.Duration)
	for range in.Concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 128<<10)
			for time.Now().Before(deadline) && ctx.Err() == nil {
				key := keys[int(next.Add(1)-1)%len(keys)]
				began := time.Now()
				r, err := d.deps.Objects.GetObject(ctx, name, key, object.GetObjectOptions{})
				if err != nil {
					errOnce.Do(func() { firstErr = fmt.Errorf("get: %w", err) })
					return
				}
				n, err := io.CopyBuffer(io.Discard, r, buf)
				_ = r.Close()
				if err != nil {
					errOnce.Do(func() { firstErr = fmt.Errorf("get: %w", err) })
					return
				}
				readRec.add(n, time.Since(began))
			}
		}()
	}
	wg.Wait()
	t.Read = readRec.measure(time.Since(start))
	if firstErr != nil {
		t.Error = firstErr.Error()
	}
	return t, nil
}

// speedtestPayload returns size bytes of random data (incompressible).
func speedtestPayload(size int64) []byte {
	b := make([]byte, size)
	_, _ = rand.Read(b)
	return b
}

// randomID names the files and the bucket of a test.
func randomID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// recorder accumulates the operations of a phase.
type recorder struct {
	mu        sync.Mutex
	bytes     int64
	latencies []time.Duration
}

func (r *recorder) add(n int64, latency time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bytes += n
	r.latencies = append(r.latencies, latency)
}

// measure summarizes the phase over elapsed.
func (r *recorder) measure(elapsed time.Duration) SpeedtestMeasure {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := SpeedtestMeasure{Bytes: r.bytes, Operations: int64(len(r.latencies)), Elapsed: elapsed}
	if secs := elapsed.Seconds(); secs > 0 {
		m.BytesPerSecond = float64(r.bytes) / secs
		m.OperationsPerSecond = float64(len(r.latencies)) / secs
	}
	if len(r.latencies) > 0 {
		sorted := slices.Clone(r.latencies)
		slices.Sort(sorted)
		m.LatencyP50 = sorted[(len(sorted)-1)*50/100]
		m.LatencyP99 = sorted[(len(sorted)-1)*99/100]
	}
	return m
}
