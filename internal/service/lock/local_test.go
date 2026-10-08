package lock

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
)

func TestLocalWriteLockIsExclusive(t *testing.T) {
	l := NewLocal(Deps{})
	ctx := context.Background()
	require.NoError(t, l.Lock(ctx, "b/o", time.Second))

	err := l.Lock(ctx, "b/o", 20*time.Millisecond)
	assert.ErrorIs(t, err, domain.ErrLockTimeout)
	err = l.RLock(ctx, "b/o", 20*time.Millisecond)
	assert.ErrorIs(t, err, domain.ErrLockTimeout)

	// Other resources are independent.
	require.NoError(t, l.Lock(ctx, "b/other", time.Second))
	require.NoError(t, l.Unlock(ctx, "b/other"))

	require.NoError(t, l.Unlock(ctx, "b/o"))
	require.NoError(t, l.Lock(ctx, "b/o", time.Second))
	require.NoError(t, l.Unlock(ctx, "b/o"))
}

func TestLocalReadLocksAreShared(t *testing.T) {
	l := NewLocal(Deps{})
	ctx := context.Background()
	require.NoError(t, l.RLock(ctx, "r", time.Second))
	require.NoError(t, l.RLock(ctx, "r", time.Second))
	assert.ErrorIs(t, l.Lock(ctx, "r", 10*time.Millisecond), domain.ErrLockTimeout)
	require.NoError(t, l.RUnlock(ctx, "r"))
	assert.ErrorIs(t, l.Lock(ctx, "r", 10*time.Millisecond), domain.ErrLockTimeout)
	require.NoError(t, l.RUnlock(ctx, "r"))
	require.NoError(t, l.Lock(ctx, "r", time.Second))
	require.NoError(t, l.Unlock(ctx, "r"))
}

func TestLocalUnlockErrors(t *testing.T) {
	l := NewLocal(Deps{})
	ctx := context.Background()
	assert.ErrorIs(t, l.Unlock(ctx, "none"), domain.ErrInvalidArgument)
	assert.ErrorIs(t, l.RUnlock(ctx, "none"), domain.ErrInvalidArgument)
	require.NoError(t, l.RLock(ctx, "x", time.Second))
	assert.ErrorIs(t, l.Unlock(ctx, "x"), domain.ErrInvalidArgument)
	require.NoError(t, l.RUnlock(ctx, "x"))
}

func TestLocalWaiterWakesUpOnRelease(t *testing.T) {
	l := NewLocal(Deps{})
	ctx := context.Background()
	require.NoError(t, l.Lock(ctx, "w", time.Second))

	acquired := make(chan error, 1)
	go func() { acquired <- l.Lock(ctx, "w", 2*time.Second) }()
	time.Sleep(20 * time.Millisecond)
	require.NoError(t, l.Unlock(ctx, "w"))
	select {
	case err := <-acquired:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("waiter was not woken up")
	}
	require.NoError(t, l.Unlock(ctx, "w"))
}

func TestLocalContextCancellation(t *testing.T) {
	l := NewLocal(Deps{})
	require.NoError(t, l.Lock(context.Background(), "c", time.Second))
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	err := l.Lock(ctx, "c", time.Second)
	assert.ErrorIs(t, err, context.Canceled)
	// No timeout: waits until the context is canceled.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel2()
	assert.ErrorIs(t, l.RLock(ctx2, "c", 0), context.DeadlineExceeded)
	require.NoError(t, l.Unlock(context.Background(), "c"))
	// Entries of released resources are garbage collected.
	assert.Empty(t, l.(*local).resources)
}

func TestLocalConcurrentWritersAreSerialized(t *testing.T) {
	l := NewLocal(Deps{})
	ctx := context.Background()
	const workers = 32
	const iterations = 50
	var counter int64
	var inCritical int32
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				require.NoError(t, l.Lock(ctx, "counter", 5*time.Second))
				if atomic.AddInt32(&inCritical, 1) != 1 {
					t.Error("two writers in the critical section")
				}
				counter++
				atomic.AddInt32(&inCritical, -1)
				require.NoError(t, l.Unlock(ctx, "counter"))
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, int64(workers*iterations), counter)
	assert.Empty(t, l.(*local).resources)
}

func TestLocalReadersAndWritersMixed(t *testing.T) {
	l := NewLocal(Deps{})
	ctx := context.Background()
	var readers int32
	var writers int32
	var violations int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 30; j++ {
				require.NoError(t, l.RLock(ctx, "mix", 5*time.Second))
				atomic.AddInt32(&readers, 1)
				if atomic.LoadInt32(&writers) != 0 {
					atomic.AddInt32(&violations, 1)
				}
				atomic.AddInt32(&readers, -1)
				require.NoError(t, l.RUnlock(ctx, "mix"))
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				require.NoError(t, l.Lock(ctx, "mix", 5*time.Second))
				atomic.AddInt32(&writers, 1)
				if atomic.LoadInt32(&readers) != 0 || atomic.LoadInt32(&writers) != 1 {
					atomic.AddInt32(&violations, 1)
				}
				atomic.AddInt32(&writers, -1)
				require.NoError(t, l.Unlock(ctx, "mix"))
			}
		}()
	}
	wg.Wait()
	assert.Zero(t, violations)
}

func TestNSLock(t *testing.T) {
	l := NewLocal(Deps{})
	ctx := context.Background()
	ns := l.NewNSLock(ctx, "bucket", "object")

	lc, err := ns.GetLock(ctx, time.Second)
	require.NoError(t, err)
	assert.NoError(t, lc.Context().Err())
	_, err = ns.GetRLock(ctx, 10*time.Millisecond)
	assert.ErrorIs(t, err, domain.ErrLockTimeout)

	lc.Unlock(ctx)
	lc.Unlock(ctx) // idempotent
	assert.ErrorIs(t, lc.Context().Err(), context.Canceled)

	r1, err := ns.GetRLock(ctx, time.Second)
	require.NoError(t, err)
	r2, err := ns.GetRLock(ctx, time.Second)
	require.NoError(t, err)
	r1.Unlock(ctx)
	r2.Unlock(ctx)
	lc, err = ns.GetLock(ctx, time.Second)
	require.NoError(t, err)
	lc.Unlock(ctx)

	assert.Equal(t, "bucket/object", Resource("bucket", "object"))
	assert.Equal(t, "bucket", Resource("bucket", ""))
}

// A waiting writer holds back new readers, so that a stream of readers
// cannot starve it; the readers proceed once the writer is done.
func TestLocalWriterPreferredOverNewReaders(t *testing.T) {
	l := NewLocal(Deps{})
	ctx := context.Background()
	require.NoError(t, l.RLock(ctx, "p", time.Second))

	writer := make(chan error, 1)
	go func() { writer <- l.Lock(ctx, "p", 2*time.Second) }()
	require.Eventually(t, func() bool {
		lc := l.(*local)
		lc.mu.Lock()
		defer lc.mu.Unlock()
		e, ok := lc.resources["p"]
		return ok && e.writersWaiting == 1
	}, time.Second, time.Millisecond)

	// A new reader waits behind the writer, the first reader leaves.
	reader := make(chan error, 1)
	go func() { reader <- l.RLock(ctx, "p", 2*time.Second) }()
	select {
	case err := <-reader:
		t.Fatalf("a reader overtook the waiting writer: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	require.NoError(t, l.RUnlock(ctx, "p"))
	require.NoError(t, <-writer)
	select {
	case err := <-reader:
		t.Fatalf("a reader entered while the writer holds the lock: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	require.NoError(t, l.Unlock(ctx, "p"))
	require.NoError(t, <-reader)
	require.NoError(t, l.RUnlock(ctx, "p"))
	assert.Empty(t, l.(*local).resources, "no entry is leaked")
}

// A writer that gives up (timeout or cancellation) lets the readers it
// held back in at once.
func TestLocalGivingUpWriterReleasesReaders(t *testing.T) {
	for name, giveUp := range map[string]func(ctx context.Context, l Locker) error{
		"timeout": func(ctx context.Context, l Locker) error { return l.Lock(ctx, "g", 50*time.Millisecond) },
		"cancel": func(ctx context.Context, l Locker) error {
			ctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
			defer cancel()
			return l.Lock(ctx, "g", time.Minute)
		},
	} {
		t.Run(name, func(t *testing.T) {
			l := NewLocal(Deps{})
			ctx := context.Background()
			require.NoError(t, l.RLock(ctx, "g", time.Second))
			writer := make(chan error, 1)
			go func() { writer <- giveUp(ctx, l) }()
			time.Sleep(10 * time.Millisecond)
			reader := make(chan error, 1)
			start := time.Now()
			go func() { reader <- l.RLock(ctx, "g", 5*time.Second) }()
			require.Error(t, <-writer)
			require.NoError(t, <-reader)
			assert.Less(t, time.Since(start), 2*time.Second, "the reader was woken when the writer gave up")
			require.NoError(t, l.RUnlock(ctx, "g"))
			require.NoError(t, l.RUnlock(ctx, "g"))
			assert.Empty(t, l.(*local).resources)
		})
	}
}

// Readers and writers hammering one resource all get through: the writers
// are not starved by overlapping readers.
func TestLocalNoWriterStarvation(t *testing.T) {
	l := NewLocal(Deps{})
	ctx := context.Background()
	stop := make(chan struct{})
	var readers sync.WaitGroup
	for range 8 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if err := l.RLock(ctx, "s", time.Second); err == nil {
					time.Sleep(time.Millisecond)
					_ = l.RUnlock(ctx, "s")
				}
			}
		}()
	}
	var writes atomic.Int32
	var writers sync.WaitGroup
	for range 4 {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for range 10 {
				if assert.NoError(t, l.Lock(ctx, "s", 2*time.Second)) {
					writes.Add(1)
					_ = l.Unlock(ctx, "s")
				}
			}
		}()
	}
	writers.Wait()
	close(stop)
	readers.Wait()
	assert.Equal(t, int32(40), writes.Load())
}
