package accessrule_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/accessrule"
	"github.com/r3n3r0/nexo/internal/service/audit"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/config"
	"github.com/r3n3r0/nexo/internal/service/match"
	"github.com/r3n3r0/nexo/internal/service/policy"
)

func TestNewRequiresDependencies(t *testing.T) {
	f := newFixture(t)
	_, err := accessrule.New(accessrule.Deps{Policy: f.policy, Matcher: f.matcher})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = accessrule.New(accessrule.Deps{Buckets: f.buckets, Matcher: f.matcher})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = accessrule.New(accessrule.Deps{Buckets: f.buckets, Policy: f.policy})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = accessrule.NewEvaluator(accessrule.EvaluatorDeps{Matcher: f.matcher})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = accessrule.NewEvaluator(accessrule.EvaluatorDeps{Source: f.source})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

// TestInvalidRules is TAR-02 for the rules: an invalid regex or glob is
// located (rule, field, position), a pattern too long or too many rules
// are refused; every problem is domain.ErrInvalidArgument.
func TestInvalidRules(t *testing.T) {
	f := newFixture(t, accessrule.WithMaxRules(3))
	valid := func(id string) accessrule.Rule { return rule(id, 1, allow, list("*"), list("read"), nil, nil) }
	long := strings.Repeat("a", match.MaxPatternLength+1)
	tests := []struct {
		name     string
		rules    []accessrule.Rule
		sentinel error
		index    int
		field    string
		position int
		msg      string
	}{
		{name: "regex with a missing paren", rules: []accessrule.Rule{valid("a"),
			rule("bad", 1, allow, list("*"), list("read"), regexP(`^img/(a|b`), nil)},
			sentinel: accessrule.ErrInvalidRule, index: 1, field: "match.pattern", position: 1, msg: "missing closing )"},
		{name: "regex with a backreference", rules: []accessrule.Rule{
			rule("bad", 1, allow, list("*"), list("read"), regexP(`^(a)\1`), nil)},
			sentinel: accessrule.ErrInvalidRule, index: 0, field: "match.pattern", position: 5, msg: "invalid escape"},
		{name: "glob with an open class", rules: []accessrule.Rule{
			rule("bad", 1, allow, list("*"), list("read"), globP("img/[abc"), nil)},
			sentinel: accessrule.ErrInvalidRule, field: "match.pattern", position: 5, msg: "unterminated character class"},
		{name: "pattern too long", rules: []accessrule.Rule{
			rule("bad", 1, allow, list("*"), list("read"), regexP(long), nil)},
			sentinel: accessrule.ErrInvalidRule, field: "match.pattern", position: match.MaxPatternLength + 1, msg: "longer than 1024"},
		{name: "unknown pattern type", rules: []accessrule.Rule{
			rule("bad", 1, allow, list("*"), list("read"), &match.Pattern{Type: "wild", Pattern: "x"}, nil)},
			sentinel: accessrule.ErrInvalidRule, field: "match.type", msg: "unknown pattern type"},
		{name: "empty pattern", rules: []accessrule.Rule{
			rule("bad", 1, allow, list("*"), list("read"), prefixP(""), nil)},
			sentinel: accessrule.ErrInvalidRule, field: "match.pattern", msg: "omit match"},
		{name: "too many rules", rules: []accessrule.Rule{valid("a"), valid("b"), valid("c"), valid("d")},
			sentinel: accessrule.ErrTooManyRules, index: -1, msg: "at most 3"},
		{name: "duplicate ids", rules: []accessrule.Rule{valid("a"), valid("a")},
			sentinel: accessrule.ErrInvalidRule, index: 1, field: "id", msg: "duplicate"},
		{name: "invalid id", rules: []accessrule.Rule{valid("-x")}, sentinel: accessrule.ErrInvalidRule, field: "id"},
		{name: "priority out of range", rules: []accessrule.Rule{rule("p", -1, allow, list("*"), list("read"), nil, nil)},
			sentinel: accessrule.ErrInvalidRule, field: "priority"},
		{name: "unknown effect", rules: []accessrule.Rule{rule("e", 1, "maybe", list("*"), list("read"), nil, nil)},
			sentinel: accessrule.ErrInvalidRule, field: "effect"},
		{name: "no principal", rules: []accessrule.Rule{rule("p", 1, allow, nil, list("read"), nil, nil)},
			sentinel: accessrule.ErrInvalidRule, field: "principals"},
		{name: "unknown principal", rules: []accessrule.Rule{rule("p", 1, allow, list("role:x"), list("read"), nil, nil)},
			sentinel: accessrule.ErrInvalidRule, field: "principals[0]"},
		{name: "empty principal name", rules: []accessrule.Rule{rule("p", 1, allow, list("user:"), list("read"), nil, nil)},
			sentinel: accessrule.ErrInvalidRule, field: "principals[0]"},
		{name: "no action", rules: []accessrule.Rule{rule("a", 1, allow, list("*"), nil, nil, nil)},
			sentinel: accessrule.ErrInvalidRule, field: "actions"},
		{name: "unknown action", rules: []accessrule.Rule{rule("a", 1, allow, list("*"), list("read", "admin:Heal"), nil, nil)},
			sentinel: accessrule.ErrInvalidRule, field: "actions[1]"},
		{name: "invalid cidr", rules: []accessrule.Rule{rule("c", 1, allow, list("*"), list("read"), nil,
			&accessrule.Conditions{SourceIP: list("10.0.0.0/8", "10.0.0.0/33")})},
			sentinel: accessrule.ErrInvalidRule, field: "conditions.sourceIp[1]"},
		{name: "invalid host", rules: []accessrule.Rule{rule("c", 1, allow, list("*"), list("read"), nil,
			&accessrule.Conditions{Host: list("cdn.example.com:443")})},
			sentinel: accessrule.ErrInvalidRule, field: "conditions.host[0]"},
		{name: "invalid time", rules: []accessrule.Rule{rule("c", 1, allow, list("*"), list("read"), nil,
			&accessrule.Conditions{TimeWindow: &accessrule.TimeWindow{From: "yesterday"}})},
			sentinel: accessrule.ErrInvalidRule, field: "conditions.timeWindow.from"},
		{name: "reversed time window", rules: []accessrule.Rule{rule("c", 1, allow, list("*"), list("read"), nil,
			&accessrule.Conditions{TimeWindow: &accessrule.TimeWindow{From: "2026-02-01T00:00:00Z", To: "2026-01-01T00:00:00Z"}})},
			sentinel: accessrule.ErrInvalidRule, field: "conditions.timeWindow"},
		{name: "invalid method", rules: []accessrule.Rule{rule("c", 1, allow, list("*"), list("read"), nil,
			&accessrule.Conditions{Methods: list("G E T")})},
			sentinel: accessrule.ErrInvalidRule, field: "conditions.methods[0]"},
		{name: "invalid header name", rules: []accessrule.Rule{rule("c", 1, allow, list("*"), list("read"), nil,
			&accessrule.Conditions{Headers: map[string]string{"X Bad": "v"}})},
			sentinel: accessrule.ErrInvalidRule, field: "conditions.headers.X Bad"},
		{name: "invalid secret reference", rules: []accessrule.Rule{rule("c", 1, allow, list("*"), list("read"), nil,
			&accessrule.Conditions{Headers: map[string]string{"X-Secret": "${secret:bad name}"}})},
			sentinel: accessrule.ErrInvalidRule, field: "conditions.headers.X-Secret"},
		{name: "unterminated reference", rules: []accessrule.Rule{rule("c", 1, allow, list("*"), list("read"), nil,
			&accessrule.Conditions{Headers: map[string]string{"X-Secret": "${secret:x"}})},
			sentinel: accessrule.ErrInvalidRule, field: "conditions.headers.X-Secret"},
		{name: "capture of an unknown group", rules: []accessrule.Rule{rule("c", 1, allow, list("*"), list("read"), regexP(`^home/(?P<user>[^/]+)/`),
			&accessrule.Conditions{CaptureEquals: map[string]string{"owner": "${aws:username}"}})},
			sentinel: accessrule.ErrInvalidRule, field: "conditions.captureEquals.owner", msg: "no named group"},
		{name: "capture without a regex", rules: []accessrule.Rule{rule("c", 1, allow, list("*"), list("read"), globP("home/*"),
			&accessrule.Conditions{CaptureEquals: map[string]string{"user": "${aws:username}"}})},
			sentinel: accessrule.ErrInvalidRule, field: "conditions.captureEquals.user"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := f.svc.Put(f.ctx, accessrule.PutInput{Bucket: testBucket, Rules: tt.rules})
			require.Error(t, err)
			assert.ErrorIs(t, err, tt.sentinel)
			assert.ErrorIs(t, err, domain.ErrInvalidArgument)
			var re *accessrule.RuleError
			require.ErrorAs(t, err, &re)
			assert.Equal(t, tt.index, re.Index, re.Error())
			if tt.field != "" {
				assert.Equal(t, tt.field, re.Field, re.Error())
			}
			assert.Equal(t, tt.position, re.Position, re.Error())
			if tt.position > 0 {
				assert.Contains(t, re.Error(), fmt.Sprintf("position %d", tt.position))
			}
			if tt.msg != "" {
				assert.Contains(t, re.Error(), tt.msg)
			}
			// Validate reports the same problem without saving.
			assert.ErrorIs(t, f.svc.Validate(f.ctx, testBucket, tt.rules), tt.sentinel)
		})
	}
	assert.Empty(t, f.st.rules, "nothing was saved")
	_, err := f.svc.Put(f.ctx, accessrule.PutInput{Bucket: "Bad_Bucket", Rules: nil})
	assert.ErrorIs(t, err, domain.ErrInvalidBucketName)
}

// TestPutCanonicalizesAndRoundTrips checks the canonical form of the
// stored rules, the ETag, Get and the If-Match of an atomic replacement.
func TestPutCanonicalizesAndRoundTrips(t *testing.T) {
	f := newFixture(t)
	in := accessrule.Rule{
		ID: " r1 ", Priority: 3, Effect: "ALLOW", Principals: []string{"User:mario", "*", "*"}, Actions: []string{"Read", "s3:GetObjectTagging"},
		Match:      &match.Pattern{Type: "GLOB", Pattern: "docs/**"},
		Conditions: &accessrule.Conditions{Methods: list("get"), Host: list("CDN.Example.com")},
	}
	cfg, err := f.svc.Put(f.ctx, accessrule.PutInput{Bucket: testBucket, Rules: []accessrule.Rule{in}, Actor: "admin"})
	require.NoError(t, err)
	require.Len(t, cfg.Rules, 1)
	got := cfg.Rules[0]
	assert.Equal(t, "r1", got.ID)
	assert.Equal(t, allow, got.Effect)
	assert.Equal(t, []string{"user:mario", "*"}, got.Principals)
	assert.Equal(t, []string{"read", "s3:GetObjectTagging"}, got.Actions)
	assert.Equal(t, match.TypeGlob, got.Match.Type)
	assert.NotEmpty(t, cfg.ETag)
	assert.Equal(t, "admin", cfg.UpdatedBy)
	assert.True(t, cfg.Public)

	again, err := f.svc.Get(f.ctx, testBucket)
	require.NoError(t, err)
	assert.Equal(t, cfg.ETag, again.ETag)
	assert.Equal(t, cfg.Rules, again.Rules)

	_, err = f.svc.Put(f.ctx, accessrule.PutInput{Bucket: testBucket, Rules: nil, IfMatch: "stale"})
	assert.ErrorIs(t, err, domain.ErrPreconditionFailed)
	empty, err := f.svc.Put(f.ctx, accessrule.PutInput{Bucket: testBucket, Rules: nil, IfMatch: cfg.ETag})
	require.NoError(t, err)
	assert.Empty(t, empty.Rules)
	assert.Equal(t, bucket.AccessRulesETag(nil), empty.ETag)
	assert.False(t, empty.Public)

	_, err = f.svc.Get(f.ctx, "missing")
	assert.ErrorIs(t, err, domain.ErrBucketNotFound)
}

// TestAuditDiff checks that every change is audited with who and the diff.
func TestAuditDiff(t *testing.T) {
	f := newFixture(t)
	var entries []audit.Entry
	f.audit.ExpectedCalls = nil
	f.audit.EXPECT().Log(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, e audit.Entry) error {
		entries = append(entries, e)
		return nil
	})
	f.put(rule("a", 1, allow, list("*"), list("read"), nil, nil), rule("b", 2, deny, list("*"), list("all"), prefixP("x/"), nil))
	f.put(rule("a", 1, allow, list("*"), list("read", "list"), nil, nil), rule("c", 3, allow, list("user:x"), list("all"), nil, nil))
	_, err := f.svc.SetBlockPublic(f.ctx, accessrule.BlockPublicInput{Bucket: testBucket, Enabled: true, Actor: "sec"})
	require.NoError(t, err)

	require.Len(t, entries, 3)
	assert.Equal(t, "PutAccessRules", entries[1].API.Name)
	assert.Equal(t, testBucket, entries[1].API.Bucket)
	assert.Equal(t, "admin", entries[1].AccessKey)
	assert.Equal(t, t0(), entries[1].Time)
	raw, err := json.Marshal(entries[1].Tags["diff"])
	require.NoError(t, err)
	var diff struct {
		Added   []accessrule.Rule `json:"added"`
		Removed []accessrule.Rule `json:"removed"`
		Changed []struct {
			ID     string          `json:"id"`
			Before accessrule.Rule `json:"before"`
			After  accessrule.Rule `json:"after"`
		} `json:"changed"`
	}
	require.NoError(t, json.Unmarshal(raw, &diff))
	require.Len(t, diff.Added, 1)
	assert.Equal(t, "c", diff.Added[0].ID)
	require.Len(t, diff.Removed, 1)
	assert.Equal(t, "b", diff.Removed[0].ID)
	require.Len(t, diff.Changed, 1)
	assert.Equal(t, "a", diff.Changed[0].ID)
	assert.Equal(t, []string{"read"}, diff.Changed[0].Before.Actions)
	assert.Equal(t, []string{"read", "list"}, diff.Changed[0].After.Actions)

	assert.Equal(t, "PutBlockPublicAccess", entries[2].API.Name)
	assert.Equal(t, "sec", entries[2].AccessKey)
}

// TestBlockPublicAccess is TAR-08: with the block public access on, the
// allow rules for everyone are refused when saved (reject) or ignored in
// the evaluation (ignore, and the rules stored before a reject); deny
// rules and the other principals keep applying; the tester reports it.
func TestBlockPublicAccess(t *testing.T) {
	public := rule("public", 10, allow, list("*", "user:mario"), list("read"), nil, nil)
	guard := rule("guard", 1, deny, list("*"), list("all"), prefixP("secret/"), nil)
	anon, mario := domain.Anonymous(), user("mario", nil)

	t.Run("cluster reject", func(t *testing.T) {
		f := newFixture(t, accessrule.WithBlockPublicDefault(true, accessrule.BlockModeReject))
		bp, err := f.svc.GetBlockPublic(f.ctx, "")
		require.NoError(t, err)
		assert.Equal(t, accessrule.BlockPublic{Enabled: true, Mode: accessrule.BlockModeReject, Cluster: true, Effective: true}, *bp)
		_, err = f.svc.Put(f.ctx, accessrule.PutInput{Bucket: testBucket, Rules: []accessrule.Rule{guard, public}})
		assert.ErrorIs(t, err, accessrule.ErrPublicAccessBlocked)
		assert.ErrorIs(t, err, domain.ErrAccessDenied)
		var re *accessrule.RuleError
		require.ErrorAs(t, err, &re)
		assert.Equal(t, 1, re.Index)
		assert.Contains(t, re.Error(), "cluster")
		// Deny rules for everyone are welcome.
		f.put(guard)
		_, err = f.svc.ApplyPreset(f.ctx, accessrule.PresetInput{Bucket: testBucket, Preset: accessrule.PresetPublicRead})
		assert.ErrorIs(t, err, accessrule.ErrPublicAccessBlocked)
	})

	t.Run("bucket ignore", func(t *testing.T) {
		f := newFixture(t, accessrule.WithBlockPublicDefault(false, accessrule.BlockModeIgnore))
		f.put(guard, public)
		assert.True(t, f.evaluate(req{id: anon, key: "a"}).Allowed)
		bp, err := f.svc.SetBlockPublic(f.ctx, accessrule.BlockPublicInput{Bucket: testBucket, Enabled: true})
		require.NoError(t, err)
		assert.Equal(t, accessrule.BlockPublic{Bucket: testBucket, Enabled: true, Mode: accessrule.BlockModeIgnore, Effective: true}, *bp)
		blocked, err := f.svc.PublicAccessBlocked(f.ctx, testBucket)
		require.NoError(t, err)
		assert.True(t, blocked)

		d := f.evaluate(req{id: anon, key: "a"})
		assert.False(t, d.Allowed, "the principal * is ignored")
		assert.True(t, f.evaluate(req{id: mario, key: "a"}).Allowed, "the other principals still apply")
		assert.False(t, f.evaluate(req{id: mario, key: "secret/a"}).Allowed, "deny rules for everyone still apply")
		other, err := f.svc.PublicAccessBlocked(f.ctx, "other")
		require.NoError(t, err)
		assert.False(t, other, "per bucket")

		cfg, err := f.svc.Put(f.ctx, accessrule.PutInput{Bucket: testBucket, Rules: []accessrule.Rule{guard, public}})
		require.NoError(t, err, "accepted in the ignore mode")
		assert.False(t, cfg.Public)
		assert.True(t, cfg.BlockPublic)
		require.Len(t, cfg.Warnings, 1)
		assert.Contains(t, cfg.Warnings[0], "ignored")

		res, err := f.svc.Test(f.ctx, accessrule.TestInput{Bucket: testBucket, Keys: list("a")})
		require.NoError(t, err)
		assert.True(t, res.BlockPublic.Effective)
		require.Len(t, res.Results, 1)
		assert.Equal(t, []string{"public"}, res.Results[0].BlockedPublic)
		assert.False(t, res.Results[0].Allowed)
		assert.Equal(t, accessrule.OutcomeNone, res.Results[0].RulesOutcome)

		status, err := f.svc.PublicStatus(f.ctx, testBucket)
		require.NoError(t, err)
		assert.True(t, status.Blocked)
		assert.False(t, status.Public)

		_, err = f.svc.SetBlockPublic(f.ctx, accessrule.BlockPublicInput{Bucket: testBucket, Enabled: false})
		require.NoError(t, err)
		assert.True(t, f.evaluate(req{id: anon, key: "a"}).Allowed, "turned off at once")
	})

	t.Run("rules stored before a reject are ignored", func(t *testing.T) {
		f := newFixture(t)
		f.put(public)
		assert.True(t, f.evaluate(req{id: anon, key: "a"}).Allowed)
		bp, err := f.svc.SetBlockPublic(f.ctx, accessrule.BlockPublicInput{Enabled: true, Mode: accessrule.BlockModeReject, Actor: "sec"})
		require.NoError(t, err)
		assert.True(t, bp.Effective)
		assert.Equal(t, accessrule.BlockModeReject, bp.Mode)
		kv, err := f.config.Get(f.ctx, accessrule.ConfigSubsystem)
		require.NoError(t, err)
		assert.Equal(t, config.KV{"block_public": "on", "block_public_mode": "reject"}, kv)
		assert.False(t, f.evaluate(req{id: anon, key: "a"}).Allowed)
		bucketLevel, err := f.svc.GetBlockPublic(f.ctx, testBucket)
		require.NoError(t, err)
		assert.Equal(t, accessrule.BlockPublic{Bucket: testBucket, Mode: accessrule.BlockModeReject, Cluster: true, Effective: true}, *bucketLevel)

		_, err = f.svc.SetBlockPublic(f.ctx, accessrule.BlockPublicInput{Enabled: false})
		require.NoError(t, err)
		assert.True(t, f.evaluate(req{id: anon, key: "a"}).Allowed)

		_, err = f.svc.SetBlockPublic(f.ctx, accessrule.BlockPublicInput{Enabled: true, Mode: "maybe"})
		assert.ErrorIs(t, err, domain.ErrInvalidArgument)
		_, err = f.svc.SetBlockPublic(f.ctx, accessrule.BlockPublicInput{Bucket: testBucket, Enabled: true, Mode: accessrule.BlockModeIgnore})
		assert.ErrorIs(t, err, domain.ErrInvalidArgument, "the mode is a cluster setting")
	})

	t.Run("unreadable cluster setting blocks", func(t *testing.T) {
		f := newFixture(t)
		f.put(public)
		require.NoError(t, f.config.Set(f.ctx, accessrule.ConfigSubsystem, config.KV{"block_public": "perhaps"}))
		assert.False(t, f.evaluate(req{id: anon, key: "a"}).Allowed)
	})
}

// TestPresets checks the public presets, their replacement and the
// public status of the bucket.
func TestPresets(t *testing.T) {
	f := newFixture(t)
	anon := domain.Anonymous()
	f.put(rule("mine", 1, allow, list("user:mario"), list("all"), nil, nil), rule("custom-public", 2, allow, list("*"), list("read"), prefixP("c/"), nil))

	status, err := f.svc.PublicStatus(f.ctx, testBucket)
	require.NoError(t, err)
	assert.Equal(t, accessrule.PresetCustom, status.Preset)
	assert.True(t, status.Public)

	apply := func(in accessrule.PresetInput) *accessrule.Config {
		t.Helper()
		in.Bucket = testBucket
		cfg, err := f.svc.ApplyPreset(f.ctx, in)
		require.NoError(t, err)
		return cfg
	}
	cfg := apply(accessrule.PresetInput{Preset: accessrule.PresetPrivate})
	assert.Equal(t, []string{"mine"}, ids(cfg.Rules), "private removes every public allow rule")
	assert.False(t, f.evaluate(req{id: anon, key: "c/a"}).Allowed)
	status, err = f.svc.PublicStatus(f.ctx, testBucket)
	require.NoError(t, err)
	assert.Equal(t, accessrule.PublicStatus{Bucket: testBucket, Preset: accessrule.PresetPrivate}, *status)

	cfg = apply(accessrule.PresetInput{Preset: accessrule.PresetPublicRead})
	assert.Equal(t, []string{"mine", "preset-public-read"}, ids(cfg.Rules))
	assert.True(t, f.evaluate(req{id: anon, key: "any/key"}).Allowed)
	assert.False(t, f.evaluate(req{id: anon, action: policy.ActionListBucket}).Allowed)
	assertPreset(t, f, accessrule.PresetPublicRead)

	cfg = apply(accessrule.PresetInput{Preset: accessrule.PresetPublicReadList})
	assert.Equal(t, []string{"mine", "preset-public-read-list"}, ids(cfg.Rules), "a preset replaces the previous one")
	assert.True(t, f.evaluate(req{id: anon, action: policy.ActionListBucket}).Allowed)
	assertPreset(t, f, accessrule.PresetPublicReadList)

	apply(accessrule.PresetInput{Preset: accessrule.PresetPublicPrefix, Match: globP("pub/**")})
	assert.True(t, f.evaluate(req{id: anon, key: "pub/a/b.txt"}).Allowed)
	assert.False(t, f.evaluate(req{id: anon, key: "priv/a.txt"}).Allowed)
	assert.False(t, f.evaluate(req{id: anon, action: policy.ActionListBucket}).Allowed)
	assertPreset(t, f, accessrule.PresetPublicPrefix)

	apply(accessrule.PresetInput{Preset: accessrule.PresetPublicPrefix, Match: regexP(`^pub/[0-9]+\.png$`)})
	assert.True(t, f.evaluate(req{id: anon, key: "pub/12.png"}).Allowed)
	assert.False(t, f.evaluate(req{id: anon, key: "pub/a.png"}).Allowed)

	_, err = f.svc.ApplyPreset(f.ctx, accessrule.PresetInput{Bucket: testBucket, Preset: accessrule.PresetPublicPrefix})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = f.svc.ApplyPreset(f.ctx, accessrule.PresetInput{Bucket: testBucket, Preset: accessrule.PresetPublicWrite})
	assert.ErrorIs(t, err, accessrule.ErrConfirmationRequired)
	_, err = f.svc.ApplyPreset(f.ctx, accessrule.PresetInput{Bucket: testBucket, Preset: "public"})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)

	cfg = apply(accessrule.PresetInput{Preset: accessrule.PresetPublicWrite, Confirm: true, Match: prefixP("inbox/")})
	assert.True(t, f.evaluate(req{id: anon, action: policy.ActionPutObject, key: "inbox/a"}).Allowed)
	assert.False(t, f.evaluate(req{id: anon, action: policy.ActionPutObject, key: "other/a"}).Allowed)
	assert.False(t, f.evaluate(req{id: anon, key: "inbox/a"}).Allowed, "write only")
	require.Len(t, cfg.Warnings, 1)
	assert.Contains(t, cfg.Warnings[0], "write")
	assertPreset(t, f, accessrule.PresetPublicWrite)

	_, err = f.svc.ApplyPreset(f.ctx, accessrule.PresetInput{Bucket: testBucket, Preset: accessrule.PresetPrivate, IfMatch: "stale"})
	assert.ErrorIs(t, err, domain.ErrPreconditionFailed)
}

func assertPreset(t *testing.T, f *fixture, want accessrule.Preset) {
	t.Helper()
	status, err := f.svc.PublicStatus(f.ctx, testBucket)
	require.NoError(t, err)
	assert.Equal(t, want, status.Preset)
	assert.True(t, status.Public)
	assert.Len(t, status.Rules, 1)
}

func ids(rules []accessrule.Rule) []string {
	out := []string{}
	for _, r := range rules {
		out = append(out, r.ID)
	}
	return out
}

// TestTester checks the rule tester (D-ACC-02, nexoctl access test): the
// rules that match each key, the outcome of the rules, the final decision
// with every source and the deciding rule.
func TestTester(t *testing.T) {
	f := newFixture(t)
	f.put(
		rule("img-pub", 10, allow, list("*"), list("read"), regexP(`^img/[0-9]{4}/.*\.(jpe?g|png)$`), nil),
		rule("no-private", 5, deny, list("*"), list("all"), globP("**/private/**"), nil),
		rule("cdn-only", 20, allow, list("*"), list("read"), prefixP("cdn/"), &accessrule.Conditions{Host: list("cdn.example.com"), SourceIP: list("1.2.3.0/24")}),
	)
	res, err := f.svc.Test(f.ctx, accessrule.TestInput{
		Bucket: testBucket, Keys: list("img/2026/a.jpg", "img/2026/private/a.jpg", "cdn/x", "docs/a", "img/2026//./b.png"),
		IP: "1.2.3.4", Host: "cdn.example.com",
	})
	require.NoError(t, err)
	assert.Equal(t, "s3:GetObject", res.Action)
	require.Len(t, res.Results, 5)

	r := res.Results[0]
	assert.True(t, r.Allowed)
	assert.Equal(t, []string{"img-pub"}, r.Matched)
	assert.Equal(t, accessrule.OutcomeAllow, r.RulesOutcome)
	assert.Equal(t, "img-pub", r.Determinant)
	assert.Equal(t, policy.SourceAccessRule, r.Source)
	require.Len(t, r.Rules, 3)
	assert.Equal(t, "no-private", r.Rules[0].ID, "deny rules first")
	assert.Equal(t, policy.RuleMismatchKey, r.Rules[0].Mismatch)
	assert.Equal(t, policy.RuleMismatchKey, r.Rules[2].Mismatch)

	r = res.Results[1]
	assert.False(t, r.Allowed)
	assert.Equal(t, []string{"no-private", "img-pub"}, r.Matched)
	assert.Equal(t, accessrule.OutcomeDeny, r.RulesOutcome)
	assert.Equal(t, "no-private", r.Determinant)

	r = res.Results[2]
	assert.True(t, r.Allowed)
	assert.Equal(t, "cdn-only", r.Determinant)

	r = res.Results[3]
	assert.False(t, r.Allowed)
	assert.Empty(t, r.Matched)
	assert.Equal(t, accessrule.OutcomeNone, r.RulesOutcome)
	assert.Empty(t, r.Determinant)

	r = res.Results[4]
	assert.Equal(t, "img/2026/b.png", r.NormalizedKey)
	assert.True(t, r.Allowed)

	// Another host or address: the condition that failed is named.
	res, err = f.svc.Test(f.ctx, accessrule.TestInput{Bucket: testBucket, Keys: list("cdn/x"), IP: "1.2.3.4", Host: "www.example.com"})
	require.NoError(t, err)
	r = res.Results[0]
	assert.False(t, r.Allowed)
	assert.Equal(t, policy.RuleMismatchCondition, r.Rules[2].Mismatch)
	assert.Equal(t, "host", r.Rules[2].Condition)

	// An identity with IAM permissions: the IAM policy decides.
	res, err = f.svc.Test(f.ctx, accessrule.TestInput{Bucket: testBucket, Keys: list("docs/a"), Identity: user("x", list("readonly"))})
	require.NoError(t, err)
	assert.True(t, res.Results[0].Allowed)
	assert.Equal(t, policy.SourceIAM, res.Results[0].Source)

	// Listing: the key is the prefix.
	res, err = f.svc.Test(f.ctx, accessrule.TestInput{Bucket: testBucket, Action: "list", Keys: list("x/private/")})
	require.NoError(t, err)
	assert.Equal(t, "s3:ListBucket", res.Action)
	assert.Equal(t, "no-private", res.Results[0].Determinant)

	_, err = f.svc.Test(f.ctx, accessrule.TestInput{Bucket: testBucket, Action: "s3:Get*", Keys: list("a")})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = f.svc.Test(f.ctx, accessrule.TestInput{Bucket: testBucket})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument, "an object action needs a key")
	_, err = f.svc.Test(f.ctx, accessrule.TestInput{Bucket: "missing", Keys: list("a")})
	assert.ErrorIs(t, err, domain.ErrBucketNotFound)
}

// TestParse checks the YAML and JSON documents of rules (docs/10 §10.1).
func TestParse(t *testing.T) {
	f := newFixture(t)
	yamlDoc := `
bucket: media
rules:
  - id: immagini-pubbliche
    priority: 10                 # ordine di valutazione
    effect: allow
    principals: ["*"]
    actions: [read]
    match:
      type: regex
      pattern: '^img/[0-9]{4}/.*\.(jpe?g|png|webp)$'
      caseInsensitive: true
    conditions:
      sourceIp: ["10.0.0.0/8", "2001:db8::/32"]
      notSourceIp: []
      referer: ["https://*.example.com/*"]
      host: ["cdn.example.com"]
      secureTransport: true
      timeWindow: { from: "2026-01-01T00:00:00Z", to: "2026-12-31T23:59:59Z" }
      methods: [GET, HEAD]
      headers: { "X-Origin-Secret": "${secret:proxy-token}" }
  - id: blocca-privati
    priority: 5
    effect: deny
    principals: ["*"]
    actions: [all]
    match: { type: glob, pattern: "**/private/**" }
  - id: home
    priority: 20
    effect: allow
    principals: ["*"]
    actions: [all]
    match: { type: regex, pattern: '^home/(?P<user>[^/]+)/' }
    conditions: { captureEquals: { user: "${aws:username}" } }
`
	doc, err := f.svc.Parse(f.ctx, []byte(yamlDoc))
	require.NoError(t, err)
	assert.Equal(t, "media", doc.Bucket)
	require.Len(t, doc.Rules, 3)
	assert.True(t, doc.Rules[0].Match.CaseInsensitive)
	assert.Equal(t, ptr(true), doc.Rules[0].Conditions.SecureTransport)
	assert.Equal(t, "2026-12-31T23:59:59Z", doc.Rules[0].Conditions.TimeWindow.To)
	assert.Equal(t, map[string]string{"user": "${aws:username}"}, doc.Rules[2].Conditions.CaptureEquals)

	// The same rules as JSON, as exported, then saved.
	data, err := json.Marshal(doc)
	require.NoError(t, err)
	again, err := f.svc.Parse(f.ctx, data)
	require.NoError(t, err)
	assert.Equal(t, doc, again)
	cfg := f.put(again.Rules...)
	assert.Equal(t, doc.Rules, cfg.Rules)

	bare, err := f.svc.Parse(f.ctx, []byte(`[{"id":"a","priority":1,"effect":"deny","principals":["*"],"actions":["all"]}]`))
	require.NoError(t, err)
	assert.Len(t, bare.Rules, 1)
	empty, err := f.svc.Parse(f.ctx, nil)
	require.NoError(t, err)
	assert.Empty(t, empty.Rules)

	_, err = f.svc.Parse(f.ctx, []byte("rules:\n  - id: a\n    priority: 1\n    effect: allow\n    principals: ['*']\n    actions: [read]\n    matc: {type: glob}\n"))
	require.Error(t, err)
	assert.ErrorIs(t, err, accessrule.ErrInvalidRule)
	assert.Contains(t, err.Error(), "line 7")
	_, err = f.svc.Parse(f.ctx, []byte("rules:\n  - id: a\n    priority: 1\n    effect: allow\n    principals: ['*']\n    actions: [read]\n    match: {type: regex, pattern: '(x'}\n"))
	var re *accessrule.RuleError
	require.ErrorAs(t, err, &re)
	assert.Equal(t, "match.pattern", re.Field)
	_, err = f.svc.Parse(f.ctx, []byte(`"just a string"`))
	assert.ErrorIs(t, err, accessrule.ErrInvalidRule)
}

// TestEvaluatorCachesPerVersion checks that the compiled rules follow the
// ETag of the stored document: a change (on any node: the bucket metadata
// is invalidated) is compiled at the next request.
func TestEvaluatorCachesPerVersion(t *testing.T) {
	f := newFixture(t)
	anon := domain.Anonymous()
	assert.False(t, f.evaluate(req{id: anon, key: "a"}).Allowed)
	f.put(rule("pub", 1, allow, list("*"), list("read"), nil, nil))
	assert.True(t, f.evaluate(req{id: anon, key: "a"}).Allowed)
	assert.True(t, f.evaluate(req{id: anon, key: "b"}).Allowed)
	f.put(rule("pub", 1, allow, list("*"), list("read"), prefixP("b"), nil))
	assert.False(t, f.evaluate(req{id: anon, key: "a"}).Allowed, "the new version applies at once")
	assert.True(t, f.evaluate(req{id: anon, key: "b"}).Allowed)
	f.put()
	assert.False(t, f.evaluate(req{id: anon, key: "b"}).Allowed)

	// A stored document that no longer validates fails closed.
	f.st.mu.Lock()
	f.st.rules[testBucket] = bucket.AccessRules{Document: []byte(`[{"id":"x"}]`), ETag: "corrupt"}
	f.st.mu.Unlock()
	_, err := f.decide(req{id: anon, key: "b"}, false)
	assert.ErrorIs(t, err, domain.ErrInvalidFormat)
}

// TestSecrets checks the static secret store.
func TestSecrets(t *testing.T) {
	s := accessrule.NewStaticSecrets(map[string]string{"PROXY_TOKEN": "a", "other.one": "b"})
	v, err := s.Secret(context.Background(), "proxy-token")
	require.NoError(t, err)
	assert.Equal(t, "a", v)
	v, err = s.Secret(context.Background(), "OTHER-ONE")
	require.NoError(t, err)
	assert.Equal(t, "b", v)
	_, err = s.Secret(context.Background(), "missing")
	assert.ErrorIs(t, err, accessrule.ErrSecretNotFound)
	assert.True(t, errors.Is(err, domain.ErrNotFound))
}
