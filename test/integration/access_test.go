package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/iam"
	"github.com/r3n3r0/nexo/internal/service/policy"
	"github.com/r3n3r0/nexo/internal/service/share"
)

// operation is one S3 call of the permission matrix, performed by the user
// on its own bucket ("mx-<user>") and home prefix ("home/<user>/").
type operation struct {
	name string
	run  func(ctx context.Context, c *s3.Client, bucket, home string) error
}

// denied reports whether err (or the DeleteObjects result) is an access
// denial; any other error fails the test.
func matrixOperations() []operation {
	str := aws.String
	return []operation{
		{"ListBuckets", func(ctx context.Context, c *s3.Client, _, _ string) error {
			_, err := c.ListBuckets(ctx, &s3.ListBucketsInput{})
			return err
		}},
		{"CreateBucket", func(ctx context.Context, c *s3.Client, b, _ string) error {
			_, err := c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: str(b + "-new")})
			return err
		}},
		{"HeadBucket", func(ctx context.Context, c *s3.Client, b, _ string) error {
			_, err := c.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: str(b)})
			return err
		}},
		{"ListObjectsV2 home", func(ctx context.Context, c *s3.Client, b, home string) error {
			_, err := c.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: str(b), Prefix: str(home)})
			return err
		}},
		{"ListObjects all", func(ctx context.Context, c *s3.Client, b, _ string) error {
			_, err := c.ListObjects(ctx, &s3.ListObjectsInput{Bucket: str(b)})
			return err
		}},
		{"GetBucketLocation", func(ctx context.Context, c *s3.Client, b, _ string) error {
			_, err := c.GetBucketLocation(ctx, &s3.GetBucketLocationInput{Bucket: str(b)})
			return err
		}},
		{"GetBucketPolicy", func(ctx context.Context, c *s3.Client, b, _ string) error {
			_, err := c.GetBucketPolicy(ctx, &s3.GetBucketPolicyInput{Bucket: str(b)})
			return err
		}},
		{"PutBucketPolicy", func(ctx context.Context, c *s3.Client, b, _ string) error {
			_, err := c.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{Bucket: str(b), Policy: str(policyJSON(
				fmt.Sprintf(`{"Effect":"Deny","Principal":"*","Action":["s3:GetObject"],"Resource":["arn:aws:s3:::%s/nothing/*"]}`, b)))})
			return err
		}},
		{"DeleteBucketPolicy", func(ctx context.Context, c *s3.Client, b, _ string) error {
			_, err := c.DeleteBucketPolicy(ctx, &s3.DeleteBucketPolicyInput{Bucket: str(b)})
			return err
		}},
		{"GetBucketVersioning", func(ctx context.Context, c *s3.Client, b, _ string) error {
			_, err := c.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: str(b)})
			return err
		}},
		{"PutBucketVersioning", func(ctx context.Context, c *s3.Client, b, _ string) error {
			_, err := c.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{Bucket: str(b),
				VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusEnabled}})
			return err
		}},
		{"PutObject", func(ctx context.Context, c *s3.Client, b, home string) error {
			_, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: str(b), Key: str(home + "new.txt"), Body: strings.NewReader("new")})
			return err
		}},
		{"GetObject", func(ctx context.Context, c *s3.Client, b, home string) error {
			out, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: str(b), Key: str(home + "doc.txt")})
			if err == nil {
				_ = out.Body.Close()
			}
			return err
		}},
		{"HeadObject", func(ctx context.Context, c *s3.Client, b, home string) error {
			_, err := c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: str(b), Key: str(home + "doc.txt")})
			return err
		}},
		{"CopyObject", func(ctx context.Context, c *s3.Client, b, home string) error {
			_, err := c.CopyObject(ctx, &s3.CopyObjectInput{Bucket: str(b), Key: str(home + "copy.txt"), CopySource: str(b + "/" + home + "doc.txt")})
			return err
		}},
		{"DeleteObject", func(ctx context.Context, c *s3.Client, b, home string) error {
			_, err := c.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: str(b), Key: str(home + "trash.txt")})
			return err
		}},
		{"DeleteObjects", func(ctx context.Context, c *s3.Client, b, home string) error {
			out, err := c.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: str(b), Delete: &types.Delete{Objects: []types.ObjectIdentifier{{Key: str(home + "trash2.txt")}}}})
			if err != nil {
				return err
			}
			if len(out.Errors) > 0 {
				return fmt.Errorf("%s: %s", aws.ToString(out.Errors[0].Code), aws.ToString(out.Errors[0].Message))
			}
			return nil
		}},
		{"CreateMultipartUpload", func(ctx context.Context, c *s3.Client, b, home string) error {
			out, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: str(b), Key: str(home + "mp.bin")})
			if err != nil {
				return err
			}
			_, err = c.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: str(b), Key: str(home + "mp.bin"), UploadId: out.UploadId})
			return err
		}},
		{"ListMultipartUploads", func(ctx context.Context, c *s3.Client, b, _ string) error {
			_, err := c.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: str(b)})
			return err
		}},
		{"GetObjectTagging", func(ctx context.Context, c *s3.Client, b, home string) error {
			_, err := c.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{Bucket: str(b), Key: str(home + "doc.txt")})
			return err
		}},
		{"PutObjectTagging", func(ctx context.Context, c *s3.Client, b, home string) error {
			_, err := c.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{Bucket: str(b), Key: str(home + "doc.txt"),
				Tagging: &types.Tagging{TagSet: []types.Tag{{Key: str("k"), Value: str("v")}}}})
			return err
		}},
		{"DeleteBucket", func(ctx context.Context, c *s3.Client, b, _ string) error {
			_, err := c.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: str(b + "-empty")})
			return err
		}},
	}
}

// customMatrixPolicy confines a user to its home prefix in the matrix
// buckets, with an explicit deny of DeleteObject.
const customMatrixPolicy = `{"Version":"2012-10-17","Statement":[
 {"Sid":"Home","Effect":"Allow","Action":["s3:*"],"Resource":["arn:aws:s3:::mx-*/home/${aws:username}/*"]},
 {"Sid":"ListHome","Effect":"Allow","Action":["s3:ListBucket"],"Resource":["arn:aws:s3:::mx-*"],
  "Condition":{"StringLike":{"s3:prefix":["home/${aws:username}/*"]}}},
 {"Sid":"Discover","Effect":"Allow","Action":["s3:GetBucketLocation","s3:ListAllMyBuckets"],"Resource":["arn:aws:s3:::*"]},
 {"Sid":"NoDelete","Effect":"Deny","Action":["s3:DeleteObject"],"Resource":["arn:aws:s3:::*"]}
]}`

// TS-I-09 / TPS-15: matrix of 22 operations × the readonly, readwrite,
// writeonly and a custom policy (variables, condition, explicit deny).
//
// covers: S-IAM-04
func TestPolicyMatrix(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.setPolicy("home-only", customMatrixPolicy)
	allowed := map[string]map[string]bool{
		"readonly": set("ListBuckets", "HeadBucket", "ListObjectsV2 home", "ListObjects all", "GetBucketLocation", "GetBucketPolicy",
			"GetBucketVersioning", "GetObject", "HeadObject", "ListMultipartUploads", "GetObjectTagging"),
		"readwrite": set(names(matrixOperations())...),
		"writeonly": set("GetBucketLocation", "PutObject", "CreateMultipartUpload", "ListMultipartUploads"),
		"home-only": set("ListBuckets", "ListObjectsV2 home", "GetBucketLocation", "PutObject", "GetObject", "HeadObject", "CopyObject",
			"CreateMultipartUpload", "GetObjectTagging", "PutObjectTagging"),
	}
	for _, pol := range []string{"readonly", "readwrite", "writeonly", "home-only"} {
		user := "mx" + strings.ReplaceAll(pol, "-", "")
		bucket, home := "mx-"+user, "home/"+user+"/"
		e.mkBucket(bucket)
		e.mkBucket(bucket + "-empty")
		for _, k := range []string{"doc.txt", "trash.txt", "trash2.txt"} {
			e.put(bucket, home+k, []byte(k))
		}
		e.putBucketPolicy(bucket, policyJSON(fmt.Sprintf(`{"Effect":"Deny","Principal":"*","Action":["s3:GetObject"],"Resource":["arn:aws:s3:::%s/nothing/*"]}`, bucket)))
		c := e.clientFor(e.createUser(user, user+"-secret", pol))
		for _, op := range matrixOperations() {
			t.Run(pol+"/"+op.name, func(t *testing.T) {
				err := op.run(e.ctx, c, bucket, home)
				if allowed[pol][op.name] {
					assert.NoError(t, err)
					return
				}
				require.Error(t, err)
				// HEAD answers carry no body, hence no S3 code.
				if !strings.Contains(err.Error(), "AccessDenied") {
					assert.Equal(t, http.StatusForbidden, httpStatus(err), "%v", err)
				}
			})
		}
	}
}

func set(items ...string) map[string]bool {
	m := map[string]bool{}
	for _, i := range items {
		m[i] = true
	}
	return m
}

func names(ops []operation) []string {
	var out []string
	for _, o := range ops {
		out = append(out, o.name)
	}
	return out
}

// TPS-15: condition operators, NotAction/NotResource, policy variables and
// explicit denies enforced on real requests.
//
// covers: S-IAM-04
func TestPolicyConditionMatrix(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	const bucket = "conds"
	e.mkBucket(bucket)
	for _, k := range []string{"pub/a.txt", "priv/b.txt", "home/condu/c.txt", "home/other/d.txt"} {
		e.put(bucket, k, []byte(k))
	}
	u := e.createUser("condu", "condu-secret")
	now := time.Now().UTC()
	stmt := func(effect, action, resource, cond string) string {
		s := fmt.Sprintf(`{"Effect":%q,"Action":[%q],"Resource":[%q]`, effect, action, resource)
		if cond != "" {
			s += `,"Condition":` + cond
		}
		return s + "}"
	}
	allowGet := func(cond string) string {
		return policyJSON(stmt("Allow", "s3:GetObject", "arn:aws:s3:::conds/*", cond))
	}
	type req struct {
		key    string
		hdr    map[string]string
		status int
	}
	cases := []struct {
		name   string
		policy string
		reqs   []req
	}{
		{"StringEquals UserAgent", allowGet(`{"StringEquals":{"aws:UserAgent":"nexo-agent/1.0"}}`), []req{
			{"pub/a.txt", map[string]string{"User-Agent": "nexo-agent/1.0"}, 200}, {"pub/a.txt", map[string]string{"User-Agent": "curl/8"}, 403}}},
		{"StringNotEquals UserAgent", allowGet(`{"StringNotEquals":{"aws:UserAgent":"blocked/1"}}`), []req{
			{"pub/a.txt", map[string]string{"User-Agent": "ok/1"}, 200}, {"pub/a.txt", map[string]string{"User-Agent": "blocked/1"}, 403}}},
		{"StringEqualsIgnoreCase", allowGet(`{"StringEqualsIgnoreCase":{"aws:UserAgent":"MiXeD"}}`), []req{
			{"pub/a.txt", map[string]string{"User-Agent": "mixed"}, 200}, {"pub/a.txt", map[string]string{"User-Agent": "mixe"}, 403}}},
		{"StringLike Referer", allowGet(`{"StringLike":{"aws:Referer":["https://*.example.com/*"]}}`), []req{
			{"pub/a.txt", map[string]string{"Referer": "https://www.example.com/page"}, 200},
			{"pub/a.txt", map[string]string{"Referer": "https://evil.com/"}, 403}, {"pub/a.txt", nil, 403}}},
		{"StringNotLike Referer", allowGet(`{"StringNotLike":{"aws:Referer":["*evil*"]}}`), []req{
			{"pub/a.txt", map[string]string{"Referer": "https://good.com/"}, 200}, {"pub/a.txt", map[string]string{"Referer": "https://evil.com/"}, 403}}},
		{"IpAddress loopback", allowGet(`{"IpAddress":{"aws:SourceIp":"127.0.0.0/8"}}`), []req{{"pub/a.txt", nil, 200}}},
		{"IpAddress other", allowGet(`{"IpAddress":{"aws:SourceIp":["10.0.0.0/8","192.168.0.0/16"]}}`), []req{{"pub/a.txt", nil, 403}}},
		{"NotIpAddress loopback", allowGet(`{"NotIpAddress":{"aws:SourceIp":"127.0.0.1/32"}}`), []req{{"pub/a.txt", nil, 403}}},
		{"Bool SecureTransport false", allowGet(`{"Bool":{"aws:SecureTransport":"false"}}`), []req{{"pub/a.txt", nil, 200}}},
		{"Bool SecureTransport true", allowGet(`{"Bool":{"aws:SecureTransport":"true"}}`), []req{{"pub/a.txt", nil, 403}}},
		{"DateGreaterThan past", allowGet(fmt.Sprintf(`{"DateGreaterThan":{"aws:CurrentTime":%q}}`, now.Add(-time.Hour).Format(time.RFC3339))), []req{{"pub/a.txt", nil, 200}}},
		{"DateGreaterThan future", allowGet(fmt.Sprintf(`{"DateGreaterThan":{"aws:CurrentTime":%q}}`, now.Add(time.Hour).Format(time.RFC3339))), []req{{"pub/a.txt", nil, 403}}},
		{"DateLessThan future", allowGet(fmt.Sprintf(`{"DateLessThan":{"aws:CurrentTime":%q}}`, now.Add(time.Hour).Format(time.RFC3339))), []req{{"pub/a.txt", nil, 200}}},
		{"NumericLessThan EpochTime", allowGet(fmt.Sprintf(`{"NumericLessThan":{"aws:EpochTime":"%d"}}`, now.Add(-time.Hour).Unix())), []req{{"pub/a.txt", nil, 403}}},
		{"NumericGreaterThanEquals EpochTime", allowGet(fmt.Sprintf(`{"NumericGreaterThanEquals":{"aws:EpochTime":"%d"}}`, now.Add(-time.Hour).Unix())), []req{{"pub/a.txt", nil, 200}}},
		{"Null Referer absent", allowGet(`{"Null":{"aws:Referer":"true"}}`), []req{
			{"pub/a.txt", nil, 200}, {"pub/a.txt", map[string]string{"Referer": "https://x/"}, 403}}},
		{"StringEqualsIfExists", allowGet(`{"StringEqualsIfExists":{"aws:Referer":"https://ok/"}}`), []req{
			{"pub/a.txt", nil, 200}, {"pub/a.txt", map[string]string{"Referer": "https://ok/"}, 200}, {"pub/a.txt", map[string]string{"Referer": "https://ko/"}, 403}}},
		{"ForAnyValue header", allowGet(`{"ForAnyValue:StringEquals":{"s3:x-amz-meta-dummy":["a"]}}`), []req{{"pub/a.txt", nil, 403}}},
		{"variable username", policyJSON(stmt("Allow", "s3:GetObject", "arn:aws:s3:::conds/home/${aws:username}/*", "")), []req{
			{"home/condu/c.txt", nil, 200}, {"home/other/d.txt", nil, 403}}},
		{"variable in condition", allowGet(`{"StringEquals":{"aws:UserAgent":"agent-${aws:username}"}}`), []req{
			{"pub/a.txt", map[string]string{"User-Agent": "agent-condu"}, 200}, {"pub/a.txt", map[string]string{"User-Agent": "agent-other"}, 403}}},
		{"NotResource", policyJSON(`{"Effect":"Allow","Action":["s3:GetObject"],"NotResource":["arn:aws:s3:::conds/priv/*"]}`), []req{
			{"pub/a.txt", nil, 200}, {"priv/b.txt", nil, 403}}},
		{"NotAction", policyJSON(`{"Effect":"Allow","NotAction":["s3:PutObject","s3:DeleteObject"],"Resource":["arn:aws:s3:::conds/*"]}`), []req{
			{"pub/a.txt", nil, 200}}},
		{"explicit deny wins", policyJSON(stmt("Allow", "s3:*", "arn:aws:s3:::conds/*", ""), stmt("Deny", "s3:GetObject", "arn:aws:s3:::conds/priv/*", "")), []req{
			{"pub/a.txt", nil, 200}, {"priv/b.txt", nil, 403}}},
		{"deny with condition", policyJSON(stmt("Allow", "s3:GetObject", "arn:aws:s3:::conds/*", ""),
			stmt("Deny", "s3:GetObject", "arn:aws:s3:::conds/*", `{"StringLike":{"aws:UserAgent":"bot*"}}`)), []req{
			{"pub/a.txt", map[string]string{"User-Agent": "browser"}, 200}, {"pub/a.txt", map[string]string{"User-Agent": "bot/2"}, 403}}},
		{"wildcard action", policyJSON(stmt("Allow", "s3:Get*", "arn:aws:s3:::conds/pub/*", "")), []req{
			{"pub/a.txt", nil, 200}, {"priv/b.txt", nil, 403}}},
		{"question mark resource", policyJSON(stmt("Allow", "s3:GetObject", "arn:aws:s3:::conds/p?b/*", "")), []req{
			{"pub/a.txt", nil, 200}, {"priv/b.txt", nil, 403}}},
		{"other bucket only", policyJSON(stmt("Allow", "s3:GetObject", "arn:aws:s3:::elsewhere/*", "")), []req{{"pub/a.txt", nil, 403}}},
		{"authType header", allowGet(`{"StringEquals":{"s3:authType":"REST-HEADER"}}`), []req{{"pub/a.txt", nil, 200}}},
		{"authType query", allowGet(`{"StringEquals":{"s3:authType":"REST-QUERY-STRING"}}`), []req{{"pub/a.txt", nil, 403}}},
		{"signatureAge", allowGet(`{"NumericLessThan":{"s3:signatureAge":"600000"}}`), []req{{"pub/a.txt", nil, 200}}},
	}
	total := 0
	for i, tc := range cases {
		name := fmt.Sprintf("cond-%02d", i)
		e.setPolicy(name, tc.policy)
		require.NoError(t, e.srv.IAM.SetUserPolicy(e.ctx, u.AccessKey, []string{name}))
		for j, r := range tc.reqs {
			total++
			req := newRequest(t, http.MethodGet, e.srv.S3URL+"/"+bucket+"/"+r.key, nil)
			for k, v := range r.hdr {
				req.Header.Set(k, v)
			}
			e.signRequest(req, u, nil)
			resp := do(t, req)
			assert.Equal(t, r.status, resp.StatusCode, "%s #%d (%s): %s", tc.name, j, r.key, resp.Body)
		}
	}
	// Listing conditions: s3:prefix, s3:delimiter, s3:max-keys.
	e.setPolicy("cond-list", policyJSON(stmt("Allow", "s3:ListBucket", "arn:aws:s3:::conds",
		`{"StringLike":{"s3:prefix":"pub/*"},"NumericLessThanEquals":{"s3:max-keys":"100"},"StringEquals":{"s3:delimiter":"/"}}`)))
	require.NoError(t, e.srv.IAM.SetUserPolicy(e.ctx, u.AccessKey, []string{"cond-list"}))
	for _, lc := range []struct {
		query  string
		status int
	}{
		{"list-type=2&prefix=pub%2F&max-keys=10&delimiter=%2F", 200},
		{"list-type=2&prefix=pub%2F&max-keys=1000&delimiter=%2F", 403},
		{"list-type=2&prefix=priv%2F&max-keys=10&delimiter=%2F", 403},
		{"list-type=2&prefix=pub%2F&max-keys=10", 403},
		{"list-type=2&max-keys=10&delimiter=%2F", 403},
	} {
		total++
		resp := e.signedDo(http.MethodGet, "/"+bucket+"?"+lc.query, u, nil, nil)
		assert.Equal(t, lc.status, resp.StatusCode, "%s: %s", lc.query, resp.Body)
	}
	// The NotAction policy above does not grant writes.
	e.setPolicy("cond-notaction", policyJSON(`{"Effect":"Allow","NotAction":["s3:PutObject","s3:DeleteObject"],"Resource":["arn:aws:s3:::conds/*"]}`))
	require.NoError(t, e.srv.IAM.SetUserPolicy(e.ctx, u.AccessKey, []string{"cond-notaction"}))
	resp := e.signedDo(http.MethodPut, "/"+bucket+"/pub/new.txt", u, []byte("x"), nil)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	total++
	t.Logf("TPS-15: %d condition cases on real requests (plus %d matrix cases in TestPolicyMatrix)", total, 4*len(matrixOperations()))
}

// TS-I-10: a bucket public for reading: anonymous list and get succeed,
// anonymous put and delete are denied.
//
// covers: S-BKT-03
func TestPublicReadBucket(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	const bucket = "public"
	e.mkBucket(bucket)
	e.put(bucket, "file.txt", []byte("public content"))
	assert.Equal(t, http.StatusForbidden, e.anonStatus(http.MethodGet, "/"+bucket+"/file.txt", nil), "private by default")

	p, err := e.srv.Policies.GeneratePublicPolicy(e.ctx, bucket, "", policy.PresetDownload)
	require.NoError(t, err)
	e.putBucketPolicy(bucket, p)
	status, err := e.root.GetBucketPolicyStatus(e.ctx, &s3.GetBucketPolicyStatusInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	assert.True(t, aws.ToBool(status.PolicyStatus.IsPublic))

	resp := do(t, newRequest(t, http.MethodGet, e.srv.S3URL+"/"+bucket+"/file.txt", nil))
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "public content", string(resp.Body))
	assert.Equal(t, http.StatusOK, e.anonStatus(http.MethodHead, "/"+bucket+"/file.txt", nil))
	resp = do(t, newRequest(t, http.MethodGet, e.srv.S3URL+"/"+bucket+"?list-type=2", nil))
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	assert.Contains(t, string(resp.Body), "<Key>file.txt</Key>")
	assert.Equal(t, http.StatusOK, e.anonStatus(http.MethodGet, "/"+bucket, nil), "v1 listing")

	assert.Equal(t, http.StatusForbidden, e.anonStatus(http.MethodPut, "/"+bucket+"/anon.txt", []byte("x")))
	assert.Equal(t, http.StatusForbidden, e.anonStatus(http.MethodDelete, "/"+bucket+"/file.txt", nil))
	assert.Equal(t, http.StatusForbidden, e.anonStatus(http.MethodGet, "/"+bucket+"?policy", nil))
	assert.Equal(t, http.StatusForbidden, e.anonStatus(http.MethodGet, "/", nil), "ListBuckets is never anonymous")
	assert.Equal(t, http.StatusForbidden, e.anonStatus(http.MethodDelete, "/"+bucket, nil))
	assert.Equal(t, []byte("public content"), e.get(bucket, "file.txt"))

	// Upload preset: anonymous writes without reads.
	p, err = e.srv.Policies.GeneratePublicPolicy(e.ctx, bucket, "", policy.PresetUpload)
	require.NoError(t, err)
	e.putBucketPolicy(bucket, p)
	assert.Equal(t, http.StatusOK, e.anonStatus(http.MethodPut, "/"+bucket+"/dropped.txt", []byte("dropped")))
	assert.Equal(t, http.StatusForbidden, e.anonStatus(http.MethodGet, "/"+bucket+"/dropped.txt", nil))
	assert.Equal(t, []byte("dropped"), e.get(bucket, "dropped.txt"))

	// Back to private.
	_, err = e.root.DeleteBucketPolicy(e.ctx, &s3.DeleteBucketPolicyInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	assert.Equal(t, http.StatusForbidden, e.anonStatus(http.MethodPut, "/"+bucket+"/again.txt", []byte("x")))
}

// TPS-16: anonymous download on the "pub/" prefix only.
//
// covers: S-BKT-03
func TestAnonymousPrefixAccess(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	const bucket = "prefixed"
	e.mkBucket(bucket)
	for _, k := range []string{"pub/a.txt", "pub/sub/b.txt", "private/c.txt", "pubx/d.txt", "top.txt"} {
		e.put(bucket, k, []byte(k))
	}
	p, err := e.srv.Policies.ApplyPublicPreset(e.ctx, nil, bucket, "pub/", policy.PresetDownload)
	require.NoError(t, err)
	e.putBucketPolicy(bucket, p)
	raw, err := e.root.GetBucketPolicy(e.ctx, &s3.GetBucketPolicyInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)
	stored, err := e.srv.Policies.Parse(e.ctx, []byte(aws.ToString(raw.Policy)))
	require.NoError(t, err)
	assert.Equal(t, policy.PresetDownload, e.srv.Policies.DetectPreset(e.ctx, stored, bucket, "pub/"))
	assert.Equal(t, policy.PresetPrivate, e.srv.Policies.DetectPreset(e.ctx, stored, bucket, "private/"))

	for path, want := range map[string]int{
		"/" + bucket + "/pub/a.txt":                       200,
		"/" + bucket + "/pub/sub/b.txt":                   200,
		"/" + bucket + "/private/c.txt":                   403,
		"/" + bucket + "/pubx/d.txt":                      403,
		"/" + bucket + "/top.txt":                         403,
		"/" + bucket + "?list-type=2&prefix=pub%2F":       200,
		"/" + bucket + "?list-type=2&prefix=pub%2Fsub%2F": 200,
		"/" + bucket + "?list-type=2&prefix=private%2F":   403,
		"/" + bucket + "?list-type=2":                     403,
		"/" + bucket + "?list-type=2&prefix=pubx%2F":      403,
	} {
		assert.Equal(t, want, e.anonStatus(http.MethodGet, path, nil), path)
	}
	resp := do(t, newRequest(t, http.MethodGet, e.srv.S3URL+"/"+bucket+"?list-type=2&prefix=pub%2F", nil))
	assert.Contains(t, string(resp.Body), "<Key>pub/a.txt</Key>")
	assert.NotContains(t, string(resp.Body), "private")
	assert.Equal(t, http.StatusForbidden, e.anonStatus(http.MethodPut, "/"+bucket+"/pub/new.txt", []byte("x")), "download does not allow uploads")
	assert.Equal(t, http.StatusForbidden, e.anonStatus(http.MethodDelete, "/"+bucket+"/pub/a.txt", nil))

	// Public preset on another prefix keeps the first rule.
	p, err = e.srv.Policies.ApplyPublicPreset(e.ctx, stored, bucket, "inbox/", policy.PresetUpload)
	require.NoError(t, err)
	e.putBucketPolicy(bucket, p)
	assert.Equal(t, http.StatusOK, e.anonStatus(http.MethodPut, "/"+bucket+"/inbox/new.txt", []byte("x")))
	assert.Equal(t, http.StatusForbidden, e.anonStatus(http.MethodPut, "/"+bucket+"/pub/new.txt", []byte("x")))
	assert.Equal(t, http.StatusOK, e.anonStatus(http.MethodGet, "/"+bucket+"/pub/a.txt", nil))
	rules := e.srv.Policies.ListPublicAccess(e.ctx, p, bucket)
	assert.Equal(t, []policy.PublicAccessRule{{Prefix: "inbox/", Preset: policy.PresetUpload}, {Prefix: "pub/", Preset: policy.PresetDownload}}, rules)
}

// TS-I-11: a service account with an inline policy narrower than its
// parent gets the intersection.
//
// covers: S-IAM-06
func TestServiceAccountNarrowPolicy(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	const bucket = "svc"
	e.mkBucket(bucket)
	e.put(bucket, "reports/q1.txt", []byte("q1"))
	e.put(bucket, "secret/s.txt", []byte("s"))
	parent := e.createUser("svc-parent", "svc-parent-secret", "readwrite")
	inline := e.parsePolicy(policyJSON(`{"Effect":"Allow","Action":["s3:GetObject","s3:ListBucket"],"Resource":["arn:aws:s3:::svc","arn:aws:s3:::svc/reports/*"]}`))
	key, err := e.srv.IAM.CreateAccessKey(e.ctx, iam.CreateAccessKeyInput{Caller: e.identity(parent.AccessKey), Name: "reports", Policy: inline})
	require.NoError(t, err)
	sa := e.client(key.AccessKey, key.SecretKey, "")
	ctx := e.ctx

	assert.Equal(t, []byte("q1"), getWith(t, ctx, sa, bucket, "reports/q1.txt"))
	_, err = sa.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(bucket)})
	assert.NoError(t, err)
	_, err = sa.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("secret/s.txt")})
	assert.Equal(t, "AccessDenied", apiCode(err), "outside the inline resources")
	_, err = sa.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String("reports/q2.txt"), Body: strings.NewReader("x")})
	assert.Equal(t, "AccessDenied", apiCode(err), "the parent may write, the key may not")
	_, err = sa.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String("reports/q1.txt")})
	assert.Equal(t, "AccessDenied", apiCode(err))
	// The parent keeps its own permissions.
	_, err = e.clientFor(parent).PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String("reports/q2.txt"), Body: strings.NewReader("x")})
	require.NoError(t, err)

	// Disabling the key, or the parent, disables the service account.
	disabled := domain.StatusDisabled
	_, err = e.srv.IAM.UpdateAccessKey(ctx, e.identity(parent.AccessKey), key.AccessKey, iam.UpdateAccessKeyInput{Status: &disabled})
	require.NoError(t, err)
	_, err = sa.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("reports/q1.txt")})
	assert.Equal(t, http.StatusForbidden, httpStatus(err))
	enabled := domain.StatusEnabled
	_, err = e.srv.IAM.UpdateAccessKey(ctx, e.identity(parent.AccessKey), key.AccessKey, iam.UpdateAccessKeyInput{Status: &enabled})
	require.NoError(t, err)
	require.NoError(t, e.srv.IAM.SetUserStatus(ctx, parent.AccessKey, domain.StatusDisabled))
	_, err = sa.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String("reports/q1.txt")})
	assert.Equal(t, http.StatusForbidden, httpStatus(err))
}

// TPS-17: a service account whose inline policy is broader than its
// parent's is still limited by the parent.
//
// covers: S-IAM-06
func TestServiceAccountBroadPolicy(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	const bucket = "broad"
	e.mkBucket(bucket)
	e.put(bucket, "doc.txt", []byte("doc"))
	parent := e.createUser("broad-parent", "broad-parent-secret", "readonly")
	inline := e.parsePolicy(policyJSON(`{"Effect":"Allow","Action":["s3:*"],"Resource":["arn:aws:s3:::*"]}`))
	key, err := e.srv.IAM.CreateAccessKey(e.ctx, iam.CreateAccessKeyInput{Caller: e.identity(parent.AccessKey), Policy: inline})
	require.NoError(t, err)
	sa := e.client(key.AccessKey, key.SecretKey, "")
	ctx := e.ctx

	assert.Equal(t, []byte("doc"), getWith(t, ctx, sa, bucket, "doc.txt"), "allowed by both")
	_, err = sa.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String("new.txt"), Body: strings.NewReader("x")})
	assert.Equal(t, "AccessDenied", apiCode(err), "the parent is readonly")
	_, err = sa.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String("doc.txt")})
	assert.Equal(t, "AccessDenied", apiCode(err))
	_, err = sa.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("broad-new")})
	assert.Equal(t, "AccessDenied", apiCode(err))
	_, err = sa.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{Bucket: aws.String(bucket), Policy: aws.String(policyJSON(
		`{"Effect":"Allow","Principal":"*","Action":["s3:GetObject"],"Resource":["arn:aws:s3:::broad/*"]}`))})
	assert.Equal(t, "AccessDenied", apiCode(err))

	// A key without inline policy inherits the parent; widening the parent
	// widens the key.
	plain, err := e.srv.IAM.CreateAccessKey(ctx, iam.CreateAccessKeyInput{Caller: e.identity(parent.AccessKey)})
	require.NoError(t, err)
	require.NoError(t, e.srv.IAM.SetUserPolicy(ctx, parent.AccessKey, []string{"readwrite"}))
	_, err = e.client(plain.AccessKey, plain.SecretKey, "").PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String("new.txt"), Body: strings.NewReader("x")})
	assert.NoError(t, err)
	_, err = sa.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String("new2.txt"), Body: strings.NewReader("x")})
	assert.NoError(t, err, "the intersection now allows writes")
}

// TPS-14: a managed share link works until it is revoked; then it answers
// 403 at once.
//
// covers: S-OBJ-08c
func TestShareLinkRevocation(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	const bucket = "shares"
	e.mkBucket(bucket)
	e.put(bucket, "report.pdf", []byte("quarterly report"))
	e.put(bucket, "album/1.jpg", []byte("one"))
	e.put(bucket, "album/2.jpg", []byte("two"))
	owner := e.createUser("sharer", "sharer-secret", "readwrite")
	ownerID := e.identity(owner.AccessKey)
	ctx := e.ctx

	sh, err := e.srv.Shares.Create(ctx, share.CreateInput{Owner: ownerID, Bucket: bucket, Key: "report.pdf", Permission: share.PermissionDownload,
		Expires: time.Hour, ContentDisposition: `attachment; filename="report.pdf"`})
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(sh.URL, e.srv.S3URL+"/"), "the link points to the S3 API: %s", sh.URL)
	assert.True(t, strings.HasPrefix(sh.AccessKey, "share-"))
	resp := presignedDo(t, http.MethodGet, sh.URL, nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	assert.Equal(t, "quarterly report", string(resp.Body))
	assert.Equal(t, `attachment; filename="report.pdf"`, resp.Header.Get("Content-Disposition"))

	// The share's key cannot be used for anything else.
	u, err := url.Parse(sh.URL)
	require.NoError(t, err)
	u.Path = "/" + bucket + "/album/1.jpg"
	resp = presignedDo(t, http.MethodGet, u.String(), nil, nil)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)

	// A prefix share yields one URL per object, revoked together.
	album, err := e.srv.Shares.Create(ctx, share.CreateInput{Owner: ownerID, Bucket: bucket, Key: "album/", Prefix: true, Permission: share.PermissionDownload, Expires: time.Hour})
	require.NoError(t, err)
	photo, err := e.srv.Shares.Presign(ctx, ownerID, album.ID, "album/2.jpg")
	require.NoError(t, err)
	resp = presignedDo(t, http.MethodGet, photo.String(), nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	assert.Equal(t, "two", string(resp.Body))
	resp = presignedDo(t, http.MethodGet, album.URL, nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "the prefix listing: %s", resp.Body)
	assert.Contains(t, string(resp.Body), "album/1.jpg")

	// An upload share.
	up, err := e.srv.Shares.Create(ctx, share.CreateInput{Owner: ownerID, Bucket: bucket, Key: "inbox/upload.bin", Permission: share.PermissionUpload, Expires: time.Hour})
	require.NoError(t, err)
	resp = presignedDo(t, http.MethodPut, up.URL, []byte("uploaded via share"), nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	assert.Equal(t, []byte("uploaded via share"), e.get(bucket, "inbox/upload.bin"))

	list, err := e.srv.Shares.List(ctx, ownerID, share.ListFilter{})
	require.NoError(t, err)
	assert.Len(t, list, 3)

	// Revoke: 403 at once (within 1 s).
	require.NoError(t, e.srv.Shares.Revoke(ctx, ownerID, sh.ID))
	start := time.Now()
	resp = presignedDo(t, http.MethodGet, sh.URL, nil, nil)
	assert.Less(t, time.Since(start), time.Second)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "%s", resp.Body)
	resp = presignedDo(t, http.MethodGet, photo.String(), nil, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "other shares are untouched")

	n, err := e.srv.Shares.RevokeAll(ctx, ownerID, "")
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	for _, raw := range []string{photo.String(), album.URL} {
		assert.Equal(t, http.StatusForbidden, presignedDo(t, http.MethodGet, raw, nil, nil).StatusCode)
	}
	assert.Equal(t, http.StatusForbidden, presignedDo(t, http.MethodPut, up.URL, []byte("again"), nil).StatusCode)
	list, err = e.srv.Shares.List(ctx, ownerID, share.ListFilter{})
	require.NoError(t, err)
	assert.Empty(t, list)

	// Disabling the owner disables its links too.
	again, err := e.srv.Shares.Create(ctx, share.CreateInput{Owner: ownerID, Bucket: bucket, Key: "report.pdf", Permission: share.PermissionDownload, Expires: time.Hour})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, presignedDo(t, http.MethodGet, again.URL, nil, nil).StatusCode)
	require.NoError(t, e.srv.IAM.SetUserStatus(ctx, owner.AccessKey, domain.StatusDisabled))
	assert.Equal(t, http.StatusForbidden, presignedDo(t, http.MethodGet, again.URL, nil, nil).StatusCode)

	// A readonly user cannot share an upload.
	reader := e.createUser("share-reader", "share-reader-secret", "readonly")
	_, err = e.srv.Shares.Create(ctx, share.CreateInput{Owner: e.identity(reader.AccessKey), Bucket: bucket, Key: "x", Permission: share.PermissionUpload, Expires: time.Hour})
	assert.ErrorIs(t, err, domain.ErrAccessDenied)
}

// The bucket policy document round-trips through S3 as JSON.
//
// covers: S-BKT-03
func TestBucketPolicyRoundTrip(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.mkBucket("doc-policy")
	doc := policyJSON(`{"Sid":"Read","Effect":"Allow","Principal":{"AWS":["*"]},"Action":["s3:GetObject"],"Resource":["arn:aws:s3:::doc-policy/*"]}`)
	e.putBucketPolicy("doc-policy", doc)
	out, err := e.root.GetBucketPolicy(e.ctx, &s3.GetBucketPolicyInput{Bucket: aws.String("doc-policy")})
	require.NoError(t, err)
	var got, want any
	require.NoError(t, json.Unmarshal([]byte(aws.ToString(out.Policy)), &got))
	require.NoError(t, json.Unmarshal([]byte(doc), &want))
	assert.Equal(t, want, got)
	_, err = e.root.DeleteBucketPolicy(e.ctx, &s3.DeleteBucketPolicyInput{Bucket: aws.String("doc-policy")})
	require.NoError(t, err)
	_, err = e.root.GetBucketPolicy(e.ctx, &s3.GetBucketPolicyInput{Bucket: aws.String("doc-policy")})
	assert.Equal(t, "NoSuchBucketPolicy", apiCode(err))
	// Invalid documents are refused with MalformedPolicy.
	for _, bad := range []string{`{`, `{"Version":"2012-10-17","Statement":[]}`,
		policyJSON(`{"Effect":"Allow","Action":["s3:GetObject"],"Resource":["arn:aws:s3:::doc-policy/*"]}`), // no principal
		policyJSON(`{"Effect":"Maybe","Principal":"*","Action":["s3:GetObject"],"Resource":["arn:aws:s3:::doc-policy/*"]}`),
	} {
		_, err = e.root.PutBucketPolicy(e.ctx, &s3.PutBucketPolicyInput{Bucket: aws.String("doc-policy"), Policy: aws.String(bad)})
		assert.Equal(t, "MalformedPolicy", apiCode(err), bad)
	}
}
