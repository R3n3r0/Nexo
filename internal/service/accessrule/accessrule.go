package accessrule

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/config"
	"github.com/r3n3r0/nexo/internal/service/policy"
)

// presetAttempts bounds the retries of ApplyPreset when the rules change
// concurrently (and the caller gave no If-Match of its own).
const presetAttempts = 3

// service is the default Service.
type service struct {
	deps Deps
	opts options
	comp *compiler
}

var _ Service = (*service)(nil)

// New returns the access rules service. Buckets, Policy and Matcher are
// required (a missing one is a wiring error, domain.ErrInvalidArgument).
func New(deps Deps, opts ...Option) (Service, error) {
	switch {
	case deps.Buckets == nil:
		return nil, fmt.Errorf("%w: accessrule: the Buckets dependency is required", domain.ErrInvalidArgument)
	case deps.Policy == nil:
		return nil, fmt.Errorf("%w: accessrule: the Policy dependency is required", domain.ErrInvalidArgument)
	case deps.Matcher == nil:
		return nil, fmt.Errorf("%w: accessrule: the Matcher dependency is required", domain.ErrInvalidArgument)
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &service{deps: deps, opts: newOptions(opts), comp: newCompiler(deps.Matcher)}, nil
}

// --- rules ---------------------------------------------------------------

// Get implements Service.
func (s *service) Get(ctx context.Context, name string) (*Config, error) {
	stored, err := s.deps.Buckets.GetAccessRules(ctx, name)
	if err != nil {
		return nil, err
	}
	rules, err := decodeRules(stored.Document)
	if err != nil {
		return nil, fmt.Errorf("accessrule: stored rules of %s: %w", name, err)
	}
	return s.config(ctx, name, stored, rules)
}

// Put implements Service.
func (s *service) Put(ctx context.Context, in PutInput) (*Config, error) {
	rules, err := s.validate(ctx, in.Bucket, in.Rules)
	if err != nil {
		return nil, err
	}
	doc, err := json.Marshal(rules)
	if err != nil {
		return nil, fmt.Errorf("accessrule: encode the rules of %s: %w", in.Bucket, err)
	}
	change, err := s.deps.Buckets.SetAccessRules(ctx, in.Bucket, bucket.SetAccessRulesInput{Document: doc, IfMatch: in.IfMatch, UpdatedBy: in.Actor})
	if err != nil {
		return nil, err
	}
	previous, err := decodeRules(change.Previous.Document)
	if err != nil {
		previous = nil
	}
	s.audit(ctx, "PutAccessRules", in.Bucket, in.Actor, diffRules(previous, rules))
	return s.config(ctx, in.Bucket, change.Current, rules)
}

// Parse implements Service.
func (s *service) Parse(ctx context.Context, data []byte) (*Document, error) {
	doc := &Document{}
	if len(bytes.TrimSpace(data)) == 0 {
		doc.Rules = []Rule{}
		return doc, nil
	}
	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		return nil, ruleError(-1, "", "", "%v", err)
	}
	root := &node
	if root.Kind == yaml.DocumentNode && len(root.Content) == 1 {
		root = root.Content[0]
	}
	var err error
	switch root.Kind {
	case yaml.SequenceNode:
		err = decodeStrict(data, &doc.Rules)
	case yaml.MappingNode:
		err = decodeStrict(data, doc)
	default:
		err = errors.New("a document of rules is a list of rules or a mapping with bucket and rules")
	}
	if err != nil {
		return nil, ruleError(-1, "", "", "%v", err)
	}
	if len(doc.Rules) > s.opts.maxRules {
		return nil, s.tooMany(len(doc.Rules))
	}
	rules, _, err := s.comp.normalizeRules(ctx, doc.Rules)
	if err != nil {
		return nil, err
	}
	if rules == nil {
		rules = []Rule{}
	}
	doc.Rules = rules
	return doc, nil
}

// decodeStrict decodes YAML (or JSON) rejecting the unknown fields.
func decodeStrict(data []byte, v any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	return dec.Decode(v)
}

// Validate implements Service.
func (s *service) Validate(ctx context.Context, name string, rules []Rule) error {
	_, err := s.validate(ctx, name, rules)
	return err
}

// validate checks the rules of a bucket and returns them in canonical
// form: the count, every rule, and the block public access in the reject
// mode.
func (s *service) validate(ctx context.Context, name string, rules []Rule) ([]Rule, error) {
	if err := domain.ValidateBucketName(name); err != nil {
		return nil, err
	}
	if len(rules) > s.opts.maxRules {
		return nil, s.tooMany(len(rules))
	}
	out, compiled, err := s.comp.normalizeRules(ctx, rules)
	if err != nil {
		return nil, err
	}
	public := -1
	for _, cr := range compiled {
		if cr.public() {
			public = cr.index
			break
		}
	}
	if public >= 0 {
		bp, err := s.GetBlockPublic(ctx, name)
		if err != nil {
			return nil, err
		}
		if bp.Effective && bp.Mode == BlockModeReject {
			re := ruleError(public, out[public].ID, "principals", "public access is blocked (%s): a rule cannot allow everyone (\"*\")", blockLevel(bp))
			re.err = ErrPublicAccessBlocked
			return nil, re
		}
	}
	if out == nil {
		out = []Rule{}
	}
	return out, nil
}

// tooMany is the error of a list of rules over the limit.
func (s *service) tooMany(n int) error {
	re := ruleError(-1, "", "rules", "%d rules, at most %d per bucket", n, s.opts.maxRules)
	re.err = ErrTooManyRules
	return re
}

// blockLevel names the level of a block public access in effect.
func blockLevel(bp *BlockPublic) string {
	if bp.Cluster {
		return "cluster"
	}
	return "bucket"
}

// config builds the Config of stored rules, with the public state and
// the warnings.
func (s *service) config(ctx context.Context, name string, stored bucket.AccessRules, rules []Rule) (*Config, error) {
	bp, err := s.GetBlockPublic(ctx, name)
	if err != nil {
		return nil, err
	}
	out := &Config{
		Bucket: name, Rules: rules, ETag: stored.ETag, UpdatedAt: stored.UpdatedAt, UpdatedBy: stored.UpdatedBy,
		BlockPublic: bp.Effective,
	}
	if out.Rules == nil {
		out.Rules = []Rule{}
	}
	classes := actionClasses()
	for _, r := range rules {
		if r.Effect != EffectAllow || !contains(r.Principals, PrincipalEveryone) {
			continue
		}
		if bp.Effective {
			out.Warnings = append(out.Warnings, fmt.Sprintf("public access is blocked: the principal \"*\" of rule %q is ignored", r.ID))
			continue
		}
		out.Public = true
		if grantsWrite(r.Actions, classes) {
			out.Warnings = append(out.Warnings, fmt.Sprintf("rule %q lets everyone write or delete objects", r.ID))
		}
	}
	return out, nil
}

// grantsWrite reports actions that write or delete objects.
func grantsWrite(actions []string, classes map[string][]policy.Action) bool {
	for _, a := range actions {
		la := strings.ToLower(a)
		if la == ActionWrite || la == ActionDelete || la == ActionAll {
			return true
		}
		if _, isClass := classes[la]; isClass {
			continue
		}
		for _, w := range []string{"s3:putobject", "s3:deleteobject"} {
			if wildcard(la, w) {
				return true
			}
		}
	}
	return false
}

// decodeRules decodes a stored document of rules ("[]" or empty: none).
func decodeRules(doc []byte) ([]Rule, error) {
	if len(bytes.TrimSpace(doc)) == 0 {
		return []Rule{}, nil
	}
	var rules []Rule
	if err := json.Unmarshal(doc, &rules); err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrInvalidFormat, err)
	}
	if rules == nil {
		rules = []Rule{}
	}
	return rules, nil
}

// --- tester --------------------------------------------------------------

// Test implements Service.
func (s *service) Test(ctx context.Context, in TestInput) (*TestResult, error) {
	if err := domain.ValidateBucketName(in.Bucket); err != nil {
		return nil, err
	}
	action, err := testAction(in.Action)
	if err != nil {
		return nil, err
	}
	bp, err := s.GetBlockPublic(ctx, in.Bucket)
	if err != nil {
		return nil, err
	}
	id := in.Identity
	if id == nil {
		id = domain.Anonymous()
	}
	now := in.Now
	if now.IsZero() {
		now = s.deps.Clock.Now()
	}
	req := policy.Request{Host: in.Host, Method: in.Method, Distribution: in.Distribution}
	if in.IP != "" || in.Secure != nil || in.Referer != "" {
		req.Client = &policy.RequestClient{IP: in.IP, Secure: in.Secure != nil && *in.Secure, Referer: in.Referer}
	}
	if len(in.Headers) > 0 {
		req.HTTPHeaders = make(map[string][]string, len(in.Headers))
		for k, v := range in.Headers {
			req.HTTPHeaders[k] = []string{v}
		}
	}
	base := policy.RequestConditions(req)
	if in.Secure == nil {
		delete(base, policy.KeySecureTransport)
	}
	keys := in.Keys
	if len(keys) == 0 {
		keys = []string{""}
	}
	bucketLevel := isBucketAction(action)
	out := &TestResult{Bucket: in.Bucket, Action: string(action), BlockPublic: *bp, Results: make([]KeyResult, 0, len(keys))}
	for _, key := range keys {
		args := policy.Args{Identity: id, Action: action, Bucket: in.Bucket, Conditions: base, Now: now}
		if bucketLevel {
			conds := make(map[string][]string, len(base)+1)
			for k, v := range base {
				conds[k] = v
			}
			conds[policy.KeyPrefix] = []string{key}
			args.Conditions = conds
		} else {
			args.Object = key
		}
		if !bucketLevel && key == "" {
			return nil, fmt.Errorf("%w: the action %s needs a key", domain.ErrInvalidArgument, action)
		}
		d, err := s.deps.Policy.Simulate(ctx, args)
		if err != nil {
			return nil, err
		}
		out.Results = append(out.Results, keyResult(key, s.deps.Matcher.Normalize(ctx, key), d))
	}
	return out, nil
}

// keyResult summarizes the decision of one key.
func keyResult(key, normalized string, d policy.Decision) KeyResult {
	kr := KeyResult{
		Key: key, NormalizedKey: normalized, Rules: d.EvaluatedRules, Matched: []string{}, RulesOutcome: OutcomeNone,
		Allowed: d.Allowed, Source: d.Source, Reason: d.Reason,
	}
	if kr.Rules == nil {
		kr.Rules = []policy.EvaluatedRule{}
	}
	for _, ev := range d.EvaluatedRules {
		if ev.Mismatch == policy.RuleMismatchBlockedPublic {
			kr.BlockedPublic = append(kr.BlockedPublic, ev.ID)
		}
		if !ev.Matched {
			continue
		}
		kr.Matched = append(kr.Matched, ev.ID)
		switch {
		case ev.Effect == domain.Deny:
			kr.RulesOutcome = OutcomeDeny
		case kr.RulesOutcome == OutcomeNone:
			kr.RulesOutcome = OutcomeAllow
		}
	}
	if d.Rule != nil {
		kr.Determinant = d.Rule.ID
	}
	return kr
}

// testAction resolves the action of a test: an S3 action or a class.
func testAction(a string) (policy.Action, error) {
	switch strings.ToLower(strings.TrimSpace(a)) {
	case "", ActionRead:
		return policy.ActionGetObject, nil
	case ActionList:
		return policy.ActionListBucket, nil
	case ActionWrite:
		return policy.ActionPutObject, nil
	case ActionDelete:
		return policy.ActionDeleteObject, nil
	case ActionTag:
		return policy.ActionPutObjectTagging, nil
	}
	if !strings.HasPrefix(strings.ToLower(a), "s3:") || strings.ContainsAny(a, "*? ") {
		return "", fmt.Errorf("%w: %q is not an S3 action nor an action class", domain.ErrInvalidArgument, a)
	}
	return policy.Action(strings.TrimSpace(a)), nil
}

// isBucketAction reports the actions on the bucket rather than on an
// object (their key is the listing prefix).
func isBucketAction(a policy.Action) bool {
	return strings.Contains(strings.ToLower(string(a)), "bucket")
}

// --- presets -------------------------------------------------------------

// ApplyPreset implements Service.
func (s *service) ApplyPreset(ctx context.Context, in PresetInput) (*Config, error) {
	added, err := presetRules(in)
	if err != nil {
		return nil, err
	}
	for attempt := 1; ; attempt++ {
		current, err := s.Get(ctx, in.Bucket)
		if err != nil {
			return nil, err
		}
		ifMatch := in.IfMatch
		if ifMatch == "" {
			ifMatch = current.ETag
		}
		rules := make([]Rule, 0, len(current.Rules)+len(added))
		for _, r := range current.Rules {
			if strings.HasPrefix(r.ID, PresetRulePrefix) {
				continue
			}
			if in.Preset == PresetPrivate && r.Effect == EffectAllow && contains(r.Principals, PrincipalEveryone) {
				continue
			}
			rules = append(rules, r)
		}
		rules = append(rules, added...)
		cfg, err := s.Put(ctx, PutInput{Bucket: in.Bucket, Rules: rules, IfMatch: ifMatch, Actor: in.Actor})
		if errors.Is(err, domain.ErrPreconditionFailed) && in.IfMatch == "" && attempt < presetAttempts {
			continue
		}
		return cfg, err
	}
}

// presetRules returns the rules of a preset.
func presetRules(in PresetInput) ([]Rule, error) {
	rule := func(suffix string, actions ...string) Rule {
		return Rule{ID: PresetRulePrefix + suffix, Priority: PresetPriority, Effect: EffectAllow,
			Principals: []string{PrincipalEveryone}, Actions: actions, Description: "generated by the " + string(in.Preset) + " preset"}
	}
	switch in.Preset {
	case PresetPrivate:
		return nil, nil
	case PresetPublicRead:
		return []Rule{rule("public-read", ActionRead)}, nil
	case PresetPublicReadList:
		return []Rule{rule("public-read-list", ActionRead, ActionList)}, nil
	case PresetPublicPrefix:
		if in.Match == nil || in.Match.Pattern == "" {
			return nil, fmt.Errorf("%w: the public-prefix preset needs a prefix, glob or regex", domain.ErrInvalidArgument)
		}
		r := rule("public-prefix", ActionRead)
		p := *in.Match
		r.Match = &p
		return []Rule{r}, nil
	case PresetPublicWrite:
		if !in.Confirm {
			return nil, fmt.Errorf("%w: the public-write preset lets anyone upload to the bucket", ErrConfirmationRequired)
		}
		r := rule("public-write", ActionWrite)
		if in.Match != nil && in.Match.Pattern != "" {
			p := *in.Match
			r.Match = &p
		}
		return []Rule{r}, nil
	}
	return nil, fmt.Errorf("%w: unknown preset %q (private, public-read, public-read-list, public-prefix, public-write)", domain.ErrInvalidArgument, in.Preset)
}

// PublicStatus implements Service.
func (s *service) PublicStatus(ctx context.Context, name string) (*PublicStatus, error) {
	cfg, err := s.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	out := &PublicStatus{Bucket: name, Preset: PresetPrivate, Blocked: cfg.BlockPublic}
	var public []Rule
	for _, r := range cfg.Rules {
		if r.Effect == EffectAllow && contains(r.Principals, PrincipalEveryone) {
			public = append(public, r)
			out.Rules = append(out.Rules, r.ID)
		}
	}
	if len(public) == 0 {
		return out, nil
	}
	out.Public = !out.Blocked
	out.Preset = PresetCustom
	if len(public) == 1 && len(public[0].Principals) == 1 && public[0].Conditions == nil {
		r := public[0]
		switch {
		case r.ID == PresetRulePrefix+"public-read" && r.Match == nil && sameActions(r.Actions, ActionRead):
			out.Preset = PresetPublicRead
		case r.ID == PresetRulePrefix+"public-read-list" && r.Match == nil && sameActions(r.Actions, ActionRead, ActionList):
			out.Preset = PresetPublicReadList
		case r.ID == PresetRulePrefix+"public-prefix" && r.Match != nil && sameActions(r.Actions, ActionRead):
			out.Preset = PresetPublicPrefix
		case r.ID == PresetRulePrefix+"public-write" && sameActions(r.Actions, ActionWrite):
			out.Preset = PresetPublicWrite
		}
	}
	return out, nil
}

// sameActions reports a list of actions equal to want.
func sameActions(actions []string, want ...string) bool {
	if len(actions) != len(want) {
		return false
	}
	for i := range actions {
		// #nosec G602 -- len(actions) == len(want) is checked above
		if !strings.EqualFold(actions[i], want[i]) {
			return false
		}
	}
	return true
}

// --- block public access -------------------------------------------------

// GetBlockPublic implements Service.
func (s *service) GetBlockPublic(ctx context.Context, name string) (*BlockPublic, error) {
	on, mode, err := clusterBlockPublic(ctx, s.deps.Config, s.opts)
	if err != nil {
		return nil, fmt.Errorf("accessrule: cluster block public access: %w", err)
	}
	out := &BlockPublic{Bucket: name, Enabled: on, Mode: mode, Cluster: on, Effective: on}
	if name == "" {
		return out, nil
	}
	b, err := s.deps.Buckets.GetBlockPublicAccess(ctx, name)
	if err != nil {
		return nil, err
	}
	out.Enabled = b
	out.Effective = on || b
	return out, nil
}

// SetBlockPublic implements Service.
func (s *service) SetBlockPublic(ctx context.Context, in BlockPublicInput) (*BlockPublic, error) {
	before, err := s.GetBlockPublic(ctx, in.Bucket)
	if err != nil {
		return nil, err
	}
	if in.Bucket == "" {
		if s.deps.Config == nil {
			return nil, fmt.Errorf("%w: the cluster-level block public access is not configurable", domain.ErrNotImplemented)
		}
		kv := config.KV{ConfigKeyBlockPublic: onOff(in.Enabled)}
		switch in.Mode {
		case "":
		case BlockModeReject, BlockModeIgnore:
			kv[ConfigKeyBlockPublicMode] = string(in.Mode)
		default:
			return nil, fmt.Errorf("%w: block public access mode %q (reject or ignore)", domain.ErrInvalidArgument, in.Mode)
		}
		if err := s.deps.Config.Set(ctx, ConfigSubsystem, kv); err != nil {
			return nil, err
		}
	} else {
		if in.Mode != "" && in.Mode != before.Mode {
			return nil, fmt.Errorf("%w: the block public access mode is a cluster-level setting", domain.ErrInvalidArgument)
		}
		if err := s.deps.Buckets.SetBlockPublicAccess(ctx, in.Bucket, in.Enabled); err != nil {
			return nil, err
		}
	}
	after, err := s.GetBlockPublic(ctx, in.Bucket)
	if err != nil {
		return nil, err
	}
	s.audit(ctx, "PutBlockPublicAccess", in.Bucket, in.Actor, map[string]any{
		"before": map[string]any{"enabled": before.Enabled, "mode": before.Mode},
		"after":  map[string]any{"enabled": after.Enabled, "mode": after.Mode},
	})
	return after, nil
}

// PublicAccessBlocked implements Service.
func (s *service) PublicAccessBlocked(ctx context.Context, name string) (bool, error) {
	if s.deps.Source == nil || name == "" {
		bp, err := s.GetBlockPublic(ctx, name)
		if err != nil {
			return false, err
		}
		return bp.Effective, nil
	}
	on, _, err := clusterBlockPublic(ctx, s.deps.Config, s.opts)
	if err != nil {
		return false, fmt.Errorf("accessrule: cluster block public access: %w", err)
	}
	if on {
		return true, nil
	}
	cfg, err := s.deps.Source.AccessConfig(ctx, name)
	if err != nil {
		return false, err
	}
	return cfg.BlockPublicAccess, nil
}

// onOff renders a switch of the configuration.
func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}
