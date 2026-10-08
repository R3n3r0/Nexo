// Package cli holds the nexoctl building blocks used by the cobra commands
// (internal/cli/commands): the alias configuration store and service, the
// S3Client contract with its aws-sdk-go-v2 implementation, the parsing of
// targets, sizes and durations, the transfer engine of cp, mv, mirror and
// rm, the anonymous access presets and the output helpers (tables, JSON
// Lines, progress bar, stable exit codes).
package cli

import (
	"context"
	"time"
)

// ConfigVersion is the current configuration file format version.
const ConfigVersion = "1"

// Alias is a named server endpoint with its credentials.
type Alias struct {
	Name      string `json:"-"`
	URL       string `json:"url"`
	AccessKey string `json:"accessKey"`
	SecretKey string `json:"secretKey"`
	API       string `json:"api,omitempty"`
	Path      string `json:"path,omitempty"`
	// Region is the signing region of the server ("" means DefaultRegion).
	Region string `json:"region,omitempty"`
	// AdminURL is the base URL of the Admin API of a Nexo server (its
	// console listener); "" derives it from URL (see AdminURL).
	AdminURL  string    `json:"adminUrl,omitempty"`
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
