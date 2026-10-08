package lifecycle_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/lifecycle"
)

type recorder struct {
	mu     sync.Mutex
	events []string
}

func (r *recorder) add(e string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recorder) runner(name string, startErr error) lifecycle.Runner {
	return lifecycle.Funcs{
		OnStart: func(context.Context) error {
			r.add("start:" + name)
			return startErr
		},
		OnStop: func(context.Context) error {
			r.add("stop:" + name)
			return nil
		},
	}
}

func TestGroupStartsInOrderAndStopsInReverse(t *testing.T) {
	rec := &recorder{}
	g := lifecycle.NewGroup()
	g.Add("a", rec.runner("a", nil)).Add("b", rec.runner("b", nil)).Add("c", rec.runner("c", nil))
	require.Equal(t, 3, g.Len())

	require.NoError(t, g.Start(context.Background()))
	require.NoError(t, g.Stop(context.Background()))
	assert.Equal(t, []string{"start:a", "start:b", "start:c", "stop:c", "stop:b", "stop:a"}, rec.events)

	// Stopping again is a no-op.
	require.NoError(t, g.Stop(context.Background()))
	assert.Len(t, rec.events, 6)
}

func TestGroupStartFailureStopsStarted(t *testing.T) {
	rec := &recorder{}
	boom := errors.New("boom")
	g := lifecycle.NewGroup()
	g.Add("a", rec.runner("a", nil)).Add("b", rec.runner("b", boom)).Add("c", rec.runner("c", nil))

	err := g.Start(context.Background())
	require.ErrorIs(t, err, boom)
	assert.Contains(t, err.Error(), "start b")
	assert.Equal(t, []string{"start:a", "start:b", "stop:a"}, rec.events)
}

func TestGroupStopTimeout(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	slow := lifecycle.Funcs{OnStop: func(ctx context.Context) error {
		select {
		case <-block:
		case <-ctx.Done():
		}
		return nil
	}}
	g := lifecycle.NewGroup(lifecycle.WithStopTimeout(20 * time.Millisecond))
	g.Add("slow", slow)
	require.NoError(t, g.Start(context.Background()))
	err := g.Stop(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Contains(t, err.Error(), "stop slow")
}

func TestGroupCollectsStopErrors(t *testing.T) {
	e1, e2 := errors.New("e1"), errors.New("e2")
	g := lifecycle.NewGroup()
	g.Add("one", lifecycle.Funcs{OnStop: func(context.Context) error { return e1 }})
	g.Add("two", lifecycle.Funcs{OnStop: func(context.Context) error { return e2 }})
	require.NoError(t, g.Start(context.Background()))
	err := g.Stop(context.Background())
	assert.ErrorIs(t, err, e1)
	assert.ErrorIs(t, err, e2)
}

func TestGroupIgnoresNilAndDoubleStart(t *testing.T) {
	g := lifecycle.NewGroup()
	g.Add("nil", nil)
	assert.Equal(t, 0, g.Len())
	g.Add("x", lifecycle.Funcs{})
	require.NoError(t, g.Start(context.Background()))
	assert.Error(t, g.Start(context.Background()))
	require.NoError(t, g.Stop(context.Background()))
}

func TestGroupCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	g := lifecycle.NewGroup()
	g.Add("x", lifecycle.Funcs{})
	assert.ErrorIs(t, g.Start(ctx), context.Canceled)
}
