// Package cli holds the nexoctl building blocks: the alias configuration
// store and the alias service used by the cobra commands.
package cli

import (
	"context"
	"time"
)

// ConfigVersion is the current configuration file format version.
const ConfigVersion = "1"

// Alias is a named server endpoint with its credentials.
type Alias struct {
	Name      string    `json:"-"`
	URL       string    `json:"url"`
	AccessKey string    `json:"accessKey"`
	SecretKey string    `json:"secretKey"`
	API       string    `json:"api,omitempty"`
	Path      string    `json:"path,omitempty"`
	UpdatedAt time.Time `json:"updatedAt,omitempty"`
}

// Config is the nexoctl configuration file.
type Config struct {
	Version string           `json:"version"`
	Aliases map[string]Alias `json:"aliases"`
}

// NewConfig returns an empty configuration.
func NewConfig() *Config {
	return &Config{Version: ConfigVersion, Aliases: make(map[string]Alias)}
}

// ConfigStore persists the configuration.
type ConfigStore interface {
	// Load returns the configuration, or an empty one when the file does
	// not exist yet.
	Load(ctx context.Context) (*Config, error)
	// Save writes the configuration atomically with owner-only permissions.
	Save(ctx context.Context, cfg *Config) error
	// Path returns the backing file path.
	Path() string
}

// AliasService manages aliases.
type AliasService interface {
	Set(ctx context.Context, a Alias) error
	Get(ctx context.Context, name string) (*Alias, error)
	List(ctx context.Context) ([]Alias, error)
	Remove(ctx context.Context, name string) error
}
