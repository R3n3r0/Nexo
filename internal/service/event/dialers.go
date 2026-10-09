package event

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/redis/go-redis/v9"
	"github.com/segmentio/kafka-go"
	"github.com/segmentio/kafka-go/sasl"
	"github.com/segmentio/kafka-go/sasl/plain"
	"github.com/segmentio/kafka-go/sasl/scram"
)

// The real clients of the brokers. They are the defaults of
// NewTargetFactory; the tests replace them with the generated mocks of
// the dialer interfaces, or run against in-process servers (NATS, Redis).

// kafkaDialer opens kafka-go writers.
type kafkaDialer struct{}

var _ KafkaDialer = kafkaDialer{}

// Dial implements KafkaDialer. The writer connects on its first write.
func (kafkaDialer) Dial(_ context.Context, cfg KafkaConfig) (KafkaProducer, error) {
	transport := &kafka.Transport{DialTimeout: defaultDialTimeout}
	if cfg.TLS.Enabled {
		transport.TLS = tlsConfig(cfg.TLS.SkipVerify)
	}
	if cfg.SASL.Mechanism != "" {
		var mech sasl.Mechanism
		switch cfg.SASL.Mechanism {
		case "plain":
			mech = plain.Mechanism{Username: cfg.SASL.Username, Password: cfg.SASL.Password}
		case "sha256", "sha512":
			algo := scram.SHA256
			if cfg.SASL.Mechanism == "sha512" {
				algo = scram.SHA512
			}
			m, err := scram.Mechanism(algo, cfg.SASL.Username, cfg.SASL.Password)
			if err != nil {
				return nil, fmt.Errorf("kafka SASL: %w", err)
			}
			mech = m
		default:
			return nil, fmt.Errorf("kafka SASL: unknown mechanism %q", cfg.SASL.Mechanism)
		}
		transport.SASL = mech
	}
	w := &kafka.Writer{
		Addr:                   kafka.TCP(cfg.Brokers...),
		Topic:                  cfg.Topic,
		Balancer:               &kafka.Hash{},
		RequiredAcks:           kafka.RequireAll,
		MaxAttempts:            1,
		BatchSize:              1,
		Transport:              transport,
		AllowAutoTopicCreation: true,
	}
	return &kafkaProducer{w: w}, nil
}

// kafkaProducer writes one message at a time.
type kafkaProducer struct {
	w *kafka.Writer
}

var _ KafkaProducer = (*kafkaProducer)(nil)

// WriteMessage implements KafkaProducer.
func (p *kafkaProducer) WriteMessage(ctx context.Context, key, value []byte) error {
	return p.w.WriteMessages(ctx, kafka.Message{Key: key, Value: value})
}

// Close implements KafkaProducer.
func (p *kafkaProducer) Close(context.Context) error { return p.w.Close() }

// natsDialer opens nats.go connections.
type natsDialer struct{}

var _ NATSDialer = natsDialer{}

// Dial implements NATSDialer.
func (natsDialer) Dial(_ context.Context, cfg NATSConfig) (NATSConn, error) {
	addr := cfg.Address
	if !strings.Contains(addr, "://") {
		addr = "nats://" + addr
	}
	opts := []nats.Option{nats.Name("nexo"), nats.Timeout(defaultDialTimeout), nats.MaxReconnects(0)}
	if cfg.Username != "" {
		opts = append(opts, nats.UserInfo(cfg.Username, cfg.Password))
	}
	if cfg.Token != "" {
		opts = append(opts, nats.Token(cfg.Token))
	}
	if cfg.TLS.Enabled || strings.HasPrefix(addr, "tls://") {
		opts = append(opts, nats.Secure(tlsConfig(cfg.TLS.SkipVerify)))
	}
	nc, err := nats.Connect(addr, opts...)
	if err != nil {
		return nil, err
	}
	return &natsConn{nc: nc}, nil
}

// natsConn publishes and flushes, so that a publication is known to have
// reached the server.
type natsConn struct {
	nc *nats.Conn
}

var _ NATSConn = (*natsConn)(nil)

// Publish implements NATSConn.
func (c *natsConn) Publish(ctx context.Context, subject string, data []byte) error {
	if err := c.nc.Publish(subject, data); err != nil {
		return err
	}
	if _, ok := ctx.Deadline(); !ok {
		return c.nc.FlushTimeout(defaultDialTimeout)
	}
	return c.nc.FlushWithContext(ctx)
}

// Close implements NATSConn.
func (c *natsConn) Close(context.Context) error {
	c.nc.Close()
	return nil
}

// amqpDialer opens amqp091-go channels in confirm mode.
type amqpDialer struct{}

var _ AMQPDialer = amqpDialer{}

// Dial implements AMQPDialer.
func (amqpDialer) Dial(_ context.Context, cfg AMQPConfig) (AMQPChannel, error) {
	conn, err := amqp.DialConfig(cfg.URL, amqp.Config{
		TLSClientConfig: tlsConfig(cfg.TLSSkipVerify),
		Dial:            amqp.DefaultDial(defaultDialTimeout),
	})
	if err != nil {
		return nil, err
	}
	ch, err := conn.Channel()
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if cfg.Exchange != "" {
		kind := cfg.ExchangeType
		if kind == "" {
			kind = "fanout"
		}
		if err := ch.ExchangeDeclare(cfg.Exchange, kind, cfg.Durable, false, false, false, nil); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("declare exchange %s: %w", cfg.Exchange, err)
		}
	}
	if err := ch.Confirm(false); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("confirm mode: %w", err)
	}
	return &amqpChannel{conn: conn, ch: ch}, nil
}

// amqpChannel publishes with publisher confirms.
type amqpChannel struct {
	conn *amqp.Connection
	ch   *amqp.Channel
}

var _ AMQPChannel = (*amqpChannel)(nil)

// Publish implements AMQPChannel.
func (c *amqpChannel) Publish(ctx context.Context, msg AMQPMessage) error {
	mode := amqp.Transient
	if msg.Persistent {
		mode = amqp.Persistent
	}
	dc, err := c.ch.PublishWithDeferredConfirmWithContext(ctx, msg.Exchange, msg.RoutingKey, msg.Mandatory, false, amqp.Publishing{
		ContentType: msg.ContentType, DeliveryMode: mode, Body: msg.Body,
	})
	if err != nil {
		return err
	}
	ok, err := dc.WaitContext(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("the broker refused the message (nack)")
	}
	return nil
}

// Close implements AMQPChannel.
func (c *amqpChannel) Close(context.Context) error {
	return errors.Join(c.ch.Close(), c.conn.Close())
}

// redisDialer opens go-redis clients.
type redisDialer struct{}

var _ RedisDialer = redisDialer{}

// Dial implements RedisDialer: the client is checked with a PING.
func (redisDialer) Dial(ctx context.Context, cfg RedisConfig) (RedisConn, error) {
	opts := &redis.Options{Addr: cfg.Address, Username: cfg.Username, Password: cfg.Password, DialTimeout: defaultDialTimeout}
	if cfg.TLS.Enabled {
		opts.TLSConfig = tlsConfig(cfg.TLS.SkipVerify)
	}
	c := redis.NewClient(opts)
	if err := c.Ping(ctx).Err(); err != nil {
		_ = c.Close()
		return nil, err
	}
	return &redisConn{c: c}, nil
}

// redisConn runs the commands of the Redis targets.
type redisConn struct {
	c *redis.Client
}

var _ RedisConn = (*redisConn)(nil)

// HSet implements RedisConn.
func (r *redisConn) HSet(ctx context.Context, key, field string, value []byte) error {
	return r.c.HSet(ctx, key, field, value).Err()
}

// HDel implements RedisConn.
func (r *redisConn) HDel(ctx context.Context, key, field string) error {
	return r.c.HDel(ctx, key, field).Err()
}

// RPush implements RedisConn.
func (r *redisConn) RPush(ctx context.Context, key string, value []byte) error {
	return r.c.RPush(ctx, key, value).Err()
}

// Close implements RedisConn.
func (r *redisConn) Close(context.Context) error { return r.c.Close() }

// postgresDialer opens pgx connections.
type postgresDialer struct{}

var _ PostgresDialer = postgresDialer{}

// Dial implements PostgresDialer.
func (postgresDialer) Dial(ctx context.Context, cfg PostgresConfig) (PostgresConn, error) {
	conn, err := pgx.Connect(ctx, cfg.ConnectionString)
	if err != nil {
		return nil, err
	}
	return &postgresConn{c: conn}, nil
}

// postgresConn executes the statements of a PostgreSQL target.
type postgresConn struct {
	c *pgx.Conn
}

var _ PostgresConn = (*postgresConn)(nil)

// Exec implements PostgresConn.
func (p *postgresConn) Exec(ctx context.Context, sql string, args []any) error {
	_, err := p.c.Exec(ctx, sql, args...)
	return err
}

// Close implements PostgresConn.
func (p *postgresConn) Close(ctx context.Context) error { return p.c.Close(ctx) }
