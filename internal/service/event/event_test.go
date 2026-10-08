package event_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/event"
)

func TestNameMatch(t *testing.T) {
	assert.True(t, event.ObjectCreatedPut.Match(event.ObjectCreatedAll))
	assert.True(t, event.ObjectCreatedPut.Match(event.ObjectCreatedPut))
	assert.True(t, event.ObjectCreatedPut.Match("*"))
	assert.False(t, event.ObjectCreatedPut.Match(event.ObjectRemovedAll))
	assert.False(t, event.ObjectCreatedPut.Match(event.ObjectCreatedCopy))
}

func TestFilterMatches(t *testing.T) {
	e := event.NewEvent(event.ObjectCreatedPut, "us-east-1", time.Now())
	e.S3.Bucket.Name = "photos"
	e.S3.Object.Key = "2024/img.jpg"

	assert.True(t, event.Filter{}.Matches(e))
	assert.True(t, event.Filter{Bucket: "photos", Prefix: "2024/", Suffix: ".jpg", Events: []event.Name{event.ObjectCreatedAll}}.Matches(e))
	assert.False(t, event.Filter{Bucket: "other"}.Matches(e))
	assert.False(t, event.Filter{Prefix: "2023/"}.Matches(e))
	assert.False(t, event.Filter{Suffix: ".png"}.Matches(e))
	assert.False(t, event.Filter{Events: []event.Name{event.ObjectRemovedAll}}.Matches(e))
}

func TestDispatcherDeliversToWebhook(t *testing.T) {
	received := make(chan event.Payload, 4)
	var auth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.Store(r.Header.Get("Authorization"))
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		body, _ := io.ReadAll(r.Body)
		var p event.Payload
		require.NoError(t, json.Unmarshal(body, &p))
		received <- p
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx := context.Background()
	d := event.NewDispatcher(event.Deps{Factory: event.NewWebhookFactory(event.WebhookDeps{Client: srv.Client()})}, event.WithQueueSize(8))
	// Publishing before Start fails.
	assert.ErrorIs(t, d.Publish(ctx, event.Event{}), domain.ErrConflict)
	require.NoError(t, d.Start(ctx))
	assert.Error(t, d.Start(ctx), "double start")

	require.NoError(t, d.SetTarget(ctx, event.TargetConfig{
		ID: "hook", Type: event.TypeWebhook, Endpoint: srv.URL, AuthToken: "secret", Enabled: true,
		Filter: event.Filter{Events: []event.Name{event.ObjectCreatedAll}, Bucket: "photos"},
	}))
	require.NoError(t, d.SetTarget(ctx, event.TargetConfig{
		ID: "disabled", Type: event.TypeWebhook, Endpoint: srv.URL, Enabled: false,
	}))

	e := event.NewEvent(event.ObjectCreatedPut, "us-east-1", time.Now())
	e.S3.Bucket.Name = "photos"
	e.S3.Object.Key = "a.jpg"
	e.S3.Object.Size = 3
	require.NoError(t, d.Publish(ctx, e))

	other := event.NewEvent(event.ObjectRemovedDelete, "us-east-1", time.Now())
	other.S3.Bucket.Name = "photos"
	require.NoError(t, d.Publish(ctx, other)) // filtered out

	select {
	case p := <-received:
		assert.Equal(t, event.ObjectCreatedPut, p.EventName)
		assert.Equal(t, "photos/a.jpg", p.Key)
		require.Len(t, p.Records, 1)
		assert.Equal(t, int64(3), p.Records[0].S3.Object.Size)
		assert.Equal(t, event.Version, p.Records[0].EventVersion)
	case <-time.After(2 * time.Second):
		t.Fatal("webhook not called")
	}
	assert.Equal(t, "Bearer secret", auth.Load())

	stopCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	require.NoError(t, d.Stop(stopCtx))
	select {
	case <-received:
		t.Fatal("the filtered event must not be delivered")
	default:
	}
	assert.ErrorIs(t, d.Publish(ctx, e), domain.ErrConflict)
	require.NoError(t, d.Stop(stopCtx), "stop is idempotent")
}

func TestDispatcherTargetCRUD(t *testing.T) {
	ctx := context.Background()
	d := event.NewDispatcher(event.Deps{})
	assert.ErrorIs(t, d.SetTarget(ctx, event.TargetConfig{Type: event.TypeWebhook}), domain.ErrInvalidArgument)
	assert.ErrorIs(t, d.SetTarget(ctx, event.TargetConfig{ID: "x", Type: "kafka"}), domain.ErrInvalidArgument)
	assert.ErrorIs(t, d.SetTarget(ctx, event.TargetConfig{ID: "x", Type: event.TypeWebhook, Endpoint: "not a url"}), domain.ErrInvalidArgument)

	require.NoError(t, d.SetTarget(ctx, event.TargetConfig{ID: "b", Type: event.TypeWebhook, Endpoint: "http://b"}))
	require.NoError(t, d.SetTarget(ctx, event.TargetConfig{ID: "a", Type: event.TypeWebhook, Endpoint: "http://a"}))
	list, err := d.ListTargets(ctx)
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, "a", list[0].ID)

	got, err := d.GetTarget(ctx, "b")
	require.NoError(t, err)
	assert.Equal(t, "http://b", got.Endpoint)
	// Replace keeps a single entry.
	require.NoError(t, d.SetTarget(ctx, event.TargetConfig{ID: "b", Type: event.TypeWebhook, Endpoint: "http://b2"}))
	got, _ = d.GetTarget(ctx, "b")
	assert.Equal(t, "http://b2", got.Endpoint)

	require.NoError(t, d.RemoveTarget(ctx, "b"))
	_, err = d.GetTarget(ctx, "b")
	assert.ErrorIs(t, err, domain.ErrPolicyNotFound)
	assert.ErrorIs(t, d.RemoveTarget(ctx, "b"), domain.ErrPolicyNotFound)
}

func TestWebhookReportsHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusBadGateway)
	}))
	defer srv.Close()
	target, err := event.NewWebhookFactory(event.WebhookDeps{Client: srv.Client()}).New(context.Background(), event.TargetConfig{ID: "h", Type: event.TypeWebhook, Endpoint: srv.URL})
	require.NoError(t, err)
	assert.Equal(t, "h", target.ID())
	err = target.Send(context.Background(), event.NewEvent(event.ObjectCreatedPut, "r", time.Now()))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "502")
	assert.NoError(t, target.Close(context.Background()))
}

func TestDispatcherQueueFull(t *testing.T) {
	ctx := context.Background()
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block }))
	defer srv.Close()
	defer close(block)

	d := event.NewDispatcher(event.Deps{Factory: event.NewWebhookFactory(event.WebhookDeps{Client: srv.Client()})}, event.WithQueueSize(1), event.WithSendTimeout(5*time.Second))
	require.NoError(t, d.Start(ctx))
	require.NoError(t, d.SetTarget(ctx, event.TargetConfig{ID: "slow", Type: event.TypeWebhook, Endpoint: srv.URL, Enabled: true}))
	e := event.NewEvent(event.ObjectCreatedPut, "r", time.Now())
	// First event is picked by the worker and blocks; second fills the queue;
	// the third must be rejected.
	require.NoError(t, d.Publish(ctx, e))
	require.Eventually(t, func() bool { return d.Publish(ctx, e) != nil }, time.Second, 5*time.Millisecond)
	stopCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	assert.ErrorIs(t, d.Stop(stopCtx), context.DeadlineExceeded)
}
