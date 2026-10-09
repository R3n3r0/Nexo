package event

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// records is the document stored by the namespace formats: the records of
// the last event of an object.
type records struct {
	Records []Event `json:"Records"`
}

// accessEntry is an entry of the Redis access format.
type accessEntry struct {
	Event     []Event `json:"Event"`
	EventTime string  `json:"EventTime"`
}

// kafkaTarget writes the payload of every event to a Kafka topic, keyed
// by "<bucket>/<key>".
type kafkaTarget struct {
	id   TargetID
	conn *lazy[KafkaProducer]
}

var _ Target = (*kafkaTarget)(nil)

// ID implements Target.
func (t *kafkaTarget) ID() TargetID { return t.id }

// Send implements Target.
func (t *kafkaTarget) Send(ctx context.Context, e Event) error {
	p := e.payload()
	value, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("kafka %s: encode: %w", t.id, err)
	}
	if err := t.conn.do(ctx, func(c KafkaProducer) error { return c.WriteMessage(ctx, []byte(p.Key), value) }); err != nil {
		return fmt.Errorf("kafka %s: %w", t.id, err)
	}
	return nil
}

// Close implements Target.
func (t *kafkaTarget) Close(ctx context.Context) error { return t.conn.close(ctx) }

// natsTarget publishes the payload of every event on a NATS subject.
type natsTarget struct {
	id      TargetID
	subject string
	conn    *lazy[NATSConn]
}

var _ Target = (*natsTarget)(nil)

// ID implements Target.
func (t *natsTarget) ID() TargetID { return t.id }

// Send implements Target.
func (t *natsTarget) Send(ctx context.Context, e Event) error {
	data, err := json.Marshal(e.payload())
	if err != nil {
		return fmt.Errorf("nats %s: encode: %w", t.id, err)
	}
	if err := t.conn.do(ctx, func(c NATSConn) error { return c.Publish(ctx, t.subject, data) }); err != nil {
		return fmt.Errorf("nats %s: %w", t.id, err)
	}
	return nil
}

// Close implements Target.
func (t *natsTarget) Close(ctx context.Context) error { return t.conn.close(ctx) }

// amqpTarget publishes the payload of every event to an AMQP exchange.
type amqpTarget struct {
	id   TargetID
	cfg  AMQPConfig
	conn *lazy[AMQPChannel]
}

var _ Target = (*amqpTarget)(nil)

// ID implements Target.
func (t *amqpTarget) ID() TargetID { return t.id }

// Send implements Target.
func (t *amqpTarget) Send(ctx context.Context, e Event) error {
	body, err := json.Marshal(e.payload())
	if err != nil {
		return fmt.Errorf("amqp %s: encode: %w", t.id, err)
	}
	msg := AMQPMessage{
		Exchange: t.cfg.Exchange, RoutingKey: t.cfg.RoutingKey, Mandatory: t.cfg.Mandatory, Persistent: t.cfg.Persistent,
		ContentType: "application/json", Body: body,
	}
	if err := t.conn.do(ctx, func(c AMQPChannel) error { return c.Publish(ctx, msg) }); err != nil {
		return fmt.Errorf("amqp %s: %w", t.id, err)
	}
	return nil
}

// Close implements Target.
func (t *amqpTarget) Close(ctx context.Context) error { return t.conn.close(ctx) }

// redisTarget stores the events in Redis: a hash field per object
// (namespace) or a list of every event (access).
type redisTarget struct {
	id   TargetID
	cfg  RedisConfig
	conn *lazy[RedisConn]
}

var _ Target = (*redisTarget)(nil)

// ID implements Target.
func (t *redisTarget) ID() TargetID { return t.id }

// Send implements Target.
func (t *redisTarget) Send(ctx context.Context, e Event) error {
	field := e.payload().Key
	var op func(c RedisConn) error
	switch {
	case t.cfg.Format == FormatAccess:
		data, err := json.Marshal([]accessEntry{{Event: []Event{e}, EventTime: e.EventTime}})
		if err != nil {
			return fmt.Errorf("redis %s: encode: %w", t.id, err)
		}
		op = func(c RedisConn) error { return c.RPush(ctx, t.cfg.Key, data) }
	case e.EventName.isRemoval(), e.EventName == TestEvent:
		// A test event checks the connection without adding an entry to
		// the namespace: it removes the field of no object ("/").
		op = func(c RedisConn) error { return c.HDel(ctx, t.cfg.Key, field) }
	default:
		data, err := json.Marshal(records{Records: []Event{e}})
		if err != nil {
			return fmt.Errorf("redis %s: encode: %w", t.id, err)
		}
		op = func(c RedisConn) error { return c.HSet(ctx, t.cfg.Key, field, data) }
	}
	if err := t.conn.do(ctx, op); err != nil {
		return fmt.Errorf("redis %s: %w", t.id, err)
	}
	return nil
}

// Close implements Target.
func (t *redisTarget) Close(ctx context.Context) error { return t.conn.close(ctx) }

// postgresTarget stores the events in a PostgreSQL table, created when
// missing: a row per object (namespace: key, value) or a row per event
// (access: event_time, event_data).
type postgresTarget struct {
	id   TargetID
	cfg  PostgresConfig
	conn *lazy[PostgresConn]
}

var _ Target = (*postgresTarget)(nil)

// SQL statements of the PostgreSQL target; the table name is validated
// (validTableName) before it is written in them.
const (
	pgCreateNamespace = "CREATE TABLE IF NOT EXISTS %s (key VARCHAR PRIMARY KEY, value JSONB);"
	pgUpsertNamespace = "INSERT INTO %s (key, value) VALUES ($1, $2) ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value;"
	pgDeleteNamespace = "DELETE FROM %s WHERE key = $1;"
	pgCreateAccess    = "CREATE TABLE IF NOT EXISTS %s (event_time TIMESTAMP WITH TIME ZONE NOT NULL, event_data JSONB);"
	pgInsertAccess    = "INSERT INTO %s (event_time, event_data) VALUES ($1, $2);"
)

// ID implements Target.
func (t *postgresTarget) ID() TargetID { return t.id }

// dial connects and creates the table when missing.
func (t *postgresTarget) dial(ctx context.Context, d PostgresDialer) (PostgresConn, error) {
	conn, err := d.Dial(ctx, t.cfg)
	if err != nil {
		return nil, err
	}
	create := pgCreateNamespace
	if t.cfg.Format == FormatAccess {
		create = pgCreateAccess
	}
	if err := conn.Exec(ctx, fmt.Sprintf(create, t.cfg.Table), nil); err != nil {
		_ = conn.Close(ctx)
		return nil, fmt.Errorf("create table %s: %w", t.cfg.Table, err)
	}
	return conn, nil
}

// Send implements Target.
func (t *postgresTarget) Send(ctx context.Context, e Event) error {
	data, err := json.Marshal(records{Records: []Event{e}})
	if err != nil {
		return fmt.Errorf("postgresql %s: encode: %w", t.id, err)
	}
	var sql string
	var args []any
	switch {
	case t.cfg.Format == FormatAccess:
		at, perr := time.Parse(eventTimeLayout, e.EventTime)
		if perr != nil {
			at = time.Now().UTC()
		}
		sql, args = fmt.Sprintf(pgInsertAccess, t.cfg.Table), []any{at, string(data)}
	case e.EventName.isRemoval(), e.EventName == TestEvent:
		// A test event checks the connection without adding a row to the
		// namespace: it deletes the row of no object ("/").
		sql, args = fmt.Sprintf(pgDeleteNamespace, t.cfg.Table), []any{e.payload().Key}
	default:
		sql, args = fmt.Sprintf(pgUpsertNamespace, t.cfg.Table), []any{e.payload().Key, string(data)}
	}
	if err := t.conn.do(ctx, func(c PostgresConn) error { return c.Exec(ctx, sql, args) }); err != nil {
		return fmt.Errorf("postgresql %s: %w", t.id, err)
	}
	return nil
}

// Close implements Target.
func (t *postgresTarget) Close(ctx context.Context) error { return t.conn.close(ctx) }
