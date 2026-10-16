package accessrule_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/accessrule"
	"github.com/r3n3r0/nexo/internal/service/policy"
)

const (
	allow = accessrule.EffectAllow
	deny  = accessrule.EffectDeny
)

// publicBucketPolicy grants everyone s3:GetObject on testBucket.
func publicBucketPolicy(t *testing.T, f *fixture) *domain.Policy {
	t.Helper()
	p, err := f.policy.ParseBucketPolicy(f.ctx, testBucket, []byte(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::media/*"}]}`))
	require.NoError(t, err)
	return p
}

// TestPrecedence is TAR-05: a deny rule prevails over an IAM allow and
// over a bucket policy allow; an allow rule grants without IAM policy;
// an IAM deny prevails over an allow rule; the inline policy of derived
// credentials still limits; root bypasses the rules; object lock actions
// are never granted by a rule.
//
// covers: S-ACC-01
func TestPrecedence(t *testing.T) {
	f := newFixture(t)
	f.put(
		rule("block-private", 5, deny, list("*"), list("all"), globP("**/private/**"), nil),
		rule("mario-docs", 10, allow, list("user:mario"), list("read", "write"), prefixP("docs/"), nil),
		rule("all-for-ops", 20, allow, list("group:ops"), list("all"), nil, nil),
		rule("public-images", 30, allow, list("*"), list("read"), regexP(`^img/[0-9]{4}/.*\.(jpe?g|png|webp)$`), nil),
	)
	inline := &domain.Policy{Version: "2012-10-17", Statement: []domain.Statement{{
		Effect: domain.Allow, Action: domain.StringSet{"s3:GetObject"}, Resource: domain.StringSet{"arn:aws:s3:::media/docs/a.txt"},
	}}}
	serviceAccount := &domain.Identity{AccessKey: "svc1", Parent: "mario", SessionPolicy: inline}
	iamDeny := &domain.Identity{AccessKey: "eve"}
	require.NoError(t, f.policy.SetPolicy(f.ctx, "deny-docs", &domain.Policy{Version: "2012-10-17", Statement: []domain.Statement{{
		Effect: domain.Deny, Action: domain.StringSet{"s3:*"}, Resource: domain.StringSet{"arn:aws:s3:::media/docs/*"},
	}}}))
	iamDeny.PolicyNames = []string{"deny-docs"}
	iamDeny.Groups = []string{"ops"}

	tests := []struct {
		name     string
		r        req
		allowed  bool
		source   policy.Source
		ruleID   string
		inReason string
	}{
		{name: "deny rule beats IAM readwrite", r: req{id: user("luigi", list("readwrite")), key: "a/private/x"},
			allowed: false, source: policy.SourceAccessRule, ruleID: "block-private"},
		{name: "IAM readwrite elsewhere", r: req{id: user("luigi", list("readwrite")), key: "a/public/x"},
			allowed: true, source: policy.SourceIAM},
		{name: "deny rule beats the bucket policy", r: req{id: domain.Anonymous(), key: "private/x", bucket: publicBucketPolicy(t, f)},
			allowed: false, source: policy.SourceAccessRule, ruleID: "block-private"},
		{name: "bucket policy elsewhere", r: req{id: domain.Anonymous(), key: "other/x", bucket: publicBucketPolicy(t, f)},
			allowed: true, source: policy.SourceBucket},
		{name: "allow rule without IAM policy", r: req{id: user("mario", nil), key: "docs/report.pdf"},
			allowed: true, source: policy.SourceAccessRule, ruleID: "mario-docs"},
		{name: "allow rule write without IAM policy", r: req{id: user("mario", nil), action: policy.ActionPutObject, key: "docs/new.pdf"},
			allowed: true, source: policy.SourceAccessRule, ruleID: "mario-docs"},
		{name: "allow rule does not cover delete", r: req{id: user("mario", nil), action: policy.ActionDeleteObject, key: "docs/new.pdf"},
			allowed: false, inReason: "no policy or access rule"},
		{name: "deny rule beats the allow rule", r: req{id: user("mario", nil), key: "docs/private/x"},
			allowed: false, source: policy.SourceAccessRule, ruleID: "block-private"},
		{name: "another user", r: req{id: user("luigi", nil), key: "docs/report.pdf"}, allowed: false},
		{name: "anonymous allowed by a public rule", r: req{id: domain.Anonymous(), key: "img/2026/a.JPG"}, allowed: false},
		{name: "anonymous allowed by a public rule (case)", r: req{id: domain.Anonymous(), key: "img/2026/a.jpg"},
			allowed: true, source: policy.SourceAccessRule, ruleID: "public-images"},
		{name: "anonymous elsewhere", r: req{id: domain.Anonymous(), key: "docs/report.pdf"}, allowed: false,
			inReason: "not granted by the bucket policy or an access rule"},
		{name: "IAM deny beats an allow rule", r: req{id: iamDeny, key: "docs/a.txt"},
			allowed: false, source: policy.SourceIAM},
		{name: "group rule", r: req{id: iamDeny, key: "ops/a.txt", action: policy.ActionDeleteObject},
			allowed: true, source: policy.SourceAccessRule, ruleID: "all-for-ops"},
		{name: "inline policy allows within the rule", r: req{id: serviceAccount, key: "docs/a.txt"},
			allowed: true, source: policy.SourceAccessRule, ruleID: "mario-docs"},
		{name: "inline policy limits the rule", r: req{id: serviceAccount, key: "docs/b.txt"},
			allowed: false, source: policy.SourceInline, inReason: "inline policy does not allow"},
		{name: "root bypasses the deny rules", r: req{id: &domain.Identity{AccessKey: "root", IsRoot: true}, key: "private/x"},
			allowed: true, source: policy.SourceRoot},
		{name: "object lock bypass never granted", r: req{id: iamDeny, key: "ops/a.txt", action: policy.ActionBypassGovernanceRetention},
			allowed: false},
		{name: "retention never granted", r: req{id: iamDeny, key: "ops/a.txt", action: policy.ActionPutObjectRetention},
			allowed: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := f.evaluate(tt.r)
			assert.Equal(t, tt.allowed, d.Allowed, d.Reason)
			if tt.source != "" {
				assert.Equal(t, tt.source, d.Source, d.Reason)
			}
			if tt.ruleID != "" {
				require.NotNil(t, d.Rule, d.Reason)
				assert.Equal(t, tt.ruleID, d.Rule.ID)
				assert.Contains(t, d.Reason, tt.ruleID)
			}
			if tt.inReason != "" {
				assert.Contains(t, d.Reason, tt.inReason)
			}
			// The simulator decides alike and lists every rule.
			sim, err := f.decide(tt.r, true)
			require.NoError(t, err)
			assert.Equal(t, d.Allowed, sim.Allowed)
			assert.Equal(t, d.Rule, sim.Rule)
			if tt.r.id == nil || !tt.r.id.IsRoot {
				assert.Len(t, sim.EvaluatedRules, 4)
			}
		})
	}
}

// TestPriorityDecides checks the deciding rule within the access rules:
// a deny rule beats every allow rule whatever their priorities, and among
// rules of the same effect the lowest priority decides (then the order of
// the document).
//
// covers: S-ACC-01
func TestPriorityDecides(t *testing.T) {
	f := newFixture(t)
	f.put(
		rule("late-allow", 50, allow, list("*"), list("read"), nil, nil),
		rule("early-allow", 1, allow, list("*"), list("read"), prefixP("a/"), nil),
		rule("same-priority", 1, allow, list("*"), list("read"), prefixP("a/"), nil),
		rule("late-deny", 900, deny, list("*"), list("read"), prefixP("a/secret"), nil),
	)
	d := f.evaluate(req{id: domain.Anonymous(), key: "a/x"})
	require.True(t, d.Allowed)
	assert.Equal(t, "early-allow", d.Rule.ID)
	d = f.evaluate(req{id: domain.Anonymous(), key: "b/x"})
	assert.Equal(t, "late-allow", d.Rule.ID)
	d = f.evaluate(req{id: domain.Anonymous(), key: "a/secret.txt"})
	assert.False(t, d.Allowed)
	assert.Equal(t, "late-deny", d.Rule.ID)

	sim, err := f.decide(req{id: domain.Anonymous(), key: "a/secret.txt"}, true)
	require.NoError(t, err)
	var order []string
	for _, ev := range sim.EvaluatedRules {
		order = append(order, ev.ID)
	}
	assert.Equal(t, []string{"late-deny", "early-allow", "same-priority", "late-allow"}, order, "deny rules first, then by priority and position")
}

// TestHomeDirectories is TAR-06: the named group of the pattern compared
// with ${aws:username}: every user reaches only its own directory.
//
// covers: S-ACC-02
func TestHomeDirectories(t *testing.T) {
	f := newFixture(t)
	f.put(rule("home", 10, allow, list("*"), list("all"), regexP(`^home/(?P<user>[^/]+)/`),
		&accessrule.Conditions{CaptureEquals: map[string]string{"user": "${aws:username}"}}))
	mario, luigi := user("mario", nil), user("luigi", nil)
	svc := &domain.Identity{AccessKey: "svc-mario", Parent: "mario"}
	tests := []struct {
		id      *domain.Identity
		action  policy.Action
		key     string
		allowed bool
	}{
		{mario, policy.ActionGetObject, "home/mario/a.txt", true},
		{mario, policy.ActionPutObject, "home/mario/dir/b.txt", true},
		{mario, policy.ActionDeleteObject, "home/mario/b.txt", true},
		{mario, policy.ActionGetObject, "home/luigi/a.txt", false},
		{luigi, policy.ActionGetObject, "home/luigi/a.txt", true},
		{luigi, policy.ActionGetObject, "home/mario/a.txt", false},
		{luigi, policy.ActionGetObject, "home/luigi2/a.txt", false},
		{svc, policy.ActionGetObject, "home/mario/a.txt", true},
		{svc, policy.ActionGetObject, "home/svc-mario/a.txt", false},
		{domain.Anonymous(), policy.ActionGetObject, "home/mario/a.txt", false},
		{mario, policy.ActionGetObject, "home/x/../mario/a.txt", true},
		{luigi, policy.ActionGetObject, "home/luigi/../mario/a.txt", false},
	}
	for _, tt := range tests {
		d := f.evaluate(req{id: tt.id, action: tt.action, key: tt.key})
		assert.Equal(t, tt.allowed, d.Allowed, "%s %s %s: %s", tt.id.EffectiveUser(), tt.action, tt.key, d.Reason)
		if tt.allowed {
			assert.Equal(t, map[string]string{"user": tt.id.EffectiveUser()}, d.Rule.Captures)
		}
	}
}

// TestConditions is TAR-07 at the level of the engine: IP (IPv4, IPv6,
// not), referer, host, transport, time window, methods, headers with
// secrets, distributions, principals by group, access key and wildcard;
// a condition that cannot be evaluated never widens access.
//
// covers: S-ACC-02
func TestConditions(t *testing.T) {
	f := newFixture(t)
	f.put(
		rule("lan", 10, allow, list("*"), list("read"), prefixP("lan/"),
			&accessrule.Conditions{SourceIP: list("10.0.0.0/8", "2001:db8::/32")}),
		rule("not-blocked-net", 11, allow, list("*"), list("read"), prefixP("net/"),
			&accessrule.Conditions{NotSourceIP: list("203.0.113.0/24")}),
		rule("hotlink", 12, allow, list("*"), list("read"), prefixP("embed/"),
			&accessrule.Conditions{Referer: list("https://*.example.com/*")}),
		rule("cdn", 13, allow, list("*"), list("read"), prefixP("cdn/"),
			&accessrule.Conditions{Host: list("cdn.example.com", "*.cdn.example.org")}),
		rule("tls", 14, allow, list("*"), list("read"), prefixP("tls/"),
			&accessrule.Conditions{SecureTransport: ptr(true)}),
		rule("window", 15, allow, list("*"), list("read"), prefixP("event/"),
			&accessrule.Conditions{TimeWindow: &accessrule.TimeWindow{From: "2026-06-01T00:00:00Z", To: "2026-06-30T23:59:59Z"}}),
		rule("head-only", 16, allow, list("*"), list("read", "write"), prefixP("meth/"),
			&accessrule.Conditions{Methods: list("get", "HEAD")}),
		rule("proxy", 17, allow, list("*"), list("read"), prefixP("proxied/"),
			&accessrule.Conditions{Headers: map[string]string{"X-Origin-Secret": "${secret:proxy-token}"}}),
		rule("dist", 18, allow, list("distribution:web*"), list("read"), prefixP("site/"), nil),
		rule("keys", 19, allow, list("accesskey:AKIA*", "user:team-?"), list("read"), prefixP("team/"), nil),
		rule("deny-outside", 1, deny, list("*"), list("all"), prefixP("vault/"),
			&accessrule.Conditions{NotSourceIP: list("192.168.1.0/24")}),
		rule("deny-missing-secret", 2, deny, list("*"), list("all"), prefixP("sealed/"),
			&accessrule.Conditions{Headers: map[string]string{"X-Key": "${secret:unknown}"}}),
		rule("vault-and-sealed", 100, allow, list("*"), list("read"), regexP(`^(vault|sealed)/`), nil),
	)
	anon := domain.Anonymous()
	tests := []struct {
		name    string
		r       req
		allowed bool
	}{
		{"ipv4 inside", req{id: anon, key: "lan/a", ip: "10.1.2.3"}, true},
		{"ipv4 outside", req{id: anon, key: "lan/a", ip: "11.1.2.3"}, false},
		{"ipv6 inside", req{id: anon, key: "lan/a", ip: "2001:db8::1"}, true},
		{"ipv6 outside", req{id: anon, key: "lan/a", ip: "2001:db9::1"}, false},
		{"mapped ipv4 inside", req{id: anon, key: "lan/a", ip: "::ffff:10.0.0.1"}, true},
		{"ip unknown: allow not applied", req{id: anon, key: "lan/a"}, false},
		{"not ip: elsewhere", req{id: anon, key: "net/a", ip: "198.51.100.7"}, true},
		{"not ip: inside the excluded net", req{id: anon, key: "net/a", ip: "203.0.113.7"}, false},
		{"referer matches", req{id: anon, key: "embed/a.png", referer: "https://www.example.com/page"}, true},
		{"referer case-insensitive", req{id: anon, key: "embed/a.png", referer: "HTTPS://WWW.EXAMPLE.COM/x"}, true},
		{"referer other site", req{id: anon, key: "embed/a.png", referer: "https://evil.com/page"}, false},
		{"referer missing", req{id: anon, key: "embed/a.png"}, false},
		{"host exact", req{id: anon, key: "cdn/a", host: "cdn.example.com"}, true},
		{"host with port and case", req{id: anon, key: "cdn/a", host: "CDN.example.com:9000"}, true},
		{"host wildcard", req{id: anon, key: "cdn/a", host: "eu.cdn.example.org"}, true},
		{"host wildcard needs a subdomain", req{id: anon, key: "cdn/a", host: "cdn.example.org"}, false},
		{"host other", req{id: anon, key: "cdn/a", host: "s3.example.com"}, false},
		{"tls", req{id: anon, key: "tls/a", secure: ptr(true)}, true},
		{"plain http", req{id: anon, key: "tls/a", secure: ptr(false)}, false},
		{"transport unknown", req{id: anon, key: "tls/a"}, false},
		{"inside the window", req{id: anon, key: "event/a", now: time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC)}, true},
		{"window bound included", req{id: anon, key: "event/a", now: time.Date(2026, 6, 30, 23, 59, 59, 0, time.UTC)}, true},
		{"before the window", req{id: anon, key: "event/a", now: time.Date(2026, 5, 31, 23, 0, 0, 0, time.UTC)}, false},
		{"after the window", req{id: anon, key: "event/a", now: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)}, false},
		{"method GET", req{id: anon, key: "meth/a", method: "GET"}, true},
		{"method HEAD", req{id: anon, key: "meth/a", method: "head"}, true},
		{"method PUT", req{id: anon, key: "meth/a", method: "PUT", action: policy.ActionPutObject}, false},
		{"method derived from a read", req{id: anon, key: "meth/a"}, true},
		{"method derived from a write", req{id: anon, key: "meth/a", action: policy.ActionPutObject}, false},
		{"header with the secret", req{id: anon, key: "proxied/a", headers: map[string]string{"x-origin-secret": "s3cr3t"}}, true},
		{"header wrong", req{id: anon, key: "proxied/a", headers: map[string]string{"X-Origin-Secret": "guess"}}, false},
		{"header missing", req{id: anon, key: "proxied/a"}, false},
		{"distribution", req{id: anon, key: "site/index.html", dist: "website"}, true},
		{"other distribution", req{id: anon, key: "site/index.html", dist: "blog"}, false},
		{"no distribution", req{id: anon, key: "site/index.html"}, false},
		{"access key wildcard", req{id: &domain.Identity{AccessKey: "AKIA123", Parent: "x"}, key: "team/a"}, true},
		{"user wildcard", req{id: user("team-a", nil), key: "team/a"}, true},
		{"user wildcard too long", req{id: user("team-ab", nil), key: "team/a"}, false},
		{"deny applies outside the trusted net", req{id: anon, key: "vault/a", ip: "8.8.8.8"}, false},
		{"deny does not apply inside the trusted net", req{id: anon, key: "vault/a", ip: "192.168.1.9"}, true},
		{"deny applies when the ip is unknown", req{id: anon, key: "vault/a"}, false},
		{"deny applies when a secret is missing", req{id: anon, key: "sealed/a", headers: map[string]string{"X-Key": "x"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.r.now.IsZero() {
				tt.r.now = t0()
			}
			d := f.evaluate(tt.r)
			assert.Equal(t, tt.allowed, d.Allowed, d.Reason)
			sim, err := f.decide(tt.r, true)
			require.NoError(t, err)
			assert.Equal(t, d.Allowed, sim.Allowed, "simulator and evaluation agree")
			assert.Equal(t, d.Rule, sim.Rule)
		})
	}
}

// TestListingsAndBucketActions checks the key of the bucket actions: the
// listing prefix for the listings, the empty key otherwise.
func TestListingsAndBucketActions(t *testing.T) {
	f := newFixture(t)
	f.put(
		rule("list-pub", 10, allow, list("*"), list("list"), prefixP("pub/"), nil),
		rule("list-all-users", 11, allow, list("user:*"), list("list"), nil, nil),
		rule("config", 12, allow, list("user:ops"), list("s3:GetBucket*"), nil, nil),
	)
	anon := domain.Anonymous()
	assert.True(t, f.evaluate(req{id: anon, action: policy.ActionListBucket, prefix: ptr("pub/")}).Allowed)
	assert.True(t, f.evaluate(req{id: anon, action: policy.ActionListBucketVersions, prefix: ptr("pub/a/")}).Allowed)
	assert.False(t, f.evaluate(req{id: anon, action: policy.ActionListBucket, prefix: ptr("")}).Allowed)
	assert.False(t, f.evaluate(req{id: anon, action: policy.ActionListBucket}).Allowed)
	assert.False(t, f.evaluate(req{id: anon, action: policy.ActionListBucket, prefix: ptr("private/")}).Allowed)
	assert.True(t, f.evaluate(req{id: user("x", nil), action: policy.ActionListBucket}).Allowed)
	assert.False(t, f.evaluate(req{id: anon, action: policy.ActionGetObject, key: "pub/a"}).Allowed, "list does not read")
	assert.True(t, f.evaluate(req{id: user("ops", nil), action: policy.ActionGetBucketPolicy}).Allowed)
	assert.False(t, f.evaluate(req{id: user("ops", nil), action: policy.ActionPutBucketPolicy}).Allowed)
}

// TestRulesFromAnotherBucket checks that the rules of a bucket never apply
// to another one.
func TestRulesFromAnotherBucket(t *testing.T) {
	f := newFixture(t)
	f.put(rule("pub", 1, allow, list("*"), list("read"), nil, nil))
	d, err := f.policy.Evaluate(f.ctx, policy.Args{Identity: domain.Anonymous(), Action: policy.ActionGetObject, Bucket: "other", Object: "a"})
	require.NoError(t, err)
	assert.False(t, d.Allowed)
	assert.True(t, f.evaluate(req{id: domain.Anonymous(), key: "a"}).Allowed)
}
