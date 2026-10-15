package audit_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/audit"
	auditmock "github.com/r3n3r0/nexo/internal/service/audit/mock"
	"github.com/r3n3r0/nexo/internal/service/config"
	"github.com/r3n3r0/nexo/internal/service/event"
	eventmock "github.com/r3n3r0/nexo/internal/service/event/mock"
)

var base = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func entry(minute int, typ, api, bucket, user string, status int) audit.Entry {
	e := audit.Entry{Time: base.Add(time.Duration(minute) * time.Minute), Type: typ, AccessKey: user, RequestID: api + bucket}
	e.API.Name, e.API.Bucket, e.API.StatusCode = api, bucket, status
	return e
}

func newSearchable(t *testing.T, peers ...audit.Peer) audit.Service {
	t.Helper()
	svc, err := audit.New(audit.Deps{Logger: slog.New(slog.DiscardHandler), NodeID: "n1", Peers: peers}, audit.WithMemory(4))
	require.NoError(t, err)
	ctx := context.Background()
	for _, e := range []audit.Entry{
		entry(0, "s3", "GetObject", "old", "alice", 200), // overwritten: the memory keeps 4
		entry(1, "s3", "PutObject", "photos", "alice", 200),
		entry(2, "s3", "GetObject", "photos", "bob", 404),
		entry(3, "admin", "GET /info", "", "root", 200),
		entry(4, "s3", "DeleteObject", "docs", "alice", 503),
	} {
		require.NoError(t, svc.Log(ctx, e))
	}
	return svc
}

func apis(entries []audit.Entry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.API.Name)
	}
	return out
}

func TestSearch(t *testing.T) {
	ctx := context.Background()
	svc := newSearchable(t)
	tests := []struct {
		name string
		q    audit.Query
		want []string
	}{
		{"newest first, memory bounded", audit.Query{}, []string{"DeleteObject", "GET /info", "GetObject", "PutObject"}},
		{"limit", audit.Query{Limit: 2}, []string{"DeleteObject", "GET /info"}},
		{"api case-insensitive", audit.Query{API: "getobject"}, []string{"GetObject"}},
		{"bucket", audit.Query{Bucket: "photos"}, []string{"GetObject", "PutObject"}},
		{"user", audit.Query{User: "alice"}, []string{"DeleteObject", "PutObject"}},
		{"type", audit.Query{Type: "admin"}, []string{"GET /info"}},
		{"status code", audit.Query{StatusCodes: []int{404}}, []string{"GetObject"}},
		{"status class", audit.Query{StatusClasses: []int{5}}, []string{"DeleteObject"}},
		{"errors", audit.Query{ErrorsOnly: true}, []string{"DeleteObject", "GetObject"}},
		{"since", audit.Query{Since: base.Add(3 * time.Minute)}, []string{"DeleteObject", "GET /info"}},
		{"until", audit.Query{Until: base.Add(2 * time.Minute)}, []string{"GetObject", "PutObject"}},
		{"request id", audit.Query{RequestID: "PutObjectphotos"}, []string{"PutObject"}},
		{"other node", audit.Query{Nodes: []string{"n9"}}, []string{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := svc.Search(ctx, tc.q)
			require.NoError(t, err)
			assert.Equal(t, tc.want, apis(got))
		})
	}
	for name, q := range map[string]audit.Query{
		"limit":  {Limit: 1001},
		"status": {StatusCodes: []int{42}},
		"class":  {StatusClasses: []int{9}},
		"range":  {Since: base, Until: base.Add(-time.Minute)},
	} {
		t.Run("invalid "+name, func(t *testing.T) {
			_, err := svc.Search(ctx, q)
			require.ErrorIs(t, err, domain.ErrInvalidArgument)
		})
	}
	got, err := svc.Search(ctx, audit.Query{Limit: 1})
	require.NoError(t, err)
	assert.Equal(t, "n1", got[0].Node, "the node is filled in")
	assert.Equal(t, audit.EntryVersion, got[0].Version)
}

func TestSearchPeers(t *testing.T) {
	ctx := context.Background()
	up := auditmock.NewPeer(t)
	up.EXPECT().NodeID().Return("n2")
	up.EXPECT().SearchAudit(mock.Anything, mock.MatchedBy(func(q audit.Query) bool {
		return q.Local && len(q.Nodes) == 0 && q.Bucket == "photos"
	})).Return([]audit.Entry{func() audit.Entry {
		e := entry(5, "s3", "PutObject", "photos", "carol", 200)
		e.Node = "n2"
		return e
	}()}, nil)
	down := auditmock.NewPeer(t)
	down.EXPECT().NodeID().Return("n3")
	down.EXPECT().SearchAudit(mock.Anything, mock.Anything).Return(nil, errors.New("unreachable"))
	svc := newSearchable(t, up, down)

	got, err := svc.Search(ctx, audit.Query{Bucket: "photos"})
	require.NoError(t, err)
	require.Len(t, got, 3, "an unreachable peer is left out")
	assert.Equal(t, "n2", got[0].Node, "merged newest first")
	assert.Equal(t, "carol", got[0].AccessKey)

	got, err = svc.Search(ctx, audit.Query{Bucket: "photos", Limit: 1})
	require.NoError(t, err)
	require.Len(t, got, 1, "the limit applies to the merge")

	// A local search asks no peer.
	got, err = svc.Search(ctx, audit.Query{Bucket: "photos", Local: true})
	require.NoError(t, err)
	assert.Len(t, got, 2)
	// The nodes select the peers asked.
	got, err = svc.Search(ctx, audit.Query{Bucket: "photos", Nodes: []string{"n2"}})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "n2", got[0].Node)
}

// webhook records the entries posted by an audit target.
type webhook struct {
	mu      sync.Mutex
	bodies  [][]byte
	headers []http.Header
	status  int
}

func (w *webhook) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.bodies = append(w.bodies, body)
	w.headers = append(w.headers, r.Header.Clone())
	rw.WriteHeader(w.status)
}

func (w *webhook) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.bodies)
}

// covers: S-OPS-06
func TestWebhookTarget(t *testing.T) {
	ctx := context.Background()
	hook := &webhook{status: http.StatusOK}
	srv := httptest.NewServer(hook)
	defer srv.Close()
	dyn := config.New(config.Deps{Store: config.NewMemoryStore(map[string]config.KV{
		audit.SubsystemWebhook + ":siem": {"enable": "on", "endpoint": srv.URL + "/audit", "auth_token": "s3cr3t"},
		audit.SubsystemWebhook + ":off":  {"enable": "off", "endpoint": srv.URL + "/off"},
		"notify_webhook:bucket":          {"enable": "on", "endpoint": srv.URL + "/notify"},
	})})
	logger := slog.New(slog.DiscardHandler)
	targets := event.NewDispatcher(event.Deps{
		Factory: audit.NewTargetFactory(audit.FactoryDeps{HTTP: srv.Client(), Logger: logger}, audit.WithUserAgent("nexo-test")),
		Logger:  logger,
	}, event.WithQueueRoot(t.TempDir()), event.WithRetryBackoff(10*time.Millisecond, 50*time.Millisecond))
	svc, err := audit.New(audit.Deps{Logger: logger, Clock: clock.NewFake(base), NodeID: "n1", DeploymentID: "dep", Targets: targets, Config: dyn})
	require.NoError(t, err)
	require.NoError(t, svc.Start(ctx))
	defer func() { require.NoError(t, svc.Stop(ctx)) }()

	e := entry(0, "s3", "PutObject", "photos", "alice", 200)
	e.ReqQuery = map[string]string{"versionId": "v1"}
	require.NoError(t, svc.Log(ctx, e))
	require.Eventually(t, func() bool { return hook.count() == 1 }, 5*time.Second, 10*time.Millisecond, "the entry reaches the audit webhook")
	hook.mu.Lock()
	body, header := hook.bodies[0], hook.headers[0]
	hook.mu.Unlock()
	var got audit.Entry
	require.NoError(t, json.Unmarshal(body, &got))
	assert.Equal(t, "PutObject", got.API.Name)
	assert.Equal(t, "n1", got.Node)
	assert.Equal(t, "dep", got.DeploymentID)
	assert.Equal(t, "alice", got.AccessKey)
	assert.Equal(t, "Bearer s3cr3t", header.Get("Authorization"))
	assert.Equal(t, "application/json", header.Get("Content-Type"))
	assert.Equal(t, "nexo-test", header.Get("User-Agent"))

	// A configuration change disables the target: no more delivery.
	require.NoError(t, dyn.Set(ctx, audit.SubsystemWebhook+":siem", config.KV{"enable": "off"}))
	require.Eventually(t, func() bool {
		_ = svc.Log(ctx, entry(1, "s3", "GetObject", "photos", "alice", 200))
		time.Sleep(20 * time.Millisecond)
		n := hook.count()
		_ = svc.Log(ctx, entry(2, "s3", "GetObject", "photos", "alice", 200))
		time.Sleep(50 * time.Millisecond)
		return hook.count() == n
	}, 5*time.Second, 10*time.Millisecond)
}

func TestWebhookTargetRetries(t *testing.T) {
	ctx := context.Background()
	hook := &webhook{status: http.StatusServiceUnavailable}
	srv := httptest.NewServer(hook)
	defer srv.Close()
	factory := audit.NewTargetFactory(audit.FactoryDeps{HTTP: srv.Client()})
	target, err := factory.New(ctx, event.TargetConfig{ID: "siem", Type: event.TypeWebhook, Enabled: true, Webhook: event.WebhookConfig{Endpoint: srv.URL}})
	require.NoError(t, err)
	ev := event.NewEvent(audit.EventName, "", base)
	ev.ResponseElements["x-nexo-audit-entry"] = `{"api":{"name":"PutObject"}}`
	require.Error(t, target.Send(ctx, ev), "a 503 keeps the entry queued")
	hook.mu.Lock()
	hook.status = http.StatusNoContent
	hook.mu.Unlock()
	require.NoError(t, target.Send(ctx, ev))
	require.Error(t, target.Send(ctx, event.NewEvent(audit.EventName, "", base)), "an event without entry is refused")

	_, err = factory.New(ctx, event.TargetConfig{ID: "x", Type: event.TypeAMQP})
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
}

// covers: S-OPS-06
func TestKafkaTarget(t *testing.T) {
	ctx := context.Background()
	dialer := eventmock.NewKafkaDialer(t)
	producer := eventmock.NewKafkaProducer(t)
	dialer.EXPECT().Dial(mock.Anything, mock.Anything).Return(producer, nil).Once()
	producer.EXPECT().WriteMessage(mock.Anything, []byte("n1/r1"), mock.Anything).Return(nil).Once()
	producer.EXPECT().WriteMessage(mock.Anything, mock.Anything, mock.Anything).Return(errors.New("broker down")).Once()
	producer.EXPECT().Close(mock.Anything).Return(nil)
	factory := audit.NewTargetFactory(audit.FactoryDeps{Kafka: dialer})
	target, err := factory.New(ctx, event.TargetConfig{ID: "k", Type: event.TypeKafka, Enabled: true,
		Kafka: event.KafkaConfig{Brokers: []string{"b:9092"}, Topic: "audit"}})
	require.NoError(t, err)
	ev := event.NewEvent(audit.EventName, "", base)
	ev.ResponseElements["x-nexo-audit-entry"] = `{"requestId":"r1","node":"n1"}`
	require.NoError(t, target.Send(ctx, ev))
	require.Error(t, target.Send(ctx, ev))
	require.NoError(t, target.Close(ctx))
}

func TestIsTargetSubsystem(t *testing.T) {
	for sub, want := range map[string]bool{
		"audit_webhook": true, "audit_webhook:siem": true, "audit_kafka": true, "audit_kafka:k1": true,
		"audit_amqp": false, "notify_webhook": false, "audit": false,
	} {
		assert.Equal(t, want, audit.IsTargetSubsystem(sub), sub)
	}
}
