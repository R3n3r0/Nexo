package event_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/config"
	configmock "github.com/r3n3r0/nexo/internal/service/config/mock"
	"github.com/r3n3r0/nexo/internal/service/event"
	eventmock "github.com/r3n3r0/nexo/internal/service/event/mock"
)

// hook is a test webhook endpoint that can be taken offline (503).
type hook struct {
	srv     *httptest.Server
	offline atomic.Bool
	mu      sync.Mutex
	got     []event.Payload
	auth    []string
	arrived chan struct{}
}

func newHook(t *testing.T) *hook {
	t.Helper()
	h := &hook{arrived: make(chan struct{}, 100)}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.offline.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var p event.Payload
		if err := json.Unmarshal(body, &p); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		h.mu.Lock()
		h.got = append(h.got, p)
		h.auth = append(h.auth, r.Header.Get("Authorization"))
		h.mu.Unlock()
		h.arrived <- struct{}{}
	}))
	t.Cleanup(h.srv.Close)
	return h
}

// keys returns the "<bucket>/<key>" of the received payloads, in order.
func (h *hook) keys() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.got))
	for _, p := range h.got {
		out = append(out, p.Key)
	}
	return out
}

// waitFor waits until n payloads arrived.
func (h *hook) waitFor(t *testing.T, n int) {
	t.Helper()
	require.Eventually(t, func() bool { return len(h.keys()) >= n }, 10*time.Second, 5*time.Millisecond, "received %v", h.keys())
}

func (h *hook) payloads() []event.Payload {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]event.Payload(nil), h.got...)
}

func fastRetry() event.Option { return event.WithRetryBackoff(5*time.Millisecond, 20*time.Millisecond) }

func TestDispatcherLifecycle(t *testing.T) {
	ctx := context.Background()
	d := event.NewDispatcher(event.Deps{})
	assert.ErrorIs(t, d.Publish(ctx, event.Event{}), domain.ErrConflict, "not started")
	require.NoError(t, d.Start(ctx))
	assert.Error(t, d.Start(ctx), "double start")
	stop(t, d)
	assert.ErrorIs(t, d.Publish(ctx, event.Event{}), domain.ErrConflict, "stopped")
	stop(t, d)
	require.NoError(t, d.Start(ctx), "a stopped dispatcher starts again")
	stop(t, d)
}

func TestDispatcherDeliversByTargetFilter(t *testing.T) {
	h := newHook(t)
	ctx := context.Background()
	d := event.NewDispatcher(event.Deps{Factory: event.NewWebhookFactory(event.WebhookDeps{Client: h.srv.Client()})}, event.WithQueueSize(8))
	hookCfg := webhookTarget("hook", h.srv.URL)
	hookCfg.Webhook.AuthToken = "secret"
	hookCfg.Filter = event.Filter{Events: []event.Name{event.ObjectCreatedAll}, Bucket: "photos"}
	require.NoError(t, d.SetTarget(ctx, hookCfg))
	disabled := webhookTarget("disabled", h.srv.URL)
	disabled.Enabled = false
	disabled.Filter = event.Filter{Bucket: "photos"}
	require.NoError(t, d.SetTarget(ctx, disabled))
	require.NoError(t, d.SetTarget(ctx, webhookTarget("nofilter", h.srv.URL)), "the zero filter routes nothing")
	require.NoError(t, d.Start(ctx))

	e := objectEvent(event.ObjectCreatedPut, "photos", "a b.jpg")
	e.S3.Object.Size = 3
	require.NoError(t, d.Publish(ctx, e))
	require.NoError(t, d.Publish(ctx, objectEvent(event.ObjectRemovedDelete, "photos", "x")), "filtered by event")
	require.NoError(t, d.Publish(ctx, objectEvent(event.ObjectCreatedPut, "other", "x")), "filtered by bucket")
	// An internal event (for the hooks of the object layer) is never
	// delivered, whatever the filters.
	require.NoError(t, d.Publish(ctx, objectEvent(event.ObjectMetadataChanged, "photos", "a b.jpg")))

	h.waitFor(t, 1)
	stop(t, d)
	got := h.payloads()
	require.Len(t, got, 1)
	assert.Equal(t, event.ObjectCreatedPut, got[0].EventName)
	assert.Equal(t, "photos/a b.jpg", got[0].Key)
	require.Len(t, got[0].Records, 1)
	assert.Equal(t, "a+b.jpg", got[0].Records[0].S3.Object.Key, "the record key is URL-encoded")
	assert.Equal(t, int64(3), got[0].Records[0].S3.Object.Size)
	assert.Equal(t, event.Version, got[0].Records[0].EventVersion)
	assert.Equal(t, event.ConfigID, got[0].Records[0].S3.ConfigurationID)
	assert.Equal(t, []string{"Bearer secret"}, h.auth)
}

func TestDispatcherRoutesByBucketConfiguration(t *testing.T) {
	ctx := context.Background()
	got1 := make(chan event.Event, 10)
	got2 := make(chan event.Event, 10)
	t1 := newCaptureTarget(t, "1", got1)
	t2 := newCaptureTarget(t, "2", got2)
	rules := bucketRules(t, map[string]string{"photos": `<NotificationConfiguration>
		<QueueConfiguration><Id>jpg</Id><Queue>arn:nexo:sqs:us-east-1:1:webhook</Queue><Event>s3:ObjectCreated:*</Event>
			<Filter><S3Key><FilterRule><Name>prefix</Name><Value>img/</Value></FilterRule><FilterRule><Name>suffix</Name><Value>.jpg</Value></FilterRule></S3Key></Filter></QueueConfiguration>
		<QueueConfiguration><Id>also-1</Id><Queue>arn:nexo:sqs:us-east-1:1:webhook</Queue><Event>s3:ObjectCreated:Put</Event></QueueConfiguration>
		<TopicConfiguration><Id>removed</Id><Topic>arn:nexo:sqs:us-east-1:2:webhook</Topic><Event>s3:ObjectRemoved:*</Event><Event>s3:LifecycleExpiration:*</Event></TopicConfiguration>
		<QueueConfiguration><Id>missing</Id><Queue>arn:nexo:sqs:us-east-1:3:webhook</Queue><Event>s3:ObjectRemoved:*</Event></QueueConfiguration>
	</NotificationConfiguration>`, "broken": `<NotificationConfiguration><QueueConfiguration>`})
	d := event.NewDispatcher(event.Deps{Rules: rules, Factory: factoryOf(t, t1, t2)})
	require.NoError(t, d.SetTarget(ctx, webhookTarget("1", "http://unused")))
	require.NoError(t, d.SetTarget(ctx, webhookTarget("2", "http://unused")))
	require.NoError(t, d.Start(ctx))
	defer stop(t, d)

	require.NoError(t, d.Publish(ctx, objectEvent(event.ObjectCreatedPut, "photos", "img/a.jpg")))
	rec := receive(t, got1)
	assert.Equal(t, "jpg", rec.S3.ConfigurationID, "first matching rule, delivered once")
	require.NoError(t, d.Publish(ctx, objectEvent(event.ObjectCreatedCopy, "photos", "img/a.png")))
	require.NoError(t, d.Publish(ctx, objectEvent(event.ObjectCreatedPut, "photos", "doc/b.txt")))
	rec = receive(t, got1)
	assert.Equal(t, "doc/b.txt", rec.S3.Object.Key)
	assert.Equal(t, "also-1", rec.S3.ConfigurationID)
	require.NoError(t, d.Publish(ctx, objectEvent(event.ObjectRemovedDelete, "photos", "img/a.jpg")))
	rec = receive(t, got2)
	assert.Equal(t, event.ObjectRemovedDelete, rec.EventName)
	assert.Equal(t, "removed", rec.S3.ConfigurationID)
	require.NoError(t, d.Publish(event.WithRequest(ctx, event.Request{Lifecycle: true}), objectEvent(event.ObjectRemovedDelete, "photos", "old")))
	rec = receive(t, got2)
	assert.Equal(t, event.LifecycleExpirationDelete, rec.EventName)
	require.NoError(t, d.Publish(ctx, objectEvent(event.ObjectCreatedPut, "broken", "x")), "an invalid configuration routes nothing")
	require.NoError(t, d.Publish(ctx, objectEvent(event.ObjectCreatedPut, "none", "x")))
	nothing(t, got1, 100*time.Millisecond)
	nothing(t, got2, 10*time.Millisecond)
}

// TestDispatcherQueuesWhileTargetOffline: the events of an offline target
// stay queued and are delivered, in order, when it comes back (docs/06
// TS-I-15).
func TestDispatcherQueuesWhileTargetOffline(t *testing.T) {
	h := newHook(t)
	h.offline.Store(true)
	ctx := context.Background()
	cfg := webhookTarget("hook", h.srv.URL)
	cfg.Filter = event.Filter{Bucket: "b"}
	cfg.QueueDir = "hooks/primary"
	root := t.TempDir()
	queue := filepath.Join(root, "queue_dir", "hooks", "primary")
	d := event.NewDispatcher(event.Deps{Factory: event.NewWebhookFactory(event.WebhookDeps{Client: h.srv.Client()})}, fastRetry(),
		event.WithQueueRoot(root))
	require.NoError(t, d.SetTarget(ctx, cfg))
	require.NoError(t, d.Start(ctx))
	defer stop(t, d)
	for _, k := range []string{"1", "2", "3"} {
		require.NoError(t, d.Publish(ctx, objectEvent(event.ObjectCreatedPut, "b", k)))
	}
	time.Sleep(100 * time.Millisecond)
	assert.Empty(t, h.keys(), "offline")
	files, err := filepath.Glob(filepath.Join(queue, "*.event"))
	require.NoError(t, err)
	assert.Len(t, files, 3, "the queue_dir is a directory of the queue root")
	h.offline.Store(false)
	h.waitFor(t, 3)
	assert.Equal(t, []string{"b/1", "b/2", "b/3"}, h.keys(), "order preserved")
	// The endpoint has the last event before the sender removes it from
	// the queue.
	require.Eventually(t, func() bool {
		files, err := filepath.Glob(filepath.Join(queue, "*.event"))
		return err == nil && len(files) == 0
	}, 5*time.Second, 5*time.Millisecond, "delivered events leave the queue")
}

// TestDispatcherQueueSurvivesRestart: the events queued for an offline
// target are delivered by the next process once the target is back.
func TestDispatcherQueueSurvivesRestart(t *testing.T) {
	h := newHook(t)
	h.offline.Store(true)
	ctx := context.Background()
	root := t.TempDir()
	cfg := webhookTarget("hook", h.srv.URL)
	cfg.Filter = event.Filter{Bucket: "b"}
	newDispatcher := func() event.Dispatcher {
		d := event.NewDispatcher(event.Deps{Factory: event.NewWebhookFactory(event.WebhookDeps{Client: h.srv.Client()})},
			fastRetry(), event.WithQueueRoot(root))
		require.NoError(t, d.SetTarget(ctx, cfg))
		require.NoError(t, d.Start(ctx))
		return d
	}
	first := newDispatcher()
	for _, k := range []string{"a", "b", "c"} {
		require.NoError(t, first.Publish(ctx, objectEvent(event.ObjectCreatedPut, "b", k)))
	}
	require.Eventually(t, func() bool {
		files, _ := filepath.Glob(filepath.Join(root, "webhook-hook", "*.event"))
		return len(files) == 3
	}, 5*time.Second, 5*time.Millisecond, "queued on disk")
	stop(t, first)

	h.offline.Store(false)
	second := newDispatcher()
	defer stop(t, second)
	require.NoError(t, second.Publish(ctx, objectEvent(event.ObjectCreatedPut, "b", "d")))
	h.waitFor(t, 4)
	// The delivery is at least once: an attempt the stop cut short may
	// still have reached the endpoint, and is sent again.
	var delivered []string
	seen := map[string]bool{}
	for _, k := range h.keys() {
		if !seen[k] {
			seen[k] = true
			delivered = append(delivered, k)
		}
	}
	assert.Equal(t, []string{"b/a", "b/b", "b/c", "b/d"}, delivered)
}

func TestDispatcherQueueLimit(t *testing.T) {
	h := newHook(t)
	h.offline.Store(true)
	ctx := context.Background()
	cfg := webhookTarget("hook", h.srv.URL)
	cfg.Filter = event.Filter{Bucket: "b"}
	cfg.QueueLimit = 2
	d := event.NewDispatcher(event.Deps{Factory: event.NewWebhookFactory(event.WebhookDeps{Client: h.srv.Client()})}, fastRetry())
	require.NoError(t, d.SetTarget(ctx, cfg))
	require.NoError(t, d.Start(ctx))
	defer stop(t, d)
	for _, k := range []string{"1", "2", "3", "4"} {
		require.NoError(t, d.Publish(ctx, objectEvent(event.ObjectCreatedPut, "b", k)))
	}
	time.Sleep(100 * time.Millisecond)
	h.offline.Store(false)
	h.waitFor(t, 2)
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, []string{"b/1", "b/2"}, h.keys(), "the events beyond the limit are dropped")
}

func TestDispatcherInMemoryQueueFull(t *testing.T) {
	ctx := context.Background()
	release := make(chan struct{})
	routing := make(chan struct{}, 1)
	rules := eventmock.NewBucketRules(t)
	rules.EXPECT().NotificationConfig(mock.Anything, "b").RunAndReturn(func(context.Context, string) ([]byte, error) {
		select {
		case routing <- struct{}{}:
		default:
		}
		<-release
		return nil, nil
	})
	d := event.NewDispatcher(event.Deps{Rules: rules}, event.WithQueueSize(1))
	require.NoError(t, d.Start(ctx))
	// The router took the first event out of the queue and blocks on it,
	// the second fills the queue, the third is refused. The test waits for
	// the router: a queue still holding the first event would refuse the
	// second and then make room for the third.
	require.NoError(t, d.Publish(ctx, objectEvent(event.ObjectCreatedPut, "b", "1")))
	select {
	case <-routing:
	case <-time.After(5 * time.Second):
		t.Fatal("the router did not take the first event")
	}
	require.NoError(t, d.Publish(ctx, objectEvent(event.ObjectCreatedPut, "b", "2")))
	assert.ErrorIs(t, d.Publish(ctx, objectEvent(event.ObjectCreatedPut, "b", "3")), domain.ErrStorageFull)
	close(release)
	stop(t, d)
}

func TestDispatcherTargetCRUD(t *testing.T) {
	ctx := context.Background()
	d := event.NewDispatcher(event.Deps{})
	tests := []struct {
		name string
		cfg  event.TargetConfig
	}{
		{"no id", event.TargetConfig{Type: event.TypeWebhook}},
		{"unknown type", event.TargetConfig{ID: "x", Type: "mysql"}},
		{"kafka without brokers", event.TargetConfig{ID: "x", Type: event.TypeKafka}},
		{"bad webhook url", event.TargetConfig{ID: "x", Type: event.TypeWebhook, Webhook: event.WebhookConfig{Endpoint: "not a url"}}},
		// queue_dir stays inside the queue root: the queue consumes and
		// removes the files of its directory.
		{"absolute queue_dir", event.TargetConfig{ID: "x", Type: event.TypeWebhook, QueueDir: "/etc", Webhook: event.WebhookConfig{Endpoint: "http://h"}}},
		{"queue_dir escaping", event.TargetConfig{ID: "x", Type: event.TypeWebhook, QueueDir: "a/../../drive1", Webhook: event.WebhookConfig{Endpoint: "http://h"}}},
		{"queue_dir parent", event.TargetConfig{ID: "x", Type: event.TypeWebhook, QueueDir: "..", Webhook: event.WebhookConfig{Endpoint: "http://h"}}},
		{"queue_dir root", event.TargetConfig{ID: "x", Type: event.TypeWebhook, QueueDir: ".", Webhook: event.WebhookConfig{Endpoint: "http://h"}}},
	}
	for _, tc := range tests {
		assert.ErrorIs(t, d.SetTarget(ctx, tc.cfg), domain.ErrInvalidArgument, tc.name)
	}

	require.NoError(t, d.SetTarget(ctx, webhookTarget("b", "http://b")))
	require.NoError(t, d.SetTarget(ctx, webhookTarget("a", "http://a")))
	require.NoError(t, d.SetTarget(ctx, event.TargetConfig{ID: "a", Type: event.TypeNATS, NATS: event.NATSConfig{Address: "127.0.0.1:1", Subject: "s"}}))
	list, err := d.ListTargets(ctx)
	require.NoError(t, err)
	require.Len(t, list, 3)
	assert.Equal(t, event.TargetID{ID: "a", Type: event.TypeNATS}, list[0].TargetID())
	assert.Equal(t, event.TargetID{ID: "a", Type: event.TypeWebhook}, list[1].TargetID())

	b := event.TargetID{ID: "b", Type: event.TypeWebhook}
	got, err := d.GetTarget(ctx, b)
	require.NoError(t, err)
	assert.Equal(t, "http://b", got.Webhook.Endpoint)
	require.NoError(t, d.SetTarget(ctx, webhookTarget("b", "http://b2")), "replace")
	got, _ = d.GetTarget(ctx, b)
	assert.Equal(t, "http://b2", got.Webhook.Endpoint)
	list, _ = d.ListTargets(ctx)
	assert.Len(t, list, 3)

	require.NoError(t, d.RemoveTarget(ctx, b))
	_, err = d.GetTarget(ctx, b)
	assert.ErrorIs(t, err, domain.ErrNotFound)
	assert.ErrorIs(t, d.RemoveTarget(ctx, b), domain.ErrNotFound)
}

// TestDispatcherLoadsTargetsFromConfig: the notify_* subsystems configure
// the targets at Start and when they change.
func TestDispatcherLoadsTargetsFromConfig(t *testing.T) {
	h := newHook(t)
	ctx := context.Background()
	var mu sync.Mutex
	settings := map[string]config.KV{
		"notify_webhook:primary": {"enable": "on", "endpoint": h.srv.URL, "auth_token": "Basic dXNlcg=="},
		"notify_webhook:off":     {"enable": "off", "endpoint": h.srv.URL},
		"notify_kafka:broken":    {"enable": "on"},
		"scanner":                {"speed": "fast"},
	}
	changes := make(chan config.Change, 4)
	cfgSvc := configmock.NewService(t)
	cfgSvc.EXPECT().List(mock.Anything).RunAndReturn(func(context.Context) ([]string, error) {
		mu.Lock()
		defer mu.Unlock()
		out := make([]string, 0, len(settings))
		for k := range settings {
			out = append(out, k)
		}
		return out, nil
	})
	cfgSvc.EXPECT().Get(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, s string) (config.KV, error) {
		mu.Lock()
		defer mu.Unlock()
		return settings[s], nil
	})
	cfgSvc.EXPECT().Watch(mock.Anything).Return(changes)
	rules := bucketRules(t, map[string]string{"b": `<NotificationConfiguration><QueueConfiguration><Queue>arn:nexo:sqs:us-east-1:primary:webhook</Queue><Event>s3:ObjectCreated:*</Event></QueueConfiguration></NotificationConfiguration>`})
	d := event.NewDispatcher(event.Deps{Config: cfgSvc, Rules: rules, Factory: event.NewWebhookFactory(event.WebhookDeps{Client: h.srv.Client()})})
	require.NoError(t, d.Start(ctx))
	defer stop(t, d)

	list, err := d.ListTargets(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1, "only the enabled, valid targets")
	assert.Equal(t, event.TargetID{ID: "primary", Type: event.TypeWebhook}, list[0].TargetID())
	require.NoError(t, d.Publish(ctx, objectEvent(event.ObjectCreatedPut, "b", "k")))
	h.waitFor(t, 1)
	assert.Equal(t, []string{"Basic dXNlcg=="}, h.auth, "an auth token naming its scheme is sent as is")

	mu.Lock()
	settings["notify_webhook:primary"] = config.KV{"enable": "off"}
	settings["notify_webhook"] = config.KV{"enable": "on", "endpoint": h.srv.URL}
	mu.Unlock()
	changes <- config.Change{Subsystem: "notify_webhook:primary"}
	require.Eventually(t, func() bool {
		list, _ := d.ListTargets(ctx)
		return len(list) == 1 && list[0].ID == event.DefaultTargetID
	}, 5*time.Second, 5*time.Millisecond)
	changes <- config.Change{Subsystem: "scanner"}
}

func TestWebhookReportsHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusBadGateway)
	}))
	defer srv.Close()
	target, err := event.NewWebhookFactory(event.WebhookDeps{Client: srv.Client()}).New(context.Background(), webhookTarget("h", srv.URL))
	require.NoError(t, err)
	assert.Equal(t, event.TargetID{ID: "h", Type: event.TypeWebhook}, target.ID())
	err = target.Send(context.Background(), event.NewEvent(event.ObjectCreatedPut, "r", time.Now()))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "502")
	assert.NoError(t, target.Close(context.Background()))

	_, err = event.NewWebhookFactory(event.WebhookDeps{}).New(context.Background(), event.TargetConfig{ID: "k", Type: event.TypeKafka,
		Kafka: event.KafkaConfig{Brokers: []string{"b:1"}, Topic: "t"}})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument, "the webhook factory builds webhooks only")
}

// TestWebhookRedactsEndpointCredentials: the errors of a webhook (failed
// request, invalid configuration) never carry the token of an endpoint in
// its query or user info: they reach the logs and the Admin API.
func TestWebhookRedactsEndpointCredentials(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := srv.Listener.Addr().String()
	srv.Close() // the endpoint refuses the connections

	f := event.NewTargetFactory(event.FactoryDeps{})
	for _, endpoint := range []string{
		"http://" + addr + "/ingest?token=s3cret-token",
		"http://user:s3cret-token@" + addr + "/ingest",
	} {
		target, err := f.New(ctx, webhookTarget("h", endpoint))
		require.NoError(t, err)
		err = target.Send(ctx, objectEvent(event.ObjectCreatedPut, "b", "k"))
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "s3cret-token", endpoint)
		assert.Contains(t, err.Error(), addr, "the host stays to identify the endpoint")
		var ue *url.Error
		assert.ErrorAs(t, err, &ue, "the cause is kept")
	}

	_, err := f.New(ctx, webhookTarget("bad", "ftp://h/x?token=s3cret-token"))
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
	assert.NotContains(t, err.Error(), "s3cret-token")
	_, err = f.New(ctx, webhookTarget("bad", "http://h x/?token=s3cret-token"))
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
	assert.NotContains(t, err.Error(), "s3cret-token")
}

// TestWebhookNeverFollowsRedirects: a webhook receiver that answers with a
// redirect cannot bounce the POST of Nexo, its body and the auth_token of
// the target, to another host (a blind SSRF to internal services): the
// redirect is not followed and the delivery fails, with the per-target
// client and with an injected *http.Client alike.
func TestWebhookNeverFollowsRedirects(t *testing.T) {
	ctx := context.Background()
	var internalCalls atomic.Int32
	var leaked atomic.Value
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		internalCalls.Add(1)
		leaked.Store(r.Header.Get("Authorization"))
	}))
	defer internal.Close()
	for _, code := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		external := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, internal.URL+"/admin/secret-action", code)
		}))
		for name, f := range map[string]event.TargetFactory{
			"per-target client": event.NewTargetFactory(event.FactoryDeps{}),
			"injected client":   event.NewWebhookFactory(event.WebhookDeps{Client: &http.Client{Timeout: 5 * time.Second}}),
		} {
			cfg := webhookTarget("h", external.URL)
			cfg.Webhook.AuthToken = "s3cr3t"
			target, err := f.New(ctx, cfg)
			require.NoError(t, err)
			err = target.Send(ctx, objectEvent(event.ObjectCreatedPut, "b", "k"))
			require.Error(t, err, "%s, %d: a redirect is a failed delivery", name, code)
			assert.Contains(t, err.Error(), strconv.Itoa(code), name)
		}
		external.Close()
	}
	assert.Zero(t, internalCalls.Load(), "the redirect target is never called")
	assert.Nil(t, leaked.Load(), "the token never reaches another host")
}

// TestWebhookTLSSkipVerify: the per-target client verifies the server
// certificate unless tls_skip_verify is set.
func TestWebhookTLSSkipVerify(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer srv.Close()
	ctx := context.Background()
	f := event.NewTargetFactory(event.FactoryDeps{})

	strict, err := f.New(ctx, webhookTarget("strict", srv.URL))
	require.NoError(t, err)
	assert.Error(t, strict.Send(ctx, objectEvent(event.ObjectCreatedPut, "b", "k")), "self-signed certificate refused")

	cfg := webhookTarget("lax", srv.URL)
	cfg.Webhook.TLSSkipVerify = true
	lax, err := f.New(ctx, cfg)
	require.NoError(t, err)
	require.NoError(t, lax.Send(ctx, objectEvent(event.ObjectCreatedPut, "b", "k")))
	assert.Equal(t, int32(1), calls.Load())

	cfg.Webhook.ClientCert = "/missing.crt"
	_, err = f.New(ctx, cfg)
	assert.ErrorIs(t, err, domain.ErrInvalidArgument, "client_cert without client_key")
	cfg.Webhook.ClientKey = "/missing.key"
	_, err = f.New(ctx, cfg)
	assert.ErrorIs(t, err, domain.ErrInvalidArgument, "unreadable client certificate")
}
