package admin

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/ilm"
)

// Limits and constants of the lifecycle configurations (S3).
const (
	// maxLifecycleRules is the largest number of rules of a configuration.
	maxLifecycleRules = 1000
	// s3XMLNamespace is the namespace of the S3 documents.
	s3XMLNamespace = "http://s3.amazonaws.com/doc/2006-03-01/"
	// lifecycleDateLayout renders the dates of the rules (midnight UTC).
	lifecycleDateLayout = "2006-01-02"
)

// LifecycleManagerDeps are the dependencies of the lifecycle manager.
type LifecycleManagerDeps struct {
	// Buckets stores the configuration documents (required).
	Buckets bucket.Service
	// Lifecycle parses, validates and applies them (required).
	Lifecycle ilm.Service
	Logger    *slog.Logger
}

// LifecycleManagerOption configures the lifecycle manager.
type LifecycleManagerOption func(*lifecycleManager)

type lifecycleManager struct {
	deps LifecycleManagerDeps
}

var _ LifecycleManager = (*lifecycleManager)(nil)

// NewLifecycleManager returns the lifecycle manager. Missing dependencies
// are a wiring error (domain.ErrInvalidArgument).
func NewLifecycleManager(deps LifecycleManagerDeps, opts ...LifecycleManagerOption) (LifecycleManager, error) {
	if deps.Buckets == nil || deps.Lifecycle == nil {
		return nil, fmt.Errorf("%w: lifecycle manager requires the bucket and lifecycle services", domain.ErrInvalidArgument)
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	m := &lifecycleManager{deps: deps}
	for _, o := range opts {
		o(m)
	}
	return m, nil
}

// Get implements LifecycleManager.
func (m *lifecycleManager) Get(ctx context.Context, name string) (*LifecycleConfig, error) {
	doc, err := m.deps.Buckets.GetLifecycle(ctx, name)
	if errors.Is(err, domain.ErrPolicyNotFound) {
		return &LifecycleConfig{Rules: []LifecycleRule{}}, nil
	}
	if err != nil {
		return nil, err
	}
	cfg, err := m.deps.Lifecycle.Parse(ctx, doc)
	if err != nil {
		return nil, fmt.Errorf("admin: stored lifecycle configuration of %s: %w", name, err)
	}
	return lifecycleFromILM(cfg), nil
}

// Set implements LifecycleManager.
func (m *lifecycleManager) Set(ctx context.Context, name string, cfg LifecycleConfig) (*LifecycleConfig, error) {
	if _, err := m.deps.Buckets.GetVersioning(ctx, name); err != nil {
		return nil, err
	}
	if len(cfg.Rules) == 0 {
		if err := m.deps.Buckets.DeleteLifecycle(ctx, name); err != nil {
			return nil, err
		}
		return &LifecycleConfig{Rules: []LifecycleRule{}}, nil
	}
	if err := m.validate(ctx, cfg); err != nil {
		return nil, err
	}
	doc, err := encodeLifecycle(cfg.Rules)
	if err != nil {
		return nil, err
	}
	canonical, err := m.deps.Lifecycle.Validate(ctx, doc)
	if err != nil {
		return nil, &ConfigError{Kind: "lifecycle", Problems: []RuleProblem{ruleProblem(0, "", err)}}
	}
	if err := m.deps.Buckets.SetLifecycle(ctx, name, canonical); err != nil {
		return nil, err
	}
	stored, err := m.deps.Lifecycle.Parse(ctx, canonical)
	if err != nil {
		return nil, fmt.Errorf("admin: canonical lifecycle configuration of %s: %w", name, err)
	}
	m.deps.Logger.InfoContext(ctx, "admin: lifecycle configuration set", "bucket", name, "rules", len(stored.Rules))
	return lifecycleFromILM(stored), nil
}

// validate checks every rule on its own, then the rule IDs together.
func (m *lifecycleManager) validate(ctx context.Context, cfg LifecycleConfig) error {
	if len(cfg.Rules) > maxLifecycleRules {
		return &ConfigError{Kind: "lifecycle", Problems: []RuleProblem{{Code: "InvalidArgument",
			Message: fmt.Sprintf("a lifecycle configuration cannot have more than %d rules", maxLifecycleRules)}}}
	}
	var problems []RuleProblem
	ids := make([]string, len(cfg.Rules))
	for i, r := range cfg.Rules {
		ids[i] = strings.TrimSpace(r.ID)
		doc, err := encodeLifecycle([]LifecycleRule{r})
		if err != nil {
			return err
		}
		if _, err := m.deps.Lifecycle.Parse(ctx, doc); err != nil {
			problems = append(problems, ruleProblem(i+1, ids[i], err))
		}
	}
	problems = append(problems, duplicateIDs(ids, "rule")...)
	if len(problems) == 0 {
		return nil
	}
	sort.SliceStable(problems, func(i, j int) bool { return problems[i].Rule < problems[j].Rule })
	return &ConfigError{Kind: "lifecycle", Problems: problems}
}

// Delete implements LifecycleManager.
func (m *lifecycleManager) Delete(ctx context.Context, name string) error {
	return m.deps.Buckets.DeleteLifecycle(ctx, name)
}

// Apply implements LifecycleManager.
func (m *lifecycleManager) Apply(ctx context.Context, name string) (ilm.ApplyResult, error) {
	if name == "" {
		return ilm.ApplyResult{}, fmt.Errorf("%w: the bucket is required", domain.ErrInvalidArgument)
	}
	if _, err := m.deps.Buckets.GetLifecycle(ctx, name); err != nil {
		if errors.Is(err, domain.ErrPolicyNotFound) {
			return ilm.ApplyResult{}, nil
		}
		return ilm.ApplyResult{}, err
	}
	res, err := m.deps.Lifecycle.ApplyNow(ctx, name)
	if err != nil {
		return ilm.ApplyResult{}, err
	}
	m.deps.Logger.InfoContext(ctx, "admin: lifecycle applied", "bucket", name, "deleted", res.Deleted,
		"deleteMarkers", res.DeleteMarkers, "uploadsAborted", res.UploadsAborted, "locked", res.Locked)
	return res, nil
}

// --- conversion --------------------------------------------------------------

// lifecycleFromILM returns the console form of a parsed configuration.
func lifecycleFromILM(cfg *ilm.Configuration) *LifecycleConfig {
	out := &LifecycleConfig{Rules: make([]LifecycleRule, 0, len(cfg.Rules))}
	for _, r := range cfg.Rules {
		rule := LifecycleRule{
			ID: r.ID, Enabled: r.Enabled,
			Filter: LifecycleFilter{Prefix: r.Filter.Prefix, SizeGreaterThan: r.Filter.SizeGreaterThan, SizeLessThan: r.Filter.SizeLessThan},
		}
		if len(r.Filter.Tags) > 0 {
			rule.Filter.Tags = make(map[string]string, len(r.Filter.Tags))
			for k, v := range r.Filter.Tags {
				rule.Filter.Tags[k] = v
			}
		}
		if e := r.Expiration; e != nil {
			exp := &LifecycleExpiration{ExpiredObjectDeleteMarker: e.ExpiredObjectDeleteMarker}
			switch {
			case e.Days > 0:
				exp.Days = ptrTo(e.Days)
			case !e.Date.IsZero():
				exp.Date = formatLifecycleDate(e.Date)
			}
			rule.Expiration = exp
		}
		if n := r.NoncurrentExpiration; n != nil {
			rule.NoncurrentExpiration = &LifecycleNoncurrentExpiration{NoncurrentDays: ptrTo(n.NoncurrentDays), NewerNoncurrentVersions: positive(n.NewerNoncurrentVersions)}
		}
		if a := r.AbortIncompleteUpload; a != nil {
			rule.AbortIncompleteUpload = &LifecycleAbortUpload{DaysAfterInitiation: ptrTo(a.DaysAfterInitiation)}
		}
		for _, t := range r.Transitions {
			tr := LifecycleTransition{StorageClass: t.StorageClass}
			if t.Date.IsZero() {
				tr.Days = ptrTo(t.Days)
			} else {
				tr.Date = formatLifecycleDate(t.Date)
			}
			rule.Transitions = append(rule.Transitions, tr)
		}
		for _, t := range r.NoncurrentTransitions {
			rule.NoncurrentTransitions = append(rule.NoncurrentTransitions, LifecycleNoncurrentTransition{
				NoncurrentDays: ptrTo(t.NoncurrentDays), NewerNoncurrentVersions: positive(t.NewerNoncurrentVersions), StorageClass: t.StorageClass,
			})
		}
		out.Rules = append(out.Rules, rule)
	}
	return out
}

// formatLifecycleDate renders the date of a rule (midnight UTC).
func formatLifecycleDate(t time.Time) string { return t.UTC().Format(lifecycleDateLayout) }

// ptrTo returns a pointer to a copy of v.
func ptrTo[T any](v T) *T { return &v }

// positive returns a pointer to n when it is positive, nil otherwise.
func positive(n int) *int {
	if n <= 0 {
		return nil
	}
	return &n
}

// XML form of a LifecycleConfiguration, written element by element from
// the console form so that ilm validates exactly what was sent (a zero
// number stays a zero number).
type lifecycleXML struct {
	XMLName xml.Name           `xml:"LifecycleConfiguration"`
	Xmlns   string             `xml:"xmlns,attr"`
	Rules   []lifecycleRuleXML `xml:"Rule"`
}

type lifecycleRuleXML struct {
	ID                    string                        `xml:"ID,omitempty"`
	Filter                lifecycleFilterXML            `xml:"Filter"`
	Status                string                        `xml:"Status"`
	Expiration            *lifecycleExpirationXML       `xml:"Expiration,omitempty"`
	Transitions           []lifecycleTransitionXML      `xml:"Transition,omitempty"`
	NoncurrentTransitions []lifecycleNoncurrentTransXML `xml:"NoncurrentVersionTransition,omitempty"`
	NoncurrentExpiration  *lifecycleNoncurrentExpXML    `xml:"NoncurrentVersionExpiration,omitempty"`
	AbortIncomplete       *lifecycleAbortXML            `xml:"AbortIncompleteMultipartUpload,omitempty"`
}

type lifecycleFilterXML struct {
	Prefix                *string          `xml:"Prefix,omitempty"`
	Tag                   *lifecycleTagXML `xml:"Tag,omitempty"`
	And                   *lifecycleAndXML `xml:"And,omitempty"`
	ObjectSizeGreaterThan *int64           `xml:"ObjectSizeGreaterThan,omitempty"`
	ObjectSizeLessThan    *int64           `xml:"ObjectSizeLessThan,omitempty"`
}

type lifecycleAndXML struct {
	Prefix                *string           `xml:"Prefix,omitempty"`
	Tags                  []lifecycleTagXML `xml:"Tag,omitempty"`
	ObjectSizeGreaterThan *int64            `xml:"ObjectSizeGreaterThan,omitempty"`
	ObjectSizeLessThan    *int64            `xml:"ObjectSizeLessThan,omitempty"`
}

type lifecycleTagXML struct {
	Key   string `xml:"Key"`
	Value string `xml:"Value"`
}

type lifecycleExpirationXML struct {
	Days                      *int    `xml:"Days,omitempty"`
	Date                      *string `xml:"Date,omitempty"`
	ExpiredObjectDeleteMarker *bool   `xml:"ExpiredObjectDeleteMarker,omitempty"`
}

type lifecycleNoncurrentExpXML struct {
	NoncurrentDays          *int `xml:"NoncurrentDays,omitempty"`
	NewerNoncurrentVersions *int `xml:"NewerNoncurrentVersions,omitempty"`
}

type lifecycleAbortXML struct {
	DaysAfterInitiation *int `xml:"DaysAfterInitiation,omitempty"`
}

type lifecycleTransitionXML struct {
	Days         *int    `xml:"Days,omitempty"`
	Date         *string `xml:"Date,omitempty"`
	StorageClass string  `xml:"StorageClass"`
}

type lifecycleNoncurrentTransXML struct {
	NoncurrentDays          *int   `xml:"NoncurrentDays,omitempty"`
	NewerNoncurrentVersions *int   `xml:"NewerNoncurrentVersions,omitempty"`
	StorageClass            string `xml:"StorageClass"`
}

// encodeLifecycle returns the LifecycleConfiguration document of rules.
func encodeLifecycle(rules []LifecycleRule) ([]byte, error) {
	doc := lifecycleXML{Xmlns: s3XMLNamespace, Rules: make([]lifecycleRuleXML, 0, len(rules))}
	for _, r := range rules {
		rx := lifecycleRuleXML{ID: strings.TrimSpace(r.ID), Status: "Disabled", Filter: encodeLifecycleFilter(r.Filter)}
		if r.Enabled {
			rx.Status = "Enabled"
		}
		if e := r.Expiration; e != nil {
			ex := &lifecycleExpirationXML{Days: e.Days, Date: optString(e.Date)}
			if e.ExpiredObjectDeleteMarker {
				ex.ExpiredObjectDeleteMarker = ptrTo(true)
			}
			rx.Expiration = ex
		}
		if n := r.NoncurrentExpiration; n != nil {
			rx.NoncurrentExpiration = &lifecycleNoncurrentExpXML{NoncurrentDays: n.NoncurrentDays, NewerNoncurrentVersions: n.NewerNoncurrentVersions}
		}
		if a := r.AbortIncompleteUpload; a != nil {
			rx.AbortIncomplete = &lifecycleAbortXML{DaysAfterInitiation: a.DaysAfterInitiation}
		}
		for _, t := range r.Transitions {
			rx.Transitions = append(rx.Transitions, lifecycleTransitionXML{Days: t.Days, Date: optString(t.Date), StorageClass: t.StorageClass})
		}
		for _, t := range r.NoncurrentTransitions {
			rx.NoncurrentTransitions = append(rx.NoncurrentTransitions, lifecycleNoncurrentTransXML(t))
		}
		doc.Rules = append(doc.Rules, rx)
	}
	out, err := xml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("admin: encode lifecycle configuration: %w", err)
	}
	return out, nil
}

// encodeLifecycleFilter returns the simplest Filter element expressing f:
// a single criterion on its own, several in an And.
func encodeLifecycleFilter(f LifecycleFilter) lifecycleFilterXML {
	keys := make([]string, 0, len(f.Tags))
	for k := range f.Tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	tags := make([]lifecycleTagXML, 0, len(keys))
	for _, k := range keys {
		tags = append(tags, lifecycleTagXML{Key: k, Value: f.Tags[k]})
	}
	criteria := len(tags)
	for _, set := range []bool{f.Prefix != "", f.SizeGreaterThan != nil, f.SizeLessThan != nil} {
		if set {
			criteria++
		}
	}
	switch {
	case criteria == 0:
		return lifecycleFilterXML{Prefix: ptrTo("")}
	case criteria > 1:
		return lifecycleFilterXML{And: &lifecycleAndXML{Prefix: optString(f.Prefix), Tags: tags,
			ObjectSizeGreaterThan: f.SizeGreaterThan, ObjectSizeLessThan: f.SizeLessThan}}
	case f.Prefix != "":
		return lifecycleFilterXML{Prefix: ptrTo(f.Prefix)}
	case len(tags) == 1:
		return lifecycleFilterXML{Tag: &tags[0]}
	default:
		return lifecycleFilterXML{ObjectSizeGreaterThan: f.SizeGreaterThan, ObjectSizeLessThan: f.SizeLessThan}
	}
}

// optString returns a pointer to s, nil for "".
func optString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
