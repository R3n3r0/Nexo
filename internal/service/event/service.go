// Package event implements bucket notifications (S-BKT-10): S3-format
// events published by the object layer and the S3 transport, routed by the
// notification configuration of their bucket (and by the filters of the
// targets) to the configured targets: webhook, Kafka, NATS, AMQP, Redis
// and PostgreSQL. Every target has a persistent queue (one file per event)
// drained in order by its own sender, which retries with a backoff while
// the target is offline: the events survive an outage of the target and a
// restart of the process.
//
// The event model (Event, Name, Filter matching), the notification
// configuration (BucketConfig, ARN) and the target configuration read from
// the notify_* subsystems of the dynamic configuration live in the other
// files; this file holds the service contracts only.
package event

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/r3n3r0/nexo/internal/lifecycle"
	"github.com/r3n3r0/nexo/internal/service/config"
)

// Service publishes events and manages the notification targets.
type Service interface {
	// Publish enqueues an event for delivery to every target the
	// notification configuration of its bucket (or the filter of a target)
	// routes it to. The request of ctx (WithRequest) completes the event:
	// identity, client, request ids, lifecycle expirations. It returns an
	// error only when the event cannot be queued.
	Publish(ctx context.Context, e Event) error

	// SetTarget creates or replaces a target from its configuration; the
	// queued events of a replaced target are kept.
	SetTarget(ctx context.Context, cfg TargetConfig) error
	// GetTarget returns the configuration of a target, or
	// domain.ErrNotFound.
	GetTarget(ctx context.Context, id TargetID) (TargetConfig, error)
	// ListTargets returns the targets sorted by id and type.
	ListTargets(ctx context.Context) ([]TargetConfig, error)
	// RemoveTarget removes a target, or fails with domain.ErrNotFound.
	RemoveTarget(ctx context.Context, id TargetID) error
	// TargetsStatus returns the delivery state of every target of this
	// node, sorted like ListTargets: online until a delivery fails and
	// again from the next success, with the events pending in its queue.
	TargetsStatus(ctx context.Context) ([]TargetStatus, error)
	// TestTarget sends a test event (TestEvent) to a target at once,
	// outside of its queue (the queued events keep their order), and
	// reports the outcome, which also updates the state of the target. An
	// unknown target is domain.ErrNotFound; a failed delivery is a
	// TestResult, not an error.
	TestTarget(ctx context.Context, id TargetID) (TestResult, error)

	// ValidateBucketConfig parses a NotificationConfiguration document,
	// checks its events, filters and destinations (every ARN must name a
	// configured target of this region: S3 InvalidArgument otherwise) and
	// returns the canonical document to store (rules without Id get one).
	ValidateBucketConfig(ctx context.Context, doc []byte) ([]byte, error)
}

// Dispatcher is a Service with the background routing and delivery
// goroutines managed by lifecycle.
type Dispatcher interface {
	Service
	lifecycle.Runner
}

// Target delivers events to one destination.
type Target interface {
	// ID returns the target identifier (pure accessor, no context).
	ID() TargetID
	// Send delivers one event; an error leaves the event queued, to be
	// sent again after a backoff.
	Send(ctx context.Context, e Event) error
	Close(ctx context.Context) error
}

// TargetFactory builds a Target from its configuration. Building a target
// validates its configuration but does not connect: the targets connect
// when they send, so that a destination offline at startup only delays
// the delivery.
type TargetFactory interface {
	New(ctx context.Context, cfg TargetConfig) (Target, error)
}

// BucketRules provides the notification configuration document of a bucket
// (the one stored by the bucket service): nil when the bucket has none.
type BucketRules interface {
	NotificationConfig(ctx context.Context, bucket string) ([]byte, error)
}

// HTTPDoer is the subset of *http.Client the webhook targets need, so that
// tests can inject the generated mock. The request carries its context
// (http.Request.Context), which is why Do has no context parameter.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// KafkaProducer writes the events of a Kafka target to its topic.
type KafkaProducer interface {
	// WriteMessage writes one message and waits for its acknowledgement.
	WriteMessage(ctx context.Context, key, value []byte) error
	Close(ctx context.Context) error
}

// KafkaDialer opens the producers of the Kafka targets.
type KafkaDialer interface {
	Dial(ctx context.Context, cfg KafkaConfig) (KafkaProducer, error)
}

// NATSConn publishes the events of a NATS target.
type NATSConn interface {
	// Publish publishes data on subject and waits until the server has
	// processed it.
	Publish(ctx context.Context, subject string, data []byte) error
	Close(ctx context.Context) error
}

// NATSDialer opens the connections of the NATS targets.
type NATSDialer interface {
	Dial(ctx context.Context, cfg NATSConfig) (NATSConn, error)
}

// AMQPMessage is a message published to an AMQP exchange.
type AMQPMessage struct {
	Exchange   string
	RoutingKey string
	Mandatory  bool
	// Persistent asks the broker to store the message on disk.
	Persistent  bool
	ContentType string
	Body        []byte
}

// AMQPChannel publishes the events of an AMQP target.
type AMQPChannel interface {
	// Publish publishes msg and waits for the broker confirmation.
	Publish(ctx context.Context, msg AMQPMessage) error
	Close(ctx context.Context) error
}

// AMQPDialer opens the channels of the AMQP targets (declaring their
// exchange).
type AMQPDialer interface {
	Dial(ctx context.Context, cfg AMQPConfig) (AMQPChannel, error)
}

// RedisConn is the subset of Redis commands the Redis targets use.
type RedisConn interface {
	HSet(ctx context.Context, key, field string, value []byte) error
	HDel(ctx context.Context, key, field string) error
	RPush(ctx context.Context, key string, value []byte) error
	Close(ctx context.Context) error
}

// RedisDialer opens the connections of the Redis targets.
type RedisDialer interface {
	Dial(ctx context.Context, cfg RedisConfig) (RedisConn, error)
}

// PostgresConn executes the statements of a PostgreSQL target.
type PostgresConn interface {
	Exec(ctx context.Context, sql string, args []any) error
	Close(ctx context.Context) error
}

// PostgresDialer opens the connections of the PostgreSQL targets.
type PostgresDialer interface {
	Dial(ctx context.Context, cfg PostgresConfig) (PostgresConn, error)
}

// Deps are the dependencies of the dispatcher.
type Deps struct {
	// Factory builds targets from configurations (default:
	// NewTargetFactory with the real clients).
	Factory TargetFactory
	// Rules provides the notification configuration of the buckets; nil
	// routes by the filters of the targets only.
	Rules BucketRules
	// Config provides the targets of the notify_* subsystems, loaded at
	// Start and reloaded when they change; nil leaves the targets to
	// SetTarget.
	Config config.Service
	Logger *slog.Logger
}

// FactoryDeps are the dependencies of the target factory. Nil clients and
// dialers are replaced by the real ones.
type FactoryDeps struct {
	// HTTP performs the webhook calls (default: an http.Client per target,
	// honoring its TLS settings).
	HTTP     HTTPDoer
	Kafka    KafkaDialer
	NATS     NATSDialer
	AMQP     AMQPDialer
	Redis    RedisDialer
	Postgres PostgresDialer
	Logger   *slog.Logger
}

// WebhookDeps are the dependencies of the webhook-only factory.
type WebhookDeps struct {
	// Client performs the HTTP calls (default: http.Client with a 10s timeout).
	Client HTTPDoer
	Logger *slog.Logger
}

// Target types: the last component of a target ARN and the suffix of its
// notify_<type> configuration subsystem (notify_postgres configures the
// postgresql targets).
const (
	TypeWebhook  = "webhook"
	TypeKafka    = "kafka"
	TypeNATS     = "nats"
	TypeAMQP     = "amqp"
	TypeRedis    = "redis"
	TypePostgres = "postgresql"
)

// Formats of the Redis and PostgreSQL targets.
const (
	// FormatNamespace keeps one entry per object (a hash field, a table
	// row) updated by the creations and removed by the deletions.
	FormatNamespace = "namespace"
	// FormatAccess appends every event (a list, a table) with its time.
	FormatAccess = "access"
)

// TargetID identifies a target: its id and its type, the last two
// components of its ARN (arn:nexo:sqs:<region>:<ID>:<Type>).
type TargetID struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

// TargetConfig is the configuration of one target.
type TargetConfig struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	// Enabled targets receive events; disabled ones keep their queue.
	Enabled bool `json:"enabled"`
	// QueueDir is the directory of the persistent queue of the target
	// (default: a directory of the queue root of the dispatcher, in
	// memory without one); QueueLimit bounds the queued events (default
	// 100000): beyond it new events are dropped.
	QueueDir   string `json:"queueDir,omitempty"`
	QueueLimit int    `json:"queueLimit,omitempty"`
	// Filter routes events to the target without a bucket notification
	// configuration; the zero Filter routes nothing (the bucket
	// configurations name their targets).
	Filter   Filter         `json:"filter"`
	Webhook  WebhookConfig  `json:"webhook"`
	Kafka    KafkaConfig    `json:"kafka"`
	NATS     NATSConfig     `json:"nats"`
	AMQP     AMQPConfig     `json:"amqp"`
	Redis    RedisConfig    `json:"redis"`
	Postgres PostgresConfig `json:"postgres"`
}

// TargetStatus is the delivery state of a target (Service.TargetsStatus).
type TargetStatus struct {
	Config TargetConfig
	// Online is false from a failed delivery to the next successful one.
	Online bool
	// Pending counts the events waiting in the queue of the target.
	Pending int
	// LastError is the error of the last failed delivery, "" when the
	// last delivery succeeded.
	LastError string
}

// TestResult is the outcome of Service.TestTarget.
type TestResult struct {
	Delivered bool
	// Error is the reason of a failed delivery.
	Error string
}

// TLSOptions are the TLS settings of a broker connection.
type TLSOptions struct {
	// Enabled connects with TLS (the URL schemes imply it where they exist).
	Enabled bool `json:"enabled,omitempty"`
	// SkipVerify accepts any server certificate (test setups only).
	SkipVerify bool `json:"skipVerify,omitempty"`
}

// WebhookConfig configures a webhook target: events are POSTed as JSON.
type WebhookConfig struct {
	Endpoint string `json:"endpoint"`
	// AuthToken is sent in the Authorization header: as a Bearer token,
	// or as is when it already names its scheme ("Basic xyz").
	AuthToken string `json:"authToken,omitempty"`
	// ClientCert and ClientKey are the PEM files of a client certificate.
	ClientCert string `json:"clientCert,omitempty"`
	ClientKey  string `json:"clientKey,omitempty"`
	// TLSSkipVerify accepts any server certificate.
	TLSSkipVerify bool `json:"tlsSkipVerify,omitempty"`
}

// SASLConfig is the SASL authentication of a Kafka target.
type SASLConfig struct {
	// Mechanism is "plain", "sha256" or "sha512" (SCRAM); empty disables SASL.
	Mechanism string `json:"mechanism,omitempty"`
	Username  string `json:"username,omitempty"`
	Password  string `json:"password,omitempty"`
}

// KafkaConfig configures a Kafka target.
type KafkaConfig struct {
	Brokers []string   `json:"brokers"`
	Topic   string     `json:"topic"`
	SASL    SASLConfig `json:"sasl"`
	TLS     TLSOptions `json:"tls"`
}

// NATSConfig configures a NATS target.
type NATSConfig struct {
	// Address is host:port or a nats:// (tls://) URL.
	Address  string     `json:"address"`
	Subject  string     `json:"subject"`
	Username string     `json:"username,omitempty"`
	Password string     `json:"password,omitempty"`
	Token    string     `json:"token,omitempty"`
	TLS      TLSOptions `json:"tls"`
}

// AMQPConfig configures an AMQP 0-9-1 (RabbitMQ) target.
type AMQPConfig struct {
	// URL is the amqp:// or amqps:// URL of the broker.
	URL string `json:"url"`
	// Exchange is declared at connection when set (ExchangeType, default
	// "fanout"; Durable); empty publishes to the default exchange.
	Exchange     string `json:"exchange,omitempty"`
	ExchangeType string `json:"exchangeType,omitempty"`
	RoutingKey   string `json:"routingKey,omitempty"`
	Mandatory    bool   `json:"mandatory,omitempty"`
	Durable      bool   `json:"durable,omitempty"`
	// Persistent publishes persistent messages (delivery mode 2).
	Persistent    bool `json:"persistent,omitempty"`
	TLSSkipVerify bool `json:"tlsSkipVerify,omitempty"`
}

// RedisConfig configures a Redis target.
type RedisConfig struct {
	Address string `json:"address"`
	// Key is the hash (FormatNamespace) or the list (FormatAccess).
	Key      string     `json:"key"`
	Format   string     `json:"format"`
	Username string     `json:"username,omitempty"`
	Password string     `json:"password,omitempty"`
	TLS      TLSOptions `json:"tls"`
}

// PostgresConfig configures a PostgreSQL target; its table is created when
// missing.
type PostgresConfig struct {
	ConnectionString string `json:"connectionString"`
	Table            string `json:"table"`
	Format           string `json:"format"`
}

// Filter selects the events a target receives without a bucket
// configuration. Empty fields match all; the zero Filter selects nothing.
type Filter struct {
	// Events are the event name patterns (e.g. "s3:ObjectCreated:*").
	Events []Name `json:"events,omitempty"`
	Bucket string `json:"bucket,omitempty"`
	Prefix string `json:"prefix,omitempty"`
	Suffix string `json:"suffix,omitempty"`
}

// Request describes the request that caused an event (WithRequest).
type Request struct {
	// PrincipalID is the access key of the requester (userIdentity).
	PrincipalID string
	// SourceIP is the client address (requestParameters.sourceIPAddress),
	// Port its port.
	SourceIP string
	Port     string
	// RequestID and HostID are the x-amz-request-id and x-amz-id-2 of the
	// response (responseElements).
	RequestID string
	HostID    string
	UserAgent string
	// Operation returns the S3 operation the request was routed to
	// (PostObject turns an ObjectCreated:Put into ObjectCreated:Post); it
	// may be nil.
	Operation func() string
	// Lifecycle marks the removals of the lifecycle rules: their
	// ObjectRemoved events are published as LifecycleExpiration events.
	Lifecycle bool
}
