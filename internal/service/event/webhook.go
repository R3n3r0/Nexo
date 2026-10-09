package event

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// webhook posts events as JSON (Payload) to an HTTP endpoint.
type webhook struct {
	id        TargetID
	cfg       WebhookConfig
	client    HTTPDoer
	userAgent string
}

var _ Target = (*webhook)(nil)

// ID implements Target.
func (w *webhook) ID() TargetID { return w.id }

// Send implements Target: any status but 2xx is a failure.
func (w *webhook) Send(ctx context.Context, e Event) error {
	body, err := json.Marshal(e.payload())
	if err != nil {
		return fmt.Errorf("webhook %s: encode: %w", w.id, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.cfg.Endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("webhook %s: build request: %w", w.id, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", w.userAgent)
	if token := strings.TrimSpace(w.cfg.AuthToken); token != "" {
		if !strings.Contains(token, " ") {
			token = "Bearer " + token
		}
		req.Header.Set("Authorization", token)
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
func (w *webhook) Close(context.Context) error { return nil }
