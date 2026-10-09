package integration_test

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	proxyproto "github.com/pires/go-proxyproto"
	"github.com/pires/go-proxyproto/tlvparse"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/config"
	"github.com/r3n3r0/nexo/internal/service/accessrule"
	"github.com/r3n3r0/nexo/internal/service/iam"
	"github.com/r3n3r0/nexo/internal/service/match"
	"github.com/r3n3r0/nexo/internal/testutil"
)

// propagationBound is the time a change of the access configuration
// takes to reach every node (TAR-09).
const propagationBound = time.Second

// putRules saves access rules on a bucket as root (through the service
// of the first node).
func (e *env) putRules(bucket string, rules ...accessrule.Rule) *accessrule.Config {
	e.t.Helper()
	cfg, err := e.srv.AccessRules.Put(e.ctx, accessrule.PutInput{Bucket: bucket, Rules: rules, Actor: e.srv.RootUser})
	require.NoError(e.t, err)
	return cfg
}

func arRule(id string, priority int, effect accessrule.Effect, principals, actions []string, m *match.Pattern, c *accessrule.Conditions) accessrule.Rule {
	return accessrule.Rule{ID: id, Priority: priority, Effect: effect, Principals: principals, Actions: actions, Match: m, Conditions: c}
}

func arList(v ...string) []string { return v }

// rawStatus sends an unsigned request with the given raw path (as is: no
// cleaning, no escaping) and returns its status.
func (e *env) rawStatus(base, rawPath string, hdr http.Header) int {
	e.t.Helper()
	req := newRequest(e.t, http.MethodGet, base+"/", nil)
	req.URL.Opaque = rawPath
	for k, vs := range hdr {
		req.Header[k] = vs
	}
	return do(e.t, req).StatusCode
}

// TestAccessRulesS3 covers TAR-05, TAR-06 and TAR-03 end to end through the
// S3 API: anonymous requests and aws-sdk-go-v2 clients; a deny rule
// prevails over IAM, the bucket policy and the allow rules, an allow rule
// grants without IAM policy, the home directories of the users, no bypass
// through the encodings of the key, root bypasses the rules.
func TestAccessRulesS3(t *testing.T) {
	e := newEnv(t)
	e.mkBucket("media")
	for _, key := range []string{"img/2026/a.jpg", "img/2026/private/b.jpg", "docs/report.pdf", "home/mario/x.txt", "home/luigi/y.txt", "città/roma.txt"} {
		e.put("media", key, []byte("content of "+key))
	}
	e.putRules("media",
		arRule("immagini-pubbliche", 10, accessrule.EffectAllow, arList("*"), arList("read"),
			&match.Pattern{Type: match.TypeRegex, Pattern: `^img/[0-9]{4}/.*\.(jpe?g|png|webp)$`, CaseInsensitive: true}, nil),
		arRule("blocca-privati", 5, accessrule.EffectDeny, arList("*"), arList("all"), &match.Pattern{Type: match.TypeGlob, Pattern: "**/private/**"}, nil),
		arRule("blocca-citta", 6, accessrule.EffectDeny, arList("*"), arList("all"), &match.Pattern{Type: match.TypePrefix, Pattern: "città/"}, nil),
		arRule("home", 20, accessrule.EffectAllow, arList("*"), arList("all"), &match.Pattern{Type: match.TypeRegex, Pattern: `^home/(?P<user>[^/]+)/`},
			&accessrule.Conditions{CaptureEquals: map[string]string{"user": "${aws:username}"}}),
	)

	// Anonymous requests.
	assert.Equal(t, http.StatusOK, e.anonStatus(http.MethodGet, "/media/img/2026/a.jpg", nil))
	assert.Equal(t, http.StatusOK, e.anonStatus(http.MethodHead, "/media/img/2026/a.jpg", nil))
	assert.Equal(t, http.StatusForbidden, e.anonStatus(http.MethodGet, "/media/img/2026/private/b.jpg", nil), "deny prevails")
	assert.Equal(t, http.StatusForbidden, e.anonStatus(http.MethodGet, "/media/docs/report.pdf", nil), "deny by default")
	assert.Equal(t, http.StatusForbidden, e.anonStatus(http.MethodPut, "/media/img/2026/new.jpg", []byte("x")), "read only")
	assert.Equal(t, http.StatusForbidden, e.anonStatus(http.MethodGet, "/media/home/mario/x.txt", nil), "no username, no home")
	assert.Equal(t, http.StatusForbidden, e.anonStatus(http.MethodGet, "/media?list-type=2", nil), "no list rule")

	// TAR-03: the encodings of the key never reach the protected object.
	for _, path := range []string{
		"/media/img/2026/private%2Fb.jpg",
		"/media/img%2F2026%2Fprivate%2Fb.jpg",
		"/media/img/2026/./private/b.jpg",
		"/media/img/2026//private/b.jpg",
		"/media/img/x/../2026/private/b.jpg",
		"/media/img/2026/%2e/private/b.jpg",
		"/media/img/2026/PRIVATE/../private/b.jpg",
	} {
		status := e.rawStatus(e.srv.S3URL, path, nil)
		assert.Contains(t, []int{http.StatusForbidden, http.StatusBadRequest}, status, "%s answered %d", path, status)
	}
	// Double encoding names another object (a literal "%2F"): never the
	// protected one, never its content.
	resp := do(t, func() *http.Request {
		req := newRequest(t, http.MethodGet, e.srv.S3URL+"/", nil)
		req.URL.Opaque = "/media/img/2026/private%252Fb.jpg"
		return req
	}())
	assert.NotEqual(t, http.StatusOK, resp.StatusCode)
	assert.NotContains(t, string(resp.Body), "content of")

	// IAM users through aws-sdk-go-v2.
	writer := e.clientFor(e.createUser("writer", "writer-secret-1", "readwrite"))
	_, err := writer.GetObject(e.ctx, &s3.GetObjectInput{Bucket: aws.String("media"), Key: aws.String("img/2026/private/b.jpg")})
	assert.Equal(t, "AccessDenied", apiCode(err), "a deny rule prevails over the IAM allow")
	assert.Equal(t, "content of docs/report.pdf", string(getWith(t, e.ctx, writer, "media", "docs/report.pdf")))
	_, err = writer.GetObject(e.ctx, &s3.GetObjectInput{Bucket: aws.String("media"), Key: aws.String("città/roma.txt")})
	assert.Equal(t, "AccessDenied", apiCode(err), "the NFD spelling of a protected key")
	_, err = writer.PutObject(e.ctx, &s3.PutObjectInput{Bucket: aws.String("media"), Key: aws.String("a/private/new.txt"), Body: strings.NewReader("x")})
	assert.Equal(t, "AccessDenied", apiCode(err))

	// TAR-06: the home directories, without any IAM policy.
	mario := e.clientFor(e.createUser("mario", "mario-secret-1"))
	assert.Equal(t, "content of home/mario/x.txt", string(getWith(t, e.ctx, mario, "media", "home/mario/x.txt")))
	_, err = mario.PutObject(e.ctx, &s3.PutObjectInput{Bucket: aws.String("media"), Key: aws.String("home/mario/new.txt"), Body: strings.NewReader("mine")})
	require.NoError(t, err)
	_, err = mario.GetObject(e.ctx, &s3.GetObjectInput{Bucket: aws.String("media"), Key: aws.String("home/luigi/y.txt")})
	assert.Equal(t, "AccessDenied", apiCode(err))
	_, err = mario.DeleteObject(e.ctx, &s3.DeleteObjectInput{Bucket: aws.String("media"), Key: aws.String("home/luigi/y.txt")})
	assert.Equal(t, "AccessDenied", apiCode(err))
	luigi := e.clientFor(e.createUser("luigi", "luigi-secret-1"))
	assert.Equal(t, "content of home/luigi/y.txt", string(getWith(t, e.ctx, luigi, "media", "home/luigi/y.txt")))
	_, err = luigi.GetObject(e.ctx, &s3.GetObjectInput{Bucket: aws.String("media"), Key: aws.String("home/mario/new.txt")})
	assert.Equal(t, "AccessDenied", apiCode(err))
	_, err = luigi.GetObject(e.ctx, &s3.GetObjectInput{Bucket: aws.String("media"), Key: aws.String("docs/report.pdf")})
	assert.Equal(t, "AccessDenied", apiCode(err), "deny by default")

	// Root bypasses the rules, like the bucket policies.
	assert.Equal(t, "content of img/2026/private/b.jpg", string(e.get("media", "img/2026/private/b.jpg")))

	// A deny rule prevails over a public bucket policy too.
	e.putBucketPolicy("media", policyJSON(`{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::media/*"}`))
	assert.Equal(t, http.StatusOK, e.anonStatus(http.MethodGet, "/media/docs/report.pdf", nil))
	assert.Equal(t, http.StatusForbidden, e.anonStatus(http.MethodGet, "/media/img/2026/private/b.jpg", nil))

	// The tester agrees with the requests.
	res, err := e.srv.AccessRules.Test(e.ctx, accessrule.TestInput{Bucket: "media", Keys: []string{"img/2026/a.jpg", "img/2026/private/b.jpg"}})
	require.NoError(t, err)
	assert.True(t, res.Results[0].Allowed)
	assert.False(t, res.Results[1].Allowed)
	assert.Equal(t, "blocca-privati", res.Results[1].Determinant)
}

// TestAccessRulesBehindProxies covers TAR-07 and TDS-08 end to end: the
// conditions on the client address, the host, the transport and the
// headers read the client behind the trusted proxies (X-Forwarded-*, a Go
// reverse proxy, the PROXY protocol v1/v2 on the S3 listener); a peer that
// is not a trusted proxy cannot forge them.
func TestAccessRulesBehindProxies(t *testing.T) {
	e := newEnv(t, testutil.WithConfig(func(cfg *config.Static) {
		cfg.TrustedProxies = []string{"127.0.0.1", "::1"}
		cfg.ProxyProtocol = []string{config.ListenerS3}
		cfg.Secrets = map[string]string{"proxy-token": "s3cr3t"}
	}))
	e.mkBucket("edge")
	for _, key := range []string{"lan/a", "cdn/a", "tls/a", "proxied/a", "window/a"} {
		e.put("edge", key, []byte(key))
	}
	e.putRules("edge",
		arRule("lan", 10, accessrule.EffectAllow, arList("*"), arList("read"), &match.Pattern{Type: match.TypePrefix, Pattern: "lan/"},
			&accessrule.Conditions{SourceIP: arList("10.0.0.0/8", "2001:db8::/32")}),
		arRule("cdn", 11, accessrule.EffectAllow, arList("*"), arList("read"), &match.Pattern{Type: match.TypePrefix, Pattern: "cdn/"},
			&accessrule.Conditions{Host: arList("cdn.example.com")}),
		arRule("tls", 12, accessrule.EffectAllow, arList("*"), arList("read"), &match.Pattern{Type: match.TypePrefix, Pattern: "tls/"},
			&accessrule.Conditions{SecureTransport: aws.Bool(true)}),
		arRule("proxied", 13, accessrule.EffectAllow, arList("*"), arList("read"), &match.Pattern{Type: match.TypePrefix, Pattern: "proxied/"},
			&accessrule.Conditions{Headers: map[string]string{"X-Origin-Secret": "${secret:proxy-token}"}, SourceIP: arList("127.0.0.3/32")}),
		arRule("window", 14, accessrule.EffectAllow, arList("*"), arList("read"), &match.Pattern{Type: match.TypePrefix, Pattern: "window/"},
			&accessrule.Conditions{TimeWindow: &accessrule.TimeWindow{From: "2000-01-01T00:00:00Z", To: "2001-01-01T00:00:00Z"}}),
	)
	node := e.srv.Nodes[0].S3URL
	nodeAddr := strings.TrimPrefix(node, "http://")
	fwd := func(h ...string) http.Header {
		out := http.Header{}
		for i := 0; i+1 < len(h); i += 2 {
			out.Add(h[i], h[i+1])
		}
		return out
	}

	// Through a trusted proxy (the deployment, or the balancer, sees
	// 127.0.0.1): the forwarded client decides.
	assert.Equal(t, http.StatusOK, e.rawStatus(e.srv.S3URL, "/edge/lan/a", fwd("X-Forwarded-For", "10.1.2.3")))
	assert.Equal(t, http.StatusOK, e.rawStatus(e.srv.S3URL, "/edge/lan/a", fwd("X-Forwarded-For", "2001:db8::9")))
	assert.Equal(t, http.StatusForbidden, e.rawStatus(e.srv.S3URL, "/edge/lan/a", fwd("X-Forwarded-For", "11.1.2.3")))
	assert.Equal(t, http.StatusForbidden, e.rawStatus(e.srv.S3URL, "/edge/lan/a", nil), "the proxy itself")
	assert.Equal(t, http.StatusOK, e.rawStatus(e.srv.S3URL, "/edge/cdn/a", fwd("X-Forwarded-Host", "cdn.example.com")))
	assert.Equal(t, http.StatusForbidden, e.rawStatus(e.srv.S3URL, "/edge/cdn/a", fwd("X-Forwarded-Host", "www.example.com")))
	assert.Equal(t, http.StatusOK, e.rawStatus(e.srv.S3URL, "/edge/tls/a", fwd("X-Forwarded-Proto", "https")))
	assert.Equal(t, http.StatusForbidden, e.rawStatus(e.srv.S3URL, "/edge/tls/a", fwd("X-Forwarded-Proto", "http")))
	assert.Equal(t, http.StatusForbidden, e.rawStatus(e.srv.S3URL, "/edge/window/a", nil), "outside the time window")

	// The Host the client addressed.
	req := newRequest(t, http.MethodGet, e.srv.S3URL+"/edge/cdn/a", nil)
	req.Host = "cdn.example.com"
	assert.Equal(t, http.StatusOK, do(t, req).StatusCode)

	// A peer that is not a trusted proxy (127.0.0.2, straight to a node)
	// cannot forge the client, the host or the transport.
	untrusted := clientFrom("127.0.0.2")
	status := func(c *http.Client, base, path string, hdr http.Header) int {
		req := newRequest(t, http.MethodGet, base+path, nil)
		for k, vs := range hdr {
			req.Header[k] = vs
		}
		return doWith(t, c, req).StatusCode
	}
	assert.Equal(t, http.StatusForbidden, status(untrusted, node, "/edge/lan/a", fwd("X-Forwarded-For", "10.1.2.3")))
	assert.Equal(t, http.StatusForbidden, status(untrusted, node, "/edge/lan/a", fwd("Forwarded", "for=10.1.2.3")))
	assert.Equal(t, http.StatusForbidden, status(untrusted, node, "/edge/cdn/a", fwd("X-Forwarded-Host", "cdn.example.com")))
	assert.Equal(t, http.StatusForbidden, status(untrusted, node, "/edge/tls/a", fwd("X-Forwarded-Proto", "https")))

	// A Go reverse proxy in front of a node: it appends the client and
	// adds the origin secret.
	target, err := url.Parse(node)
	require.NoError(t, err)
	proxy := &httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) {
		r.SetURL(target)
		r.SetXForwarded()
		r.Out.Header.Set("X-Origin-Secret", "s3cr3t")
	}}
	front := serveProxy(t, proxy)
	assert.Equal(t, http.StatusOK, status(clientFrom("127.0.0.3"), front, "/edge/proxied/a", nil), "secret and client of the proxy")
	assert.Equal(t, http.StatusForbidden, status(clientFrom("127.0.0.4"), front, "/edge/proxied/a", fwd("X-Forwarded-For", "127.0.0.3")),
		"a client forging its address behind the proxy")
	assert.Equal(t, http.StatusForbidden, status(clientFrom("127.0.0.3"), node, "/edge/proxied/a", nil), "straight to the node, no secret")
	assert.Equal(t, http.StatusForbidden, status(clientFrom("127.0.0.3"), node, "/edge/proxied/a", fwd("X-Origin-Secret", "guess")))

	// PROXY protocol on the S3 listener of the node: v1 and v2 from the
	// trusted proxy; the header of another peer is ignored.
	assert.Equal(t, http.StatusOK, proxiedGet(t, "127.0.0.1", nodeAddr, ppV1("10.9.9.9"), "/edge/lan/a"))
	assert.Equal(t, http.StatusForbidden, proxiedGet(t, "127.0.0.1", nodeAddr, ppV1("203.0.113.9"), "/edge/lan/a"))
	assert.Equal(t, http.StatusOK, proxiedGet(t, "127.0.0.1", nodeAddr, ppV2(t, "10.9.9.9", false), "/edge/lan/a"))
	assert.Equal(t, http.StatusOK, proxiedGet(t, "127.0.0.1", nodeAddr, ppV2(t, "198.51.100.1", true), "/edge/tls/a"), "TLS of the proxy")
	assert.Equal(t, http.StatusForbidden, proxiedGet(t, "127.0.0.2", nodeAddr, ppV1("10.9.9.9"), "/edge/lan/a"), "spoofed header ignored")
	assert.Equal(t, http.StatusForbidden, proxiedGet(t, "127.0.0.2", nodeAddr, ppV2(t, "198.51.100.1", true), "/edge/tls/a"))
	assert.Equal(t, http.StatusForbidden, proxiedGet(t, "127.0.0.1", nodeAddr, nil, "/edge/lan/a"), "a trusted proxy without header is the client")
}

// clientFrom returns an HTTP client whose connections leave from the
// given loopback address.
func clientFrom(ip string) *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second, LocalAddr: &net.TCPAddr{IP: net.ParseIP(ip)}}
	return &http.Client{Timeout: 30 * time.Second, CheckRedirect: noRedirect,
		Transport: &http.Transport{DialContext: dialer.DialContext, DisableKeepAlives: true}}
}

// serveProxy serves a reverse proxy on a free port of 127.0.0.1 and
// returns its base URL.
func serveProxy(t *testing.T, h http.Handler) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + ln.Addr().String()
}

// ppV1 is a PROXY protocol v1 header from src.
func ppV1(src string) []byte {
	return []byte(fmt.Sprintf("PROXY TCP4 %s 127.0.0.1 40000 9000\r\n", src))
}

// ppV2 is a PROXY protocol v2 header from src, with the SSL information of
// a client connection over TLS when tls is set.
func ppV2(t *testing.T, src string, tls bool) []byte {
	t.Helper()
	h := &proxyproto.Header{Version: 2, Command: proxyproto.PROXY, TransportProtocol: proxyproto.TCPv4,
		SourceAddr: &net.TCPAddr{IP: net.ParseIP(src), Port: 40000}, DestinationAddr: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 9000}}
	if tls {
		tlv, err := tlvparse.PP2SSL{Client: tlvparse.PP2_BITFIELD_CLIENT_SSL,
			TLV: []proxyproto.TLV{{Type: proxyproto.PP2_SUBTYPE_SSL_VERSION, Value: []byte("TLSv1.3")}}}.Marshal()
		require.NoError(t, err)
		require.NoError(t, h.SetTLVs([]proxyproto.TLV{tlv}))
	}
	data, err := h.Format()
	require.NoError(t, err)
	return data
}

// proxiedGet sends, from the local address from, a PROXY header (when
// any) and an anonymous GET to addr; it returns the status.
func proxiedGet(t *testing.T, from, addr string, header []byte, path string) int {
	t.Helper()
	dialer := &net.Dialer{Timeout: 5 * time.Second, LocalAddr: &net.TCPAddr{IP: net.ParseIP(from)}}
	conn, err := dialer.Dial("tcp", addr)
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.SetDeadline(time.Now().Add(30*time.Second)))
	if len(header) > 0 {
		_, err = conn.Write(header)
		require.NoError(t, err)
	}
	req := newRequest(t, http.MethodGet, "http://"+addr+path, nil)
	require.NoError(t, req.Write(conn))
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	return resp.StatusCode
}

// TestAccessRulesConsole checks that the console decides with the access
// rules as the S3 API does (the same policy engine): an allow rule grants a
// user without IAM policy, a deny rule prevails over its IAM policy.
func TestAccessRulesConsole(t *testing.T) {
	e := newEnv(t)
	e.mkBucket("shared")
	e.put("shared", "team/plan.txt", []byte("plan"))
	e.put("shared", "team/private/salaries.txt", []byte("salaries"))
	e.put("shared", "other/x.txt", []byte("x"))
	e.putRules("shared",
		arRule("team-read", 10, accessrule.EffectAllow, arList("group:team"), arList("read", "list"), &match.Pattern{Type: match.TypePrefix, Pattern: "team/"}, nil),
		arRule("no-private", 1, accessrule.EffectDeny, arList("*"), arList("all"), &match.Pattern{Type: match.TypeGlob, Pattern: "**/private/**"}, nil),
		arRule("no-delete", 2, accessrule.EffectDeny, arList("*"), arList("delete"), nil, &accessrule.Conditions{Methods: arList("DELETE")}),
	)
	anna := e.createUser("anna", "anna-secret-1")
	_, err := e.srv.IAM.CreateGroup(e.ctx, iamGroup("team", "anna"))
	require.NoError(t, err)
	boss := e.createUser("boss", "boss-secret-1", "readwrite")

	c := e.consoleLogin(anna)
	resp := c.do(http.MethodGet, "/buckets/shared/objects/download?key=team/plan.txt", "")
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	assert.Equal(t, "plan", string(resp.Body))
	assert.Equal(t, http.StatusForbidden, c.do(http.MethodGet, "/buckets/shared/objects/download?key=team/private/salaries.txt", "").StatusCode)
	assert.Equal(t, http.StatusForbidden, c.do(http.MethodGet, "/buckets/shared/objects/download?key=other/x.txt", "").StatusCode)
	_, err = e.clientFor(anna).GetObject(e.ctx, &s3.GetObjectInput{Bucket: aws.String("shared"), Key: aws.String("team/plan.txt")})
	require.NoError(t, err, "the same decision through S3")

	b := e.consoleLogin(boss)
	assert.Equal(t, http.StatusForbidden, b.do(http.MethodGet, "/buckets/shared/objects/download?key=team/private/salaries.txt", "").StatusCode,
		"the deny rule prevails over IAM in the console too")
	resp = b.do(http.MethodGet, "/buckets/shared/objects/download?key=other/x.txt", "")
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	// The console has no S3 method: the one the action stands for
	// (DELETE) decides, as through S3.
	resp = b.do(http.MethodDelete, "/buckets/shared/objects", `{"objects":[{"key":"other/x.txt"}]}`)
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", resp.Body)
	assert.Contains(t, string(resp.Body), "access_denied", "%s", resp.Body)
	_, err = e.clientFor(boss).DeleteObject(e.ctx, &s3.DeleteObjectInput{Bucket: aws.String("shared"), Key: aws.String("other/x.txt")})
	assert.Equal(t, "AccessDenied", apiCode(err))
	assert.Equal(t, "x", string(e.get("shared", "other/x.txt")), "nothing deleted")
}

// iamGroup is the creation of a group with members.
func iamGroup(name string, members ...string) iam.CreateGroupInput {
	return iam.CreateGroupInput{Name: name, Members: members}
}

// TestAccessRulesPublicAndBlocked covers the presets (public-read,
// public-prefix) and TAR-08: the block public access at the bucket and at
// the cluster level, in the reject and the ignore modes.
func TestAccessRulesPublicAndBlocked(t *testing.T) {
	e := newEnv(t)
	e.mkBucket("site")
	e.put("site", "pub/index.html", []byte("hello"))
	e.put("site", "priv/x", []byte("x"))
	ar := e.srv.AccessRules

	_, err := ar.ApplyPreset(e.ctx, accessrule.PresetInput{Bucket: "site", Preset: accessrule.PresetPublicPrefix,
		Match: &match.Pattern{Type: match.TypeGlob, Pattern: "pub/**"}})
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, e.anonStatus(http.MethodGet, "/site/pub/index.html", nil))
	assert.Equal(t, http.StatusForbidden, e.anonStatus(http.MethodGet, "/site/priv/x", nil))
	status, err := ar.PublicStatus(e.ctx, "site")
	require.NoError(t, err)
	assert.Equal(t, accessrule.PresetPublicPrefix, status.Preset)
	assert.True(t, status.Public)

	// Bucket level: ignored at once.
	_, err = ar.SetBlockPublic(e.ctx, accessrule.BlockPublicInput{Bucket: "site", Enabled: true})
	require.NoError(t, err)
	assert.Equal(t, http.StatusForbidden, e.anonStatus(http.MethodGet, "/site/pub/index.html", nil))
	_, err = ar.ApplyPreset(e.ctx, accessrule.PresetInput{Bucket: "site", Preset: accessrule.PresetPublicRead})
	assert.ErrorIs(t, err, accessrule.ErrPublicAccessBlocked, "reject mode (default)")
	_, err = ar.SetBlockPublic(e.ctx, accessrule.BlockPublicInput{Bucket: "site", Enabled: false})
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, e.anonStatus(http.MethodGet, "/site/pub/index.html", nil))

	// Cluster level in the ignore mode: saved, ignored.
	_, err = ar.SetBlockPublic(e.ctx, accessrule.BlockPublicInput{Enabled: true, Mode: accessrule.BlockModeIgnore})
	require.NoError(t, err)
	assert.Equal(t, http.StatusForbidden, e.anonStatus(http.MethodGet, "/site/pub/index.html", nil))
	cfg, err := ar.ApplyPreset(e.ctx, accessrule.PresetInput{Bucket: "site", Preset: accessrule.PresetPublicRead})
	require.NoError(t, err)
	assert.False(t, cfg.Public)
	assert.NotEmpty(t, cfg.Warnings)
	assert.Equal(t, http.StatusForbidden, e.anonStatus(http.MethodGet, "/site/priv/x", nil))
	res, err := ar.Test(e.ctx, accessrule.TestInput{Bucket: "site", Keys: []string{"priv/x"}})
	require.NoError(t, err)
	assert.True(t, res.BlockPublic.Effective)
	assert.Equal(t, []string{"preset-public-read"}, res.Results[0].BlockedPublic)
	_, err = ar.SetBlockPublic(e.ctx, accessrule.BlockPublicInput{Enabled: false})
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, e.anonStatus(http.MethodGet, "/site/priv/x", nil))
}

// TestAccessRulesPropagation is TAR-09: a change of the rules or of the
// block public access saved on a node is in effect on every node within a
// second, without restart (the bucket metadata and the configuration are
// invalidated by the cluster notifications).
func TestAccessRulesPropagation(t *testing.T) {
	e := newEnv(t)
	e.mkBucket("prop")
	e.put("prop", "a.txt", []byte("a"))
	nodes := e.srv.Nodes
	first, last := nodes[0], nodes[len(nodes)-1]
	waitStatus := func(n *testutil.Node, want int) time.Duration {
		t.Helper()
		start := time.Now()
		for {
			got := do(t, newRequest(t, http.MethodGet, n.S3URL+"/prop/a.txt", nil)).StatusCode
			if got == want {
				return time.Since(start)
			}
			if time.Since(start) > propagationBound {
				t.Fatalf("node %s answered %d instead of %d after %s", n.S3URL, got, want, propagationBound)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	for _, n := range nodes {
		waitStatus(n, http.StatusForbidden)
	}
	_, err := first.App.AccessRules().Put(e.ctx, accessrule.PutInput{Bucket: "prop",
		Rules: []accessrule.Rule{arRule("pub", 1, accessrule.EffectAllow, arList("*"), arList("read"), nil, nil)}})
	require.NoError(t, err)
	for _, n := range nodes {
		t.Logf("rules on %s after %s", n.S3URL, waitStatus(n, http.StatusOK))
	}
	_, err = last.App.AccessRules().SetBlockPublic(e.ctx, accessrule.BlockPublicInput{Enabled: true})
	require.NoError(t, err)
	for _, n := range nodes {
		t.Logf("block public on %s after %s", n.S3URL, waitStatus(n, http.StatusForbidden))
	}
	_, err = first.App.AccessRules().SetBlockPublic(e.ctx, accessrule.BlockPublicInput{Enabled: false})
	require.NoError(t, err)
	for _, n := range nodes {
		waitStatus(n, http.StatusOK)
	}
	_, err = last.App.AccessRules().Put(e.ctx, accessrule.PutInput{Bucket: "prop", Rules: nil})
	require.NoError(t, err)
	for _, n := range nodes {
		waitStatus(n, http.StatusForbidden)
	}
}
