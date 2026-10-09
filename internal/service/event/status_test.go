package event_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/event"
	eventmock "github.com/r3n3r0/nexo/internal/service/event/mock"
)

// TestDispatcherTargetsStatus: a target is online until a delivery fails,
// its pending events are counted, and a successful delivery (a test event
// here) brings it back online.
func TestDispatcherTargetsStatus(t *testing.T) {
	h := newHook(t)
	h.offline.Store(true)
	ctx := context.Background()
	cfg := webhookTarget("hook", h.srv.URL)
	cfg.Filter = event.Filter{Bucket: "b"}
	d := event.NewDispatcher(event.Deps{Factory: event.NewWebhookFactory(event.WebhookDeps{Client: h.srv.Client()})}, fastRetry(), event.WithRegion("eu-west-1"))
	require.NoError(t, d.SetTarget(ctx, cfg))
	require.NoError(t, d.SetTarget(ctx, webhookTarget("idle", h.srv.URL)))
	require.NoError(t, d.Start(ctx))
	defer stop(t, d)

	status, err := d.TargetsStatus(ctx)
	require.NoError(t, err)
	require.Len(t, status, 2)
	assert.Equal(t, "hook", status[0].Config.ID, "sorted by id")
	assert.True(t, status[0].Online, "online until a delivery fails")
	assert.Zero(t, status[0].Pending)

	for _, k := range []string{"1", "2"} {
		require.NoError(t, d.Publish(ctx, objectEvent(event.ObjectCreatedPut, "b", k)))
	}
	require.Eventually(t, func() bool {
		status, err = d.TargetsStatus(ctx)
		return err == nil && !status[0].Online && status[0].Pending == 2
	}, 5*time.Second, 5*time.Millisecond, "offline with 2 pending events: %+v", status)
	assert.Contains(t, status[0].LastError, "503")
	assert.True(t, status[1].Online, "the other target is not affected")

	res, err := d.TestTarget(ctx, event.TargetID{ID: "hook", Type: event.TypeWebhook})
	require.NoError(t, err)
	assert.False(t, res.Delivered)
	assert.Contains(t, res.Error, "503")

	h.offline.Store(false)
	res, err = d.TestTarget(ctx, event.TargetID{ID: "hook", Type: event.TypeWebhook})
	require.NoError(t, err)
	assert.True(t, res.Delivered)
	assert.Empty(t, res.Error)
	h.waitFor(t, 3)
	var tested bool
	for _, p := range h.payloads() {
		if p.EventName == event.TestEvent {
			tested = true
			require.Len(t, p.Records, 1)
			assert.Equal(t, "eu-west-1", p.Records[0].AwsRegion)
		}
	}
	assert.True(t, tested, "the test event is delivered: %v", h.keys())
	require.Eventually(t, func() bool {
		status, err = d.TargetsStatus(ctx)
		return err == nil && status[0].Online && status[0].Pending == 0 && status[0].LastError == ""
	}, 5*time.Second, 5*time.Millisecond, "back online, queue drained: %+v", status)

	_, err = d.TestTarget(ctx, event.TargetID{ID: "missing", Type: event.TypeWebhook})
	assert.ErrorIs(t, err, domain.ErrNotFound)
}

// TestTestEventKeepsNamespaces: the test event of a namespace target
// (Redis hash, PostgreSQL table) removes the entry of no object instead
// of adding one; an access target records it.
func TestTestEventKeepsNamespaces(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name   string
		format string
		expect func(c *eventmock.RedisConn)
	}{
		{"namespace", event.FormatNamespace, func(c *eventmock.RedisConn) {
			c.EXPECT().HDel(mock.Anything, "events", "/").Return(nil).Once()
		}},
		{"access", event.FormatAccess, func(c *eventmock.RedisConn) {
			c.EXPECT().RPush(mock.Anything, "events", mock.Anything).Return(nil).Once()
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			conn := eventmock.NewRedisConn(t)
			tc.expect(conn)
			conn.EXPECT().Close(mock.Anything).Return(nil).Maybe()
			dialer := eventmock.NewRedisDialer(t)
			dialer.EXPECT().Dial(mock.Anything, mock.Anything).Return(conn, nil)
			d := event.NewDispatcher(event.Deps{Factory: event.NewTargetFactory(event.FactoryDeps{Redis: dialer})})
			cfg := event.TargetConfig{ID: "r", Type: event.TypeRedis, Enabled: true,
				Redis: event.RedisConfig{Address: "127.0.0.1:6379", Key: "events", Format: tc.format}}
			require.NoError(t, d.SetTarget(ctx, cfg))
			res, err := d.TestTarget(ctx, cfg.TargetID())
			require.NoError(t, err)
			assert.True(t, res.Delivered, res.Error)
		})
	}
}
