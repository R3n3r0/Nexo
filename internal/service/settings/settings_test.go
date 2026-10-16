package settings_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/config"
	configmock "github.com/r3n3r0/nexo/internal/service/config/mock"
	"github.com/r3n3r0/nexo/internal/service/policy"
	policymock "github.com/r3n3r0/nexo/internal/service/policy/mock"
	"github.com/r3n3r0/nexo/internal/service/settings"
	settingsmock "github.com/r3n3r0/nexo/internal/service/settings/mock"
)

var errDown = errors.New("store down")

func TestNewRequiresConfig(t *testing.T) {
	_, err := settings.New(settings.Deps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

func TestAPI(t *testing.T) {
	tests := []struct {
		name string
		kv   config.KV
		want settings.API
	}{
		{name: "default", kv: config.KV{}, want: settings.API{RootAccess: true}},
		{name: "off", kv: config.KV{"root_access": "off"}, want: settings.API{RootAccess: false}},
		{name: "false", kv: config.KV{"root_access": "FALSE"}, want: settings.API{RootAccess: false}},
		{name: "on", kv: config.KV{"root_access": "on"}, want: settings.API{RootAccess: true}},
		{name: "reset to the default", kv: config.KV{"root_access": ""}, want: settings.API{RootAccess: true}},
		{name: "garbage keeps the default", kv: config.KV{"root_access": "maybe"}, want: settings.API{RootAccess: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := configmock.NewService(t)
			cfg.EXPECT().Get(mock.Anything, "api").Return(tt.kv, nil)
			s, err := settings.New(settings.Deps{Config: cfg})
			require.NoError(t, err)
			got, err := s.API(context.Background())
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
	t.Run("store failure", func(t *testing.T) {
		cfg := configmock.NewService(t)
		cfg.EXPECT().Get(mock.Anything, "api").Return(nil, errDown)
		s, err := settings.New(settings.Deps{Config: cfg})
		require.NoError(t, err)
		_, err = s.API(context.Background())
		assert.ErrorIs(t, err, errDown)
	})
}

func TestApplied(t *testing.T) {
	assert.True(t, settings.Applied("api", "root_access"))
	assert.True(t, settings.Applied("api", "requests_max"), "read by the rate limiter")
	assert.True(t, settings.Applied("ratelimit", "s3_ip_burst"))
	assert.True(t, settings.Applied("scanner", "speed"), "read by the scanner at every cycle")
	assert.True(t, settings.Applied("scanner", "cycle"))
	assert.False(t, settings.Applied("region", "comment"))
	assert.True(t, settings.Applied("logger", "level"), "applied at once by the log stream")
	assert.True(t, settings.Applied("logger", "format"))
	assert.True(t, settings.Applied("audit_webhook:siem", "endpoint"), "applied by the audit log")
	assert.True(t, settings.Applied("audit_kafka", "brokers"))
	assert.False(t, settings.Applied("audit_webhook", "comment"))
	assert.True(t, settings.Applied("api", "requests_deadline"))
	assert.False(t, settings.Applied("browser", "session_duration"))
}

// covers: S-OPS-01 (api.root_access=off denies the root credentials on
// the S3 API; regression: the key was stored and shown but never applied)
func TestGuardRootAccess(t *testing.T) {
	ctx := context.Background()
	root := &domain.Identity{AccessKey: "rootadmin", IsRoot: true}
	derived := &domain.Identity{AccessKey: "svc-root", IsRoot: true, Parent: "rootadmin"}
	user := &domain.Identity{AccessKey: "bob"}
	allowed := policy.Decision{Allowed: true, Source: policy.SourceRoot}
	tests := []struct {
		name       string
		identity   *domain.Identity
		rootAccess *bool // nil: the setting is not read
		engine     bool  // the engine decides
		allowed    bool
	}{
		{name: "root allowed", identity: root, rootAccess: ptr(true), engine: true, allowed: true},
		{name: "root denied", identity: root, rootAccess: ptr(false), allowed: false},
		{name: "derived credentials of root", identity: derived, engine: true, allowed: true},
		{name: "other users", identity: user, engine: true, allowed: true},
		{name: "anonymous", identity: nil, engine: true, allowed: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			engine := policymock.NewService(t)
			applied := settingsmock.NewService(t)
			args := policy.Args{Identity: tt.identity, Action: policy.ActionGetObject, Bucket: "b", Object: "k"}
			if tt.rootAccess != nil {
				applied.EXPECT().API(mock.Anything).Return(settings.API{RootAccess: *tt.rootAccess}, nil)
			}
			if tt.engine {
				engine.EXPECT().Evaluate(mock.Anything, args).Return(allowed, nil)
			}
			d, err := settings.GuardRootAccess(engine, applied).Evaluate(ctx, args)
			require.NoError(t, err)
			assert.Equal(t, tt.allowed, d.Allowed)
			if !tt.allowed {
				assert.Contains(t, d.Reason, "api.root_access=off")
			}
		})
	}
	t.Run("a setting that cannot be read fails closed", func(t *testing.T) {
		applied := settingsmock.NewService(t)
		applied.EXPECT().API(mock.Anything).Return(settings.API{}, errDown)
		_, err := settings.GuardRootAccess(policymock.NewService(t), applied).Evaluate(ctx, policy.Args{Identity: root, Action: policy.ActionGetObject})
		assert.ErrorIs(t, err, errDown)
	})
	t.Run("the other methods are the engine's", func(t *testing.T) {
		engine := policymock.NewService(t)
		engine.EXPECT().ListPolicies(mock.Anything).Return([]policy.Named{{Name: "readonly"}}, nil)
		got, err := settings.GuardRootAccess(engine, settingsmock.NewService(t)).ListPolicies(ctx)
		require.NoError(t, err)
		assert.Len(t, got, 1)
	})
}

func ptr[T any](v T) *T { return &v }

func TestAPICORSAllowOrigin(t *testing.T) {
	cfg := configmock.NewService(t)
	cfg.EXPECT().Get(mock.Anything, "api").Return(config.KV{"cors_allow_origin": " https://a.example.com, ,https://*.b.example.com "}, nil)
	svc, err := settings.New(settings.Deps{Config: cfg})
	require.NoError(t, err)
	api, err := svc.API(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"https://a.example.com", "https://*.b.example.com"}, api.CORSAllowOrigin)
	assert.True(t, api.RootAccess)
	assert.True(t, settings.Applied("api", "cors_allow_origin"))
}

// covers: S-OPS-10 (the request limits of the dynamic configuration:
// defaults, burst defaulting to the rate, invalid values logged as no
// limit)
func TestLimits(t *testing.T) {
	cfg := configmock.NewService(t)
	cfg.EXPECT().Get(mock.Anything, "api").Return(config.KV{"requests_max": "64", "requests_deadline": "2s"}, nil).Once()
	cfg.EXPECT().Get(mock.Anything, "ratelimit").Return(config.KV{
		"s3_ip": "100", "s3_ip_burst": "250", "s3_access_key": "50", "s3_bucket": "nope", "admin_ip": "10", "admin_user": "-3",
	}, nil).Once()
	s, err := settings.New(settings.Deps{Config: cfg})
	require.NoError(t, err)
	got, err := s.Limits(context.Background())
	require.NoError(t, err)
	assert.Equal(t, settings.Limits{
		RequestsMax: 64, RequestsDeadline: 2 * time.Second,
		S3IP:        settings.Rate{PerSecond: 100, Burst: 250},
		S3AccessKey: settings.Rate{PerSecond: 50, Burst: 50},
		AdminIP:     settings.Rate{PerSecond: 10, Burst: 10},
	}, got)

	cfg.EXPECT().Get(mock.Anything, "api").Return(config.KV{"requests_deadline": "soon"}, nil).Once()
	cfg.EXPECT().Get(mock.Anything, "ratelimit").Return(config.KV{}, nil).Once()
	got, err = s.Limits(context.Background())
	require.NoError(t, err)
	assert.Equal(t, settings.Limits{RequestsDeadline: settings.DefaultRequestsDeadline}, got, "nothing set: no limit")

	cfg.EXPECT().Get(mock.Anything, "api").Return(nil, errDown).Once()
	_, err = s.Limits(context.Background())
	assert.ErrorIs(t, err, errDown)
	cfg.EXPECT().Get(mock.Anything, "api").Return(config.KV{}, nil).Once()
	cfg.EXPECT().Get(mock.Anything, "ratelimit").Return(nil, errDown).Once()
	_, err = s.Limits(context.Background())
	assert.ErrorIs(t, err, errDown)
}
