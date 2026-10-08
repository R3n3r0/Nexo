package policy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
)

// IsPublic implements Service.
func (s *service) IsPublic(_ context.Context, p *domain.Policy) bool {
	if p == nil {
		return false
	}
	for i := range p.Statement {
		if statementIsPublic(&p.Statement[i]) {
			return true
		}
	}
	return false
}

// statementIsPublic reports whether one statement grants access to
// everyone: Allow, a wildcard principal (or NotPrincipal, i.e. "everyone
// but"), and no condition that pins the requester to fixed sources.
func statementIsPublic(st *domain.Statement) bool {
	if st.Effect != domain.Allow {
		return false
	}
	if !st.Principal.IsWildcard() && st.NotPrincipal == nil {
		return false
	}
	return !restrictsSource(st.Condition)
}

// restrictsSource reports whether a condition block limits the statement to
// fixed values of a key identifying the requester or its network. Negated
// operators, IfExists variants and ForAllValues sets never restrict: they
// pass when the key is absent.
func restrictsSource(cond domain.Condition) bool {
	for op, keys := range cond {
		parsed, ok := parseOperator(op)
		if !ok || parsed.negated || parsed.ifExists || parsed.forAll {
			continue
		}
		for key, values := range keys {
			if isSourceKey(key) && fixedValues(parsed.base, values) {
				return true
			}
		}
	}
	return false
}

// isSourceKey reports whether a condition key identifies the requester or
// the network it comes from.
func isSourceKey(key string) bool {
	switch strings.ToLower(key) {
	case "aws:sourceip", "aws:sourcevpc", "aws:sourcevpce", "aws:sourcearn",
		"aws:sourceaccount", "aws:sourceowner", "aws:principalorgid",
		"aws:principalaccount", "aws:principalarn", "aws:userid", "aws:username":
		return true
	}
	return false
}

// fixedValues reports whether the operator compares the key against fixed,
// non-universal values.
func fixedValues(base string, values []string) bool {
	if len(values) == 0 {
		return false
	}
	for _, v := range values {
		switch base {
		case "StringEquals", "StringEqualsIgnoreCase":
			if v == "" {
				return false
			}
		case "StringLike":
			if v == "" || strings.ContainsAny(v, "*?") {
				return false
			}
		case "IpAddress":
			if !fixedNetwork(v) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// fixedNetwork reports whether v is an IP or a CIDR narrower than the
// whole address space.
func fixedNetwork(v string) bool {
	v = strings.TrimSpace(v)
	if !strings.Contains(v, "/") {
		return net.ParseIP(v) != nil
	}
	_, network, err := net.ParseCIDR(v)
	if err != nil {
		return false
	}
	ones, _ := network.Mask.Size()
	return ones > 0
}

// --- anonymous access presets (docs/09 §9.3) -------------------------------------

// presetSidPrefix starts the Sid of the statements generated for presets.
const presetSidPrefix = "NexoAnonymous"

// probeObject is the object name, under the prefix, used to probe what a
// bucket policy grants to anonymous users.
const probeObject = "nexo-preset-probe"

// checkPresetArgs validates the bucket and prefix of a preset.
func checkPresetArgs(bucket, prefix string) error {
	if bucket == "" || strings.ContainsAny(bucket, "/*?$") {
		return fmt.Errorf("%w: policy: invalid bucket name %q", domain.ErrInvalidArgument, bucket)
	}
	if strings.ContainsAny(prefix, "*?") || strings.Contains(prefix, "${") || strings.HasPrefix(prefix, "/") {
		return fmt.Errorf("%w: policy: invalid prefix %q (no wildcards, variables or leading slash)", domain.ErrInvalidArgument, prefix)
	}
	return nil
}

// presetSids returns the Sids of the statements generated for a prefix:
// bucket-level actions, listing, object actions. They embed a hash of the
// prefix so that the rules of different prefixes coexist.
func presetSids(prefix string) [3]string {
	sum := sha256.Sum256([]byte(prefix))
	id := hex.EncodeToString(sum[:6])
	return [3]string{presetSidPrefix + "Bucket" + id, presetSidPrefix + "List" + id, presetSidPrefix + "Objects" + id}
}

// presetStatements generates the anonymous statements of a preset.
func presetStatements(bucket, prefix string, preset Preset) ([]domain.Statement, error) {
	var read, write bool
	switch preset {
	case PresetPrivate:
		return nil, nil
	case PresetDownload:
		read = true
	case PresetUpload:
		write = true
	case PresetPublic:
		read, write = true, true
	default:
		return nil, fmt.Errorf("%w: policy: unknown preset %q (private, download, upload, public)", domain.ErrInvalidArgument, preset)
	}
	sids := presetSids(prefix)
	everyone := func() *domain.Principal { return &domain.Principal{AWS: domain.StringSet{"*"}} }
	bucketARN := "arn:aws:s3:::" + bucket
	bucketActions := domain.StringSet{string(ActionGetBucketLocation)}
	var objectActions domain.StringSet
	if read {
		objectActions = append(objectActions, string(ActionGetObject))
	}
	if write {
		bucketActions = append(bucketActions, string(ActionListMultipart))
		objectActions = append(objectActions, string(ActionPutObject), string(ActionAbortMultipart), string(ActionListMultipartParts))
	}
	if read && write {
		objectActions = append(objectActions, string(ActionDeleteObject))
	}
	out := []domain.Statement{{SID: sids[0], Effect: domain.Allow, Principal: everyone(), Action: bucketActions, Resource: domain.StringSet{bucketARN}}}
	if read {
		list := domain.Statement{SID: sids[1], Effect: domain.Allow, Principal: everyone(), Action: domain.StringSet{string(ActionListBucket)}, Resource: domain.StringSet{bucketARN}}
		if prefix != "" {
			list.Condition = domain.Condition{"StringLike": {"s3:prefix": domain.StringSet{prefix + "*"}}}
		}
		out = append(out, list)
	}
	out = append(out, domain.Statement{SID: sids[2], Effect: domain.Allow, Principal: everyone(), Action: objectActions, Resource: domain.StringSet{bucketARN + "/" + prefix + "*"}})
	return out, nil
}

// GeneratePublicPolicy implements Service.
func (s *service) GeneratePublicPolicy(ctx context.Context, bucket, prefix string, preset Preset) (*domain.Policy, error) {
	return s.ApplyPublicPreset(ctx, nil, bucket, prefix, preset)
}

// ApplyPublicPreset implements Service.
func (s *service) ApplyPublicPreset(_ context.Context, current *domain.Policy, bucket, prefix string, preset Preset) (*domain.Policy, error) {
	if err := checkPresetArgs(bucket, prefix); err != nil {
		return nil, err
	}
	generated, err := presetStatements(bucket, prefix, preset)
	if err != nil {
		return nil, err
	}
	out := &domain.Policy{Version: domain.PolicyVersion}
	if current != nil {
		cp := clonePolicy(current)
		out.ID = cp.ID
		sids := presetSids(prefix)
		for _, st := range cp.Statement {
			if st.SID != sids[0] && st.SID != sids[1] && st.SID != sids[2] {
				out.Statement = append(out.Statement, st)
			}
		}
	}
	out.Statement = append(out.Statement, generated...)
	if len(out.Statement) == 0 {
		return nil, nil
	}
	return out, nil
}

// DetectPreset implements Service: the preset is inferred from what the
// policy actually grants to an anonymous request on the prefix, so that
// documents written by hand or by other tools are recognized too.
func (s *service) DetectPreset(ctx context.Context, p *domain.Policy, bucket, prefix string) Preset {
	if p == nil || checkPresetArgs(bucket, prefix) != nil {
		return PresetPrivate
	}
	allowed := func(action Action, object string, conds map[string][]string) bool {
		d, err := s.evaluate(ctx, Args{Identity: domain.Anonymous(), Action: action, Bucket: bucket, Object: object, Conditions: conds, BucketPolicy: p}, false)
		return err == nil && d.Allowed
	}
	probe := prefix + probeObject
	var listConds map[string][]string
	if prefix != "" {
		listConds = map[string][]string{"s3:prefix": {prefix}}
	}
	list := allowed(ActionListBucket, "", listConds)
	get := allowed(ActionGetObject, probe, nil)
	put := allowed(ActionPutObject, probe, nil)
	del := allowed(ActionDeleteObject, probe, nil)
	read := list && get
	switch {
	case read && put:
		return PresetPublic
	case read && !put && !del:
		return PresetDownload
	case put && !list && !get && !del:
		return PresetUpload
	case list || get || put || del || touchesPrefix(p, bucket, prefix):
		return PresetCustom
	default:
		return PresetPrivate
	}
}

// touchesPrefix reports whether an anonymous Allow statement targets
// objects that may lie under the prefix with a resource at least as broad
// as the prefix. Bucket-level statements do not count, nor do statements
// on deeper paths, which ListPublicAccess reports as rules of their own.
func touchesPrefix(p *domain.Policy, bucket, prefix string) bool {
	target := "arn:aws:s3:::" + bucket + "/" + prefix
	for i := range p.Statement {
		st := &p.Statement[i]
		if st.Effect != domain.Allow || !principalApplies(st, domain.Anonymous()) {
			continue
		}
		if len(st.NotResource) > 0 {
			return true
		}
		for _, r := range st.Resource {
			if r == "*" {
				return true
			}
			literal := literalPrefix(r)
			if literal == r && !strings.Contains(strings.TrimPrefix(r, "arn:aws:s3:::"), "/") {
				continue // the bucket itself
			}
			if strings.HasPrefix(target, literal) {
				return true
			}
		}
	}
	return false
}

// literalPrefix returns the part of a resource pattern before its first
// wildcard or variable.
func literalPrefix(r string) string {
	if i := strings.IndexAny(r, "*?"); i >= 0 {
		r = r[:i]
	}
	if i := strings.Index(r, "${"); i >= 0 {
		r = r[:i]
	}
	return r
}

// ListPublicAccess implements Service: candidate prefixes come from the
// object resources of the anonymous Allow statements of the bucket.
func (s *service) ListPublicAccess(ctx context.Context, p *domain.Policy, bucket string) []PublicAccessRule {
	if p == nil || checkPresetArgs(bucket, "") != nil {
		return nil
	}
	objects := "arn:aws:s3:::" + bucket + "/"
	candidates := map[string]bool{}
	for i := range p.Statement {
		st := &p.Statement[i]
		if st.Effect != domain.Allow || !principalApplies(st, domain.Anonymous()) {
			continue
		}
		for _, r := range st.Resource {
			if prefix, ok := strings.CutPrefix(literalPrefix(r), objects); ok {
				candidates[prefix] = true
			}
		}
	}
	out := make([]PublicAccessRule, 0, len(candidates))
	for _, prefix := range sortedKeys(candidates) {
		if checkPresetArgs(bucket, prefix) != nil {
			continue
		}
		if preset := s.DetectPreset(ctx, p, bucket, prefix); preset != PresetPrivate {
			out = append(out, PublicAccessRule{Prefix: prefix, Preset: preset})
		}
	}
	return out
}
