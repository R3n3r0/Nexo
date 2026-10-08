package admin_test

import (
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/admin"
	"github.com/r3n3r0/nexo/internal/service/iam"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/service/share"
)

// collect walks every page of a paged list and returns the keys of the
// items in order.
func collect[T any](t *testing.T, limit int, list func(admin.Page) (*admin.Paged[T], error), key func(T) string) []string {
	t.Helper()
	var keys []string
	p := admin.Page{Limit: limit}
	for range 100 {
		got, err := list(p)
		require.NoError(t, err)
		require.LessOrEqual(t, len(got.Items), limit)
		for _, it := range got.Items {
			keys = append(keys, key(it))
		}
		if got.NextCursor == "" {
			return keys
		}
		p.Cursor = got.NextCursor
	}
	t.Fatal("the pages never end")
	return nil
}

// covers: docs/12 §12.3 (the services select the pages of the console
// lists; regression: the handlers sorted and paginated whole collections)
func TestPagedLists(t *testing.T) {
	ctx := context.Background()
	t.Run("users by access key", func(t *testing.T) {
		m, users, _ := newIdentityManager(t, allowAll(t))
		users.EXPECT().ListUsers(mock.Anything).Return([]iam.User{{AccessKey: "c"}, {AccessKey: "a"}, {AccessKey: "b"}}, nil)
		got := collect(t, 2, func(p admin.Page) (*admin.Paged[iam.User], error) { return m.ListUsers(ctx, p) },
			func(u iam.User) string { return u.AccessKey })
		assert.Equal(t, []string{"a", "b", "c"}, got)
	})
	t.Run("groups by name", func(t *testing.T) {
		m, users, _ := newIdentityManager(t, allowAll(t))
		users.EXPECT().ListGroups(mock.Anything).Return([]iam.Group{{Name: "ops"}, {Name: "dev"}}, nil)
		got := collect(t, 1, func(p admin.Page) (*admin.Paged[iam.Group], error) { return m.ListGroups(ctx, p) },
			func(g iam.Group) string { return g.Name })
		assert.Equal(t, []string{"dev", "ops"}, got)
	})
	t.Run("policies by name", func(t *testing.T) {
		m, engine, _ := newPolicyManager(t)
		engine.EXPECT().ListPolicies(mock.Anything).Return([]policy.Named{{Name: "readwrite"}, {Name: "custom"}, {Name: "readonly"}}, nil)
		got := collect(t, 2, func(p admin.Page) (*admin.Paged[policy.Named], error) { return m.List(ctx, p) },
			func(n policy.Named) string { return n.Name })
		assert.Equal(t, []string{"custom", "readonly", "readwrite"}, got)
	})
	t.Run("share links newest first", func(t *testing.T) {
		e := newShareEnv(t, allowAll(t))
		base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		e.shares.EXPECT().List(mock.Anything, mock.Anything, share.ListFilter{}).Return([]share.Share{
			{ID: "old", CreatedAt: base}, {ID: "new", CreatedAt: base.Add(time.Hour)}, {ID: "mid", CreatedAt: base.Add(time.Minute)}, {ID: "mid2", CreatedAt: base.Add(time.Minute)},
		}, nil)
		got := collect(t, 2, func(p admin.Page) (*admin.Paged[share.Share], error) {
			return e.mgr.List(ctx, admin.ListSharesInput{Requester: req("bob"), Page: p})
		}, func(s share.Share) string { return s.ID })
		assert.Equal(t, []string{"new", "mid", "mid2", "old"}, got)
	})
	t.Run("invalid pages", func(t *testing.T) {
		m, users, _ := newIdentityManager(t, allowAll(t))
		users.EXPECT().ListUsers(mock.Anything).Return(nil, nil).Maybe()
		for _, p := range []admin.Page{{Limit: -1}, {Limit: admin.MaxPageLimit + 1}, {Cursor: "not base64!"}} {
			_, err := m.ListUsers(ctx, p)
			assert.ErrorIs(t, err, domain.ErrInvalidArgument, "%+v", p)
		}
		got, err := m.ListUsers(ctx, admin.Page{})
		require.NoError(t, err)
		assert.Empty(t, got.Items)
		assert.Empty(t, got.NextCursor)
	})
}

// covers: D-SHR-01 (a shared prefix is handed out one link per object,
// all revoked with the share; regression: the console showed the
// ListObjectsV2 URL of the prefix, an XML listing nobody could download
// from)
func TestShareLinks(t *testing.T) {
	ctx := context.Background()
	prefixShare := &share.Share{ID: "s1", Owner: "bob", Bucket: "bkt", Key: "docs/", Prefix: true, Permission: share.PermissionDownload}
	t.Run("one link per object, signed by the share", func(t *testing.T) {
		var asked []admin.AuthorizeInput
		e := newShareEnv(t, allowing(t, func(in admin.AuthorizeInput) bool { asked = append(asked, in); return true }))
		e.shares.EXPECT().Get(mock.Anything, mock.Anything, "s1").Return(prefixShare, nil)
		e.objects.EXPECT().ListObjects(mock.Anything, domain.ListObjectsInput{Bucket: "bkt", Prefix: "docs/", ContinuationToken: "t1", MaxKeys: 2}).
			Return(domain.ListObjectsResult{Objects: []domain.ObjectInfo{{Key: "docs/"}, {Key: "docs/a.pdf", Size: 3}, {Key: "docs/sub/b.txt", Size: 5}}, IsTruncated: true, NextContinuationToken: "t2"}, nil)
		for _, key := range []string{"docs/a.pdf", "docs/sub/b.txt"} {
			u, err := url.Parse("https://s3.example.com/bkt/" + key + "?X-Amz-Credential=share-s1")
			require.NoError(t, err)
			e.shares.EXPECT().Presign(mock.Anything, mock.Anything, "s1", key).Return(u, nil)
		}
		got, err := e.mgr.Links(ctx, admin.ShareLinksInput{Requester: req("bob"), ID: "s1", Page: admin.Page{Cursor: "t1", Limit: 2}})
		require.NoError(t, err)
		assert.Equal(t, prefixShare, got.Share)
		assert.Equal(t, []admin.ShareLink{
			{Key: "docs/a.pdf", Size: 3, URL: "https://s3.example.com/bkt/docs/a.pdf?X-Amz-Credential=share-s1"},
			{Key: "docs/sub/b.txt", Size: 5, URL: "https://s3.example.com/bkt/docs/sub/b.txt?X-Amz-Credential=share-s1"},
		}, got.Items, "the folder placeholder has no link")
		assert.Equal(t, "t2", got.NextCursor)
		// The enumeration is a listing of the requester.
		require.Len(t, asked, 2)
		assert.Equal(t, []policy.Action{policy.ActionListBucket}, asked[1].Actions)
		assert.Equal(t, "bkt", asked[1].Bucket)
		assert.Equal(t, []string{"docs/"}, asked[1].Conditions["s3:prefix"])
	})
	t.Run("the requester may not list the prefix", func(t *testing.T) {
		e := newShareEnv(t, allowing(t, func(in admin.AuthorizeInput) bool { return in.Actions[0] != policy.ActionListBucket }))
		e.shares.EXPECT().Get(mock.Anything, mock.Anything, "s1").Return(prefixShare, nil)
		_, err := e.mgr.Links(ctx, admin.ShareLinksInput{Requester: req("bob"), ID: "s1"})
		assert.ErrorIs(t, err, domain.ErrAccessDenied)
	})
	t.Run("only prefix download shares", func(t *testing.T) {
		for _, sh := range []*share.Share{
			{ID: "o", Bucket: "bkt", Key: "a.pdf", Permission: share.PermissionDownload},
			{ID: "u", Bucket: "bkt", Key: "in/", Prefix: true, Permission: share.PermissionUpload},
		} {
			e := newShareEnv(t, allowAll(t))
			e.shares.EXPECT().Get(mock.Anything, mock.Anything, sh.ID).Return(sh, nil)
			_, err := e.mgr.Links(ctx, admin.ShareLinksInput{Requester: req("bob"), ID: sh.ID})
			assert.ErrorIs(t, err, domain.ErrInvalidArgument, sh.ID)
		}
	})
	t.Run("a share of another user", func(t *testing.T) {
		e := newShareEnv(t, allowAll(t))
		e.shares.EXPECT().Get(mock.Anything, mock.Anything, "s9").Return(nil, fmt.Errorf("x: %w", domain.ErrShareNotFound))
		_, err := e.mgr.Links(ctx, admin.ShareLinksInput{Requester: req("bob"), ID: "s9"})
		assert.ErrorIs(t, err, domain.ErrShareNotFound)
	})
	t.Run("invalid limit", func(t *testing.T) {
		e := newShareEnv(t, allowAll(t))
		_, err := e.mgr.Links(ctx, admin.ShareLinksInput{Requester: req("bob"), ID: "s1", Page: admin.Page{Limit: 5000}})
		assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	})
}
