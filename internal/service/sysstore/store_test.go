package sysstore_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/lock"
	"github.com/r3n3r0/nexo/internal/service/storage"
	storagemock "github.com/r3n3r0/nexo/internal/service/storage/mock"
	"github.com/r3n3r0/nexo/internal/service/sysstore"
	"github.com/r3n3r0/nexo/internal/testutil"
)

// faults is the fault injection state of one drive.
type faults struct {
	mu      sync.Mutex
	offline bool
	delay   time.Duration
}

func (f *faults) set(offline bool, delay time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.offline, f.delay = offline, delay
}

func (f *faults) get() (bool, time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.offline, f.delay
}

// hooked wraps a real local drive with the generated mock: calls are
// delegated unless the drive is offline (ErrDiskNotFound) or slow.
func hooked(t *testing.T, base storage.StorageAPI, f *faults) storage.StorageAPI {
	t.Helper()
	m := storagemock.NewStorageAPI(t)
	wait := func(ctx context.Context) error {
		off, delay := f.get()
		if off {
			return fmt.Errorf("%w: injected", domain.ErrDiskNotFound)
		}
		if delay > 0 {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	m.EXPECT().Endpoint().RunAndReturn(base.Endpoint).Maybe()
	m.EXPECT().IsOnline().RunAndReturn(func() bool { off, _ := f.get(); return !off }).Maybe()
	m.EXPECT().ReadAll(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(func(ctx context.Context, v, p string) ([]byte, error) {
		if err := wait(ctx); err != nil {
			return nil, err
		}
		return base.ReadAll(ctx, v, p)
	}).Maybe()
	m.EXPECT().WriteAll(mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(func(ctx context.Context, v, p string, b []byte) error {
		if err := wait(ctx); err != nil {
			return err
		}
		return base.WriteAll(ctx, v, p, b)
	}).Maybe()
	m.EXPECT().Delete(mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(func(ctx context.Context, v, p string, o storage.DeleteOptions) error {
		if err := wait(ctx); err != nil {
			return err
		}
		return base.Delete(ctx, v, p, o)
	}).Maybe()
	m.EXPECT().StatInfoFile(mock.Anything, mock.Anything, mock.Anything, mock.Anything).RunAndReturn(func(ctx context.Context, v, p string, glob bool) ([]storage.StatInfo, error) {
		if err := wait(ctx); err != nil {
			return nil, err
		}
		return base.StatInfoFile(ctx, v, p, glob)
	}).Maybe()
	return m
}

// fixture is a store over n hooked drives.
type fixture struct {
	raw    []storage.StorageAPI
	drives []storage.StorageAPI
	faults []*faults
	locks  lock.Locker
}

func newFixture(t *testing.T, n int) *fixture {
	t.Helper()
	fx := &fixture{locks: lock.NewLocal(lock.Deps{})}
	for i := range n {
		base := testutil.NewLocalDriveIndex(t, "", i)
		f := &faults{}
		fx.raw = append(fx.raw, base)
		fx.faults = append(fx.faults, f)
		fx.drives = append(fx.drives, hooked(t, base, f))
	}
	return fx
}

func (fx *fixture) store(t *testing.T, opts ...sysstore.Option) sysstore.Store {
	t.Helper()
	s, err := sysstore.New(sysstore.Deps{Drives: fx.drives, Locks: fx.locks}, opts...)
	require.NoError(t, err)
	return s
}

// offline marks the drives of idx offline (or back online).
func (fx *fixture) offline(on bool, idx ...int) {
	for _, i := range idx {
		fx.faults[i].set(on, 0)
	}
}

// stamp reads the stamp of the raw copy of path on drive i (-1 when none).
func (fx *fixture) stamp(t *testing.T, i int, path string) int64 {
	t.Helper()
	data, err := fx.raw[i].ReadAll(context.Background(), domain.SystemBucket, "config/"+path)
	if err != nil {
		return -1
	}
	var env struct{ Stamp int64 }
	require.NoError(t, json.Unmarshal(data, &env))
	return env.Stamp
}

func TestRoundTrip(t *testing.T) {
	for _, n := range []int{1, 2, 3, 4, 8} {
		t.Run(fmt.Sprintf("%d drives", n), func(t *testing.T) {
			ctx := context.Background()
			s := newFixture(t, n).store(t)

			_, err := s.Get(ctx, "iam/users/a.json")
			require.ErrorIs(t, err, sysstore.ErrNotFound)
			require.ErrorIs(t, err, domain.ErrNotFound)

			require.NoError(t, s.Put(ctx, "iam/users/a.json", []byte("one")))
			require.NoError(t, s.Put(ctx, "iam/users/b.json", []byte("two")))
			require.NoError(t, s.Put(ctx, "iam/users/a.json", []byte("three")))
			got, err := s.Get(ctx, "iam/users/a.json")
			require.NoError(t, err)
			assert.Equal(t, "three", string(got))

			all, err := s.List(ctx, "iam/users")
			require.NoError(t, err)
			assert.Equal(t, map[string][]byte{"a.json": []byte("three"), "b.json": []byte("two")}, all)

			stored, err := s.Create(ctx, "iam/users/a.json", []byte("ignored"))
			require.NoError(t, err)
			assert.Equal(t, "three", string(stored), "create keeps the existing document")
			stored, err = s.Create(ctx, "kdf.json", []byte("salt"))
			require.NoError(t, err)
			assert.Equal(t, "salt", string(stored))

			require.NoError(t, s.Delete(ctx, "iam/users/a.json"))
			require.NoError(t, s.Delete(ctx, "iam/users/a.json"), "deleting twice is not an error")
			require.NoError(t, s.Delete(ctx, "never/written.json"))
			_, err = s.Get(ctx, "iam/users/a.json")
			require.ErrorIs(t, err, sysstore.ErrNotFound)
			all, err = s.List(ctx, "iam/users")
			require.NoError(t, err)
			assert.Equal(t, map[string][]byte{"b.json": []byte("two")}, all)

			stored, err = s.Create(ctx, "iam/users/a.json", []byte("again"))
			require.NoError(t, err)
			assert.Equal(t, "again", string(stored), "create over a tombstone")

			empty, err := s.List(ctx, "nothing/here")
			require.NoError(t, err)
			assert.Empty(t, empty)
			root, err := s.List(ctx, "")
			require.NoError(t, err)
			assert.Equal(t, map[string][]byte{"kdf.json": []byte("salt")}, root, "directories are not listed")
		})
	}
}

func TestInvalidPaths(t *testing.T) {
	ctx := context.Background()
	s := newFixture(t, 1).store(t)
	for _, p := range []string{"", "/a", "a/", "a//b", "../a", "a/./b", "a b", "a\x00b", "é"} {
		t.Run(p, func(t *testing.T) {
			_, err := s.Get(ctx, p)
			assert.ErrorIs(t, err, domain.ErrInvalidArgument)
			assert.ErrorIs(t, s.Put(ctx, p, nil), domain.ErrInvalidArgument)
			assert.ErrorIs(t, s.Delete(ctx, p), domain.ErrInvalidArgument)
		})
	}
	_, err := s.List(ctx, "../x")
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = sysstore.New(sysstore.Deps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	// No silent local locker: in a cluster two nodes would write a
	// document at once.
	_, err = sysstore.New(sysstore.Deps{Drives: newFixture(t, 1).drives})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

func TestQuorum(t *testing.T) {
	tests := []struct {
		name      string
		drives    int
		offline   []int
		readErr   error
		writeErr  error
		listError error
	}{
		{name: "4 drives, 1 offline", drives: 4, offline: []int{3}},
		{name: "4 drives, 2 offline: reads only", drives: 4, offline: []int{0, 3}, writeErr: domain.ErrWriteQuorum},
		{name: "4 drives, 3 offline", drives: 4, offline: []int{0, 1, 2}, readErr: domain.ErrReadQuorum, writeErr: domain.ErrWriteQuorum, listError: domain.ErrReadQuorum},
		{name: "8 drives, 3 offline", drives: 8, offline: []int{0, 4, 7}},
		{name: "8 drives, 4 offline: reads only", drives: 8, offline: []int{0, 1, 2, 3}, writeErr: domain.ErrWriteQuorum},
		{name: "8 drives, 5 offline", drives: 8, offline: []int{0, 1, 2, 3, 4}, readErr: domain.ErrReadQuorum, writeErr: domain.ErrWriteQuorum, listError: domain.ErrReadQuorum},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			fx := newFixture(t, tt.drives)
			s := fx.store(t)
			require.NoError(t, s.Put(ctx, "doc.json", []byte("v1")))
			fx.offline(true, tt.offline...)

			got, err := s.Get(ctx, "doc.json")
			if tt.readErr != nil {
				require.ErrorIs(t, err, tt.readErr)
			} else {
				require.NoError(t, err)
				assert.Equal(t, "v1", string(got))
			}
			_, err = s.List(ctx, "")
			if tt.listError != nil {
				require.ErrorIs(t, err, tt.listError)
			} else {
				require.NoError(t, err)
			}
			err = s.Put(ctx, "doc.json", []byte("v2"))
			if tt.writeErr != nil {
				require.ErrorIs(t, err, tt.writeErr)
				require.ErrorIs(t, s.Delete(ctx, "doc.json"), tt.writeErr)
			} else {
				require.NoError(t, err)
			}

			// Back online: the last successful write wins.
			fx.offline(false, tt.offline...)
			got, err = s.Get(ctx, "doc.json")
			require.NoError(t, err)
			want := "v2"
			if tt.writeErr != nil {
				want = "v1"
			}
			assert.Equal(t, want, string(got))
		})
	}
}

func TestReadRepairAndTombstones(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t, 4)
	s := fx.store(t)
	require.NoError(t, s.Put(ctx, "a/doc.json", []byte("v1")))

	// Drive 0 misses an update, then comes back with the old copy.
	fx.offline(true, 0)
	require.NoError(t, s.Put(ctx, "a/doc.json", []byte("v2")))
	fx.offline(false, 0)
	assert.Less(t, fx.stamp(t, 0, "a/doc.json"), fx.stamp(t, 1, "a/doc.json"))
	got, err := s.Get(ctx, "a/doc.json")
	require.NoError(t, err)
	assert.Equal(t, "v2", string(got))
	assert.Equal(t, fx.stamp(t, 1, "a/doc.json"), fx.stamp(t, 0, "a/doc.json"), "the stale copy was repaired")

	// A drive that missed the delete cannot resurrect the document.
	fx.offline(true, 1)
	require.NoError(t, s.Delete(ctx, "a/doc.json"))
	fx.offline(false, 1)
	fx.offline(true, 2, 3)
	_, err = s.Get(ctx, "a/doc.json")
	require.ErrorIs(t, err, sysstore.ErrNotFound)
	all, err := s.List(ctx, "a")
	require.NoError(t, err)
	assert.Empty(t, all)
	fx.offline(false, 2, 3)

	// A wiped drive (no copy at all) is repopulated by the reads.
	require.NoError(t, s.Put(ctx, "a/other.json", []byte("x")))
	require.NoError(t, fx.raw[2].Delete(ctx, domain.SystemBucket, "config/a/other.json", storage.DeleteOptions{}))
	assert.Equal(t, int64(-1), fx.stamp(t, 2, "a/other.json"))
	got, err = s.Get(ctx, "a/other.json")
	require.NoError(t, err)
	assert.Equal(t, "x", string(got))
	assert.Positive(t, fx.stamp(t, 2, "a/other.json"))
}

func TestSlowDriveDoesNotStallReads(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t, 4)
	s := fx.store(t, sysstore.WithGrace(20*time.Millisecond))
	require.NoError(t, s.Put(ctx, "doc.json", []byte("v1")))
	fx.faults[2].set(false, 5*time.Second)

	start := time.Now()
	got, err := s.Get(ctx, "doc.json")
	require.NoError(t, err)
	assert.Equal(t, "v1", string(got))
	require.NoError(t, s.Put(ctx, "doc.json", []byte("v2")))
	assert.Less(t, time.Since(start), 2*time.Second, "a frozen drive is not waited for")
	fx.faults[2].set(false, 0)
}

func TestConcurrentWriters(t *testing.T) {
	// Two nodes share the drives and the (distributed) locker: Create
	// agrees on a single value and Puts never go back in time.
	ctx := context.Background()
	fx := newFixture(t, 4)
	a, b := fx.store(t), fx.store(t)

	var wg sync.WaitGroup
	results := make([]string, 16)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := a
			if i%2 == 1 {
				s = b
			}
			v, err := s.Create(ctx, "kdf.json", []byte(fmt.Sprintf("salt-%d", i)))
			assert.NoError(t, err)
			results[i] = string(v)
		}()
	}
	wg.Wait()
	for _, r := range results {
		assert.Equal(t, results[0], r)
	}

	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := a
			if i%2 == 1 {
				s = b
			}
			assert.NoError(t, s.Put(ctx, "counter.json", []byte(fmt.Sprint(i))))
		}()
	}
	wg.Wait()
	ga, err := a.Get(ctx, "counter.json")
	require.NoError(t, err)
	gb, err := b.Get(ctx, "counter.json")
	require.NoError(t, err)
	assert.Equal(t, ga, gb)
	for i := range fx.raw {
		assert.Equal(t, fx.stamp(t, 0, "counter.json"), fx.stamp(t, i, "counter.json"))
	}
}
