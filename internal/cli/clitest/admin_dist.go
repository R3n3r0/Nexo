package clitest

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/stretchr/testify/mock"

	"github.com/r3n3r0/nexo/internal/cli"
	climock "github.com/r3n3r0/nexo/internal/cli/mock"
	"github.com/r3n3r0/nexo/internal/service/match"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// The access rules, distributions, key groups and certificates of MemAdmin
// (nexoctl access and dist). The evaluation of the access rules is a
// simplified model of the policy engine: principals "*", user:NAME and
// accesskey:KEY, the action classes, the three pattern types and the
// sourceIp and host conditions; the administrators are allowed by IAM.

// memDistState is the state of the access rules and distributions.
type memDistState struct {
	rules        map[string]*memRules
	blockCluster bool
	blockMode    string
	blockBuckets map[string]bool
	dists        map[string]*nexoadmin.Distribution
	groups       map[string]*nexoadmin.KeyGroup
	// privateKeys maps the private keys generated to their key id.
	privateKeys map[string]string
	certs       map[string]*nexoadmin.Certificate
}

// memRules are the access rules of a bucket.
type memRules struct {
	rules     []nexoadmin.BucketAccessRule
	etag      string
	updatedBy string
	updatedAt time.Time
}

// distLocked returns the state, initialized on first use (lock held).
func (s *memSession) distLocked() *memDistState {
	d := &s.m.dist
	if d.rules == nil {
		d.rules, d.blockBuckets = map[string]*memRules{}, map[string]bool{}
		d.dists, d.groups = map[string]*nexoadmin.Distribution{}, map[string]*nexoadmin.KeyGroup{}
		d.privateKeys, d.certs = map[string]string{}, map[string]*nexoadmin.Certificate{}
		d.blockMode = nexoadmin.BlockModeReject
	}
	return d
}

// nextETag returns a new ETag (lock held).
func (s *memSession) nextETag() string {
	s.m.seq++
	return fmt.Sprintf("etag-%d", s.m.seq)
}

// expectDistribution registers the access rules, distribution and
// certificate methods of the store on c.
func (s *memSession) expectDistribution(c *climock.AdminClient) {
	x := mock.Anything
	c.EXPECT().GetAccessRules(x, x).RunAndReturn(s.getAccessRules).Maybe()
	c.EXPECT().SetAccessRules(x, x, x, x).RunAndReturn(s.setAccessRules).Maybe()
	c.EXPECT().TestAccessRules(x, x, x).RunAndReturn(s.testAccessRules).Maybe()
	c.EXPECT().GetAccessRulesPublic(x, x).RunAndReturn(s.getAccessRulesPublic).Maybe()
	c.EXPECT().ApplyAccessPreset(x, x, x, x).RunAndReturn(s.applyAccessPreset).Maybe()
	c.EXPECT().GetBlockPublic(x, x).RunAndReturn(s.getBlockPublic).Maybe()
	c.EXPECT().SetBlockPublic(x, x, x).RunAndReturn(s.setBlockPublic).Maybe()
	c.EXPECT().ListDistributions(x).RunAndReturn(s.listDistributions).Maybe()
	c.EXPECT().CreateDistribution(x, x).RunAndReturn(func(ctx context.Context, d nexoadmin.Distribution) (*nexoadmin.Distribution, error) {
		return s.saveDistribution(ctx, "CreateDistribution", d, true, "")
	}).Maybe()
	c.EXPECT().UpdateDistribution(x, x, x).RunAndReturn(func(ctx context.Context, d nexoadmin.Distribution, ifMatch string) (*nexoadmin.Distribution, error) {
		return s.saveDistribution(ctx, "UpdateDistribution", d, false, ifMatch)
	}).Maybe()
	c.EXPECT().GetDistribution(x, x).RunAndReturn(s.getDistribution).Maybe()
	c.EXPECT().DeleteDistribution(x, x).RunAndReturn(s.deleteDistribution).Maybe()
	c.EXPECT().TestDistribution(x, x, x).RunAndReturn(s.testDistribution).Maybe()
	c.EXPECT().RotateOriginSecret(x, x, x).RunAndReturn(s.rotateOriginSecret).Maybe()
	c.EXPECT().SignDistributionURL(x, x, x).RunAndReturn(s.signDistributionURL).Maybe()
	c.EXPECT().ResolveHost(x, x).RunAndReturn(s.resolveHost).Maybe()
	c.EXPECT().ListKeyGroups(x).RunAndReturn(s.listKeyGroups).Maybe()
	c.EXPECT().CreateKeyGroup(x, x).RunAndReturn(s.createKeyGroup).Maybe()
	c.EXPECT().GetKeyGroup(x, x).RunAndReturn(s.getKeyGroup).Maybe()
	c.EXPECT().DeleteKeyGroup(x, x).RunAndReturn(s.deleteKeyGroup).Maybe()
	c.EXPECT().AddKeyGroupKey(x, x, x).RunAndReturn(s.addKeyGroupKey).Maybe()
	c.EXPECT().DeleteKeyGroupKey(x, x, x).RunAndReturn(s.deleteKeyGroupKey).Maybe()
	c.EXPECT().ListCertificates(x).RunAndReturn(s.listCertificates).Maybe()
	c.EXPECT().UploadCertificate(x, x).RunAndReturn(s.uploadCertificate).Maybe()
	c.EXPECT().DeleteCertificate(x, x).RunAndReturn(s.deleteCertificate).Maybe()
}

// accessRuleError is the error of an invalid access rule.
func accessRuleError(status int, code string, ae nexoadmin.AccessRuleError) error {
	return cli.MapAdminError(&nexoadmin.APIError{
		Status: status, Code: code, Title: http.StatusText(status), Detail: "invalid access rule: " + ae.Message, RequestID: "req-mem",
		AccessRuleErrors: []nexoadmin.AccessRuleError{ae},
	}, MemAdminEndpoint)
}

// --- access rules -----------------------------------------------------------------

// rulesLocked returns the rules of a bucket (lock held).
func (s *memSession) rulesLocked(bucket string) *memRules {
	d := s.distLocked()
	r := d.rules[bucket]
	if r == nil {
		r = &memRules{rules: []nexoadmin.BucketAccessRule{}, etag: "etag-empty"}
		d.rules[bucket] = r
	}
	return r
}

// blockedLocked reports the block public access in effect on a bucket.
func (s *memSession) blockedLocked(bucket string) bool {
	d := s.distLocked()
	return d.blockCluster || d.blockBuckets[bucket]
}

// configLocked builds the AccessRules of a bucket (lock held).
func (s *memSession) configLocked(bucket string) *nexoadmin.AccessRules {
	r := s.rulesLocked(bucket)
	blocked := s.blockedLocked(bucket)
	out := &nexoadmin.AccessRules{Bucket: bucket, Rules: slices.Clone(r.rules), ETag: r.etag, UpdatedBy: r.updatedBy, BlockPublic: blocked, Warnings: []string{}}
	if !r.updatedAt.IsZero() {
		t := r.updatedAt
		out.UpdatedAt = &t
	}
	for _, rule := range r.rules {
		if rule.Effect != nexoadmin.EffectAllow || !slices.Contains(rule.Principals, "*") {
			continue
		}
		if blocked {
			out.Warnings = append(out.Warnings, fmt.Sprintf("public access is blocked: the principal \"*\" of rule %q is ignored", rule.ID))
			continue
		}
		out.Public = true
		if slices.ContainsFunc(rule.Actions, func(a string) bool { return a == "write" || a == "delete" || a == "all" }) {
			out.Warnings = append(out.Warnings, fmt.Sprintf("rule %q lets everyone write or delete objects", rule.ID))
		}
	}
	if out.Rules == nil {
		out.Rules = []nexoadmin.BucketAccessRule{}
	}
	return out
}

func (s *memSession) getAccessRules(_ context.Context, bucket string) (*nexoadmin.AccessRules, error) {
	if err := s.begin("GetAccessRules"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:ManageAccessRules"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	return s.configLocked(bucket), nil
}

// validateRules checks rules as the server does, in part (lock held).
func (s *memSession) validateRules(bucket string, rules []nexoadmin.BucketAccessRule) error {
	seen := map[string]bool{}
	for i, r := range rules {
		fail := func(field, format string, args ...any) error {
			return accessRuleError(http.StatusBadRequest, nexoadmin.CodeInvalidAccessRule,
				nexoadmin.AccessRuleError{Rule: i + 1, ID: r.ID, Field: field, Message: fmt.Sprintf(format, args...)})
		}
		switch {
		case r.ID == "":
			return fail("id", "the id is required")
		case seen[r.ID]:
			return fail("id", "duplicate id")
		case r.Effect != nexoadmin.EffectAllow && r.Effect != nexoadmin.EffectDeny:
			return fail("effect", "must be allow or deny, not %q", r.Effect)
		case len(r.Principals) == 0:
			return fail("principals", "at least one principal")
		case len(r.Actions) == 0:
			return fail("actions", "at least one action")
		}
		seen[r.ID] = true
		if r.Match != nil {
			if _, err := memPattern(*r.Match); err != nil {
				pos, msg := 1, err.Error()
				var pe *match.PatternError
				if errors.As(err, &pe) {
					pos, msg = pe.Position, pe.Msg
				}
				return accessRuleError(http.StatusBadRequest, nexoadmin.CodeInvalidAccessRule,
					nexoadmin.AccessRuleError{Rule: i + 1, ID: r.ID, Field: "match.pattern", Position: pos, Message: msg})
			}
		}
		d := s.distLocked()
		if s.blockedLocked(bucket) && d.blockMode == nexoadmin.BlockModeReject && r.Effect == nexoadmin.EffectAllow && slices.Contains(r.Principals, "*") {
			return accessRuleError(http.StatusForbidden, nexoadmin.CodePublicAccessBlocked,
				nexoadmin.AccessRuleError{Rule: i + 1, ID: r.ID, Field: "principals", Message: "public access is blocked: a rule cannot allow everyone (\"*\")"})
		}
	}
	return nil
}

func (s *memSession) setAccessRules(_ context.Context, bucket string, rules []nexoadmin.BucketAccessRule, ifMatch string) (*nexoadmin.AccessRules, error) {
	if err := s.begin("SetAccessRules"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:ManageAccessRules"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	r := s.rulesLocked(bucket)
	if ifMatch != "" && ifMatch != r.etag {
		return nil, apiError(http.StatusPreconditionFailed, nexoadmin.CodePreconditionFailed, fmt.Sprintf("the access rules of %s changed (ETag %s)", bucket, r.etag))
	}
	if err := s.validateRules(bucket, rules); err != nil {
		return nil, err
	}
	r.rules, r.etag, r.updatedBy, r.updatedAt = slices.Clone(rules), s.nextETag(), s.caller, s.now()
	return s.configLocked(bucket), nil
}

// memPattern compiles a pattern of keys with the matcher of the server
// (internal/service/match), so that the in-memory admin matches the keys
// and the paths exactly like Nexo.
func memPattern(p nexoadmin.KeyPattern) (memMatcher, error) {
	ctx := context.Background()
	svc := match.New(match.Deps{})
	m, err := svc.Compile(ctx, match.Pattern{Type: match.Type(p.Type), Pattern: p.Pattern, CaseInsensitive: p.CaseInsensitive})
	if err != nil {
		return memMatcher{}, err
	}
	return memMatcher{svc: svc, m: m}, nil
}

// memMatcher matches the normalized keys and paths.
type memMatcher struct {
	svc match.Service
	m   match.Matcher
}

// MatchString reports whether the pattern matches s.
func (m memMatcher) MatchString(s string) bool {
	return m.m.Match(m.svc.Normalize(context.Background(), s)).Matched
}

// actionClass maps an action of a test to its class.
func actionClass(a string) string {
	switch strings.ToLower(a) {
	case "", "read", "s3:getobject":
		return "read"
	case "list", "s3:listbucket":
		return "list"
	case "write", "s3:putobject":
		return "write"
	case "delete", "s3:deleteobject":
		return "delete"
	case "tag", "s3:putobjecttagging":
		return "tag"
	}
	return strings.ToLower(a)
}

// s3Action names the S3 action of a class.
func s3Action(class string) string {
	switch class {
	case "list":
		return "s3:ListBucket"
	case "write":
		return "s3:PutObject"
	case "delete":
		return "s3:DeleteObject"
	case "tag":
		return "s3:PutObjectTagging"
	}
	return "s3:GetObject"
}

// ruleApplies evaluates a rule for a request; mismatch names the element
// that did not match.
func ruleApplies(r nexoadmin.BucketAccessRule, user, class, key, ip, host string, blocked bool) (bool, string, string) {
	principal := slices.ContainsFunc(r.Principals, func(p string) bool {
		return p == "*" || (user != "" && (p == "user:"+user || p == "accesskey:"+user))
	})
	if !principal {
		return false, "principal", ""
	}
	if blocked && r.Effect == nexoadmin.EffectAllow && !slices.ContainsFunc(r.Principals, func(p string) bool { return p != "*" }) {
		return false, "blocked-public", ""
	}
	if !slices.ContainsFunc(r.Actions, func(a string) bool { return a == "all" || actionClass(a) == class }) {
		return false, "action", ""
	}
	if r.Match != nil {
		re, err := memPattern(*r.Match)
		if err != nil || !re.MatchString(key) {
			return false, "key", ""
		}
	}
	if c := r.Conditions; c != nil {
		if len(c.SourceIP) > 0 && !ipIn(ip, c.SourceIP) {
			return false, "condition", "sourceIp"
		}
		if len(c.Host) > 0 && !slices.Contains(c.Host, host) {
			return false, "condition", "host"
		}
	}
	return true, "", ""
}

// ipIn reports whether ip is one of the IPs or CIDRs.
func ipIn(ip string, list []string) bool {
	addr := net.ParseIP(ip)
	for _, v := range list {
		if _, n, err := net.ParseCIDR(v); err == nil && addr != nil && n.Contains(addr) {
			return true
		}
		if v == ip {
			return true
		}
	}
	return false
}

// normalizeKey resolves ".", ".." and "//" in a key, as the server does.
func normalizeKey(key string) string {
	if key == "" {
		return ""
	}
	out := strings.TrimPrefix(path.Clean("/"+key), "/")
	if strings.HasSuffix(key, "/") && out != "" {
		out += "/"
	}
	return out
}

func (s *memSession) testAccessRules(_ context.Context, bucket string, in nexoadmin.TestAccessRulesRequest) (*nexoadmin.AccessRulesTestResult, error) {
	if err := s.begin("TestAccessRules"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:ManageAccessRules"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	user := ""
	switch p := in.Principal; {
	case p == "" || p == "anonymous" || p == "*" || strings.HasPrefix(p, "distribution:"):
	case strings.HasPrefix(p, "user:"), strings.HasPrefix(p, "accesskey:"):
		_, user, _ = strings.Cut(p, ":")
	default:
		user = p
	}
	if _, ok := s.m.users[user]; user != "" && user != MemAdminRoot && !ok {
		return nil, notFound("user_not_found", "user %s does not exist", user)
	}
	class := actionClass(in.Action)
	blocked := s.blockedLocked(bucket)
	bp := s.blockPublicLocked(bucket)
	rules := s.rulesLocked(bucket).rules
	ordered := slices.Clone(rules)
	sort.SliceStable(ordered, func(i, j int) bool {
		di, dj := ordered[i].Effect == nexoadmin.EffectDeny, ordered[j].Effect == nexoadmin.EffectDeny
		if di != dj {
			return di
		}
		return ordered[i].Priority < ordered[j].Priority
	})
	keys := in.Keys
	if len(keys) == 0 {
		keys = []string{""}
	}
	out := &nexoadmin.AccessRulesTestResult{Bucket: bucket, Action: s3Action(class), BlockPublic: *bp}
	admin := user == MemAdminRoot || slices.Contains(s.effectiveLocked(user), "consoleAdmin")
	for _, key := range keys {
		norm := normalizeKey(key)
		kr := nexoadmin.AccessRulesKeyResult{Key: key, NormalizedKey: norm, Rules: []nexoadmin.EvaluatedAccessRule{}, Matched: []string{}, RulesOutcome: "none"}
		var deny, allow string
		for _, r := range ordered {
			ok, mismatch, cond := ruleApplies(r, user, class, norm, in.IP, in.Host, blocked)
			effect := "Allow"
			if r.Effect == nexoadmin.EffectDeny {
				effect = "Deny"
			}
			kr.Rules = append(kr.Rules, nexoadmin.EvaluatedAccessRule{ID: r.ID, Priority: r.Priority, Effect: effect, Matched: ok, Mismatch: mismatch, Condition: cond})
			if mismatch == "blocked-public" {
				kr.BlockedPublic = append(kr.BlockedPublic, r.ID)
			}
			if !ok {
				continue
			}
			kr.Matched = append(kr.Matched, r.ID)
			switch {
			case r.Effect == nexoadmin.EffectDeny && deny == "":
				deny = r.ID
			case r.Effect == nexoadmin.EffectAllow && allow == "":
				allow = r.ID
			}
		}
		switch {
		case deny != "":
			kr.RulesOutcome, kr.Determinant, kr.Source, kr.Reason = "deny", deny, "accessrule", "denied by access rule "+deny
		case allow != "" && admin:
			kr.RulesOutcome, kr.Allowed, kr.Source, kr.Reason = "allow", true, "iam", "allowed by the IAM policy consoleAdmin"
		case allow != "":
			kr.RulesOutcome, kr.Allowed, kr.Determinant, kr.Source, kr.Reason = "allow", true, allow, "accessrule", "allowed by access rule "+allow
		case admin:
			kr.Allowed, kr.Source, kr.Reason = true, "iam", "allowed by the IAM policy consoleAdmin"
		default:
			kr.Reason = "implicit deny"
		}
		out.Results = append(out.Results, kr)
	}
	return out, nil
}

// presetIDPrefix starts the ids of the rules of the presets.
const presetIDPrefix = "preset-"

func (s *memSession) getAccessRulesPublic(_ context.Context, bucket string) (*nexoadmin.AccessRulesPublicStatus, error) {
	if err := s.begin("GetAccessRulesPublic"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:ManageAccessRules"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	return s.publicLocked(bucket), nil
}

// publicLocked computes the public status of a bucket (lock held).
func (s *memSession) publicLocked(bucket string) *nexoadmin.AccessRulesPublicStatus {
	out := &nexoadmin.AccessRulesPublicStatus{Bucket: bucket, Preset: nexoadmin.PresetPrivate, Blocked: s.blockedLocked(bucket)}
	custom := false
	for _, r := range s.rulesLocked(bucket).rules {
		if r.Effect != nexoadmin.EffectAllow || !slices.Contains(r.Principals, "*") {
			continue
		}
		out.Rules = append(out.Rules, r.ID)
		if p, ok := strings.CutPrefix(r.ID, presetIDPrefix); ok {
			out.Preset = p
		} else {
			custom = true
		}
	}
	if custom {
		out.Preset = nexoadmin.PresetCustom
	}
	out.Public = len(out.Rules) > 0 && !out.Blocked
	if len(out.Rules) == 0 {
		out.Blocked = false
	}
	return out
}

func (s *memSession) applyAccessPreset(_ context.Context, bucket string, in nexoadmin.AccessPresetRequest, ifMatch string) (*nexoadmin.AccessRules, error) {
	if err := s.begin("ApplyAccessPreset"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:ManageAccessRules"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	r := s.rulesLocked(bucket)
	if ifMatch != "" && ifMatch != r.etag {
		return nil, apiError(http.StatusPreconditionFailed, nexoadmin.CodePreconditionFailed, "the access rules changed")
	}
	var kept []nexoadmin.BucketAccessRule
	for _, rule := range r.rules {
		everyone := rule.Effect == nexoadmin.EffectAllow && slices.Contains(rule.Principals, "*")
		if strings.HasPrefix(rule.ID, presetIDPrefix) || (in.Preset == nexoadmin.PresetPrivate && everyone) {
			continue
		}
		kept = append(kept, rule)
	}
	add := nexoadmin.BucketAccessRule{ID: presetIDPrefix + in.Preset, Priority: 1000, Effect: nexoadmin.EffectAllow, Principals: []string{"*"}, Match: in.Match}
	switch in.Preset {
	case nexoadmin.PresetPrivate:
	case nexoadmin.PresetPublicRead, nexoadmin.PresetPublicPrefix:
		if in.Preset == nexoadmin.PresetPublicPrefix && in.Match == nil {
			return nil, invalid("public-prefix needs a pattern")
		}
		add.Actions = []string{"read"}
	case nexoadmin.PresetPublicReadList:
		add.Actions = []string{"read", "list"}
	case nexoadmin.PresetPublicWrite:
		if !in.Confirm {
			return nil, apiError(http.StatusBadRequest, nexoadmin.CodeConfirmationRequired, "public-write needs an explicit confirmation")
		}
		add.Actions = []string{"write"}
	default:
		return nil, invalid("unknown preset %q", in.Preset)
	}
	if add.Actions != nil {
		kept = append(kept, add)
	}
	if err := s.validateRules(bucket, kept); err != nil {
		return nil, err
	}
	if kept == nil {
		kept = []nexoadmin.BucketAccessRule{}
	}
	r.rules, r.etag, r.updatedBy, r.updatedAt = kept, s.nextETag(), s.caller, s.now()
	return s.configLocked(bucket), nil
}

// blockPublicLocked builds the block public access of a bucket ("" for
// the cluster).
func (s *memSession) blockPublicLocked(bucket string) *nexoadmin.BlockPublic {
	d := s.distLocked()
	out := &nexoadmin.BlockPublic{Bucket: bucket, Mode: d.blockMode, Cluster: d.blockCluster, Enabled: d.blockCluster, Effective: d.blockCluster}
	if bucket != "" {
		out.Enabled = d.blockBuckets[bucket]
		out.Effective = d.blockCluster || out.Enabled
	}
	return out
}

func (s *memSession) getBlockPublic(_ context.Context, bucket string) (*nexoadmin.BlockPublic, error) {
	if err := s.begin("GetBlockPublic"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:ManageAccessRules"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	return s.blockPublicLocked(bucket), nil
}

func (s *memSession) setBlockPublic(_ context.Context, bucket string, in nexoadmin.BlockPublicRequest) (*nexoadmin.BlockPublic, error) {
	if err := s.begin("SetBlockPublic"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:ManageAccessRules"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	d := s.distLocked()
	switch {
	case bucket != "" && in.Mode != "":
		return nil, invalid("the mode of the block public access is a setting of the cluster")
	case in.Mode != "" && in.Mode != nexoadmin.BlockModeReject && in.Mode != nexoadmin.BlockModeIgnore:
		return nil, invalid("block public access mode %q (reject, ignore)", in.Mode)
	case bucket != "":
		d.blockBuckets[bucket] = in.Enabled
	default:
		d.blockCluster = in.Enabled
		if in.Mode != "" {
			d.blockMode = in.Mode
		}
	}
	return s.blockPublicLocked(bucket), nil
}

// --- distributions ------------------------------------------------------------------

// copyDistribution returns a deep enough copy of a distribution.
func copyDistribution(d *nexoadmin.Distribution) *nexoadmin.Distribution {
	out := *d
	out.Aliases = slices.Clone(d.Aliases)
	out.Behaviors = slices.Clone(d.Behaviors)
	out.ErrorPages = slices.Clone(d.ErrorPages)
	out.Redirects = slices.Clone(d.Redirects)
	return &out
}

func (s *memSession) listDistributions(context.Context) ([]nexoadmin.Distribution, error) {
	if err := s.begin("ListDistributions"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:ManageDistributions"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	d := s.distLocked()
	out := []nexoadmin.Distribution{}
	for _, id := range cli.SortedKeys(d.dists) {
		out = append(out, *copyDistribution(d.dists[id]))
	}
	return out, nil
}

// distNotFound is the error of an unknown distribution.
func distNotFound(id string) error {
	return notFound(nexoadmin.CodeDistributionNotFound, "distribution not found: %s", id)
}

func (s *memSession) getDistribution(_ context.Context, id string) (*nexoadmin.Distribution, error) {
	if err := s.begin("GetDistribution"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:ManageDistributions"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	d, ok := s.distLocked().dists[id]
	if !ok {
		return nil, distNotFound(id)
	}
	return copyDistribution(d), nil
}

func (s *memSession) saveDistribution(_ context.Context, method string, d nexoadmin.Distribution, create bool, ifMatch string) (*nexoadmin.Distribution, error) {
	if err := s.begin(method); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:ManageDistributions"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	st := s.distLocked()
	cur, exists := st.dists[d.ID]
	switch {
	case d.ID == "":
		return nil, apiError(http.StatusBadRequest, nexoadmin.CodeInvalidDistribution, "the id is required")
	case create && exists:
		return nil, conflict("distribution %s already exists", d.ID)
	case !create && !exists:
		return nil, distNotFound(d.ID)
	case !create && ifMatch != "" && ifMatch != cur.ETag:
		return nil, apiError(http.StatusPreconditionFailed, nexoadmin.CodePreconditionFailed, fmt.Sprintf("distribution %s changed (ETag %s)", d.ID, cur.ETag))
	case len(d.Aliases) == 0:
		return nil, apiError(http.StatusBadRequest, nexoadmin.CodeInvalidDistribution, "at least one alias (domain) is required")
	case d.Origin.Bucket == "":
		return nil, apiError(http.StatusBadRequest, nexoadmin.CodeInvalidDistribution, "the origin bucket is required")
	}
	for _, other := range st.dists {
		if other.ID == d.ID {
			continue
		}
		for _, a := range d.Aliases {
			if slices.Contains(other.Aliases, a) {
				return nil, apiError(http.StatusConflict, nexoadmin.CodeAliasConflict, fmt.Sprintf("%s: alias already used by another distribution", a))
			}
		}
	}
	if d.Mode == "" {
		d.Mode = nexoadmin.ModeWebsite
	}
	now := s.now()
	created := now
	if exists && cur.CreatedAt != nil {
		created = *cur.CreatedAt
	}
	d.CreatedAt, d.UpdatedAt, d.ETag = &created, &now, s.nextETag()
	st.dists[d.ID] = copyDistribution(&d)
	return copyDistribution(&d), nil
}

func (s *memSession) deleteDistribution(_ context.Context, id string) error {
	if err := s.begin("DeleteDistribution"); err != nil {
		return err
	}
	if err := s.requireAdminLocked("admin:ManageDistributions"); err != nil {
		return err
	}
	defer s.m.mu.Unlock()
	st := s.distLocked()
	if _, ok := st.dists[id]; !ok {
		return distNotFound(id)
	}
	delete(st.dists, id)
	return nil
}

// aliasMatches reports whether host is served by alias.
func aliasMatches(alias, host string) bool {
	if rest, ok := strings.CutPrefix(alias, "*."); ok {
		return strings.HasSuffix(host, "."+rest)
	}
	return alias == host
}

// routeLocked resolves a host (lock held).
func (s *memSession) routeLocked(host string) (nexoadmin.Route, *nexoadmin.Distribution) {
	h := strings.ToLower(host)
	if name, _, err := net.SplitHostPort(h); err == nil {
		h = name
	}
	st := s.distLocked()
	for _, id := range cli.SortedKeys(st.dists) {
		d := st.dists[id]
		for _, a := range d.Aliases {
			if aliasMatches(a, h) {
				enabled := d.Enabled
				return nexoadmin.Route{Kind: "distribution", Host: h, Alias: a, Distribution: d.ID, Mode: d.Mode, Enabled: &enabled, Bucket: d.Origin.Bucket}, d
			}
		}
	}
	return nexoadmin.Route{Kind: "path-style", Host: h}, nil
}

// effective merges a behavior over the default behavior.
func effective(def, b nexoadmin.Behavior) nexoadmin.Behavior {
	out := def
	out.PathPattern = b.PathPattern
	if b.Access != "" {
		out.Access = b.Access
	}
	if b.AllowedMethods != nil {
		out.AllowedMethods = b.AllowedMethods
	}
	if b.IndexDocument != "" {
		out.IndexDocument = b.IndexDocument
	}
	if b.SPAFallback != "" {
		out.SPAFallback = b.SPAFallback
	}
	if b.Cache != nil {
		out.Cache = b.Cache
	}
	if b.Signing != nil {
		out.Signing = b.Signing
	}
	if b.ResponseHeaders != nil {
		out.ResponseHeaders = b.ResponseHeaders
	}
	if b.CORS != nil {
		out.CORS = b.CORS
	}
	return out
}

// planLocked resolves the URL of a test or a signature (lock held).
func (s *memSession) planLocked(d *nexoadmin.Distribution, raw string) (*url.URL, *nexoadmin.DistributionTestResult, error) {
	if strings.HasPrefix(raw, "/") {
		host := ""
		for _, a := range d.Aliases {
			if !strings.HasPrefix(a, "*.") {
				host = a
				break
			}
		}
		raw = "https://" + host + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, nil, invalid("%q is not an http(s) URL nor a path", raw)
	}
	route, rd := s.routeLocked(u.Host)
	if rd == nil || rd.ID != d.ID {
		return nil, nil, invalid("the host %s is not an alias of distribution %s", u.Hostname(), d.ID)
	}
	res := &nexoadmin.DistributionTestResult{Route: route, BehaviorIndex: -1, Behavior: d.DefaultBehavior, Bucket: d.Origin.Bucket, ResponseHeaders: map[string]string{}}
	p := u.Path
	if p == "" {
		p = "/"
	}
	for i, b := range d.Behaviors {
		if b.PathPattern == nil {
			continue
		}
		re, err := memPattern(*b.PathPattern)
		if err == nil && re.MatchString(p) {
			res.BehaviorIndex, res.Behavior = i, effective(d.DefaultBehavior, b)
			break
		}
	}
	if res.Behavior.Access == nexoadmin.AccessDeny {
		res.RefusalStatus, res.Refusal = http.StatusForbidden, "the behavior denies every request"
		return u, res, nil
	}
	// As the server, the index document is appended when serving.
	key := p
	res.Key = strings.TrimPrefix(strings.TrimPrefix(d.Origin.PathPrefix, "/")+"/"+strings.TrimPrefix(key, "/"), "/")
	for k, v := range d.Headers.Response {
		res.ResponseHeaders[k] = v
	}
	for k, v := range res.Behavior.ResponseHeaders {
		res.ResponseHeaders[k] = v
	}
	// As the server, the Cache-Control of the behavior is not among the
	// headers: it depends on the object when the behavior has none.
	return u, res, nil
}

func (s *memSession) testDistribution(_ context.Context, id string, in nexoadmin.DistributionTestRequest) (*nexoadmin.DistributionTestResult, error) {
	if err := s.begin("TestDistribution"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:ManageDistributions"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	d, ok := s.distLocked().dists[id]
	if !ok {
		return nil, distNotFound(id)
	}
	_, res, err := s.planLocked(d, in.URL)
	return res, err
}

func (s *memSession) rotateOriginSecret(_ context.Context, id string, in nexoadmin.RotateOriginSecretRequest) (*nexoadmin.RotatedOriginSecret, error) {
	if err := s.begin("RotateOriginSecret"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:ManageDistributions"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	d, ok := s.distLocked().dists[id]
	if !ok {
		return nil, distNotFound(id)
	}
	sec := d.Restrictions.OriginSecretHeader
	if sec == nil {
		sec = &nexoadmin.OriginSecret{Name: "X-Origin-Secret"}
	}
	if in.Header != "" {
		sec.Name = in.Header
	}
	keep := in.Keep
	if keep <= 0 {
		keep = 1
	}
	if in.DropPrevious {
		keep = 0
	}
	if len(sec.Values) > keep {
		sec.Values = sec.Values[len(sec.Values)-keep:]
	}
	s.m.seq++
	secret := fmt.Sprintf("mem-origin-secret-%d", s.m.seq)
	sec.Values = append(slices.Clone(sec.Values), fmt.Sprintf("sha256:mem-%d", s.m.seq))
	d.Restrictions.OriginSecretHeader = sec
	d.ETag = s.nextETag()
	return &nexoadmin.RotatedOriginSecret{Secret: secret, Header: sec.Name, Distribution: *copyDistribution(d)}, nil
}

func (s *memSession) signDistributionURL(_ context.Context, id string, in nexoadmin.SignRequest) (*nexoadmin.SignedURL, error) {
	if err := s.begin("SignDistributionURL"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:SignDistributionURL"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	st := s.distLocked()
	d, ok := st.dists[id]
	if !ok {
		return nil, distNotFound(id)
	}
	raw := in.URL
	if strings.HasPrefix(raw, "/") && (in.Scheme != "" || in.Host != "") {
		scheme, host := in.Scheme, in.Host
		if scheme == "" {
			scheme = "https"
		}
		if host == "" {
			host = d.Aliases[0]
		}
		raw = scheme + "://" + host + raw
	}
	u, res, err := s.planLocked(d, raw)
	if err != nil {
		return nil, err
	}
	if res.Behavior.Access != nexoadmin.AccessSigned || res.Behavior.Signing == nil {
		return nil, invalid("%s is served by a behavior with access %q", u.Path, res.Behavior.Access)
	}
	g, ok := st.groups[res.Behavior.Signing.KeyGroup]
	if !ok {
		return nil, notFound(nexoadmin.CodeKeyGroupNotFound, "key group not found: %s", res.Behavior.Signing.KeyGroup)
	}
	keyID, known := st.privateKeys[in.PrivateKeyPEM]
	if !known || !slices.ContainsFunc(g.Keys, func(k nexoadmin.PublicKey) bool { return k.ID == keyID }) || (in.KeyPairID != "" && in.KeyPairID != keyID) {
		return nil, invalid("the private key is not the one of a key of key group %s", g.ID)
	}
	expires := s.now().Add(time.Duration(in.ExpiresIn) * time.Second)
	if in.ExpiresAt != nil {
		expires = in.ExpiresAt.UTC()
	}
	out := &nexoadmin.SignedURL{URL: u.String(), Cookies: []nexoadmin.SignedCookie{}, KeyGroup: g.ID, KeyPairID: keyID, ExpiresAt: expires}
	if in.Cookies {
		out.Cookies = []nexoadmin.SignedCookie{
			{Name: "CloudFront-Policy", Value: "mem-policy", Expires: expires},
			{Name: "CloudFront-Signature", Value: "mem-signature", Expires: expires},
			{Name: "CloudFront-Key-Pair-Id", Value: keyID, Expires: expires},
		}
		return out, nil
	}
	q := u.Query()
	q.Set("Expires", strconv.FormatInt(expires.Unix(), 10))
	q.Set("Signature", "mem-signature")
	q.Set("Key-Pair-Id", keyID)
	u.RawQuery = q.Encode()
	out.URL = u.String()
	return out, nil
}

func (s *memSession) resolveHost(_ context.Context, host string) (*nexoadmin.Route, error) {
	if err := s.begin("ResolveHost"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:ManageDistributions"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	rt, _ := s.routeLocked(host)
	return &rt, nil
}

// --- key groups ------------------------------------------------------------------------

// generateKeyLocked creates a key pair in g (lock held).
func (s *memSession) generateKeyLocked(g *nexoadmin.KeyGroup, algorithm string, bits int, comment string) nexoadmin.GeneratedKey {
	s.m.seq++
	if algorithm == "" {
		algorithm = nexoadmin.KeyEd25519
	}
	if algorithm == nexoadmin.KeyRSA && bits == 0 {
		bits = 2048
	}
	id := fmt.Sprintf("KMEM%04d", s.m.seq)
	key := nexoadmin.PublicKey{ID: id, Algorithm: algorithm, Bits: bits, Comment: comment, CreatedAt: s.now(),
		PublicKeyPEM: "-----BEGIN PUBLIC KEY-----\nmem-public-" + id + "\n-----END PUBLIC KEY-----\n"}
	private := "-----BEGIN PRIVATE KEY-----\nmem-private-" + id + "\n-----END PRIVATE KEY-----\n"
	g.Keys = append(g.Keys, key)
	g.UpdatedAt = s.now()
	s.distLocked().privateKeys[private] = id
	return nexoadmin.GeneratedKey{PublicKey: key, PrivateKeyPEM: private}
}

// copyKeyGroup returns a copy of a key group.
func copyKeyGroup(g *nexoadmin.KeyGroup) *nexoadmin.KeyGroup {
	out := *g
	out.Keys = slices.Clone(g.Keys)
	if out.Keys == nil {
		out.Keys = []nexoadmin.PublicKey{}
	}
	return &out
}

func (s *memSession) listKeyGroups(context.Context) ([]nexoadmin.KeyGroup, error) {
	if err := s.begin("ListKeyGroups"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:ManageDistributions"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	st := s.distLocked()
	out := []nexoadmin.KeyGroup{}
	for _, id := range cli.SortedKeys(st.groups) {
		out = append(out, *copyKeyGroup(st.groups[id]))
	}
	return out, nil
}

func (s *memSession) createKeyGroup(_ context.Context, in nexoadmin.CreateKeyGroupRequest) (*nexoadmin.CreatedKeyGroup, error) {
	if err := s.begin("CreateKeyGroup"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:ManageDistributions"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	st := s.distLocked()
	if in.ID == "" {
		return nil, apiError(http.StatusBadRequest, nexoadmin.CodeInvalidDistribution, "the id is required")
	}
	if _, ok := st.groups[in.ID]; ok {
		return nil, conflict("key group %s already exists", in.ID)
	}
	g := &nexoadmin.KeyGroup{ID: in.ID, Comment: in.Comment, Keys: []nexoadmin.PublicKey{}, CreatedAt: s.now(), UpdatedAt: s.now()}
	st.groups[in.ID] = g
	out := &nexoadmin.CreatedKeyGroup{}
	if k := in.GenerateKey; k != nil {
		gen := s.generateKeyLocked(g, k.Algorithm, k.Bits, k.Comment)
		out.Key = &gen
	}
	out.KeyGroup = *copyKeyGroup(g)
	return out, nil
}

func (s *memSession) getKeyGroup(_ context.Context, id string) (*nexoadmin.KeyGroup, error) {
	if err := s.begin("GetKeyGroup"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:ManageDistributions"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	g, ok := s.distLocked().groups[id]
	if !ok {
		return nil, notFound(nexoadmin.CodeKeyGroupNotFound, "key group not found: %s", id)
	}
	return copyKeyGroup(g), nil
}

func (s *memSession) deleteKeyGroup(_ context.Context, id string) error {
	if err := s.begin("DeleteKeyGroup"); err != nil {
		return err
	}
	if err := s.requireAdminLocked("admin:ManageDistributions"); err != nil {
		return err
	}
	defer s.m.mu.Unlock()
	st := s.distLocked()
	if _, ok := st.groups[id]; !ok {
		return notFound(nexoadmin.CodeKeyGroupNotFound, "key group not found: %s", id)
	}
	for _, did := range cli.SortedKeys(st.dists) {
		d := st.dists[did]
		for _, b := range append([]nexoadmin.Behavior{d.DefaultBehavior}, d.Behaviors...) {
			if b.Signing != nil && b.Signing.KeyGroup == id {
				return conflict("key group %s is used by the distribution %s", id, did)
			}
		}
	}
	delete(st.groups, id)
	return nil
}

func (s *memSession) addKeyGroupKey(_ context.Context, group string, in nexoadmin.AddKeyRequest) (*nexoadmin.AddedKey, error) {
	if err := s.begin("AddKeyGroupKey"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:ManageDistributions"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	g, ok := s.distLocked().groups[group]
	if !ok {
		return nil, notFound(nexoadmin.CodeKeyGroupNotFound, "key group not found: %s", group)
	}
	if in.PublicKeyPEM == "" {
		gen := s.generateKeyLocked(g, in.Algorithm, in.Bits, in.Comment)
		return &nexoadmin.AddedKey{PublicKey: gen.PublicKey, PrivateKeyPEM: gen.PrivateKeyPEM}, nil
	}
	if !strings.Contains(in.PublicKeyPEM, "PUBLIC KEY") {
		return nil, apiError(http.StatusBadRequest, nexoadmin.CodeInvalidDistribution, "the public key is not PEM")
	}
	s.m.seq++
	key := nexoadmin.PublicKey{ID: fmt.Sprintf("KMEM%04d", s.m.seq), Algorithm: nexoadmin.KeyEd25519, PublicKeyPEM: in.PublicKeyPEM, Comment: in.Comment, CreatedAt: s.now()}
	g.Keys = append(g.Keys, key)
	return &nexoadmin.AddedKey{PublicKey: key}, nil
}

func (s *memSession) deleteKeyGroupKey(_ context.Context, group, keyID string) error {
	if err := s.begin("DeleteKeyGroupKey"); err != nil {
		return err
	}
	if err := s.requireAdminLocked("admin:ManageDistributions"); err != nil {
		return err
	}
	defer s.m.mu.Unlock()
	g, ok := s.distLocked().groups[group]
	if !ok {
		return notFound(nexoadmin.CodeKeyGroupNotFound, "key group not found: %s", group)
	}
	i := slices.IndexFunc(g.Keys, func(k nexoadmin.PublicKey) bool { return k.ID == keyID })
	if i < 0 {
		return notFound(nexoadmin.CodeKeyNotFound, "key not found: %s", keyID)
	}
	g.Keys = slices.Delete(g.Keys, i, i+1)
	return nil
}

// --- certificates ----------------------------------------------------------------------

// usedByLocked lists the distributions naming a certificate (lock held).
func (s *memSession) usedByLocked(id string) []string {
	st := s.distLocked()
	out := []string{}
	for _, did := range cli.SortedKeys(st.dists) {
		if st.dists[did].TLS.Certificate == nexoadmin.CertificateUploadPrefix+id {
			out = append(out, did)
		}
	}
	return out
}

func (s *memSession) listCertificates(context.Context) (*nexoadmin.CertificateList, error) {
	if err := s.begin("ListCertificates"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:ManageCertificates"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	st := s.distLocked()
	out := &nexoadmin.CertificateList{Items: []nexoadmin.Certificate{},
		ACME: nexoadmin.ACMEStatus{Directory: "https://acme.mem.test/directory", Challenge: "http-01", Managed: []nexoadmin.ManagedCertificate{}}}
	for _, id := range cli.SortedKeys(st.certs) {
		c := *st.certs[id]
		c.UsedBy = s.usedByLocked(id)
		out.Items = append(out.Items, c)
	}
	for _, did := range cli.SortedKeys(st.dists) {
		d := st.dists[did]
		if d.Enabled && d.TLS.Certificate == nexoadmin.CertificateAuto {
			out.ACME.Managed = append(out.ACME.Managed, nexoadmin.ManagedCertificate{ID: "acme-" + did, Domains: slices.Clone(d.Aliases),
				Distributions: []string{did}, State: "pending"})
		}
	}
	return out, nil
}

func (s *memSession) uploadCertificate(_ context.Context, in nexoadmin.UploadCertificateRequest) (*nexoadmin.Certificate, error) {
	if err := s.begin("UploadCertificate"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:ManageCertificates"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	block, _ := pem.Decode([]byte(in.CertificatePEM))
	if block == nil || !strings.Contains(in.PrivateKeyPEM, "PRIVATE KEY") {
		return nil, apiError(http.StatusBadRequest, nexoadmin.CodeInvalidCertificate, "invalid certificate: the chain and its key are required, in PEM")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil || len(leaf.DNSNames) == 0 {
		return nil, apiError(http.StatusBadRequest, nexoadmin.CodeInvalidCertificate, "invalid certificate: no DNS name")
	}
	id := in.ID
	if id == "" {
		id = strings.ReplaceAll(strings.TrimPrefix(leaf.DNSNames[0], "*."), ".", "-")
	}
	now := s.now()
	c := &nexoadmin.Certificate{ID: id, Source: "upload", Domains: leaf.DNSNames, Subject: leaf.Subject.String(), Issuer: leaf.Issuer.String(),
		Serial: leaf.SerialNumber.String(), NotBefore: leaf.NotBefore.UTC(), NotAfter: leaf.NotAfter.UTC(), Fingerprint: "mem-fingerprint",
		Status: nexoadmin.CertificateValid, CreatedAt: now, UpdatedAt: now}
	if leaf.NotAfter.Sub(now) < 21*24*time.Hour {
		c.Status = nexoadmin.CertificateExpiring
	}
	s.distLocked().certs[id] = c
	out := *c
	out.UsedBy = s.usedByLocked(id)
	return &out, nil
}

func (s *memSession) deleteCertificate(_ context.Context, id string) error {
	if err := s.begin("DeleteCertificate"); err != nil {
		return err
	}
	if err := s.requireAdminLocked("admin:ManageCertificates"); err != nil {
		return err
	}
	defer s.m.mu.Unlock()
	st := s.distLocked()
	if _, ok := st.certs[id]; !ok {
		return notFound(nexoadmin.CodeCertificateNotFound, "certificate not found: %s", id)
	}
	if users := s.usedByLocked(id); len(users) > 0 {
		return conflict("certificate %s is used by the distributions %s", id, strings.Join(users, ", "))
	}
	delete(st.certs, id)
	return nil
}
