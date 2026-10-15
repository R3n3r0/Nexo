package config_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/config"
)

// baseEnv are the variables every load needs.
func baseEnv(extra map[string]string) map[string]string {
	m := map[string]string{config.EnvRootUser: "admin", config.EnvRootPassword: "password123"}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func environOf(m map[string]string) config.Environ {
	return func() []string {
		out := make([]string, 0, len(m))
		for k, v := range m {
			out = append(out, k+"="+v)
		}
		return out
	}
}

func TestAccessDefaults(t *testing.T) {
	env := baseEnv(nil)
	cfg, err := config.NewLoader(config.WithEnv(envOf(env)), config.WithEnviron(environOf(env))).
		Load(context.Background(), config.Input{Args: []string{"/data"}})
	require.NoError(t, err)
	assert.Empty(t, cfg.ProxyProtocol)
	assert.Equal(t, config.ForwardedHeadersX, cfg.ForwardedHeaders)
	assert.Equal(t, config.DefaultAccessRulesMax, cfg.AccessRulesMax)
	assert.Equal(t, config.KeyNormalizationNFC, cfg.KeyNormalization)
	assert.False(t, cfg.BlockPublicAccess)
	assert.Equal(t, config.BlockPublicReject, cfg.BlockPublicAccessMode)
	assert.False(t, cfg.TrustForwardedHost, "the Host header by default")
	assert.Empty(t, cfg.Secrets)
	assert.False(t, cfg.ProxyProtocolOn(config.ListenerS3))
}

func TestAccessFromEnvAndFlags(t *testing.T) {
	env := baseEnv(map[string]string{
		config.EnvTrustedProxies:        "10.0.0.0/8",
		config.EnvProxyProtocol:         "S3, web",
		config.EnvForwardedHeaders:      "Forwarded",
		config.EnvAccessRulesMax:        "50",
		config.EnvKeyNormalization:      "none",
		config.EnvBlockPublicAccess:     "true",
		config.EnvBlockPublicAccessMode: "ignore",
		config.EnvTrustForwardedHost:    "true",
		"NEXO_SECRET_PROXY_TOKEN":       "s3cr3t",
		"NEXO_SECRET_FROM_FILE_FILE":    "/run/secrets/token",
		"NEXO_SECRET_":                  "ignored",
		"OTHER":                         "x",
	})
	l := config.NewLoader(config.WithEnv(envOf(env)), config.WithEnviron(environOf(env)),
		config.WithReadFile(filesOf(map[string]string{"/run/secrets/token": "from-file\n"})))
	cfg, err := l.Load(context.Background(), config.Input{Args: []string{"/data"}})
	require.NoError(t, err)
	assert.Equal(t, []string{"s3", "web"}, cfg.ProxyProtocol)
	assert.True(t, cfg.ProxyProtocolOn(config.ListenerWeb))
	assert.False(t, cfg.ProxyProtocolOn(config.ListenerConsole))
	assert.Equal(t, config.ForwardedHeadersRFC7239, cfg.ForwardedHeaders)
	assert.True(t, cfg.TrustForwardedHost)
	assert.Equal(t, 50, cfg.AccessRulesMax)
	assert.Equal(t, config.KeyNormalizationNone, cfg.KeyNormalization)
	assert.True(t, cfg.BlockPublicAccess)
	assert.Equal(t, config.BlockPublicIgnore, cfg.BlockPublicAccessMode)
	assert.Equal(t, map[string]string{"proxy_token": "s3cr3t", "from_file": "from-file"}, cfg.Secrets)

	cfg, err = l.Load(context.Background(), config.Input{Args: []string{"/data"}, Flags: flagsFrom(t,
		"--proxy-protocol", "console", "--forwarded-headers", "x-forwarded", "--access-rules-max", "7",
		"--key-normalization", "nfc", "--block-public-access=false", "--block-public-access-mode", "reject", "--trust-forwarded-host=false")})
	require.NoError(t, err)
	assert.Equal(t, []string{"console"}, cfg.ProxyProtocol)
	assert.Equal(t, config.ForwardedHeadersX, cfg.ForwardedHeaders)
	assert.False(t, cfg.TrustForwardedHost)
	assert.Equal(t, 7, cfg.AccessRulesMax)
	assert.Equal(t, config.KeyNormalizationNFC, cfg.KeyNormalization)
	assert.False(t, cfg.BlockPublicAccess)
	assert.Equal(t, config.BlockPublicReject, cfg.BlockPublicAccessMode)
}

func TestAccessValidation(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"unknown listener", map[string]string{config.EnvTrustedProxies: "10.0.0.1", config.EnvProxyProtocol: "rpc"}, "proxy protocol listener"},
		{"proxy protocol without trusted proxies", map[string]string{config.EnvProxyProtocol: "s3"}, "needs the trusted proxies"},
		{"forwarded headers", map[string]string{config.EnvForwardedHeaders: "both"}, "forwarded headers"},
		{"key normalization", map[string]string{config.EnvKeyNormalization: "nfd"}, "key normalization"},
		{"block mode", map[string]string{config.EnvBlockPublicAccessMode: "drop"}, "block public access mode"},
		{"rules max", map[string]string{config.EnvAccessRulesMax: "0"}, "access rules max"},
		{"rules max not a number", map[string]string{config.EnvAccessRulesMax: "many"}, config.EnvAccessRulesMax},
		{"block not a bool", map[string]string{config.EnvBlockPublicAccess: "maybe"}, config.EnvBlockPublicAccess},
		{"trust forwarded host not a bool", map[string]string{config.EnvTrustForwardedHost: "maybe"}, config.EnvTrustForwardedHost},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := baseEnv(tt.env)
			_, err := config.NewLoader(config.WithEnv(envOf(env)), config.WithEnviron(environOf(env))).
				Load(context.Background(), config.Input{Args: []string{"/data"}})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}
