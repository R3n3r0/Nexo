package config_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/config"
)

func TestCacheDefaults(t *testing.T) {
	env := baseEnv(nil)
	cfg, err := config.NewLoader(config.WithEnv(envOf(env)), config.WithEnviron(environOf(env))).
		Load(context.Background(), config.Input{Args: []string{"/data"}})
	require.NoError(t, err)
	assert.Empty(t, cfg.CacheDir, "memory only by default")
	assert.Zero(t, cfg.CacheSize)
	assert.Zero(t, cfg.CacheMemory)
	assert.Contains(t, []string{"", config.CacheModeLocal}, cfg.CacheMode)
}

func TestCacheFromEnvAndFlags(t *testing.T) {
	env := baseEnv(map[string]string{
		config.EnvCacheDir:    " /cache ",
		config.EnvCacheSize:   "20GiB",
		config.EnvCacheMemory: "512MiB",
		config.EnvCacheMode:   "Shared",
	})
	l := config.NewLoader(config.WithEnv(envOf(env)), config.WithEnviron(environOf(env)))
	cfg, err := l.Load(context.Background(), config.Input{Args: []string{"/data"}})
	require.NoError(t, err)
	assert.Equal(t, "/cache", cfg.CacheDir)
	assert.Equal(t, int64(20<<30), cfg.CacheSize)
	assert.Equal(t, int64(512<<20), cfg.CacheMemory)
	assert.Equal(t, config.CacheModeShared, cfg.CacheMode)

	// The flags win over the environment.
	cfg, err = l.Load(context.Background(), config.Input{Args: []string{"/data"}, Flags: flagsFrom(t,
		"--cache-dir", "/mnt/nvme/cache", "--cache-size", "1.5G", "--cache-memory", "64M", "--cache-mode", "shared+local")})
	require.NoError(t, err)
	assert.Equal(t, "/mnt/nvme/cache", cfg.CacheDir)
	assert.Equal(t, int64(1.5*(1<<30)), cfg.CacheSize)
	assert.Equal(t, int64(64<<20), cfg.CacheMemory)
	assert.Equal(t, config.CacheModeSharedLocal, cfg.CacheMode)
}

func TestCacheValidation(t *testing.T) {
	tests := []struct {
		name  string
		env   map[string]string
		flags []string
	}{
		{name: "mode", env: map[string]string{config.EnvCacheMode: "distributed"}},
		{name: "size", env: map[string]string{config.EnvCacheSize: "lots"}},
		{name: "negative memory", env: map[string]string{config.EnvCacheMemory: "-1"}},
		{name: "flag size", flags: []string{"--cache-size", "10 parsecs"}},
		{name: "flag mode", flags: []string{"--cache-mode", "global"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := baseEnv(tc.env)
			in := config.Input{Args: []string{"/data"}}
			if tc.flags != nil {
				in.Flags = flagsFrom(t, tc.flags...)
			}
			_, err := config.NewLoader(config.WithEnv(envOf(env)), config.WithEnviron(environOf(env))).Load(context.Background(), in)
			require.Error(t, err)
		})
	}
}

func TestParseSize(t *testing.T) {
	tests := []struct {
		in   string
		want int64
		err  bool
	}{
		{in: "1024", want: 1024},
		{in: "10GiB", want: 10 << 30},
		{in: "10 gib", want: 10 << 30},
		{in: "256MiB", want: 256 << 20},
		{in: "2KiB", want: 2048},
		{in: "1TiB", want: 1 << 40},
		{in: "5GB", want: 5e9},
		{in: "5MB", want: 5e6},
		{in: "3KB", want: 3000},
		{in: "2G", want: 2 << 30},
		{in: "512b", want: 512},
		{in: "0.5M", want: 512 << 10},
		{in: "", err: true},
		{in: "abc", err: true},
		{in: "-5", err: true},
		{in: "99999999999TiB", err: true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := config.ParseSize(tc.in)
			if tc.err {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
