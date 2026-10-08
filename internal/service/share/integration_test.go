package share_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/auth"
	"github.com/r3n3r0/nexo/internal/service/iam"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/service/share"
)

// stack is the real IAM, policy engine and SigV4 signer/verifier behind
// the share service.
type stack struct {
	clk      *clock.Fake
	iam      iam.Service
	policies policy.Service
	verifier auth.Verifier
	shares   share.Service
}

func newStack(t *testing.T) *stack {
	t.Helper()
	ctx := context.Background()
	logger := slog.New(slog.DiscardHandler)
	clk := clock.NewFake(t0())
	ids := iam.New(iam.Deps{Clock: clk, Logger: logger})
	require.NoError(t, ids.Bootstrap(ctx, domain.Credentials{AccessKey: "rootadmin", SecretKey: "rootsecret123"}))
	_, err := ids.CreateUser(ctx, iam.CreateUserInput{AccessKey: "alice", SecretKey: "alice-secret", PolicyNames: []string{policy.PolicyReadWrite}})
	require.NoError(t, err)
	policies := policy.New(policy.Deps{Attachments: ids, Clock: clk, Logger: logger})
	verifier, err := auth.NewVerifier(auth.Deps{Creds: ids, Clock: clk, Logger: logger})
	require.NoError(t, err)
	shares := share.New(share.Deps{
		IAM: ids, Signer: auth.NewSigner(auth.Deps{Clock: clk, Logger: logger}), Policy: policies, Clock: clk, Logger: logger,
	}, share.WithServerURL("http://127.0.0.1:9000"))
	return &stack{clk: clk, iam: ids, policies: policies, verifier: verifier, shares: shares}
}

// use authenticates a request to rawURL as the S3 API would.
func (s *stack) use(t *testing.T, method, rawURL string) (*domain.Identity, error) {
	t.Helper()
	return s.verifier.VerifyPresigned(context.Background(), httptest.NewRequest(method, rawURL, nil))
}

func (s *stack) allowed(t *testing.T, id *domain.Identity, action policy.Action, bucket, object string) bool {
	t.Helper()
	d, err := s.policies.Evaluate(context.Background(), policy.Args{Identity: id, Action: action, Bucket: bucket, Object: object})
	require.NoError(t, err)
	return d.Allowed
}

// TestShareLinkEndToEnd follows a managed link through creation, use,
// intersection with the parent's permissions and revocation (docs/09
// TPS-07, TPS-14, TPS-17).
func TestShareLinkEndToEnd(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	alice := domain.Identity{AccessKey: "alice"}

	sh, err := s.shares.Create(ctx, share.CreateInput{Owner: alice, Bucket: "photos", Key: "trip/a b.jpg", Permission: share.PermissionDownload, Expires: time.Hour})
	require.NoError(t, err)

	id, err := s.use(t, http.MethodGet, sh.URL)
	require.NoError(t, err)
	assert.Equal(t, sh.AccessKey, id.AccessKey)
	assert.Equal(t, "alice", id.EffectiveUser())
	assert.Equal(t, domain.AuthTypeQuery, id.AuthType)
	require.NotNil(t, id.SessionPolicy)

	// The link grants the shared action on the shared object only, although
	// alice may do everything.
	assert.True(t, s.allowed(t, id, policy.ActionGetObject, "photos", "trip/a b.jpg"))
	assert.False(t, s.allowed(t, id, policy.ActionGetObject, "photos", "trip/other.jpg"))
	assert.False(t, s.allowed(t, id, policy.ActionPutObject, "photos", "trip/a b.jpg"))
	assert.False(t, s.allowed(t, id, policy.ActionDeleteObject, "photos", "trip/a b.jpg"))

	// Removing the parent's permissions disables the link at use time.
	require.NoError(t, s.iam.SetUserPolicy(ctx, "alice", nil))
	id, err = s.use(t, http.MethodGet, sh.URL)
	require.NoError(t, err)
	assert.False(t, s.allowed(t, id, policy.ActionGetObject, "photos", "trip/a b.jpg"))
	require.NoError(t, s.iam.SetUserPolicy(ctx, "alice", []string{policy.PolicyReadOnly}))

	// Disabling the user rejects the link immediately.
	require.NoError(t, s.iam.SetUserStatus(ctx, "alice", domain.StatusDisabled))
	_, err = s.use(t, http.MethodGet, sh.URL)
	assert.ErrorIs(t, err, domain.ErrAccessDenied)
	require.NoError(t, s.iam.SetUserStatus(ctx, "alice", domain.StatusEnabled))
	_, err = s.use(t, http.MethodGet, sh.URL)
	require.NoError(t, err)

	// Revocation: the same URL stops working at once.
	require.NoError(t, s.shares.Revoke(ctx, alice, sh.ID))
	_, err = s.use(t, http.MethodGet, sh.URL)
	assert.ErrorIs(t, err, domain.ErrInvalidAccessKey)
}

func TestShareLinkExpiry(t *testing.T) {
	ctx := context.Background()
	s := newStack(t)
	alice := domain.Identity{AccessKey: "alice"}
	sh, err := s.shares.Create(ctx, share.CreateInput{Owner: alice, Bucket: "drop", Key: "in/", Prefix: true, Permission: share.PermissionUpload, Expires: 10 * time.Minute})
	require.NoError(t, err)
	assert.Empty(t, sh.URL)

	u, err := s.shares.Presign(ctx, alice, sh.ID, "in/report.csv")
	require.NoError(t, err)
	id, err := s.use(t, http.MethodPut, u.String())
	require.NoError(t, err)
	assert.True(t, s.allowed(t, id, policy.ActionPutObject, "drop", "in/report.csv"))
	assert.False(t, s.allowed(t, id, policy.ActionPutObject, "drop", "out/report.csv"))
	assert.False(t, s.allowed(t, id, policy.ActionGetObject, "drop", "in/report.csv"))

	s.clk.Advance(10 * time.Minute)
	_, err = s.use(t, http.MethodPut, u.String())
	assert.Error(t, err, "expired")
	n, err := s.shares.PurgeExpired(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	keys, err := s.iam.ListAccessKeys(ctx, domain.Identity{AccessKey: "rootadmin", IsRoot: true}, "")
	require.NoError(t, err)
	assert.Empty(t, keys, "the service account is gone with the share")
}
