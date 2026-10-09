package integration_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/auth"
	"github.com/r3n3r0/nexo/internal/testutil"
)

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// postPolicy builds and signs a POST policy by hand (independently of the
// server's signer): the signature is the SigV4 HMAC of the base64 policy.
type postPolicy struct {
	creds      creds
	region     string
	bucket     string
	expiration time.Time
	// conditions are added to the mandatory ones (bucket, algorithm,
	// credential, date).
	conditions []any
	signedAt   time.Time
}

// fields returns the signed form fields (without key and file).
func (p postPolicy) fields(t *testing.T) map[string]string {
	t.Helper()
	at := p.signedAt
	if at.IsZero() {
		at = time.Now().UTC()
	}
	scopeDate := at.Format("20060102")
	amzDate := at.Format("20060102T150405Z")
	credential := fmt.Sprintf("%s/%s/%s/s3/aws4_request", p.creds.AccessKey, scopeDate, p.region)
	conds := []any{
		map[string]string{"bucket": p.bucket},
		map[string]string{"x-amz-algorithm": "AWS4-HMAC-SHA256"},
		map[string]string{"x-amz-credential": credential},
		map[string]string{"x-amz-date": amzDate},
	}
	if p.creds.Token != "" {
		conds = append(conds, map[string]string{"x-amz-security-token": p.creds.Token})
	}
	conds = append(conds, p.conditions...)
	doc, err := json.Marshal(map[string]any{"expiration": p.expiration.UTC().Format("2006-01-02T15:04:05.000Z"), "conditions": conds})
	require.NoError(t, err)
	b64 := base64.StdEncoding.EncodeToString(doc)
	key := hmacSHA256([]byte("AWS4"+p.creds.SecretKey), scopeDate)
	key = hmacSHA256(key, p.region)
	key = hmacSHA256(key, "s3")
	key = hmacSHA256(key, "aws4_request")
	out := map[string]string{
		"policy":           b64,
		"x-amz-algorithm":  "AWS4-HMAC-SHA256",
		"x-amz-credential": credential,
		"x-amz-date":       amzDate,
		"x-amz-signature":  hex.EncodeToString(hmacSHA256(key, b64)),
	}
	if p.creds.Token != "" {
		out["x-amz-security-token"] = p.creds.Token
	}
	return out
}

// postForm sends a browser upload: the fields (sorted, for determinism)
// then the file, last.
func (e *env) postForm(bucket string, fields map[string]string, fileName string, content []byte) response {
	e.t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	names := make([]string, 0, len(fields))
	for k := range fields {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		require.NoError(e.t, mw.WriteField(k, fields[k]))
	}
	fw, err := mw.CreateFormFile("file", fileName)
	require.NoError(e.t, err)
	_, err = fw.Write(content)
	require.NoError(e.t, err)
	require.NoError(e.t, mw.Close())
	req := newRequest(e.t, http.MethodPost, e.srv.S3URL+"/"+bucket, body.Bytes())
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return do(e.t, req)
}

// postTruncatedForm sends a browser upload cut short: the file part has
// no closing boundary, as when the connection of the browser drops (the
// request itself ends cleanly, so only the form is incomplete).
func (e *env) postTruncatedForm(bucket string, fields map[string]string, content []byte) response {
	e.t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	names := make([]string, 0, len(fields))
	for k := range fields {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		require.NoError(e.t, mw.WriteField(k, fields[k]))
	}
	fw, err := mw.CreateFormFile("file", "cut.bin")
	require.NoError(e.t, err)
	_, err = fw.Write(content)
	require.NoError(e.t, err)
	req := newRequest(e.t, http.MethodPost, e.srv.S3URL+"/"+bucket, body.Bytes())
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return do(e.t, req)
}

// TestPostPolicyTruncatedForm (anti-truncation): a POST policy upload
// whose form ends without its closing boundary (a body of unknown size
// cut short) is refused with 400 IncompleteBody in every mode, encrypted
// or not: the truncated content is never stored (regression: it was
// committed with SSE, and in the erasure-coded modes without it).
func TestPostPolicyTruncatedForm(t *testing.T) {
	t.Parallel()
	e := newEnv(t, testutil.WithKMS())
	const bucket = "cut-forms"
	e.mkBucket(bucket)
	content := bytes.Repeat([]byte("truncated-form-"), 200<<10/15)
	for _, sse := range []string{"", "AES256"} {
		key := "plain"
		conds := []any{[]any{"starts-with", "$key", ""}}
		fields := map[string]string{}
		if sse != "" {
			key = "encrypted"
			conds = append(conds, map[string]string{"x-amz-server-side-encryption": sse})
			fields["x-amz-server-side-encryption"] = sse
		}
		pol := postPolicy{creds: e.rootCreds(), region: e.srv.Config.Region, bucket: bucket, expiration: time.Now().Add(time.Hour), conditions: conds}
		fields["key"] = key
		resp := e.postTruncatedForm(bucket, merge(pol.fields(t), fields), content)
		requireS3Error(t, resp, http.StatusBadRequest, "IncompleteBody")
		_, err := e.root.HeadObject(e.ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
		assert.Equal(t, http.StatusNotFound, httpStatus(err), "%s: nothing is stored", key)

		// The same form, complete, is stored.
		ok := e.postForm(bucket, merge(pol.fields(t), fields), "ok.bin", content)
		require.Equal(t, http.StatusNoContent, ok.StatusCode, "%s: %s", key, ok.Body)
		head, err := e.root.HeadObject(e.ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
		require.NoError(t, err)
		assert.Equal(t, int64(len(content)), aws.ToInt64(head.ContentLength))
	}
}

func merge(maps ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// TPS-12 / TS-I-08: POST policy uploads from an HTML form: conditions met
// (204/201) or violated (403/400): size, key prefix, content type,
// expiration, signature, extra fields, signer permissions.
//
// covers: S-OBJ-08b
func TestPostPolicy(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	const bucket = "forms"
	e.mkBucket(bucket)
	region := e.srv.Config.Region
	standard := []any{
		[]any{"starts-with", "$key", "uploads/"},
		[]any{"content-length-range", 1, 1024},
		[]any{"starts-with", "$Content-Type", "text/"},
		map[string]string{"success_action_status": "201"},
		[]any{"starts-with", "$x-amz-meta-owner", ""},
	}
	policyFor := func(c creds, exp time.Duration, conds ...any) postPolicy {
		return postPolicy{creds: c, region: region, bucket: bucket, expiration: time.Now().Add(exp), conditions: conds}
	}
	base := policyFor(e.rootCreds(), time.Hour, standard...)
	baseFields := base.fields(t)
	okFields := map[string]string{"key": "uploads/a.txt", "Content-Type": "text/plain", "success_action_status": "201", "x-amz-meta-owner": "form"}

	t.Run("accepted 201", func(t *testing.T) {
		resp := e.postForm(bucket, merge(baseFields, okFields), "a.txt", []byte("hello form"))
		require.Equal(t, http.StatusCreated, resp.StatusCode, "%s", resp.Body)
		var doc struct {
			XMLName  xml.Name `xml:"PostResponse"`
			Location string   `xml:"Location"`
			Bucket   string   `xml:"Bucket"`
			Key      string   `xml:"Key"`
			ETag     string   `xml:"ETag"`
		}
		require.NoError(t, xml.Unmarshal(resp.Body, &doc))
		assert.Equal(t, bucket, doc.Bucket)
		assert.Equal(t, "uploads/a.txt", doc.Key)
		assert.Equal(t, `"`+md5Hex([]byte("hello form"))+`"`, doc.ETag)
		head, err := e.root.HeadObject(e.ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String("uploads/a.txt")})
		require.NoError(t, err)
		assert.Equal(t, "text/plain", aws.ToString(head.ContentType))
		assert.Equal(t, "form", head.Metadata["owner"])
		assert.Equal(t, []byte("hello form"), e.get(bucket, "uploads/a.txt"))
	})

	t.Run("accepted 204 with filename", func(t *testing.T) {
		p := policyFor(e.rootCreds(), time.Hour, []any{"starts-with", "$key", "uploads/"}, []any{"content-length-range", 0, 100})
		resp := e.postForm(bucket, merge(p.fields(t), map[string]string{"key": "uploads/${filename}"}), "photo.txt", []byte("pixels"))
		require.Equal(t, http.StatusNoContent, resp.StatusCode, "%s", resp.Body)
		assert.Equal(t, []byte("pixels"), e.get(bucket, "uploads/photo.txt"))
		assert.NotEmpty(t, resp.Header.Get("ETag"))
	})

	rejected := []struct {
		name    string
		fields  map[string]string
		content []byte
		status  int
		code    string
	}{
		{"too large", merge(baseFields, okFields, map[string]string{"key": "uploads/large.txt"}), bytes.Repeat([]byte("x"), 1025), 400, "EntityTooLarge"},
		{"too small", merge(baseFields, okFields, map[string]string{"key": "uploads/empty.txt"}), nil, 400, "EntityTooSmall"},
		{"key outside the prefix", merge(baseFields, okFields, map[string]string{"key": "private/a.txt"}), []byte("x"), 403, "AccessDenied"},
		{"content type", merge(baseFields, okFields, map[string]string{"key": "uploads/b.json", "Content-Type": "application/json"}), []byte("{}"), 403, "AccessDenied"},
		{"status not in policy", merge(baseFields, okFields, map[string]string{"key": "uploads/c.txt", "success_action_status": "200"}), []byte("x"), 403, "AccessDenied"},
		{"extra field", merge(baseFields, okFields, map[string]string{"key": "uploads/d.txt", "x-amz-meta-extra": "1"}), []byte("x"), 403, "AccessDenied"},
		{"signature altered", merge(baseFields, okFields, map[string]string{"key": "uploads/e.txt", "x-amz-signature": strings.Repeat("0", 64)}), []byte("x"), 403, "SignatureDoesNotMatch"},
		{"policy altered", merge(baseFields, okFields, map[string]string{"key": "uploads/f.txt",
			"policy": base64.StdEncoding.EncodeToString([]byte(`{"expiration":"2099-01-01T00:00:00.000Z","conditions":[]}`))}), []byte("x"), 403, "SignatureDoesNotMatch"},
		{"expired", merge(policyFor(e.rootCreds(), -time.Minute, standard...).fields(t), okFields, map[string]string{"key": "uploads/g.txt"}), []byte("x"), 403, "AccessDenied"},
		{"other bucket", merge(postPolicy{creds: e.rootCreds(), region: region, bucket: "elsewhere", expiration: time.Now().Add(time.Hour), conditions: standard}.fields(t),
			okFields, map[string]string{"key": "uploads/h.txt"}), []byte("x"), 403, "AccessDenied"},
		{"unknown key", merge(policyFor(creds{AccessKey: "ghost", SecretKey: "ghost-secret"}, time.Hour, standard...).fields(t), okFields,
			map[string]string{"key": "uploads/i.txt"}), []byte("x"), 403, "InvalidAccessKeyId"},
		{"missing key", policyFor(e.rootCreds(), time.Hour).fields(t), []byte("x"), 400, "InvalidArgument"},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			resp := e.postForm(bucket, tc.fields, "file.txt", tc.content)
			requireS3Error(t, resp, tc.status, tc.code)
			if k := tc.fields["key"]; k != "" {
				_, err := e.root.HeadObject(e.ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(k)})
				assert.Equal(t, http.StatusNotFound, httpStatus(err), "nothing stored for %s", k)
			}
		})
	}

	t.Run("signer without write permission", func(t *testing.T) {
		reader := e.createUser("form-reader", "form-reader-secret", "readonly")
		p := policyFor(reader, time.Hour, standard...)
		resp := e.postForm(bucket, merge(p.fields(t), okFields, map[string]string{"key": "uploads/reader.txt"}), "r.txt", []byte("x"))
		requireS3Error(t, resp, http.StatusForbidden, "AccessDenied")
		writer := e.createUser("form-writer", "form-writer-secret", "writeonly")
		p = policyFor(writer, time.Hour, standard...)
		resp = e.postForm(bucket, merge(p.fields(t), okFields, map[string]string{"key": "uploads/writer.txt"}), "w.txt", []byte("x"))
		require.Equal(t, http.StatusCreated, resp.StatusCode, "%s", resp.Body)
	})

	t.Run("server signer form", func(t *testing.T) {
		form, err := e.srv.Signer.PresignPostPolicy(e.ctx, auth.PostPolicyInput{
			Bucket: bucket, KeyPrefix: "signed/", Expires: time.Hour,
			Creds:      domain.Credentials{AccessKey: e.srv.RootUser, SecretKey: e.srv.RootPassword},
			Conditions: []auth.PostCondition{{Operator: "content-length-range", Key: "content-length-range", Min: 1, Max: 64}},
		})
		require.NoError(t, err)
		resp := e.postForm(bucket, form.Fields, "doc.txt", []byte("signed by the server"))
		require.Equal(t, http.StatusNoContent, resp.StatusCode, "%s", resp.Body)
		assert.Equal(t, []byte("signed by the server"), e.get(bucket, "signed/doc.txt"))
		resp = e.postForm(bucket, form.Fields, "big.txt", bytes.Repeat([]byte("y"), 65))
		requireS3Error(t, resp, http.StatusBadRequest, "EntityTooLarge")
	})
}

// TPS-13: success_action_redirect answers 303 with bucket, key and etag in
// the query of the redirect.
//
// covers: S-OBJ-08b
func TestPostPolicyRedirect(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	const bucket = "redirects"
	e.mkBucket(bucket)
	const target = "https://app.example.com/done?session=42"
	p := postPolicy{creds: e.rootCreds(), region: e.srv.Config.Region, bucket: bucket, expiration: time.Now().Add(time.Hour),
		conditions: []any{map[string]string{"key": "in/report.pdf"}, map[string]string{"success_action_redirect": target}}}
	resp := e.postForm(bucket, merge(p.fields(t), map[string]string{"key": "in/report.pdf", "success_action_redirect": target}), "report.pdf", []byte("%PDF-1.7"))
	require.Equal(t, http.StatusSeeOther, resp.StatusCode, "%s", resp.Body)
	loc, err := url.Parse(resp.Header.Get("Location"))
	require.NoError(t, err)
	assert.Equal(t, "app.example.com", loc.Host)
	assert.Equal(t, "/done", loc.Path)
	q := loc.Query()
	assert.Equal(t, "42", q.Get("session"), "the original query is kept")
	assert.Equal(t, bucket, q.Get("bucket"))
	assert.Equal(t, "in/report.pdf", q.Get("key"))
	assert.Equal(t, `"`+md5Hex([]byte("%PDF-1.7"))+`"`, q.Get("etag"))
	assert.Equal(t, []byte("%PDF-1.7"), e.get(bucket, "in/report.pdf"))

	// A redirect not allowed by the policy is refused.
	resp = e.postForm(bucket, merge(p.fields(t), map[string]string{"key": "in/report.pdf", "success_action_redirect": "https://evil.example.com/"}), "report.pdf", []byte("x"))
	requireS3Error(t, resp, http.StatusForbidden, "AccessDenied")
}
