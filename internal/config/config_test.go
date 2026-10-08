package config

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func envOf(m map[string]string) LookupEnv {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func filesOf(m map[string]string) ReadFile {
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
	BindFlags(fs)
	require.NoError(t, fs.Parse(args))
	return fs
}

func TestLoadDefaultsAndEnv(t *testing.T) {
	l := NewLoader(WithEnv(envOf(map[string]string{
		EnvRootUser:       "admin",
		EnvRootPassword:   "password123",
		EnvTrustedProxies: "10.0.0.0/8, 192.168.1.1",
	})))
	cfg, err := l.Load(context.Background(), Input{Args: []string{"/data"}})
	require.NoError(t, err)
	assert.Equal(t, DefaultAddress, cfg.Address)
	assert.Equal(t, DefaultConsoleAddress, cfg.ConsoleAddress)
	assert.Equal(t, DefaultRegion, cfg.Region)
	assert.Equal(t, "admin", cfg.RootUser)
	assert.Equal(t, []string{"10.0.0.0/8", "192.168.1.1"}, cfg.TrustedProxies)
	assert.Equal(t, []string{"/data"}, cfg.Endpoints)
	assert.False(t, cfg.TLSEnabled())
}

func TestLoadPrecedenceFlagsOverEnvOverFile(t *testing.T) {
	env := map[string]string{
		EnvRootUser:     "admin",
		EnvRootPassword: "password123",
		EnvConfigFile:   "/etc/nexo.json",
		EnvAddress:      ":7000",
		EnvRegion:       "eu-west-1",
	}
	files := map[string]string{
		"/etc/nexo.json": `{"address":":6000","consoleAddress":":6001","region":"ap-south-1","logLevel":"debug","endpoints":["/from-file"]}`,
	}
	l := NewLoader(WithEnv(envOf(env)), WithReadFile(filesOf(files)))
	fs := flagsFrom(t, "--address", ":8000", "--trusted-proxies", "127.0.0.1")
	cfg, err := l.Load(context.Background(), Input{Flags: fs})
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
	cfg, err = l.Load(context.Background(), Input{Flags: fs, Args: []string{"/a", "/b"}})
	require.NoError(t, err)
	assert.Equal(t, []string{"/a", "/b"}, cfg.Endpoints)
	assert.Equal(t, "text", cfg.LogFormat)
	assert.Equal(t, ":7000", cfg.Address, "env still applies")
}

func TestLoadSecretsFromFiles(t *testing.T) {
	env := map[string]string{
		EnvRootUser:                  "stale",
		EnvRootUser + FileSuffix:     "/run/secrets/user",
		EnvRootPassword + FileSuffix: "/run/secrets/password",
	}
	files := map[string]string{
		"/run/secrets/user":     "admin\n",
		"/run/secrets/password": "  supersecret123 \n",
	}
	l := NewLoader(WithEnv(envOf(env)), WithReadFile(filesOf(files)))
	cfg, err := l.Load(context.Background(), Input{Args: []string{"/data"}})
	require.NoError(t, err)
	assert.Equal(t, "admin", cfg.RootUser, "_FILE wins over the plain variable")
	assert.Equal(t, "supersecret123", cfg.RootPassword)

	env[EnvRootPassword+FileSuffix] = "/missing"
	_, err = l.Load(context.Background(), Input{Args: []string{"/data"}})
	require.Error(t, err)
	assert.True(t, errors.Is(err, os.ErrNotExist))
}

func TestLoadErrors(t *testing.T) {
	l := NewLoader(WithEnv(envOf(map[string]string{EnvConfigFile: "/nope"})), WithReadFile(filesOf(nil)))
	_, err := l.Load(context.Background(), Input{})
	assert.ErrorIs(t, err, os.ErrNotExist)

	l = NewLoader(WithEnv(envOf(map[string]string{EnvConfigFile: "/bad"})), WithReadFile(filesOf(map[string]string{"/bad": "{"})))
	_, err = l.Load(context.Background(), Input{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse config file")
}

func TestValidate(t *testing.T) {
	valid := Default()
	valid.RootUser, valid.RootPassword, valid.Endpoints = "admin", "password123", []string{"/data"}
	require.NoError(t, valid.Validate())

	tests := []struct {
		name   string
		mutate func(*Static)
		want   string
	}{
		{"short password", func(s *Static) { s.RootPassword = "short" }, "root password"},
		{"short user", func(s *Static) { s.RootUser = "ab" }, "root user"},
		{"no endpoints", func(s *Static) { s.Endpoints = nil }, "endpoint"},
		{"bad address", func(s *Static) { s.Address = "9000" }, "address"},
		{"same addresses", func(s *Static) { s.ConsoleAddress = s.Address }, "must differ"},
		{"bad log level", func(s *Static) { s.LogLevel = "loud" }, "log level"},
		{"bad log format", func(s *Static) { s.LogFormat = "yaml" }, "log format"},
		{"bad server url", func(s *Static) { s.ServerURL = "ftp://x" }, "server url"},
		{"bad proxy", func(s *Static) { s.TrustedProxies = []string{"not-an-ip"} }, "trusted proxy"},
		{"empty region", func(s *Static) { s.Region = "" }, "region"},
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
	empty := Static{}
	err := empty.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "root user")
	assert.Contains(t, err.Error(), "endpoint")
	assert.Contains(t, err.Error(), "address")
}
