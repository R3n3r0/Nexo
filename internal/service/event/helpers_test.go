package event_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/service/event"
	eventmock "github.com/r3n3r0/nexo/internal/service/event/mock"
)

// bucketRules returns a BucketRules mock serving the given documents (no
// configuration for the other buckets).
func bucketRules(t *testing.T, docs map[string]string) *eventmock.BucketRules {
	t.Helper()
	m := eventmock.NewBucketRules(t)
	m.EXPECT().NotificationConfig(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, bucket string) ([]byte, error) {
		if doc, ok := docs[bucket]; ok {
			return []byte(doc), nil
		}
		return nil, nil
	}).Maybe()
	return m
}

// webhookTarget is the enabled configuration of a webhook target.
func webhookTarget(id, endpoint string) event.TargetConfig {
	return event.TargetConfig{ID: id, Type: event.TypeWebhook, Enabled: true, Webhook: event.WebhookConfig{Endpoint: endpoint}}
}

// stop stops the dispatcher at the end of the test.
func stop(t *testing.T, d event.Dispatcher) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, d.Stop(ctx))
}

// newCaptureTarget returns a Target mock forwarding the sent events to got.
func newCaptureTarget(t *testing.T, id string, got chan<- event.Event) *eventmock.Target {
	t.Helper()
	m := eventmock.NewTarget(t)
	m.EXPECT().ID().Return(event.TargetID{ID: id, Type: event.TypeWebhook}).Maybe()
	m.EXPECT().Send(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, e event.Event) error {
		got <- e
		return nil
	}).Maybe()
	m.EXPECT().Close(mock.Anything).Return(nil).Maybe()
	return m
}

// factoryOf returns a TargetFactory mock building the given targets by id.
func factoryOf(t *testing.T, targets ...*eventmock.Target) *eventmock.TargetFactory {
	t.Helper()
	byID := map[string]event.Target{}
	for _, tg := range targets {
		byID[tg.ID().ID] = tg
	}
	f := eventmock.NewTargetFactory(t)
	f.EXPECT().New(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, cfg event.TargetConfig) (event.Target, error) {
		tg, ok := byID[cfg.ID]
		require.True(t, ok, "unexpected target %s", cfg.ID)
		return tg, nil
	}).Maybe()
	return f
}

// receive waits for an event.
func receive(t *testing.T, ch <-chan event.Event) event.Event {
	t.Helper()
	select {
	case e := <-ch:
		return e
	case <-time.After(5 * time.Second):
		t.Fatal("no event delivered")
		return event.Event{}
	}
}

// nothing asserts that no event arrives within d.
func nothing(t *testing.T, ch <-chan event.Event, d time.Duration) {
	t.Helper()
	select {
	case e := <-ch:
		t.Fatalf("unexpected event %s %s", e.EventName, e.S3.Object.Key)
	case <-time.After(d):
	}
}

// objectEvent builds an event on bucket/key.
func objectEvent(name event.Name, bucket, key string) event.Event {
	e := event.NewEvent(name, "us-east-1", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	e.S3.Bucket.Name = bucket
	e.S3.Object.Key = key
	return e
}
