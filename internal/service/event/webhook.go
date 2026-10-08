package event

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
)

// webhookFactory builds webhook targets sharing one HTTP client.
type webhookFactory struct {
	client *http.Client
}

var _ TargetFactory = (*webhookFactory)(nil)

// NewWebhookFactory returns a factory for webhook targets. A nil client uses
// a default one with a 10s timeout.
func NewWebhookFactory(client *http.Client) TargetFactory {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &webhookFactory{client: client}
}

// New implements TargetFactory.
func (f *webhookFactory) New(cfg TargetConfig) (Target, error) {
	if cfg.Type != TypeWebhook {
		return nil, fmt.Errorf("%w: unsupported target type %q", domain.ErrInvalidArgument, cfg.Type)
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("%w: webhook endpoint %q must be an http(s) URL", domain.ErrInvalidArgument, cfg.Endpoint)
	}
	return &webhook{id: cfg.ID, endpoint: cfg.Endpoint, token: cfg.AuthToken, client: f.client}, nil
}

// webhook posts events as JSON to an HTTP endpoint.
type webhook struct {
	id       string
	endpoint string
	token    string
	client   *http.Client
}

var _ Target = (*webhook)(nil)

// Payload is the JSON body sent by the webhook target.
type Payload struct {
	EventName Name    `json:"EventName"`
	Key       string  `json:"Key"`
	Records   []Event `json:"Records"`
}

// ID implements Target.
func (w *webhook) ID() string { return w.id }

// Send implements Target.
func (w *webhook) Send(ctx context.Context, e Event) error {
	body, err := json.Marshal(Payload{
		EventName: e.EventName,
		Key:       e.S3.Bucket.Name + "/" + e.S3.Object.Key,
		Records:   []Event{e},
	})
	if err != nil {
		return fmt.Errorf("webhook %s: encode: %w", w.id, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("webhook %s: build request: %w", w.id, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "nexo-webhook")
	if w.token != "" {
		req.Header.Set("Authorization", "Bearer "+w.token)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("webhook %s: post: %w", w.id, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("webhook %s: endpoint returned %s", w.id, resp.Status)
	}
	return nil
}

// Close implements Target.
func (w *webhook) Close() error { return nil }
