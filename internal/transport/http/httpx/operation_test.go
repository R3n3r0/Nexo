package httpx_test

import (
	"context"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"

	"github.com/r3n3r0/nexo/internal/transport/http/httpx"
)

func TestOperationHolder(t *testing.T) {
	// Without a holder the name is dropped.
	httpx.SetOperationName(context.Background(), "ignored")
	assert.Empty(t, httpx.OperationNameFrom(context.Background()))
	var nilHolder *httpx.OperationHolder
	assert.Empty(t, nilHolder.Name())

	ctx, holder := httpx.WithOperationHolder(context.Background())
	var wg sync.WaitGroup
	for _, name := range []string{"GetObject", "PutObject"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			httpx.SetOperationName(ctx, name)
		}()
	}
	wg.Wait()
	assert.Contains(t, []string{"GetObject", "PutObject"}, holder.Name())
	httpx.SetOperationName(ctx, "ListObjectsV2")
	assert.Equal(t, "ListObjectsV2", holder.Name())
	assert.Equal(t, "ListObjectsV2", httpx.OperationNameFrom(ctx))

	assert.Empty(t, nilHolder.Throttled())
	httpx.SetOperationThrottled(context.Background(), "ip")
	assert.Empty(t, holder.Throttled())
	httpx.SetOperationThrottled(ctx, "bucket")
	assert.Equal(t, "bucket", holder.Throttled())
}

func TestRoutesFunc(t *testing.T) {
	var got chi.Router
	r := chi.NewRouter()
	httpx.RoutesFunc(func(router chi.Router) { got = router }).Register(r)
	assert.Equal(t, chi.Router(r), got)
}
