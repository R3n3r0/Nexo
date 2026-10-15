package app

import (
	"log/slog"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/config"
	"github.com/r3n3r0/nexo/internal/service/accessrule"
	"github.com/r3n3r0/nexo/internal/service/audit"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	configsvc "github.com/r3n3r0/nexo/internal/service/config"
	"github.com/r3n3r0/nexo/internal/service/match"
	"github.com/r3n3r0/nexo/internal/service/policy"
)

// accessWiring holds what the access rules (docs/10 §10.1) share between
// the evaluator the policy engine consults and the service managing
// them: the matcher of the key patterns and the options of the
// configuration.
type accessWiring struct {
	matcher match.Service
	eval    accessrule.Evaluator
	opts    []accessrule.Option
}

// newAccessEvaluator builds the evaluator of the access rules, which reads
// the rules from the bucket metadata store: their changes on any node
// reach this one with the invalidation of the bucket metadata.
func newAccessEvaluator(cfg config.Static, buckets bucket.MetadataStore, dyn configsvc.Service, clk clock.Clock, logger *slog.Logger) (accessWiring, error) {
	w := accessWiring{
		matcher: match.New(match.Deps{}, match.WithNFC(cfg.KeyNormalization != config.KeyNormalizationNone)),
		opts: []accessrule.Option{
			accessrule.WithMaxRules(cfg.AccessRulesMax),
			accessrule.WithBlockPublicDefault(cfg.BlockPublicAccess, accessrule.BlockPublicMode(cfg.BlockPublicAccessMode)),
		},
	}
	var err error
	w.eval, err = accessrule.NewEvaluator(accessrule.EvaluatorDeps{
		Source: buckets, Matcher: w.matcher, Config: dyn, Secrets: accessrule.NewStaticSecrets(cfg.Secrets),
		Clock: clk, Logger: logger.With("component", "accessrule"),
	}, w.opts...)
	return w, err
}

// service builds the service managing the access rules; source is the
// cache of the bucket metadata its block public access is read from.
func (w accessWiring) service(buckets bucket.Service, source accessrule.ConfigSource, policies policy.Service, dyn configsvc.Service, auditor audit.Service, clk clock.Clock, logger *slog.Logger) (accessrule.Service, error) {
	return accessrule.New(accessrule.Deps{
		Buckets: buckets, Source: source, Policy: policies, Matcher: w.matcher, Config: dyn, Audit: auditor,
		Clock: clk, Logger: logger.With("component", "accessrule"),
	}, w.opts...)
}
