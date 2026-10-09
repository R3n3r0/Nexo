package integration_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/cli/commands"
	"github.com/r3n3r0/nexo/internal/testutil"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// migrationTest is the name of the migration test: it starts its own two
// deployments (a single drive and a cluster), so it is not one of the
// suite run in every mode.
const migrationTest = "TestMigrateSingleDriveToCluster"

// lifecycleXML is the lifecycle configuration seeded on the source.
const lifecycleXML = `<LifecycleConfiguration><Rule><ID>expire-tmp</ID><Status>Enabled</Status>` +
	`<Filter><Prefix>tmp/</Prefix></Filter><Expiration><Days>7</Days></Expiration></Rule></LifecycleConfiguration>`

// TestMigrateSingleDriveToCluster is the migration of a single-drive
// deployment to a cluster of 4 nodes x 2 drives with the procedure of
// docs/08 §8.2: "nexoctl admin cluster export" of the source, "admin
// cluster import" into the cluster (IAM, configuration, bucket settings),
// then "nexoctl mirror --preserve" of every bucket. Objects, versions and
// delete markers (with content type, metadata and tags), users, groups,
// access keys, policies, configuration and bucket settings (versioning,
// policy, quota, tags, lifecycle) are identical, and the migrated
// credentials work on the cluster.
//
// covers: TM-04
func TestMigrateSingleDriveToCluster(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src := &env{t: t, srv: testutil.StartServer(t), ctx: ctx}
	src.root = src.client(src.srv.RootUser, src.srv.RootPassword, "")
	dst := &env{t: t, srv: testutil.StartCluster(t, 4, 2), ctx: ctx}
	dst.root = dst.client(dst.srv.RootUser, dst.srv.RootPassword, "")
	srcAdmin, dstAdmin := adminClient(t, src), adminClient(t, dst)

	// The source: IAM, configuration, buckets with settings and history.
	src.setPolicy("photo-read", policyJSON(`{"Effect":"Allow","Action":["s3:GetObject","s3:ListBucket"],"Resource":["arn:aws:s3:::photos","arn:aws:s3:::photos/*"]}`))
	bob := src.createUser("bob", "bob-secret-123", "photo-read")
	src.createUser("carol", "carol-secret-123")
	_, err := srcAdmin.CreateGroup(ctx, nexoadmin.CreateGroupRequest{Name: "editors", Members: []string{"carol"}, PolicyNames: []string{"readwrite"}})
	require.NoError(t, err)
	_, err = srcAdmin.CreateAccessKey(ctx, nexoadmin.CreateAccessKeyRequest{ParentUser: "bob", AccessKey: "bob-ci", SecretKey: "bob-ci-secret-123", Name: "ci"})
	require.NoError(t, err)
	_, err = srcAdmin.SetConfig(ctx, "notify_webhook", map[string]string{"endpoint": "http://hook.example/x", "auth_token": "t0ken-1"})
	require.NoError(t, err)

	for _, b := range []string{"photos", "logs"} {
		src.mkBucket(b)
	}
	_, err = src.root.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{Bucket: aws.String("photos"),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled}})
	require.NoError(t, err)
	src.putBucketPolicy("logs", policyJSON(`{"Effect":"Allow","Principal":"*","Action":["s3:GetObject"],"Resource":["arn:aws:s3:::logs/public/*"]}`))
	_, err = srcAdmin.SetBucketQuota(ctx, "logs", nexoadmin.BucketQuota{Type: "hard", Size: 1 << 30})
	require.NoError(t, err)
	require.NoError(t, srcAdmin.SetBucketTags(ctx, "photos", map[string]string{"team": "media"}))
	require.NoError(t, src.srv.Buckets.SetLifecycle(ctx, "logs", []byte(lifecycleXML)))

	putVersion := func(bucket, key, body, contentType string, meta map[string]string, tagging string) {
		in := &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: strings.NewReader(body),
			ContentType: aws.String(contentType), Metadata: meta}
		if tagging != "" {
			in.Tagging = aws.String(tagging)
		}
		_, err := src.root.PutObject(ctx, in)
		require.NoError(t, err)
	}
	putVersion("photos", "2024/a.jpg", "first a", "image/jpeg", map[string]string{"camera": "x100"}, "")
	putVersion("photos", "2024/a.jpg", "second a", "image/jpeg", nil, "stage=final")
	putVersion("photos", "2024/b.jpg", "only b", "image/jpeg", nil, "")
	putVersion("photos", "gone.txt", "removed later", "text/plain", nil, "")
	_, err = src.root.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("photos"), Key: aws.String("gone.txt")})
	require.NoError(t, err)
	putVersion("photos", "2024/a.jpg", "third a", "image/png", map[string]string{"edited": "yes"}, "")
	putVersion("logs", "public/today.log", "log line", "text/plain", nil, "kind=log")
	putVersion("logs", "tmp/scratch", "scratch", "application/octet-stream", nil, "")

	// The migration: export, import into the cluster, mirror the objects.
	dir := t.TempDir()
	nexoctl(t, dir, "alias", "set", "old", src.srv.S3URL, src.srv.RootUser, src.srv.RootPassword, "--admin-url", src.srv.ConsoleURL)
	nexoctl(t, dir, "alias", "set", "new", dst.srv.S3URL, dst.srv.RootUser, dst.srv.RootPassword, "--admin-url", dst.srv.ConsoleURL)
	archive := filepath.Join(dir, "backup.zip")
	nexoctl(t, dir, "admin", "cluster", "export", "old", "--output", archive)
	out := nexoctl(t, dir, "admin", "cluster", "import", "new", archive)
	assert.Contains(t, out, "Buckets created: logs, photos")
	for _, b := range []string{"photos", "logs"} {
		nexoctl(t, dir, "mirror", "--preserve", "old/"+b, "new/"+b)
	}
	out = nexoctl(t, dir, "mirror", "--preserve", "old/photos", "new/photos")
	assert.Contains(t, out, "Mirrored 0 objects", "a second run replays nothing")

	// Objects and versions.
	photos := versionHistory(t, src, "photos")
	require.Len(t, photos, 6, "3 versions of a.jpg, b.jpg, gone.txt and its delete marker")
	assert.Contains(t, photos, "gone.txt delete-marker latest=true")
	assert.Equal(t, photos, versionHistory(t, dst, "photos"))
	assert.Equal(t, versionHistory(t, src, "logs"), versionHistory(t, dst, "logs"))

	// IAM, configuration and bucket settings.
	srcIAM := iamState(t, srcAdmin)
	require.Len(t, srcIAM, 5, "bob, carol, editors, bob-ci, photo-read")
	assert.Equal(t, srcIAM, iamState(t, dstAdmin))
	for _, sub := range []string{"notify_webhook"} {
		want, err := srcAdmin.GetConfig(ctx, sub)
		require.NoError(t, err)
		got, err := dstAdmin.GetConfig(ctx, sub)
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}
	settings := bucketState(t, src, srcAdmin)
	assert.Equal(t, settings, bucketState(t, dst, dstAdmin))

	// The migrated credentials work on the cluster with their policies.
	bobOnCluster := dst.clientFor(bob)
	assert.Equal(t, []byte("third a"), getWith(t, ctx, bobOnCluster, "photos", "2024/a.jpg"))
	_, err = bobOnCluster.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("photos"), Key: aws.String("x"), Body: strings.NewReader("x")})
	assert.Equal(t, "AccessDenied", apiCode(err), "photo-read is read-only")
	assert.Equal(t, []byte("only b"), getWith(t, ctx, dst.client("bob-ci", "bob-ci-secret-123", ""), "photos", "2024/b.jpg"))
}

// adminClient returns an Admin API client of the root user.
func adminClient(t *testing.T, e *env) nexoadmin.AdminClient {
	t.Helper()
	c, err := nexoadmin.New(nexoadmin.Config{Endpoint: e.srv.ConsoleURL, AccessKey: e.srv.RootUser, SecretKey: e.srv.RootPassword, Region: e.srv.Config.Region})
	require.NoError(t, err)
	return c
}

// nexoctl runs nexoctl in-process with its configuration in dir and
// returns its output; the command must succeed.
func nexoctl(t *testing.T, dir string, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	lookup := func(key string) (string, bool) {
		if key == "NEXOCTL_CONFIG_DIR" {
			return dir, true
		}
		return "", false
	}
	deps := commands.Deps{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &out, LookupEnv: lookup,
		HomeDir: func() (string, error) { return dir, nil }}
	code := commands.Run(deps, args)
	require.Equal(t, 0, code, "nexoctl %s:\n%s", strings.Join(args, " "), out.String())
	return out.String()
}

// versionHistory describes every version of a bucket, the latest first
// for each key: the key, a delete marker or the content, content type,
// user metadata and tags of the version.
func versionHistory(t *testing.T, e *env, bucket string) []string {
	t.Helper()
	ctx := e.ctx
	list, err := e.root.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	require.False(t, aws.ToBool(list.IsTruncated))
	type entry struct {
		key, id string
		latest  bool
		marker  bool
		order   int
	}
	var all []entry
	for i, v := range list.Versions {
		all = append(all, entry{key: aws.ToString(v.Key), id: aws.ToString(v.VersionId), latest: aws.ToBool(v.IsLatest), order: i})
	}
	for i, m := range list.DeleteMarkers {
		all = append(all, entry{key: aws.ToString(m.Key), id: aws.ToString(m.VersionId), latest: aws.ToBool(m.IsLatest), marker: true, order: len(list.Versions) + i})
	}
	// Versions and markers interleave by date within a key: the listing
	// returns them in two arrays, so the dates order them back.
	dates := map[string]int64{}
	for _, v := range list.Versions {
		dates[aws.ToString(v.VersionId)] = aws.ToTime(v.LastModified).UnixNano()
	}
	for _, m := range list.DeleteMarkers {
		dates[aws.ToString(m.VersionId)] = aws.ToTime(m.LastModified).UnixNano()
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].key != all[j].key {
			return all[i].key < all[j].key
		}
		return dates[all[i].id] > dates[all[j].id]
	})
	var out []string
	for _, v := range all {
		if v.marker {
			out = append(out, fmt.Sprintf("%s delete-marker latest=%v", v.key, v.latest))
			continue
		}
		obj, err := e.root.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(v.key), VersionId: aws.String(v.id)})
		require.NoError(t, err)
		body, err := io.ReadAll(obj.Body)
		require.NoError(t, err)
		_ = obj.Body.Close()
		tags, err := e.root.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{Bucket: aws.String(bucket), Key: aws.String(v.key), VersionId: aws.String(v.id)})
		require.NoError(t, err)
		var tagList []string
		for _, tg := range tags.TagSet {
			tagList = append(tagList, aws.ToString(tg.Key)+"="+aws.ToString(tg.Value))
		}
		sort.Strings(tagList)
		out = append(out, fmt.Sprintf("%s %q %s %v %v latest=%v", v.key, body, aws.ToString(obj.ContentType), obj.Metadata, tagList, v.latest))
	}
	return out
}

// iamState describes the users, groups, access keys and custom policies.
func iamState(t *testing.T, c nexoadmin.AdminClient) []string {
	t.Helper()
	ctx := context.Background()
	var out []string
	users, err := c.ListUsers(ctx, nexoadmin.ListOptions{})
	require.NoError(t, err)
	for _, u := range users.Items {
		out = append(out, fmt.Sprintf("user %s %s %v %v", u.AccessKey, u.Status, u.PolicyNames, u.Groups))
	}
	groups, err := c.ListGroups(ctx, nexoadmin.ListOptions{})
	require.NoError(t, err)
	for _, g := range groups.Items {
		out = append(out, fmt.Sprintf("group %s %s %v %v", g.Name, g.Status, g.Members, g.PolicyNames))
	}
	keys, err := c.ListAccessKeys(ctx, nexoadmin.ListAccessKeysOptions{})
	require.NoError(t, err)
	for _, k := range keys.Items {
		out = append(out, fmt.Sprintf("key %s %s %s %s", k.AccessKey, k.ParentUser, k.Name, k.Status))
	}
	policies, err := c.ListPolicies(ctx, nexoadmin.ListOptions{})
	require.NoError(t, err)
	for _, p := range policies.Items {
		if !p.BuiltIn {
			out = append(out, fmt.Sprintf("policy %s %s", p.Name, p.Document))
		}
	}
	sort.Strings(out)
	return out
}

// bucketState describes the settings of the buckets.
func bucketState(t *testing.T, e *env, c nexoadmin.AdminClient) []string {
	t.Helper()
	ctx := e.ctx
	var out []string
	list, err := c.ListBuckets(ctx, nexoadmin.ListBucketsOptions{})
	require.NoError(t, err)
	for _, b := range list.Items {
		versioning, err := c.GetBucketVersioning(ctx, b.Name)
		require.NoError(t, err)
		tags, err := c.GetBucketTags(ctx, b.Name)
		require.NoError(t, err)
		quota, _ := c.GetBucketQuota(ctx, b.Name)
		policy, _ := c.GetBucketPolicy(ctx, b.Name)
		lifecycle, _ := e.srv.Buckets.GetLifecycle(ctx, b.Name)
		out = append(out, fmt.Sprintf("%s versioning=%s tags=%v quota=%+v policy=%s lifecycle=%s", b.Name, versioning, tags, quota, policy, lifecycle))
	}
	return out
}
