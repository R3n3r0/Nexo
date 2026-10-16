package dns01

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// CloudflareEndpoint is the base URL of the Cloudflare v4 API.
const CloudflareEndpoint = "https://api.cloudflare.com/client/v4"

// CloudflareConfig configures the Cloudflare provider.
type CloudflareConfig struct {
	// APIToken is an API token with the Zone:DNS:Edit permission on the
	// zone (and Zone:Zone:Read without ZoneID). A secret.
	APIToken string
	// ZoneID is the id of the zone (default: the zone whose name encloses
	// the record, found through the API).
	ZoneID string
	Settings
}

type cloudflare struct {
	cfg  CloudflareConfig
	opts options
}

var _ Provider = (*cloudflare)(nil)

// NewCloudflare returns the provider of the zones hosted by Cloudflare.
func NewCloudflare(cfg CloudflareConfig, opts ...Option) (Provider, error) {
	cfg.APIToken, cfg.ZoneID = strings.TrimSpace(cfg.APIToken), strings.TrimSpace(cfg.ZoneID)
	if cfg.APIToken == "" {
		return nil, fmt.Errorf("%w: cloudflare: the API token is required", ErrInvalidConfig)
	}
	o := buildOptions(opts)
	if o.endpoint == "" {
		o.endpoint = CloudflareEndpoint
	}
	return &cloudflare{cfg: cfg, opts: o}, nil
}

// Propagation implements Provider.
func (p *cloudflare) Propagation() (time.Duration, time.Duration) { return p.cfg.propagation() }

// cfResponse is the envelope of the answers of the API.
type cfResponse struct {
	Success bool `json:"success"`
	Errors  []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
	Result json.RawMessage `json:"result"`
}

// call sends a request to the API and decodes its result into out.
func (p *cloudflare) call(ctx context.Context, method, path string, query url.Values, body, out any) error {
	u := p.opts.endpoint + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.cfg.APIToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := p.opts.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("cloudflare: %s %s: %w", method, path, err)
	}
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("cloudflare: %s %s: %w", method, path, err)
	}
	var env cfResponse
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("cloudflare: %s %s: HTTP %d, unexpected answer", method, path, res.StatusCode)
	}
	if !env.Success || res.StatusCode >= 300 {
		var msgs []string
		for _, e := range env.Errors {
			msgs = append(msgs, fmt.Sprintf("%d %s", e.Code, e.Message))
		}
		return fmt.Errorf("cloudflare: %s %s: HTTP %d: %s", method, path, res.StatusCode, strings.Join(msgs, "; "))
	}
	if out != nil {
		if err := json.Unmarshal(env.Result, out); err != nil {
			return fmt.Errorf("cloudflare: %s %s: %w", method, path, err)
		}
	}
	return nil
}

// zone returns the id of the zone of fqdn.
func (p *cloudflare) zone(ctx context.Context, fqdn string) (string, error) {
	if p.cfg.ZoneID != "" {
		return p.cfg.ZoneID, nil
	}
	for _, name := range parentNames(fqdn) {
		var zones []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if err := p.call(ctx, http.MethodGet, "/zones", url.Values{"name": {name}}, nil, &zones); err != nil {
			return "", err
		}
		for _, z := range zones {
			if strings.EqualFold(z.Name, name) {
				return z.ID, nil
			}
		}
	}
	return "", fmt.Errorf("cloudflare: no zone of the account encloses %s", unFqdn(fqdn))
}

type cfRecord struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl,omitempty"`
}

// records returns the TXT records of fqdn holding value.
func (p *cloudflare) records(ctx context.Context, zone, fqdn, value string) ([]cfRecord, error) {
	var recs []cfRecord
	q := url.Values{"type": {"TXT"}, "name": {unFqdn(fqdn)}}
	if err := p.call(ctx, http.MethodGet, "/zones/"+url.PathEscape(zone)+"/dns_records", q, nil, &recs); err != nil {
		return nil, err
	}
	var out []cfRecord
	for _, r := range recs {
		if strings.Trim(r.Content, `"`) == value {
			out = append(out, r)
		}
	}
	return out, nil
}

// Present implements Provider.
func (p *cloudflare) Present(ctx context.Context, fqdn, value string) error {
	zone, err := p.zone(ctx, fqdn)
	if err != nil {
		return err
	}
	existing, err := p.records(ctx, zone, fqdn, value)
	if err != nil {
		return err
	}
	if len(existing) > 0 {
		return nil
	}
	rec := cfRecord{Type: "TXT", Name: unFqdn(fqdn), Content: value, TTL: p.cfg.ttl()}
	return p.call(ctx, http.MethodPost, "/zones/"+url.PathEscape(zone)+"/dns_records", nil, rec, nil)
}

// CleanUp implements Provider.
func (p *cloudflare) CleanUp(ctx context.Context, fqdn, value string) error {
	zone, err := p.zone(ctx, fqdn)
	if err != nil {
		return err
	}
	recs, err := p.records(ctx, zone, fqdn, value)
	if err != nil {
		return err
	}
	for _, r := range recs {
		if err := p.call(ctx, http.MethodDelete, "/zones/"+url.PathEscape(zone)+"/dns_records/"+url.PathEscape(r.ID), nil, nil, nil); err != nil {
			return err
		}
	}
	return nil
}
