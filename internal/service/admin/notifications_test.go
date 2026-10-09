package admin_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/admin"
	bucketmock "github.com/r3n3r0/nexo/internal/service/bucket/mock"
	"github.com/r3n3r0/nexo/internal/service/config"
	configmock "github.com/r3n3r0/nexo/internal/service/config/mock"
	"github.com/r3n3r0/nexo/internal/service/event"
	eventmock "github.com/r3n3r0/nexo/internal/service/event/mock"
)

const hookARN = "arn:nexo:sqs:us-east-1:_:webhook"

// newNotificationEnv returns a manager validating against a real
// dispatcher (not started) with the default webhook target configured.
func newNotificationEnv(t *testing.T) (admin.NotificationManager, *bucketmock.Service) {
	t.Helper()
	ctx := context.Background()
	d := event.NewDispatcher(event.Deps{}, event.WithRegion("us-east-1"))
	require.NoError(t, d.SetTarget(ctx, event.TargetConfig{ID: event.DefaultTargetID, Type: event.TypeWebhook, Enabled: true,
		Webhook: event.WebhookConfig{Endpoint: "http://hook.local/events"}}))
	buckets := bucketmock.NewService(t)
	mgr, err := admin.NewNotificationManager(admin.NotificationManagerDeps{Buckets: buckets, Events: d}, admin.WithARNRegion("us-east-1"))
	require.NoError(t, err)
	return mgr, buckets
}

func TestNewNotificationManagerRequiresDeps(t *testing.T) {
	_, err := admin.NewNotificationManager(admin.NotificationManagerDeps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

func TestNotificationsRoundTrip(t *testing.T) {
	ctx := context.Background()
	mgr, buckets := newNotificationEnv(t)
	var stored []byte
	buckets.EXPECT().GetVersioning(mock.Anything, "bkt").Return(domain.VersioningOff, nil)
	buckets.EXPECT().SetNotification(mock.Anything, "bkt", mock.Anything).RunAndReturn(func(_ context.Context, _ string, doc []byte) error {
		stored = doc
		return nil
	})
	got, err := mgr.Set(ctx, "bkt", admin.NotificationConfig{Subscriptions: []admin.Subscription{
		{ID: "images", ARN: hookARN, Events: []string{"s3:ObjectCreated:*", " s3:ObjectRemoved:Delete "}, Prefix: "img/", Suffix: ".jpg"},
		{ARN: "arn:nexo:sqs::_:webhook", Events: []string{"s3:ObjectAccessed:Get"}},
	}})
	require.NoError(t, err)
	require.Len(t, got.Subscriptions, 2)
	assert.Equal(t, admin.Subscription{ID: "images", ARN: hookARN, Events: []string{"s3:ObjectCreated:*", "s3:ObjectRemoved:Delete"}, Prefix: "img/", Suffix: ".jpg"}, got.Subscriptions[0])
	assert.NotEmpty(t, got.Subscriptions[1].ID, "a subscription without ID gets one")
	assert.Equal(t, "arn:nexo:sqs::_:webhook", got.Subscriptions[1].ARN, "an ARN without region matches any")
	assert.Contains(t, string(stored), "<QueueConfiguration>")

	buckets.EXPECT().GetNotification(mock.Anything, "bkt").Return(stored, nil)
	again, err := mgr.Get(ctx, "bkt")
	require.NoError(t, err)
	assert.Equal(t, got, again)
}

func TestNotificationsValidationPerSubscription(t *testing.T) {
	ctx := context.Background()
	mgr, buckets := newNotificationEnv(t)
	buckets.EXPECT().GetVersioning(mock.Anything, "bkt").Return(domain.VersioningOff, nil)
	_, err := mgr.Set(ctx, "bkt", admin.NotificationConfig{Subscriptions: []admin.Subscription{
		{ID: "ok", ARN: hookARN, Events: []string{"s3:ObjectCreated:Put"}},
		{ID: "missing", ARN: "arn:nexo:sqs:us-east-1:other:webhook", Events: []string{"s3:ObjectCreated:Put"}},
		{ID: "event", ARN: hookARN, Events: []string{"s3:Replication:*"}},
		{ID: "ok", ARN: hookARN, Events: []string{"s3:ObjectRemoved:*"}},
		{ID: "region", ARN: "arn:nexo:sqs:eu-west-1:_:webhook", Events: []string{"s3:ObjectCreated:*"}},
		{ID: "none", ARN: hookARN},
		{ID: "bad", ARN: "not-an-arn", Events: []string{"s3:ObjectCreated:*"}},
	}})
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
	problems, ok := admin.RuleProblemsOf(err)
	require.True(t, ok)
	got := map[int]string{}
	for _, p := range problems {
		got[p.Rule] = p.ID
		assert.Equal(t, "InvalidArgument", p.Code, p.Message)
	}
	assert.Equal(t, map[int]string{2: "missing", 3: "event", 4: "ok", 5: "region", 6: "none", 7: "bad"}, got)
}

func TestNotificationsDelete(t *testing.T) {
	ctx := context.Background()
	mgr, buckets := newNotificationEnv(t)
	buckets.EXPECT().SetNotification(mock.Anything, "bkt", mock.Anything).RunAndReturn(func(_ context.Context, _ string, doc []byte) error {
		cfg, err := event.ParseBucketConfig(doc, "")
		require.NoError(t, err)
		assert.Empty(t, cfg.Rules)
		return nil
	}).Twice()
	require.NoError(t, mgr.Delete(ctx, "bkt"))
	buckets.EXPECT().GetVersioning(mock.Anything, "bkt").Return(domain.VersioningOff, nil)
	got, err := mgr.Set(ctx, "bkt", admin.NotificationConfig{})
	require.NoError(t, err)
	assert.Empty(t, got.Subscriptions)
}

// TestNotificationTargets: the targets of the dispatcher with their state
// and the disabled targets of the configuration, without secrets.
func TestNotificationTargets(t *testing.T) {
	ctx := context.Background()
	events := eventmock.NewService(t)
	cfgSvc := configmock.NewService(t)
	mgr, err := admin.NewNotificationManager(admin.NotificationManagerDeps{Buckets: bucketmock.NewService(t), Events: events, Config: cfgSvc}, admin.WithARNRegion("us-east-1"))
	require.NoError(t, err)
	events.EXPECT().TargetsStatus(mock.Anything).Return([]event.TargetStatus{
		{Config: event.TargetConfig{ID: "_", Type: event.TypeWebhook, Enabled: true, Webhook: event.WebhookConfig{Endpoint: "https://user:secret@hook.local/x?token=t", AuthToken: "tok"}},
			Online: false, Pending: 3, LastError: "503"},
		{Config: event.TargetConfig{ID: "pg", Type: event.TypePostgres, Enabled: true, Postgres: event.PostgresConfig{ConnectionString: "postgres://u:p@db/x", Table: "events"}}, Online: true},
	}, nil)
	cfgSvc.EXPECT().List(mock.Anything).Return([]string{"api", "notify_webhook", "notify_kafka:k1", "notify_nats:bad"}, nil)
	cfgSvc.EXPECT().Get(mock.Anything, "notify_webhook").Return(config.KV{"enable": "on", "endpoint": "https://hook.local/x"}, nil)
	cfgSvc.EXPECT().Get(mock.Anything, "notify_kafka:k1").Return(config.KV{"enable": "off", "brokers": "b1:9092,b2:9092", "topic": "t", "sasl_password": "pw"}, nil)
	cfgSvc.EXPECT().Get(mock.Anything, "notify_nats:bad").Return(config.KV{"enable": "maybe"}, nil)

	got, err := mgr.Targets(ctx)
	require.NoError(t, err)
	assert.Equal(t, []admin.NotifyTarget{
		{ID: "_", Type: event.TypeWebhook, ARN: "arn:nexo:sqs:us-east-1:_:webhook", Enabled: true, Endpoint: "https://hook.local/x", Pending: 3, LastError: "503"},
		// A disabled target is not parsed further: no endpoint.
		{ID: "k1", Type: event.TypeKafka, ARN: "arn:nexo:sqs:us-east-1:k1:kafka"},
		{ID: "pg", Type: event.TypePostgres, ARN: "arn:nexo:sqs:us-east-1:pg:postgresql", Enabled: true, Endpoint: "table events", Online: true},
	}, got)
}

func TestNotificationTestTarget(t *testing.T) {
	ctx := context.Background()
	events := eventmock.NewService(t)
	mgr, err := admin.NewNotificationManager(admin.NotificationManagerDeps{Buckets: bucketmock.NewService(t), Events: events})
	require.NoError(t, err)
	hook := event.TargetID{ID: "_", Type: event.TypeWebhook}
	events.EXPECT().TestTarget(mock.Anything, hook).Return(event.TestResult{Delivered: true}, nil).Twice()
	events.EXPECT().TestTarget(mock.Anything, event.TargetID{ID: "k", Type: event.TypeKafka}).Return(event.TestResult{Error: "connect: refused"}, nil)
	events.EXPECT().TestTarget(mock.Anything, event.TargetID{ID: "x", Type: event.TypeWebhook}).Return(event.TestResult{}, domain.ErrNotFound)

	tests := []struct {
		target string
		want   *admin.TargetTest
		err    error
	}{
		{target: "_:webhook", want: &admin.TargetTest{Target: "_:webhook", Delivered: true}},
		{target: hookARN, want: &admin.TargetTest{Target: "_:webhook", Delivered: true}},
		{target: "k:kafka", want: &admin.TargetTest{Target: "k:kafka", Error: "connect: refused"}},
		{target: "x:webhook", err: domain.ErrNotFound},
		{target: "nocolon", err: domain.ErrInvalidArgument},
		{target: "arn:nexo:sqs:r:x:smtp", err: domain.ErrInvalidArgument},
	}
	for _, tt := range tests {
		t.Run(tt.target, func(t *testing.T) {
			got, err := mgr.TestTarget(ctx, tt.target)
			if tt.err != nil {
				assert.ErrorIs(t, err, tt.err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestNotificationTargetEndpoints: the destination of every target type
// is described without the credentials its configuration holds.
func TestNotificationTargetEndpoints(t *testing.T) {
	tests := []struct {
		name string
		cfg  event.TargetConfig
		want string
	}{
		{"webhook", event.TargetConfig{Type: event.TypeWebhook, Webhook: event.WebhookConfig{Endpoint: "http://u:p@h:8080/path?sig=x#f"}}, "http://h:8080/path"},
		{"kafka", event.TargetConfig{Type: event.TypeKafka, Kafka: event.KafkaConfig{Brokers: []string{"b1:9092", "b2:9092"}, Topic: "t",
			SASL: event.SASLConfig{Password: "pw"}}}, "b1:9092,b2:9092 topic t"},
		{"nats url", event.TargetConfig{Type: event.TypeNATS, NATS: event.NATSConfig{Address: "nats://u:p@n:4222", Subject: "s", Token: "tok"}}, "nats://n:4222 subject s"},
		{"nats host", event.TargetConfig{Type: event.TypeNATS, NATS: event.NATSConfig{Address: "n:4222", Subject: "s"}}, "n:4222 subject s"},
		{"amqp", event.TargetConfig{Type: event.TypeAMQP, AMQP: event.AMQPConfig{URL: "amqps://guest:guest@rabbit:5671/vh", Exchange: "ex"}}, "amqps://rabbit:5671/vh exchange ex"},
		{"redis", event.TargetConfig{Type: event.TypeRedis, Redis: event.RedisConfig{Address: "user:pw@redis:6379", Key: "k", Password: "pw"}}, "redis:6379 key k"},
		{"postgres", event.TargetConfig{Type: event.TypePostgres, Postgres: event.PostgresConfig{ConnectionString: "postgres://u:p@db/x", Table: "t"}}, "table t"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events := eventmock.NewService(t)
			mgr, err := admin.NewNotificationManager(admin.NotificationManagerDeps{Buckets: bucketmock.NewService(t), Events: events})
			require.NoError(t, err)
			cfg := tt.cfg
			cfg.ID, cfg.Enabled = "x", true
			events.EXPECT().TargetsStatus(mock.Anything).Return([]event.TargetStatus{{Config: cfg, Online: true}}, nil)
			got, err := mgr.Targets(context.Background())
			require.NoError(t, err)
			require.Len(t, got, 1)
			assert.Equal(t, tt.want, got[0].Endpoint)
			assert.Equal(t, "arn:nexo:sqs::x:"+cfg.Type, got[0].ARN, "without region the ARN matches any")
		})
	}
}
