package middleware_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/r3n3r0/nexo/internal/transport/http/middleware"
)

func TestActivityLoad(t *testing.T) {
	ctx := context.Background()
	a := middleware.NewActivity(middleware.ActivityDeps{}, middleware.WithActivityCapacity(4))
	assert.Zero(t, a.Load(ctx), "idle")

	entered, release := make(chan struct{}), make(chan chan struct{}, 1)
	release <- make(chan struct{})
	h := a.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		g := <-release
		release <- g
		entered <- struct{}{}
		<-g
	}))
	done := make(chan struct{})
	for range 2 {
		go func() {
			serve(h, "/b/k")
			done <- struct{}{}
		}()
		<-entered
	}
	assert.InDelta(t, 0.5, a.Load(ctx), 1e-9, "two requests in progress")
	close(<-release)
	release <- make(chan struct{})
	<-done
	<-done
	// The requests in progress at the previous sample count once more,
	// then nothing is.
	assert.InDelta(t, 0.5, a.Load(ctx), 1e-9)
	assert.Zero(t, a.Load(ctx))

	// A burst that ended between two samples is still seen once.
	for range 8 {
		go func() {
			serve(h, "/b/k")
			done <- struct{}{}
		}()
	}
	for range 8 {
		<-entered
	}
	close(<-release)
	for range 8 {
		<-done
	}
	assert.InDelta(t, 1.0, a.Load(ctx), 1e-9, "the peak, capped at 1")
	assert.Zero(t, a.Load(ctx))
}
