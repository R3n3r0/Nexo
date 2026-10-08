package lock_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/lock"
)

func newTestServer(t *testing.T, clk clock.Clock, opts ...lock.ServerOption) lock.Server {
	t.Helper()
	return lock.NewServer(lock.ServerDeps{Clock: clk, Logger: slog.New(slog.DiscardHandler)}, opts...)
}

func args(uid string, resources ...string) lock.Args {
	return lock.Args{UID: uid, Resources: resources, Owner: "node-1", Quorum: 1}
}

func TestServerGrants(t *testing.T) {
	ctx := context.Background()
	type step struct {
		op   string
		args lock.Args
		want bool
	}
	tests := []struct {
		name  string
		steps []step
	}{
		{name: "write lock is exclusive", steps: []step{
			{"lock", args("w1", "b/o"), true},
			{"lock", args("w2", "b/o"), false},
			{"rlock", args("r1", "b/o"), false},
			{"lock", args("w3", "b/other"), true},
			{"unlock", args("w1", "b/o"), true},
			{"lock", args("w2", "b/o"), true},
		}},
		{name: "read locks are shared and block writers", steps: []step{
			{"rlock", args("r1", "x"), true},
			{"rlock", args("r2", "x"), true},
			{"lock", args("w1", "x"), false},
			{"runlock", args("r1", "x"), true},
			{"lock", args("w1", "x"), false},
			{"runlock", args("r2", "x"), true},
			{"lock", args("w1", "x"), true},
		}},
		{name: "grants are idempotent per uid", steps: []step{
			{"lock", args("w1", "k"), true},
			{"lock", args("w1", "k"), true},
			{"rlock", args("r1", "j"), true},
			{"rlock", args("r1", "j"), true},
			{"runlock", args("r1", "j"), true},
			{"runlock", args("r1", "j"), false},
		}},
		{name: "multiple resources are all or nothing", steps: []step{
			{"lock", args("w1", "a"), true},
			{"lock", args("w2", "a", "b"), false},
			{"lock", args("w3", "b"), true},
			{"unlock", args("w1", "a"), true},
			{"unlock", args("w3", "b"), true},
			{"lock", args("w2", "a", "b"), true},
		}},
		{name: "releases need the holder uid and kind", steps: []step{
			{"lock", args("w1", "k"), true},
			{"unlock", args("other", "k"), false},
			{"runlock", args("w1", "k"), false},
			{"unlock", args("w1", "k"), true},
			{"unlock", args("w1", "k"), false},
		}},
		{name: "refresh reports held entries", steps: []step{
			{"refresh", args("w1", "k"), false},
			{"lock", args("w1", "k"), true},
			{"refresh", args("w1", "k"), true},
			{"refresh", lock.Args{UID: "w1"}, true},
			{"force", args("", "k"), true},
			{"refresh", args("w1", "k"), false},
		}},
		{name: "force unlock by uid", steps: []step{
			{"rlock", args("r1", "a", "b"), true},
			{"rlock", args("r2", "a"), true},
			{"force", lock.Args{UID: "r1"}, true},
			{"force", lock.Args{UID: "r1"}, false},
			{"lock", args("w1", "b"), true},
			{"lock", args("w2", "a"), false},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(t, nil)
			for i, st := range tt.steps {
				var got bool
				var err error
				switch st.op {
				case "lock":
					got, err = s.Lock(ctx, st.args)
				case "rlock":
					got, err = s.RLock(ctx, st.args)
				case "unlock":
					got, err = s.Unlock(ctx, st.args)
				case "runlock":
					got, err = s.RUnlock(ctx, st.args)
				case "refresh":
					got, err = s.Refresh(ctx, st.args)
				case "force":
					got, err = s.ForceUnlock(ctx, st.args)
				}
				require.NoError(t, err, "step %d", i)
				assert.Equal(t, st.want, got, "step %d: %s %v", i, st.op, st.args)
			}
		})
	}
}

func TestServerInvalidArguments(t *testing.T) {
	ctx := context.Background()
	s := newTestServer(t, nil)
	_, err := s.Lock(ctx, lock.Args{Resources: []string{"x"}})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument, "empty uid")
	_, err = s.RLock(ctx, lock.Args{UID: "u"})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument, "no resource")
	_, err = s.Unlock(ctx, lock.Args{UID: "u", Resources: []string{""}})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument, "empty resource")
	_, err = s.ForceUnlock(ctx, lock.Args{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = s.Lock(canceled, args("u", "x"))
	assert.ErrorIs(t, err, context.Canceled)
	_, err = s.List(canceled)
	assert.ErrorIs(t, err, context.Canceled)
}

// TestServerOrphansExpire covers the lease: an entry not refreshed for
// longer than the stale timeout disappears, a refreshed one stays.
func TestServerOrphansExpire(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC))
	s := newTestServer(t, clk, lock.WithStaleAfter(time.Minute), lock.WithReapInterval(10*time.Second))

	ok, err := s.Lock(ctx, args("orphan", "a"))
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = s.RLock(ctx, args("alive", "b"))
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = s.Lock(ctx, args("abandoned", "c"))
	require.NoError(t, err)
	require.True(t, ok)

	clk.Advance(50 * time.Second)
	ok, err = s.Refresh(ctx, args("alive", "b"))
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = s.Lock(ctx, args("w", "a"))
	require.NoError(t, err)
	assert.False(t, ok, "still within the lease")

	clk.Advance(20 * time.Second) // orphan idle for 70s, alive for 20s
	ok, err = s.Lock(ctx, args("w", "a"))
	require.NoError(t, err)
	assert.True(t, ok, "the orphan expired")
	ok, err = s.Lock(ctx, args("w2", "b"))
	require.NoError(t, err)
	assert.False(t, ok, "the refreshed read lock is kept")
	ok, err = s.Refresh(ctx, args("orphan", "a"))
	require.NoError(t, err)
	assert.False(t, ok, "the holder of an expired lock learns it lost it")

	// The sweep removed the abandoned resource nobody touched.
	infos, err := s.List(ctx)
	require.NoError(t, err)
	var resources []string
	for _, info := range infos {
		resources = append(resources, info.Resource)
		assert.Equal(t, "node-1", info.Owner)
		assert.False(t, info.Since.IsZero())
	}
	assert.Equal(t, []string{"a", "b"}, resources)
	assert.True(t, infos[0].Writer)
	assert.False(t, infos[1].Writer)
}
