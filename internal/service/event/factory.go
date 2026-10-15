package event

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
)

// Defaults of the targets.
const (
	defaultUserAgent   = "nexo-webhook"
	defaultHTTPTimeout = 10 * time.Second
	defaultDialTimeout = 10 * time.Second
)

// FactoryOption configures the target factory.
type FactoryOption func(*factory)

// WithUserAgent sets the User-Agent header of the webhook deliveries
// (default "nexo-webhook").
func WithUserAgent(ua string) FactoryOption {
	return func(f *factory) {
		if ua != "" {
			f.userAgent = ua
		}
	}
}

// factory builds the targets of every type.
type factory struct {
	deps      FactoryDeps
	userAgent string
	// webhookOnly restricts the factory to webhook targets.
	webhookOnly bool
}

var _ TargetFactory = (*factory)(nil)

// NewTargetFactory returns the factory of every target type; nil dialers
// are replaced by the real clients (segmentio/kafka-go, nats.go,
// amqp091-go, go-redis, pgx).
func NewTargetFactory(deps FactoryDeps, opts ...FactoryOption) TargetFactory {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Kafka == nil {
		deps.Kafka = kafkaDialer{}
	}
	if deps.NATS == nil {
		deps.NATS = natsDialer{}
	}
	if deps.AMQP == nil {
		deps.AMQP = amqpDialer{}
	}
	if deps.Redis == nil {
		deps.Redis = redisDialer{}
	}
	if deps.Postgres == nil {
		deps.Postgres = postgresDialer{}
	}
	f := &factory{deps: deps, userAgent: defaultUserAgent}
	for _, o := range opts {
		o(f)
	}
	return f
}

// NewWebhookFactory returns a factory of webhook targets only, sharing
// deps.Client (default: a client per target with a 10s timeout).
func NewWebhookFactory(deps WebhookDeps, opts ...FactoryOption) TargetFactory {
	f := NewTargetFactory(FactoryDeps{HTTP: deps.Client, Logger: deps.Logger}, opts...).(*factory)
	f.webhookOnly = true
	return f
}

// New implements TargetFactory.
func (f *factory) New(_ context.Context, cfg TargetConfig) (Target, error) {
	if f.webhookOnly && cfg.Type != TypeWebhook {
		return nil, fmt.Errorf("%w: unsupported target type %q", domain.ErrInvalidArgument, cfg.Type)
	}
	if err := validateTarget(cfg); err != nil {
		return nil, err
	}
	id := cfg.TargetID()
	switch cfg.Type {
	case TypeWebhook:
		client := withoutRedirects(f.deps.HTTP)
		if client == nil {
			c, err := webhookClient(cfg.Webhook)
			if err != nil {
				return nil, err
			}
			client = c
		}
		return &webhook{id: id, cfg: cfg.Webhook, client: client, userAgent: f.userAgent}, nil
	case TypeKafka:
		k := cfg.Kafka
		return &kafkaTarget{id: id, conn: newLazy(func(ctx context.Context) (KafkaProducer, error) { return f.deps.Kafka.Dial(ctx, k) })}, nil
	case TypeNATS:
		n := cfg.NATS
		return &natsTarget{id: id, subject: n.Subject, conn: newLazy(func(ctx context.Context) (NATSConn, error) { return f.deps.NATS.Dial(ctx, n) })}, nil
	case TypeAMQP:
		a := cfg.AMQP
		return &amqpTarget{id: id, cfg: a, conn: newLazy(func(ctx context.Context) (AMQPChannel, error) { return f.deps.AMQP.Dial(ctx, a) })}, nil
	case TypeRedis:
		r := cfg.Redis
		return &redisTarget{id: id, cfg: r, conn: newLazy(func(ctx context.Context) (RedisConn, error) { return f.deps.Redis.Dial(ctx, r) })}, nil
	case TypePostgres:
		p := cfg.Postgres
		t := &postgresTarget{id: id, cfg: p}
		t.conn = newLazy(func(ctx context.Context) (PostgresConn, error) { return t.dial(ctx, f.deps.Postgres) })
		return t, nil
	}
	return nil, fmt.Errorf("%w: unsupported target type %q", domain.ErrInvalidArgument, cfg.Type)
}

// validateTarget checks the settings of a target of any type.
func validateTarget(cfg TargetConfig) error {
	if cfg.ID == "" {
		return fmt.Errorf("%w: target id is required", domain.ErrInvalidArgument)
	}
	if cfg.QueueLimit < 0 {
		return fmt.Errorf("%w: negative queue limit", domain.ErrInvalidArgument)
	}
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s target %s: %s", domain.ErrInvalidArgument, cfg.Type, cfg.ID, fmt.Sprintf(format, args...))
	}
	if err := ValidateQueueDir(cfg.QueueDir); err != nil {
		return fmt.Errorf("%s target %s: %w", cfg.Type, cfg.ID, err)
	}
	switch cfg.Type {
	case TypeWebhook:
		u, err := url.Parse(cfg.Webhook.Endpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return invalid("endpoint %q must be an http(s) URL", redactURL(cfg.Webhook.Endpoint))
		}
		if (cfg.Webhook.ClientCert == "") != (cfg.Webhook.ClientKey == "") {
			return invalid("client_cert and client_key go together")
		}
	case TypeKafka:
		if len(cfg.Kafka.Brokers) == 0 || cfg.Kafka.Topic == "" {
			return invalid("brokers and topic are required")
		}
		switch cfg.Kafka.SASL.Mechanism {
		case "", "plain", "sha256", "sha512":
		default:
			return invalid("unknown SASL mechanism %q (plain, sha256, sha512)", cfg.Kafka.SASL.Mechanism)
		}
	case TypeNATS:
		if cfg.NATS.Address == "" || cfg.NATS.Subject == "" {
			return invalid("address and subject are required")
		}
	case TypeAMQP:
		u, err := url.Parse(cfg.AMQP.URL)
		if err != nil || (u.Scheme != "amqp" && u.Scheme != "amqps") || u.Host == "" {
			return invalid("url must be an amqp:// or amqps:// URL")
		}
		switch cfg.AMQP.ExchangeType {
		case "", "direct", "fanout", "topic", "headers":
		default:
			return invalid("unknown exchange type %q", cfg.AMQP.ExchangeType)
		}
	case TypeRedis:
		if cfg.Redis.Address == "" || cfg.Redis.Key == "" {
			return invalid("address and key are required")
		}
		if err := checkFormat(cfg.Redis.Format); err != nil {
			return invalid("%v", err)
		}
	case TypePostgres:
		if cfg.Postgres.ConnectionString == "" {
			return invalid("connection_string is required")
		}
		if !validTableName(cfg.Postgres.Table) {
			return invalid("table %q is not a valid SQL identifier", cfg.Postgres.Table)
		}
		if err := checkFormat(cfg.Postgres.Format); err != nil {
			return invalid("%v", err)
		}
	default:
		return fmt.Errorf("%w: unsupported target type %q", domain.ErrInvalidArgument, cfg.Type)
	}
	return nil
}

// checkFormat validates the format of a Redis or PostgreSQL target.
func checkFormat(format string) error {
	switch format {
	case FormatNamespace, FormatAccess:
		return nil
	}
	return fmt.Errorf("format %q must be %s or %s", format, FormatNamespace, FormatAccess)
}

// validTableName accepts [schema.]table made of letters, digits and
// underscores, not starting with a digit: the name is written in the SQL
// statements.
func validTableName(name string) bool {
	if name == "" {
		return false
	}
	parts := strings.Split(name, ".")
	if len(parts) > 2 {
		return false
	}
	for _, p := range parts {
		if p == "" || len(p) > 63 {
			return false
		}
		for i, c := range p {
			switch {
			case c == '_', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
			case c >= '0' && c <= '9' && i > 0:
			default:
				return false
			}
		}
	}
	return true
}

// tlsConfig returns the client TLS configuration of a broker connection.
func tlsConfig(skipVerify bool) *tls.Config {
	// The verification is skipped only when the target asks for it
	// (tls_skip_verify, test setups).
	return &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: skipVerify} // #nosec G402 -- InsecureSkipVerify is an explicit opt-in of the target configuration (tls_skip_verify), off by default
}

// webhookClient returns the HTTP client of a webhook target.
func webhookClient(cfg WebhookConfig) (*http.Client, error) {
	tc := tlsConfig(cfg.TLSSkipVerify)
	if cfg.ClientCert != "" {
		cert, err := tls.LoadX509KeyPair(cfg.ClientCert, cfg.ClientKey)
		if err != nil {
			return nil, fmt.Errorf("%w: webhook client certificate: %v", domain.ErrInvalidArgument, err)
		}
		tc.Certificates = []tls.Certificate{cert}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tc
	return &http.Client{Timeout: defaultHTTPTimeout, Transport: transport, CheckRedirect: noRedirects}, nil
}

// noRedirects is the redirect policy of the webhook clients: a redirect is
// never followed, the 3xx answer is the response (a failed delivery). The
// receiver of a webhook, often a third party, could otherwise send the
// POST of Nexo, with its body and the auth_token of the target, to any
// host the server reaches (127.0.0.1, 169.254.169.254: a blind SSRF),
// which no administrator configured.
func noRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// withoutRedirects returns the client of a webhook target: an injected
// *http.Client that would follow redirects (its default policy) is copied
// with noRedirects; other HTTPDoer implementations must not follow them
// (FactoryDeps.HTTP).
func withoutRedirects(client HTTPDoer) HTTPDoer {
	c, ok := client.(*http.Client)
	if !ok || c == nil {
		return client
	}
	cp := *c // http.Client holds no lock: a copy is a valid client
	cp.CheckRedirect = noRedirects
	return &cp
}

// closer is a connection of a target.
type closer interface {
	Close(ctx context.Context) error
}

// lazy is the connection of a target: opened by the first send, closed
// and dropped after a failed send so that the next one reconnects.
type lazy[T closer] struct {
	dial func(ctx context.Context) (T, error)

	mu   sync.Mutex
	conn T
	open bool
}

func newLazy[T closer](dial func(ctx context.Context) (T, error)) *lazy[T] {
	return &lazy[T]{dial: dial}
}

// do runs fn on the connection, opening it first when needed.
func (l *lazy[T]) do(ctx context.Context, fn func(conn T) error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.open {
		conn, err := l.dial(ctx)
		if err != nil {
			return fmt.Errorf("connect: %w", err)
		}
		l.conn, l.open = conn, true
	}
	if err := fn(l.conn); err != nil {
		_ = l.conn.Close(ctx)
		var zero T
		l.conn, l.open = zero, false
		return err
	}
	return nil
}

// close closes the connection when open.
func (l *lazy[T]) close(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.open {
		return nil
	}
	err := l.conn.Close(ctx)
	var zero T
	l.conn, l.open = zero, false
	return err
}

// ValidateQueueDir checks a queue_dir: empty (the default queue) or a
// relative path that stays inside the queue root it is resolved against
// (see WithQueueRoot); it fails with domain.ErrInvalidArgument. An
// absolute path or ".." would let the holder of the configuration
// permission make the dispatcher delete and send the files of any
// directory (a queue consumes the *.event files it finds and removes the
// *.tmp ones).
func ValidateQueueDir(dir string) error {
	if dir == "" || (filepath.IsLocal(dir) && filepath.Clean(dir) != "." && !strings.Contains(dir, "\\")) {
		return nil
	}
	return fmt.Errorf("%w: queue_dir %q must be a relative directory inside the event queue root (no absolute path, no ..)",
		domain.ErrInvalidArgument, dir)
}
