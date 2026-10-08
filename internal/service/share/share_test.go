package share_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/auth"
	authmock "github.com/r3n3r0/nexo/internal/service/auth/mock"
	"github.com/r3n3r0/nexo/internal/service/iam"
	iammock "github.com/r3n3r0/nexo/internal/service/iam/mock"
	"github.com/r3n3r0/nexo/internal/service/policy"
	policymock "github.com/r3n3r0/nexo/internal/service/policy/mock"
	"github.com/r3n3r0/nexo/internal/service/share"
)

const serverURL = "https://files.example.com"

func t0() time.Time { return time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC) }

func alice() domain.Identity { return domain.Identity{AccessKey: "alice"} }

func bob() domain.Identity { return domain.Identity{AccessKey: "bob"} }

func root() domain.Identity { return domain.Identity{AccessKey: "rootadmin", IsRoot: true} }

// fixture wires the share service to generated mocks and a memory store.
type fixture struct {
	svc    share.Service
	iam    *iammock.Service
	signer *authmock.Signer
	policy *policymock.Service
	store  iam.Store
	clk    *clock.Fake

	mu sync.Mutex
	// keys are the service accounts created through the IAM mock.
	keys map[string]*iam.AccessKey
	// deleted are the service accounts deleted through the IAM mock.
	deleted []string
}

type fixtureOption struct {
	noPolicy bool
	store    iam.Store
	opts     []share.Option
}

func newFixture(t *testing.T, fo fixtureOption) *fixture {
	t.Helper()
	f := &fixture{
		iam:    iammock.NewService(t),
		signer: authmock.NewSigner(t),
		store:  fo.store,
		clk:    clock.NewFake(t0()),
		keys:   map[string]*iam.AccessKey{},
	}
	if f.store == nil {
		f.store = iam.NewMemoryStore()
	}
	deps := share.Deps{IAM: f.iam, Signer: f.signer, Store: f.store, Clock: f.clk, Logger: slog.New(slog.DiscardHandler)}
	if !fo.noPolicy {
		f.policy = policymock.NewService(t)
		deps.Policy = f.policy
	}
	opts := append([]share.Option{share.WithServerURL(serverURL + "/")}, fo.opts...)
	f.svc = share.New(deps, opts...)
	return f
}

// expectIAM models the service accounts with RunAndReturn on the mock.
func (f *fixture) expectIAM() {
	f.iam.EXPECT().CreateAccessKey(mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, in iam.CreateAccessKeyInput) (*iam.AccessKey, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			k := &iam.AccessKey{
				AccessKey: in.AccessKey, SecretKey: "secret-of-" + in.AccessKey, ParentUser: in.ParentUser,
				Name: in.Name, Description: in.Description, Status: domain.StatusEnabled,
				Expiration: in.Expiration, Policy: in.Policy,
			}
			f.keys[in.AccessKey] = k
			return k, nil
		}).Maybe()
	f.iam.EXPECT().DeleteAccessKey(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, _ domain.Identity, ak string) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			if _, ok := f.keys[ak]; !ok {
				return domain.ErrAccessKeyNotFound
			}
			delete(f.keys, ak)
			f.deleted = append(f.deleted, ak)
			return nil
		}).Maybe()
	f.iam.EXPECT().LookupCredentials(mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, ak string) (domain.Credentials, domain.Identity, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			k, ok := f.keys[ak]
			if !ok {
				return domain.Credentials{}, domain.Identity{}, domain.ErrInvalidAccessKey
			}
			return domain.Credentials{AccessKey: k.AccessKey, SecretKey: k.SecretKey, Expiration: k.Expiration},
				domain.Identity{AccessKey: ak, Parent: k.ParentUser, SessionPolicy: k.Policy}, nil
		}).Maybe()
}

// expectSigner returns the input URL with a fake signature and records
// the inputs.
func (f *fixture) expectSigner() *[]auth.PresignInput {
	var calls []auth.PresignInput
	f.signer.EXPECT().Presign(mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, in auth.PresignInput) (*url.URL, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			calls = append(calls, in)
			u := *in.URL
			if u.RawQuery != "" {
				u.RawQuery += "&"
			}
			u.RawQuery += "X-Amz-Signature=fake"
			return &u, nil
		}).Maybe()
	return &calls
}

func (f *fixture) allow() {
	f.policy.EXPECT().Evaluate(mock.Anything, mock.Anything).Return(policy.Decision{Allowed: true}, nil).Maybe()
}

func (f *fixture) key(ak string) *iam.AccessKey {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.keys[ak]
}

func TestCreateObjectDownload(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fixtureOption{})
	f.expectIAM()
	calls := f.expectSigner()
	f.policy.EXPECT().Evaluate(mock.Anything, mock.MatchedBy(func(a policy.Args) bool {
		return a.Identity.AccessKey == "alice" && a.Action == policy.ActionGetObject && a.Bucket == "photos" && a.Object == "2026/a b.jpg"
	})).Return(policy.Decision{Allowed: true}, nil).Once()

	sh, err := f.svc.Create(ctx, share.CreateInput{
		Owner: alice(), Bucket: "photos", Key: "2026/a b.jpg", Permission: share.PermissionDownload,
		Expires: time.Hour, ContentDisposition: `attachment; filename="a b.jpg"`, ContentType: "image/jpeg", VersionID: "v1",
	})
	require.NoError(t, err)
	assert.Len(t, sh.ID, 12)
	assert.Equal(t, "share-"+sh.ID, sh.AccessKey)
	assert.Equal(t, "alice", sh.Owner)
	assert.Equal(t, t0().Add(time.Hour), sh.Expires)
	assert.Equal(t, t0(), sh.CreatedAt)

	// The dedicated service account: child of alice, same expiry, policy
	// limited to the object.
	k := f.key(sh.AccessKey)
	require.NotNil(t, k)
	assert.Equal(t, "alice", k.ParentUser)
	assert.Equal(t, sh.Expires, k.Expiration)
	assert.Equal(t, &domain.Policy{Version: domain.PolicyVersion, Statement: domain.Statements{{
		SID: "Share", Effect: domain.Allow,
		Action:   domain.StringSet{"s3:GetObject", "s3:GetObjectVersion"},
		Resource: domain.StringSet{"arn:aws:s3:::photos/2026/a b.jpg"},
	}}}, k.Policy)
	assert.Contains(t, k.Description, sh.ID)

	// The presigned URL, signed by the service account with the public host.
	require.Len(t, *calls, 1)
	in := (*calls)[0]
	assert.Equal(t, http.MethodGet, in.Method)
	assert.Equal(t, time.Hour, in.Expires)
	assert.Equal(t, t0(), in.Now)
	assert.Equal(t, sh.AccessKey, in.Creds.AccessKey)
	assert.Equal(t, "secret-of-"+sh.AccessKey, in.Creds.SecretKey)
	assert.Equal(t, "files.example.com", in.URL.Host)
	assert.Equal(t, "/photos/2026/a b.jpg", in.URL.Path)
	q := in.URL.Query()
	assert.Equal(t, "v1", q.Get("versionId"))
	assert.Equal(t, `attachment; filename="a b.jpg"`, q.Get("response-content-disposition"))
	assert.Equal(t, "image/jpeg", q.Get("response-content-type"))
	u, err := url.Parse(sh.URL)
	require.NoError(t, err)
	assert.Equal(t, "https", u.Scheme)
	assert.Equal(t, "fake", u.Query().Get("X-Amz-Signature"))

	// Persisted and visible to the owner.
	got, err := f.svc.Get(ctx, alice(), sh.ID)
	require.NoError(t, err)
	assert.Equal(t, sh, got)
	raw, err := f.store.Get(ctx, iam.NamespaceShares, sh.ID)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "secret-of-", "the secret is not persisted with the share")
}

func TestCreateVariants(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name       string
		in         share.CreateInput
		wantMethod string
		wantPath   string
		wantQuery  url.Values
		wantHeader http.Header
		wantPolicy domain.Statements
		noURL      bool
	}{
		{
			name:       "object upload",
			in:         share.CreateInput{Owner: alice(), Bucket: "drop", Key: "in/report.pdf", Permission: share.PermissionUpload, ContentType: "application/pdf"},
			wantMethod: http.MethodPut, wantPath: "/drop/in/report.pdf", wantQuery: url.Values{},
			wantHeader: http.Header{"Content-Type": {"application/pdf"}},
			wantPolicy: domain.Statements{{SID: "Share", Effect: domain.Allow, Action: domain.StringSet{"s3:PutObject"}, Resource: domain.StringSet{"arn:aws:s3:::drop/in/report.pdf"}}},
		},
		{
			name:       "prefix download",
			in:         share.CreateInput{Owner: alice(), Bucket: "photos", Key: "2026/", Prefix: true, Permission: share.PermissionDownload},
			wantMethod: http.MethodGet, wantPath: "/photos", wantQuery: url.Values{"list-type": {"2"}, "prefix": {"2026/"}},
			wantPolicy: domain.Statements{
				{SID: "Share", Effect: domain.Allow, Action: domain.StringSet{"s3:GetObject"}, Resource: domain.StringSet{"arn:aws:s3:::photos/2026/*"}},
				{SID: "ShareList", Effect: domain.Allow, Action: domain.StringSet{"s3:ListBucket"}, Resource: domain.StringSet{"arn:aws:s3:::photos"},
					Condition: domain.Condition{"StringLike": {"s3:prefix": domain.StringSet{"2026/*"}}}},
			},
		},
		{
			name:  "prefix upload",
			in:    share.CreateInput{Owner: alice(), Bucket: "drop", Key: "", Prefix: true, Permission: share.PermissionUpload},
			noURL: true,
			wantPolicy: domain.Statements{
				{SID: "Share", Effect: domain.Allow, Action: domain.StringSet{"s3:PutObject"}, Resource: domain.StringSet{"arn:aws:s3:::drop/*"}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, fixtureOption{})
			f.expectIAM()
			calls := f.expectSigner()
			f.allow()
			sh, err := f.svc.Create(ctx, tt.in)
			require.NoError(t, err)
			assert.Equal(t, t0().Add(share.MaxExpiry), sh.Expires, "default expiry: 7 days")
			assert.Equal(t, tt.wantPolicy, f.key(sh.AccessKey).Policy.Statement)
			if tt.noURL {
				assert.Empty(t, *calls)
				assert.Empty(t, sh.URL)
				return
			}
			require.Len(t, *calls, 1)
			in := (*calls)[0]
			assert.Equal(t, tt.wantMethod, in.Method)
			assert.Equal(t, tt.wantPath, in.URL.Path)
			assert.Equal(t, tt.wantQuery, in.URL.Query())
			assert.Equal(t, tt.wantHeader, in.Headers)
			assert.Equal(t, share.MaxExpiry, in.Expires)
			assert.NotEmpty(t, sh.URL)
		})
	}
}

func TestCreateValidation(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fixtureOption{})
	ok := share.CreateInput{Owner: alice(), Bucket: "photos", Key: "a.jpg", Permission: share.PermissionDownload}
	with := func(change func(in *share.CreateInput)) share.CreateInput {
		in := ok
		change(&in)
		return in
	}
	tests := []struct {
		name string
		in   share.CreateInput
		want error
	}{
		{"anonymous", with(func(in *share.CreateInput) { in.Owner = *domain.Anonymous() }), domain.ErrAccessDenied},
		{"no owner", with(func(in *share.CreateInput) { in.Owner = domain.Identity{} }), domain.ErrAccessDenied},
		{"bad bucket", with(func(in *share.CreateInput) { in.Bucket = "No_Such" }), domain.ErrInvalidBucketName},
		{"system bucket", with(func(in *share.CreateInput) { in.Bucket = domain.SystemBucket }), nil},
		{"empty key", with(func(in *share.CreateInput) { in.Key = "" }), domain.ErrInvalidObjectName},
		{"dot segment", with(func(in *share.CreateInput) { in.Key = "a/../b" }), domain.ErrInvalidObjectName},
		{"bad prefix", with(func(in *share.CreateInput) { in.Key, in.Prefix = "a//b/", true }), domain.ErrInvalidObjectName},
		{"wildcard key", with(func(in *share.CreateInput) { in.Key = "a*.jpg" }), domain.ErrInvalidArgument},
		{"question mark", with(func(in *share.CreateInput) { in.Key = "a?.jpg" }), domain.ErrInvalidArgument},
		{"policy variable", with(func(in *share.CreateInput) { in.Key = "${aws:username}/a" }), domain.ErrInvalidArgument},
		{"no permission", with(func(in *share.CreateInput) { in.Permission = "" }), domain.ErrInvalidArgument},
		{"unknown permission", with(func(in *share.CreateInput) { in.Permission = "delete" }), domain.ErrInvalidArgument},
		{"version on upload", with(func(in *share.CreateInput) { in.Permission, in.VersionID = share.PermissionUpload, "v1" }), domain.ErrInvalidArgument},
		{"disposition on upload", with(func(in *share.CreateInput) {
			in.Permission, in.ContentDisposition = share.PermissionUpload, "attachment"
		}), domain.ErrInvalidArgument},
		{"version on prefix", with(func(in *share.CreateInput) { in.Prefix, in.VersionID = true, "v1" }), domain.ErrInvalidArgument},
		{"header injection", with(func(in *share.CreateInput) { in.ContentType = "text/plain\r\nX-Evil: 1" }), domain.ErrInvalidArgument},
		{"long content type", with(func(in *share.CreateInput) { in.ContentType = strings.Repeat("x", 1025) }), domain.ErrInvalidArgument},
		{"negative expiry", with(func(in *share.CreateInput) { in.Expires = -time.Second }), domain.ErrInvalidArgument},
		{"sub-second expiry", with(func(in *share.CreateInput) { in.Expires = time.Millisecond }), domain.ErrInvalidArgument},
		{"expiry over 7 days", with(func(in *share.CreateInput) { in.Expires = share.MaxExpiry + time.Second }), domain.ErrInvalidArgument},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := f.svc.Create(ctx, tt.in)
			require.Error(t, err)
			if tt.want != nil {
				assert.ErrorIs(t, err, tt.want)
			}
		})
	}
}

func TestCreateAuthorization(t *testing.T) {
	ctx := context.Background()
	in := share.CreateInput{Owner: alice(), Bucket: "photos", Key: "dir/", Prefix: true, Permission: share.PermissionUpload}

	t.Run("denied by the policy engine", func(t *testing.T) {
		f := newFixture(t, fixtureOption{})
		f.policy.EXPECT().Evaluate(mock.Anything, mock.MatchedBy(func(a policy.Args) bool {
			return a.Action == policy.ActionPutObject && a.Object == "dir/*"
		})).Return(policy.Decision{Reason: "no policy allows s3:PutObject"}, nil).Once()
		_, err := f.svc.Create(ctx, in)
		assert.ErrorIs(t, err, domain.ErrAccessDenied)
		assert.Contains(t, err.Error(), "no policy allows")
	})

	t.Run("engine failure", func(t *testing.T) {
		f := newFixture(t, fixtureOption{})
		f.policy.EXPECT().Evaluate(mock.Anything, mock.Anything).Return(policy.Decision{}, errors.New("store down")).Once()
		_, err := f.svc.Create(ctx, in)
		assert.ErrorContains(t, err, "store down")
	})

	t.Run("root skips the engine", func(t *testing.T) {
		f := newFixture(t, fixtureOption{})
		f.expectIAM()
		f.expectSigner()
		rootIn := in
		rootIn.Owner = root()
		sh, err := f.svc.Create(ctx, rootIn)
		require.NoError(t, err)
		assert.Equal(t, "rootadmin", sh.Owner)
		assert.Equal(t, "rootadmin", f.key(sh.AccessKey).ParentUser)
	})

	t.Run("derived owner needs the engine", func(t *testing.T) {
		f := newFixture(t, fixtureOption{noPolicy: true})
		derived := in
		derived.Owner = domain.Identity{AccessKey: "AKALICE", Parent: "alice", SessionPolicy: &domain.Policy{}}
		_, err := f.svc.Create(ctx, derived)
		assert.ErrorIs(t, err, domain.ErrAccessDenied)
	})

	t.Run("derived owner shares for its parent", func(t *testing.T) {
		f := newFixture(t, fixtureOption{})
		f.expectIAM()
		f.expectSigner()
		derived := in
		derived.Owner = domain.Identity{AccessKey: "AKALICE", Parent: "alice"}
		f.policy.EXPECT().Evaluate(mock.Anything, mock.MatchedBy(func(a policy.Args) bool {
			return a.Identity.AccessKey == "AKALICE"
		})).Return(policy.Decision{Allowed: true}, nil).Once()
		sh, err := f.svc.Create(ctx, derived)
		require.NoError(t, err)
		assert.Equal(t, "alice", sh.Owner)
	})

	t.Run("without engine users share", func(t *testing.T) {
		f := newFixture(t, fixtureOption{noPolicy: true})
		f.expectIAM()
		f.expectSigner()
		_, err := f.svc.Create(ctx, in)
		require.NoError(t, err)
	})
}

// TestCreateCappedBySTSSession checks docs/09 §9.1: a link never outlives
// the temporary credentials that created it.
func TestCreateCappedBySTSSession(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fixtureOption{})
	f.expectIAM()
	calls := f.expectSigner()
	f.allow()
	owner := domain.Identity{AccessKey: "STSKEY", Parent: "alice", Claims: map[string]any{"exp": t0().Add(30 * time.Minute).Unix()}}
	sh, err := f.svc.Create(ctx, share.CreateInput{Owner: owner, Bucket: "photos", Key: "a.jpg", Permission: share.PermissionDownload, Expires: 2 * time.Hour})
	require.NoError(t, err)
	assert.Equal(t, t0().Add(30*time.Minute), sh.Expires)
	assert.Equal(t, 30*time.Minute, (*calls)[0].Expires)

	for _, exp := range []any{float64(t0().Add(time.Hour).Unix()), int(t0().Add(time.Hour).Unix()), json.Number(fmt.Sprint(t0().Add(time.Hour).Unix()))} {
		owner.Claims = map[string]any{"exp": exp}
		sh, err = f.svc.Create(ctx, share.CreateInput{Owner: owner, Bucket: "photos", Key: "a.jpg", Permission: share.PermissionDownload, Expires: 2 * time.Hour})
		require.NoError(t, err)
		assert.Equal(t, t0().Add(time.Hour), sh.Expires)
	}

	owner.Claims = map[string]any{"exp": t0().Unix()}
	_, err = f.svc.Create(ctx, share.CreateInput{Owner: owner, Bucket: "photos", Key: "a.jpg", Permission: share.PermissionDownload})
	assert.ErrorIs(t, err, domain.ErrAccessDenied)
}

func TestCreateRollback(t *testing.T) {
	ctx := context.Background()
	in := share.CreateInput{Owner: alice(), Bucket: "photos", Key: "a.jpg", Permission: share.PermissionDownload}

	t.Run("service account failure", func(t *testing.T) {
		f := newFixture(t, fixtureOption{})
		f.allow()
		f.iam.EXPECT().CreateAccessKey(mock.Anything, mock.Anything).Return(nil, domain.ErrUserNotFound).Once()
		_, err := f.svc.Create(ctx, in)
		assert.ErrorIs(t, err, domain.ErrUserNotFound)
	})

	t.Run("presign failure", func(t *testing.T) {
		f := newFixture(t, fixtureOption{})
		f.allow()
		f.expectIAM()
		f.signer.EXPECT().Presign(mock.Anything, mock.Anything).Return(nil, domain.ErrInvalidArgument).Once()
		_, err := f.svc.Create(ctx, in)
		assert.ErrorIs(t, err, domain.ErrInvalidArgument)
		assert.Len(t, f.deleted, 1, "the service account is removed")
		list, err := f.svc.List(ctx, alice(), share.ListFilter{})
		require.NoError(t, err)
		assert.Empty(t, list)
	})

	t.Run("store failure", func(t *testing.T) {
		store := iammock.NewStore(t)
		f := newFixture(t, fixtureOption{store: store})
		f.allow()
		f.expectIAM()
		f.expectSigner()
		store.EXPECT().List(mock.Anything, iam.NamespaceShares).Return(map[string][]byte{}, nil)
		store.EXPECT().Put(mock.Anything, iam.NamespaceShares, mock.Anything, mock.Anything).Return(errors.New("disk full")).Once()
		_, err := f.svc.Create(ctx, in)
		assert.ErrorContains(t, err, "disk full")
		assert.Len(t, f.deleted, 1, "the service account is removed")
	})

	// Regression: the client going away during the save must not leave an
	// orphan service account (no record: the Share API cannot revoke it).
	t.Run("canceled client", func(t *testing.T) {
		store := iammock.NewStore(t)
		f := newFixture(t, fixtureOption{store: store})
		f.allow()
		// Registered first, this expectation answers the rollback.
		var rollbackCtxErr error
		f.iam.EXPECT().DeleteAccessKey(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
			func(ctx context.Context, _ domain.Identity, _ string) error {
				rollbackCtxErr = ctx.Err() // a canceled context would make the rollback fail
				return rollbackCtxErr
			}).Once()
		f.expectIAM()
		f.expectSigner()
		cctx, cancel := context.WithCancel(ctx)
		defer cancel()
		store.EXPECT().List(mock.Anything, iam.NamespaceShares).Return(map[string][]byte{}, nil)
		store.EXPECT().Put(mock.Anything, iam.NamespaceShares, mock.Anything, mock.Anything).RunAndReturn(
			func(ctx context.Context, _ iam.Namespace, _ string, _ []byte) error {
				cancel()
				return ctx.Err()
			}).Once()
		_, err := f.svc.Create(cctx, in)
		assert.ErrorIs(t, err, context.Canceled)
		assert.NoError(t, rollbackCtxErr, "the rollback runs with a live context")
	})

	t.Run("rollback failure is logged", func(t *testing.T) {
		f := newFixture(t, fixtureOption{})
		f.allow()
		f.iam.EXPECT().CreateAccessKey(mock.Anything, mock.Anything).Return(&iam.AccessKey{AccessKey: "share-x", SecretKey: "s"}, nil).Once()
		f.iam.EXPECT().DeleteAccessKey(mock.Anything, mock.Anything, mock.Anything).Return(errors.New("iam down")).Once()
		f.signer.EXPECT().Presign(mock.Anything, mock.Anything).Return(nil, errors.New("signer down")).Once()
		_, err := f.svc.Create(ctx, in)
		assert.ErrorContains(t, err, "signer down")
	})
}

func TestMisconfiguration(t *testing.T) {
	ctx := context.Background()
	in := share.CreateInput{Owner: alice(), Bucket: "photos", Key: "a.jpg", Permission: share.PermissionDownload}

	svc := share.New(share.Deps{})
	_, err := svc.Create(ctx, in)
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = svc.Presign(ctx, alice(), "abcdefghijkl", "")
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	assert.ErrorIs(t, svc.Revoke(ctx, alice(), "abcdefghijkl"), domain.ErrInvalidArgument)
	_, err = svc.RevokeAll(ctx, alice(), "")
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = svc.PurgeExpired(ctx)
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)

	for _, raw := range []string{"ftp://host", "localhost:9000", "http://", "http://h/?q=1", "http://u:p@h", "http://h/#f", "http://[::1"} {
		f := newFixture(t, fixtureOption{opts: []share.Option{share.WithServerURL(raw)}})
		_, err := f.svc.Create(ctx, in)
		assert.ErrorIs(t, err, domain.ErrInvalidArgument, raw)
	}
}

func TestServerURLAndRegion(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fixtureOption{opts: []share.Option{share.WithServerURL("http://proxy.local:8080/s3/"), share.WithRegion("eu-south-1")}})
	f.expectIAM()
	calls := f.expectSigner()
	f.allow()
	_, err := f.svc.Create(ctx, share.CreateInput{Owner: alice(), Bucket: "photos", Key: "a.jpg", Permission: share.PermissionDownload})
	require.NoError(t, err)
	in := (*calls)[0]
	assert.Equal(t, "http://proxy.local:8080/s3/photos/a.jpg", in.URL.String())
	assert.Equal(t, "eu-south-1", in.Region)
}

func TestVisibilityAndList(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fixtureOption{})
	f.expectIAM()
	f.expectSigner()
	f.allow()
	create := func(owner domain.Identity, bucket, key string, d time.Duration) *share.Share {
		sh, err := f.svc.Create(ctx, share.CreateInput{Owner: owner, Bucket: bucket, Key: key, Permission: share.PermissionDownload, Expires: d})
		require.NoError(t, err)
		f.clk.Advance(time.Second)
		return sh
	}
	a1 := create(alice(), "photos", "a1", time.Hour)
	a2 := create(alice(), "docs", "a2", 10*time.Minute)
	b1 := create(bob(), "photos", "b1", time.Hour)
	r1 := create(root(), "photos", "r1", time.Hour)

	ids := func(list []share.Share) []string {
		out := []string{}
		for _, s := range list {
			out = append(out, s.ID)
		}
		return out
	}
	tests := []struct {
		name   string
		caller domain.Identity
		filter share.ListFilter
		want   []string
	}{
		{"alice sees hers, newest first", alice(), share.ListFilter{}, []string{a2.ID, a1.ID}},
		{"alice by bucket", alice(), share.ListFilter{Bucket: "photos"}, []string{a1.ID}},
		{"alice names herself", alice(), share.ListFilter{Owner: "alice"}, []string{a2.ID, a1.ID}},
		{"alice's key acts as alice", domain.Identity{AccessKey: "AK", Parent: "alice"}, share.ListFilter{}, []string{a2.ID, a1.ID}},
		{"root sees all", root(), share.ListFilter{}, []string{r1.ID, b1.ID, a2.ID, a1.ID}},
		{"root by owner", root(), share.ListFilter{Owner: "bob"}, []string{b1.ID}},
		{"root by bucket", root(), share.ListFilter{Bucket: "docs"}, []string{a2.ID}},
		{"root key is not root", domain.Identity{AccessKey: "AKR", Parent: "rootadmin", IsRoot: true}, share.ListFilter{}, []string{r1.ID}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			list, err := f.svc.List(ctx, tt.caller, tt.filter)
			require.NoError(t, err)
			assert.Equal(t, tt.want, ids(list))
		})
	}

	_, err := f.svc.List(ctx, alice(), share.ListFilter{Owner: "bob"})
	assert.ErrorIs(t, err, domain.ErrAccessDenied)
	_, err = f.svc.List(ctx, *domain.Anonymous(), share.ListFilter{})
	assert.ErrorIs(t, err, domain.ErrAccessDenied)

	_, err = f.svc.Get(ctx, bob(), a1.ID)
	assert.ErrorIs(t, err, domain.ErrShareNotFound, "other users' shares are invisible")
	_, err = f.svc.Get(ctx, *domain.Anonymous(), a1.ID)
	assert.ErrorIs(t, err, domain.ErrShareNotFound)
	got, err := f.svc.Get(ctx, root(), a1.ID)
	require.NoError(t, err)
	assert.Equal(t, a1, got)
	for _, id := range []string{"", "short", "ABCDEFGHIJKL", "abcdefghijk1", "abcdefghijkm"} {
		_, err = f.svc.Get(ctx, root(), id)
		assert.ErrorIs(t, err, domain.ErrShareNotFound, id)
	}

	// Expired shares are not listed.
	f.clk.Advance(10 * time.Minute)
	list, err := f.svc.List(ctx, alice(), share.ListFilter{})
	require.NoError(t, err)
	assert.Equal(t, []string{a1.ID}, ids(list))
}

func TestRevoke(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fixtureOption{})
	f.expectIAM()
	f.expectSigner()
	f.allow()
	in := share.CreateInput{Owner: alice(), Bucket: "photos", Key: "a.jpg", Permission: share.PermissionDownload}
	sh, err := f.svc.Create(ctx, in)
	require.NoError(t, err)

	assert.ErrorIs(t, f.svc.Revoke(ctx, bob(), sh.ID), domain.ErrShareNotFound, "bob cannot revoke alice's link")
	assert.NotNil(t, f.key(sh.AccessKey))

	require.NoError(t, f.svc.Revoke(ctx, alice(), sh.ID))
	assert.Nil(t, f.key(sh.AccessKey), "the service account is gone: the link stops working")
	assert.Equal(t, []string{sh.AccessKey}, f.deleted)
	_, err = f.svc.Get(ctx, alice(), sh.ID)
	assert.ErrorIs(t, err, domain.ErrShareNotFound)
	assert.ErrorIs(t, f.svc.Revoke(ctx, alice(), sh.ID), domain.ErrShareNotFound)

	// A share whose service account already vanished is still revocable.
	sh2, err := f.svc.Create(ctx, in)
	require.NoError(t, err)
	f.mu.Lock()
	delete(f.keys, sh2.AccessKey)
	f.mu.Unlock()
	require.NoError(t, f.svc.Revoke(ctx, root(), sh2.ID))

	// An IAM failure keeps the record so that the revocation can be retried.
	sh3, err := f.svc.Create(ctx, in)
	require.NoError(t, err)
	f.iam.ExpectedCalls = nil
	f.iam.EXPECT().DeleteAccessKey(mock.Anything, mock.Anything, sh3.AccessKey).Return(errors.New("iam down")).Once()
	assert.ErrorContains(t, f.svc.Revoke(ctx, alice(), sh3.ID), "iam down")
	_, err = f.svc.Get(ctx, alice(), sh3.ID)
	assert.NoError(t, err)
}

func TestRevokeAllAndPurge(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fixtureOption{})
	f.expectIAM()
	f.expectSigner()
	f.allow()
	for _, owner := range []domain.Identity{alice(), alice(), alice(), bob()} {
		_, err := f.svc.Create(ctx, share.CreateInput{Owner: owner, Bucket: "photos", Key: "a.jpg", Permission: share.PermissionDownload, Expires: time.Hour})
		require.NoError(t, err)
	}
	_, err := f.svc.Create(ctx, share.CreateInput{Owner: bob(), Bucket: "photos", Key: "b.jpg", Permission: share.PermissionDownload, Expires: 2 * time.Hour})
	require.NoError(t, err)

	_, err = f.svc.RevokeAll(ctx, alice(), "bob")
	assert.ErrorIs(t, err, domain.ErrAccessDenied)
	_, err = f.svc.RevokeAll(ctx, *domain.Anonymous(), "")
	assert.ErrorIs(t, err, domain.ErrAccessDenied)

	n, err := f.svc.RevokeAll(ctx, domain.Identity{AccessKey: "AK", Parent: "alice"}, "")
	require.NoError(t, err)
	assert.Equal(t, 3, n)
	n, err = f.svc.RevokeAll(ctx, root(), "nobody")
	require.NoError(t, err)
	assert.Zero(t, n)

	// After one hour bob's first link expires and is purged with its account.
	f.clk.Advance(time.Hour)
	n, err = f.svc.PurgeExpired(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	list, err := f.svc.List(ctx, root(), share.ListFilter{})
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "b.jpg", list[0].Key)
	assert.Len(t, f.deleted, 4)

	// Root revokes the rest of bob's links.
	n, err = f.svc.RevokeAll(ctx, root(), "bob")
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	// Creation purges what expired in the meantime.
	_, err = f.svc.Create(ctx, share.CreateInput{Owner: bob(), Bucket: "photos", Key: "c.jpg", Permission: share.PermissionDownload, Expires: time.Second})
	require.NoError(t, err)
	f.clk.Advance(time.Second)
	_, err = f.svc.Create(ctx, share.CreateInput{Owner: bob(), Bucket: "photos", Key: "d.jpg", Permission: share.PermissionDownload})
	require.NoError(t, err)
	list, err = f.svc.List(ctx, root(), share.ListFilter{})
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "d.jpg", list[0].Key)
	assert.Len(t, f.deleted, 6)
}

func TestRemoveWhereReportsErrors(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fixtureOption{})
	f.expectSigner()
	f.allow()
	f.iam.EXPECT().CreateAccessKey(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, in iam.CreateAccessKeyInput) (*iam.AccessKey, error) {
		return &iam.AccessKey{AccessKey: in.AccessKey, SecretKey: "s"}, nil
	}).Times(2)
	for range 2 {
		_, err := f.svc.Create(ctx, share.CreateInput{Owner: alice(), Bucket: "photos", Key: "a.jpg", Permission: share.PermissionDownload})
		require.NoError(t, err)
	}
	f.iam.EXPECT().DeleteAccessKey(mock.Anything, mock.Anything, mock.Anything).Return(errors.New("iam down")).Once()
	f.iam.EXPECT().DeleteAccessKey(mock.Anything, mock.Anything, mock.Anything).Return(nil).Once()
	n, err := f.svc.RevokeAll(ctx, alice(), "")
	assert.Equal(t, 1, n)
	assert.ErrorContains(t, err, "iam down")
}

func TestPresign(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fixtureOption{})
	f.expectIAM()
	calls := f.expectSigner()
	f.allow()
	prefix, err := f.svc.Create(ctx, share.CreateInput{Owner: alice(), Bucket: "photos", Key: "2026/", Prefix: true, Permission: share.PermissionDownload, Expires: time.Hour})
	require.NoError(t, err)
	upload, err := f.svc.Create(ctx, share.CreateInput{Owner: alice(), Bucket: "drop", Key: "in/", Prefix: true, Permission: share.PermissionUpload, ContentType: "text/csv", Expires: time.Hour})
	require.NoError(t, err)
	object, err := f.svc.Create(ctx, share.CreateInput{Owner: alice(), Bucket: "photos", Key: "a.jpg", Permission: share.PermissionDownload, VersionID: "v9", Expires: time.Hour})
	require.NoError(t, err)

	f.clk.Advance(15 * time.Minute)
	u, err := f.svc.Presign(ctx, alice(), prefix.ID, "2026/x/y.jpg")
	require.NoError(t, err)
	assert.Equal(t, "/photos/2026/x/y.jpg", u.Path)
	in := (*calls)[len(*calls)-1]
	assert.Equal(t, http.MethodGet, in.Method)
	assert.Equal(t, 45*time.Minute, in.Expires, "the remaining validity")
	assert.Equal(t, prefix.AccessKey, in.Creds.AccessKey)
	assert.Empty(t, in.URL.RawQuery)

	_, err = f.svc.Presign(ctx, alice(), upload.ID, "in/data.csv")
	require.NoError(t, err)
	in = (*calls)[len(*calls)-1]
	assert.Equal(t, http.MethodPut, in.Method)
	assert.Equal(t, http.Header{"Content-Type": {"text/csv"}}, in.Headers)

	u, err = f.svc.Presign(ctx, alice(), object.ID, "")
	require.NoError(t, err)
	assert.Equal(t, "v9", u.Query().Get("versionId"))
	_, err = f.svc.Presign(ctx, root(), object.ID, "a.jpg")
	require.NoError(t, err)

	tests := []struct {
		name   string
		caller domain.Identity
		id     string
		key    string
		want   error
	}{
		{"outside the prefix", alice(), prefix.ID, "2025/a.jpg", domain.ErrInvalidArgument},
		{"invalid key", alice(), prefix.ID, "2026/../x", domain.ErrInvalidObjectName},
		{"the prefix itself", alice(), prefix.ID, "", domain.ErrInvalidObjectName},
		{"another object", alice(), object.ID, "b.jpg", domain.ErrInvalidArgument},
		{"another user", bob(), object.ID, "", domain.ErrShareNotFound},
		{"unknown share", alice(), "zzzzzzzzzzzz", "", domain.ErrShareNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := f.svc.Presign(ctx, tt.caller, tt.id, tt.key)
			assert.ErrorIs(t, err, tt.want)
		})
	}

	// A revoked account or an expired share signs nothing.
	f.mu.Lock()
	delete(f.keys, object.AccessKey)
	f.mu.Unlock()
	_, err = f.svc.Presign(ctx, alice(), object.ID, "")
	assert.ErrorIs(t, err, domain.ErrShareNotFound)
	f.clk.Advance(time.Hour)
	_, err = f.svc.Presign(ctx, alice(), prefix.ID, "2026/a.jpg")
	assert.ErrorIs(t, err, domain.ErrShareNotFound)
}

func TestStoreErrors(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("disk on fire")

	store := iammock.NewStore(t)
	f := newFixture(t, fixtureOption{store: store})
	store.EXPECT().List(mock.Anything, iam.NamespaceShares).Return(nil, boom)
	store.EXPECT().Get(mock.Anything, iam.NamespaceShares, "abcdefghijkl").Return(nil, boom).Once()
	store.EXPECT().Get(mock.Anything, iam.NamespaceShares, "bcdefghijklm").Return([]byte("{"), nil).Once()
	store.EXPECT().Get(mock.Anything, iam.NamespaceShares, "cdefghijklmn").Return([]byte(`{"id":"other","accessKey":"x"}`), nil).Once()

	_, err := f.svc.List(ctx, root(), share.ListFilter{})
	assert.ErrorIs(t, err, boom)
	_, err = f.svc.PurgeExpired(ctx)
	assert.ErrorIs(t, err, boom)
	_, err = f.svc.Get(ctx, root(), "abcdefghijkl")
	assert.ErrorIs(t, err, boom)
	_, err = f.svc.Get(ctx, root(), "bcdefghijklm")
	assert.ErrorIs(t, err, domain.ErrInvalidFormat)
	_, err = f.svc.Get(ctx, root(), "cdefghijklmn")
	assert.ErrorIs(t, err, domain.ErrInvalidFormat)

	// A purge failure does not prevent creation.
	f.expectIAM()
	f.expectSigner()
	f.allow()
	store.EXPECT().Put(mock.Anything, iam.NamespaceShares, mock.Anything, mock.Anything).Return(nil).Once()
	_, err = f.svc.Create(ctx, share.CreateInput{Owner: alice(), Bucket: "photos", Key: "a.jpg", Permission: share.PermissionDownload})
	require.NoError(t, err)

	// A record whose deletion fails after the account is gone.
	store2 := iammock.NewStore(t)
	f2 := newFixture(t, fixtureOption{store: store2})
	f2.expectIAM()
	raw := []byte(`{"id":"abcdefghijkl","owner":"alice","bucket":"b","key":"k","permission":"download","accessKey":"share-abcdefghijkl"}`)
	store2.EXPECT().Get(mock.Anything, iam.NamespaceShares, "abcdefghijkl").Return(raw, nil).Once()
	store2.EXPECT().Delete(mock.Anything, iam.NamespaceShares, "abcdefghijkl").Return(boom).Once()
	assert.ErrorIs(t, f2.svc.Revoke(ctx, alice(), "abcdefghijkl"), boom)
}

func TestConcurrentCreateAndRevoke(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, fixtureOption{})
	f.expectIAM()
	f.expectSigner()
	f.allow()
	var wg sync.WaitGroup
	ids := make(chan string, 32)
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sh, err := f.svc.Create(ctx, share.CreateInput{Owner: alice(), Bucket: "photos", Key: "a.jpg", Permission: share.PermissionDownload})
			if assert.NoError(t, err) {
				ids <- sh.ID
			}
			_, err = f.svc.List(ctx, alice(), share.ListFilter{})
			assert.NoError(t, err)
		}()
	}
	wg.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		assert.False(t, seen[id], "unique ids")
		seen[id] = true
		wg.Add(1)
		go func() {
			defer wg.Done()
			assert.NoError(t, f.svc.Revoke(ctx, alice(), id))
		}()
	}
	wg.Wait()
	list, err := f.svc.List(ctx, alice(), share.ListFilter{})
	require.NoError(t, err)
	assert.Empty(t, list)
	assert.Len(t, f.deleted, 16)
}
