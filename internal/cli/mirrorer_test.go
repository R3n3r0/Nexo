package cli_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/internal/cli/clitest"
	"github.com/r3n3r0/nexo/internal/domain"
)

func TestNewMirrorerRequiresResolver(t *testing.T) {
	_, err := cli.NewMirrorer(cli.MirrorDeps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

// events records the events of a mirror as "kind dest [error]" lines.
type events struct {
	mu    sync.Mutex
	lines []string
}

func (e *events) report(ev cli.MirrorEvent) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	line := string(ev.Kind) + " " + ev.Dest.String()
	if ev.Err != nil {
		line += " failed"
	}
	e.lines = append(e.lines, line)
	return nil
}

func (e *events) has(line string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, l := range e.lines {
		if l == line {
			return true
		}
	}
	return false
}

// TestMirrorerPlanApply: the engine of nexoctl mirror compares both sides,
// copies what is missing, keeps what differs without overwrite, removes
// what is gone with remove, reports a name it cannot write as a failure of
// that path only, and does nothing in a dry run.
func TestMirrorerPlanApply(t *testing.T) {
	ctx := context.Background()
	newEnv := func(t *testing.T) (*fixture, cli.Mirrorer) {
		f := newFixture(t)
		f.mem.MakeBucket("bkt")
		f.mem.PutObject("bkt", "site/a.txt", "AAA", clitest.ObjectSpec{})
		f.mem.PutObject("bkt", "site/b.txt", "new", clitest.ObjectSpec{})
		f.mem.PutObject("bkt", "site/../../escaped.txt", "evil", clitest.ObjectSpec{})
		f.write(t, "out/b.txt", "old content")
		f.write(t, "out/gone.txt", "x")
		m, err := cli.NewMirrorer(cli.MirrorDeps{Resolve: f.resolve})
		require.NoError(t, err)
		return f, m
	}
	t.Run("dry run", func(t *testing.T) {
		f, m := newEnv(t)
		req := cli.MirrorRequest{Source: f.target(t, "mem/bkt/site"), Target: f.target(t, "out"), DryRun: true,
			Options: cli.MirrorOptions{Remove: true}}
		plan, err := m.Plan(ctx, req)
		require.NoError(t, err)
		assert.Equal(t, int64(len("AAA")+len("evil")), plan.Bytes, "copies and overwrites")
		var ev events
		stats, err := m.Apply(ctx, req, plan, ev.report)
		require.NoError(t, err)
		assert.Equal(t, cli.MirrorStats{Copied: 1, Bytes: 3, Removed: 1, Conflicts: 1, Failed: 1, First: stats.First}, stats)
		require.ErrorIs(t, stats.First, cli.ErrUnsafeName)
		assert.Equal(t, "old content", f.read(t, "out/b.txt"))
		assert.FileExists(t, filepath.Join(f.dir, "out", "gone.txt"), "a dry run changes nothing")
		assert.NoFileExists(t, filepath.Join(f.dir, "out", "a.txt"))
	})
	t.Run("apply", func(t *testing.T) {
		f, m := newEnv(t)
		req := cli.MirrorRequest{Source: f.target(t, "mem/bkt/site"), Target: f.target(t, "out"),
			Options: cli.MirrorOptions{Remove: true, Overwrite: true}}
		plan, err := m.Plan(ctx, req)
		require.NoError(t, err)
		var ev events
		stats, err := m.Apply(ctx, req, plan, ev.report)
		require.NoError(t, err)
		assert.Equal(t, 2, stats.Copied)
		assert.Equal(t, 1, stats.Removed)
		assert.Equal(t, 1, stats.Failed)
		assert.Equal(t, "AAA", f.read(t, "out/a.txt"))
		assert.Equal(t, "new", f.read(t, "out/b.txt"), "overwritten")
		assert.NoFileExists(t, filepath.Join(f.dir, "out", "gone.txt"))
		assert.NoFileExists(t, filepath.Join(f.dir, "escaped.txt"), "never written outside the target")
		assert.True(t, ev.has("overwrite "+f.target(t, "out/b.txt").String()))
		assert.True(t, ev.has("remove "+f.target(t, "out/gone.txt").String()))

		// A second pass transfers nothing.
		plan, err = m.Plan(ctx, req)
		require.NoError(t, err)
		for _, a := range plan.Actions {
			assert.Equal(t, "../../escaped.txt", a.Rel, "only the unsafe name is left")
		}
	})
	t.Run("the reporter ends the pass", func(t *testing.T) {
		f, m := newEnv(t)
		req := cli.MirrorRequest{Source: f.target(t, "mem/bkt/site"), Target: f.target(t, "out")}
		plan, err := m.Plan(ctx, req)
		require.NoError(t, err)
		stop := errors.New("stop")
		_, err = m.Apply(ctx, req, plan, func(cli.MirrorEvent) error { return stop })
		assert.ErrorIs(t, err, stop)
	})
}

// TestMirrorerWatch: changes of the local source are copied and removed
// on the target; a different target is kept without overwrite.
func TestMirrorerWatch(t *testing.T) {
	f := newFixture(t)
	f.mem.MakeBucket("bkt")
	f.write(t, "src/keep.txt", "k")
	f.mem.PutObject("bkt", "dst/conflict.txt", "remote", clitest.ObjectSpec{})
	m, err := cli.NewMirrorer(cli.MirrorDeps{Resolve: f.resolve}, cli.WithWatchDebounce(20*time.Millisecond))
	require.NoError(t, err)
	req := cli.MirrorRequest{Source: f.target(t, "src"), Target: f.target(t, "mem/bkt/dst"), Options: cli.MirrorOptions{Remove: true}}

	// Only a local source can be watched.
	remote := req
	remote.Source = f.target(t, "mem/bkt/dst")
	require.ErrorIs(t, m.Watch(context.Background(), remote, nil, nil, nil), cli.ErrUsage)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var ev events
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- m.Watch(ctx, req, func() error { close(ready); return nil }, ev.report, func(err error) { t.Logf("watch: %v", err) })
	}()
	<-ready
	dst := func(key string) string { return f.target(t, "mem/bkt/dst/"+key).String() }

	f.write(t, "src/new.txt", "fresh")
	assert.Eventually(t, func() bool { return ev.has("copy " + dst("new.txt")) }, 5*time.Second, 10*time.Millisecond)
	data, ok := f.mem.Object("bkt", "dst/new.txt")
	assert.True(t, ok)
	assert.Equal(t, "fresh", data)

	f.write(t, "src/conflict.txt", "local, different")
	assert.Eventually(t, func() bool { return ev.has("conflict " + dst("conflict.txt")) }, 5*time.Second, 10*time.Millisecond)
	data, _ = f.mem.Object("bkt", "dst/conflict.txt")
	assert.Equal(t, "remote", data, "kept without overwrite")

	require.NoError(t, os.Remove(filepath.Join(f.dir, "src", "new.txt")))
	assert.Eventually(t, func() bool { return ev.has("remove " + dst("new.txt")) }, 5*time.Second, 10*time.Millisecond)
	_, ok = f.mem.Object("bkt", "dst/new.txt")
	assert.False(t, ok)

	cancel()
	require.NoError(t, <-done)
}
