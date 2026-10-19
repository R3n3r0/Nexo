package event

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
)

// Configuration subsystems of the targets: notify_<kind> configures the
// target of id DefaultTargetID, notify_<kind>:<id> the target <id>.
const (
	SubsystemPrefix = "notify_"
	// DefaultTargetID is the id of the target of a subsystem without id
	// (arn:nexo:sqs:<region>:_:<type>).
	DefaultTargetID = "_"
	// DefaultQueueLimit is the default capacity of a target queue.
	DefaultQueueLimit = 100000
)

// Keys of the notify_* subsystems.
const (
	KeyEnable           = "enable"
	KeyQueueDir         = "queue_dir"
	KeyQueueLimit       = "queue_limit"
	KeyComment          = "comment"
	KeyEndpoint         = "endpoint"
	KeyAuthToken        = "auth_token"
	KeyClientCert       = "client_cert"
	KeyClientKey        = "client_key"
	KeyTLSSkipVerify    = "tls_skip_verify"
	KeyTLS              = "tls"
	KeyBrokers          = "brokers"
	KeyTopic            = "topic"
	KeySASL             = "sasl"
	KeySASLUsername     = "sasl_username"
	KeySASLPassword     = "sasl_password"
	KeySASLMechanism    = "sasl_mechanism"
	KeyAddress          = "address"
	KeySubject          = "subject"
	KeyUsername         = "username"
	KeyUser             = "user"
	KeyPassword         = "password"
	KeyToken            = "token"
	KeyURL              = "url"
	KeyExchange         = "exchange"
	KeyExchangeType     = "exchange_type"
	KeyRoutingKey       = "routing_key"
	KeyMandatory        = "mandatory"
	KeyDurable          = "durable"
	KeyDeliveryMode     = "delivery_mode"
	KeyKey              = "key"
	KeyFormat           = "format"
	KeyConnectionString = "connection_string"
	KeyTable            = "table"
)

// subsystemTypes maps the kind of a notify_<kind> subsystem to its target
// type.
func subsystemTypes() map[string]string {
	return map[string]string{
		"webhook":      TypeWebhook,
		"kafka":        TypeKafka,
		"nats":         TypeNATS,
		"amqp":         TypeAMQP,
		"redis":        TypeRedis,
		"postgres":     TypePostgres,
		"media_worker": TypeMediaWorker,
	}
}

// IsTargetSubsystem reports whether a configuration subsystem configures a
// notification target (notify_<kind> or notify_<kind>:<id>).
func IsTargetSubsystem(subsystem string) bool {
	_, _, err := splitSubsystem(subsystem)
	return err == nil
}

// splitSubsystem returns the target type and id of a notify subsystem.
func splitSubsystem(subsystem string) (typ, id string, err error) {
	rest, ok := strings.CutPrefix(subsystem, SubsystemPrefix)
	if !ok {
		return "", "", fmt.Errorf("%w: %q is not a notification subsystem", domain.ErrInvalidArgument, subsystem)
	}
	kind, id, hasID := strings.Cut(rest, ":")
	typ, known := subsystemTypes()[kind]
	if !known {
		return "", "", fmt.Errorf("%w: unknown notification target kind %q", domain.ErrInvalidArgument, kind)
	}
	if !hasID {
		id = DefaultTargetID
	}
	if id == "" || strings.ContainsAny(id, ":/\\ ") {
		return "", "", fmt.Errorf("%w: invalid notification target id %q", domain.ErrInvalidArgument, id)
	}
	return typ, id, nil
}

// ParseTargetConfig builds the configuration of a target from its
// notify_<kind>[:<id>] subsystem. A disabled target (enable off, the
// default) is returned with Enabled false and is not validated further.
func ParseTargetConfig(subsystem string, kv map[string]string) (TargetConfig, error) {
	typ, id, err := splitSubsystem(subsystem)
	if err != nil {
		return TargetConfig{}, err
	}
	cfg := TargetConfig{ID: id, Type: typ, QueueDir: strings.TrimSpace(kv[KeyQueueDir])}
	if cfg.Enabled, err = parseBool(kv, KeyEnable); err != nil {
		return TargetConfig{}, err
	}
	if v := strings.TrimSpace(kv[KeyQueueLimit]); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return TargetConfig{}, fmt.Errorf("%w: %s: queue_limit %q is not a non-negative integer", domain.ErrInvalidArgument, subsystem, v)
		}
		cfg.QueueLimit = n
	}
	if err := ValidateQueueDir(cfg.QueueDir); err != nil {
		return TargetConfig{}, fmt.Errorf("%s: %w", subsystem, err)
	}
	if !cfg.Enabled {
		return cfg, nil
	}
	var flags struct{ tls, skip, sasl, mandatory, durable bool }
	for key, dst := range map[string]*bool{
		KeyTLS: &flags.tls, KeyTLSSkipVerify: &flags.skip, KeySASL: &flags.sasl, KeyMandatory: &flags.mandatory, KeyDurable: &flags.durable,
	} {
		if *dst, err = parseBool(kv, key); err != nil {
			return TargetConfig{}, fmt.Errorf("%s: %w", subsystem, err)
		}
	}
	get := func(k string) string { return strings.TrimSpace(kv[k]) }
	tlsOpts := TLSOptions{Enabled: flags.tls, SkipVerify: flags.skip}
	switch typ {
	case TypeWebhook, TypeMediaWorker:
		if typ == TypeMediaWorker {
			cfg.Filter = Filter{Events: []Name{MediaJob}}
		}
		cfg.Webhook = WebhookConfig{
			Endpoint: get(KeyEndpoint), AuthToken: kv[KeyAuthToken], ClientCert: get(KeyClientCert), ClientKey: get(KeyClientKey),
			TLSSkipVerify: flags.skip,
		}
	case TypeKafka:
		cfg.Kafka = KafkaConfig{Brokers: splitList(get(KeyBrokers)), Topic: get(KeyTopic), TLS: tlsOpts}
		if flags.sasl || get(KeySASLUsername) != "" {
			mech := strings.ToLower(get(KeySASLMechanism))
			if mech == "" {
				mech = "plain"
			}
			cfg.Kafka.SASL = SASLConfig{Mechanism: mech, Username: get(KeySASLUsername), Password: kv[KeySASLPassword]}
		}
	case TypeNATS:
		cfg.NATS = NATSConfig{
			Address: get(KeyAddress), Subject: get(KeySubject), Username: get(KeyUsername), Password: kv[KeyPassword],
			Token: kv[KeyToken], TLS: tlsOpts,
		}
	case TypeAMQP:
		cfg.AMQP = AMQPConfig{
			URL: get(KeyURL), Exchange: get(KeyExchange), ExchangeType: get(KeyExchangeType), RoutingKey: get(KeyRoutingKey),
			Mandatory: flags.mandatory, Durable: flags.durable, TLSSkipVerify: flags.skip,
		}
		switch get(KeyDeliveryMode) {
		case "", "1":
		case "2":
			cfg.AMQP.Persistent = true
		default:
			return TargetConfig{}, fmt.Errorf("%w: %s: delivery_mode must be 1 or 2", domain.ErrInvalidArgument, subsystem)
		}
	case TypeRedis:
		user := get(KeyUser)
		if user == "" {
			user = get(KeyUsername)
		}
		cfg.Redis = RedisConfig{
			Address: get(KeyAddress), Key: get(KeyKey), Format: strings.ToLower(get(KeyFormat)), Username: user,
			Password: kv[KeyPassword], TLS: tlsOpts,
		}
	case TypePostgres:
		cfg.Postgres = PostgresConfig{ConnectionString: get(KeyConnectionString), Table: get(KeyTable), Format: strings.ToLower(get(KeyFormat))}
	}
	if err := validateTarget(cfg); err != nil {
		return TargetConfig{}, fmt.Errorf("%s: %w", subsystem, err)
	}
	return cfg, nil
}

// parseBool reads an on/off setting (empty is off).
func parseBool(kv map[string]string, key string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(kv[key])) {
	case "", "off", "false":
		return false, nil
	case "on", "true":
		return true, nil
	}
	return false, fmt.Errorf("%w: %s %q is not a boolean (on, off)", domain.ErrInvalidArgument, key, kv[key])
}

// splitList splits a comma separated list, dropping the empty entries.
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
