package accessrule

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/config"
	"github.com/r3n3r0/nexo/internal/service/policy"
)

// evaluator is the default Evaluator: the compiled rules of every bucket,
// keyed by the ETag of their document, so that a change of the rules (on
// this node, or on another one, which invalidates the bucket metadata)
// is compiled again at the next request.
type evaluator struct {
	deps EvaluatorDeps
	opts options
	comp *compiler
	// emptyETag is the ETag of a bucket without rules.
	emptyETag string
	listing   map[string]bool
	lock      map[string]bool

	mu    sync.RWMutex
	cache map[string]*compiledSet
}

var _ Evaluator = (*evaluator)(nil)

// NewEvaluator returns the Evaluator. Source and Matcher are required (a
// missing one is a wiring error, domain.ErrInvalidArgument).
func NewEvaluator(deps EvaluatorDeps, opts ...Option) (Evaluator, error) {
	if deps.Source == nil {
		return nil, fmt.Errorf("%w: accessrule: the evaluator requires the configuration source", domain.ErrInvalidArgument)
	}
	if deps.Matcher == nil {
		return nil, fmt.Errorf("%w: accessrule: the evaluator requires the matcher", domain.ErrInvalidArgument)
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &evaluator{
		deps: deps, opts: newOptions(opts), comp: newCompiler(deps.Matcher), emptyETag: bucket.AccessRulesETag(nil),
		listing: listingActions(), lock: lockActions(), cache: make(map[string]*compiledSet),
	}, nil
}

// EvaluateRules implements Evaluator (policy.AccessRules).
func (e *evaluator) EvaluateRules(ctx context.Context, in policy.RuleRequest) (policy.RuleOutcome, error) {
	if in.Bucket == "" {
		return policy.RuleOutcome{}, nil
	}
	cfg, err := e.deps.Source.AccessConfig(ctx, in.Bucket)
	if err != nil {
		if errors.Is(err, domain.ErrInvalidBucketName) || errors.Is(err, domain.ErrBucketNotFound) {
			return policy.RuleOutcome{}, nil
		}
		return policy.RuleOutcome{}, fmt.Errorf("accessrule: access configuration of %s: %w", in.Bucket, err)
	}
	if cfg.Rules == nil || cfg.Rules.ETag == e.emptyETag {
		e.drop(in.Bucket)
		return policy.RuleOutcome{}, nil
	}
	cs, err := e.compiled(ctx, in.Bucket, cfg.Rules)
	if err != nil {
		return policy.RuleOutcome{}, err
	}
	req := e.prepare(ctx, in, cs, cfg)
	return evaluate(cs, req, in.Trace), nil
}

// prepare builds the request the rules are evaluated for.
func (e *evaluator) prepare(ctx context.Context, in policy.RuleRequest, cs *compiledSet, cfg bucket.AccessConfig) *evalRequest {
	id := in.Identity
	if id == nil {
		id = domain.Anonymous()
	}
	action := strings.ToLower(string(in.Action))
	key := in.Object
	if key == "" && e.listing[action] {
		if v, ok := in.Conditions[condPrefix]; ok && len(v) > 0 {
			key = v[0]
		}
	}
	req := &evalRequest{
		ctx: ctx, id: id, action: action, lock: e.lock[action],
		key: e.deps.Matcher.Normalize(ctx, key), conds: in.Conditions, now: in.Now, secrets: e.deps.Secrets,
	}
	if req.now.IsZero() {
		req.now = e.deps.Clock.Now()
	}
	if m, ok := req.first(condMethod); ok && m != "" {
		req.method = strings.ToUpper(m)
	} else {
		req.method = methodOf(action)
	}
	if cs.public {
		req.blocked = cfg.BlockPublicAccess
		if !req.blocked {
			on, _, err := clusterBlockPublic(ctx, e.deps.Config, e.opts)
			if err != nil {
				// The configuration cannot be read: block (fail closed).
				e.deps.Logger.WarnContext(ctx, "accessrule: block public access unknown, blocking", "err", err)
				on = true
			}
			req.blocked = on
		}
	}
	return req
}

// compiled returns the compiled rules of a bucket, compiling the stored
// document when its ETag changed.
func (e *evaluator) compiled(ctx context.Context, name string, stored *bucket.AccessRules) (*compiledSet, error) {
	e.mu.RLock()
	cs, ok := e.cache[name]
	e.mu.RUnlock()
	if ok && cs.etag == stored.ETag {
		return cs, nil
	}
	var rules []Rule
	if err := json.Unmarshal(stored.Document, &rules); err != nil {
		return nil, fmt.Errorf("accessrule: stored rules of %s: %w: %v", name, domain.ErrInvalidFormat, err)
	}
	_, compiled, err := e.comp.normalizeRules(ctx, rules)
	if err != nil {
		return nil, fmt.Errorf("accessrule: stored rules of %s: %w: %v", name, domain.ErrInvalidFormat, err)
	}
	cs, err = compileSet(ctx, e.deps.Matcher, stored.ETag, compiled)
	if err != nil {
		return nil, fmt.Errorf("accessrule: stored rules of %s: %w: %v", name, domain.ErrInvalidFormat, err)
	}
	e.mu.Lock()
	e.cache[name] = cs
	e.mu.Unlock()
	e.deps.Logger.DebugContext(ctx, "accessrule: rules compiled", "bucket", name, "rules", len(cs.rules), "etag", stored.ETag)
	return cs, nil
}

// drop forgets the compiled rules of a bucket.
func (e *evaluator) drop(name string) {
	e.mu.RLock()
	_, ok := e.cache[name]
	e.mu.RUnlock()
	if ok {
		e.mu.Lock()
		delete(e.cache, name)
		e.mu.Unlock()
	}
}

// clusterBlockPublic reads the cluster-level block public access from the
// dynamic configuration, with the defaults of the options for the keys
// it does not set.
func clusterBlockPublic(ctx context.Context, cfg config.Service, o options) (bool, BlockPublicMode, error) {
	on, mode := o.blockPublic, o.blockMode
	if cfg == nil {
		return on, mode, nil
	}
	kv, err := cfg.Get(ctx, ConfigSubsystem)
	if err != nil {
		return on, mode, err
	}
	if v, ok := kv[ConfigKeyBlockPublic]; ok && strings.TrimSpace(v) != "" {
		b, ok := parseSwitch(v)
		if !ok {
			return true, mode, fmt.Errorf("%w: %s.%s %q is not on or off", domain.ErrInvalidArgument, ConfigSubsystem, ConfigKeyBlockPublic, v)
		}
		on = b
	}
	if v, ok := kv[ConfigKeyBlockPublicMode]; ok {
		switch m := BlockPublicMode(strings.ToLower(strings.TrimSpace(v))); m {
		case BlockModeReject, BlockModeIgnore:
			mode = m
		}
	}
	return on, mode, nil
}

// parseSwitch parses an on/off value of the configuration.
func parseSwitch(v string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "on", "true", "enable", "enabled", "1", "yes":
		return true, true
	case "off", "false", "disable", "disabled", "0", "no":
		return false, true
	}
	return false, false
}
