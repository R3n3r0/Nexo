package heal_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/heal"
)

func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

// nextWithin leases the next entry or fails the test after d.
func nextWithin(t *testing.T, q heal.Queue, d time.Duration) (heal.Entry, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return q.Next(ctx)
}

func TestQueueDedupAndBounds(t *testing.T) {
	ctx := context.Background()
	q := heal.NewQueue(heal.QueueDeps{Logger: quiet()}, heal.WithQueueSize(2))
	q.NotifyHeal(ctx, "b", "k1", "")
	q.NotifyHeal(ctx, "b", "k1", "") // merged
	q.NotifyHeal(ctx, "b", "k1", "v2")
	q.NotifyHeal(ctx, "b", "k2", "") // full: dropped
	q.NotifyHeal(ctx, domain.SystemBucket, "config/x", "")
	q.NotifyHeal(ctx, "", "k", "")
	st := q.Stats(ctx)
	assert.Equal(t, heal.QueueStats{Pending: 2, Queued: 2, Merged: 1, Dropped: 1}, st)

	// FIFO: the first queued is leased first.
	e, err := nextWithin(t, q, time.Second)
	require.NoError(t, err)
	assert.Equal(t, "k1", e.Object)
	assert.Empty(t, e.VersionID)
	assert.Equal(t, 1, q.Stats(ctx).Leased)
	q.Done(ctx, e, false)
	e, err = nextWithin(t, q, time.Second)
	require.NoError(t, err)
	assert.Equal(t, "v2", e.VersionID)
	q.Done(ctx, e, false)
	assert.Equal(t, uint64(2), q.Stats(ctx).Healed)

	// Empty: Next waits until the context ends.
	_, err = nextWithin(t, q, 20*time.Millisecond)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestQueueRetryBackoff(t *testing.T) {
	ctx := context.Background()
	fake := clock.NewFake(time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC))
	q := heal.NewQueue(heal.QueueDeps{Clock: fake, Logger: quiet()}, heal.WithBackoff(time.Hour, 4*time.Hour), heal.WithMaxAttempts(3))
	q.NotifyHeal(ctx, "b", "k", "")
	e, err := nextWithin(t, q, time.Second)
	require.NoError(t, err)

	// A notification during the lease queues the entry again at once,
	// whatever the outcome.
	q.NotifyHeal(ctx, "b", "k", "")
	q.Done(ctx, e, true)
	e, err = nextWithin(t, q, time.Second)
	require.NoError(t, err)
	assert.Zero(t, e.Attempts)

	// Failures are retried after the backoff (1h, then 2h), then dropped.
	q.Done(ctx, e, true)
	_, err = nextWithin(t, q, 20*time.Millisecond)
	require.ErrorIs(t, err, context.DeadlineExceeded, "not due before the backoff")
	fake.Advance(time.Hour)
	e, err = nextWithin(t, q, time.Second)
	require.NoError(t, err)
	assert.Equal(t, 1, e.Attempts)
	assert.Equal(t, fake.Now(), e.NotBefore)
	q.Done(ctx, e, true)
	fake.Advance(time.Hour)
	_, err = nextWithin(t, q, 20*time.Millisecond)
	require.ErrorIs(t, err, context.DeadlineExceeded, "the backoff doubles")
	fake.Advance(time.Hour)
	e, err = nextWithin(t, q, time.Second)
	require.NoError(t, err)
	assert.Equal(t, 2, e.Attempts)
	q.Done(ctx, e, true)
	st := q.Stats(ctx)
	assert.Zero(t, st.Pending, "abandoned after the attempts limit")
	assert.Equal(t, uint64(1), st.Dropped)
	assert.Equal(t, uint64(3), st.Retried)

	// Done of an unknown or not leased entry is ignored.
	q.Done(ctx, heal.Entry{Bucket: "b", Object: "other"}, false)
}

func TestQueuePersistence(t *testing.T) {
	ctx := context.Background()
	drives := openDrives(t, t.TempDir(), 2)
	q := heal.NewQueue(heal.QueueDeps{Drives: drives, Logger: quiet()}, heal.WithPersistInterval(10*time.Millisecond))
	require.NoError(t, q.Start(ctx))
	q.NotifyHeal(ctx, "b", "k1", "")
	q.NotifyHeal(ctx, "b", "k2", "v1")
	// Persisted periodically on every drive.
	require.Eventually(t, func() bool {
		for _, d := range drives {
			data, err := d.ReadAll(ctx, domain.SystemBucket, heal.MRFFile)
			if err != nil {
				return false
			}
			var doc struct{ Entries []heal.Entry }
			if json.Unmarshal(data, &doc) != nil || len(doc.Entries) != 2 {
				return false
			}
		}
		return true
	}, 5*time.Second, 10*time.Millisecond)
	e, err := nextWithin(t, q, time.Second)
	require.NoError(t, err)
	q.Done(ctx, e, false)
	require.NoError(t, q.Stop(ctx))
	require.NoError(t, q.Stop(ctx), "idempotent")

	// A new queue (after a restart) resumes the pending entry; a drive
	// with an unreadable document is ignored.
	require.NoError(t, drives[1].WriteAll(ctx, domain.SystemBucket, heal.MRFFile, []byte("{")))
	again := heal.NewQueue(heal.QueueDeps{Drives: drives, Logger: quiet()})
	require.NoError(t, again.Start(ctx))
	t.Cleanup(func() { _ = again.Stop(ctx) })
	assert.Equal(t, 1, again.Stats(ctx).Pending)
	e2, err := nextWithin(t, again, time.Second)
	require.NoError(t, err)
	assert.Equal(t, "k2", e2.Object)
	assert.Equal(t, "v1", e2.VersionID)
}
