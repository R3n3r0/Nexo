package config_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/config"
)

func envOf(m map[string]string) config.LookupEnv {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func filesOf(m map[string]string) config.ReadFile {
	return func(name string) ([]byte, error) {
		v, ok := m[name]
		if !ok {
			return nil, os.ErrNotExist
		}
		return []byte(v), nil
	}
}

func flagsFrom(t *testing.T, args ...string) *pflag.FlagSet {
	t.Helper()
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	config.BindFlags(fs)
	require.NoError(t, fs.Parse(args))
	return fs
}

// covers: S-IAM-01, S-OPS-01
func TestLoadDefaultsAndEnv(t *testing.T) {
	l := config.NewLoader(config.WithEnv(envOf(map[string]string{
		config.EnvRootUser:       "admin",
		config.EnvRootPassword:   "password123",
		config.EnvTrustedProxies: "10.0.0.0/8, 192.168.1.1",
	})))
	cfg, err := l.Load(context.Background(), config.Input{Args: []string{"/data"}})
	require.NoError(t, err)
	assert.Equal(t, config.DefaultAddress, cfg.Address)
	assert.Equal(t, config.DefaultConsoleAddress, cfg.ConsoleAddress)
	assert.Equal(t, config.DefaultRegion, cfg.Region)
	assert.Equal(t, "admin", cfg.RootUser)
	assert.Equal(t, []string{"10.0.0.0/8", "192.168.1.1"}, cfg.TrustedProxies)
	assert.Equal(t, []string{"/data"}, cfg.Endpoints)
	assert.False(t, cfg.TLSEnabled())
	assert.Equal(t, config.PrometheusAuthJWT, cfg.PrometheusAuthType, "the metrics endpoints are protected by default")
}

func TestLoadPrometheusAuthType(t *testing.T) {
	tests := []struct {
		value   string
		want    string
		wantErr bool
	}{
		{value: "public", want: config.PrometheusAuthPublic},
		{value: " JWT ", want: config.PrometheusAuthJWT},
		{value: "none", wantErr: true},
		{value: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			l := config.NewLoader(config.WithEnv(envOf(map[string]string{
				config.EnvRootUser: "admin", config.EnvRootPassword: "password123", config.EnvPrometheusAuthType: tt.value,
			})))
			cfg, err := l.Load(context.Background(), config.Input{Args: []string{"/data"}})
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "prometheus auth type")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, cfg.PrometheusAuthType)
		})
	}
}

// TestLoadClusterSettings covers the settings of the erasure-coded and
// distributed modes: storage class parities (flags over environment) and
// the internode durations (environment only).
func TestLoadClusterSettings(t *testing.T) {
	env := map[string]string{
		config.EnvRootUser:          "admin",
		config.EnvRootPassword:      "password123",
		config.EnvStorageClassStd:   "EC:3",
		config.EnvStorageClassRRS:   "EC:1",
		config.EnvLockStaleAfter:    "20s",
		config.EnvLockRefresh:       " 2s ",
		config.EnvRPCCallTimeout:    "5s",
		config.EnvHealRetryMaxDelay: "3s",
		config.EnvSendTimeout:       "45s",
	}
	l := config.NewLoader(config.WithEnv(envOf(env)))
	cfg, err := l.Load(context.Background(), config.Input{Flags: flagsFrom(t, "--storage-class-standard", "EC:4"), Args: []string{"/d{1...8}"}})
	require.NoError(t, err)
	assert.Equal(t, "EC:4", cfg.StorageClassStandard, "flag wins over the environment")
	assert.Equal(t, "EC:1", cfg.StorageClassRRS)
	assert.Equal(t, 20*time.Second, cfg.LockStaleAfter)
	assert.Equal(t, 2*time.Second, cfg.LockRefreshInterval)
	assert.Equal(t, 5*time.Second, cfg.RPCCallTimeout)
	assert.Equal(t, 3*time.Second, cfg.HealRetryMaxDelay)
	assert.Equal(t, 45*time.Second, cfg.SendTimeout)

	tests := []struct {
		name, key, value, want string
	}{
		{name: "malformed duration", key: config.EnvRPCCallTimeout, value: "soon", want: config.EnvRPCCallTimeout},
		{name: "malformed storage class", key: config.EnvStorageClassRRS, value: "EC", want: "storage class"},
		{name: "refresh longer than stale", key: config.EnvLockRefresh, value: "1m", want: "lock refresh interval"},
		{name: "negative heal retry delay", key: config.EnvHealRetryMaxDelay, value: "-1s", want: "heal retry max delay"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bad := map[string]string{}
			for k, v := range env {
				bad[k] = v
			}
			bad[tt.key] = tt.value
			_, err := config.NewLoader(config.WithEnv(envOf(bad))).Load(context.Background(), config.Input{Args: []string{"/d{1...8}"}})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

// covers: S-OPS-01
func TestLoadPrecedenceFlagsOverEnvOverFile(t *testing.T) {
	env := map[string]string{
		config.EnvRootUser:     "admin",
		config.EnvRootPassword: "password123",
		config.EnvConfigFile:   "/etc/nexo.json",
		config.EnvAddress:      ":7000",
		config.EnvRegion:       "eu-west-1",
	}
	files := map[string]string{
		"/etc/nexo.json": `{"address":":6000","consoleAddress":":6001","region":"ap-south-1","logLevel":"debug","endpoints":["/from-file"]}`,
	}
	l := config.NewLoader(config.WithEnv(envOf(env)), config.WithReadFile(filesOf(files)))
	fs := flagsFrom(t, "--address", ":8000", "--trusted-proxies", "127.0.0.1")
	cfg, err := l.Load(context.Background(), config.Input{Flags: fs})
	require.NoError(t, err)
	assert.Equal(t, ":8000", cfg.Address, "flag wins")
	assert.Equal(t, "eu-west-1", cfg.Region, "env wins over file")
	assert.Equal(t, ":6001", cfg.ConsoleAddress, "file wins over default")
	assert.Equal(t, "debug", cfg.LogLevel)
	assert.Equal(t, []string{"/from-file"}, cfg.Endpoints, "endpoints from file when no args")
	assert.Equal(t, []string{"127.0.0.1"}, cfg.TrustedProxies)

	// Positional args override file endpoints; --config flag overrides env.
	files["/other.json"] = `{"endpoints":["/x"],"logFormat":"text"}`
	fs = flagsFrom(t, "--config", "/other.json")
	cfg, err = l.Load(context.Background(), config.Input{Flags: fs, Args: []string{"/a", "/b"}})
	require.NoError(t, err)
	assert.Equal(t, []string{"/a", "/b"}, cfg.Endpoints)
	assert.Equal(t, "text", cfg.LogFormat)
	assert.Equal(t, ":7000", cfg.Address, "env still applies")
}

// covers: S-IAM-01, S-OPS-13
func TestLoadSecretsFromFiles(t *testing.T) {
	env := map[string]string{
		config.EnvRootUser:                         "stale",
		config.EnvRootUser + config.FileSuffix:     "/run/secrets/user",
		config.EnvRootPassword + config.FileSuffix: "/run/secrets/password",
	}
	files := map[string]string{
		"/run/secrets/user":     "admin\n",
		"/run/secrets/password": "  supersecret123 \n",
	}
	l := config.NewLoader(config.WithEnv(envOf(env)), config.WithReadFile(filesOf(files)))
	cfg, err := l.Load(context.Background(), config.Input{Args: []string{"/data"}})
	require.NoError(t, err)
	assert.Equal(t, "admin", cfg.RootUser, "_FILE wins over the plain variable")
	assert.Equal(t, "supersecret123", cfg.RootPassword)

	env[config.EnvRootPassword+config.FileSuffix] = "/missing"
	_, err = l.Load(context.Background(), config.Input{Args: []string{"/data"}})
	require.Error(t, err)
	assert.True(t, errors.Is(err, os.ErrNotExist))
}

func TestLoadErrors(t *testing.T) {
	l := config.NewLoader(config.WithEnv(envOf(map[string]string{config.EnvConfigFile: "/nope"})), config.WithReadFile(filesOf(nil)))
	_, err := l.Load(context.Background(), config.Input{})
	assert.ErrorIs(t, err, os.ErrNotExist)

	l = config.NewLoader(config.WithEnv(envOf(map[string]string{config.EnvConfigFile: "/bad"})), config.WithReadFile(filesOf(map[string]string{"/bad": "{"})))
	_, err = l.Load(context.Background(), config.Input{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse config file")
}

// covers: S-OPS-14
func TestValidate(t *testing.T) {
	valid := config.Default()
	valid.RootUser, valid.RootPassword, valid.Endpoints = "admin", "password123", []string{"/data"}
	require.NoError(t, valid.Validate())

	tests := []struct {
		name   string
		mutate func(*config.Static)
		want   string
	}{
		// A password set but too short is not "required" (regression).
		{"short password", func(s *config.Static) { s.RootPassword = "short" }, "root password must be at least 8 characters (NEXO_ROOT_PASSWORD has 5)"},
		{"missing password", func(s *config.Static) { s.RootPassword = "" }, "root password is required (NEXO_ROOT_PASSWORD, at least 8 characters)"},
		{"short user", func(s *config.Static) { s.RootUser = "ab" }, "root user"},
		{"no endpoints", func(s *config.Static) { s.Endpoints = nil }, "endpoint"},
		{"bad address", func(s *config.Static) { s.Address = "9000" }, "address"},
		{"same addresses", func(s *config.Static) { s.ConsoleAddress = s.Address }, "must differ"},
		{"bad log level", func(s *config.Static) { s.LogLevel = "loud" }, "log level"},
		{"bad log format", func(s *config.Static) { s.LogFormat = "yaml" }, "log format"},
		{"bad server url", func(s *config.Static) { s.ServerURL = "ftp://x" }, "server url"},
		{"bad proxy", func(s *config.Static) { s.TrustedProxies = []string{"not-an-ip"} }, "trusted proxy"},
		{"empty region", func(s *config.Static) { s.Region = "" }, "region"},
		{"bad prometheus auth", func(s *config.Static) { s.PrometheusAuthType = "basic" }, "prometheus auth type"},
		{"bad standard storage class", func(s *config.Static) { s.StorageClassStandard = "RS:2" }, "standard storage class"},
		{"negative parity", func(s *config.Static) { s.StorageClassRRS = "EC:-1" }, "reduced redundancy storage class"},
		{"negative rpc timeout", func(s *config.Static) { s.RPCCallTimeout = -time.Second }, "rpc call timeout"},
		{"internode mTLS without TLS", func(s *config.Static) { s.TLSInternodeVerify = true }, "tls internode verify"},
		{"refresh not shorter than stale", func(s *config.Static) {
			s.LockStaleAfter, s.LockRefreshInterval = 5*time.Second, 5*time.Second
		}, "lock refresh interval"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid
			cfg.Endpoints = append([]string(nil), valid.Endpoints...)
			tc.mutate(&cfg)
			err := cfg.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}

	// Every error is reported at once.
	empty := config.Static{}
	err := empty.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "root user")
	assert.Contains(t, err.Error(), "endpoint")
	assert.Contains(t, err.Error(), "address")
}

// TestLoadTLSInternodeVerify: --tls-internode-verify (or its variable)
// turns mutual TLS between the nodes on; it needs the certificates.
func TestLoadTLSInternodeVerify(t *testing.T) {
	env := map[string]string{config.EnvRootUser: "admin", config.EnvRootPassword: "password123", config.EnvCertsDir: "/certs"}
	cfg, err := config.NewLoader(config.WithEnv(envOf(env))).Load(context.Background(), config.Input{Args: []string{"/data"}})
	require.NoError(t, err)
	assert.False(t, cfg.TLSInternodeVerify, "off by default")

	env[config.EnvTLSInternodeVerify] = "true"
	cfg, err = config.NewLoader(config.WithEnv(envOf(env))).Load(context.Background(), config.Input{Args: []string{"/data"}})
	require.NoError(t, err)
	assert.True(t, cfg.TLSInternodeVerify)
	cfg, err = config.NewLoader(config.WithEnv(envOf(env))).Load(context.Background(),
		config.Input{Flags: flagsFrom(t, "--tls-internode-verify=false"), Args: []string{"/data"}})
	require.NoError(t, err)
	assert.False(t, cfg.TLSInternodeVerify, "the flag wins")

	env[config.EnvTLSInternodeVerify] = "maybe"
	_, err = config.NewLoader(config.WithEnv(envOf(env))).Load(context.Background(), config.Input{Args: []string{"/data"}})
	require.ErrorContains(t, err, config.EnvTLSInternodeVerify)
}
