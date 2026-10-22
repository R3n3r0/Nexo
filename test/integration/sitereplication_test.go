package integration_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// TestSiteReplication (S-STO-11): two deployments sharing the root
// credentials grouped by admin replicate add exchange what each had
// (users, policies, buckets with their objects), then keep the changes of
// either side (creations, deletions, new objects) in sync; the group is
// dissolved on both sides at once.
//
// covers: S-STO-11
func TestSiteReplication(t *testing.T) {
	t.Parallel()
	a, b := newEnv(t), newEnv(t)
	adminA, adminB := adminClient(t, a), adminClient(t, b)
	ctx := a.ctx
	const doc = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject"],"Resource":["arn:aws:s3:::shared/*"]}]}`

	// What each site has before the group.
	_, err := adminA.CreatePolicy(ctx, "shared-read", []byte(doc))
	require.NoError(t, err)
	_, err = adminA.CreateUser(ctx, nexoadmin.CreateUserRequest{AccessKey: "alice", SecretKey: "alice-secret-1", PolicyNames: []string{"shared-read"}})
	require.NoError(t, err)
	a.mkBucket("shared")
	_, err = a.root.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("shared"), Key: aws.String("before"), Body: bytes.NewReader([]byte("from A"))})
	require.NoError(t, err)
	_, err = adminB.CreateUser(ctx, nexoadmin.CreateUserRequest{AccessKey: "bob", SecretKey: "bob-secret-12"})
	require.NoError(t, err)
	b.mkBucket("beta")

	sites := []nexoadmin.SiteReplicationSite{
		{Name: "site-a", Endpoint: a.srv.S3URL, AdminEndpoint: a.srv.ConsoleURL},
		{Name: "site-b", Endpoint: b.srv.S3URL, AdminEndpoint: b.srv.ConsoleURL},
	}
	// A site that cannot be reached refuses the group.
	_, err = adminA.AddSiteReplication(ctx, append(sites[:1:1], nexoadmin.SiteReplicationSite{Name: "ghost", Endpoint: "http://127.0.0.1:1", AdminEndpoint: "http://127.0.0.1:1"}))
	require.Error(t, err)
	info, err := adminA.AddSiteReplication(ctx, sites)
	require.NoError(t, err)
	assert.True(t, info.Enabled)
	assert.Equal(t, "site-a", info.Name)
	require.Len(t, info.Sites, 2)
	// A second group is refused.
	_, err = adminB.AddSiteReplication(ctx, sites)
	require.Error(t, err)

	eventually := func(what string, cond func() bool) {
		t.Helper()
		require.Eventually(t, cond, 90*time.Second, 200*time.Millisecond, what)
	}
	hasUser := func(c nexoadmin.AdminClient, name string) bool {
		_, err := c.GetUser(ctx, name)
		return err == nil
	}
	readObject := func(e *env, bucket, key, want string) bool {
		data, _, err := readVersion(ctx, e.root, bucket, key, "")
		return err == nil && string(data) == want
	}
	eventually("alice on B", func() bool { return hasUser(adminB, "alice") })
	eventually("bob on A", func() bool { return hasUser(adminA, "bob") })
	u, err := adminB.GetUser(ctx, "alice")
	require.NoError(t, err)
	assert.Contains(t, u.PolicyNames, "shared-read")
	_, err = adminB.GetPolicy(ctx, "shared-read")
	require.NoError(t, err)
	// The users authenticate on both sites with the same secret.
	alice := b.client("alice", "alice-secret-1", "")
	eventually("the object written before the group on B", func() bool { return readObject(b, "shared", "before", "from A") })
	_, err = alice.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("shared"), Key: aws.String("before")})
	require.NoError(t, err)
	eventually("bucket beta on A", func() bool {
		_, err := a.root.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String("beta")})
		return err == nil
	})

	// Changes on either side follow.
	_, err = b.root.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("beta"), Key: aws.String("k"), Body: bytes.NewReader([]byte("from B"))})
	require.NoError(t, err)
	eventually("the object written on B on A", func() bool { return readObject(a, "beta", "k", "from B") })
	_, err = adminB.CreateUser(ctx, nexoadmin.CreateUserRequest{AccessKey: "carol", SecretKey: "carol-secret-1"})
	require.NoError(t, err)
	eventually("carol on A", func() bool { return hasUser(adminA, "carol") })
	require.NoError(t, adminA.DeleteUser(ctx, "alice"))
	eventually("alice deleted on B", func() bool { return !hasUser(adminB, "alice") })
	b.mkBucket("gamma")
	eventually("bucket gamma on A", func() bool {
		_, err := a.root.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String("gamma")})
		return err == nil
	})

	eventually("both sites in sync", func() bool {
		st, err := adminA.GetSiteReplication(ctx)
		return err == nil && len(st.Peers) == 1 && !st.Peers[0].LastSync.IsZero() && st.Peers[0].LastError == ""
	})

	// Dissolving the group leaves what was replicated.
	require.NoError(t, adminA.RemoveSiteReplication(ctx))
	for _, c := range []nexoadmin.AdminClient{adminA, adminB} {
		st, err := c.GetSiteReplication(ctx)
		require.NoError(t, err)
		assert.False(t, st.Enabled)
	}
	assert.True(t, hasUser(adminB, "carol"))
	assert.True(t, readObject(a, "beta", "k", "from B"))
}
