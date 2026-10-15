package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/event"
)

// EventName is the name of the events that carry the audit entries
// through the queues of the audit targets (never a bucket notification:
// only the audit targets subscribe to it).
const EventName event.Name = "audit:Entry"

// envelopeKey is the element of the event holding the JSON entry.
const envelopeKey = "x-nexo-audit-entry"

// FactoryDeps are the dependencies of the factory of the audit targets.
// Nil transports are replaced by the ones of the notification targets
// (event.NewWebhookClient, event.NewKafkaDialer).
type FactoryDeps struct {
	// HTTP performs the webhook calls (tests); it must not follow
	// redirects.
	HTTP event.HTTPDoer
	// Kafka opens the Kafka producers.
	Kafka  event.KafkaDialer
	Logger *slog.Logger
}

// FactoryOption configures the factory.
type FactoryOption func(*factory)

// WithUserAgent sets the User-Agent of the webhook calls (default
// "nexo-audit").
func WithUserAgent(ua string) FactoryOption {
	return func(f *factory) {
		if ua != "" {
			f.userAgent = ua
		}
	}
}

type factory struct {
	deps      FactoryDeps
	userAgent string
}

var _ event.TargetFactory = (*factory)(nil)

// NewTargetFactory returns the factory of the audit targets for the event
// dispatcher of the audit log: webhook targets post every entry as a JSON
// document, Kafka targets write it to the topic keyed by its request id.
func NewTargetFactory(deps FactoryDeps, opts ...FactoryOption) event.TargetFactory {
	if deps.Kafka == nil {
		deps.Kafka = event.NewKafkaDialer()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	f := &factory{deps: deps, userAgent: "nexo-audit"}
	for _, o := range opts {
		o(f)
	}
	return f
}

// New implements event.TargetFactory.
func (f *factory) New(_ context.Context, cfg event.TargetConfig) (event.Target, error) {
	id := cfg.TargetID()
	switch cfg.Type {
	case event.TypeWebhook:
		client := f.deps.HTTP
		if client == nil {
			c, err := event.NewWebhookClient(cfg.Webhook)
			if err != nil {
				return nil, err
			}
			client = c
		}
		return &webhookTarget{id: id, cfg: cfg.Webhook, client: client, userAgent: f.userAgent}, nil
	case event.TypeKafka:
		return &kafkaTarget{id: id, cfg: cfg.Kafka, dialer: f.deps.Kafka}, nil
	}
	return nil, fmt.Errorf("%w: audit: unsupported target type %q", domain.ErrInvalidArgument, cfg.Type)
}

// entryOf extracts the JSON entry carried by an event.
func entryOf(e event.Event) ([]byte, error) {
	raw, ok := e.ResponseElements[envelopeKey]
	if !ok || !json.Valid([]byte(raw)) {
		return nil, fmt.Errorf("%w: audit: the event carries no audit entry", domain.ErrInvalidArgument)
	}
	return []byte(raw), nil
}

// webhookTarget posts the entries to an HTTP endpoint.
type webhookTarget struct {
	id        event.TargetID
	cfg       event.WebhookConfig
	client    event.HTTPDoer
	userAgent string
}

var _ event.Target = (*webhookTarget)(nil)

// ID implements event.Target.
func (t *webhookTarget) ID() event.TargetID { return t.id }

// Send implements event.Target: any status but 2xx is a failure (the
// entry stays queued).
func (t *webhookTarget) Send(ctx context.Context, e event.Event) error {
	body, err := entryOf(e)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.cfg.Endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("audit webhook %s: build request: %w", t.id, redactURLError(err))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", t.userAgent)
	if token := strings.TrimSpace(t.cfg.AuthToken); token != "" {
		if !strings.Contains(token, " ") {
			token = "Bearer " + token
		}
		req.Header.Set("Authorization", token)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("audit webhook %s: post: %w", t.id, redactURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("audit webhook %s: endpoint returned %s", t.id, resp.Status)
	}
	return nil
}

// Close implements event.Target.
func (t *webhookTarget) Close(context.Context) error { return nil }

// redactURLError returns err with the URL of its *url.Error without its
// credentials (user info, query).
func redactURLError(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	return &url.Error{Op: ue.Op, URL: domain.RedactURL(ue.URL), Err: ue.Err}
}

// kafkaTarget writes the entries to a Kafka topic; the producer is opened
// on the first entry and again after a failure.
type kafkaTarget struct {
	id     event.TargetID
	cfg    event.KafkaConfig
	dialer event.KafkaDialer

	mu   sync.Mutex
	prod event.KafkaProducer
}

var _ event.Target = (*kafkaTarget)(nil)

// ID implements event.Target.
func (t *kafkaTarget) ID() event.TargetID { return t.id }

// Send implements event.Target.
func (t *kafkaTarget) Send(ctx context.Context, e event.Event) error {
	body, err := entryOf(e)
	if err != nil {
		return err
	}
	var key struct {
		RequestID string `json:"requestId"`
		Node      string `json:"node"`
	}
	_ = json.Unmarshal(body, &key)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.prod == nil {
		p, err := t.dialer.Dial(ctx, t.cfg)
		if err != nil {
			return fmt.Errorf("audit kafka %s: %w", t.id, err)
		}
		t.prod = p
	}
	if err := t.prod.WriteMessage(ctx, []byte(key.Node+"/"+key.RequestID), body); err != nil {
		_ = t.prod.Close(ctx)
		t.prod = nil
		return fmt.Errorf("audit kafka %s: %w", t.id, err)
	}
	return nil
}

// Close implements event.Target.
func (t *kafkaTarget) Close(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.prod == nil {
		return nil
	}
	err := t.prod.Close(ctx)
	t.prod = nil
	return err
}
