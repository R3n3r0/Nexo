//go:build proxy

package proxy_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	smithyendpoints "github.com/aws/smithy-go/endpoints"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/config"
	"github.com/r3n3r0/nexo/internal/service/cache"
	"github.com/r3n3r0/nexo/internal/testutil"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// Addresses of the test: the two edges are the trusted proxies of the
// deployment; the viewers connect from addresses of their own so that the
// tests can tell which client Nexo saw.
const (
	edgeAIP = "127.0.0.11"
	edgeBIP = "127.0.0.12"
	// viewerIP is an ordinary client; deniedIP is refused by the source
	// address restriction of the distributions; otherIP is outside the
	// address range of the signed URLs.
	viewerIP = "127.0.0.61"
	deniedIP = "127.0.0.66"
	otherIP  = "127.0.0.62"
)

// Domains of the edges.
const (
	domainA = ".alpha.test"
	domainB = ".beta.test"
	// mainHost is the public S3 domain (NEXO_SERVER_URL, behind edge A),
	// consoleHost the console behind edge A.
	mainHost    = "s3.alpha.test"
	consoleHost = "console.alpha.test"
)

// Origin secrets of the edges, given to the nodes as NEXO_SECRET_PROXY_A
// and NEXO_SECRET_PROXY_B: the distributions reference them as
// ${secret:proxy-a} and ${secret:proxy-b}.
const (
	secretHeader = "X-Origin-Secret"
	secretA      = "edge-a-0b1d7e5c9f3a"
	secretB      = "edge-b-8c2e4a6f0d91"
)

// deployment is a Nexo deployment behind the two edges.
type deployment struct {
	t      *testing.T
	ctx    context.Context
	mode   string
	srv    *testutil.Server
	pki    *pki
	edgeA  *edge
	edgeB  *edge
	admin  nexoadmin.AdminClient
	root   *s3.Client
	region string
	// signingKey is the private key of the key group of the signed
	// downloads, returned once by the Admin API.
	signingKey string
}

// modes returns the deployment modes to run (NEXO_PROXY_MODES, default
// every mode).
func modes() []string {
	all := []string{"snsd", "snmd", "cluster"}
	v := strings.TrimSpace(os.Getenv("NEXO_PROXY_MODES"))
	if v == "" || v == "all" {
		return all
	}
	var out []string
	for _, m := range strings.Split(v, ",") {
		if m = strings.TrimSpace(m); slices.Contains(all, m) {
			out = append(out, m)
		}
	}
	return out
}

// requireLoopbackAliases skips the test where the loopback addresses of
// the edges and the viewers cannot be bound (everything but Linux).
func requireLoopbackAliases(t *testing.T) {
	t.Helper()
	for _, ip := range []string{edgeAIP, edgeBIP, viewerIP, deniedIP, otherIP} {
		ln, err := net.Listen("tcp", net.JoinHostPort(ip, "0"))
		if err != nil {
			t.Skipf("the loopback address %s cannot be bound: %v", ip, err)
		}
		_ = ln.Close()
	}
}

// startDeployment starts the deployment of mode behind the edges: edge A
// (X-Forwarded-*) serves *.alpha.test with the S3 API on s3.alpha.test and
// the console on console.alpha.test, edge B (PROXY protocol v2) serves
// *.beta.test. The nodes trust both edges, accept the PROXY protocol on
// the S3 listener and know the secrets of both.
func startDeployment(t *testing.T, mode string) *deployment {
	t.Helper()
	d := &deployment{t: t, ctx: context.Background(), mode: mode, pki: newPKI(t)}
	var srvPtr atomic.Pointer[testutil.Server]
	listeners := func(console bool) func() []string {
		return func() []string {
			srv := srvPtr.Load()
			if srv == nil {
				return nil
			}
			out := make([]string, 0, len(srv.Nodes))
			for _, n := range srv.Nodes {
				if console {
					out = append(out, n.Config.ConsoleAddress)
				} else {
					out = append(out, n.Config.Address)
				}
			}
			return out
		}
	}
	d.edgeA = newEdge(t, edgeConfig{
		name: "A", ip: edgeAIP, domain: domainA, mode: forwardHeaders, secretHeader: secretHeader, secret: secretA,
		cert: d.pki.leaf(t, "alpha.test", "*.alpha.test"),
		s3:   listeners(false), console: listeners(true), consoleHost: consoleHost,
	})
	d.edgeB = newEdge(t, edgeConfig{
		name: "B", ip: edgeBIP, domain: domainB, mode: proxyProtocolV2, secretHeader: secretHeader, secret: secretB,
		cert: d.pki.leaf(t, "beta.test", "*.beta.test"),
		s3:   listeners(false),
	})

	d.srv = startNexo(t, mode, d.edgeA.url(mainHost))
	srvPtr.Store(d.srv)
	d.region = d.srv.Config.Region
	d.waitEdgesReady(t)
	d.connect(d.edgeA.url(consoleHost), d.edgeA.url(mainHost), d.viewer(viewerIP, d.edgeA))
	return d
}

// startNexo starts the deployment of mode behind the proxies at
// edgeAIP and edgeBIP: it trusts both, accepts the PROXY protocol on the
// S3 listener and knows the origin secrets of both (NEXO_SECRET_PROXY_A,
// NEXO_SECRET_PROXY_B). serverURL is the public URL of the S3 API.
func startNexo(t *testing.T, mode, serverURL string) *testutil.Server {
	t.Helper()
	opts := []testutil.Option{testutil.WithConfig(func(c *config.Static) {
		c.TrustedProxies = []string{edgeAIP, edgeBIP}
		c.ProxyProtocol = []string{config.ListenerS3}
		c.Secrets = map[string]string{"proxy-a": secretA, "proxy-b": secretB}
		// The links generated by the console use the public domain.
		c.ServerURL = serverURL
	})}
	switch mode {
	case "snsd":
		return testutil.StartServer(t, opts...)
	case "snmd":
		return testutil.StartServer(t, append(opts, testutil.Drives(4))...)
	default:
		return testutil.StartCluster(t, 4, 2, opts...)
	}
}

// connect creates the Admin API client of the console at consoleURL and
// the root S3 client of the S3 API at s3URL, both through client.
func (d *deployment) connect(consoleURL, s3URL string, client *http.Client) {
	d.t.Helper()
	var err error
	d.admin, err = nexoadmin.New(nexoadmin.Config{
		Endpoint: consoleURL, AccessKey: d.srv.RootUser, SecretKey: d.srv.RootPassword,
		Region: d.region, HTTPClient: client,
	})
	require.NoError(d.t, err)
	d.root = d.s3Client(client, s3URL, true)
}

// waitEdgesReady waits until the probes of the edges see every node ready.
func (d *deployment) waitEdgesReady(t *testing.T) {
	t.Helper()
	require.Eventually(t, func() bool {
		for _, n := range d.srv.Nodes {
			if d.edgeA.s3.isDown(n.Config.Address) || d.edgeA.console.isDown(n.Config.ConsoleAddress) || d.edgeB.s3.isDown(n.Config.Address) {
				return false
			}
		}
		return true
	}, 30*time.Second, 50*time.Millisecond, "the edges see every node ready")
	// One more probe round: the first one may still be in flight.
	time.Sleep(2 * probeInterval)
}

// noRedirect returns the redirects to the caller.
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// viewer returns a client connecting from ip to the edge e whatever the
// host of the URL, as the DNS records of the edge domains do; it trusts
// the certificates of the edges.
func (d *deployment) viewer(ip string, e *edge) *http.Client { return d.viewerTo(ip, e.addr) }

// viewerTo returns a client connecting from ip to addr whatever the host
// of the URL; it trusts the certificates of the edges.
func (d *deployment) viewerTo(ip, addr string) *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second, LocalAddr: &net.TCPAddr{IP: net.ParseIP(ip)}}
	return &http.Client{
		Timeout:       60 * time.Second,
		CheckRedirect: noRedirect,
		Transport: &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, network, addr)
			},
			TLSClientConfig:     &tls.Config{RootCAs: d.pki.roots, MinVersion: tls.VersionTLS12},
			MaxIdleConnsPerHost: 16,
		},
	}
}

// insecureViewer returns a viewer of e that does not verify the
// certificate of the edge (to reach a host the edge has no certificate
// for).
func (d *deployment) insecureViewer(e *edge) *http.Client {
	c := d.viewer(viewerIP, e)
	tr, _ := c.Transport.(*http.Transport)
	tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12} //nolint:gosec // the host has no certificate on this edge
	return c
}

// direct returns a client connecting from ip straight to addr (a node),
// bypassing the edges; prelude, when set, is written on every new
// connection before the request (a forged PROXY header).
func direct(ip, addr string, prelude func(net.Conn) error) *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second, LocalAddr: &net.TCPAddr{IP: net.ParseIP(ip)}}
	return &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: noRedirect,
		Transport: &http.Transport{
			Proxy: nil, DisableKeepAlives: true,
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				conn, err := dialer.DialContext(ctx, network, addr)
				if err != nil || prelude == nil {
					return conn, err
				}
				if err := prelude(conn); err != nil {
					_ = conn.Close()
					return nil, err
				}
				return conn, nil
			},
		},
	}
}

// response is an answer read whole.
type response struct {
	status int
	header http.Header
	body   []byte
}

// xcache is the X-Cache of the answer.
func (r response) xcache() string { return r.header.Get(cache.HeaderXCache) }

// fetch sends a request with client and reads the answer.
func fetch(t *testing.T, client *http.Client, method, rawURL string, hdr http.Header, body []byte) response {
	t.Helper()
	r, err := try(client, method, rawURL, hdr, body)
	require.NoError(t, err, "%s %s", method, rawURL)
	return r
}

// try sends a request with client and reads the answer.
func try(client *http.Client, method, rawURL string, hdr http.Header, body []byte) (response, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, rawURL, rd)
	if err != nil {
		return response{}, err
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	if h := hdr.Get("Host"); h != "" {
		req.Host = h
		req.Header.Del("Host")
	}
	resp, err := client.Do(req)
	if err != nil {
		return response{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return response{}, err
	}
	return response{status: resp.StatusCode, header: resp.Header, body: data}, nil
}

// pinned is a header that pins a request to one node through an edge.
func pinned(node string) http.Header { return http.Header{headerPin: {node}} }

// fixedEndpoint resolves every request of the SDK to one endpoint: the
// custom domain of an s3 distribution is the bucket itself.
type fixedEndpoint struct{ u url.URL }

func (f fixedEndpoint) ResolveEndpoint(context.Context, s3.EndpointParameters) (smithyendpoints.Endpoint, error) {
	return smithyendpoints.Endpoint{URI: f.u}, nil
}

// s3Client returns a root S3 client of endpoint through client (a viewer
// of an edge): path-style for the S3 API of the deployment, or the fixed
// endpoint of an s3 distribution (the domain is the bucket). The SDK does
// not retry: the tests assert the first answer.
func (d *deployment) s3Client(client *http.Client, endpoint string, pathStyle bool) *s3.Client {
	cfg := aws.Config{
		Region:           d.region,
		Credentials:      credentials.NewStaticCredentialsProvider(d.srv.RootUser, d.srv.RootPassword, ""),
		HTTPClient:       client,
		RetryMaxAttempts: 1,
	}
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		if pathStyle {
			o.BaseEndpoint = aws.String(endpoint)
			o.UsePathStyle = true
			return
		}
		u, err := url.Parse(endpoint)
		require.NoError(d.t, err)
		o.EndpointResolverV2 = fixedEndpoint{u: *u}
	})
}

// put stores an object through the S3 API behind edge A.
func (d *deployment) put(t *testing.T, bucket, key, body, contentType string) {
	t.Helper()
	_, err := d.root.PutObject(d.ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key),
		Body: strings.NewReader(body), ContentType: aws.String(contentType)})
	require.NoError(t, err, "put %s/%s", bucket, key)
}

// nodeAddrs are the S3 listeners of the nodes.
func (d *deployment) nodeAddrs() []string {
	out := make([]string, 0, len(d.srv.Nodes))
	for _, n := range d.srv.Nodes {
		out = append(out, n.Config.Address)
	}
	return out
}

// cacheSynced waits for the caches of the running nodes to know the
// invalidations of their peers: their entries are served as HITs.
func (d *deployment) cacheSynced(t *testing.T) {
	t.Helper()
	require.Eventually(t, func() bool {
		for _, n := range d.srv.Nodes {
			if n.App == nil {
				continue
			}
			st, err := n.App.Cache().NodeStats(d.ctx, cache.StatsQuery{})
			if err != nil || !st.Synced {
				return false
			}
		}
		return true
	}, 15*time.Second, 10*time.Millisecond, "the node caches are synchronized")
}
