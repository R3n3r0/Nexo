package metrics_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/event"
	eventmock "github.com/r3n3r0/nexo/internal/service/event/mock"
	"github.com/r3n3r0/nexo/internal/service/kms"
	kmsmock "github.com/r3n3r0/nexo/internal/service/kms/mock"
	lockmock "github.com/r3n3r0/nexo/internal/service/lock/mock"
	"github.com/r3n3r0/nexo/internal/service/metrics"
	"github.com/r3n3r0/nexo/internal/service/scanner"
	scannermock "github.com/r3n3r0/nexo/internal/service/scanner/mock"
	"github.com/r3n3r0/nexo/internal/service/storage"
	storagemock "github.com/r3n3r0/nexo/internal/service/storage/mock"
)

func newTestRegistry() metrics.Registry {
	return metrics.New(metrics.Deps{}, metrics.WithoutRuntimeCollectors())
}

func snapshot(t *testing.T, reg metrics.Registry) []metrics.Family {
	t.Helper()
	fams, err := reg.Snapshot(context.Background())
	require.NoError(t, err)
	return fams
}

// histogramCount returns the count of the histogram sample with labels.
func histogramCount(t *testing.T, fams []metrics.Family, name string, labels map[string]string) uint64 {
	t.Helper()
	f, ok := family(fams, name)
	require.True(t, ok, "family %s missing", name)
	for _, s := range f.Samples {
		if assert.ObjectsAreEqual(labels, s.Labels) {
			return s.Count
		}
	}
	require.Failf(t, "sample missing", "%s%v", name, labels)
	return 0
}

func TestDriveErrorType(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{nil, ""},
		{fmt.Errorf("x: %w", domain.ErrObjectNotFound), ""},
		{domain.ErrNoSuchVersion, ""},
		{context.Canceled, ""},
		{storage.ErrFileCorrupt, "corrupt"},
		{domain.ErrFaultyDisk, "faulty"},
		{domain.ErrDiskNotFound, "offline"},
		{domain.ErrNodeOffline, "offline"},
		{domain.ErrStorageFull, "full"},
		{context.DeadlineExceeded, "timeout"},
		{domain.ErrFormatMismatch, "format"},
		{errors.New("boom"), "other"},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, metrics.DriveErrorType(tc.err), "%v", tc.err)
	}
}

func TestWrapDrive(t *testing.T) {
	ctx := context.Background()
	reg := newTestRegistry()
	d := storagemock.NewStorageAPI(t)
	d.EXPECT().Endpoint().Return(domain.Endpoint{Path: "/data/d1"})
	d.EXPECT().MakeVol(mock.Anything, "a").Return(nil)
	d.EXPECT().MakeVol(mock.Anything, "b").Return(domain.ErrFaultyDisk)
	d.EXPECT().StatVol(mock.Anything, "c").Return(storage.VolInfo{}, domain.ErrBucketNotFound)
	d.EXPECT().ReadAll(mock.Anything, "v", "p").Return(nil, storage.ErrFileCorrupt)
	d.EXPECT().IsOnline().Return(true)

	w, err := metrics.WrapDrive(ctx, d, reg)
	require.NoError(t, err)
	require.NoError(t, w.MakeVol(ctx, "a"))
	require.ErrorIs(t, w.MakeVol(ctx, "b"), domain.ErrFaultyDisk)
	_, err = w.StatVol(ctx, "c")
	require.ErrorIs(t, err, domain.ErrBucketNotFound)
	_, err = w.ReadAll(ctx, "v", "p")
	require.ErrorIs(t, err, storage.ErrFileCorrupt)
	assert.True(t, w.IsOnline(), "the accessors pass through")

	fams := snapshot(t, reg)
	assert.InDelta(t, 2, valueOf(t, fams, "nexo_drive_operations_total", map[string]string{"drive": "/data/d1", "op": "make_vol"}), 0)
	assert.InDelta(t, 1, valueOf(t, fams, "nexo_drive_operations_total", map[string]string{"drive": "/data/d1", "op": "stat_vol"}), 0)
	assert.InDelta(t, 1, valueOf(t, fams, "nexo_drive_errors_total", map[string]string{"drive": "/data/d1", "type": "faulty"}), 0)
	assert.InDelta(t, 1, valueOf(t, fams, "nexo_drive_errors_total", map[string]string{"drive": "/data/d1", "type": "corrupt"}), 0)
	errs, _ := family(fams, "nexo_drive_errors_total")
	assert.Len(t, errs.Samples, 2, "a missing bucket is not a failure of the drive")
	_, ok := family(fams, "nexo_drive_operation_seconds_total")
	assert.True(t, ok)

	_, err = metrics.WrapDrive(ctx, nil, reg)
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
}

func TestWrapLocker(t *testing.T) {
	ctx := context.Background()
	reg := newTestRegistry()
	l := lockmock.NewLocker(t)
	l.EXPECT().Lock(mock.Anything, "r1", time.Second).Return(nil)
	l.EXPECT().Lock(mock.Anything, "r2", time.Second).Return(fmt.Errorf("x: %w", domain.ErrLockTimeout))
	l.EXPECT().RLock(mock.Anything, "r3", time.Second).Return(errors.New("quorum lost"))
	l.EXPECT().RLock(mock.Anything, "r4", time.Second).Return(context.Canceled)
	l.EXPECT().Unlock(mock.Anything, "r1").Return(nil)
	ns := lockmock.NewNSLock(t)
	lc := lockmock.NewLockContext(t)
	ns.EXPECT().GetRLock(mock.Anything, time.Second).Return(lc, nil)
	l.EXPECT().NewNSLock(mock.Anything, "b", "o").Return(ns)

	w, err := metrics.WrapLocker(ctx, l, reg)
	require.NoError(t, err)
	require.NoError(t, w.Lock(ctx, "r1", time.Second))
	require.Error(t, w.Lock(ctx, "r2", time.Second))
	require.Error(t, w.RLock(ctx, "r3", time.Second))
	require.Error(t, w.RLock(ctx, "r4", time.Second))
	require.NoError(t, w.Unlock(ctx, "r1"))
	got, err := w.NewNSLock(ctx, "b", "o").GetRLock(ctx, time.Second)
	require.NoError(t, err)
	assert.Same(t, lc, got)

	fams := snapshot(t, reg)
	assert.Equal(t, uint64(1), histogramCount(t, fams, "nexo_lock_acquire_duration_seconds", map[string]string{"type": "write"}))
	assert.Equal(t, uint64(1), histogramCount(t, fams, "nexo_lock_acquire_duration_seconds", map[string]string{"type": "read"}))
	assert.InDelta(t, 1, valueOf(t, fams, "nexo_lock_acquire_timeouts_total", map[string]string{"type": "write"}), 0)
	assert.InDelta(t, 1, valueOf(t, fams, "nexo_lock_acquire_errors_total", map[string]string{"type": "read"}), 0, "a canceled call is not an error")
}

func TestWrapKMS(t *testing.T) {
	ctx := context.Background()
	reg := newTestRegistry()
	k := kmsmock.NewService(t)
	k.EXPECT().GenerateKey(mock.Anything, "k1", mock.Anything).Return(kms.DEK{}, nil)
	k.EXPECT().Decrypt(mock.Anything, "k1", mock.Anything, mock.Anything).Return(nil, fmt.Errorf("x: %w", domain.ErrKMSUnavailable))
	k.EXPECT().CreateKey(mock.Anything, "k2").Return(domain.ErrKMSKeyNotFound)
	k.EXPECT().DefaultKeyID().Return("k1")

	w, err := metrics.WrapKMS(ctx, k, reg)
	require.NoError(t, err)
	_, err = w.GenerateKey(ctx, "k1", nil)
	require.NoError(t, err)
	_, err = w.Decrypt(ctx, "k1", []byte("c"), nil)
	require.Error(t, err)
	require.Error(t, w.CreateKey(ctx, "k2"))
	assert.Equal(t, "k1", w.DefaultKeyID())

	fams := snapshot(t, reg)
	assert.InDelta(t, 1, valueOf(t, fams, "nexo_kms_requests_total", map[string]string{"op": "generate"}), 0)
	assert.InDelta(t, 1, valueOf(t, fams, "nexo_kms_errors_total", map[string]string{"op": "decrypt", "reason": "unavailable"}), 0)
	assert.InDelta(t, 1, valueOf(t, fams, "nexo_kms_errors_total", map[string]string{"op": "create", "reason": "not_found"}), 0)
	assert.Equal(t, uint64(1), histogramCount(t, fams, "nexo_kms_request_duration_seconds", map[string]string{"op": "decrypt"}))
}

func TestWrapEvents(t *testing.T) {
	ctx := context.Background()
	reg := newTestRegistry()
	id := event.TargetID{ID: "1", Type: "webhook"}
	target := eventmock.NewTarget(t)
	target.EXPECT().ID().Return(id)
	target.EXPECT().Send(mock.Anything, mock.MatchedBy(func(e event.Event) bool { return e.EventTime == "ok" })).Return(nil)
	target.EXPECT().Send(mock.Anything, mock.MatchedBy(func(e event.Event) bool { return e.EventTime == "ko" })).Return(errors.New("503"))
	factory := eventmock.NewTargetFactory(t)
	factory.EXPECT().New(mock.Anything, mock.Anything).Return(target, nil).Once()
	factory.EXPECT().New(mock.Anything, mock.Anything).Return(nil, errors.New("bad config")).Once()

	wf, err := metrics.WrapTargetFactory(ctx, factory, reg)
	require.NoError(t, err)
	tg, err := wf.New(ctx, event.TargetConfig{})
	require.NoError(t, err)
	assert.Equal(t, id, tg.ID())
	require.NoError(t, tg.Send(ctx, event.Event{EventTime: "ok"}))
	require.Error(t, tg.Send(ctx, event.Event{EventTime: "ko"}))
	_, err = wf.New(ctx, event.TargetConfig{})
	require.Error(t, err)

	d := eventmock.NewDispatcher(t)
	d.EXPECT().Publish(mock.Anything, mock.MatchedBy(func(e event.Event) bool { return e.EventTime == "a" })).Return(nil)
	d.EXPECT().Publish(mock.Anything, mock.MatchedBy(func(e event.Event) bool { return e.EventTime == "b" })).Return(errors.New("queue full"))
	d.EXPECT().Publish(mock.Anything, mock.MatchedBy(func(e event.Event) bool { return e.EventTime == "internal" })).Return(nil)
	wd, err := metrics.WrapEvents(ctx, d, reg)
	require.NoError(t, err)
	require.NoError(t, wd.Publish(ctx, event.Event{EventName: event.ObjectCreatedPut, EventTime: "a"}))
	require.Error(t, wd.Publish(ctx, event.Event{EventName: event.ObjectCreatedPut, EventTime: "b"}))
	require.NoError(t, wd.Publish(ctx, event.Event{EventName: "nexo:Internal", EventTime: "internal"}))

	fams := snapshot(t, reg)
	target1 := map[string]string{"target": "1:webhook"}
	assert.InDelta(t, 1, valueOf(t, fams, "nexo_events_sent_total", target1), 0)
	assert.InDelta(t, 1, valueOf(t, fams, "nexo_events_failed_total", target1), 0)
	assert.Equal(t, uint64(2), histogramCount(t, fams, "nexo_events_send_duration_seconds", target1))
	assert.InDelta(t, 1, valueOf(t, fams, "nexo_events_published_total", nil), 0, "internal events are not counted")
	assert.InDelta(t, 1, valueOf(t, fams, "nexo_events_dropped_total", nil), 0)
}

func TestWrapLifecycle(t *testing.T) {
	ctx := context.Background()
	reg := newTestRegistry()
	l := scannermock.NewLifecycle(t)
	l.EXPECT().Apply(mock.Anything, mock.MatchedBy(func(o domain.ObjectInfo) bool { return o.Key == "old" })).Return(scanner.LifecycleDeleted, nil)
	l.EXPECT().Apply(mock.Anything, mock.MatchedBy(func(o domain.ObjectInfo) bool { return o.Key == "cold" })).Return(scanner.LifecycleTransitioned, nil)
	l.EXPECT().Apply(mock.Anything, mock.MatchedBy(func(o domain.ObjectInfo) bool { return o.Key == "new" })).Return(scanner.LifecycleNone, nil)
	l.EXPECT().Apply(mock.Anything, mock.MatchedBy(func(o domain.ObjectInfo) bool { return o.Key == "bad" })).Return(scanner.LifecycleNone, errors.New("x"))
	l.EXPECT().AbortUploads(mock.Anything, "b").Return(3, nil)

	w, err := metrics.WrapLifecycle(ctx, l, reg)
	require.NoError(t, err)
	for _, name := range []string{"old", "cold", "new", "bad"} {
		_, _ = w.Apply(ctx, domain.ObjectInfo{Key: name})
	}
	n, err := w.AbortUploads(ctx, "b")
	require.NoError(t, err)
	assert.Equal(t, 3, n)

	fams := snapshot(t, reg)
	assert.InDelta(t, 1, valueOf(t, fams, "nexo_ilm_actions_total", map[string]string{"action": "expire"}), 0)
	assert.InDelta(t, 1, valueOf(t, fams, "nexo_ilm_actions_total", map[string]string{"action": "transition"}), 0)
	assert.InDelta(t, 4, valueOf(t, fams, "nexo_ilm_versions_evaluated_total", nil), 0)
	assert.InDelta(t, 1, valueOf(t, fams, "nexo_ilm_errors_total", nil), 0)
	assert.InDelta(t, 3, valueOf(t, fams, "nexo_ilm_uploads_aborted_total", nil), 0)
}

func TestRoundTripper(t *testing.T) {
	ctx := context.Background()
	reg := newTestRegistry()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/nexo/rpc/v1/storage/fail" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	rt, err := metrics.NewRoundTripper(ctx, http.DefaultTransport, reg, "n2:9000", metrics.WithPathPrefix("/nexo/rpc/v1"))
	require.NoError(t, err)
	client := &http.Client{Transport: rt}
	for _, p := range []string{"/storage/read-all", "/storage/read-all", "/storage/fail"} {
		resp, err := client.Post(srv.URL+"/nexo/rpc/v1"+p, "application/msgpack", nil)
		require.NoError(t, err)
		_ = resp.Body.Close()
	}
	_, err = client.Post("http://127.0.0.1:1/nexo/rpc/v1/lock/lock", "application/msgpack", nil)
	require.Error(t, err)

	fams := snapshot(t, reg)
	peer := func(m string) map[string]string { return map[string]string{"peer": "n2:9000", "method": m} }
	assert.InDelta(t, 2, valueOf(t, fams, "nexo_rpc_requests_total", peer("/storage/read-all")), 0)
	assert.InDelta(t, 1, valueOf(t, fams, "nexo_rpc_errors_total", peer("/storage/fail")), 0, "a 5xx is an error")
	assert.InDelta(t, 1, valueOf(t, fams, "nexo_rpc_errors_total", peer("/lock/lock")), 0, "a transport error is an error")
	assert.Equal(t, uint64(2), histogramCount(t, fams, "nexo_rpc_request_duration_seconds", peer("/storage/read-all")))
}
