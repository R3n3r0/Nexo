package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/config"
	"github.com/r3n3r0/nexo/internal/service/settings"
)

// SensitiveMask replaces the sensitive configuration values returned by
// the ConfigManager; writing it back keeps the stored secret.
const SensitiveMask = "********"

// ConfigManagerDeps are the dependencies of the configuration manager.
type ConfigManagerDeps struct {
	// Config stores the dynamic configuration (required).
	Config config.Service
	Logger *slog.Logger
}

// ConfigManagerOption configures the configuration manager.
type ConfigManagerOption func(*configManager)

type configManager struct {
	deps ConfigManagerDeps
}

var _ ConfigManager = (*configManager)(nil)

// NewConfigManager returns the configuration manager. A missing
// configuration service is a wiring error (domain.ErrInvalidArgument).
func NewConfigManager(deps ConfigManagerDeps, opts ...ConfigManagerOption) (ConfigManager, error) {
	if deps.Config == nil {
		return nil, fmt.Errorf("%w: config manager requires the config service", domain.ErrInvalidArgument)
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	m := &configManager{deps: deps}
	for _, o := range opts {
		o(m)
	}
	return m, nil
}

// configSchema is the schema of the known subsystems, sorted by name,
// each key marked with whether the server applies it (settings.Applied).
func configSchema() []SubsystemSchema {
	schema := knownSubsystems()
	sort.Slice(schema, func(i, j int) bool { return schema[i].Name < schema[j].Name })
	for i := range schema {
		for j := range schema[i].Keys {
			schema[i].Keys[j].Applied = settings.Applied(schema[i].Name, schema[i].Keys[j].Name)
		}
	}
	return schema
}

// knownSubsystems lists the subsystems and keys the configuration accepts.
func knownSubsystems() []SubsystemSchema {
	return []SubsystemSchema{
		{Name: "api", Description: "S3 API behavior", Keys: []KeySchema{
			{Name: "cors_allow_origin", Type: KeyList, Default: "*", Description: "Origins allowed by CORS, comma separated"},
			{Name: "requests_max", Type: KeyInt, Default: "0", Description: "Maximum concurrent requests (0: automatic)"},
			{Name: "requests_deadline", Type: KeyDuration, Default: "10s", Description: "How long a request waits for a slot"},
			{Name: "root_access", Type: KeyBool, Default: "on", Description: "Allow the root credentials on the S3 API (off: denied there; the console and the credentials derived from root keep working)"},
		}},
		{Name: "browser", Description: "Web console", Keys: []KeySchema{
			{Name: "session_duration", Type: KeyDuration, Default: "12h", Description: "Lifetime of a console session"},
			{Name: "upload_max_size", Type: KeyInt, Default: "67108864", Description: "Largest single-request upload in bytes"},
		}},
		{Name: "logger", Description: "Server logs", Keys: []KeySchema{
			{Name: "level", Type: KeyEnum, Default: "info", Values: []string{"debug", "info", "warn", "error"}, Description: "Minimum level"},
			{Name: "format", Type: KeyEnum, Default: "text", Values: []string{"text", "json"}, Description: "Output format"},
		}},
		{Name: "notify_webhook", Description: "Bucket notifications to a webhook", Keys: notifyKeys(
			KeySchema{Name: "endpoint", Type: KeyURL, Description: "URL receiving the events"},
			KeySchema{Name: "auth_token", Type: KeyString, Description: "Bearer token sent to the endpoint (or \"<scheme> <credentials>\")", Sensitive: true},
			KeySchema{Name: "client_cert", Type: KeyString, Description: "PEM file of the client certificate"},
			KeySchema{Name: "client_key", Type: KeyString, Description: "PEM file of the client key"},
			KeySchema{Name: "tls_skip_verify", Type: KeyBool, Default: "off", Description: "Accept any server certificate"},
		)},
		{Name: "notify_kafka", Description: "Bucket notifications to Kafka", Keys: notifyKeys(
			KeySchema{Name: "brokers", Type: KeyList, Description: "Brokers (host:port), comma separated"},
			KeySchema{Name: "topic", Type: KeyString, Description: "Topic of the events"},
			KeySchema{Name: "sasl", Type: KeyBool, Default: "off", Description: "Authenticate with SASL"},
			KeySchema{Name: "sasl_username", Type: KeyString, Description: "SASL user name"},
			KeySchema{Name: "sasl_password", Type: KeyString, Description: "SASL password", Sensitive: true},
			KeySchema{Name: "sasl_mechanism", Type: KeyEnum, Default: "plain", Values: []string{"plain", "sha256", "sha512"}, Description: "SASL mechanism"},
			KeySchema{Name: "tls", Type: KeyBool, Default: "off", Description: "Connect with TLS"},
			KeySchema{Name: "tls_skip_verify", Type: KeyBool, Default: "off", Description: "Accept any server certificate"},
		)},
		{Name: "notify_nats", Description: "Bucket notifications to NATS", Keys: notifyKeys(
			KeySchema{Name: "address", Type: KeyString, Description: "Server address (host:port or nats:// URL)"},
			KeySchema{Name: "subject", Type: KeyString, Description: "Subject of the events"},
			KeySchema{Name: "username", Type: KeyString, Description: "User name"},
			KeySchema{Name: "password", Type: KeyString, Description: "Password", Sensitive: true},
			KeySchema{Name: "token", Type: KeyString, Description: "Authentication token", Sensitive: true},
			KeySchema{Name: "tls", Type: KeyBool, Default: "off", Description: "Connect with TLS"},
			KeySchema{Name: "tls_skip_verify", Type: KeyBool, Default: "off", Description: "Accept any server certificate"},
		)},
		{Name: "notify_amqp", Description: "Bucket notifications to AMQP (RabbitMQ)", Keys: notifyKeys(
			KeySchema{Name: "url", Type: KeyString, Description: "amqp:// or amqps:// URL of the broker", Sensitive: true},
			KeySchema{Name: "exchange", Type: KeyString, Description: "Exchange (declared when set)"},
			KeySchema{Name: "exchange_type", Type: KeyEnum, Default: "fanout", Values: []string{"direct", "fanout", "topic", "headers"}, Description: "Exchange type"},
			KeySchema{Name: "routing_key", Type: KeyString, Description: "Routing key"},
			KeySchema{Name: "mandatory", Type: KeyBool, Default: "off", Description: "Publish mandatory messages"},
			KeySchema{Name: "durable", Type: KeyBool, Default: "off", Description: "Declare a durable exchange"},
			KeySchema{Name: "delivery_mode", Type: KeyEnum, Default: "1", Values: []string{"1", "2"}, Description: "1 transient, 2 persistent"},
			KeySchema{Name: "tls_skip_verify", Type: KeyBool, Default: "off", Description: "Accept any server certificate"},
		)},
		{Name: "notify_redis", Description: "Bucket notifications to Redis", Keys: notifyKeys(
			KeySchema{Name: "address", Type: KeyString, Description: "Server address (host:port)"},
			KeySchema{Name: "key", Type: KeyString, Description: "Hash (namespace) or list (access) of the events"},
			KeySchema{Name: "format", Type: KeyEnum, Default: "namespace", Values: []string{"namespace", "access"}, Description: "namespace: one entry per object; access: every event"},
			KeySchema{Name: "user", Type: KeyString, Description: "User name (ACL)"},
			KeySchema{Name: "password", Type: KeyString, Description: "Password", Sensitive: true},
			KeySchema{Name: "tls", Type: KeyBool, Default: "off", Description: "Connect with TLS"},
			KeySchema{Name: "tls_skip_verify", Type: KeyBool, Default: "off", Description: "Accept any server certificate"},
		)},
		{Name: "notify_postgres", Description: "Bucket notifications to PostgreSQL", Keys: notifyKeys(
			KeySchema{Name: "connection_string", Type: KeyString, Description: "Connection string (postgres:// URL or key=value)", Sensitive: true},
			KeySchema{Name: "table", Type: KeyString, Description: "Table of the events (created when missing)"},
			KeySchema{Name: "format", Type: KeyEnum, Default: "namespace", Values: []string{"namespace", "access"}, Description: "namespace: one row per object; access: every event"},
		)},
		{Name: "region", Description: "Server location", Keys: []KeySchema{
			{Name: "name", Type: KeyString, Default: "us-east-1", Description: "Region name reported to clients"},
			{Name: "comment", Type: KeyString, Description: "Free text"},
		}},
		{Name: "scanner", Description: "Background scanner (usage, lifecycle, healing checks)", Keys: []KeySchema{
			{Name: "speed", Type: KeyEnum, Default: "default", Values: []string{"fastest", "fast", "default", "slow", "slowest"}, Description: "Pace of the scan"},
			{Name: "cycle", Type: KeyDuration, Default: "1m", Description: "Pause between two scan cycles"},
		}},
	}
}

// notifyKeys returns the keys of a notification target subsystem: the
// common ones and those of its kind.
func notifyKeys(specific ...KeySchema) []KeySchema {
	keys := []KeySchema{
		{Name: "enable", Type: KeyBool, Default: "off", Description: "Enable the target"},
	}
	keys = append(keys, specific...)
	return append(keys,
		KeySchema{Name: "queue_dir", Type: KeyString, Description: "Directory of the persistent event queue (default: the system volume of a local drive)"},
		KeySchema{Name: "queue_limit", Type: KeyInt, Default: "100000", Description: "Maximum queued events"},
		KeySchema{Name: "comment", Type: KeyString, Description: "Free text"},
	)
}

// subsystem returns the schema of a known subsystem. A notification target
// subsystem may name a target id (notify_webhook:<id>, ARN
// arn:nexo:sqs:<region>:<id>:webhook); without one it configures the
// target "_".
func subsystem(name string) (SubsystemSchema, error) {
	base := name
	if prefix, id, ok := strings.Cut(name, ":"); ok && strings.HasPrefix(prefix, "notify_") && id != "" && !strings.ContainsAny(id, ":/\\ ") {
		base = prefix
	}
	for _, s := range configSchema() {
		if s.Name == base {
			s.Name = name
			return s, nil
		}
	}
	return SubsystemSchema{}, fmt.Errorf("%w: unknown configuration subsystem %q", domain.ErrInvalidArgument, name)
}

// key returns the schema of a key of s.
func (s SubsystemSchema) key(name string) (KeySchema, bool) {
	for _, k := range s.Keys {
		if k.Name == name {
			return k, true
		}
	}
	return KeySchema{}, false
}

// Schema implements ConfigManager.
func (m *configManager) Schema(context.Context) []SubsystemSchema { return configSchema() }

// List implements ConfigManager: every known subsystem, configured or
// not, with its stored settings.
func (m *configManager) List(ctx context.Context) ([]Subsystem, error) {
	schema := configSchema()
	out := make([]Subsystem, 0, len(schema))
	for _, s := range schema {
		kv, err := m.Get(ctx, s.Name)
		if err != nil {
			return nil, err
		}
		out = append(out, Subsystem{Name: s.Name, Settings: kv})
	}
	return out, nil
}

// Get implements ConfigManager.
func (m *configManager) Get(ctx context.Context, name string) (config.KV, error) {
	s, err := subsystem(name)
	if err != nil {
		return nil, err
	}
	kv, err := m.deps.Config.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	return mask(s, kv), nil
}

// mask returns a copy of kv with the sensitive values replaced.
func mask(s SubsystemSchema, kv config.KV) config.KV {
	out := make(config.KV, len(kv))
	for k, v := range kv {
		if ks, ok := s.key(k); ok && ks.Sensitive && v != "" {
			v = SensitiveMask
		}
		out[k] = v
	}
	return out
}

// validate checks kv against the schema of s and returns it without the
// masked sensitive values (which keep the stored secret).
func validate(s SubsystemSchema, kv config.KV) (config.KV, error) {
	out := make(config.KV, len(kv))
	for k, v := range kv {
		ks, ok := s.key(k)
		if !ok {
			return nil, fmt.Errorf("%w: unknown key %q of subsystem %s", domain.ErrInvalidArgument, k, s.Name)
		}
		if ks.Sensitive && v == SensitiveMask {
			continue
		}
		if err := checkValue(ks, v); err != nil {
			return nil, fmt.Errorf("%w: %s.%s: %s", domain.ErrInvalidArgument, s.Name, k, err)
		}
		out[k] = v
	}
	return out, nil
}

// checkValue validates one value; the empty string resets a key to its
// default and is always valid.
func checkValue(ks KeySchema, v string) error {
	if v == "" {
		return nil
	}
	switch ks.Type {
	case KeyBool:
		switch strings.ToLower(v) {
		case "on", "off", "true", "false":
			return nil
		}
		return fmt.Errorf("%q is not a boolean (on, off, true, false)", v)
	case KeyInt:
		if n, err := strconv.ParseInt(v, 10, 64); err != nil || n < 0 {
			return fmt.Errorf("%q is not a non-negative integer", v)
		}
	case KeyDuration:
		if d, err := time.ParseDuration(v); err != nil || d < 0 {
			return fmt.Errorf("%q is not a duration (e.g. 30s, 12h)", v)
		}
	case KeyURL:
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("%q is not an http(s) URL", v)
		}
	case KeyEnum:
		for _, allowed := range ks.Values {
			if v == allowed {
				return nil
			}
		}
		return fmt.Errorf("%q is not one of %s", v, strings.Join(ks.Values, ", "))
	case KeyList:
		for _, item := range strings.Split(v, ",") {
			if strings.TrimSpace(item) == "" {
				return fmt.Errorf("%q has an empty item", v)
			}
		}
	}
	return nil
}

// Set implements ConfigManager.
func (m *configManager) Set(ctx context.Context, name string, kv config.KV) (config.KV, error) {
	s, err := subsystem(name)
	if err != nil {
		return nil, err
	}
	valid, err := validate(s, kv)
	if err != nil {
		return nil, err
	}
	if len(valid) > 0 {
		if err := m.deps.Config.Set(ctx, name, valid); err != nil {
			return nil, err
		}
	}
	m.deps.Logger.InfoContext(ctx, "admin: configuration changed", "subsystem", name, "keys", sortedKeys(valid))
	return m.Get(ctx, name)
}

// SetAll implements ConfigManager.
func (m *configManager) SetAll(ctx context.Context, all map[string]config.KV) error {
	valid := make(map[string]config.KV, len(all))
	for name, kv := range all {
		s, err := subsystem(name)
		if err != nil {
			return err
		}
		if valid[name], err = validate(s, kv); err != nil {
			return err
		}
	}
	for _, name := range sortedKeys(valid) {
		if len(valid[name]) == 0 {
			continue
		}
		if err := m.deps.Config.Set(ctx, name, valid[name]); err != nil {
			return err
		}
	}
	return nil
}

// Delete implements ConfigManager.
func (m *configManager) Delete(ctx context.Context, name string, keys []string) error {
	s, err := subsystem(name)
	if err != nil {
		return err
	}
	for _, k := range keys {
		if _, ok := s.key(k); !ok {
			return fmt.Errorf("%w: unknown key %q of subsystem %s", domain.ErrInvalidArgument, k, name)
		}
	}
	return m.deps.Config.Delete(ctx, name, keys...)
}

// Export implements ConfigManager.
func (m *configManager) Export(ctx context.Context) ([]byte, error) {
	return m.deps.Config.Export(ctx)
}

// Import implements ConfigManager.
func (m *configManager) Import(ctx context.Context, data []byte) error {
	var all map[string]config.KV
	if err := json.Unmarshal(data, &all); err != nil {
		return fmt.Errorf("%w: configuration document: %v", domain.ErrInvalidArgument, err)
	}
	for name, kv := range all {
		s, err := subsystem(name)
		if err != nil {
			return err
		}
		for k, v := range kv {
			ks, ok := s.key(k)
			if !ok {
				return fmt.Errorf("%w: unknown key %q of subsystem %s", domain.ErrInvalidArgument, k, name)
			}
			if err := checkValue(ks, v); err != nil {
				return fmt.Errorf("%w: %s.%s: %s", domain.ErrInvalidArgument, name, k, err)
			}
		}
	}
	if err := m.deps.Config.Import(ctx, data); err != nil {
		return err
	}
	m.deps.Logger.InfoContext(ctx, "admin: configuration imported", "subsystems", len(all))
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
