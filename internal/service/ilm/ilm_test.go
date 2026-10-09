package ilm_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/event"
	eventmock "github.com/r3n3r0/nexo/internal/service/event/mock"
	"github.com/r3n3r0/nexo/internal/service/ilm"
	ilmmock "github.com/r3n3r0/nexo/internal/service/ilm/mock"
	"github.com/r3n3r0/nexo/internal/service/object"
	objectmock "github.com/r3n3r0/nexo/internal/service/object/mock"
	"github.com/r3n3r0/nexo/internal/service/scanner"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

// published is an event published by the object layer with the request
// of its context.
type published struct {
	name      event.Name
	key       string
	lifecycle bool
}

// world is a single-drive object layer driven by a fake clock, with the
// bucket settings the lifecycle reads.
type world struct {
	t       *testing.T
	clk     *clock.Fake
	objects object.Service
	svc     ilm.Service

	mu       sync.Mutex
	settings map[string]*bucket.Metadata
	events   []published
}

func newWorld(t *testing.T) *world {
	t.Helper()
	ctx := context.Background()
	w := &world{t: t, clk: clock.NewFake(time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)), settings: map[string]*bucket.Metadata{}}
	ep, err := domain.ParseEndpoint(t.TempDir())
	require.NoError(t, err)
	drive, err := storage.NewLocal(storage.Deps{Logger: quiet()}, ep, storage.WithoutFsync())
	require.NoError(t, err)
	t.Cleanup(func() { _ = drive.Close(ctx) })
	cfgs := objectmock.NewBucketConfigSource(t)
	cfgs.EXPECT().BucketConfig(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, b string) (object.BucketConfig, error) {
		w.mu.Lock()
		defer w.mu.Unlock()
		if m, ok := w.settings[b]; ok {
			return object.BucketConfig{Versioning: m.Versioning}, nil
		}
		return object.BucketConfig{}, nil
	}).Maybe()
	events := eventmock.NewService(t)
	events.EXPECT().Publish(mock.Anything, mock.Anything).RunAndReturn(func(ctx context.Context, e event.Event) error {
		req, _ := event.RequestFrom(ctx)
		w.mu.Lock()
		defer w.mu.Unlock()
		w.events = append(w.events, published{name: e.EventName, key: e.S3.Object.Key, lifecycle: req.Lifecycle})
		return nil
	}).Maybe()
	w.objects, err = object.NewFS(object.Deps{Store: drive, Buckets: cfgs, Events: events, Clock: w.clk, Logger: quiet()},
		object.WithMinPartSize(16))
	require.NoError(t, err)
	settings := ilmmock.NewBucketSettings(t)
	settings.EXPECT().Load(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, b string) (*bucket.Metadata, error) {
		w.mu.Lock()
		defer w.mu.Unlock()
		if m, ok := w.settings[b]; ok {
			cp := *m
			return &cp, nil
		}
		return &bucket.Metadata{Name: b}, nil
	}).Maybe()
	w.svc, err = ilm.New(ilm.Deps{Objects: w.objects, Buckets: settings, Clock: w.clk, Logger: quiet()}, ilm.WithPageSize(2))
	require.NoError(t, err)
	return w
}

// bucket creates a bucket with a lifecycle document.
func (w *world) bucket(name string, versioning domain.Versioning, lifecycle string) {
	w.t.Helper()
	require.NoError(w.t, w.objects.MakeBucket(context.Background(), name, object.MakeBucketOptions{Versioning: versioning}))
	canonical, err := w.svc.Validate(context.Background(), []byte(lifecycle))
	require.NoError(w.t, err)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.settings[name] = &bucket.Metadata{Name: name, Versioning: versioning, Lifecycle: canonical}
}

// setVersioning changes the versioning of a bucket, for the object layer
// and the lifecycle.
func (w *world) setVersioning(name string, versioning domain.Versioning) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.settings[name].Versioning = versioning
}

func (w *world) put(bucket, key string) domain.ObjectInfo {
	w.t.Helper()
	info, err := w.objects.PutObject(context.Background(), object.PutObjectInput{Bucket: bucket, Key: key, Reader: bytes.NewReader([]byte(key)), Size: int64(len(key))})
	require.NoError(w.t, err)
	return info
}

// versions returns "<key>@<version id or marker>" of every version of a
// bucket, in listing order.
func (w *world) versions(bucket string) []string {
	w.t.Helper()
	res, err := w.objects.ListObjectVersions(context.Background(), domain.ListObjectsInput{Bucket: bucket, IncludeVersions: true})
	require.NoError(w.t, err)
	var out []string
	for _, v := range res.Objects {
		id := v.VersionID
		if v.IsDeleteMarker {
			id = "marker"
		}
		out = append(out, v.Key+"@"+id)
	}
	return out
}

func (w *world) lifecycleEvents() []published {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []published
	for _, e := range w.events {
		if e.lifecycle {
			out = append(out, e)
		}
	}
	return out
}

// TestApplyNowUnversioned: the expired objects of an unversioned bucket
// are removed, with s3:LifecycleExpiration:Delete notifications.
func TestApplyNowUnversioned(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.bucket("plain", domain.VersioningOff, doc(`<Rule><ID>logs</ID><Filter><Prefix>logs/</Prefix></Filter><Status>Enabled</Status><Expiration><Days>1</Days></Expiration></Rule>`))
	w.put("plain", "logs/a")
	w.put("plain", "logs/b")
	w.put("plain", "data/c")
	w.bucket("nolc", domain.VersioningOff, doc(`<Rule><ID>off</ID><Status>Disabled</Status><Filter></Filter><Expiration><Days>1</Days></Expiration></Rule>`))
	w.put("nolc", "logs/a")

	res, err := w.svc.ApplyNow(ctx, "")
	require.NoError(t, err)
	assert.Equal(t, 0, res.Deleted, "nothing is due yet")

	w.clk.Advance(48 * time.Hour)
	res, err = w.svc.ApplyNow(ctx, "")
	require.NoError(t, err)
	assert.Equal(t, ilm.ApplyResult{Buckets: 2, Versions: 2, Deleted: 2}, res, "the keys no rule may select are not evaluated")
	assert.Equal(t, []string{"data/c@"}, w.versions("plain"))
	assert.Equal(t, []string{"logs/a@"}, w.versions("nolc"), "a disabled rule applies nothing")
	got := w.lifecycleEvents()
	require.Len(t, got, 2)
	for _, e := range got {
		assert.Equal(t, event.ObjectRemovedDelete, e.name, "the dispatcher publishes it as s3:LifecycleExpiration:Delete")
	}
}

// TestApplyNowVersioned: the current version gets a delete marker, the
// noncurrent versions go after their days, and the marker left alone is
// removed (expired object delete marker).
func TestApplyNowVersioned(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.bucket("ver", domain.VersioningEnabled, doc(
		`<Rule><ID>current</ID><Status>Enabled</Status><Filter></Filter><Expiration><Days>10</Days></Expiration></Rule>`,
		`<Rule><ID>noncurrent</ID><Status>Enabled</Status><Filter></Filter><NoncurrentVersionExpiration><NoncurrentDays>2</NoncurrentDays></NoncurrentVersionExpiration></Rule>`,
		`<Rule><ID>markers</ID><Status>Enabled</Status><Filter></Filter><Expiration><ExpiredObjectDeleteMarker>true</ExpiredObjectDeleteMarker></Expiration></Rule>`,
	))
	v1 := w.put("ver", "k")
	w.clk.Advance(24 * time.Hour)
	v2 := w.put("ver", "k")

	w.clk.Advance(3 * 24 * time.Hour)
	res, err := w.svc.ApplyNow(ctx, "ver")
	require.NoError(t, err)
	assert.Equal(t, 1, res.Deleted, "v1 is noncurrent for 3 days")
	assert.Equal(t, []string{"k@" + v2.VersionID}, w.versions("ver"))

	w.clk.Advance(10 * 24 * time.Hour)
	res, err = w.svc.ApplyNow(ctx, "ver")
	require.NoError(t, err)
	assert.Equal(t, 1, res.DeleteMarkers, "the current version expires under a delete marker")
	assert.Equal(t, []string{"k@marker", "k@" + v2.VersionID}, w.versions("ver"))

	w.clk.Advance(3 * 24 * time.Hour)
	_, err = w.svc.ApplyNow(ctx, "ver")
	require.NoError(t, err)
	assert.Equal(t, []string{"k@marker"}, w.versions("ver"), "v2 noncurrent for 3 days")
	res, err = w.svc.ApplyNow(ctx, "ver")
	require.NoError(t, err)
	assert.Equal(t, 1, res.Deleted, "the expired object delete marker")
	assert.Empty(t, w.versions("ver"))
	assert.NotEqual(t, v1.VersionID, v2.VersionID)

	var names []event.Name
	for _, e := range w.lifecycleEvents() {
		names = append(names, e.name)
	}
	assert.Equal(t, []event.Name{event.ObjectRemovedDelete, event.ObjectRemovedDeleteMarkerCreated, event.ObjectRemovedDelete, event.ObjectRemovedDelete}, names)
}

// TestScannerHook runs the real scanner with the lifecycle as its hook.
func TestScannerHook(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.bucket("ver", domain.VersioningEnabled, doc(
		`<Rule><ID>nc</ID><Filter><Prefix>a/</Prefix></Filter><Status>Enabled</Status><NoncurrentVersionExpiration><NoncurrentDays>1</NoncurrentDays></NoncurrentVersionExpiration></Rule>`,
		`<Rule><ID>mpu</ID><Status>Enabled</Status><Filter></Filter><AbortIncompleteMultipartUpload><DaysAfterInitiation>1</DaysAfterInitiation></AbortIncompleteMultipartUpload></Rule>`,
	))
	for range 3 {
		w.put("ver", "a/x")
		w.put("ver", "b/y")
	}
	_, err := w.objects.NewMultipartUpload(ctx, object.NewMultipartInput{Bucket: "ver", Key: "big"})
	require.NoError(t, err)
	w.clk.Advance(72 * time.Hour)

	s, err := scanner.New(scanner.Deps{Objects: w.objects, Lifecycle: w.svc, Clock: w.clk, Logger: quiet()},
		scanner.WithPageSize(2), scanner.WithStaleUploads(0), scanner.WithSpeed(scanner.SpeedFastest))
	require.NoError(t, err)
	stats, err := s.Scan(ctx)
	require.NoError(t, err)
	// The first version of a/x evaluates the key and removes both
	// noncurrent versions; the scanner meets one of them (the listing
	// resumes past the other).
	assert.Equal(t, uint64(1), stats.LifecycleActions)
	assert.Equal(t, 1, stats.UploadsCleaned, "the incomplete upload aborted by the rule")
	u, err := s.Usage(ctx)
	require.NoError(t, err)
	assert.Equal(t, uint64(2), u.Buckets["ver"].Versions, "the removed versions are left out of the usage")
	got := w.versions("ver")
	assert.Len(t, got, 4, "a/x current + b/y x3: %v", got)
	uploads, err := w.objects.ListMultipartUploads(ctx, domain.ListMultipartUploadsInput{Bucket: "ver"})
	require.NoError(t, err)
	assert.Empty(t, uploads.Uploads)
}

func TestExpiration(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.bucket("ver", domain.VersioningEnabled, doc(
		`<Rule><ID>tmp</ID><Filter><Prefix>tmp/</Prefix></Filter><Status>Enabled</Status><Expiration><Days>3</Days></Expiration></Rule>`,
		`<Rule><ID>soon</ID><Filter><And><Prefix>tmp/</Prefix><ObjectSizeLessThan>6</ObjectSizeLessThan></And></Filter><Status>Enabled</Status><Expiration><Days>1</Days></Expiration></Rule>`,
	))
	old := w.put("ver", "tmp/long-name")
	cur := w.put("ver", "tmp/long-name")
	small := w.put("ver", "tmp/a")
	w.put("ver", "keep")
	require.NoError(t, w.objects.MakeBucket(ctx, "none", object.MakeBucketOptions{}))

	exp, err := w.svc.Expiration(ctx, "ver", "tmp/long-name", "")
	require.NoError(t, err)
	assert.Equal(t, ilm.Expiry{Date: time.Date(2026, 3, 5, 0, 0, 0, 0, time.UTC), RuleID: "tmp"}, exp)
	exp, err = w.svc.Expiration(ctx, "ver", "tmp/long-name", cur.VersionID)
	require.NoError(t, err)
	assert.Equal(t, "tmp", exp.RuleID, "the current version by id")
	exp, err = w.svc.Expiration(ctx, "ver", "tmp/a", small.VersionID)
	require.NoError(t, err)
	assert.Equal(t, ilm.Expiry{Date: time.Date(2026, 3, 3, 0, 0, 0, 0, time.UTC), RuleID: "soon"}, exp, "the earliest rule")
	for _, tc := range []struct{ bucket, key, version string }{
		{"ver", "tmp/long-name", old.VersionID}, {"ver", "keep", ""}, {"ver", "tmp/missing", ""}, {"none", "tmp/x", ""},
	} {
		exp, err := w.svc.Expiration(ctx, tc.bucket, tc.key, tc.version)
		require.NoError(t, err)
		assert.Equal(t, ilm.Expiry{}, exp, "%s/%s@%s", tc.bucket, tc.key, tc.version)
	}
}

func TestNewRequiresDependencies(t *testing.T) {
	_, err := ilm.New(ilm.Deps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = ilm.New(ilm.Deps{Objects: objectmock.NewService(t)})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

// TestApplyRespectsTheObjectLayer: a removal the object layer refuses
// because of Object Lock is skipped and counted; a version that changed
// since it was listed (the object layer refuses the conditional delete)
// is kept; a null version listed without id is removed as "null".
func TestApplyRespectsTheObjectLayer(t *testing.T) {
	clk := clock.NewFake(time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC))
	expire := `<Rule><ID>r</ID><Status>Enabled</Status><Filter></Filter><Expiration><Days>1</Days></Expiration></Rule>`
	noncurrent := `<Rule><ID>nc</ID><Status>Enabled</Status><Filter></Filter><NoncurrentVersionExpiration><NoncurrentDays>1</NoncurrentDays></NoncurrentVersionExpiration></Rule>`
	tests := []struct {
		name       string
		versioning domain.Versioning
		rule       string
		listed     []domain.ObjectInfo
		// deletes are the deletions expected, with the error each gets.
		deletes []object.DeleteObjectOptions
		errs    []error
		want    ilm.ApplyResult
	}{
		{
			name: "unversioned: locked", rule: expire,
			listed:  []domain.ObjectInfo{{Bucket: "b", Key: "k", ModTime: created, IsLatest: true}},
			deletes: []object.DeleteObjectOptions{{VersionID: "null", IfModTime: created}},
			errs:    []error{domain.ErrObjectLocked},
			want:    ilm.ApplyResult{Buckets: 1, Versions: 1, Locked: 1},
		},
		{
			name: "unversioned: overwritten since the listing", rule: expire,
			listed:  []domain.ObjectInfo{{Bucket: "b", Key: "k", ModTime: created, IsLatest: true}},
			deletes: []object.DeleteObjectOptions{{VersionID: "null", IfModTime: created}},
			errs:    []error{domain.ErrPreconditionFailed},
			want:    ilm.ApplyResult{Buckets: 1, Versions: 1},
		},
		{
			name: "unversioned: removed", rule: expire,
			listed:  []domain.ObjectInfo{{Bucket: "b", Key: "k", ModTime: created, IsLatest: true}},
			deletes: []object.DeleteObjectOptions{{VersionID: "null", IfModTime: created}},
			errs:    []error{nil},
			want:    ilm.ApplyResult{Buckets: 1, Versions: 1, Deleted: 1},
		},
		{
			name: "suspended: the null version replaced since the listing", versioning: domain.VersioningSuspended, rule: expire,
			listed:  []domain.ObjectInfo{{Bucket: "b", Key: "k", VersionID: "null", ModTime: created, IsLatest: true}},
			deletes: []object.DeleteObjectOptions{{IfModTime: created}},
			errs:    []error{domain.ErrPreconditionFailed},
			want:    ilm.ApplyResult{Buckets: 1, Versions: 1},
		},
		{
			name: "enabled: a delete marker over the version evaluated", versioning: domain.VersioningEnabled, rule: expire,
			listed:  []domain.ObjectInfo{{Bucket: "b", Key: "k", VersionID: "v1", ModTime: created, IsLatest: true}},
			deletes: []object.DeleteObjectOptions{{IfModTime: created}},
			errs:    []error{nil},
			want:    ilm.ApplyResult{Buckets: 1, Versions: 1, DeleteMarkers: 1},
		},
		{
			name: "enabled: an unlabeled noncurrent null version", versioning: domain.VersioningEnabled, rule: noncurrent,
			listed: []domain.ObjectInfo{
				{Bucket: "b", Key: "k", VersionID: "v1", ModTime: created, IsLatest: true},
				{Bucket: "b", Key: "k", ModTime: created.Add(-time.Hour)},
			},
			deletes: []object.DeleteObjectOptions{{VersionID: "null", IfModTime: created.Add(-time.Hour)}},
			errs:    []error{nil},
			want:    ilm.ApplyResult{Buckets: 1, Versions: 2, Deleted: 1},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			objects := objectmock.NewService(t)
			settings := ilmmock.NewBucketSettings(t)
			settings.EXPECT().Load(mock.Anything, "b").Return(&bucket.Metadata{Name: "b", Versioning: tc.versioning, Lifecycle: []byte(doc(tc.rule))}, nil)
			objects.EXPECT().ListObjectVersions(mock.Anything, mock.Anything).Return(domain.ListObjectsResult{Objects: tc.listed}, nil)
			for i, opts := range tc.deletes {
				objects.EXPECT().DeleteObject(mock.Anything, "b", "k", opts).RunAndReturn(
					func(ctx context.Context, _, _ string, _ object.DeleteObjectOptions) (object.DeletedObject, error) {
						req, ok := event.RequestFrom(ctx)
						assert.True(t, ok && req.Lifecycle, "the removal carries the lifecycle request")
						return object.DeletedObject{Key: "k"}, tc.errs[i]
					}).Once()
			}
			svc, err := ilm.New(ilm.Deps{Objects: objects, Buckets: settings, Clock: clk, Logger: quiet()})
			require.NoError(t, err)
			res, err := svc.ApplyNow(ctx, "b")
			require.NoError(t, err)
			assert.Equal(t, tc.want, res)
		})
	}
}

// TestApplyNeverRemovesALaterWrite interleaves a write of the key between
// the evaluation of the lifecycle and its deletion (the object layer is
// the real one): the version written meanwhile is never removed nor
// hidden, whatever the versioning of the bucket.
func TestApplyNeverRemovesALaterWrite(t *testing.T) {
	expire := `<Rule><ID>r</ID><Status>Enabled</Status><Filter></Filter><Expiration><Days>1</Days></Expiration></Rule>`
	noncurrent := `<Rule><ID>nc</ID><Status>Enabled</Status><Filter></Filter><NoncurrentVersionExpiration><NoncurrentDays>1</NoncurrentDays></NoncurrentVersionExpiration></Rule>`
	tests := []struct {
		name string
		rule string
		// setup writes the key and leaves the bucket in its final state.
		setup func(w *world)
	}{
		{name: "unversioned", rule: expire, setup: func(w *world) {
			w.bucket("bkt", domain.VersioningOff, doc(expire))
			w.put("bkt", "k")
		}},
		{name: "enabled", rule: expire, setup: func(w *world) {
			w.bucket("bkt", domain.VersioningEnabled, doc(expire))
			w.put("bkt", "k")
		}},
		{name: "suspended: current null version", rule: expire, setup: func(w *world) {
			w.bucket("bkt", domain.VersioningSuspended, doc(expire))
			w.put("bkt", "k")
		}},
		{name: "suspended: noncurrent null version", rule: noncurrent, setup: func(w *world) {
			w.bucket("bkt", domain.VersioningSuspended, doc(noncurrent))
			w.put("bkt", "k")
			w.setVersioning("bkt", domain.VersioningEnabled)
			w.put("bkt", "k")
			w.setVersioning("bkt", domain.VersioningSuspended)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			w := newWorld(t)
			tc.setup(w)
			w.clk.Advance(72 * time.Hour)
			objects := objectmock.NewService(t)
			objects.EXPECT().ListObjectVersions(mock.Anything, mock.Anything).RunAndReturn(w.objects.ListObjectVersions)
			objects.EXPECT().DeleteObject(mock.Anything, "bkt", "k", mock.Anything).RunAndReturn(
				func(ctx context.Context, b, k string, opts object.DeleteObjectOptions) (object.DeletedObject, error) {
					// A client writes the key between the listing and the
					// deletion.
					_, err := w.objects.PutObject(ctx, object.PutObjectInput{Bucket: b, Key: k, Reader: bytes.NewReader([]byte("new")), Size: 3})
					require.NoError(t, err)
					return w.objects.DeleteObject(ctx, b, k, opts)
				})
			settings := ilmmock.NewBucketSettings(t)
			settings.EXPECT().Load(mock.Anything, "bkt").RunAndReturn(func(_ context.Context, b string) (*bucket.Metadata, error) {
				w.mu.Lock()
				defer w.mu.Unlock()
				cp := *w.settings[b]
				return &cp, nil
			})
			svc, err := ilm.New(ilm.Deps{Objects: objects, Buckets: settings, Clock: w.clk, Logger: quiet()})
			require.NoError(t, err)
			res, err := svc.ApplyNow(ctx, "bkt")
			require.NoError(t, err)
			assert.Zero(t, res.Deleted+res.DeleteMarkers, "nothing removed nor hidden: %+v", res)
			r, err := w.objects.GetObject(ctx, "bkt", "k", object.GetObjectOptions{})
			require.NoError(t, err, "the version written meanwhile is the current one")
			body, err := io.ReadAll(r)
			require.NoError(t, r.Close())
			require.NoError(t, err)
			assert.Equal(t, "new", string(body))
		})
	}
}

// TestApplyNowVersionedAfterWrites: the null version written before
// versioning was enabled is the null version of the bucket: listed as
// "null" and removed by NoncurrentVersionExpiration, through ApplyNow and
// through the scanner hook.
func TestApplyNowVersionedAfterWrites(t *testing.T) {
	noncurrent := doc(`<Rule><ID>nc</ID><Status>Enabled</Status><Filter></Filter><NoncurrentVersionExpiration><NoncurrentDays>1</NoncurrentDays></NoncurrentVersionExpiration></Rule>`)
	for _, viaScanner := range []bool{false, true} {
		t.Run(map[bool]string{false: "ApplyNow", true: "scanner"}[viaScanner], func(t *testing.T) {
			ctx := context.Background()
			w := newWorld(t)
			w.bucket("late", domain.VersioningOff, noncurrent)
			w.put("late", "pre")
			w.setVersioning("late", domain.VersioningEnabled)
			pre := w.put("late", "pre")
			w.put("late", "ver")
			ver := w.put("late", "ver")
			got := w.versions("late")
			assert.Contains(t, got, "pre@null", "the version written before versioning is the null version")
			w.clk.Advance(72 * time.Hour)
			if viaScanner {
				s, err := scanner.New(scanner.Deps{Objects: w.objects, Lifecycle: w.svc, Clock: w.clk, Logger: quiet()},
					scanner.WithPageSize(10), scanner.WithStaleUploads(0), scanner.WithSpeed(scanner.SpeedFastest))
				require.NoError(t, err)
				_, err = s.Scan(ctx)
				require.NoError(t, err)
			} else {
				res, err := w.svc.ApplyNow(ctx, "late")
				require.NoError(t, err)
				assert.Equal(t, ilm.ApplyResult{Buckets: 1, Versions: 4, Deleted: 2}, res)
			}
			assert.Equal(t, []string{"pre@" + pre.VersionID, "ver@" + ver.VersionID}, w.versions("late"))
		})
	}
}

func TestApplyNowReportsErrors(t *testing.T) {
	ctx := context.Background()
	objects := objectmock.NewService(t)
	settings := ilmmock.NewBucketSettings(t)
	objects.EXPECT().ListBuckets(mock.Anything).Return(nil, errors.New("drives offline")).Once()
	svc, err := ilm.New(ilm.Deps{Objects: objects, Buckets: settings, Logger: quiet()})
	require.NoError(t, err)
	_, err = svc.ApplyNow(ctx, "")
	require.Error(t, err)

	settings.EXPECT().Load(mock.Anything, "gone").Return(nil, domain.ErrBucketNotFound)
	_, err = svc.ApplyNow(ctx, "gone")
	assert.ErrorIs(t, err, domain.ErrBucketNotFound)
	action, err := svc.Apply(ctx, domain.ObjectInfo{Bucket: "gone", Key: "k", IsLatest: true})
	assert.ErrorIs(t, err, domain.ErrBucketNotFound)
	assert.Equal(t, scanner.LifecycleNone, action)
	n, err := svc.AbortUploads(ctx, "gone")
	assert.Error(t, err)
	assert.Zero(t, n)
}
