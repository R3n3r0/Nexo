package admin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/config"
	"github.com/r3n3r0/nexo/internal/service/event"
)

// NotificationManagerDeps are the dependencies of the notification
// manager.
type NotificationManagerDeps struct {
	// Buckets stores the notification documents (required).
	Buckets bucket.Service
	// Events validates the subscriptions against the configured targets
	// and reports their state (required).
	Events event.Service
	// Config lists the notify_* subsystems, so that the disabled targets
	// are listed too; nil lists the targets of Events only.
	Config config.Service
	Logger *slog.Logger
}

// NotificationManagerOption configures the notification manager.
type NotificationManagerOption func(*notificationManager)

// WithARNRegion sets the region of the target ARNs the manager reports
// (the region of the deployment; default none, which every region
// accepts).
func WithARNRegion(region string) NotificationManagerOption {
	return func(m *notificationManager) { m.region = strings.TrimSpace(region) }
}

type notificationManager struct {
	deps   NotificationManagerDeps
	region string
}

var _ NotificationManager = (*notificationManager)(nil)

// NewNotificationManager returns the notification manager. Missing
// dependencies are a wiring error (domain.ErrInvalidArgument).
func NewNotificationManager(deps NotificationManagerDeps, opts ...NotificationManagerOption) (NotificationManager, error) {
	if deps.Buckets == nil || deps.Events == nil {
		return nil, fmt.Errorf("%w: notification manager requires the bucket and event services", domain.ErrInvalidArgument)
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	m := &notificationManager{deps: deps}
	for _, o := range opts {
		o(m)
	}
	return m, nil
}

// Get implements NotificationManager.
func (m *notificationManager) Get(ctx context.Context, name string) (*NotificationConfig, error) {
	doc, err := m.deps.Buckets.GetNotification(ctx, name)
	if err != nil {
		return nil, err
	}
	cfg, err := event.ParseBucketConfig(doc, "")
	if err != nil {
		return nil, fmt.Errorf("admin: stored notification configuration of %s: %w", name, err)
	}
	return notificationsFromEvent(cfg), nil
}

// Set implements NotificationManager.
func (m *notificationManager) Set(ctx context.Context, name string, cfg NotificationConfig) (*NotificationConfig, error) {
	if _, err := m.deps.Buckets.GetVersioning(ctx, name); err != nil {
		return nil, err
	}
	if len(cfg.Subscriptions) == 0 {
		if err := m.Delete(ctx, name); err != nil {
			return nil, err
		}
		return &NotificationConfig{Subscriptions: []Subscription{}}, nil
	}
	var problems []RuleProblem
	ids := make([]string, len(cfg.Subscriptions))
	rules := make([]event.Rule, 0, len(cfg.Subscriptions))
	for i, s := range cfg.Subscriptions {
		ids[i] = strings.TrimSpace(s.ID)
		rule, err := m.check(ctx, s)
		if err != nil {
			if !isValidation(err) {
				return nil, err
			}
			problems = append(problems, ruleProblem(i+1, ids[i], err))
			continue
		}
		rules = append(rules, rule)
	}
	problems = append(problems, duplicateIDs(ids, "subscription")...)
	if len(problems) > 0 {
		sort.SliceStable(problems, func(i, j int) bool { return problems[i].Rule < problems[j].Rule })
		return nil, &ConfigError{Kind: "notification", Problems: problems}
	}
	doc, err := event.EncodeBucketConfig(&event.BucketConfig{Rules: rules})
	if err != nil {
		return nil, err
	}
	canonical, err := m.deps.Events.ValidateBucketConfig(ctx, doc)
	if err != nil {
		if !isValidation(err) {
			return nil, err
		}
		return nil, &ConfigError{Kind: "notification", Problems: []RuleProblem{ruleProblem(0, "", err)}}
	}
	if err := m.deps.Buckets.SetNotification(ctx, name, canonical); err != nil {
		return nil, err
	}
	stored, err := event.ParseBucketConfig(canonical, "")
	if err != nil {
		return nil, fmt.Errorf("admin: canonical notification configuration of %s: %w", name, err)
	}
	m.deps.Logger.InfoContext(ctx, "admin: bucket notifications set", "bucket", name, "subscriptions", len(stored.Rules))
	return notificationsFromEvent(stored), nil
}

// check validates one subscription on its own, as the S3 API validates a
// configuration: its ARN, events and filters, and its destination (a
// configured target).
func (m *notificationManager) check(ctx context.Context, s Subscription) (event.Rule, error) {
	arn, err := event.ParseARN(strings.TrimSpace(s.ARN))
	if err != nil {
		return event.Rule{}, err
	}
	rule := event.Rule{ID: strings.TrimSpace(s.ID), Kind: event.KindQueue, ARN: arn, Prefix: s.Prefix, Suffix: s.Suffix}
	for _, e := range s.Events {
		rule.Events = append(rule.Events, event.Name(strings.TrimSpace(e)))
	}
	doc, err := event.EncodeBucketConfig(&event.BucketConfig{Rules: []event.Rule{rule}})
	if err != nil {
		return event.Rule{}, err
	}
	if _, err := m.deps.Events.ValidateBucketConfig(ctx, doc); err != nil {
		return event.Rule{}, err
	}
	return rule, nil
}

// isValidation reports an error describing an invalid configuration
// rather than a failure of the service.
func isValidation(err error) bool {
	return errors.Is(err, domain.ErrInvalidArgument) || errors.Is(err, domain.ErrMalformedXML) || errors.Is(err, domain.ErrNotImplemented)
}

// Delete implements NotificationManager.
func (m *notificationManager) Delete(ctx context.Context, name string) error {
	doc, err := event.EncodeBucketConfig(&event.BucketConfig{})
	if err != nil {
		return err
	}
	return m.deps.Buckets.SetNotification(ctx, name, doc)
}

// notificationsFromEvent returns the console form of a parsed
// configuration.
func notificationsFromEvent(cfg *event.BucketConfig) *NotificationConfig {
	out := &NotificationConfig{Subscriptions: make([]Subscription, 0, len(cfg.Rules))}
	for _, r := range cfg.Rules {
		s := Subscription{ID: r.ID, ARN: r.ARN.String(), Events: make([]string, 0, len(r.Events)), Prefix: r.Prefix, Suffix: r.Suffix}
		for _, e := range r.Events {
			s.Events = append(s.Events, string(e))
		}
		out.Subscriptions = append(out.Subscriptions, s)
	}
	return out
}

// Targets implements NotificationManager.
func (m *notificationManager) Targets(ctx context.Context) ([]NotifyTarget, error) {
	statuses, err := m.deps.Events.TargetsStatus(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]NotifyTarget, 0, len(statuses))
	loaded := make(map[event.TargetID]bool, len(statuses))
	for _, st := range statuses {
		loaded[st.Config.TargetID()] = true
		t := m.target(st.Config)
		t.Online, t.Pending, t.LastError = st.Online, st.Pending, st.LastError
		out = append(out, t)
	}
	configured, err := m.configured(ctx)
	if err != nil {
		return nil, err
	}
	for _, cfg := range configured {
		if loaded[cfg.TargetID()] {
			continue
		}
		t := m.target(cfg)
		if cfg.Enabled {
			// Enabled but not (yet) loaded by the dispatcher of this node.
			t.LastError = "the target is not loaded on this node yet"
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ID != out[j].ID {
			return out[i].ID < out[j].ID
		}
		return out[i].Type < out[j].Type
	})
	return out, nil
}

// configured returns the targets of the notify_* subsystems of the
// configuration (none without Config); the invalid ones are skipped.
func (m *notificationManager) configured(ctx context.Context) ([]event.TargetConfig, error) {
	if m.deps.Config == nil {
		return nil, nil
	}
	subsystems, err := m.deps.Config.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []event.TargetConfig
	for _, s := range subsystems {
		if !event.IsTargetSubsystem(s) {
			continue
		}
		kv, err := m.deps.Config.Get(ctx, s)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				continue
			}
			return nil, err
		}
		cfg, err := event.ParseTargetConfig(s, kv)
		if err != nil {
			m.deps.Logger.DebugContext(ctx, "admin: invalid notification target skipped", "subsystem", s, "err", err)
			continue
		}
		out = append(out, cfg)
	}
	return out, nil
}

// target describes a target without its secrets.
func (m *notificationManager) target(cfg event.TargetConfig) NotifyTarget {
	id := cfg.TargetID()
	return NotifyTarget{ID: cfg.ID, Type: cfg.Type, ARN: id.ARN(m.region), Enabled: cfg.Enabled, Endpoint: targetEndpoint(cfg)}
}

// targetEndpoint describes the destination of a target without the
// credentials it may hold (URL user info and query, passwords, the
// PostgreSQL connection string).
func targetEndpoint(cfg event.TargetConfig) string {
	switch cfg.Type {
	case event.TypeWebhook:
		return redactURL(cfg.Webhook.Endpoint)
	case event.TypeKafka:
		return joinNonEmpty(strings.Join(cfg.Kafka.Brokers, ","), "topic", cfg.Kafka.Topic)
	case event.TypeNATS:
		return joinNonEmpty(redactURL(cfg.NATS.Address), "subject", cfg.NATS.Subject)
	case event.TypeAMQP:
		return joinNonEmpty(redactURL(cfg.AMQP.URL), "exchange", cfg.AMQP.Exchange)
	case event.TypeRedis:
		return joinNonEmpty(redactURL(cfg.Redis.Address), "key", cfg.Redis.Key)
	case event.TypePostgres:
		return joinNonEmpty("", "table", cfg.Postgres.Table)
	}
	return ""
}

// joinNonEmpty returns "<base> <label> <value>", leaving out what is empty.
func joinNonEmpty(base, label, value string) string {
	parts := []string{}
	if base != "" {
		parts = append(parts, base)
	}
	if value != "" {
		parts = append(parts, label, value)
	}
	return strings.Join(parts, " ")
}

// redactURL removes the user info, the query and the fragment of a URL
// ("scheme://..."); a host:port is returned as is.
func redactURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		if at := strings.LastIndex(raw, "@"); at >= 0 {
			return raw[at+1:]
		}
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	u.User, u.RawQuery, u.ForceQuery, u.Fragment, u.RawFragment = nil, "", false, "", ""
	return u.String()
}

// TestTarget implements NotificationManager.
func (m *notificationManager) TestTarget(ctx context.Context, target string) (*TargetTest, error) {
	id, err := parseTargetRef(target)
	if err != nil {
		return nil, err
	}
	res, err := m.deps.Events.TestTarget(ctx, id)
	if err != nil {
		return nil, err
	}
	m.deps.Logger.InfoContext(ctx, "admin: notification target tested", "target", id.String(), "delivered", res.Delivered)
	return &TargetTest{Target: id.String(), Delivered: res.Delivered, Error: res.Error}, nil
}

// parseTargetRef parses "<id>:<type>" or the ARN of a target.
func parseTargetRef(s string) (event.TargetID, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "arn:") {
		arn, err := event.ParseARN(s)
		if err != nil {
			return event.TargetID{}, err
		}
		return arn.TargetID(), nil
	}
	i := strings.LastIndex(s, ":")
	if i <= 0 || i == len(s)-1 {
		return event.TargetID{}, fmt.Errorf("%w: a notification target is named <id>:<type> or by its ARN, not %q", domain.ErrInvalidArgument, s)
	}
	return event.TargetID{ID: s[:i], Type: s[i+1:]}, nil
}
