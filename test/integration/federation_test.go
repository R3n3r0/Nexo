package integration_test

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"net/http"
	"net/http/cookiejar"
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

// TestConsoleFederatedLogin (D-AUTH-02): the console login page offers the
// enabled providers; the OpenID Connect login runs the authorization code
// flow (state cookie, PKCE, nonce) and the LDAP login goes through POST
// /auth/login, both opening a session of the federated user.
//
// covers: D-AUTH-02, S-IAM-08, S-IAM-09
func TestConsoleFederatedLogin(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	ctx := e.ctx
	p := testutil.NewFakeOIDC(t)
	p.SetUser(map[string]any{"sub": "00u9", "preferred_username": "ann", "groups": []string{"analysts"}})
	d := testutil.NewFakeLDAP(t)
	d.AddUser("carol", "carol-pw", "analysts")
	admin := adminClient(t, e)
	_, err := admin.SetConfig(ctx, "identity_openid", map[string]string{
		"enable": "on", "config_url": p.ConfigURL(), "client_id": p.ClientID, "client_secret": p.ClientSecret, "display_name": "Corporate SSO",
	})
	require.NoError(t, err)
	_, err = admin.SetConfig(ctx, "identity_ldap", map[string]string{
		"enable": "on", "server_addr": d.Addr, "tls": "off", "lookup_bind_dn": testutil.LDAPBindDN, "lookup_bind_password": testutil.LDAPBindPass,
		"user_dn_search_base_dn": testutil.LDAPUserBase, "group_search_base_dn": testutil.LDAPGroupBase,
	})
	require.NoError(t, err)
	_, err = e.srv.IAM.CreateGroup(ctx, iam.CreateGroupInput{Name: "analysts", PolicyNames: []string{"readonly"}})
	require.NoError(t, err)

	api := e.srv.ConsoleURL + "/api/v1"
	browser := func() *http.Client {
		jar, err := cookiejar.New(nil)
		require.NoError(t, err)
		return &http.Client{Jar: jar, CheckRedirect: noRedirect}
	}
	get := func(c *http.Client, target string) response {
		t.Helper()
		return doWith(t, c, newRequest(t, http.MethodGet, target, nil))
	}

	resp := get(browser(), api+"/auth/providers")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.JSONEq(t, `{"openid":true,"openidName":"Corporate SSO","ldap":true}`, string(resp.Body))

	// OpenID Connect: start, provider, callback.
	c := browser()
	resp = get(c, api+"/auth/openid/start")
	require.Equal(t, http.StatusFound, resp.StatusCode, "%s", resp.Body)
	authorize := resp.Header.Get("Location")
	require.True(t, strings.HasPrefix(authorize, p.URL+"/authorize?"), authorize)
	resp = get(c, authorize)
	require.Equal(t, http.StatusFound, resp.StatusCode, "%s", resp.Body)
	callback := resp.Header.Get("Location")
	require.True(t, strings.HasPrefix(callback, api+"/auth/openid/callback?"), callback)

	// Another browser (without the state cookie) cannot use the code.
	resp = get(browser(), callback)
	require.Equal(t, http.StatusFound, resp.StatusCode)
	assert.Equal(t, "/login?error=sso_failed", resp.Header.Get("Location"))

	resp = get(c, callback)
	require.Equal(t, http.StatusFound, resp.StatusCode, "%s", resp.Body)
	assert.Equal(t, "/", resp.Header.Get("Location"))
	resp = get(c, api+"/auth/session")
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	var sess struct {
		Identity struct {
			Provider    string   `json:"provider"`
			Username    string   `json:"username"`
			PolicyNames []string `json:"policyNames"`
		} `json:"identity"`
	}
	require.NoError(t, json.Unmarshal(resp.Body, &sess))
	assert.Equal(t, "openid", sess.Identity.Provider)
	assert.Equal(t, "ann", sess.Identity.Username)
	assert.Equal(t, []string{"readonly"}, sess.Identity.PolicyNames)
	resp = get(c, api+"/buckets")
	assert.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)

	// The callback is single use: replaying it fails.
	resp = get(c, callback)
	assert.Equal(t, "/login?error=sso_failed", resp.Header.Get("Location"))

	// LDAP through the login endpoint.
	login := func(body string) response {
		req := newRequest(t, http.MethodPost, api+"/auth/login", []byte(body))
		req.Header.Set("Content-Type", "application/json")
		return do(t, req)
	}
	resp = login(`{"provider":"ldap","username":"carol","password":"carol-pw"}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	assert.Contains(t, string(resp.Body), `"username":"carol"`)
	resp = login(`{"provider":"ldap","username":"carol","password":"wrong"}`)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "%s", resp.Body)
	resp = login(`{"provider":"kerberos","username":"carol","password":"carol-pw"}`)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "%s", resp.Body)
}
