package worker

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/mediaworker/imaging"
	"github.com/r3n3r0/nexo/internal/mediaworker/openh264"
)

// Defaults of the configuration.
const (
	DefaultConcurrency = 1
	DefaultMaxAttempts = 5
	// maxJobBody bounds the body of a job request.
	maxJobBody = 1 << 20
)

// Config configures a worker.
type Config struct {
	// AuthToken is the bearer token Nexo sends (the auth_token of the
	// media_worker target); empty accepts any request (tests only).
	AuthToken string
	// WorkDir holds the queue and the files of the running jobs.
	WorkDir string
	// Concurrency is the number of jobs processed at once.
	Concurrency int
	// MaxAttempts bounds the attempts of a job that fails for a
	// transient reason.
	MaxAttempts int
	// Backoff is the delay before the first retry, doubled at each
	// attempt (default 10s).
	Backoff time.Duration
	// Name identifies the worker in status.json (default the host name).
	Name string
	// ImageLimits bound the images the worker decodes (default
	// imaging.DefaultMaxBytes and imaging.DefaultMaxPixels): a larger one
	// fails for good.
	ImageLimits imaging.Limits
	// ImageMemory bounds the memory of the image variants encoded at once
	// (default imaging.DefaultMemoryBudget; see imaging.EncodeMemory).
	ImageMemory int64
}

// Worker receives and processes the media jobs.
type Worker struct {
	cfg    Config
	store  Store
	lib    *openh264.Library
	queue  *queue
	logger *slog.Logger
	wg     sync.WaitGroup
}

// New returns a worker.
func New(cfg Config, store Store, lib *openh264.Library, logger *slog.Logger) (*Worker, error) {
	if store == nil || lib == nil {
		return nil, errors.New("worker: the store and the OpenH264 library are required")
	}
	if cfg.WorkDir == "" {
		return nil, errors.New("worker: a work directory is required")
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = DefaultConcurrency
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = DefaultMaxAttempts
	}
	if cfg.Backoff <= 0 {
		cfg.Backoff = 10 * time.Second
	}
	if cfg.Name == "" {
		cfg.Name, _ = os.Hostname()
	}
	cfg.ImageLimits = cfg.ImageLimits.WithDefaults()
	if logger == nil {
		logger = slog.Default()
	}
	q, err := newQueue(cfg.WorkDir + "/queue")
	if err != nil {
		return nil, err
	}
	return &Worker{cfg: cfg, store: store, lib: lib, queue: q, logger: logger}, nil
}

// Handler serves the jobs endpoint (POST /jobs) and the health check
// (GET /healthz).
func (w *Worker) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /jobs", w.receive)
	mux.HandleFunc("GET /healthz", func(rw http.ResponseWriter, _ *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(map[string]any{"status": "ok", "pending": w.queue.pending()})
	})
	return mux
}

// authorized checks the bearer token of a request.
func (w *Worker) authorized(r *http.Request) bool {
	if w.cfg.AuthToken == "" {
		return true
	}
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(got), []byte(w.cfg.AuthToken)) == 1
}

// receive queues a job durably before acknowledging it.
func (w *Worker) receive(rw http.ResponseWriter, r *http.Request) {
	if !w.authorized(r) {
		http.Error(rw, "unauthorized", http.StatusUnauthorized)
		return
	}
	var job domain.MediaJob
	if err := json.NewDecoder(io.LimitReader(r.Body, maxJobBody)).Decode(&job); err != nil {
		http.Error(rw, "invalid job: "+err.Error(), http.StatusBadRequest)
		return
	}
	switch job.Action {
	case domain.MediaJobTest:
		rw.WriteHeader(http.StatusOK)
		return
	case domain.MediaJobProcess:
		if job.Rule == nil {
			http.Error(rw, "invalid job: a process job needs its rule", http.StatusBadRequest)
			return
		}
	case domain.MediaJobDelete:
	default:
		http.Error(rw, fmt.Sprintf("invalid job: unknown action %q", job.Action), http.StatusBadRequest)
		return
	}
	if job.Bucket == "" || job.Key == "" || domain.IsMediaSidecar(job.Key) {
		http.Error(rw, "invalid job: a bucket and a video key are required", http.StatusBadRequest)
		return
	}
	if job.Sidecar != "" && job.Sidecar != domain.MediaSidecarDir(job.Key, job.VersionID) {
		http.Error(rw, "invalid job: the sidecar directory does not match the version", http.StatusBadRequest)
		return
	}
	if err := w.queue.push(job); err != nil {
		w.logger.Error("worker: cannot queue a job", "bucket", job.Bucket, "key", job.Key, "err", err)
		http.Error(rw, "cannot queue the job", http.StatusServiceUnavailable)
		return
	}
	w.logger.Info("worker: job queued", "action", job.Action, "bucket", job.Bucket, "key", job.Key, "version", job.VersionID)
	rw.WriteHeader(http.StatusAccepted)
}

// Run processes the queued jobs until ctx ends, then waits for the running
// ones to stop.
func (w *Worker) Run(ctx context.Context) {
	for range w.cfg.Concurrency {
		w.wg.Go(func() { w.loop(ctx) })
	}
	<-ctx.Done()
	w.wg.Wait()
}

func (w *Worker) loop(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		if item, ok := w.queue.next(); ok {
			w.handle(ctx, item)
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-w.queue.wake:
		case <-tick.C:
		}
	}
}

// handle runs a job and settles it: done, retried later or given up.
func (w *Worker) handle(ctx context.Context, item *queued) {
	job := item.Job
	log := w.logger.With("action", job.Action, "bucket", job.Bucket, "key", job.Key, "version", job.VersionID, "attempt", item.Attempts+1)
	start := time.Now()
	err := w.run(ctx, job)
	switch {
	case err == nil:
		log.Info("worker: job done", "elapsed", time.Since(start).Round(time.Millisecond))
		w.queue.done(item)
	case ctx.Err() != nil:
		// Stopping: the job stays queued for the next start, without
		// counting the interrupted attempt.
		w.queue.release(item)
	case permanent(err) || item.Attempts+1 >= w.cfg.MaxAttempts:
		log.Error("worker: job failed", "err", err)
		w.queue.done(item)
	default:
		delay := w.cfg.Backoff << min(item.Attempts, 6)
		log.Warn("worker: job failed, retried later", "err", err, "retry_in", delay)
		if rerr := w.queue.retry(item, delay, err); rerr != nil {
			log.Error("worker: cannot keep the job for a retry", "err", rerr)
		}
	}
}

// errPermanent marks a failure that another attempt would not fix.
var errPermanent = errors.New("permanent failure")

func permanent(err error) bool { return errors.Is(err, errPermanent) }

// run executes a job.
func (w *Worker) run(ctx context.Context, job domain.MediaJob) error {
	switch job.Action {
	case domain.MediaJobDelete:
		return w.remove(ctx, job)
	case domain.MediaJobProcess:
		return w.process(ctx, job)
	}
	return fmt.Errorf("%w: unknown action %q", errPermanent, job.Action)
}

// remove deletes the sidecar directory of a version that is gone.
func (w *Worker) remove(ctx context.Context, job domain.MediaJob) error {
	if job.Sidecar == "" {
		return nil
	}
	versions, err := w.store.List(ctx, job.Bucket, job.Sidecar)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil // the bucket is gone
		}
		return err
	}
	return w.store.Delete(ctx, job.Bucket, versions)
}
