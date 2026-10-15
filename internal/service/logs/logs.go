package logs

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
)

// Deps are the dependencies of the log stream.
type Deps struct {
	// Output is where the server writes its logs: the service builds the
	// JSON or text handler on it and switches the format at runtime.
	Output io.Writer
	// Base is the handler the records go to when Output is nil (an
	// embedding application, the tests): the format is then the one of
	// Base, only the level changes at runtime. Without both the records
	// are only kept and streamed.
	Base slog.Handler
	// NodeID names this node in the entries.
	NodeID string
}

// Option configures the log stream.
type Option func(*options)

type options struct {
	level    slog.Level
	format   string
	ring     int
	buffer   int
	retryMin time.Duration
	retryMax time.Duration
}

// WithLevel sets the level the server starts with (debug, info, warn,
// error; default info): the "logger" subsystem overrides it at runtime,
// removing the setting restores it.
func WithLevel(level string) Option {
	return func(o *options) {
		if l, err := parseLevel(level); err == nil && level != "" {
			o.level = l
		}
	}
}

// WithFormat sets the format the server starts with (json or text;
// default json).
func WithFormat(format string) Option {
	return func(o *options) {
		if f := strings.ToLower(strings.TrimSpace(format)); f == formatJSON || f == formatText {
			o.format = f
		}
	}
}

// WithRingSize sets how many recent entries the node keeps for the replay
// of a new subscription (default 2000).
func WithRingSize(n int) Option {
	return func(o *options) {
		if n > 0 {
			o.ring = n
		}
	}
}

// WithBuffer sets the entries a subscriber may lag behind before it loses
// them (default 1024).
func WithBuffer(n int) Option {
	return func(o *options) {
		if n > 0 {
			o.buffer = n
		}
	}
}

// WithReconnect sets the delays before a broken stream of a peer is
// opened again: min doubles up to max (default 1s to 30s).
func WithReconnect(minDelay, maxDelay time.Duration) Option {
	return func(o *options) {
		if minDelay > 0 {
			o.retryMin = minDelay
		}
		if maxDelay >= o.retryMin {
			o.retryMax = maxDelay
		}
	}
}

// Formats of the output.
const (
	formatJSON = "json"
	formatText = "text"
)

// maxLast bounds the replay of a subscription.
const maxLast = 10000

// output is the handler of the output, numbered so that the derived
// handlers (WithAttrs, WithGroup) notice a format change.
type output struct {
	gen     uint64
	handler slog.Handler
}

type service struct {
	deps Deps
	opts options

	level  slog.LevelVar
	out    atomic.Pointer[output]
	format atomic.Value // string
	redact *redactor

	mu   sync.RWMutex
	ring []Entry
	next int
	full bool
	subs map[*subscriber]struct{}
}

var _ Service = (*service)(nil)

// New returns the log stream of the node.
func New(deps Deps, opts ...Option) (Service, error) {
	if deps.NodeID == "" {
		deps.NodeID = "local"
	}
	o := options{level: slog.LevelInfo, format: formatJSON, ring: 2000, buffer: 1024, retryMin: time.Second, retryMax: 30 * time.Second}
	for _, opt := range opts {
		opt(&o)
	}
	// The ring grows with the entries up to its size.
	s := &service{deps: deps, opts: o, subs: map[*subscriber]struct{}{}, redact: newRedactor()}
	s.level.Set(o.level)
	s.format.Store(o.format)
	s.out.Store(&output{handler: s.newHandler(o.format)})
	return s, nil
}

// newHandler returns the handler of the output in format.
func (s *service) newHandler(format string) slog.Handler {
	switch {
	case s.deps.Output == nil && s.deps.Base != nil:
		return s.deps.Base
	case s.deps.Output == nil:
		return nil
	case format == formatText:
		return slog.NewTextHandler(s.deps.Output, &slog.HandlerOptions{Level: &s.level})
	default:
		return slog.NewJSONHandler(s.deps.Output, &slog.HandlerOptions{Level: &s.level})
	}
}

// Handler implements Service.
func (s *service) Handler() slog.Handler { return &tee{s: s} }

// Configure implements Service.
func (s *service) Configure(ctx context.Context, set Settings) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	level := s.opts.level
	if set.Level != "" {
		l, err := parseLevel(set.Level)
		if err != nil {
			return err
		}
		level = l
	}
	format := s.opts.format
	if set.Format != "" {
		format = strings.ToLower(strings.TrimSpace(set.Format))
		if format != formatJSON && format != formatText {
			return fmt.Errorf("%w: logs: unknown format %q (json or text)", domain.ErrInvalidArgument, set.Format)
		}
	}
	s.level.Set(level)
	if s.deps.Output != nil && s.format.Load() != format {
		s.format.Store(format)
		cur := s.out.Load()
		s.out.Store(&output{gen: cur.gen + 1, handler: s.newHandler(format)})
	}
	return nil
}

// parseLevel parses a level name.
func parseLevel(name string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("%w: logs: unknown level %q (debug, info, warn, error)", domain.ErrInvalidArgument, name)
}

// record keeps an entry and delivers it to the subscribers.
func (s *service) record(e Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.full {
		s.ring[s.next] = e
		s.next = (s.next + 1) % len(s.ring)
	} else {
		s.ring = append(s.ring, e)
		s.next = len(s.ring)
		if s.next == s.opts.ring {
			s.next, s.full = 0, true
		}
	}
	// The sends never block: a subscriber that does not keep up loses
	// entries (counted).
	for sub := range s.subs {
		if sub.filter.match(e) {
			sub.send(e)
		}
	}
}

// recent returns the last n entries matching f, oldest first (s.mu held).
func (s *service) recent(f filter, n int) []Entry {
	var all []Entry
	if s.full {
		all = append(append(all, s.ring[s.next:]...), s.ring[:s.next]...)
	} else {
		all = append(all, s.ring[:s.next]...)
	}
	var out []Entry
	for i := len(all) - 1; i >= 0 && len(out) < n; i-- {
		if f.match(all[i]) {
			out = append(out, all[i])
		}
	}
	slices.Reverse(out)
	return out
}

// Subscribe implements Service: the entries of this node (the cluster
// view of NewCluster adds the ones of the peers).
func (s *service) Subscribe(ctx context.Context, f Filter) (Subscription, error) {
	cf, err := compile(f)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sub := &subscriber{filter: cf, ch: make(chan Entry, max(s.opts.buffer, f.Last))}
	s.mu.Lock()
	if f.Last > 0 && cf.wants(s.deps.NodeID) {
		for _, e := range s.recent(cf, f.Last) {
			sub.send(e)
		}
	}
	s.subs[sub] = struct{}{}
	s.mu.Unlock()
	context.AfterFunc(ctx, func() {
		s.mu.Lock()
		delete(s.subs, sub)
		s.mu.Unlock()
		sub.close()
	})
	return sub, nil
}

// subscriber is a subscription: a bounded buffer, closed once.
type subscriber struct {
	filter  filter
	ch      chan Entry
	dropped atomic.Uint64

	mu     sync.Mutex
	closed bool
}

var _ Subscription = (*subscriber)(nil)

// Entries implements Subscription.
func (s *subscriber) Entries() <-chan Entry { return s.ch }

// Dropped implements Subscription.
func (s *subscriber) Dropped() uint64 { return s.dropped.Load() }

// send queues e without blocking (a full buffer drops it, counted).
func (s *subscriber) send(e Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.ch <- e:
	default:
		s.dropped.Add(1)
	}
}

func (s *subscriber) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.ch)
	}
}

// filter is a compiled Filter.
type filter struct {
	nodes map[string]bool
	level slog.Level
}

func compile(f Filter) (filter, error) {
	out := filter{level: slog.LevelDebug}
	if f.Level != "" {
		l, err := parseLevel(f.Level)
		if err != nil {
			return out, err
		}
		out.level = l
	}
	if f.Last < 0 || f.Last > maxLast {
		return out, fmt.Errorf("%w: logs: the replay is between 0 and %d entries", domain.ErrInvalidArgument, maxLast)
	}
	if len(f.Nodes) > 0 {
		out.nodes = map[string]bool{}
		for _, n := range f.Nodes {
			out.nodes[n] = true
		}
	}
	return out, nil
}

// wants reports whether the entries of node are selected.
func (f filter) wants(node string) bool { return f.nodes == nil || f.nodes[node] }

// match reports whether e passes the filter.
func (f filter) match(e Entry) bool {
	if !f.wants(e.Node) {
		return false
	}
	l, err := parseLevel(e.Level)
	return err != nil || l >= f.level
}

// tee is the handler of the process logger: it writes the records to the
// output and records them into the service. ops are the attributes and
// groups added with WithAttrs and WithGroup, in order.
type tee struct {
	s   *service
	ops []op
	// prefix is the group path of the record attributes, attrs the
	// flattened attributes of ops.
	prefix string
	attrs  []kv
	cache  atomic.Pointer[output]
}

// op is a WithAttrs or WithGroup call.
type op struct {
	group string
	attrs []slog.Attr
}

type kv struct{ key, value string }

var _ slog.Handler = (*tee)(nil)

// Enabled implements slog.Handler: the level of the output.
func (t *tee) Enabled(_ context.Context, l slog.Level) bool { return l >= t.s.level.Level() }

// Handle implements slog.Handler.
func (t *tee) Handle(ctx context.Context, r slog.Record) error {
	var err error
	if h := t.handler(); h != nil {
		err = h.Handle(ctx, r)
	}
	t.s.record(t.entry(r))
	return err
}

// handler returns the output handler with the attributes and groups of
// t, rebuilt after a format change.
func (t *tee) handler() slog.Handler {
	cur := t.s.out.Load()
	if c := t.cache.Load(); c != nil && c.gen == cur.gen {
		return c.handler
	}
	h := cur.handler
	if h == nil {
		return nil
	}
	for _, o := range t.ops {
		if o.group != "" {
			h = h.WithGroup(o.group)
		} else {
			h = h.WithAttrs(o.attrs)
		}
	}
	t.cache.Store(&output{gen: cur.gen, handler: h})
	return h
}

// WithAttrs implements slog.Handler.
func (t *tee) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return t
	}
	out := &tee{s: t.s, ops: append(slices.Clip(t.ops), op{attrs: attrs}), prefix: t.prefix, attrs: slices.Clip(t.attrs)}
	for _, a := range attrs {
		out.attrs = flatten(out.attrs, t.prefix, a)
	}
	return out
}

// WithGroup implements slog.Handler.
func (t *tee) WithGroup(name string) slog.Handler {
	if name == "" {
		return t
	}
	return &tee{s: t.s, ops: append(slices.Clip(t.ops), op{group: name}), prefix: t.prefix + name + ".", attrs: t.attrs}
}

// entry converts a record, its attributes redacted.
func (t *tee) entry(r slog.Record) Entry {
	all := slices.Clone(t.attrs)
	r.Attrs(func(a slog.Attr) bool {
		all = flatten(all, t.prefix, a)
		return true
	})
	attrs := make(map[string]string, len(all))
	for _, a := range all {
		attrs[a.key] = a.value
	}
	return Entry{
		Time: r.Time, Node: t.s.deps.NodeID, Level: r.Level.String(),
		Message: t.s.redact.value(r.Message), Attrs: t.s.redact.attrs(attrs),
	}
}

// flatten appends the attribute a (groups expanded with dots).
func flatten(dst []kv, prefix string, a slog.Attr) []kv {
	v := a.Value.Resolve()
	if v.Kind() == slog.KindGroup {
		p := prefix
		if a.Key != "" {
			p += a.Key + "."
		}
		for _, inner := range v.Group() {
			dst = flatten(dst, p, inner)
		}
		return dst
	}
	if a.Key == "" {
		return dst
	}
	return append(dst, kv{key: prefix + a.Key, value: v.String()})
}

// redactor removes the secrets of the entries.
type redactor struct {
	query  *regexp.Regexp
	bearer *regexp.Regexp
}

func newRedactor() *redactor {
	return &redactor{
		query:  regexp.MustCompile(`(?i)((?:x-amz-signature|x-amz-credential|x-amz-security-token|signature|awsaccesskeyid|key-pair-id|token|access_token)=)[^&\s"']+`),
		bearer: regexp.MustCompile(`(?i)\b(bearer|basic|aws4-hmac-sha256)\s+[^\s"']+`),
	}
}

// sensitiveKey reports the attribute names whose value is a secret.
func sensitiveKey(key string) bool {
	k := strings.ToLower(key)
	if i := strings.LastIndexByte(k, '.'); i >= 0 {
		k = k[i+1:]
	}
	for _, s := range []string{"authorization", "password", "passwd", "secret", "token", "signature", "credential", "cookie",
		"customer-key", "customer_key", "private_key", "privatekey", "sse_c", "ssec"} {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}

// redactedValue replaces a secret.
const redactedValue = "REDACTED"

// value redacts the signatures, credentials and authorization schemes
// found in a text.
func (r *redactor) value(v string) string {
	// Both patterns need a "=" or a blank: most values (numbers, names,
	// durations) skip the regular expressions.
	if !strings.ContainsAny(v, "= \t\r\n") {
		return v
	}
	v = r.query.ReplaceAllString(v, "${1}"+redactedValue)
	return r.bearer.ReplaceAllString(v, "${1} "+redactedValue)
}

// attrs redacts the attributes in place.
func (r *redactor) attrs(m map[string]string) map[string]string {
	for k, v := range m {
		if sensitiveKey(k) {
			if v != "" {
				m[k] = redactedValue
			}
			continue
		}
		m[k] = r.value(v)
	}
	return m
}
