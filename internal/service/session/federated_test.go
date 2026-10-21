package session_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/config"
	"github.com/r3n3r0/nexo/internal/service/iam"
	"github.com/r3n3r0/nexo/internal/service/idp"
	"github.com/r3n3r0/nexo/internal/service/session"
	"github.com/r3n3r0/nexo/internal/testutil"
)

// covers: S-IAM-09, D-AUTH-02 (console login with LDAP: federated
// credentials behind the session, groups resolved at each request,
// throttling, expiry)
func TestLoginLDAP(t *testing.T) {
	ctx := context.Background()
	clk := clock.NewFake(time.Now())
	dir := testutil.NewFakeLDAP(t)
	dir.AddUser("carol", "carol-pw", "analysts")
	dir.AddUser("dave", "dave-pw", "nobody")

	users := iam.New(iam.Deps{Store: iam.NewMemoryStore(), Clock: clk}, iam.WithExportKDFParams(iam.KDFParams{Time: 1, MemoryKiB: 64, Threads: 1}))
	require.NoError(t, users.Bootstrap(ctx, domain.Credentials{AccessKey: "rootadmin", SecretKey: "rootsecret123"}))
	_, err := users.CreateGroup(ctx, iam.CreateGroupInput{Name: "analysts", PolicyNames: []string{"readonly"}})
	require.NoError(t, err)
	providers, err := idp.New(idp.Deps{Clock: clk, Config: config.New(config.Deps{Store: config.NewMemoryStore(map[string]config.KV{
		idp.SubsystemLDAP: {
			"enable": "on", "server_addr": dir.Addr, "tls": "off", "lookup_bind_dn": testutil.LDAPBindDN, "lookup_bind_password": testutil.LDAPBindPass,
			"user_dn_search_base_dn": testutil.LDAPUserBase, "group_search_base_dn": testutil.LDAPGroupBase,
		},
	})})})
	require.NoError(t, err)
	svc, err := session.New(session.Deps{IAM: users, IDP: providers, Clock: clk}, session.WithTTL(2*time.Hour))
	require.NoError(t, err)

	sess, err := svc.LoginLDAP(ctx, session.LDAPLoginInput{Username: "carol", Password: "carol-pw", Client: "192.0.2.1"})
	require.NoError(t, err)
	assert.Equal(t, idp.ProviderLDAP, sess.Provider)
	assert.Equal(t, "carol", sess.Username)
	assert.Equal(t, "ldap:carol", sess.Identity.Parent)
	assert.Equal(t, []string{"readonly"}, sess.Identity.PolicyNames)
	assert.Equal(t, clk.Now().Add(2*time.Hour).Unix(), sess.ExpiresAt.Unix())

	got, err := svc.Validate(ctx, sess.Token)
	require.NoError(t, err)
	assert.Equal(t, "carol", got.Username)
	assert.Equal(t, sess.AccessKey, got.AccessKey)

	// The Nexo group is resolved at each request: disabling it removes
	// its policies from the open session.
	require.NoError(t, users.SetGroupStatus(ctx, "analysts", domain.StatusDisabled))
	got, err = svc.Validate(ctx, sess.Token)
	require.NoError(t, err)
	assert.Empty(t, got.Identity.PolicyNames)
	require.NoError(t, users.SetGroupStatus(ctx, "analysts", domain.StatusEnabled))

	// A user mapping to no policy, wrong passwords: refused.
	_, err = svc.LoginLDAP(ctx, session.LDAPLoginInput{Username: "dave", Password: "dave-pw"})
	require.ErrorIs(t, err, domain.ErrAccessDenied)
	for range 5 {
		_, err = svc.LoginLDAP(ctx, session.LDAPLoginInput{Username: "carol", Password: "bad", Client: "192.0.2.9"})
		require.Error(t, err)
	}
	_, err = svc.LoginLDAP(ctx, session.LDAPLoginInput{Username: "carol", Password: "carol-pw", Client: "192.0.2.9"})
	require.ErrorIs(t, err, domain.ErrTooManyRequests)

	// The session ends with its credentials.
	clk.Advance(2*time.Hour + time.Second)
	_, err = svc.Validate(ctx, sess.Token)
	require.ErrorIs(t, err, domain.ErrAccessDenied)

	// Without identity providers the federated logins are not available.
	plain, err := session.New(session.Deps{IAM: users, Clock: clk})
	require.NoError(t, err)
	_, err = plain.LoginLDAP(ctx, session.LDAPLoginInput{Username: "carol", Password: "carol-pw"})
	require.ErrorIs(t, err, domain.ErrNotImplemented)
	_, err = plain.LoginOpenID(ctx, session.OpenIDLoginInput{Code: "x"})
	require.ErrorIs(t, err, domain.ErrNotImplemented)
}
