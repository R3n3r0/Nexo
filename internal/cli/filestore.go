package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Default configuration locations.
const (
	ConfigDirName  = ".nexoctl"
	ConfigFileName = "config.json"
	// EnvConfigDir overrides the configuration directory.
	EnvConfigDir = "NEXOCTL_CONFIG_DIR"
)

// DefaultConfigPath returns $NEXOCTL_CONFIG_DIR/config.json or
// ~/.nexoctl/config.json.
func DefaultConfigPath() (string, error) {
	if dir := os.Getenv(EnvConfigDir); dir != "" {
		return filepath.Join(dir, ConfigFileName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ConfigDirName, ConfigFileName), nil
}

type fileStore struct {
	path string
}

var _ ConfigStore = (*fileStore)(nil)

// NewFileStore returns a ConfigStore backed by the JSON file at path.
func NewFileStore(path string) ConfigStore {
	return &fileStore{path: path}
}

// Path implements ConfigStore.
func (s *fileStore) Path() string { return s.path }

// Load implements ConfigStore.
func (s *fileStore) Load(context.Context) (*Config, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return NewConfig(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", s.path, err)
	}
	cfg := NewConfig()
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.path, err)
	}
	if cfg.Aliases == nil {
		cfg.Aliases = make(map[string]Alias)
	}
	if cfg.Version == "" {
		cfg.Version = ConfigVersion
	}
	for name, a := range cfg.Aliases {
		a.Name = name
		cfg.Aliases[name] = a
	}
	return cfg, nil
}

// Save implements ConfigStore: directory 0700, file 0600, written through a
// temporary file and renamed into place.
func (s *fileStore) Save(_ context.Context, cfg *Config) error {
	if cfg == nil {
		return errors.New("nil config")
	}
	if cfg.Version == "" {
		cfg.Version = ConfigVersion
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".config-*.json")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		cleanup()
		return fmt.Errorf("rename into %s: %w", s.path, err)
	}
	return nil
}
