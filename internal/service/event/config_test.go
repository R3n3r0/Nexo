package event_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/event"
)

func TestIsTargetSubsystem(t *testing.T) {
	for s, want := range map[string]bool{
		"notify_webhook": true, "notify_webhook:primary": true, "notify_kafka:1": true, "notify_nats": true,
		"notify_amqp": true, "notify_redis:r": true, "notify_postgres:pg": true,
		"notify_mysql": false, "notify_webhook:": false, "notify_webhook:a:b": false, "scanner": false, "api": false,
	} {
		assert.Equal(t, want, event.IsTargetSubsystem(s), s)
	}
}

func TestParseTargetConfig(t *testing.T) {
	tests := []struct {
		name      string
		subsystem string
		kv        map[string]string
		want      event.TargetConfig
		wantErr   bool
	}{
		{
			name:      "default webhook",
			subsystem: "notify_webhook",
			kv:        map[string]string{"enable": "on", "endpoint": "https://hooks.example.com/s3", "auth_token": "secret", "tls_skip_verify": "on", "queue_dir": "/q", "queue_limit": "10"},
			want: event.TargetConfig{ID: "_", Type: event.TypeWebhook, Enabled: true, QueueDir: "/q", QueueLimit: 10,
				Webhook: event.WebhookConfig{Endpoint: "https://hooks.example.com/s3", AuthToken: "secret", TLSSkipVerify: true}},
		},
		{
			name:      "disabled target is not validated",
			subsystem: "notify_webhook:off",
			kv:        map[string]string{"enable": "off"},
			want:      event.TargetConfig{ID: "off", Type: event.TypeWebhook},
		},
		{
			name:      "kafka with sasl",
			subsystem: "notify_kafka:k1",
			kv:        map[string]string{"enable": "on", "brokers": "b1:9092, b2:9092", "topic": "events", "sasl": "on", "sasl_username": "u", "sasl_password": "p", "sasl_mechanism": "SHA512", "tls": "on"},
			want: event.TargetConfig{ID: "k1", Type: event.TypeKafka, Enabled: true, Kafka: event.KafkaConfig{
				Brokers: []string{"b1:9092", "b2:9092"}, Topic: "events", SASL: event.SASLConfig{Mechanism: "sha512", Username: "u", Password: "p"},
				TLS: event.TLSOptions{Enabled: true}}},
		},
		{
			name:      "nats",
			subsystem: "notify_nats:n",
			kv:        map[string]string{"enable": "on", "address": "127.0.0.1:4222", "subject": "nexo", "username": "u", "password": "p", "token": "t"},
			want: event.TargetConfig{ID: "n", Type: event.TypeNATS, Enabled: true, NATS: event.NATSConfig{
				Address: "127.0.0.1:4222", Subject: "nexo", Username: "u", Password: "p", Token: "t"}},
		},
		{
			name:      "amqp persistent",
			subsystem: "notify_amqp:a",
			kv:        map[string]string{"enable": "on", "url": "amqp://guest:guest@127.0.0.1:5672/", "exchange": "ex", "exchange_type": "topic", "routing_key": "rk", "durable": "on", "mandatory": "on", "delivery_mode": "2"},
			want: event.TargetConfig{ID: "a", Type: event.TypeAMQP, Enabled: true, AMQP: event.AMQPConfig{
				URL: "amqp://guest:guest@127.0.0.1:5672/", Exchange: "ex", ExchangeType: "topic", RoutingKey: "rk", Durable: true, Mandatory: true, Persistent: true}},
		},
		{
			name:      "redis",
			subsystem: "notify_redis:r",
			kv:        map[string]string{"enable": "on", "address": "127.0.0.1:6379", "key": "bucketevents", "format": "Namespace", "password": "pw", "user": "u"},
			want: event.TargetConfig{ID: "r", Type: event.TypeRedis, Enabled: true, Redis: event.RedisConfig{
				Address: "127.0.0.1:6379", Key: "bucketevents", Format: event.FormatNamespace, Username: "u", Password: "pw"}},
		},
		{
			name:      "postgres",
			subsystem: "notify_postgres:pg",
			kv:        map[string]string{"enable": "on", "connection_string": "postgres://u:p@db/nexo", "table": "public.events", "format": "access"},
			want: event.TargetConfig{ID: "pg", Type: event.TypePostgres, Enabled: true, Postgres: event.PostgresConfig{
				ConnectionString: "postgres://u:p@db/nexo", Table: "public.events", Format: event.FormatAccess}},
		},
		{name: "not a notify subsystem", subsystem: "scanner", wantErr: true},
		{name: "bad boolean", subsystem: "notify_webhook", kv: map[string]string{"enable": "maybe"}, wantErr: true},
		{name: "bad queue limit", subsystem: "notify_webhook", kv: map[string]string{"enable": "on", "endpoint": "http://h", "queue_limit": "-1"}, wantErr: true},
		{name: "webhook without endpoint", subsystem: "notify_webhook", kv: map[string]string{"enable": "on"}, wantErr: true},
		{name: "kafka without topic", subsystem: "notify_kafka", kv: map[string]string{"enable": "on", "brokers": "b:9092"}, wantErr: true},
		{name: "kafka bad mechanism", subsystem: "notify_kafka", kv: map[string]string{"enable": "on", "brokers": "b:9092", "topic": "t", "sasl": "on", "sasl_mechanism": "md5"}, wantErr: true},
		{name: "amqp bad url", subsystem: "notify_amqp", kv: map[string]string{"enable": "on", "url": "http://x"}, wantErr: true},
		{name: "amqp bad delivery mode", subsystem: "notify_amqp", kv: map[string]string{"enable": "on", "url": "amqp://x", "delivery_mode": "3"}, wantErr: true},
		{name: "redis bad format", subsystem: "notify_redis", kv: map[string]string{"enable": "on", "address": "a:1", "key": "k", "format": "json"}, wantErr: true},
		{name: "postgres injection in table", subsystem: "notify_postgres", kv: map[string]string{"enable": "on", "connection_string": "c", "table": "t; DROP TABLE x", "format": "access"}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := event.ParseTargetConfig(tc.subsystem, tc.kv)
			if tc.wantErr {
				require.Error(t, err)
				assert.ErrorIs(t, err, domain.ErrInvalidArgument)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
