package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// redacted replaces the secrets in the debug output.
const redacted = "REDACTED"

// signatureExpr matches the signature of an Authorization header.
const signatureExpr = `Signature=[0-9a-fA-F]+`

// secretQueryParams are the query parameters hidden in the debug output.
func secretQueryParams() []string {
	return []string{"X-Amz-Signature", "X-Amz-Security-Token", "X-Amz-Credential"}
}

// secretHeaders are the headers hidden in the debug output (Authorization
// keeps its scope and signed headers, only the signature is hidden).
func secretHeaders() []string {
	return []string{"X-Amz-Security-Token", "Cookie", "Set-Cookie", "X-Amz-Server-Side-Encryption-Customer-Key"}
}

// debugTransport dumps the requests and responses (headers only) with the
// credentials redacted (--debug).
type debugTransport struct {
	next      http.RoundTripper
	w         io.Writer
	mu        sync.Mutex
	signature *regexp.Regexp
}

func newDebugTransport(next http.RoundTripper, w io.Writer) http.RoundTripper {
	return &debugTransport{next: next, w: w, signature: regexp.MustCompile(signatureExpr)}
}

// RoundTrip implements http.RoundTripper.
func (t *debugTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.dump(func(b *strings.Builder) {
		proto := req.Proto
		if proto == "" {
			proto = "HTTP/1.1"
		}
		fmt.Fprintf(b, "> %s %s %s\n", req.Method, RedactURL(req.URL.RequestURI()), proto)
		fmt.Fprintf(b, "> Host: %s\n", hostOf(req))
		t.headers(b, ">", req.Header)
	})
	start := time.Now()
	resp, err := t.next.RoundTrip(req)
	elapsed := time.Since(start).Round(time.Millisecond)
	t.dump(func(b *strings.Builder) {
		if err != nil {
			fmt.Fprintf(b, "< error after %s: %v\n\n", elapsed, err)
			return
		}
		fmt.Fprintf(b, "< %s %s (%s)\n", resp.Proto, resp.Status, elapsed)
		t.headers(b, "<", resp.Header)
		b.WriteString("\n")
	})
	return resp, err
}

// hostOf returns the Host of a request (the URL host when the field is
// empty).
func hostOf(req *http.Request) string {
	if req.Host != "" {
		return req.Host
	}
	return req.URL.Host
}

func (t *debugTransport) dump(fn func(b *strings.Builder)) {
	var b strings.Builder
	fn(&b)
	t.mu.Lock()
	defer t.mu.Unlock()
	_, _ = io.WriteString(t.w, b.String())
}

func (t *debugTransport) headers(b *strings.Builder, dir string, h http.Header) {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, v := range h[k] {
			fmt.Fprintf(b, "%s %s: %s\n", dir, k, t.redactHeader(k, v))
		}
	}
}

func (t *debugTransport) redactHeader(name, value string) string {
	if strings.EqualFold(name, "Authorization") {
		return t.signature.ReplaceAllString(value, "Signature="+redacted)
	}
	for _, s := range secretHeaders() {
		if strings.EqualFold(name, s) {
			return redacted
		}
	}
	return value
}

// RedactURL hides the signature, the credential and the session token of
// a presigned URL (or of a request URI).
func RedactURL(raw string) string {
	base, query, ok := strings.Cut(raw, "?")
	if !ok {
		return raw
	}
	parts := strings.Split(query, "&")
	for i, p := range parts {
		name, _, _ := strings.Cut(p, "=")
		decoded, err := url.QueryUnescape(name)
		if err != nil {
			continue
		}
		for _, s := range secretQueryParams() {
			if strings.EqualFold(decoded, s) {
				parts[i] = name + "=" + redacted
			}
		}
	}
	return base + "?" + strings.Join(parts, "&")
}

// limitTransport caps the bandwidth of the request bodies (uploads) and
// response bodies (downloads) of all the requests of a client.
type limitTransport struct {
	next       http.RoundTripper
	upload     *rateLimiter
	download   *rateLimiter
	chunkBytes int
}

func newLimitTransport(next http.RoundTripper, upload, download int64) http.RoundTripper {
	t := &limitTransport{next: next, chunkBytes: 32 << 10}
	if upload > 0 {
		t.upload = &rateLimiter{rate: upload}
	}
	if download > 0 {
		t.download = &rateLimiter{rate: download}
	}
	return t
}

// RoundTrip implements http.RoundTripper.
func (t *limitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.upload != nil && req.Body != nil && req.Body != http.NoBody {
		clone := req.Clone(req.Context())
		clone.Body = &limitedBody{ReadCloser: req.Body, ctx: req.Context(), limiter: t.upload, chunk: t.chunkBytes}
		req = clone
	}
	resp, err := t.next.RoundTrip(req)
	if err == nil && t.download != nil && resp.Body != nil {
		resp.Body = &limitedBody{ReadCloser: resp.Body, ctx: req.Context(), limiter: t.download, chunk: t.chunkBytes}
	}
	return resp, err
}

// limitedBody paces the reads of a body.
type limitedBody struct {
	io.ReadCloser
	ctx     context.Context
	limiter *rateLimiter
	chunk   int
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if len(p) > b.chunk {
		p = p[:b.chunk]
	}
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		if werr := b.limiter.wait(b.ctx, n); werr != nil {
			return n, werr
		}
	}
	return n, err
}

// rateLimiter spaces the transfers of n bytes so that the average rate does
// not exceed rate bytes per second (shared by the concurrent requests).
type rateLimiter struct {
	mu   sync.Mutex
	rate int64
	next time.Time
}

func (l *rateLimiter) wait(ctx context.Context, n int) error {
	l.mu.Lock()
	now := time.Now()
	if l.next.Before(now) {
		l.next = now
	}
	l.next = l.next.Add(time.Duration(float64(n) / float64(l.rate) * float64(time.Second)))
	delay := l.next.Sub(now)
	l.mu.Unlock()
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
