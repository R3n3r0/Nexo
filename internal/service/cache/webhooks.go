package cache

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/r3n3r0/nexo/internal/service/distribution"
)

// Limits of the purge webhooks.
const (
	// cloudflareBatch is the number of URLs of a Cloudflare purge call.
	cloudflareBatch = 30
	maxWebhookBody  = 64 << 10
	webhookTimeout  = 10 * time.Second
	defaultCFAPI    = "https://api.cloudflare.com/client/v4"
	defaultFastly   = "https://api.fastly.com"
)

// purgeJob is a purge to send to a webhook, kept in a persistent queue
// until the endpoint accepts it (TCC-15).
type purgeJob struct {
	ID             string                     `json:"id"`
	Distribution   string                     `json:"distribution"`
	Webhook        string                     `json:"webhook"`
	Domain         string                     `json:"domain,omitempty"`
	Hosts          []string                   `json:"hosts,omitempty"`
	Paths          []string                   `json:"paths,omitempty"`
	Patterns       []distribution.PathPattern `json:"patterns,omitempty"`
	Tags           []string                   `json:"tags,omitempty"`
	All            bool                       `json:"all,omitempty"`
	Soft           bool                       `json:"soft,omitempty"`
	Automatic      bool                       `json:"automatic,omitempty"`
	InvalidationID string                     `json:"invalidationId,omitempty"`
	CreatedAt      time.Time                  `json:"createdAt"`
	Attempts       int                        `json:"attempts"`
	NextAttempt    time.Time                  `json:"nextAttempt"`
	LastError      string                     `json:"lastError,omitempty"`
}

// webhookQueue is the persistent queue of the purge webhooks of this node:
// one file per job, sent in order, retried with a growing delay.
type webhookQueue struct {
	dir string
	// sending serializes the senders (a job is sent by one at a time).
	sending sync.Mutex
	mu      sync.Mutex
	jobs    []*purgeJob
	wake    chan struct{}
}

// newWebhookQueue opens the queue (in memory without dir) and reloads the
// jobs left by the previous run.
func newWebhookQueue(dir string) (*webhookQueue, error) {
	q := &webhookQueue{dir: dir, wake: make(chan struct{}, 1)}
	if dir == "" {
		return q, nil
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("cache: webhook queue %s: %w", dir, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), docSuffix) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name())) // #nosec G304 -- a job file of the queue directory of the server
		if err != nil {
			continue
		}
		var j purgeJob
		if json.Unmarshal(data, &j) != nil || j.ID == "" {
			_ = os.Remove(filepath.Join(dir, e.Name()))
			continue
		}
		q.jobs = append(q.jobs, &j)
	}
	sort.Slice(q.jobs, func(a, b int) bool { return q.jobs[a].ID < q.jobs[b].ID })
	return q, nil
}

// persist writes a job.
func (q *webhookQueue) persist(j *purgeJob) error {
	if q.dir == "" {
		return nil
	}
	data, err := json.Marshal(j)
	if err != nil {
		return err
	}
	path := filepath.Join(q.dir, j.ID+docSuffix)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// add queues a job.
func (q *webhookQueue) add(j *purgeJob) error {
	if err := q.persist(j); err != nil {
		return err
	}
	q.mu.Lock()
	q.jobs = append(q.jobs, j)
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return nil
}

// due returns the jobs to send now, in order.
func (q *webhookQueue) due(now time.Time) []*purgeJob {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []*purgeJob
	for _, j := range q.jobs {
		if !j.NextAttempt.After(now) {
			out = append(out, j)
		}
	}
	return out
}

// done removes a job.
func (q *webhookQueue) done(j *purgeJob) {
	q.mu.Lock()
	q.jobs = slices.DeleteFunc(q.jobs, func(x *purgeJob) bool { return x == j })
	q.mu.Unlock()
	if q.dir != "" {
		_ = os.Remove(filepath.Join(q.dir, j.ID+docSuffix))
	}
}

// size is the number of jobs queued.
func (q *webhookQueue) size() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.jobs)
}

// queueWebhooks queues a purge for every webhook of the distribution that
// applies to the domains concerned.
func (s *service) queueWebhooks(ctx context.Context, sn *snapshot, d distribution.Distribution, job purgeJob) {
	cd := sn.dists[d.ID]
	if cd == nil || len(cd.cfg.Webhooks) == 0 || s.webhooks == nil {
		return
	}
	var hosts []string
	for _, a := range d.Aliases {
		if strings.HasPrefix(a, "*.") {
			continue
		}
		if job.Domain == "" || hostMatches(job.Domain, a) {
			hosts = append(hosts, a)
		}
	}
	if job.Domain != "" && !strings.HasPrefix(job.Domain, "*.") && !slices.Contains(hosts, job.Domain) {
		hosts = append(hosts, job.Domain)
	}
	bound := map[string]bool{}
	for _, ds := range cd.cfg.Domains {
		for _, w := range ds.Cache.PurgeWebhooks {
			bound[w] = true
		}
	}
	for _, w := range cd.cfg.Webhooks {
		var applies []string
		for _, h := range hosts {
			if webhookApplies(cd, w, h, bound[w.ID]) {
				applies = append(applies, h)
			}
		}
		if len(applies) == 0 {
			continue
		}
		j := job
		j.ID, j.Webhook, j.Hosts, j.CreatedAt = s.newID("purge"), w.ID, applies, s.now().UTC()
		if err := s.webhooks.add(&j); err != nil {
			s.deps.Logger.WarnContext(ctx, "cache: purge webhook not queued", "webhook", w.ID, "err", err)
		}
	}
}

// webhookApplies reports whether a webhook purges a host: listed in its
// domains, bound by the settings of the domain, or unbound (every
// domain).
func webhookApplies(cd *compiledDist, w Webhook, host string, boundByDomain bool) bool {
	for _, d := range w.Domains {
		if hostMatches(d, host) {
			return true
		}
	}
	if i := cd.domainFor(host); i >= 0 && slices.Contains(cd.cfg.Domains[i].Cache.PurgeWebhooks, w.ID) {
		return true
	}
	return len(w.Domains) == 0 && !boundByDomain
}

// webhookLoop sends the queued purges.
func (s *service) webhookLoop(ctx context.Context) {
	defer s.bg.Done()
	ticker := time.NewTicker(s.opts.webhookMin)
	defer ticker.Stop()
	for {
		s.sendDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.webhooks.wake:
		}
	}
}

// sendDue sends the due jobs; a failed job waits for its next attempt.
func (s *service) sendDue(ctx context.Context) {
	s.webhooks.sending.Lock()
	defer s.webhooks.sending.Unlock()
	sn := s.snap.Load()
	for _, j := range s.webhooks.due(s.now()) {
		if ctx.Err() != nil {
			return
		}
		w, ok := sn.webhook(j.Distribution, j.Webhook)
		if !ok {
			s.deps.Logger.InfoContext(ctx, "cache: purge of a removed webhook dropped", "webhook", j.Webhook, "job", j.ID)
			s.webhooks.done(j)
			continue
		}
		err := s.sendPurge(ctx, w, j, false)
		if err == nil {
			s.webhooks.done(j)
			s.webhookCalls(ctx, w.Type, "ok")
			continue
		}
		s.webhookCalls(ctx, w.Type, "error")
		j.Attempts++
		delay := s.opts.webhookMin << min(j.Attempts-1, 20)
		if delay > s.opts.webhookMax || delay <= 0 {
			delay = s.opts.webhookMax
		}
		j.NextAttempt, j.LastError = s.now().Add(delay), err.Error()
		if perr := s.webhooks.persist(j); perr != nil {
			s.deps.Logger.WarnContext(ctx, "cache: purge job not saved", "job", j.ID, "err", perr)
		}
		s.deps.Logger.WarnContext(ctx, "cache: purge webhook failed, retried", "webhook", w.ID, "type", w.Type, "attempts", j.Attempts,
			"next", delay, "err", err)
	}
}

// webhook returns a webhook of a distribution.
func (sn *snapshot) webhook(dist, id string) (Webhook, bool) {
	if cd := sn.dists[dist]; cd != nil {
		for _, w := range cd.cfg.Webhooks {
			if w.ID == id {
				return w, true
			}
		}
	}
	return Webhook{}, false
}

// TestWebhook implements Manager.
func (s *service) TestWebhook(ctx context.Context, dist, id string) (WebhookTestResult, error) {
	d, err := s.deps.Distributions.Get(ctx, dist)
	if err != nil {
		return WebhookTestResult{}, err
	}
	cfg, err := s.loadDistConfig(ctx, dist)
	if err != nil {
		return WebhookTestResult{}, err
	}
	i := slices.IndexFunc(cfg.Webhooks, func(w Webhook) bool { return w.ID == id })
	if i < 0 {
		return WebhookTestResult{}, fmt.Errorf("%w: %s", ErrWebhookNotFound, id)
	}
	w := cfg.Webhooks[i]
	var hosts []string
	for _, a := range d.Aliases {
		if !strings.HasPrefix(a, "*.") {
			hosts = append(hosts, a)
		}
	}
	start := s.now()
	err = s.sendPurge(ctx, w, &purgeJob{ID: "test", Distribution: dist, Webhook: id, Hosts: hosts}, true)
	res := WebhookTestResult{OK: err == nil, Duration: s.now().Sub(start)}
	var se *statusError
	if errors.As(err, &se) {
		res.Status = se.status
	} else if err == nil {
		res.Status = http.StatusOK
	}
	if err != nil {
		// The status only: the body of an endpoint is never echoed back.
		res.Error = err.Error()
	}
	return res, nil
}

// statusError is a refusal of a webhook endpoint. It carries the status
// only: the answer of an endpoint is never reported (a webhook pointed at
// an internal service would read it back).
type statusError struct {
	status int
	// rejected reports a 2xx answer whose JSON says success: false.
	rejected bool
}

func (e *statusError) Error() string {
	if e.rejected {
		return fmt.Sprintf("webhook answered %d %s with success false", e.status, http.StatusText(e.status))
	}
	return fmt.Sprintf("webhook answered %d %s", e.status, http.StatusText(e.status))
}

// sendPurge sends the calls of a job to a webhook (a connection test with
// test).
func (s *service) sendPurge(ctx context.Context, w Webhook, j *purgeJob, test bool) error {
	reqs, err := s.purgeRequests(ctx, w, j, test)
	if err != nil {
		return err
	}
	for _, req := range reqs {
		if err := s.do(req); err != nil {
			return err
		}
	}
	return nil
}

// do sends one call.
func (s *service) do(req *http.Request) error {
	ctx, cancel := context.WithTimeout(req.Context(), webhookTimeout)
	defer cancel()
	resp, err := s.deps.HTTP.Do(req.WithContext(ctx))
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxWebhookBody))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &statusError{status: resp.StatusCode}
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "json") {
		var r struct {
			Success *bool `json:"success"`
		}
		if json.Unmarshal(body, &r) == nil && r.Success != nil && !*r.Success {
			return &statusError{status: resp.StatusCode, rejected: true}
		}
	}
	return nil
}

// urlsOf are the URLs of the paths of a job on its hosts.
func urlsOf(w Webhook, j *purgeJob) []string {
	scheme := w.Scheme
	if scheme == "" {
		scheme = "https"
	}
	var out []string
	for _, h := range j.Hosts {
		for _, p := range j.Paths {
			out = append(out, scheme+"://"+h+escapePath(p))
		}
	}
	return out
}

// escapePath escapes a decoded path for a URL.
func escapePath(p string) string { return (&url.URL{Path: p}).EscapedPath() }

// literalPrefix is the literal start of a pattern ("/assets/" of
// "/assets/**"); whole reports a prefix pattern (nothing after).
func literalPrefix(p distribution.PathPattern) (prefix string, whole bool) {
	switch p.Type {
	case distribution.PatternPrefix:
		return p.Pattern, true
	case distribution.PatternGlob:
		i := strings.IndexAny(p.Pattern, "*?[{")
		if i < 0 {
			return p.Pattern, false
		}
		return p.Pattern[:i], strings.HasSuffix(p.Pattern, "**") && i == len(p.Pattern)-2
	}
	return "", false
}

// patternRegex is the regular expression of a pattern for the Varnish
// bans: the RE2 expression the matcher evaluates the pattern with (RE2 is
// a subset of PCRE), so that the external caches purge exactly what Nexo
// purges.
func (s *service) patternRegex(ctx context.Context, p distribution.PathPattern) string {
	m, err := s.deps.Matcher.Compile(ctx, p)
	if err != nil {
		// The patterns are validated with the invalidation: ban the
		// literal path only.
		return "^" + regexp.QuoteMeta(p.Pattern) + "$"
	}
	return m.Expr()
}

// purgeRequests builds the calls of a job for the type of the webhook.
func (s *service) purgeRequests(ctx context.Context, w Webhook, j *purgeJob, test bool) ([]*http.Request, error) {
	token := s.webhookSecret(ctx, w.Token)
	newReq := func(method, target string, body []byte, ctype string) (*http.Request, error) {
		var rd io.Reader
		if body != nil {
			rd = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, target, rd)
		if err != nil {
			return nil, fmt.Errorf("%w: webhook %s: %v", ErrInvalid, w.ID, err)
		}
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		for k, v := range w.Headers {
			if w.Type != WebhookGeneric {
				req.Header.Set(k, v)
			}
		}
		return req, nil
	}
	jsonReq := func(method, target string, v any) (*http.Request, error) {
		body, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		return newReq(method, target, body, "application/json")
	}
	var out []*http.Request
	urls := urlsOf(w, j)
	switch w.Type {
	case WebhookCloudflare:
		base := strings.TrimRight(cmpOr(w.URL, defaultCFAPI), "/")
		target := base + "/zones/" + url.PathEscape(w.Zone) + "/purge_cache"
		auth := func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
		if test {
			r, err := newReq(http.MethodGet, base+"/zones/"+url.PathEscape(w.Zone), nil, "")
			if err != nil {
				return nil, err
			}
			auth(r)
			return []*http.Request{r}, nil
		}
		var bodies []any
		switch {
		case j.All && j.Domain == "":
			bodies = append(bodies, map[string]any{"purge_everything": true})
		case j.All || (j.Domain != "" && len(j.Paths)+len(j.Patterns)+len(j.Tags) == 0):
			bodies = append(bodies, map[string]any{"hosts": j.Hosts})
		}
		for i := 0; i < len(urls); i += cloudflareBatch {
			bodies = append(bodies, map[string]any{"files": urls[i:min(i+cloudflareBatch, len(urls))]})
		}
		if len(j.Tags) > 0 {
			bodies = append(bodies, map[string]any{"tags": j.Tags})
		}
		var prefixes []string
		wholeHosts := false
		for _, p := range j.Patterns {
			if pre, _ := literalPrefix(p); pre != "" && pre != "/" {
				for _, h := range j.Hosts {
					prefixes = append(prefixes, h+pre)
				}
			} else {
				wholeHosts = true
			}
		}
		if len(prefixes) > 0 {
			bodies = append(bodies, map[string]any{"prefixes": prefixes})
		}
		if wholeHosts {
			bodies = append(bodies, map[string]any{"hosts": j.Hosts})
		}
		for _, b := range bodies {
			r, err := jsonReq(http.MethodPost, target, b)
			if err != nil {
				return nil, err
			}
			auth(r)
			out = append(out, r)
		}
	case WebhookFastly:
		base := strings.TrimRight(cmpOr(w.URL, defaultFastly), "/")
		set := func(r *http.Request) {
			r.Header.Set("Fastly-Key", token)
			if j.Soft {
				r.Header.Set("Fastly-Soft-Purge", "1")
			}
		}
		if test {
			r, err := newReq(http.MethodGet, base+"/service/"+url.PathEscape(w.Zone), nil, "")
			if err != nil {
				return nil, err
			}
			set(r)
			return []*http.Request{r}, nil
		}
		for _, u := range urls {
			r, err := newReq(http.MethodPost, base+"/purge/"+strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://"), nil, "")
			if err != nil {
				return nil, err
			}
			set(r)
			out = append(out, r)
		}
		if len(j.Tags) > 0 {
			r, err := newReq(http.MethodPost, base+"/service/"+url.PathEscape(w.Zone)+"/purge", nil, "")
			if err != nil {
				return nil, err
			}
			set(r)
			r.Header.Set("Surrogate-Key", strings.Join(j.Tags, " "))
			out = append(out, r)
		}
		if j.All || len(j.Patterns) > 0 || (j.Domain != "" && len(j.Paths)+len(j.Tags) == 0) {
			r, err := newReq(http.MethodPost, base+"/service/"+url.PathEscape(w.Zone)+"/purge_all", nil, "")
			if err != nil {
				return nil, err
			}
			set(r)
			out = append(out, r)
		}
	case WebhookAkamai:
		base := strings.TrimRight(w.URL, "/")
		network := cmpOr(w.Network, "production")
		action := "delete"
		if j.Soft {
			action = "invalidate"
		}
		var calls []struct {
			kind    string
			objects any
		}
		if test {
			urls = urls[:min(1, len(urls))]
		}
		if len(urls) > 0 {
			calls = append(calls, struct {
				kind    string
				objects any
			}{"url", urls})
		}
		if len(j.Tags) > 0 && !test {
			calls = append(calls, struct {
				kind    string
				objects any
			}{"tag", j.Tags})
		}
		if !test && (j.All || len(j.Patterns) > 0 || (j.Domain != "" && len(j.Paths)+len(j.Tags) == 0)) {
			if w.CPCode == "" {
				s.deps.Logger.WarnContext(ctx, "cache: Akamai purge of a whole domain needs the CP code of the webhook", "webhook", w.ID)
			} else {
				calls = append(calls, struct {
					kind    string
					objects any
				}{"cpcode", []string{w.CPCode}})
			}
		}
		for _, c := range calls {
			body, err := json.Marshal(map[string]any{"objects": c.objects})
			if err != nil {
				return nil, err
			}
			r, err := newReq(http.MethodPost, base+"/ccu/v3/"+action+"/"+c.kind+"/"+network, body, "application/json")
			if err != nil {
				return nil, err
			}
			edgeGridSign(r, body, w.ClientToken, token, s.webhookSecret(ctx, w.ClientSecret), s.now())
			out = append(out, r)
		}
	case WebhookNginx:
		base := strings.TrimRight(w.URL, "/")
		method := cmpOr(w.Method, "PURGE")
		var paths []string
		paths = append(paths, j.Paths...)
		for _, p := range j.Patterns {
			pre, _ := literalPrefix(p)
			paths = append(paths, cmpOr(pre, "/")+"*")
		}
		if j.All || (j.Domain != "" && len(j.Paths)+len(j.Patterns)+len(j.Tags) == 0) {
			paths = append(paths, "/*")
		}
		if test {
			method, paths = http.MethodOptions, []string{"/"}
		}
		for _, h := range j.Hosts {
			for _, p := range paths {
				target := base + escapePath(strings.TrimSuffix(p, "*"))
				if strings.HasSuffix(p, "*") {
					target += "*"
				}
				r, err := newReq(method, target, nil, "")
				if err != nil {
					return nil, err
				}
				r.Host = h
				out = append(out, r)
			}
		}
	case WebhookVarnish:
		method := cmpOr(w.Method, "BAN")
		if test {
			method = http.MethodOptions
		}
		r, err := newReq(method, strings.TrimRight(w.URL, "/")+"/", nil, "")
		if err != nil {
			return nil, err
		}
		quoted := make([]string, 0, len(j.Hosts))
		for _, h := range j.Hosts {
			quoted = append(quoted, regexp.QuoteMeta(h))
		}
		r.Header.Set("X-Ban-Host", "^("+strings.Join(quoted, "|")+")$")
		var res []string
		for _, p := range j.Paths {
			res = append(res, "^"+regexp.QuoteMeta(p)+"$")
		}
		for _, p := range j.Patterns {
			res = append(res, s.patternRegex(ctx, p))
		}
		switch {
		case len(j.Tags) > 0:
			qt := make([]string, 0, len(j.Tags))
			for _, t := range j.Tags {
				qt = append(qt, regexp.QuoteMeta(t))
			}
			r.Header.Set("X-Ban-Tags", `(^|[ ,])(`+strings.Join(qt, "|")+`)([ ,]|$)`)
		case len(res) > 0:
			r.Header.Set("X-Ban-Url", strings.Join(res, "|"))
		default:
			r.Header.Set("X-Ban-Url", ".")
		}
		if j.Soft {
			r.Header.Set("X-Ban-Soft", "1")
		}
		out = append(out, r)
	case WebhookGeneric:
		data := PurgeTemplateData{ID: j.InvalidationID, Distribution: j.Distribution, Hosts: j.Hosts, Paths: j.Paths, URLs: urls,
			Tags: j.Tags, All: j.All, Soft: j.Soft, Automatic: j.Automatic, Test: test}
		for _, p := range j.Patterns {
			data.Paths = append(data.Paths, p.Pattern)
		}
		target, err := render(w.URL, data)
		if err != nil {
			return nil, err
		}
		var body []byte
		if w.Body != "" {
			b, err := render(w.Body, data)
			if err != nil {
				return nil, err
			}
			body = []byte(b)
		} else if body, err = json.Marshal(data); err != nil {
			return nil, err
		}
		r, err := newReq(cmpOr(w.Method, http.MethodPost), target, body, "application/json")
		if err != nil {
			return nil, err
		}
		for k, v := range w.Headers {
			// Plain values: the secrets go in the token, never in a
			// free-form header.
			hv, err := render(v, data)
			if err != nil {
				return nil, err
			}
			r.Header.Set(k, hv)
		}
		if token != "" && r.Header.Get("Authorization") == "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		out = append(out, r)
	default:
		return nil, fmt.Errorf("%w: webhook type %q", ErrInvalid, w.Type)
	}
	return out, nil
}

// render executes a template of a generic webhook.
func render(text string, data PurgeTemplateData) (string, error) {
	t, err := template.New("webhook").Option("missingkey=error").Funcs(template.FuncMap{
		"join": strings.Join,
		"json": func(v any) (string, error) {
			b, err := json.Marshal(v)
			return string(b), err
		},
		"urlquery": url.QueryEscape,
	}).Parse(text)
	if err != nil {
		return "", fmt.Errorf("%w: webhook template: %v", ErrInvalid, err)
	}
	var b strings.Builder
	if err := t.Execute(&b, data); err != nil {
		return "", fmt.Errorf("%w: webhook template: %v", ErrInvalid, err)
	}
	return b.String(), nil
}

// edgeGridSign signs an Akamai OPEN API call (EdgeGrid EG1-HMAC-SHA256).
func edgeGridSign(r *http.Request, body []byte, clientToken, accessToken, clientSecret string, now time.Time) {
	ts := now.UTC().Format("20060102T15:04:05+0000")
	var rnd [16]byte
	_, _ = rand.Read(rnd[:])
	nonce := hex.EncodeToString(rnd[:])
	authHeader := "EG1-HMAC-SHA256 client_token=" + clientToken + ";access_token=" + accessToken + ";timestamp=" + ts + ";nonce=" + nonce + ";"
	mac := func(key, data string) string {
		h := hmac.New(sha256.New, []byte(key))
		h.Write([]byte(data))
		return base64.StdEncoding.EncodeToString(h.Sum(nil))
	}
	contentHash := ""
	if r.Method == http.MethodPost && len(body) > 0 {
		sum := sha256.Sum256(body)
		contentHash = base64.StdEncoding.EncodeToString(sum[:])
	}
	pathQuery := r.URL.EscapedPath()
	if r.URL.RawQuery != "" {
		pathQuery += "?" + r.URL.RawQuery
	}
	data := strings.Join([]string{r.Method, r.URL.Scheme, r.URL.Host, pathQuery, "", contentHash, authHeader}, "\t")
	signingKey := mac(clientSecret, ts)
	r.Header.Set("Authorization", authHeader+"signature="+mac(signingKey, data))
}

// validateWebhook checks a webhook definition: its endpoint against the
// egress policy (an internal address literal is refused), its secrets in
// the webhook namespace, no secret reference in its headers.
func (s *service) validateWebhook(w *Webhook) error {
	if !validName(w.ID) {
		return fmt.Errorf("%w: webhook id %q: lowercase letters, digits, '.', '_' and '-'", ErrInvalid, w.ID)
	}
	need := func(field, v string) error {
		if v == "" {
			return fmt.Errorf("%w: a %s webhook needs %s", ErrInvalid, w.Type, field)
		}
		return nil
	}
	checkURL := func(raw string) error {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
			return fmt.Errorf("%w: webhook url %q must be an http(s) URL", ErrInvalid, raw)
		}
		return s.egress.checkURLHost(u.Hostname())
	}
	for field, v := range map[string]string{"token": w.Token, "clientSecret": w.ClientSecret} {
		if name, ref := secretRef(v); ref && !webhookSecretName(name) {
			return fmt.Errorf("%w: webhook %s references ${secret:%s}: the secrets of a webhook are named webhook-<name> (NEXO_SECRET_WEBHOOK_<NAME>)",
				ErrInvalid, field, name)
		}
	}
	switch w.Type {
	case WebhookCloudflare, WebhookFastly:
		if err := need("zone", w.Zone); err != nil {
			return err
		}
		if err := need("token", w.Token); err != nil {
			return err
		}
		if w.URL != "" {
			return checkURL(w.URL)
		}
	case WebhookAkamai:
		for f, v := range map[string]string{"url": w.URL, "token": w.Token, "clientToken": w.ClientToken, "clientSecret": w.ClientSecret} {
			if err := need(f, v); err != nil {
				return err
			}
		}
		if w.Network != "" && w.Network != "production" && w.Network != "staging" {
			return fmt.Errorf("%w: akamai network %q must be production or staging", ErrInvalid, w.Network)
		}
		return checkURL(w.URL)
	case WebhookNginx, WebhookVarnish:
		if err := need("url", w.URL); err != nil {
			return err
		}
		return checkURL(w.URL)
	case WebhookGeneric:
		if err := need("url", w.URL); err != nil {
			return err
		}
		data := PurgeTemplateData{Hosts: []string{"example.com"}, URLs: []string{"https://example.com/"}, Paths: []string{"/"}}
		target, err := render(w.URL, data)
		if err != nil {
			return err
		}
		if err := checkURL(target); err != nil {
			return err
		}
		if w.Body != "" {
			if _, err := render(w.Body, data); err != nil {
				return err
			}
		}
		for _, v := range w.Headers {
			if _, err := render(v, data); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("%w: webhook type %q must be cloudflare, fastly, akamai, nginx, varnish or generic", ErrInvalid, w.Type)
	}
	if w.Scheme != "" && w.Scheme != "http" && w.Scheme != "https" {
		return fmt.Errorf("%w: webhook scheme %q", ErrInvalid, w.Scheme)
	}
	for k, v := range w.Headers {
		if !validToken(k) || strings.ContainsAny(v, "\r\n") {
			return fmt.Errorf("%w: webhook header %q", ErrInvalid, k)
		}
		if strings.Contains(v, "${secret:") {
			return fmt.Errorf("%w: webhook header %q: a secret goes in the token of the webhook, not in a header", ErrInvalid, k)
		}
	}
	return nil
}

// secretRef returns the name of a "${secret:<name>}" reference.
func secretRef(v string) (string, bool) {
	name, ok := strings.CutPrefix(v, "${secret:")
	if !ok || !strings.HasSuffix(name, "}") {
		return "", false
	}
	return strings.TrimSuffix(name, "}"), true
}

// resolveSecret resolves a secret given in clear or as a reference.
func (s *service) resolveSecret(ctx context.Context, v string) string {
	name, ok := secretRef(v)
	if !ok {
		return v
	}
	if s.deps.Secrets == nil {
		return ""
	}
	value, found := s.deps.Secrets.LookupSecret(ctx, name)
	if !found {
		return ""
	}
	return value
}

// webhookSecret resolves a secret of a webhook: in clear, or a reference
// of the webhook namespace (any other reference resolves to nothing, also
// for the configurations saved before the namespace existed).
func (s *service) webhookSecret(ctx context.Context, v string) string {
	if name, ref := secretRef(v); ref && !webhookSecretName(name) {
		s.deps.Logger.WarnContext(ctx, "cache: a webhook references a secret outside of the webhook namespace: not sent", "secret", name)
		return ""
	}
	return s.resolveSecret(ctx, v)
}

// cmpOr returns v, or def when v is empty.
func cmpOr(v, def string) string {
	if v != "" {
		return v
	}
	return def
}
