package config_test

import (
	"context"
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/config"
)

func TestLoadKMS(t *testing.T) {
	key := "nexo-key:" + base64.StdEncoding.EncodeToString(make([]byte, 32))
	old := "old-key:" + base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	base := map[string]string{config.EnvRootUser: "admin", config.EnvRootPassword: "password123"}
	tests := []struct {
		name    string
		env     map[string]string
		files   map[string]string
		check   func(t *testing.T, cfg config.Static)
		wantErr string
	}{
		{name: "no KMS", check: func(t *testing.T, cfg config.Static) { assert.False(t, cfg.KMSConfigured()) }},
		{
			name: "builtin with a rotated key",
			env:  map[string]string{config.EnvKMSSecretKey: key, config.EnvKMSSecretKeyPrev: old, config.EnvSSECAllowInsecure: "true"},
			check: func(t *testing.T, cfg config.Static) {
				assert.True(t, cfg.KMSConfigured())
				assert.Equal(t, key, cfg.KMSSecretKey)
				assert.Equal(t, []string{old}, cfg.KMSPreviousSecretKeys)
				assert.True(t, cfg.SSECAllowInsecure)
			},
		},
		{
			name:  "builtin from a file",
			env:   map[string]string{config.EnvKMSSecretKey + config.FileSuffix: "/run/secrets/kms"},
			files: map[string]string{"/run/secrets/kms": key + "\n"},
			check: func(t *testing.T, cfg config.Static) { assert.Equal(t, key, cfg.KMSSecretKey) },
		},
		{
			name: "KES",
			env: map[string]string{
				config.EnvKMSEndpoint: "https://kes1:7373, https://kes2:7373", config.EnvKMSKeyName: "nexo",
				config.EnvKMSClientCert: "/c.crt", config.EnvKMSClientKey: "/c.key", config.EnvKMSCAPath: "/ca",
			},
			check: func(t *testing.T, cfg config.Static) {
				assert.Equal(t, []string{"https://kes1:7373", "https://kes2:7373"}, cfg.KMSEndpoints)
				assert.Equal(t, "nexo", cfg.KMSKeyName)
				assert.Equal(t, "/ca", cfg.KMSCAPath)
			},
		},
		{name: "malformed key", env: map[string]string{config.EnvKMSSecretKey: "nexo-key"}, wantErr: "NEXO_KMS_SECRET_KEY"},
		{name: "short key", env: map[string]string{config.EnvKMSSecretKey: "k:" + base64.StdEncoding.EncodeToString([]byte("short"))}, wantErr: "32 bytes"},
		{name: "both KMS", env: map[string]string{config.EnvKMSSecretKey: key, config.EnvKMSEndpoint: "https://kes:7373", config.EnvKMSKeyName: "k"}, wantErr: "not both"},
		{name: "KES without key name", env: map[string]string{config.EnvKMSEndpoint: "https://kes:7373"}, wantErr: "NEXO_KMS_KEY_NAME"},
		{name: "KES bad URL", env: map[string]string{config.EnvKMSEndpoint: "kes:7373", config.EnvKMSKeyName: "k"}, wantErr: "https URL"},
		{name: "KES over plain HTTP", env: map[string]string{config.EnvKMSEndpoint: "https://kes1:7373,http://kes2:7373", config.EnvKMSKeyName: "k"}, wantErr: "must use https"},
		{name: "KES URL with credentials", env: map[string]string{config.EnvKMSEndpoint: "https://u:s3cr3t-pass@kes:7373", config.EnvKMSKeyName: "k"}, wantErr: "credentials"},
		{name: "KES credentials over plain HTTP", env: map[string]string{config.EnvKMSEndpoint: "http://u:s3cr3t-pass@kes:7373?token=s3cr3t-pass", config.EnvKMSKeyName: "k"}, wantErr: "must use https"},
		{name: "KES credentials in a malformed URL", env: map[string]string{config.EnvKMSEndpoint: "https://u:s3cr3t-pass@kes host", config.EnvKMSKeyName: "k"}, wantErr: "https URL"},
		{name: "certificate without key", env: map[string]string{config.EnvKMSEndpoint: "https://kes:7373", config.EnvKMSKeyName: "k", config.EnvKMSClientCert: "/c.crt"}, wantErr: "go together"},
		{name: "previous without current", env: map[string]string{config.EnvKMSSecretKeyPrev: old}, wantErr: "need the current one"},
		{name: "bad insecure flag", env: map[string]string{config.EnvSSECAllowInsecure: "maybe"}, wantErr: config.EnvSSECAllowInsecure},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := map[string]string{}
			for k, v := range base {
				env[k] = v
			}
			for k, v := range tt.env {
				env[k] = v
			}
			l := config.NewLoader(config.WithEnv(envOf(env)), config.WithReadFile(filesOf(tt.files)))
			cfg, err := l.Load(context.Background(), config.Input{Args: []string{"/data"}})
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				assert.NotContains(t, err.Error(), "s3cr3t-pass", "the errors reach stderr and the logs: no credentials")
				return
			}
			require.NoError(t, err)
			tt.check(t, cfg)
		})
	}
}
