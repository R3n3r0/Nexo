package admin

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/bucket"
)

// CORSManagerDeps are the dependencies of the CORS manager.
type CORSManagerDeps struct {
	// Buckets stores and parses the CORS documents (required).
	Buckets bucket.Service
	Logger  *slog.Logger
}

// CORSManagerOption configures the CORS manager.
type CORSManagerOption func(*corsManager)

type corsManager struct {
	deps CORSManagerDeps
}

var _ CORSManager = (*corsManager)(nil)

// NewCORSManager returns the CORS manager. Missing dependencies are a
// wiring error (domain.ErrInvalidArgument).
func NewCORSManager(deps CORSManagerDeps, opts ...CORSManagerOption) (CORSManager, error) {
	if deps.Buckets == nil {
		return nil, fmt.Errorf("%w: CORS manager requires the bucket service", domain.ErrInvalidArgument)
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	m := &corsManager{deps: deps}
	for _, o := range opts {
		o(m)
	}
	return m, nil
}

// Get implements CORSManager.
func (m *corsManager) Get(ctx context.Context, name string) (*CORSConfig, error) {
	cfg, err := m.deps.Buckets.GetCORSConfig(ctx, name)
	if errors.Is(err, domain.ErrPolicyNotFound) {
		return &CORSConfig{Rules: []domain.CORSRule{}}, nil
	}
	if err != nil {
		return nil, err
	}
	return &CORSConfig{Rules: nonNilRules(cfg.Rules)}, nil
}

// Set implements CORSManager.
func (m *corsManager) Set(ctx context.Context, name string, cfg CORSConfig) (*CORSConfig, error) {
	if _, err := m.deps.Buckets.GetVersioning(ctx, name); err != nil {
		return nil, err
	}
	if len(cfg.Rules) == 0 {
		if err := m.deps.Buckets.DeleteCORS(ctx, name); err != nil {
			return nil, err
		}
		return &CORSConfig{Rules: []domain.CORSRule{}}, nil
	}
	if len(cfg.Rules) > domain.MaxCORSRules {
		return nil, &ConfigError{Kind: "cors", Problems: []RuleProblem{{Code: "InvalidRequest",
			Message: fmt.Sprintf("a CORS configuration must have at most %d rules", domain.MaxCORSRules)}}}
	}
	rules := make([]domain.CORSRule, len(cfg.Rules))
	var problems []RuleProblem
	for i, r := range cfg.Rules {
		rules[i] = trimCORSRule(r)
		if err := (domain.CORSConfiguration{Rules: rules[i : i+1]}).Validate(); err != nil {
			p := ruleProblem(i+1, rules[i].ID, err)
			// The rule validated alone is "CORSRule 1": the problem
			// carries its index.
			p.Message = strings.Replace(strings.Replace(p.Message, "CORSRule 1: ", "", 1), "CORSRule 1 ", "the rule ", 1)
			problems = append(problems, p)
		}
	}
	if len(problems) > 0 {
		return nil, &ConfigError{Kind: "cors", Problems: problems}
	}
	doc, err := encodeCORS(rules)
	if err != nil {
		return nil, err
	}
	if err := m.deps.Buckets.SetCORS(ctx, name, doc); err != nil {
		if isValidation(err) {
			return nil, &ConfigError{Kind: "cors", Problems: []RuleProblem{ruleProblem(0, "", err)}}
		}
		return nil, err
	}
	m.deps.Logger.InfoContext(ctx, "admin: CORS configuration set", "bucket", name, "rules", len(rules))
	return m.Get(ctx, name)
}

// Delete implements CORSManager.
func (m *corsManager) Delete(ctx context.Context, name string) error {
	return m.deps.Buckets.DeleteCORS(ctx, name)
}

// trimCORSRule trims the values of a rule, as the bucket service does
// when it parses the stored document.
func trimCORSRule(r domain.CORSRule) domain.CORSRule {
	trim := func(values []string) []string {
		if len(values) == 0 {
			return nil
		}
		out := make([]string, len(values))
		for i, v := range values {
			out[i] = strings.TrimSpace(v)
		}
		return out
	}
	return domain.CORSRule{
		ID: strings.TrimSpace(r.ID), AllowedOrigins: trim(r.AllowedOrigins), AllowedMethods: trim(r.AllowedMethods),
		AllowedHeaders: trim(r.AllowedHeaders), ExposeHeaders: trim(r.ExposeHeaders), MaxAgeSeconds: r.MaxAgeSeconds,
	}
}

// nonNilRules returns rules, or an empty slice for nil.
func nonNilRules(rules []domain.CORSRule) []domain.CORSRule {
	if rules == nil {
		return []domain.CORSRule{}
	}
	return rules
}

// corsXML is the S3 CORSConfiguration document.
type corsXML struct {
	XMLName xml.Name      `xml:"CORSConfiguration"`
	Xmlns   string        `xml:"xmlns,attr"`
	Rules   []corsRuleXML `xml:"CORSRule"`
}

type corsRuleXML struct {
	ID             string   `xml:"ID,omitempty"`
	AllowedHeaders []string `xml:"AllowedHeader"`
	AllowedMethods []string `xml:"AllowedMethod"`
	AllowedOrigins []string `xml:"AllowedOrigin"`
	ExposeHeaders  []string `xml:"ExposeHeader"`
	MaxAgeSeconds  *int     `xml:"MaxAgeSeconds,omitempty"`
}

// encodeCORS returns the CORSConfiguration document of rules.
func encodeCORS(rules []domain.CORSRule) ([]byte, error) {
	doc := corsXML{Xmlns: s3XMLNamespace, Rules: make([]corsRuleXML, 0, len(rules))}
	for _, r := range rules {
		doc.Rules = append(doc.Rules, corsRuleXML{
			ID: r.ID, AllowedHeaders: r.AllowedHeaders, AllowedMethods: r.AllowedMethods, AllowedOrigins: r.AllowedOrigins,
			ExposeHeaders: r.ExposeHeaders, MaxAgeSeconds: r.MaxAgeSeconds,
		})
	}
	out, err := xml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("admin: encode CORS configuration: %w", err)
	}
	return out, nil
}
