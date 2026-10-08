package cli_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/policy"
)

// The anonymous presets of nexoctl duplicate the logic of the server
// (policy.Service: GeneratePublicPolicy, ApplyPublicPreset, DetectPreset,
// ListPublicAccess) because the CLI does not link the policy engine. These
// tests keep the two in step: same documents, same detections.

func policyEngine() policy.Service {
	return policy.New(policy.Deps{Logger: slog.New(slog.DiscardHandler)})
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

func handWrittenPolicies(t *testing.T) []*domain.Policy {
	t.Helper()
	docs := []string{
		`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":["s3:GetObject","s3:ListBucket"],"Resource":["arn:aws:s3:::bkt","arn:aws:s3:::bkt/*"]}]}`,
		`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":["*"]},"Action":"s3:*","Resource":"arn:aws:s3:::bkt/*"}]}`,
		`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":["arn:aws:iam::123:user/x"]},"Action":"s3:GetObject","Resource":"arn:aws:s3:::bkt/*"}]}`,
		`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::bkt/pub/*"},{"Effect":"Allow","Principal":"*","Action":"s3:ListBucket","Resource":"arn:aws:s3:::bkt","Condition":{"StringLike":{"s3:prefix":["pub/*"]}}}]}`,
		`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":["s3:GetObject","s3:ListBucket","s3:PutObject"],"Resource":["arn:aws:s3:::bkt","arn:aws:s3:::bkt/*"]},{"Effect":"Deny","Principal":"*","Action":"s3:*","Resource":"arn:aws:s3:::bkt/secret/*"}]}`,
		`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::bkt/*","Condition":{"IpAddress":{"aws:SourceIp":"10.0.0.0/8"}}}]}`,
		`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::bkt/*","Condition":{"NotIpAddress":{"aws:SourceIp":"10.0.0.0/8"}}}]}`,
		`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","NotAction":"s3:DeleteObject","Resource":["arn:aws:s3:::bkt","arn:aws:s3:::bkt/*"]}]}`,
		`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","NotResource":"arn:aws:s3:::bkt/private/*"}]}`,
		`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","NotPrincipal":{"AWS":["arn:aws:iam::1:user/a"]},"Action":"s3:PutObject","Resource":"arn:aws:s3:::bkt/inbox/*"}]}`,
		`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::bkt/home/${aws:username}/*"}]}`,
		`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":["s3:Get*","s3:List*"],"Resource":["arn:aws:s3:::bkt","arn:aws:s3:::bkt/data/2026/*"],"Condition":{"StringEquals":{"s3:prefix":["data/2026/"]}}}]}`,
		`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::bkt/*","Condition":{"DateLessThan":{"aws:CurrentTime":"2000-01-01T00:00:00Z"}}}]}`,
		`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":["s3:ListBucket","s3:GetObject"],"Resource":["arn:aws:s3:::bkt","arn:aws:s3:::bkt/*"],"Condition":{"StringNotEquals":{"s3:prefix":["hidden/"]}}}]}`,
		`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:PutObject","Resource":"arn:aws:s3:::bkt/up?/*"}]}`,
	}
	out := make([]*domain.Policy, 0, len(docs))
	for _, d := range docs {
		p, err := cli.ParseBucketPolicy(d)
		require.NoError(t, err, d)
		out = append(out, p)
	}
	return out
}

func probePrefixes() []string {
	return []string{"", "pub/", "pub", "secret/", "secret/x/", "private/", "inbox/", "home/", "data/", "data/2026/", "hidden/", "up1/", "other/"}
}

func TestAnonymousPresetsMatchThePolicyEngine(t *testing.T) {
	ctx := context.Background()
	svc := policyEngine()
	presets := []cli.AnonymousPreset{cli.AnonymousPrivate, cli.AnonymousDownload, cli.AnonymousUpload, cli.AnonymousPublic}
	currents := append([]*domain.Policy{nil}, handWrittenPolicies(t)...)
	for ci, current := range currents {
		for _, prefix := range []string{"", "pub/", "a/b/"} {
			for _, preset := range presets {
				t.Run(fmt.Sprintf("apply/%d/%q/%s", ci, prefix, preset), func(t *testing.T) {
					got, err := cli.ApplyAnonymousPreset(current, "bkt", prefix, preset)
					require.NoError(t, err)
					want, err := svc.ApplyPublicPreset(ctx, current, "bkt", prefix, policy.Preset(preset))
					require.NoError(t, err)
					assert.Equal(t, mustJSON(t, want), mustJSON(t, got))
					if current == nil {
						gen, err := svc.GeneratePublicPolicy(ctx, "bkt", prefix, policy.Preset(preset))
						require.NoError(t, err)
						assert.Equal(t, mustJSON(t, gen), mustJSON(t, got))
					}
					if current == nil && got != nil {
						assert.Equal(t, string(preset), string(cli.DetectAnonymousPreset(got, "bkt", prefix)), "a preset is detected as itself")
					}
				})
			}
		}
	}
}

func TestAnonymousDetectionMatchesThePolicyEngine(t *testing.T) {
	ctx := context.Background()
	svc := policyEngine()
	corpus := handWrittenPolicies(t)
	// Random combinations of presets on prefixes, on top of hand-written
	// policies.
	rng := rand.New(rand.NewPCG(1, 2))
	presets := []cli.AnonymousPreset{cli.AnonymousPrivate, cli.AnonymousDownload, cli.AnonymousUpload, cli.AnonymousPublic}
	prefixes := probePrefixes()
	for range 60 {
		var p *domain.Policy
		if rng.IntN(2) == 0 {
			p = corpus[rng.IntN(len(corpus))]
		}
		for range 1 + rng.IntN(3) {
			next, err := cli.ApplyAnonymousPreset(p, "bkt", prefixes[rng.IntN(len(prefixes))], presets[rng.IntN(len(presets))])
			require.NoError(t, err)
			p = next
		}
		if p != nil {
			corpus = append(corpus, p)
		}
	}
	for i, p := range corpus {
		for _, prefix := range prefixes {
			got := cli.DetectAnonymousPreset(p, "bkt", prefix)
			want := svc.DetectPreset(ctx, p, "bkt", prefix)
			assert.Equal(t, string(want), string(got), "policy %d %s prefix %q", i, mustJSON(t, p), prefix)
		}
		got := cli.ListAnonymousAccess(p, "bkt")
		want := svc.ListPublicAccess(ctx, p, "bkt")
		require.Len(t, got, len(want), "policy %d %s", i, mustJSON(t, p))
		for j := range want {
			assert.Equal(t, want[j].Prefix, got[j].Prefix)
			assert.Equal(t, string(want[j].Preset), string(got[j].Preset))
		}
	}
}

func TestAnonymousArguments(t *testing.T) {
	for _, s := range []string{"private", "DOWNLOAD", "upload", "public"} {
		_, err := cli.ParseAnonymousPreset(s)
		assert.NoError(t, err, s)
	}
	_, err := cli.ParseAnonymousPreset("readonly")
	assert.ErrorIs(t, err, cli.ErrUsage)
	for _, bad := range [][2]string{{"", ""}, {"b/x", ""}, {"bkt", "a*"}, {"bkt", "/a"}, {"bkt", "${x}"}} {
		_, err := cli.ApplyAnonymousPreset(nil, bad[0], bad[1], cli.AnonymousDownload)
		assert.ErrorIs(t, err, cli.ErrUsage, bad)
	}
	p, err := cli.ApplyAnonymousPreset(nil, "bkt", "", cli.AnonymousPrivate)
	require.NoError(t, err)
	assert.Nil(t, p, "private on an empty policy leaves no policy")
	assert.Equal(t, cli.AnonymousPrivate, cli.DetectAnonymousPreset(nil, "bkt", ""))
	assert.Nil(t, cli.ListAnonymousAccess(nil, "bkt"))
	_, err = cli.ParseBucketPolicy("{bad")
	assert.ErrorIs(t, err, domain.ErrMalformedPolicy)
}
