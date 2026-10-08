package object_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
	objectmock "github.com/r3n3r0/nexo/internal/service/object/mock"
	"github.com/r3n3r0/nexo/internal/service/storage"
	storagemock "github.com/r3n3r0/nexo/internal/service/storage/mock"
	"github.com/r3n3r0/nexo/internal/testutil"
)

// driveFaults is the fault injection state of one drive (generated
// StorageAPI mock delegating to a real local drive): offline (every call
// fails with ErrDiskNotFound, as an unreachable remote drive), slow shard
// reads (they wait for the delay or the end of the context), full (new
// files fail with ErrStorageFull), failing commits, and an optional
// DiskInfo override (free space of the pool placement).
type driveFaults struct {
	mu         sync.Mutex
	offline    bool
	slowReads  time.Duration
	full       bool
	failCommit error
	diskInfo   *domain.DiskInfo
}

func (f *driveFaults) set(fn func(f *driveFaults)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *driveFaults) get() driveFaults {
	f.mu.Lock()
	defer f.mu.Unlock()
	return driveFaults{offline: f.offline, slowReads: f.slowReads, full: f.full, failCommit: f.failCommit, diskInfo: f.diskInfo}
}

func errOffline() error { return fmt.Errorf("%w: injected: drive offline", domain.ErrDiskNotFound) }

// slowReader delays every read by delay, or until ctx ends.
type slowReader struct {
	ctx   context.Context
	rc    io.ReadCloser
	delay time.Duration
}

func (s *slowReader) Read(p []byte) (int, error) {
	t := time.NewTimer(s.delay)
	defer t.Stop()
	select {
	case <-t.C:
	case <-s.ctx.Done():
		return 0, s.ctx.Err()
	}
	return s.rc.Read(p)
}

func (s *slowReader) Close() error { return s.rc.Close() }

// hookedDrive wraps base with the generated mock: every call is delegated
// to base unless a fault of f applies.
func hookedDrive(t *testing.T, base storage.StorageAPI, f *driveFaults) *storagemock.StorageAPI {
	t.Helper()
	m := storagemock.NewStorageAPI(t)
	off := func() bool { return f.get().offline }
	m.EXPECT().Endpoint().RunAndReturn(base.Endpoint).Maybe()
	m.EXPECT().IsOnline().RunAndReturn(func() bool { return !off() && base.IsOnline() }).Maybe()
	m.EXPECT().Healing().RunAndReturn(base.Healing).Maybe()
	m.EXPECT().DiskInfo(mock.Anything, mock.Anything).RunAndReturn(func(ctx context.Context, opts storage.DiskInfoOptions) (domain.DiskInfo, error) {
		st := f.get()
		if st.offline {
			return domain.DiskInfo{}, errOffline()
		}
		if st.diskInfo != nil {
			return *st.diskInfo, nil
		}
		return base.DiskInfo(ctx, opts)
	}).Maybe()
	m.EXPECT().MakeVol(mock.Anything, mock.Anything).RunAndReturn(func(ctx context.Context, v string) error {
		if off() {
			return errOffline()
		}
		return base.MakeVol(ctx, v)
	}).Maybe()
	m.EXPECT().ListVols(mock.Anything).RunAndReturn(func(ctx context.Context) ([]storage.VolInfo, error) {
		if off() {
			return nil, errOffline()
		}
		return base.ListVols(ctx)
	}).Maybe()
	m.EXPECT().StatVol(mock.Anything, mock.Anything).RunAndReturn(func(ctx context.Context, v string) (storage.VolInfo, error) {
		if off() {
			return storage.VolInfo{}, errOffline()
		}
		return base.StatVol(ctx, v)
	}).Maybe()
	m.EXPECT().DeleteVol(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(func(ctx context.Context, v string, force bool) error {
		if off() {
			return errOffline()
		}
		return base.DeleteVol(ctx, v, force)
	}).Maybe()
	m.EXPECT().ReadVersion(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(ctx context.Context, v, p, id string, o storage.ReadOptions) (storage.FileInfo, error) {
			if off() {
				return storage.FileInfo{}, errOffline()
			}
			return base.ReadVersion(ctx, v, p, id, o)
		}).Maybe()
	m.EXPECT().WriteMetadata(mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(func(ctx context.Context, v, p string, fi storage.FileInfo) error {
		if off() {
			return errOffline()
		}
		return base.WriteMetadata(ctx, v, p, fi)
	}).Maybe()
	m.EXPECT().UpdateMetadata(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(ctx context.Context, v, p string, fi storage.FileInfo, o storage.UpdateMetadataOptions) error {
			if off() {
				return errOffline()
			}
			return base.UpdateMetadata(ctx, v, p, fi, o)
		}).Maybe()
	m.EXPECT().DeleteVersion(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(ctx context.Context, v, p string, fi storage.FileInfo, o storage.DeleteOptions) error {
			if off() {
				return errOffline()
			}
			return base.DeleteVersion(ctx, v, p, fi, o)
		}).Maybe()
	m.EXPECT().CreateFile(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(ctx context.Context, v, p string, size int64, r io.Reader) error {
			st := f.get()
			switch {
			case st.offline:
				return errOffline()
			case st.full:
				return fmt.Errorf("%w: injected: no space left", domain.ErrStorageFull)
			}
			return base.CreateFile(ctx, v, p, size, r)
		}).Maybe()
	m.EXPECT().ReadFileStream(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(ctx context.Context, v, p string, off, length int64) (io.ReadCloser, error) {
			st := f.get()
			if st.offline {
				return nil, errOffline()
			}
			rc, err := base.ReadFileStream(ctx, v, p, off, length)
			if err != nil || st.slowReads == 0 {
				return rc, err
			}
			return &slowReader{ctx: ctx, rc: rc, delay: st.slowReads}, nil
		}).Maybe()
	m.EXPECT().RenameData(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(ctx context.Context, sv, sp string, fi storage.FileInfo, dv, dp string, o storage.RenameOptions) (storage.RenameDataResult, error) {
			st := f.get()
			switch {
			case st.offline:
				return storage.RenameDataResult{}, errOffline()
			case st.failCommit != nil:
				return storage.RenameDataResult{}, st.failCommit
			}
			return base.RenameData(ctx, sv, sp, fi, dv, dp, o)
		}).Maybe()
	m.EXPECT().RenameFile(mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(ctx context.Context, sv, sp, dv, dp string) error {
			if off() {
				return errOffline()
			}
			return base.RenameFile(ctx, sv, sp, dv, dp)
		}).Maybe()
	m.EXPECT().WalkDir(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(ctx context.Context, o storage.WalkDirOptions, fn storage.WalkDirFunc) error {
			if off() {
				return errOffline()
			}
			return base.WalkDir(ctx, o, fn)
		}).Maybe()
	m.EXPECT().ReadAll(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(func(ctx context.Context, v, p string) ([]byte, error) {
		if off() {
			return nil, errOffline()
		}
		return base.ReadAll(ctx, v, p)
	}).Maybe()
	m.EXPECT().WriteAll(mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(func(ctx context.Context, v, p string, b []byte) error {
		if off() {
			return errOffline()
		}
		return base.WriteAll(ctx, v, p, b)
	}).Maybe()
	m.EXPECT().Delete(mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(func(ctx context.Context, v, p string, o storage.DeleteOptions) error {
		if off() {
			return errOffline()
		}
		return base.Delete(ctx, v, p, o)
	}).Maybe()
	m.EXPECT().CheckParts(mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(base.CheckParts).Maybe()
	m.EXPECT().StatInfoFile(mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(ctx context.Context, v, p string, glob bool) ([]storage.StatInfo, error) {
			if off() {
				return nil, errOffline()
			}
			return base.StatInfoFile(ctx, v, p, glob)
		}).Maybe()
	m.EXPECT().Close(mock.Anything).RunAndReturn(base.Close).Maybe()
	return m
}

// healRecorder is the HealNotifier mock recording the notifications.
type healRecorder struct {
	*objectmock.HealNotifier
	mu    sync.Mutex
	calls []string
}

func newHealRecorder(t *testing.T) *healRecorder {
	h := &healRecorder{HealNotifier: objectmock.NewHealNotifier(t)}
	h.EXPECT().NotifyHeal(mock.Anything, mock.Anything, mock.Anything, mock.Anything).Run(func(_ context.Context, bucket, key, versionID string) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.calls = append(h.calls, bucket+"/"+key+"@"+versionID)
	}).Maybe()
	return h
}

// notified returns the notifications so far and forgets them.
func (h *healRecorder) notified() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.calls
	h.calls = nil
	return out
}

// cluster is an erasure layer over hooked local drives.
type cluster struct {
	svc    object.Service
	roots  []string
	drives []storage.StorageAPI
	faults []*driveFaults
	heal   *healRecorder
}

// clusterSpec describes the layout of a test cluster: pools × sets × drives.
type clusterSpec struct {
	pools, sets, drives int
	// disk keeps the drives in the test temporary directory; by default
	// they go to the tmpfs /dev/shm when it exists (faster).
	disk bool
	// decommissioning marks pools excluded from the placement.
	decommissioning map[int]bool
	// buckets is the bucket configuration source (nil: unversioned).
	buckets object.BucketConfigSource
	opts    []object.Option
}

// newCluster builds NewServerPools over hooked local drives.
func newCluster(t *testing.T, spec clusterSpec) *cluster {
	t.Helper()
	base := t.TempDir()
	if !spec.disk {
		if dir, err := os.MkdirTemp("/dev/shm", "nexo-test-"); err == nil {
			base = dir
			t.Cleanup(func() { _ = os.RemoveAll(dir) })
		}
	}
	c := &cluster{heal: newHealRecorder(t)}
	var pools []object.PoolSpec
	for p := 0; p < spec.pools; p++ {
		ps := object.PoolSpec{Decommissioning: spec.decommissioning[p]}
		for s := 0; s < spec.sets; s++ {
			var set []storage.StorageAPI
			for d := 0; d < spec.drives; d++ {
				root := filepath.Join(base, fmt.Sprintf("p%d-s%d-d%d", p, s, d))
				f := &driveFaults{}
				drive := hookedDrive(t, testutil.NewLocalDriveIndex(t, root, d), f)
				set = append(set, drive)
				c.roots = append(c.roots, root)
				c.drives = append(c.drives, drive)
				c.faults = append(c.faults, f)
			}
			ps.Sets = append(ps.Sets, set)
		}
		pools = append(pools, ps)
	}
	opts := append([]object.Option{
		object.WithInlineLimit(testInlineLimit), object.WithBlockSize(testBlockSize), object.WithMinPartSize(16),
		object.WithLockTimeout(5 * time.Second), object.WithHedgeDelay(50 * time.Millisecond),
	}, spec.opts...)
	svc, err := object.NewServerPools(object.PoolsDeps{
		Pools: pools, DeploymentID: "deployment-test", Heal: c.heal, Buckets: spec.buckets, Logger: slog.New(slog.DiscardHandler),
	}, opts...)
	require.NoError(t, err)
	c.svc = svc
	return c
}

// setFaults applies fn to the faults of the drives with the given indexes.
func (c *cluster) setFaults(fn func(f *driveFaults), idx ...int) {
	for _, i := range idx {
		c.faults[i].set(fn)
	}
}

// versionedBuckets returns a BucketConfigSource (generated mock) reporting
// every bucket with versioning enabled.
func versionedBuckets(t *testing.T) object.BucketConfigSource {
	m := objectmock.NewBucketConfigSource(t)
	m.EXPECT().BucketConfig(mock.Anything, mock.Anything).Return(object.BucketConfig{Versioning: domain.VersioningEnabled}, nil).Maybe()
	return m
}

// poolOf returns the pools whose drives hold bucket/key.
func (c *cluster) poolOf(t *testing.T, spec clusterSpec, bucket, key string) []int {
	t.Helper()
	perPool := spec.sets * spec.drives
	var out []int
	for p := 0; p < spec.pools; p++ {
		for _, root := range c.roots[p*perPool : (p+1)*perPool] {
			if _, err := os.Stat(filepath.Join(root, bucket, key, storage.MetaFile)); err == nil {
				out = append(out, p)
				break
			}
		}
	}
	return out
}
