package iam

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
)

func TestKDFParams(t *testing.T) {
	assert.Equal(t, defaultKDFParams(), KDFParams{}.withDefaults())
	custom := KDFParams{Time: 2, MemoryKiB: 128, Threads: 2}
	assert.Equal(t, custom, custom.withDefaults())
	require.NoError(t, defaultKDFParams().validate())
	for _, bad := range []KDFParams{
		{Time: 0, MemoryKiB: 64, Threads: 1},
		{Time: 17, MemoryKiB: 64, Threads: 1},
		{Time: 1, MemoryKiB: 64, Threads: 0},
		{Time: 1, MemoryKiB: 64, Threads: 65},
		{Time: 1, MemoryKiB: 15, Threads: 2},
		{Time: 1, MemoryKiB: 1<<20 + 1, Threads: 1},
	} {
		assert.Error(t, bad.validate(), "%+v", bad)
	}
}

func TestCryptoHelpers(t *testing.T) {
	key := deriveKey("secret", make([]byte, saltSize), KDFParams{Time: 1, MemoryKiB: 64, Threads: 1})
	nonce, ct, err := seal(key, []byte("hello"), []byte("aad"))
	require.NoError(t, err)
	plain, err := open(key, nonce, ct, []byte("aad"))
	require.NoError(t, err)
	assert.Equal(t, "hello", string(plain))
	_, err = open(key, nonce, ct, []byte("other"))
	assert.ErrorIs(t, err, errOpen)
	_, err = open(key, nonce[:4], ct, nil)
	assert.Error(t, err)
	_, _, err = seal([]byte("short"), nil, nil)
	assert.Error(t, err)
	_, err = open([]byte("short"), nonce, ct, nil)
	assert.Error(t, err)
	s, err := randomString(32, "ab")
	require.NoError(t, err)
	assert.Regexp(t, `^[ab]{32}$`, s)
}

func TestClonePolicy(t *testing.T) {
	assert.Nil(t, clonePolicy(nil))
	p := &domain.Policy{ID: "x", Version: domain.PolicyVersion, Statement: domain.Statements{{
		SID: "s", Effect: domain.Allow,
		Principal:    &domain.Principal{AWS: domain.StringSet{"*"}},
		NotPrincipal: &domain.Principal{Service: domain.StringSet{"svc"}},
		Action:       domain.StringSet{"s3:GetObject"},
		NotResource:  domain.StringSet{"arn:aws:s3:::b/private/*"},
		Condition:    domain.Condition{"IpAddress": {"aws:SourceIp": domain.StringSet{"10.0.0.0/8"}}},
	}}}
	c := clonePolicy(p)
	assert.Equal(t, p, c)
	c.Statement[0].Principal.AWS[0] = "alice"
	c.Statement[0].Condition["IpAddress"]["aws:SourceIp"][0] = "0.0.0.0/0"
	c.Statement[0].Action[0] = "s3:*"
	assert.Equal(t, "*", p.Statement[0].Principal.AWS[0])
	assert.Equal(t, "10.0.0.0/8", p.Statement[0].Condition["IpAddress"]["aws:SourceIp"][0])
	assert.Equal(t, "s3:GetObject", p.Statement[0].Action[0])
	assert.Equal(t, &domain.Policy{Version: domain.PolicyVersion}, clonePolicy(&domain.Policy{Version: domain.PolicyVersion}))
}

func TestExportPayloadValidate(t *testing.T) {
	user := &userRecord{AccessKey: "alice", SecretKey: "alice-secret", Status: domain.StatusEnabled}
	group := &groupRecord{Name: "dev", Status: domain.StatusEnabled}
	sa := &serviceAccountRecord{AccessKey: "alice-key", SecretKey: "alice-key-secret", ParentUser: "alice", Status: domain.StatusEnabled}
	sts := &stsRecord{AccessKey: "TMP", SecretKey: "s", SessionToken: "t"}
	valid := exportPayload{Users: []*userRecord{user}, Groups: []*groupRecord{group}, ServiceAccounts: []*serviceAccountRecord{sa}, STS: []*stsRecord{sts}}
	require.NoError(t, valid.validate())

	tests := []struct {
		name    string
		payload exportPayload
		want    error
	}{
		{"null user", exportPayload{Users: []*userRecord{nil}}, domain.ErrInvalidFormat},
		{"bad user key", exportPayload{Users: []*userRecord{{AccessKey: "a", SecretKey: "alice-secret", Status: domain.StatusEnabled}}}, domain.ErrInvalidArgument},
		{"bad user secret", exportPayload{Users: []*userRecord{{AccessKey: "alice", SecretKey: "x", Status: domain.StatusEnabled}}}, domain.ErrInvalidArgument},
		{"bad user status", exportPayload{Users: []*userRecord{{AccessKey: "alice", SecretKey: "alice-secret"}}}, domain.ErrInvalidArgument},
		{"null group", exportPayload{Groups: []*groupRecord{nil}}, domain.ErrInvalidFormat},
		{"bad group name", exportPayload{Groups: []*groupRecord{{Name: "a,b", Status: domain.StatusEnabled}}}, domain.ErrInvalidArgument},
		{"bad group status", exportPayload{Groups: []*groupRecord{{Name: "dev", Status: "x"}}}, domain.ErrInvalidArgument},
		{"null key", exportPayload{ServiceAccounts: []*serviceAccountRecord{nil}}, domain.ErrInvalidFormat},
		{"bad key", exportPayload{ServiceAccounts: []*serviceAccountRecord{{AccessKey: "k", SecretKey: "alice-key-secret", ParentUser: "alice"}}}, domain.ErrInvalidArgument},
		{"bad key secret", exportPayload{ServiceAccounts: []*serviceAccountRecord{{AccessKey: "alice-key", SecretKey: "s", ParentUser: "alice"}}}, domain.ErrInvalidArgument},
		{"orphan key", exportPayload{ServiceAccounts: []*serviceAccountRecord{{AccessKey: "alice-key", SecretKey: "alice-key-secret"}}}, domain.ErrInvalidArgument},
		{"bad key policy", exportPayload{ServiceAccounts: []*serviceAccountRecord{{AccessKey: "alice-key", SecretKey: "alice-key-secret", ParentUser: "alice", Policy: &domain.Policy{}}}}, domain.ErrMalformedPolicy},
		{"incomplete sts", exportPayload{STS: []*stsRecord{{AccessKey: "TMP"}}}, domain.ErrInvalidFormat},
		{"null sts", exportPayload{STS: []*stsRecord{nil}}, domain.ErrInvalidFormat},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.ErrorIs(t, tt.payload.validate(), tt.want)
		})
	}
}

// TestSTSSessionPolicyClaim covers tokens correctly signed but carrying a
// damaged session policy (a key compromise or a bug, never a client).
func TestSTSSessionPolicyClaim(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	svc := New(Deps{Clock: clock.NewFake(now)}).(*service)
	require.NoError(t, svc.Bootstrap(ctx, domain.Credentials{AccessKey: "rootadmin", SecretKey: "rootsecret123"}))
	sign := func(policyClaim string) string {
		cl := stsClaims{
			RegisteredClaims: jwt.RegisteredClaims{Issuer: stsIssuer, ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour))},
			AccessKey:        "TMP", Parent: "rootadmin", SessionPolicy: policyClaim,
		}
		tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, cl).SignedString(svc.stsKey)
		require.NoError(t, err)
		return tok
	}
	for name, claim := range map[string]string{
		"not base64": "!!!",
		"not JSON":   base64.StdEncoding.EncodeToString([]byte("[")),
	} {
		t.Run(name, func(t *testing.T) {
			rec := &stsRecord{AccessKey: "TMP", SecretKey: "s", SessionToken: sign(claim), ParentUser: "rootadmin", Expiration: now.Add(time.Hour)}
			_, _, err := svc.verifySTSToken(rec)
			assert.ErrorIs(t, err, domain.ErrAccessDenied)
		})
	}
	raw, err := json.Marshal(readOnly())
	require.NoError(t, err)
	rec := &stsRecord{AccessKey: "TMP", SecretKey: "s", SessionToken: sign(base64.StdEncoding.EncodeToString(raw)), ParentUser: "rootadmin", Expiration: now.Add(time.Hour)}
	_, p, err := svc.verifySTSToken(rec)
	require.NoError(t, err)
	assert.Equal(t, readOnly(), p)
}

func readOnly() *domain.Policy {
	return &domain.Policy{Version: domain.PolicyVersion, Statement: domain.Statements{{
		Effect: domain.Allow, Action: domain.StringSet{"s3:GetObject"}, Resource: domain.StringSet{"arn:aws:s3:::b/*"},
	}}}
}
