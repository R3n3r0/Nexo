package admin_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/config"
	configmock "github.com/r3n3r0/nexo/internal/service/config/mock"
)

func newConfigManager(t *testing.T) (admin.ConfigManager, *configmock.Service) {
	store := configmock.NewService(t)
	m, err := admin.NewConfigManager(admin.ConfigManagerDeps{Config: store})
	require.NoError(t, err)
	return m, store
}

func TestConfigManagerValidation(t *testing.T) {
	tests := []struct {
		subsystem string
		kv        config.KV
		ok        bool
	}{
		{"api", config.KV{"cors_allow_origin": "https://a.example, https://b.example", "requests_max": "100", "requests_deadline": "30s", "root_access": "off"}, true},
		{"api", config.KV{"cors_allow_origin": "a,,b"}, false},
		{"api", config.KV{"requests_max": "-1"}, false},
		{"api", config.KV{"requests_deadline": "soon"}, false},
		{"api", config.KV{"root_access": "maybe"}, false},
		{"ratelimit", config.KV{"s3_ip": "100", "s3_ip_burst": "200", "admin_user": "5"}, true},
		{"ratelimit", config.KV{"s3_bucket": "-5"}, false},
		{"ratelimit", config.KV{"s3_user": "5"}, false},
		{"browser", config.KV{"session_duration": "2h"}, true},
		{"logger", config.KV{"level": "debug", "format": "json"}, true},
		{"logger", config.KV{"level": "loud"}, false},
		{"notify_webhook", config.KV{"enable": "on", "endpoint": "https://hooks.example.com/x"}, true},
		{"notify_webhook", config.KV{"endpoint": "ftp://x"}, false},
		{"notify_webhook", config.KV{"unknown_key": "x"}, false},
		{"notify_webhook:primary", config.KV{"enable": "on", "endpoint": "https://hooks.example.com/x", "tls_skip_verify": "on"}, true},
		{"notify_webhook:", config.KV{"enable": "on"}, false},
		{"notify_webhook:q", config.KV{"queue_dir": "webhooks/q"}, true},
		{"notify_webhook:q", config.KV{"queue_dir": "/var/lib/nexo/drive1"}, false},
		{"notify_webhook:q", config.KV{"queue_dir": "../../drive1"}, false},
		{"notify_kafka:k1", config.KV{"brokers": "b1:9092,b2:9092", "topic": "events", "sasl_mechanism": "sha512"}, true},
		{"notify_kafka", config.KV{"sasl_mechanism": "md5"}, false},
		{"notify_nats", config.KV{"address": "127.0.0.1:4222", "subject": "nexo"}, true},
		{"notify_amqp", config.KV{"url": "amqp://mq:5672", "delivery_mode": "2"}, true},
		{"notify_redis:r", config.KV{"address": "127.0.0.1:6379", "key": "events", "format": "access"}, true},
		{"notify_postgres", config.KV{"connection_string": "postgres://db/nexo", "table": "events", "format": "csv"}, false},
		{"region", config.KV{"name": "eu-south-1", "comment": ""}, true},
		{"scanner", config.KV{"speed": "slow", "cycle": "5m"}, true},
		{"scanner", config.KV{"speed": "warp"}, false},
		{"nope", config.KV{"a": "b"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.subsystem, func(t *testing.T) {
			m, store := newConfigManager(t)
			if tt.ok {
				store.EXPECT().Set(mock.Anything, tt.subsystem, tt.kv).Return(nil)
				store.EXPECT().Get(mock.Anything, tt.subsystem).Return(tt.kv, nil)
			}
			_, err := m.Set(context.Background(), tt.subsystem, tt.kv)
			if tt.ok {
				assert.NoError(t, err)
			} else {
				assert.ErrorIs(t, err, domain.ErrInvalidArgument)
			}
		})
	}
}

func TestConfigManagerMasksSecrets(t *testing.T) {
	ctx := context.Background()
	m, store := newConfigManager(t)
	store.EXPECT().Get(mock.Anything, "notify_webhook").Return(config.KV{"auth_token": "s3cret", "endpoint": "https://x.example"}, nil)
	kv, err := m.Get(ctx, "notify_webhook")
	require.NoError(t, err)
	assert.Equal(t, admin.SensitiveMask, kv["auth_token"])
	assert.Equal(t, "https://x.example", kv["endpoint"])

	// The mask written back keeps the stored secret: only the other keys
	// are merged.
	store.EXPECT().Set(mock.Anything, "notify_webhook", config.KV{"enable": "on"}).Return(nil)
	_, err = m.Set(ctx, "notify_webhook", config.KV{"auth_token": admin.SensitiveMask, "enable": "on"})
	require.NoError(t, err)
}

// TestConfigManagerMasksEndpointCredentials: a webhook endpoint whose
// query or user info carries a token is masked like the other secrets, and
// the mask written back keeps the stored URL.
func TestConfigManagerMasksEndpointCredentials(t *testing.T) {
	tests := []struct {
		endpoint string
		masked   bool
	}{
		{endpoint: "https://hooks.example.com/ingest", masked: false},
		{endpoint: "https://hooks.example.com/ingest?token=s3cret", masked: true},
		{endpoint: "https://user:s3cret@hooks.example.com/ingest", masked: true},
		{endpoint: "https://hooks.example.com/ingest?", masked: true},
	}
	for _, tt := range tests {
		t.Run(tt.endpoint, func(t *testing.T) {
			ctx := context.Background()
			m, store := newConfigManager(t)
			store.EXPECT().Get(mock.Anything, "notify_webhook").Return(config.KV{"endpoint": tt.endpoint}, nil)
			kv, err := m.Get(ctx, "notify_webhook")
			require.NoError(t, err)
			if !tt.masked {
				assert.Equal(t, tt.endpoint, kv["endpoint"])
				return
			}
			assert.Equal(t, admin.SensitiveMask, kv["endpoint"])
			store.EXPECT().Set(mock.Anything, "notify_webhook", config.KV{"enable": "on"}).Return(nil)
			_, err = m.Set(ctx, "notify_webhook", config.KV{"endpoint": admin.SensitiveMask, "enable": "on"})
			require.NoError(t, err)
		})
	}
}

// TestConfigManagerMasksAddressCredentials: a NATS or Redis address given
// as a URL with credentials (user info, query) is masked by GET /config
// like the other secrets, a plain host:port is not, and the mask written
// back keeps the stored address.
func TestConfigManagerMasksAddressCredentials(t *testing.T) {
	tests := []struct {
		subsystem, address string
		masked             bool
	}{
		{subsystem: "notify_nats", address: "nats.example.com:4222", masked: false},
		{subsystem: "notify_nats", address: "nats://nats.example.com:4222", masked: false},
		{subsystem: "notify_nats", address: "nats://user:s3cret@nats.example.com:4222", masked: true},
		{subsystem: "notify_nats", address: "nats://s3cret-token@nats.example.com:4222", masked: true},
		{subsystem: "notify_redis", address: "redis.example.com:6379", masked: false},
		{subsystem: "notify_redis", address: "redis://:s3cret@redis.example.com:6379", masked: true},
		{subsystem: "notify_redis", address: "rediss://redis.example.com:6379/0?password=s3cret", masked: true},
	}
	for _, tt := range tests {
		t.Run(tt.subsystem+" "+tt.address, func(t *testing.T) {
			ctx := context.Background()
			m, store := newConfigManager(t)
			store.EXPECT().Get(mock.Anything, tt.subsystem).Return(config.KV{"address": tt.address}, nil)
			kv, err := m.Get(ctx, tt.subsystem)
			require.NoError(t, err)
			if !tt.masked {
				assert.Equal(t, tt.address, kv["address"])
				return
			}
			assert.Equal(t, admin.SensitiveMask, kv["address"])
			for _, v := range kv {
				assert.NotContains(t, v, "s3cret")
			}
			store.EXPECT().Set(mock.Anything, tt.subsystem, config.KV{"enable": "on"}).Return(nil)
			_, err = m.Set(ctx, tt.subsystem, config.KV{"address": admin.SensitiveMask, "enable": "on"})
			require.NoError(t, err, "the mask written back keeps the stored address")
		})
	}
}

func TestConfigManagerListAndSchema(t *testing.T) {
	ctx := context.Background()
	m, store := newConfigManager(t)
	schema := m.Schema(ctx)
	names := make([]string, 0, len(schema))
	for _, s := range schema {
		names = append(names, s.Name)
	}
	assert.Equal(t, []string{"access", "api", "audit_kafka", "audit_webhook", "browser", "logger", "notify_amqp", "notify_kafka", "notify_media_worker", "notify_nats",
		"notify_postgres", "notify_redis", "notify_webhook", "ratelimit", "region", "scanner", "storage"}, names)
	// Every key says whether the server applies it (regression: keys were
	// shown as if they took effect while nothing read them).
	applied := map[string]bool{}
	for _, s := range schema {
		for _, k := range s.Keys {
			applied[s.Name+"."+k.Name] = k.Applied
		}
	}
	for _, k := range []string{"api.root_access", "scanner.speed", "scanner.cycle", "access.block_public", "access.block_public_mode",
		"logger.level", "logger.format", "audit_webhook.endpoint", "audit_kafka.topic",
		"api.requests_max", "api.requests_deadline", "ratelimit.s3_ip", "ratelimit.s3_ip_burst", "ratelimit.admin_user"} {
		assert.True(t, applied[k], k)
	}
	for _, k := range []string{"browser.session_duration", "region.name", "audit_webhook.comment"} {
		assert.False(t, applied[k], k)
	}
	store.EXPECT().Get(mock.Anything, mock.Anything).Return(config.KV{}, nil)
	all, err := m.List(ctx)
	require.NoError(t, err)
	assert.Len(t, all, len(schema), "every known subsystem, configured or not")
}

func TestConfigManagerSetAllDeleteImport(t *testing.T) {
	ctx := context.Background()
	m, store := newConfigManager(t)
	err := m.SetAll(ctx, map[string]config.KV{"region": {"name": "x"}, "logger": {"level": "nope"}})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument, "validated before anything is written")
	store.EXPECT().Set(mock.Anything, "logger", config.KV{"level": "warn"}).Return(nil)
	store.EXPECT().Set(mock.Anything, "region", config.KV{"name": "x"}).Return(nil)
	require.NoError(t, m.SetAll(ctx, map[string]config.KV{"region": {"name": "x"}, "logger": {"level": "warn"}}))

	assert.ErrorIs(t, m.Delete(ctx, "region", []string{"nope"}), domain.ErrInvalidArgument)
	store.EXPECT().Delete(mock.Anything, "region", "name").Return(nil)
	require.NoError(t, m.Delete(ctx, "region", []string{"name"}))

	assert.ErrorIs(t, m.Import(ctx, []byte(`{"logger":{"level":"loud"}}`)), domain.ErrInvalidArgument)
	assert.ErrorIs(t, m.Import(ctx, []byte(`not json`)), domain.ErrInvalidArgument)
	data := []byte(`{"logger":{"level":"info"}}`)
	store.EXPECT().Import(mock.Anything, data).Return(nil)
	require.NoError(t, m.Import(ctx, data))
	store.EXPECT().Export(mock.Anything).Return(data, nil)
	out, err := m.Export(ctx)
	require.NoError(t, err)
	assert.Equal(t, data, out)
}
