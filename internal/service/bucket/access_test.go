package bucket_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/bucket"
)

// TestAccessRulesDocument checks the storage of the access rules: the
// empty document of a bucket without rules, the ETag, the atomic
// replacement with If-Match, the persistence and the read of the
// evaluation (AccessConfig, from the cache).
func TestAccessRulesDocument(t *testing.T) {
	ctx := context.Background()
	svc, objects, store, _, clk := newService(t)
	objects.EXPECT().GetBucketInfo(mock.Anything, "media").Return(domain.BucketInfo{Bucket: domain.Bucket{Name: "media"}}, nil)
	objects.EXPECT().GetBucketInfo(mock.Anything, "missing").Return(domain.BucketInfo{}, domain.ErrBucketNotFound)

	empty, err := svc.GetAccessRules(ctx, "media")
	require.NoError(t, err)
	assert.JSONEq(t, "[]", string(empty.Document))
	assert.Equal(t, bucket.AccessRulesETag(nil), empty.ETag)
	assert.Equal(t, bucket.AccessRulesETag([]byte("[]")), empty.ETag)
	assert.Len(t, empty.ETag, 32)

	ac, err := store.AccessConfig(ctx, "media")
	require.NoError(t, err)
	assert.Nil(t, ac.Rules)
	assert.False(t, ac.BlockPublicAccess)

	doc1 := json.RawMessage(`[{"id":"a"}]`)
	change, err := svc.SetAccessRules(ctx, "media", bucket.SetAccessRulesInput{Document: doc1, IfMatch: empty.ETag, UpdatedBy: "admin"})
	require.NoError(t, err)
	assert.Equal(t, empty.ETag, change.Previous.ETag)
	assert.Equal(t, bucket.AccessRulesETag(doc1), change.Current.ETag)
	assert.NotEqual(t, empty.ETag, change.Current.ETag)
	assert.Equal(t, "admin", change.Current.UpdatedBy)
	assert.Equal(t, clk.Now(), change.Current.UpdatedAt)

	got, err := svc.GetAccessRules(ctx, "media")
	require.NoError(t, err)
	assert.Equal(t, change.Current, got)
	ac, err = store.AccessConfig(ctx, "media")
	require.NoError(t, err)
	require.NotNil(t, ac.Rules)
	assert.Equal(t, change.Current.ETag, ac.Rules.ETag)

	// A stale ETag is refused, the rules are left alone.
	_, err = svc.SetAccessRules(ctx, "media", bucket.SetAccessRulesInput{Document: json.RawMessage(`[]`), IfMatch: empty.ETag})
	assert.ErrorIs(t, err, domain.ErrPreconditionFailed)
	got, err = svc.GetAccessRules(ctx, "media")
	require.NoError(t, err)
	assert.JSONEq(t, string(doc1), string(got.Document))

	// Without If-Match the replacement is unconditional; the persisted
	// document survives the cache.
	doc2 := json.RawMessage(`[{"id":"b"}]`)
	change, err = svc.SetAccessRules(ctx, "media", bucket.SetAccessRulesInput{Document: doc2})
	require.NoError(t, err)
	assert.JSONEq(t, string(doc1), string(change.Previous.Document))
	store.Invalidate(ctx, "")
	ac, err = store.AccessConfig(ctx, "media")
	require.NoError(t, err)
	assert.JSONEq(t, string(doc2), string(ac.Rules.Document))
	assert.Equal(t, bucket.AccessRulesETag(doc2), ac.Rules.ETag)

	// The other settings keep the rules (read-modify-write of the
	// document) and a caller cannot alter the cache.
	require.NoError(t, svc.SetVersioning(ctx, "media", domain.VersioningEnabled))
	m, err := store.Load(ctx, "media")
	require.NoError(t, err)
	m.AccessRules.Document[2] = 'X'
	got, err = svc.GetAccessRules(ctx, "media")
	require.NoError(t, err)
	assert.JSONEq(t, string(doc2), string(got.Document))

	// Malformed documents and missing buckets.
	_, err = svc.SetAccessRules(ctx, "media", bucket.SetAccessRulesInput{Document: json.RawMessage(`[{`)})
	assert.ErrorIs(t, err, domain.ErrInvalidArgument)
	_, err = svc.GetAccessRules(ctx, "missing")
	assert.ErrorIs(t, err, domain.ErrBucketNotFound)
	_, err = svc.SetAccessRules(ctx, "missing", bucket.SetAccessRulesInput{Document: doc2})
	assert.ErrorIs(t, err, domain.ErrBucketNotFound)
	_, err = store.AccessConfig(ctx, "Not_Valid")
	assert.ErrorIs(t, err, domain.ErrInvalidBucketName)
}

// TestBlockPublicAccessFlag checks the block public access of a bucket.
func TestBlockPublicAccessFlag(t *testing.T) {
	ctx := context.Background()
	svc, objects, store, _, _ := newService(t)
	objects.EXPECT().GetBucketInfo(mock.Anything, "media").Return(domain.BucketInfo{Bucket: domain.Bucket{Name: "media"}}, nil)

	on, err := svc.GetBlockPublicAccess(ctx, "media")
	require.NoError(t, err)
	assert.False(t, on)
	require.NoError(t, svc.SetBlockPublicAccess(ctx, "media", true))
	on, err = svc.GetBlockPublicAccess(ctx, "media")
	require.NoError(t, err)
	assert.True(t, on)
	ac, err := store.AccessConfig(ctx, "media")
	require.NoError(t, err)
	assert.True(t, ac.BlockPublicAccess)
	require.NoError(t, svc.SetBlockPublicAccess(ctx, "media", false))
	ac, err = store.AccessConfig(ctx, "media")
	require.NoError(t, err)
	assert.False(t, ac.BlockPublicAccess)
}
