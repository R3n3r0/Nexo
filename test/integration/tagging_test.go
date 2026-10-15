package integration_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBucketTagging covers the bucket tags: round trip, deletion and the
// S3 validation rules.
//
// covers: S-BKT-05
func TestBucketTagging(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := e.ctx
	const bucket = "tagged"
	e.mkBucket(bucket)
	_, err := e.root.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: aws.String(bucket)})
	assert.Equal(t, "NoSuchTagSet", apiCode(err))
	tagSet := func(kv ...string) *types.Tagging {
		var tags []types.Tag
		for i := 0; i+1 < len(kv); i += 2 {
			tags = append(tags, types.Tag{Key: aws.String(kv[i]), Value: aws.String(kv[i+1])})
		}
		return &types.Tagging{TagSet: tags}
	}
	_, err = e.root.PutBucketTagging(ctx, &s3.PutBucketTaggingInput{Bucket: aws.String(bucket), Tagging: tagSet("team", "blue", "env", "prod")})
	require.NoError(t, err)
	got, err := e.root.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	assert.Len(t, got.TagSet, 2)

	many := make([]string, 0, 102)
	for i := 0; i < 51; i++ {
		many = append(many, fmt.Sprintf("k%d", i), "v")
	}
	for name, tc := range map[string]struct {
		tagging *types.Tagging
		code    string
	}{
		"more than 50 tags": {tagSet(many...), "BadRequest"},
		"reserved prefix":   {tagSet("aws:owner", "x"), "InvalidTag"},
		"key too long":      {tagSet(strings.Repeat("k", 129), "v"), "InvalidTag"},
		"value too long":    {tagSet("k", strings.Repeat("v", 257)), "InvalidTag"},
		"duplicate keys":    {tagSet("k", "1", "k", "2"), "InvalidTag"},
	} {
		_, err := e.root.PutBucketTagging(ctx, &s3.PutBucketTaggingInput{Bucket: aws.String(bucket), Tagging: tc.tagging})
		assert.Equal(t, tc.code, apiCode(err), name)
	}
	got, err = e.root.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	assert.Len(t, got.TagSet, 2, "a refused tag set leaves the tags as they were")
	_, err = e.root.DeleteBucketTagging(ctx, &s3.DeleteBucketTaggingInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	_, err = e.root.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: aws.String(bucket)})
	assert.Equal(t, "NoSuchTagSet", apiCode(err))
}

// TestObjectTagsAndPolicies covers the tags of uploads and copies
// (x-amz-tagging, x-amz-tagging-directive) and the tag condition keys of
// the policies: s3:RequestObjectTag/<key> (header of PutObject, body of
// PutObjectTagging) and s3:ExistingObjectTag/<key> (tags of the object
// read), in Allow and Deny statements.
//
// covers: S-OBJ-11, S-IAM-04, S-OBJ-04
func TestObjectTagsAndPolicies(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := e.ctx
	const bucket = "tagpol"
	e.mkBucket(bucket)

	// Uploads and copies.
	_, err := e.root.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String("src"), Body: strings.NewReader("x"),
		Tagging: aws.String("class=public&team=blue")})
	require.NoError(t, err)
	get, err := e.root.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("src")})
	require.NoError(t, err)
	_ = get.Body.Close()
	assert.Equal(t, int32(2), aws.ToInt32(get.TagCount))
	_, err = e.root.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String(bucket), Key: aws.String("copy"), CopySource: aws.String(bucket + "/src")})
	require.NoError(t, err)
	tags, err := e.root.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{Bucket: aws.String(bucket), Key: aws.String("copy")})
	require.NoError(t, err)
	assert.Len(t, tags.TagSet, 2, "COPY keeps the tags of the source")
	_, err = e.root.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String(bucket), Key: aws.String("copy"), CopySource: aws.String(bucket + "/src"),
		TaggingDirective: types.TaggingDirectiveReplace, Tagging: aws.String("class=secret")})
	require.NoError(t, err)
	tags, err = e.root.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{Bucket: aws.String(bucket), Key: aws.String("copy")})
	require.NoError(t, err)
	require.Len(t, tags.TagSet, 1, "REPLACE takes the tags of the request")
	assert.Equal(t, "secret", aws.ToString(tags.TagSet[0].Value))

	// Policies on the tags.
	e.setPolicy("tag-scoped", `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Action":["s3:PutObject","s3:PutObjectTagging"],"Resource":["arn:aws:s3:::tagpol/*"],
		 "Condition":{"StringEquals":{"s3:RequestObjectTag/team":"blue"}}},
		{"Effect":"Allow","Action":["s3:GetObject"],"Resource":["arn:aws:s3:::tagpol/*"],
		 "Condition":{"StringEquals":{"s3:ExistingObjectTag/class":"public"}}}]}`)
	scoped := e.clientFor(e.createUser("tagscoped", "tagscoped-secret", "tag-scoped"))
	putTagged := func(c *s3.Client, key, tagging string) error {
		in := &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: strings.NewReader("y")}
		if tagging != "" {
			in.Tagging = aws.String(tagging)
		}
		_, err := c.PutObject(ctx, in)
		return err
	}
	require.NoError(t, putTagged(scoped, "blue", "team=blue"), "s3:RequestObjectTag/team from x-amz-tagging")
	assert.Equal(t, "AccessDenied", apiCode(putTagged(scoped, "red", "team=red")))
	assert.Equal(t, "AccessDenied", apiCode(putTagged(scoped, "none", "")))
	setTags := func(c *s3.Client, key, team string) error {
		_, err := c.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{Bucket: aws.String(bucket), Key: aws.String(key),
			Tagging: &types.Tagging{TagSet: []types.Tag{{Key: aws.String("team"), Value: aws.String(team)}, {Key: aws.String("class"), Value: aws.String("public")}}}})
		return err
	}
	require.NoError(t, setTags(scoped, "blue", "blue"), "s3:RequestObjectTag/team from the PutObjectTagging body")
	assert.Equal(t, "AccessDenied", apiCode(setTags(scoped, "blue", "red")))
	read := func(c *s3.Client, key string) error {
		out, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
		if err == nil {
			_ = out.Body.Close()
		}
		return err
	}
	require.NoError(t, read(scoped, "src"), "s3:ExistingObjectTag/class is public")
	require.NoError(t, read(scoped, "blue"), "tags set by PutObjectTagging count")
	assert.Equal(t, "AccessDenied", apiCode(read(scoped, "copy")), "s3:ExistingObjectTag/class is secret")

	// A Deny on the tags of the object read beats a broad Allow.
	e.setPolicy("no-secrets", `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Action":["s3:*"],"Resource":["arn:aws:s3:::tagpol","arn:aws:s3:::tagpol/*"]},
		{"Effect":"Deny","Action":["s3:GetObject","s3:GetObjectTagging"],"Resource":["arn:aws:s3:::tagpol/*"],
		 "Condition":{"StringEquals":{"s3:ExistingObjectTag/class":"secret"}}}]}`)
	reader := e.clientFor(e.createUser("tagreader", "tagreader-secret", "no-secrets"))
	require.NoError(t, read(reader, "src"))
	assert.Equal(t, "AccessDenied", apiCode(read(reader, "copy")))
	_, err = reader.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{Bucket: aws.String(bucket), Key: aws.String("copy")})
	assert.Equal(t, "AccessDenied", apiCode(err))
	// Root is not restricted by the policies.
	require.NoError(t, read(e.root, "copy"))
	resp := e.signedDo(http.MethodGet, "/"+bucket+"/copy", creds{AccessKey: "tagreader", SecretKey: "tagreader-secret"}, nil, nil)
	requireS3Error(t, resp, http.StatusForbidden, "AccessDenied")
}
