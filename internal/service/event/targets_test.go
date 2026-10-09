package event_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/service/event"
	eventmock "github.com/r3n3r0/nexo/internal/service/event/mock"
)

// startNATS runs an embedded NATS server on a free port.
func startNATS(t *testing.T) *natsserver.Server {
	t.Helper()
	ns, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
	require.NoError(t, err)
	go ns.Start()
	require.True(t, ns.ReadyForConnections(10*time.Second), "nats server not ready")
	t.Cleanup(ns.Shutdown)
	return ns
}

// TestNATSTarget publishes through an embedded NATS server and survives a
// server restart (the target reconnects).
func TestNATSTarget(t *testing.T) {
	ns := startNATS(t)
	ctx := context.Background()
	sub, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer sub.Close()
	msgs := make(chan *nats.Msg, 4)
	s, err := sub.ChanSubscribe("nexo.events", msgs)
	require.NoError(t, err)
	defer func() { _ = s.Unsubscribe() }()
	require.NoError(t, sub.Flush())

	addr := strings.TrimPrefix(ns.ClientURL(), "nats://")
	target, err := event.NewTargetFactory(event.FactoryDeps{}).New(ctx, event.TargetConfig{
		ID: "n", Type: event.TypeNATS, Enabled: true, NATS: event.NATSConfig{Address: addr, Subject: "nexo.events"},
	})
	require.NoError(t, err)
	defer func() { _ = target.Close(ctx) }()
	require.NoError(t, target.Send(ctx, objectEvent(event.ObjectCreatedPut, "b", "a.txt")))
	select {
	case m := <-msgs:
		var p event.Payload
		require.NoError(t, json.Unmarshal(m.Data, &p))
		assert.Equal(t, "b/a.txt", p.Key)
		assert.Equal(t, event.ObjectCreatedPut, p.EventName)
	case <-time.After(5 * time.Second):
		t.Fatal("no NATS message")
	}

	bad, err := event.NewTargetFactory(event.FactoryDeps{}).New(ctx, event.TargetConfig{
		ID: "down", Type: event.TypeNATS, NATS: event.NATSConfig{Address: "127.0.0.1:1", Subject: "x"},
	})
	require.NoError(t, err, "targets connect lazily")
	assert.Error(t, bad.Send(ctx, objectEvent(event.ObjectCreatedPut, "b", "a")), "unreachable server")
}

// TestRedisTarget checks both formats against an in-process Redis.
func TestRedisTarget(t *testing.T) {
	mr := miniredis.RunT(t)
	ctx := context.Background()
	f := event.NewTargetFactory(event.FactoryDeps{})

	ns, err := f.New(ctx, event.TargetConfig{ID: "ns", Type: event.TypeRedis, Redis: event.RedisConfig{Address: mr.Addr(), Key: "bucketevents", Format: event.FormatNamespace}})
	require.NoError(t, err)
	defer func() { _ = ns.Close(ctx) }()
	require.NoError(t, ns.Send(ctx, objectEvent(event.ObjectCreatedPut, "b", "dir/a b.txt")))
	require.NoError(t, ns.Send(ctx, objectEvent(event.ObjectCreatedPut, "b", "c.txt")))
	val := mr.HGet("bucketevents", "b/dir/a b.txt")
	require.NotEmpty(t, val, "namespace entry keyed by bucket/key")
	var rec struct{ Records []event.Event }
	require.NoError(t, json.Unmarshal([]byte(val), &rec))
	require.Len(t, rec.Records, 1)
	assert.Equal(t, "dir/a b.txt", rec.Records[0].S3.Object.Key, "the target stores the record it is given")
	require.NoError(t, ns.Send(ctx, objectEvent(event.ObjectRemovedDelete, "b", "dir/a b.txt")))
	assert.Empty(t, mr.HGet("bucketevents", "b/dir/a b.txt"), "a removal deletes the entry")
	require.NoError(t, ns.Send(ctx, objectEvent(event.LifecycleExpirationDelete, "b", "c.txt")))
	assert.Empty(t, mr.HGet("bucketevents", "b/c.txt"))

	access, err := f.New(ctx, event.TargetConfig{ID: "acc", Type: event.TypeRedis, Redis: event.RedisConfig{Address: mr.Addr(), Key: "log", Format: event.FormatAccess}})
	require.NoError(t, err)
	defer func() { _ = access.Close(ctx) }()
	require.NoError(t, access.Send(ctx, objectEvent(event.ObjectCreatedPut, "b", "x")))
	require.NoError(t, access.Send(ctx, objectEvent(event.ObjectRemovedDelete, "b", "x")))
	list, err := mr.List("log")
	require.NoError(t, err)
	require.Len(t, list, 2, "every event is appended")
	var entries []struct {
		Event     []event.Event
		EventTime string
	}
	require.NoError(t, json.Unmarshal([]byte(list[1]), &entries))
	require.Len(t, entries, 1)
	assert.Equal(t, event.ObjectRemovedDelete, entries[0].Event[0].EventName)
	assert.Equal(t, "2026-01-02T03:04:05.000Z", entries[0].EventTime)

	mr.Close()
	assert.Error(t, access.Send(ctx, objectEvent(event.ObjectCreatedPut, "b", "y")), "redis down")
}

// TestKafkaTarget: the payload is written keyed by bucket/key; a failed
// write drops the connection, the next send dials again.
func TestKafkaTarget(t *testing.T) {
	ctx := context.Background()
	type msg struct{ key, value string }
	var mu sync.Mutex
	var written []msg
	fail := true
	dials := 0
	dialer := eventmock.NewKafkaDialer(t)
	dialer.EXPECT().Dial(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, cfg event.KafkaConfig) (event.KafkaProducer, error) {
		assert.Equal(t, []string{"b1:9092"}, cfg.Brokers)
		assert.Equal(t, "events", cfg.Topic)
		dials++
		p := eventmock.NewKafkaProducer(t)
		p.EXPECT().WriteMessage(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, key, value []byte) error {
			mu.Lock()
			defer mu.Unlock()
			if fail {
				fail = false
				return errors.New("broker not available")
			}
			written = append(written, msg{string(key), string(value)})
			return nil
		})
		p.EXPECT().Close(mock.Anything).Return(nil)
		return p, nil
	})
	target, err := event.NewTargetFactory(event.FactoryDeps{Kafka: dialer}).New(ctx, event.TargetConfig{
		ID: "k", Type: event.TypeKafka, Kafka: event.KafkaConfig{Brokers: []string{"b1:9092"}, Topic: "events"},
	})
	require.NoError(t, err)
	assert.Error(t, target.Send(ctx, objectEvent(event.ObjectCreatedPut, "b", "k1")))
	require.NoError(t, target.Send(ctx, objectEvent(event.ObjectCreatedPut, "b", "k1")))
	require.NoError(t, target.Close(ctx))
	assert.Equal(t, 2, dials, "reconnected after the failure")
	require.Len(t, written, 1)
	assert.Equal(t, "b/k1", written[0].key)
	var p event.Payload
	require.NoError(t, json.Unmarshal([]byte(written[0].value), &p))
	assert.Equal(t, event.ObjectCreatedPut, p.EventName)
}

func TestAMQPTarget(t *testing.T) {
	ctx := context.Background()
	var published []event.AMQPMessage
	ch := eventmock.NewAMQPChannel(t)
	ch.EXPECT().Publish(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, m event.AMQPMessage) error {
		published = append(published, m)
		return nil
	})
	ch.EXPECT().Close(mock.Anything).Return(nil)
	dialer := eventmock.NewAMQPDialer(t)
	dialer.EXPECT().Dial(mock.Anything, mock.Anything).Return(ch, nil).Once()
	target, err := event.NewTargetFactory(event.FactoryDeps{AMQP: dialer}).New(ctx, event.TargetConfig{
		ID: "a", Type: event.TypeAMQP, AMQP: event.AMQPConfig{URL: "amqp://guest:guest@mq:5672/", Exchange: "ex", RoutingKey: "rk", Persistent: true},
	})
	require.NoError(t, err)
	require.NoError(t, target.Send(ctx, objectEvent(event.ObjectCreatedPut, "b", "x")))
	require.NoError(t, target.Send(ctx, objectEvent(event.ObjectCreatedPut, "b", "y")))
	require.NoError(t, target.Close(ctx))
	require.Len(t, published, 2, "one connection for both")
	assert.Equal(t, "ex", published[0].Exchange)
	assert.Equal(t, "rk", published[0].RoutingKey)
	assert.True(t, published[0].Persistent)
	assert.Equal(t, "application/json", published[0].ContentType)
	assert.Contains(t, string(published[1].Body), `"Key":"b/y"`)

	refused := eventmock.NewAMQPDialer(t)
	refused.EXPECT().Dial(mock.Anything, mock.Anything).Return(nil, errors.New("connection refused"))
	down, err := event.NewTargetFactory(event.FactoryDeps{AMQP: refused}).New(ctx, event.TargetConfig{
		ID: "a", Type: event.TypeAMQP, AMQP: event.AMQPConfig{URL: "amqp://mq/"},
	})
	require.NoError(t, err)
	assert.ErrorContains(t, down.Send(ctx, objectEvent(event.ObjectCreatedPut, "b", "x")), "connection refused")
}

// fakePostgres is a PostgreSQL connection backed by the generated mock:
// it records the statements and keeps the rows of the namespace format.
type fakePostgres struct {
	mu    sync.Mutex
	stmts []string
	rows  map[string]string
}

func (f *fakePostgres) conn(t *testing.T) *eventmock.PostgresConn {
	c := eventmock.NewPostgresConn(t)
	c.EXPECT().Exec(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, sql string, args []any) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.stmts = append(f.stmts, sql)
		switch {
		case strings.HasPrefix(sql, "INSERT INTO nexo_events (key"):
			f.rows[args[0].(string)] = args[1].(string)
		case strings.HasPrefix(sql, "DELETE FROM nexo_events"):
			delete(f.rows, args[0].(string))
		}
		return nil
	})
	c.EXPECT().Close(mock.Anything).Return(nil).Maybe()
	return c
}

func TestPostgresTarget(t *testing.T) {
	ctx := context.Background()
	fake := &fakePostgres{rows: map[string]string{}}
	dialer := eventmock.NewPostgresDialer(t)
	dialer.EXPECT().Dial(mock.Anything, mock.Anything).RunAndReturn(func(context.Context, event.PostgresConfig) (event.PostgresConn, error) {
		return fake.conn(t), nil
	})
	f := event.NewTargetFactory(event.FactoryDeps{Postgres: dialer})

	ns, err := f.New(ctx, event.TargetConfig{ID: "pg", Type: event.TypePostgres, Postgres: event.PostgresConfig{
		ConnectionString: "postgres://db/nexo", Table: "nexo_events", Format: event.FormatNamespace}})
	require.NoError(t, err)
	require.NoError(t, ns.Send(ctx, objectEvent(event.ObjectCreatedPut, "b", "a")))
	require.NoError(t, ns.Send(ctx, objectEvent(event.ObjectCreatedPut, "b", "c")))
	require.NoError(t, ns.Send(ctx, objectEvent(event.ObjectRemovedDelete, "b", "a")))
	require.NoError(t, ns.Close(ctx))
	require.NotEmpty(t, fake.stmts)
	assert.Equal(t, "CREATE TABLE IF NOT EXISTS nexo_events (key VARCHAR PRIMARY KEY, value JSONB);", fake.stmts[0], "table created at connection")
	assert.Equal(t, []string{"b/c"}, keysOf(fake.rows))
	assert.Contains(t, fake.rows["b/c"], `"Records"`)

	access, err := f.New(ctx, event.TargetConfig{ID: "log", Type: event.TypePostgres, Postgres: event.PostgresConfig{
		ConnectionString: "postgres://db/nexo", Table: "nexo_log", Format: event.FormatAccess}})
	require.NoError(t, err)
	fake.stmts = nil
	require.NoError(t, access.Send(ctx, objectEvent(event.ObjectRemovedDelete, "b", "a")))
	assert.Equal(t, []string{
		"CREATE TABLE IF NOT EXISTS nexo_log (event_time TIMESTAMP WITH TIME ZONE NOT NULL, event_data JSONB);",
		"INSERT INTO nexo_log (event_time, event_data) VALUES ($1, $2);",
	}, fake.stmts, "the access format appends every event")

	failing := eventmock.NewPostgresDialer(t)
	broken := eventmock.NewPostgresConn(t)
	broken.EXPECT().Exec(mock.Anything, mock.Anything, mock.Anything).Return(errors.New("permission denied"))
	broken.EXPECT().Close(mock.Anything).Return(nil)
	failing.EXPECT().Dial(mock.Anything, mock.Anything).Return(broken, nil)
	tg, err := event.NewTargetFactory(event.FactoryDeps{Postgres: failing}).New(ctx, event.TargetConfig{ID: "pg", Type: event.TypePostgres,
		Postgres: event.PostgresConfig{ConnectionString: "c", Table: "t", Format: event.FormatAccess}})
	require.NoError(t, err)
	assert.ErrorContains(t, tg.Send(ctx, objectEvent(event.ObjectCreatedPut, "b", "a")), "create table t")
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestDispatcherDeliversToNATS runs the whole chain (bucket configuration,
// queue, NATS target) against the embedded server.
func TestDispatcherDeliversToNATS(t *testing.T) {
	ns := startNATS(t)
	ctx := context.Background()
	sub, err := nats.Connect(ns.ClientURL())
	require.NoError(t, err)
	defer sub.Close()
	msgs := make(chan *nats.Msg, 4)
	s, err := sub.ChanSubscribe("bucket.events", msgs)
	require.NoError(t, err)
	defer func() { _ = s.Unsubscribe() }()
	require.NoError(t, sub.Flush())

	rules := bucketRules(t, map[string]string{"b": `<NotificationConfiguration><QueueConfiguration><Id>n</Id><Queue>arn:nexo:sqs::nats1:nats</Queue><Event>s3:ObjectRemoved:*</Event></QueueConfiguration></NotificationConfiguration>`})
	d := event.NewDispatcher(event.Deps{Rules: rules})
	require.NoError(t, d.SetTarget(ctx, event.TargetConfig{ID: "nats1", Type: event.TypeNATS, Enabled: true,
		NATS: event.NATSConfig{Address: ns.ClientURL(), Subject: "bucket.events"}}))
	require.NoError(t, d.Start(ctx))
	defer stop(t, d)
	require.NoError(t, d.Publish(ctx, objectEvent(event.ObjectRemovedDeleteMarkerCreated, "b", "gone")))
	select {
	case m := <-msgs:
		var p event.Payload
		require.NoError(t, json.Unmarshal(m.Data, &p))
		assert.Equal(t, event.ObjectRemovedDeleteMarkerCreated, p.EventName)
		assert.Equal(t, "n", p.Records[0].S3.ConfigurationID)
	case <-time.After(5 * time.Second):
		t.Fatal("no NATS message")
	}
}
