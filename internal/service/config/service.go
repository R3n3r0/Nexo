// Package config is the dynamic configuration service: key/value settings
// grouped by subsystem (e.g. "notify_webhook", "scanner"), persisted through
// a Store and observable through Watch.
package config

import "context"

// KV is the key/value settings of one subsystem.
type KV map[string]string

// Service manages the dynamic configuration.
type Service interface {
	// Get returns the settings of a subsystem; a missing subsystem yields
	// an empty KV and no error.
	Get(ctx context.Context, subsystem string) (KV, error)
	// Set merges kv into the subsystem settings (existing keys are
	// overwritten, other keys kept) and persists the result.
	Set(ctx context.Context, subsystem string, kv KV) error
	// Delete removes the whole subsystem, or only the given keys when any.
	Delete(ctx context.Context, subsystem string, keys ...string) error
	// List returns the names of the configured subsystems.
	List(ctx context.Context) ([]string, error)
	// Export serializes the whole configuration as JSON.
	Export(ctx context.Context) ([]byte, error)
	// Import replaces the whole configuration with the JSON produced by Export.
	Import(ctx context.Context, data []byte) error
	// Watch returns a channel that receives a Change for every mutation
	// until ctx is canceled, when it is closed. Slow consumers may miss
	// intermediate changes; they should re-read the subsystem.
	Watch(ctx context.Context) <-chan Change
}

// Change describes a configuration mutation.
type Change struct {
	Subsystem string
	// Deleted reports that the subsystem (or the Keys) were removed.
	Deleted bool
	// Keys are the affected keys; empty means the whole subsystem.
	Keys []string
}

// Store persists the configuration. Implementations: in-memory (tests,
// SNSD bootstrap) and the system bucket (.nexo.sys/config) in M1.
type Store interface {
	// Load returns the whole configuration; an empty store returns an
	// empty map and no error.
	Load(ctx context.Context) (map[string]KV, error)
	// Save atomically replaces the whole configuration.
	Save(ctx context.Context, all map[string]KV) error
}
