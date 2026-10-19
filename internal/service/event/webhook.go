package event

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
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

// Send implements Target: any status but 2xx is a failure, a redirect
// included (it is never followed: see noRedirects).
func (w *webhook) Send(ctx context.Context, e Event) error {
	body, err := json.Marshal(e.payload())
	if err != nil {
		return fmt.Errorf("webhook %s: encode: %w", w.id, err)
	}
	return w.post(ctx, body)
}

// post sends a JSON body to the endpoint.
func (w *webhook) post(ctx context.Context, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.cfg.Endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("webhook %s: build request: %w", w.id, redactURLError(err))
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
		// The error names the URL, whose query or user info may hold a
		// token (common for ingest URLs): it reaches the logs and the
		// Admin API (target status, test event).
		return fmt.Errorf("webhook %s: post: %w", w.id, redactURLError(err))
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

// redactedPart replaces the secrets of a URL in the redacted form.
const redactedPart = "REDACTED"

// redactURL returns raw without the parts of a URL that may carry
// credentials: the user info and the query are replaced, the fragment is
// dropped. A value that is not a URL is replaced altogether.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "[invalid URL]"
	}
	if u.User != nil {
		u.User = url.User(redactedPart)
	}
	if u.RawQuery != "" || u.ForceQuery {
		u.RawQuery, u.ForceQuery = redactedPart, false
	}
	u.Fragment, u.RawFragment = "", ""
	return u.String()
}

// redactURLError returns err with the URL of its *url.Error (the error of
// an HTTP request) redacted; the cause is kept for errors.Is and As.
func redactURLError(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	return &url.Error{Op: ue.Op, URL: redactURL(ue.URL), Err: ue.Err}
}

// mediaWorker posts the media jobs (Event.Media of the MediaJob events)
// to a media worker, as JSON, with the authentication of a webhook. The
// test event posts a job with the action "test"; any other event is not
// for it and is dropped.
type mediaWorker struct {
	webhook
}

// Send implements Target.
func (m *mediaWorker) Send(ctx context.Context, e Event) error {
	job := e.Media
	switch {
	case job != nil:
	case e.EventName == TestEvent:
		job = &domain.MediaJob{Action: domain.MediaJobTest}
	default:
		return nil
	}
	body, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("media worker %s: encode: %w", m.id, err)
	}
	return m.post(ctx, body)
}
