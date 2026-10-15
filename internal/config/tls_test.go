package config_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/config"
)

func TestTLSMinVersion(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		flags   []string
		want    string
		wantErr bool
	}{
		{name: "default", want: ""},
		{name: "environment", env: map[string]string{config.EnvTLSMinVersion: " 1.3 "}, want: config.TLSVersion13},
		{name: "flag wins over the environment", env: map[string]string{config.EnvTLSMinVersion: "1.3"},
			flags: []string{"--tls-min-version", "1.2"}, want: config.TLSVersion12},
		{name: "TLS 1.1 refused", env: map[string]string{config.EnvTLSMinVersion: "1.1"}, wantErr: true},
		{name: "garbage refused", flags: []string{"--tls-min-version", "tls1.3"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := baseEnv(tt.env)
			in := config.Input{Args: []string{"/data"}}
			if tt.flags != nil {
				in.Flags = flagsFrom(t, tt.flags...)
			}
			cfg, err := config.NewLoader(config.WithEnv(envOf(env)), config.WithEnviron(environOf(env))).Load(context.Background(), in)
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "tls min version")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, cfg.TLSMinVersion)
		})
	}
}
