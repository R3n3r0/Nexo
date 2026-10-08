package admin_test

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/admin"
	adminmock "github.com/r3n3r0/nexo/internal/service/admin/mock"
	"github.com/r3n3r0/nexo/internal/service/auth"
	authmock "github.com/r3n3r0/nexo/internal/service/auth/mock"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/service/share"
	sharemock "github.com/r3n3r0/nexo/internal/service/share/mock"
)

type shareEnv struct {
	shares *sharemock.Service
	signer *authmock.Signer
	creds  *authmock.CredentialLookup
	clk    *clock.Fake
	mgr    admin.ShareManager
}

func newShareEnv(t *testing.T, authz admin.Authorizer) *shareEnv {
	e := &shareEnv{shares: sharemock.NewService(t), signer: authmock.NewSigner(t), creds: authmock.NewCredentialLookup(t),
		clk: clock.NewFake(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))}
	mgr, err := admin.NewShareManager(admin.ShareManagerDeps{Shares: e.shares, Signer: e.signer, Creds: e.creds, Authz: authz, Clock: e.clk},
		admin.WithServerURL("https://s3.example.com/"), admin.WithRegion("eu-1"))
	require.NoError(t, err)
	e.mgr = mgr
	return e
}

func TestNewShareManagerValidates(t *testing.T) {
	_, err := admin.NewShareManager(admin.ShareManagerDeps{})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = admin.NewShareManager(admin.ShareManagerDeps{Shares: sharemock.NewService(t), Signer: authmock.NewSigner(t), Creds: authmock.NewCredentialLookup(t), Authz: allowAll(t)},
		admin.WithServerURL("ftp://x"))
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
}

func TestShareCreate(t *testing.T) {
	ctx := context.Background()
	e := newShareEnv(t, allowing(t, func(in admin.AuthorizeInput) bool {
		return in.Actions[0] == policy.ActionPutObject && in.Bucket == "bkt" && in.Object == "up/*"
	}))
	e.shares.EXPECT().Create(mock.Anything, share.CreateInput{Owner: domain.Identity{AccessKey: "bob"}, Bucket: "bkt", Key: "up/", Prefix: true,
		Permission: share.PermissionUpload, Expires: time.Hour}).Return(&share.Share{ID: "s1"}, nil)
	s, err := e.mgr.Create(ctx, admin.CreateShareInput{Requester: req("bob"), Bucket: "bkt", Key: "up/", Prefix: true, Permission: share.PermissionUpload, Expires: time.Hour})
	require.NoError(t, err)
	assert.Equal(t, "s1", s.ID)
	_, err = e.mgr.Create(ctx, admin.CreateShareInput{Requester: req("bob"), Bucket: "bkt", Key: "k", Permission: share.PermissionDownload})
	assert.ErrorIs(t, err, domain.ErrAccessDenied)
}

// TestShareCreateDefaultPermission: a share without a permission is a
// download link (authorized as s3:GetObject).
func TestShareCreateDefaultPermission(t *testing.T) {
	ctx := context.Background()
	var asked []policy.Action
	e := newShareEnv(t, allowing(t, func(in admin.AuthorizeInput) bool { asked = append(asked, in.Actions...); return true }))
	e.shares.EXPECT().Create(mock.Anything, mock.MatchedBy(func(in share.CreateInput) bool { return in.Permission == share.PermissionDownload })).
		Return(&share.Share{ID: "s2"}, nil)
	_, err := e.mgr.Create(ctx, admin.CreateShareInput{Requester: req("bob"), Bucket: "bkt", Key: "k"})
	require.NoError(t, err)
	assert.Equal(t, []policy.Action{policy.ActionGetObject}, asked)
}

func TestShareManagementScope(t *testing.T) {
	ctx := context.Background()
	explicit := adminAuthorizer(t, func(in admin.AuthorizeInput) admin.Decision {
		switch in.Requester.Identity.AccessKey {
		case "admin1":
			return admin.Decision{Allowed: true}
		case "denied":
			return admin.Decision{DeniedAction: in.Actions[0], Reason: "explicit deny"}
		}
		return admin.Decision{Allowed: true, SelfService: true}
	})
	e := newShareEnv(t, explicit)
	// An administrator acts with root semantics.
	e.shares.EXPECT().List(mock.Anything, domain.Identity{IsRoot: true}, share.ListFilter{Owner: "carol"}).Return([]share.Share{{ID: "c1"}}, nil)
	got, err := e.mgr.List(ctx, admin.ListSharesInput{Requester: req("admin1"), Owner: "carol"})
	require.NoError(t, err)
	assert.Len(t, got, 1)
	// A user acts as itself (own shares only).
	e.shares.EXPECT().Revoke(mock.Anything, domain.Identity{AccessKey: "bob"}, "c1").Return(fmt.Errorf("x: %w", domain.ErrShareNotFound))
	assert.ErrorIs(t, e.mgr.Revoke(ctx, req("bob"), "c1"), domain.ErrShareNotFound)
	e.shares.EXPECT().RevokeAll(mock.Anything, domain.Identity{AccessKey: "bob"}, "bob").Return(2, nil)
	n, err := e.mgr.RevokeAll(ctx, req("bob"), "")
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	// An explicit deny of admin:ManageShares denies even the own shares.
	_, err = e.mgr.Get(ctx, req("denied"), "c1")
	assert.ErrorIs(t, err, domain.ErrAccessDenied)
}

// adminAuthorizer decides with fn.
func adminAuthorizer(t *testing.T, fn func(in admin.AuthorizeInput) admin.Decision) admin.Authorizer {
	a := adminmock.NewAuthorizer(t)
	a.EXPECT().Authorize(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, in admin.AuthorizeInput) (admin.Decision, error) {
		return fn(in), nil
	}).Maybe()
	return a
}

func TestPresign(t *testing.T) {
	tests := []struct {
		name    string
		in      admin.PresignInput
		creds   domain.Credentials
		check   func(t *testing.T, in auth.PresignInput)
		wantErr error
		allow   bool
	}{
		{name: "GET with overrides", allow: true,
			in:    admin.PresignInput{Bucket: "bkt", Key: "dir/a b.txt", ContentDisposition: "attachment", ContentType: "text/plain", Expires: time.Hour},
			creds: domain.Credentials{AccessKey: "bob", SecretKey: "s"},
			check: func(t *testing.T, in auth.PresignInput) {
				assert.Equal(t, http.MethodGet, in.Method)
				assert.Equal(t, "https://s3.example.com/bkt/dir/a%20b.txt?response-content-disposition=attachment&response-content-type=text%2Fplain", in.URL.String())
				assert.Equal(t, time.Hour, in.Expires)
				assert.Equal(t, "eu-1", in.Region)
			}},
		{name: "PUT signs the content type", allow: true,
			in:    admin.PresignInput{Method: "put", Bucket: "bkt", Key: "k", ContentType: "image/png", Headers: map[string]string{"X-Amz-Meta-A": "1"}},
			creds: domain.Credentials{AccessKey: "bob", SecretKey: "s"},
			check: func(t *testing.T, in auth.PresignInput) {
				assert.Equal(t, http.MethodPut, in.Method)
				assert.Equal(t, "image/png", in.Headers.Get("Content-Type"))
				assert.Equal(t, "1", in.Headers.Get("X-Amz-Meta-A"))
				assert.Equal(t, share.MaxExpiry, in.Expires, "default validity")
			}},
		{name: "capped at the credentials expiration", allow: true,
			in:    admin.PresignInput{Method: "DELETE", Bucket: "bkt", Key: "k", Expires: 24 * time.Hour},
			creds: domain.Credentials{AccessKey: "bob", SecretKey: "s", SessionToken: "tok", Expiration: time.Date(2026, 3, 1, 12, 30, 0, 0, time.UTC)},
			check: func(t *testing.T, in auth.PresignInput) {
				assert.Equal(t, 30*time.Minute, in.Expires)
				assert.Equal(t, "tok", in.Creds.SessionToken)
			}},
		{name: "unknown method", allow: true, in: admin.PresignInput{Method: "POST", Bucket: "bkt", Key: "k"}, wantErr: domain.ErrInvalidArgument},
		{name: "too long", allow: true, in: admin.PresignInput{Bucket: "bkt", Key: "k", Expires: 8 * 24 * time.Hour}, wantErr: domain.ErrInvalidArgument},
		{name: "reserved header", allow: true, in: admin.PresignInput{Bucket: "bkt", Key: "k", Headers: map[string]string{"Authorization": "x"}},
			wantErr: domain.ErrInvalidArgument},
		{name: "denied", in: admin.PresignInput{Bucket: "bkt", Key: "k"}, wantErr: domain.ErrAccessDenied},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newShareEnv(t, allowing(t, func(admin.AuthorizeInput) bool { return tt.allow }))
			if tt.creds.AccessKey != "" {
				e.creds.EXPECT().LookupCredentials(mock.Anything, "bob").Return(tt.creds, domain.Identity{}, nil)
			}
			if tt.check != nil {
				e.signer.EXPECT().Presign(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, in auth.PresignInput) (*url.URL, error) {
					tt.check(t, in)
					return url.Parse(in.URL.String() + "&X-Amz-Signature=x")
				})
			}
			tt.in.Requester = req("bob")
			out, err := e.mgr.Presign(context.Background(), tt.in)
			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Contains(t, out.URL, "X-Amz-Signature")
		})
	}
}

func TestPostPolicy(t *testing.T) {
	ctx := context.Background()
	e := newShareEnv(t, allowing(t, func(in admin.AuthorizeInput) bool { return in.Object == "up/*" }))
	e.creds.EXPECT().LookupCredentials(mock.Anything, "bob").Return(domain.Credentials{AccessKey: "bob", SecretKey: "s"}, domain.Identity{}, nil)
	e.signer.EXPECT().PresignPostPolicy(mock.Anything, mock.MatchedBy(func(in auth.PostPolicyInput) bool {
		return in.KeyPrefix == "up/" && in.Expires == time.Hour && in.Fields["success_action_status"] == "201" &&
			len(in.Conditions) == 2 && in.Conditions[1].Operator == auth.ConditionLengthRange && in.Conditions[1].Max == 1024
	})).Return(&auth.PostPolicyForm{Fields: map[string]string{"key": "up/${filename}"}, Policy: auth.PostPolicy{Conditions: []auth.PostCondition{{Operator: "eq", Key: "bucket", Value: "bkt"}}}}, nil)
	form, err := e.mgr.PostPolicy(ctx, admin.PostPolicyInput{Requester: req("bob"), Bucket: "bkt", KeyPrefix: "up/", MaxSize: 1024, ContentTypePrefix: "image/", SuccessActionStatus: 201})
	require.NoError(t, err)
	assert.Equal(t, "https://s3.example.com/bkt", form.URL)
	assert.Equal(t, []admin.PostCondition{{Operator: "eq", Key: "bucket", Value: "bkt"}}, form.Conditions)

	_, err = e.mgr.PostPolicy(ctx, admin.PostPolicyInput{Requester: req("bob"), Bucket: "bkt"})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument, "key or prefix")
	_, err = e.mgr.PostPolicy(ctx, admin.PostPolicyInput{Requester: req("bob"), Bucket: "bkt", Key: "k", MinSize: 10, MaxSize: 5})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = e.mgr.PostPolicy(ctx, admin.PostPolicyInput{Requester: req("bob"), Bucket: "bkt", Key: "k", SuccessActionStatus: 302})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = e.mgr.PostPolicy(ctx, admin.PostPolicyInput{Requester: req("bob"), Bucket: "bkt", Key: "other"})
	assert.ErrorIs(t, err, domain.ErrAccessDenied)
}
