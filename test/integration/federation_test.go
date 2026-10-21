package integration_test

import (
	"bytes"
	"encoding/xml"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/service/iam"
	"github.com/r3n3r0/nexo/internal/testutil"
)

// TestAssumeRoleWithWebIdentity (S-IAM-08): an ID token of the OpenID
// provider is traded for temporary credentials (AWS SDK, unsigned call)
// whose policies come from the policy claim and from the Nexo groups named
// by the groups claim.
//
// covers: S-IAM-08
func TestAssumeRoleWithWebIdentity(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := e.ctx
	p := testutil.NewFakeOIDC(t)
	_, err := adminClient(t, e).SetConfig(ctx, "identity_openid", map[string]string{
		"enable": "on", "config_url": p.ConfigURL(), "client_id": p.ClientID, "client_secret": p.ClientSecret,
	})
	require.NoError(t, err)
	e.mkBucket("reports")
	e.mkBucket("private")
	e.setPolicy("reports-read", policyJSON(`{"Effect":"Allow","Action":["s3:GetObject","s3:ListBucket"],"Resource":["arn:aws:s3:::reports","arn:aws:s3:::reports/*"]}`))
	_, err = e.srv.IAM.CreateGroup(ctx, iam.CreateGroupInput{Name: "analysts", PolicyNames: []string{"reports-read"}})
	require.NoError(t, err)
	_, err = e.root.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("reports"), Key: aws.String("q3.csv"), Body: strings.NewReader("a,b\n")})
	require.NoError(t, err)

	client := sts.New(sts.Options{Region: "us-east-1", BaseEndpoint: aws.String(e.srv.S3URL)})
	out, err := client.AssumeRoleWithWebIdentity(ctx, &sts.AssumeRoleWithWebIdentityInput{
		RoleArn: aws.String("arn:aws:iam::123456789012:role/any"), RoleSessionName: aws.String("ci"), DurationSeconds: aws.Int32(900),
		WebIdentityToken: aws.String(p.Token(map[string]any{"sub": "00u7", "preferred_username": "ann", "groups": []string{"analysts"}})),
	})
	require.NoError(t, err)
	assert.Equal(t, "00u7", aws.ToString(out.SubjectFromWebIdentityToken))
	c := out.Credentials
	fed := e.client(aws.ToString(c.AccessKeyId), aws.ToString(c.SecretAccessKey), aws.ToString(c.SessionToken))
	got, err := fed.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("reports"), Key: aws.String("q3.csv")})
	require.NoError(t, err)
	assert.Equal(t, "a,b\n", string(readAll(t, got)))
	_, err = fed.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("private")})
	assert.Equal(t, "AccessDenied", apiCode(err), "only the policies of the groups")
	_, err = fed.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("reports"), Key: aws.String("x"), Body: strings.NewReader("x")})
	assert.Equal(t, "AccessDenied", apiCode(err))

	// A forged token, and a user mapping to no policy, are refused.
	other := testutil.NewFakeOIDC(t)
	_, err = client.AssumeRoleWithWebIdentity(ctx, &sts.AssumeRoleWithWebIdentityInput{
		RoleArn: aws.String("arn:x"), RoleSessionName: aws.String("xx"),
		WebIdentityToken: aws.String(other.Token(map[string]any{"sub": "x", "iss": p.URL, "groups": []string{"analysts"}})),
	})
	assert.Equal(t, "InvalidIdentityToken", apiCode(err))
	_, err = client.AssumeRoleWithWebIdentity(ctx, &sts.AssumeRoleWithWebIdentityInput{
		RoleArn: aws.String("arn:x"), RoleSessionName: aws.String("xx"), WebIdentityToken: aws.String(p.Token(map[string]any{"sub": "nobody"})),
	})
	assert.Equal(t, "AccessDenied", apiCode(err))
}

// TestAssumeRoleWithLDAPIdentity (S-IAM-09): the password of a user of the
// directory is traded for temporary credentials, with the policies of the
// Nexo groups named like its directory groups.
//
// covers: S-IAM-09
func TestAssumeRoleWithLDAPIdentity(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := e.ctx
	d := testutil.NewFakeLDAP(t)
	d.AddUser("carol", "carol-pw", "writers")
	_, err := adminClient(t, e).SetConfig(ctx, "identity_ldap", map[string]string{
		"enable": "on", "server_addr": d.Addr, "tls": "off", "lookup_bind_dn": testutil.LDAPBindDN, "lookup_bind_password": testutil.LDAPBindPass,
		"user_dn_search_base_dn": testutil.LDAPUserBase, "group_search_base_dn": testutil.LDAPGroupBase,
	})
	require.NoError(t, err)
	e.mkBucket("drop")
	_, err = e.srv.IAM.CreateGroup(ctx, iam.CreateGroupInput{Name: "writers", PolicyNames: []string{"readwrite"}})
	require.NoError(t, err)

	assume := func(user, password string) response {
		form := url.Values{"Action": {"AssumeRoleWithLDAPIdentity"}, "Version": {"2011-06-15"}, "LDAPUsername": {user}, "LDAPPassword": {password}}
		req := newRequest(t, http.MethodPost, e.srv.S3URL+"/", []byte(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		return do(t, req)
	}
	resp := assume("carol", "carol-pw")
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	var out struct {
		Result struct {
			Credentials struct {
				AccessKeyID     string `xml:"AccessKeyId"`
				SecretAccessKey string `xml:"SecretAccessKey"`
				SessionToken    string `xml:"SessionToken"`
			} `xml:"Credentials"`
		} `xml:"AssumeRoleWithLDAPIdentityResult"`
	}
	require.NoError(t, xml.Unmarshal(resp.Body, &out))
	c := out.Result.Credentials
	fed := e.client(c.AccessKeyID, c.SecretAccessKey, c.SessionToken)
	_, err = fed.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("drop"), Key: aws.String("from-ldap"), Body: bytes.NewReader([]byte("hi"))})
	require.NoError(t, err)

	resp = assume("carol", "wrong")
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "%s", resp.Body)
	assert.Contains(t, string(resp.Body), "<Code>AccessDenied</Code>")

	// The password is refused in the query string.
	req := newRequest(t, http.MethodPost, e.srv.S3URL+"/?LDAPPassword=carol-pw", []byte("Action=AssumeRoleWithLDAPIdentity&Version=2011-06-15&LDAPUsername=carol"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp = do(t, req)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}
