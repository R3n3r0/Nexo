package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/mediaworker/openh264"
)

const fixture = "../transcode/testdata/h264_aac_360p.mp4" // 640x360, 3 s

// memStore is an in-memory Store; versioned keeps every version of a key
// as S3 does in a versioned bucket.
type memStore struct {
	mu        sync.Mutex
	objects   map[string][]memVersion // bucket/key, oldest first
	types     map[string]string
	versioned bool
	version   string // the VersionID Head reports
	failPut   error
	seq       int
	// putIfRace runs once before the next conditional write checks its
	// condition: another writer.
	putIfRace func()
}

type memVersion struct {
	id     string
	data   []byte
	marker bool
}

func newMemStore() *memStore {
	return &memStore{objects: map[string][]memVersion{}, types: map[string]string{}}
}

func etagOf(data []byte) string { return `"` + jobID(domain.MediaJob{Key: string(data)}) + `"` }

// latest returns the current content of a key; mu is held.
func (s *memStore) latest(k string) ([]byte, bool) {
	vs := s.objects[k]
	if len(vs) == 0 || vs[len(vs)-1].marker {
		return nil, false
	}
	return vs[len(vs)-1].data, true
}

func (s *memStore) Head(_ context.Context, bucket, key, _ string) (ObjectInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.latest(bucket + "/" + key)
	if !ok {
		return ObjectInfo{}, ErrNotFound
	}
	return ObjectInfo{VersionID: s.version, ETag: etagOf(data), Size: int64(len(data))}, nil
}

func (s *memStore) Download(_ context.Context, bucket, key, _, path string) error {
	s.mu.Lock()
	data, ok := s.latest(bucket + "/" + key)
	s.mu.Unlock()
	if !ok {
		return ErrNotFound
	}
	return os.WriteFile(path, data, 0o600)
}

func (s *memStore) Upload(ctx context.Context, bucket, key, path, contentType string) error {
	data, err := os.ReadFile(path) // #nosec G304 -- test file
	if err != nil {
		return err
	}
	return s.Put(ctx, bucket, key, data, contentType)
}

func (s *memStore) Put(_ context.Context, bucket, key string, data []byte, contentType string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failPut != nil {
		return s.failPut
	}
	k := bucket + "/" + key
	v := memVersion{id: domain.MediaNullVersion, data: append([]byte(nil), data...)}
	if s.versioned {
		s.seq++
		v.id = "v" + strconv.Itoa(s.seq)
		s.objects[k] = append(s.objects[k], v)
	} else {
		s.objects[k] = []memVersion{v}
	}
	s.types[k] = contentType
	return nil
}

func (s *memStore) Get(_ context.Context, bucket, key string, _ int64) ([]byte, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.latest(bucket + "/" + key)
	if !ok {
		return nil, "", ErrNotFound
	}
	return append([]byte(nil), data...), etagOf(data), nil
}

func (s *memStore) PutIf(ctx context.Context, bucket, key string, data []byte, contentType, ifMatch string) error {
	s.mu.Lock()
	cur, ok := s.latest(bucket + "/" + key)
	race := s.putIfRace
	s.putIfRace = nil
	s.mu.Unlock()
	if race != nil {
		race()
		s.mu.Lock()
		cur, ok = s.latest(bucket + "/" + key)
		s.mu.Unlock()
	}
	if (ifMatch == "" && ok) || (ifMatch != "" && (!ok || etagOf(cur) != ifMatch)) {
		return ErrPrecondition
	}
	return s.Put(ctx, bucket, key, data, contentType)
}

func (s *memStore) List(_ context.Context, bucket, prefix string) ([]ObjectVersion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ObjectVersion
	for k, vs := range s.objects {
		key, ok := strings.CutPrefix(k, bucket+"/")
		if !ok || !strings.HasPrefix(key, prefix) {
			continue
		}
		for i, v := range vs {
			out = append(out, ObjectVersion{Key: key, VersionID: v.id, Latest: i == len(vs)-1 && !v.marker})
		}
	}
	return out, nil
}

func (s *memStore) Delete(_ context.Context, bucket string, versions []ObjectVersion) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range versions {
		k := bucket + "/" + v.Key
		s.objects[k] = slices.DeleteFunc(s.objects[k], func(m memVersion) bool { return m.id == v.VersionID })
		if len(s.objects[k]) == 0 {
			delete(s.objects, k)
		}
	}
	return nil
}

// deleteObject removes a key as a DELETE without a version does.
func (s *memStore) deleteObject(bucket, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := bucket + "/" + key
	if s.versioned {
		s.seq++
		s.objects[k] = append(s.objects[k], memVersion{id: "v" + strconv.Itoa(s.seq), marker: true})
		return
	}
	delete(s.objects, k)
}

// keys lists the current keys under a prefix.
func (s *memStore) keys(bucket, prefix string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k := range s.objects {
		key, ok := strings.CutPrefix(k, bucket+"/")
		if _, live := s.latest(k); ok && live && strings.HasPrefix(key, prefix) {
			out = append(out, key)
		}
	}
	slices.Sort(out)
	return out
}

// versions counts the versions under a prefix, delete markers included.
func (s *memStore) versions(bucket, prefix string) int {
	all, _ := s.List(context.Background(), bucket, prefix)
	return len(all)
}

func (s *memStore) get(bucket, key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.latest(bucket + "/" + key)
}

func (s *memStore) status(t *testing.T, bucket, sidecar string) domain.MediaStatus {
	t.Helper()
	data, ok := s.get(bucket, sidecar+domain.MediaStatusFile)
	require.True(t, ok, "status.json is written")
	var st domain.MediaStatus
	require.NoError(t, json.Unmarshal(data, &st))
	return st
}

// newWorker returns a worker on store; lib may be nil for the jobs that do
// not transcode.
func newWorker(t *testing.T, store Store, lib *openh264.Library, token string) *Worker {
	t.Helper()
	q, err := newQueue(t.TempDir())
	require.NoError(t, err)
	return &Worker{
		cfg:    Config{AuthToken: token, WorkDir: t.TempDir(), Concurrency: 1, MaxAttempts: 3, Backoff: time.Millisecond, Name: "test"},
		store:  store,
		lib:    lib,
		queue:  q,
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func library(t *testing.T) *openh264.Library {
	t.Helper()
	path := os.Getenv("NEXO_OPENH264")
	if path == "" {
		t.Skip("NEXO_OPENH264 is not set (the path of libopenh264 " + openh264.Version + ")")
	}
	l, err := openh264.Open(path)
	require.NoError(t, err)
	return l
}

func TestNewValidates(t *testing.T) {
	_, err := New(Config{WorkDir: t.TempDir()}, nil, nil, nil)
	require.Error(t, err)
	_, err = New(Config{}, newMemStore(), &openh264.Library{}, nil)
	require.Error(t, err)
	w, err := New(Config{WorkDir: t.TempDir()}, newMemStore(), &openh264.Library{}, nil)
	require.NoError(t, err)
	assert.Equal(t, DefaultConcurrency, w.cfg.Concurrency)
	assert.Equal(t, DefaultMaxAttempts, w.cfg.MaxAttempts)
	assert.Equal(t, 10*time.Second, w.cfg.Backoff)
}

func TestQueueLatestWinsAndSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	q, err := newQueue(dir)
	require.NoError(t, err)
	a := domain.MediaJob{Action: domain.MediaJobProcess, Bucket: "b", Key: "a.mp4", ETag: "1"}
	require.NoError(t, q.push(a))
	a.ETag = "2"
	require.NoError(t, q.push(a))
	require.NoError(t, q.push(domain.MediaJob{Action: domain.MediaJobProcess, Bucket: "b", Key: "b.mp4"}))
	assert.Equal(t, 2, q.pending(), "the newer job of a version replaces the pending one")

	// A restart finds the jobs on disk.
	q, err = newQueue(dir)
	require.NoError(t, err)
	item, ok := q.next()
	require.True(t, ok)
	assert.Equal(t, "a.mp4", item.Job.Key)
	assert.Equal(t, "2", item.Job.ETag)

	// While a version runs, a new job for it waits behind it.
	a.ETag = "3"
	require.NoError(t, q.push(a))
	other, ok := q.next()
	require.True(t, ok)
	assert.Equal(t, "b.mp4", other.Job.Key, "the running version is not taken twice")
	_, ok = q.next()
	assert.False(t, ok)
	q.done(item)
	q.done(other)
	item, ok = q.next()
	require.True(t, ok)
	assert.Equal(t, "3", item.Job.ETag)

	// A retry is delayed and counts the attempts.
	require.NoError(t, q.retry(item, time.Hour, errors.New("boom")))
	_, ok = q.next()
	assert.False(t, ok, "not before the delay")
	q.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	item, ok = q.next()
	require.True(t, ok)
	assert.Equal(t, 1, item.Attempts)
	assert.Equal(t, "boom", item.Error)
	q.done(item)
	assert.Equal(t, 0, q.pending())
}

func TestHandler(t *testing.T) {
	w := newWorker(t, newMemStore(), nil, "secret")
	srv := httptest.NewServer(w.Handler())
	t.Cleanup(srv.Close)
	post := func(token, body string) int {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/jobs", strings.NewReader(body))
		require.NoError(t, err)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := srv.Client().Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	rule := `"rule":{"id":"r","poster":true}`
	assert.Equal(t, http.StatusUnauthorized, post("", `{"action":"test"}`))
	assert.Equal(t, http.StatusUnauthorized, post("wrong", `{"action":"test"}`))
	assert.Equal(t, http.StatusOK, post("secret", `{"action":"test"}`))
	assert.Equal(t, http.StatusBadRequest, post("secret", `{`))
	assert.Equal(t, http.StatusBadRequest, post("secret", `{"action":"bogus","bucket":"b","key":"k"}`))
	assert.Equal(t, http.StatusBadRequest, post("secret", `{"action":"process","bucket":"b","key":"k"}`), "no rule")
	assert.Equal(t, http.StatusBadRequest, post("secret", `{"action":"process","bucket":"b","key":".nexo-media/k/null/poster.jpg",`+rule+`}`))
	assert.Equal(t, http.StatusBadRequest, post("secret", `{"action":"delete","bucket":"b","key":"k","versionId":"v1","sidecar":".nexo-media/other/v1/"}`))
	assert.Equal(t, 0, w.queue.pending())
	assert.Equal(t, http.StatusAccepted, post("secret", `{"action":"process","bucket":"b","key":"k.mp4","versionId":"v1","sidecar":".nexo-media/k.mp4/v1/",`+rule+`}`))
	assert.Equal(t, http.StatusAccepted, post("secret", `{"action":"delete","bucket":"b","key":"x.mp4","sidecar":".nexo-media/x.mp4/null/"}`))
	assert.Equal(t, 2, w.queue.pending())

	resp, err := srv.Client().Get(srv.URL + "/healthz")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	var health map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&health))
	assert.Equal(t, "ok", health["status"])
	assert.InDelta(t, 2, health["pending"], 0)
}

func TestDeleteRemovesTheSidecar(t *testing.T) {
	s := newMemStore()
	dir := domain.MediaSidecarDir("v.mp4", "")
	for _, k := range []string{"v.mp4", dir + "status.json", dir + "poster.jpg", domain.MediaSidecarDir("v.mp4", "v2") + "poster.jpg"} {
		require.NoError(t, s.Put(t.Context(), "b", k, []byte(k), ""))
	}
	w := newWorker(t, s, nil, "")
	require.NoError(t, w.run(t.Context(), domain.MediaJob{Action: domain.MediaJobDelete, Bucket: "b", Key: "v.mp4", Sidecar: dir}))
	assert.Equal(t, []string{".nexo-media/v.mp4/v2/poster.jpg", "v.mp4"}, s.keys("b", ""))
	require.NoError(t, w.run(t.Context(), domain.MediaJob{Action: domain.MediaJobDelete, Bucket: "b", Key: "v.mp4"}), "nothing to remove")
}

// TestDeleteInVersionedBucket: the sidecar of a version is removed for
// good, every version of its outputs included.
func TestDeleteInVersionedBucket(t *testing.T) {
	s := newMemStore()
	s.versioned = true
	dir := domain.MediaSidecarDir("v.mp4", "v1")
	for range 2 {
		require.NoError(t, s.Put(t.Context(), "b", dir+"status.json", []byte("{}"), ""))
		require.NoError(t, s.Put(t.Context(), "b", dir+"poster.jpg", []byte("jpeg"), ""))
	}
	s.deleteObject("b", dir+"poster.jpg")
	require.NoError(t, s.Put(t.Context(), "b", "v.mp4", []byte("video"), ""))
	w := newWorker(t, s, nil, "")
	require.NoError(t, w.run(t.Context(), domain.MediaJob{Action: domain.MediaJobDelete, Bucket: "b", Key: "v.mp4", VersionID: "v1", Sidecar: dir}))
	assert.Equal(t, 0, s.versions("b", domain.MediaSidecarPrefix))
	assert.Equal(t, []string{"v.mp4"}, s.keys("b", ""), "the video is untouched")
}

func TestProcessSkipsGoneOrOverwritten(t *testing.T) {
	s := newMemStore()
	w := newWorker(t, s, nil, "")
	rule := &domain.MediaRule{ID: "r", Poster: true}
	job := domain.MediaJob{Action: domain.MediaJobProcess, Bucket: "b", Key: "v.mp4", Rule: rule}
	require.NoError(t, w.run(t.Context(), job), "the video is gone")
	require.NoError(t, s.Put(t.Context(), "b", "v.mp4", []byte("new content"), "video/mp4"))
	job.ETag = `"old"`
	require.NoError(t, w.run(t.Context(), job), "overwritten meanwhile")
	assert.Empty(t, s.keys("b", domain.MediaSidecarPrefix), "nothing written")
}

func TestProcessUnsupportedIsPermanent(t *testing.T) {
	s := newMemStore()
	require.NoError(t, s.Put(t.Context(), "b", "v.mp4", []byte("not an mp4"), "video/mp4"))
	w := newWorker(t, s, nil, "")
	require.NoError(t, w.queue.push(domain.MediaJob{Action: domain.MediaJobProcess, Bucket: "b", Key: "v.mp4", Rule: &domain.MediaRule{ID: "r", Poster: true}}))
	item, ok := w.queue.next()
	require.True(t, ok)
	w.handle(t.Context(), item)
	assert.Equal(t, 0, w.queue.pending(), "a permanent failure is not retried")
	st := s.status(t, "b", domain.MediaSidecarDir("v.mp4", ""))
	assert.Equal(t, domain.MediaStateFailed, st.State)
	assert.NotEmpty(t, st.Error)
	assert.Equal(t, "r", st.Rule)
}

func TestTransientFailureIsRetried(t *testing.T) {
	s := newMemStore()
	require.NoError(t, s.Put(t.Context(), "b", "v.mp4", []byte("x"), "video/mp4"))
	s.failPut = errors.New("unavailable")
	w := newWorker(t, s, nil, "")
	require.NoError(t, w.queue.push(domain.MediaJob{Action: domain.MediaJobProcess, Bucket: "b", Key: "v.mp4", Rule: &domain.MediaRule{ID: "r"}}))
	for attempt := 1; attempt <= w.cfg.MaxAttempts; attempt++ {
		var item *queued
		require.Eventually(t, func() bool {
			var ok bool
			item, ok = w.queue.next()
			return ok
		}, time.Second, time.Millisecond)
		assert.Equal(t, attempt-1, item.Attempts)
		w.handle(t.Context(), item)
	}
	assert.Equal(t, 0, w.queue.pending(), "given up after MaxAttempts")
}

func TestProcessVideo(t *testing.T) {
	lib := library(t)
	video, err := os.ReadFile(fixture)
	require.NoError(t, err)
	s := newMemStore()
	s.versioned = true
	s.version = "v1"
	require.NoError(t, s.Put(t.Context(), "b", "clips/v.mp4", video, "video/mp4"))
	dir := domain.MediaSidecarDir("clips/v.mp4", "v1")
	// A file of an earlier run the rule no longer produces.
	require.NoError(t, s.Put(t.Context(), "b", dir+"renditions/720p.mp4", []byte("stale"), "video/mp4"))

	w := newWorker(t, s, lib, "")
	rule := &domain.MediaRule{ID: "r", Renditions: []string{"1080p", "240p"}, Poster: true, Thumbnails: &domain.MediaThumbnails{Interval: "1s"}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	require.NoError(t, w.queue.push(domain.MediaJob{Action: domain.MediaJobProcess, Bucket: "b", Key: "clips/v.mp4", ETag: etagOf(video), Rule: rule}))
	require.Eventually(t, func() bool { return w.queue.pending() == 0 }, time.Minute, 10*time.Millisecond)
	cancel()
	<-done

	st := s.status(t, "b", dir)
	assert.Equal(t, domain.MediaStateDone, st.State, st.Error)
	assert.Equal(t, etagOf(video), st.ETag)
	assert.Equal(t, 640, st.Width)
	assert.Equal(t, 360, st.Height)
	require.Len(t, st.Renditions, 1, "1080p is taller than the source")
	assert.Equal(t, "240p", st.Renditions[0].Name)
	assert.Equal(t, 240, st.Renditions[0].Height)
	assert.True(t, st.Poster)
	require.NotNil(t, st.Thumbnails)
	assert.Equal(t, 3, st.Thumbnails.Count)

	outputs := []string{dir + "poster.jpg", dir + "renditions/240p.mp4", dir + "status.json", dir + "thumbnails/sprite.jpg", dir + "thumbnails/thumbnails.vtt"}
	assert.Equal(t, outputs, s.keys("b", dir))
	assert.Equal(t, len(outputs), s.versions("b", dir), "only the latest version of each output is kept")
	assert.Equal(t, "video/mp4", s.types["b/"+dir+"renditions/240p.mp4"])
	vtt, _ := s.get("b", dir+"thumbnails/thumbnails.vtt")
	assert.True(t, strings.HasPrefix(string(vtt), "WEBVTT"))
}

// TestStopKeepsTheJob: a job interrupted by a stop stays queued, its
// attempt not counted, and the runners return at once.
func TestStopKeepsTheJob(t *testing.T) {
	s := newMemStore()
	require.NoError(t, s.Put(t.Context(), "b", "v.mp4", []byte("x"), "video/mp4"))
	w := newWorker(t, s, nil, "")
	require.NoError(t, w.queue.push(domain.MediaJob{Action: domain.MediaJobProcess, Bucket: "b", Key: "v.mp4", Rule: &domain.MediaRule{ID: "r"}}))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	item, ok := w.queue.next()
	require.True(t, ok)
	w.handle(ctx, item)
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run does not return once stopped while a job is queued")
	}
	item, ok = w.queue.next()
	require.True(t, ok, "the job is still queued")
	assert.Equal(t, 0, item.Attempts)
}

// photoJPEG returns a w x h JPEG of gradients, its EXIF orientation 6
// (stored on its side, shown rotated 90° clockwise) when rotated.
func photoJPEG(t *testing.T, w, h int, rotated bool) []byte {
	t.Helper()
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			m.SetRGBA(x, y, color.RGBA{uint8(x * 255 / w), uint8(y * 255 / h), 128, 255}) // #nosec G115 -- < 256
		}
	}
	var b bytes.Buffer
	require.NoError(t, jpeg.Encode(&b, m, &jpeg.Options{Quality: 90}))
	data := b.Bytes()
	if !rotated {
		return data
	}
	// APP1 Exif: a big-endian TIFF whose IFD0 holds Orientation = 6.
	exif := []byte("Exif\x00\x00MM\x00*\x00\x00\x00\x08\x00\x01\x01\x12\x00\x03\x00\x00\x00\x01\x00\x06\x00\x00\x00\x00\x00\x00")
	seg := []byte{0xff, 0xe1, 0, byte(len(exif) + 2)}
	return slices.Concat(data[:2], seg, exif, data[2:])
}

// runJob pushes a job and runs the worker until the queue is empty.
func runJob(t *testing.T, w *Worker, job domain.MediaJob) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	require.NoError(t, w.queue.push(job))
	require.Eventually(t, func() bool { return w.queue.pending() == 0 }, 2*time.Minute, 10*time.Millisecond)
	cancel()
	<-done
}

func TestProcessImage(t *testing.T) {
	for _, versioned := range []bool{false, true} {
		t.Run(map[bool]string{false: "unversioned", true: "versioned"}[versioned], func(t *testing.T) {
			s := newMemStore()
			s.versioned = versioned
			version := ""
			if versioned {
				s.version = "v1"
				version = "v1"
			}
			photo := photoJPEG(t, 400, 300, true) // shown 300x400
			require.NoError(t, s.Put(t.Context(), "b", "photos/p.jpg", photo, "image/jpeg"))
			dir := domain.MediaSidecarDir("photos/p.jpg", version)
			// Outputs of an earlier run the rule no longer produces, and
			// one it produces again.
			require.NoError(t, s.Put(t.Context(), "b", dir+"images/640.avif", []byte("stale"), "image/avif"))
			require.NoError(t, s.Put(t.Context(), "b", dir+"images/160.webp", []byte("old"), "image/webp"))

			w := newWorker(t, s, nil, "")
			rule := &domain.MediaRule{ID: "img", Images: &domain.MediaImages{Formats: domain.MediaImageFormats(), Widths: []int{160, 320, 640}}}
			runJob(t, w, domain.MediaJob{Action: domain.MediaJobProcess, Bucket: "b", Key: "photos/p.jpg", ETag: etagOf(photo), Rule: rule})

			st := s.status(t, "b", dir)
			require.Equal(t, domain.MediaStateDone, st.State, st.Error)
			assert.Equal(t, "img", st.Rule)
			assert.Empty(t, st.Step)
			assert.Equal(t, etagOf(photo), st.ETag)
			assert.Equal(t, 300, st.Width, "upright")
			assert.Equal(t, 400, st.Height)
			assert.False(t, st.Alpha, "an opaque photo")
			assert.Empty(t, st.Renditions)

			var want []string
			var outputs []domain.MediaImageOutput
			for _, f := range domain.MediaImageFormats() {
				for _, size := range [][3]int{{160, 160, 213}, {0, 300, 400}} {
					want = append(want, dir+domain.MediaImageFile(size[0], f))
					outputs = append(outputs, domain.MediaImageOutput{Format: f, Width: size[0], PixelWidth: size[1], PixelHeight: size[2]})
				}
			}
			require.Len(t, st.Images, len(outputs), "320 and 640 are not smaller than the image")
			for i, o := range st.Images {
				data, ok := s.get("b", dir+domain.MediaImageFile(o.Width, o.Format))
				require.True(t, ok)
				assert.Equal(t, int64(len(data)), o.Size)
				o.Size = 0
				assert.Equal(t, outputs[i], o)
				assert.Equal(t, domain.MediaImageContentType(o.Format), s.types["b/"+dir+domain.MediaImageFile(o.Width, o.Format)])
			}
			jpg, _ := s.get("b", dir+"images/full.jpg")
			cfg, err := jpeg.DecodeConfig(bytes.NewReader(jpg))
			require.NoError(t, err)
			assert.Equal(t, [2]int{300, 400}, [2]int{cfg.Width, cfg.Height})
			assert.NotContains(t, string(jpg), "Exif", "no metadata of the source")

			want = append(want, dir+domain.MediaStatusFile)
			slices.Sort(want)
			assert.Equal(t, want, s.keys("b", dir), "the stale outputs are removed")
			assert.Equal(t, len(want), s.versions("b", dir), "only the latest version of each output is kept")

			// The delete job of the version removes every variant.
			require.NoError(t, w.run(t.Context(), domain.MediaJob{Action: domain.MediaJobDelete, Bucket: "b", Key: "photos/p.jpg", VersionID: version, Sidecar: dir}))
			assert.Equal(t, 0, s.versions("b", domain.MediaSidecarPrefix))
		})
	}
}

// TestProcessImageAlpha: an image with transparent pixels is marked
// alpha; one whose color model has alpha but every pixel opaque is not.
func TestProcessImageAlpha(t *testing.T) {
	encode := func(a uint8) []byte {
		m := image.NewNRGBA(image.Rect(0, 0, 64, 48))
		for i := range m.Pix {
			m.Pix[i] = []uint8{200, 200, 200, 255}[i%4]
		}
		m.SetNRGBA(10, 10, color.NRGBA{200, 200, 200, a})
		var b bytes.Buffer
		require.NoError(t, png.Encode(&b, m))
		return b.Bytes()
	}
	s := newMemStore()
	require.NoError(t, s.Put(t.Context(), "b", "clear.png", encode(0), "image/png"))
	require.NoError(t, s.Put(t.Context(), "b", "solid.png", encode(255), "image/png"))
	w := newWorker(t, s, nil, "")
	rule := &domain.MediaRule{ID: "img", Images: &domain.MediaImages{Formats: []string{"jpeg", "png"}, Widths: []int{32}}}
	for key, alpha := range map[string]bool{"clear.png": true, "solid.png": false} {
		require.NoError(t, w.run(t.Context(), domain.MediaJob{Action: domain.MediaJobProcess, Bucket: "b", Key: key, Rule: rule}))
		st := s.status(t, "b", domain.MediaSidecarDir(key, ""))
		require.Equal(t, domain.MediaStateDone, st.State, st.Error)
		assert.Equal(t, alpha, st.Alpha, key)
		assert.Len(t, st.Images, 4)
	}
}

func TestProcessImageFailures(t *testing.T) {
	rule := &domain.MediaRule{ID: "img", Images: &domain.MediaImages{Formats: []string{"jpeg"}, Widths: []int{160}}}
	handle := func(t *testing.T, w *Worker, key string) {
		t.Helper()
		require.NoError(t, w.queue.push(domain.MediaJob{Action: domain.MediaJobProcess, Bucket: "b", Key: key, Rule: rule}))
		item, ok := w.queue.next()
		require.True(t, ok)
		w.handle(t.Context(), item)
		assert.Equal(t, 0, w.queue.pending(), "a permanent failure is not retried")
	}

	t.Run("unsupported", func(t *testing.T) {
		s := newMemStore()
		require.NoError(t, s.Put(t.Context(), "b", "a.png", []byte("not a png"), "image/png"))
		w := newWorker(t, s, nil, "")
		handle(t, w, "a.png")
		st := s.status(t, "b", domain.MediaSidecarDir("a.png", ""))
		assert.Equal(t, domain.MediaStateFailed, st.State)
		assert.Contains(t, st.Error, "unsupported image")
		assert.Equal(t, []string{".nexo-media/a.png/null/status.json"}, s.keys("b", domain.MediaSidecarPrefix))
	})

	t.Run("too many pixels", func(t *testing.T) {
		s := newMemStore()
		require.NoError(t, s.Put(t.Context(), "b", "big.jpg", photoJPEG(t, 400, 300, false), "image/jpeg"))
		w := newWorker(t, s, nil, "")
		w.cfg.ImageLimits.MaxPixels = 400*300 - 1
		handle(t, w, "big.jpg")
		st := s.status(t, "b", domain.MediaSidecarDir("big.jpg", ""))
		assert.Equal(t, domain.MediaStateFailed, st.State)
		assert.Contains(t, st.Error, "too large")
	})

	t.Run("file too large", func(t *testing.T) {
		s := newMemStore()
		require.NoError(t, s.Put(t.Context(), "b", "big.jpg", photoJPEG(t, 40, 30, false), "image/jpeg"))
		w := newWorker(t, s, nil, "")
		w.cfg.ImageLimits.MaxBytes = 100
		handle(t, w, "big.jpg")
		st := s.status(t, "b", domain.MediaSidecarDir("big.jpg", ""))
		assert.Equal(t, domain.MediaStateFailed, st.State)
		assert.Equal(t, "", st.Step, "refused before the download")
		assert.Contains(t, st.Error, "too large")
	})

	t.Run("overwritten or gone", func(t *testing.T) {
		s := newMemStore()
		w := newWorker(t, s, nil, "")
		job := domain.MediaJob{Action: domain.MediaJobProcess, Bucket: "b", Key: "p.jpg", Rule: rule}
		require.NoError(t, w.run(t.Context(), job), "the image is gone")
		require.NoError(t, s.Put(t.Context(), "b", "p.jpg", photoJPEG(t, 40, 30, false), "image/jpeg"))
		job.ETag = `"old"`
		require.NoError(t, w.run(t.Context(), job), "overwritten meanwhile")
		assert.Empty(t, s.keys("b", domain.MediaSidecarPrefix), "nothing written")
	})
}
