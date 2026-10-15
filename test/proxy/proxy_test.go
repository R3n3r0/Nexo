//go:build proxy

package proxy_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// Content of the website distribution.
const (
	siteBucket  = "site"
	filesBucket = "files"
	home        = "<h1>home</h1>"
	notFound    = "<h1>not found</h1>"
	zipData     = "zipdata"
	keyGroup    = "releases"
)

// Hosts of the distributions: "site" (website) and "files" (s3 mode) are
// both served by the two edges, each on its own domain.
const (
	siteA  = "www.alpha.test"
	siteB  = "www.beta.test"
	filesA = "files.alpha.test"
	filesB = "files.beta.test"
)

// TestMultiProxy runs Nexo behind two edge proxies with different domains
// and origin secrets (docs/10 §10.6 TCD-01..03, docs/11 §11.12 TCC-C-05,
// docs/09 TPS-19 on the custom domains) in every deployment mode.
//
// covers: S-DST-01, S-DST-05, S-DST-07b, TPS-19
func TestMultiProxy(t *testing.T) {
	requireLoopbackAliases(t)
	for _, mode := range modes() {
		t.Run(mode, func(t *testing.T) {
			d := startDeployment(t, mode)
			d.seed(t)
			require.True(t, t.Run("TCD-03 Admin API changes apply on every node", d.testAdminAPI),
				"the next steps need the distributions")
			t.Run("TCD-01 both access points", d.testAccessPoints)
			t.Run("TCD-01 direct access refused", d.testDirectAccess)
			t.Run("TDS-08 client address through the edges", d.testClientAddress)
			t.Run("TPS-19 presigned URLs on the custom domains", d.testPresigned)
			t.Run("TDS-06 signed URLs behind the edges", d.testSignedURLs)
			t.Run("TCC-C-05 purge of one domain", d.testPurgeOneDomain)
			t.Run("TCD-02 node stopped", d.testNodeStopped)
		})
	}
}

// originSecrets accepts the secrets of both edges, referenced by name: the
// values are NEXO_SECRET_PROXY_A and NEXO_SECRET_PROXY_B of the nodes.
func originSecrets() *nexoadmin.OriginSecret {
	return &nexoadmin.OriginSecret{Name: secretHeader, Values: []string{"${secret:proxy-a}", "${secret:proxy-b}"}}
}

// seed creates the buckets, the objects and the key group through the
// edges: the S3 API behind edge A and the Admin API of the console behind
// edge A.
func (d *deployment) seed(t *testing.T) {
	t.Helper()
	for _, b := range []string{siteBucket, filesBucket} {
		_, err := d.root.CreateBucket(d.ctx, &s3.CreateBucketInput{Bucket: aws.String(b)})
		require.NoError(t, err, "create bucket %s through edge A", b)
	}
	d.put(t, siteBucket, "index.html", home, "text/html")
	d.put(t, siteBucket, "404.html", notFound, "text/html")
	d.put(t, siteBucket, "assets/app.js", "console.log(1)", "application/javascript")
	d.put(t, siteBucket, "downloads/v1.zip", zipData, "application/zip")
	kg, err := d.admin.CreateKeyGroup(d.ctx, nexoadmin.CreateKeyGroupRequest{ID: keyGroup,
		GenerateKey: &nexoadmin.GenerateKeyRequest{Algorithm: nexoadmin.KeyEd25519}})
	require.NoError(t, err)
	require.NotNil(t, kg.Key)
	d.signingKey = kg.Key.PrivateKeyPEM
}

// siteDistribution is the website distribution served by both edges, on
// hostA behind edge A and hostB behind edge B.
func siteDistribution(hostA, hostB string) nexoadmin.Distribution {
	return nexoadmin.Distribution{
		ID: "site", Enabled: true, Aliases: []string{hostA, hostB}, Mode: nexoadmin.ModeWebsite,
		Origin:          nexoadmin.Origin{Bucket: siteBucket},
		DefaultBehavior: nexoadmin.Behavior{Access: nexoadmin.AccessPublic, IndexDocument: "index.html"},
		Behaviors: []nexoadmin.Behavior{{
			PathPattern: &nexoadmin.KeyPattern{Type: "glob", Pattern: "/downloads/**"},
			Access:      nexoadmin.AccessSigned, Signing: &nexoadmin.Signing{KeyGroup: keyGroup},
		}},
		ErrorPages: []nexoadmin.ErrorPage{{Status: http.StatusNotFound, Path: "/404.html"}},
		// Plain HTTP is redirected: the edges terminate TLS and say so
		// (X-Forwarded-Proto, the SSL TLV of the PROXY header).
		Redirects: []nexoadmin.Redirect{{HTTPSOnly: true}},
		Restrictions: nexoadmin.Restrictions{
			SourceIP:           &nexoadmin.IPRestriction{Deny: []string{deniedIP}},
			OriginSecretHeader: originSecrets(),
		},
		TLS: nexoadmin.DistributionTLS{Certificate: nexoadmin.CertificateNone},
	}
}

// filesDistribution is the s3 distribution of the files bucket, an S3
// endpoint on a domain of each edge.
func filesDistribution(hostA, hostB string) nexoadmin.Distribution {
	return nexoadmin.Distribution{
		ID: "files", Enabled: true, Aliases: []string{hostA, hostB}, Mode: nexoadmin.ModeS3,
		Origin:       nexoadmin.Origin{Bucket: filesBucket},
		Restrictions: nexoadmin.Restrictions{OriginSecretHeader: originSecrets()},
		TLS:          nexoadmin.DistributionTLS{Certificate: nexoadmin.CertificateNone},
	}
}

// siteVia is the website distribution through each edge.
func (d *deployment) siteVia() []struct {
	e    *edge
	host string
} {
	return []struct {
		e    *edge
		host string
	}{{d.edgeA, siteA}, {d.edgeB, siteB}}
}

// propagationBound bounds the time a change of the Admin API takes to be
// served by every node (a few milliseconds measured; generous for the
// race detector on a loaded runner).
const propagationBound = 3 * time.Second

// servedEverywhere waits until every node serves path of host through e
// with the wanted status and body, and returns how long it took.
func (d *deployment) servedEverywhere(t *testing.T, e *edge, host, path string, status int, body string) time.Duration {
	t.Helper()
	start := time.Now()
	client := d.viewer(viewerIP, e)
	for _, node := range d.nodeAddrs() {
		var last response
		ok := assert.Eventually(t, func() bool {
			r, err := try(client, http.MethodGet, e.url(host)+path, pinned(node), nil)
			last = r
			return err == nil && r.status == status && (body == "" || string(r.body) == body)
		}, propagationBound, 5*time.Millisecond, "node %s serves %s%s", node, host, path)
		if !ok {
			t.Logf("last answer of %s: %d %s", node, last.status, last.body)
		}
	}
	return time.Since(start)
}

// testAdminAPI creates, changes and deletes distributions through the
// Admin API of the console behind edge A (what the dashboard does): every
// change is served by every node through both edges at once, without
// restart (TCD-03).
func (d *deployment) testAdminAPI(t *testing.T) {
	for _, dist := range []nexoadmin.Distribution{siteDistribution(siteA, siteB), filesDistribution(filesA, filesB)} {
		_, err := d.admin.CreateDistribution(d.ctx, dist)
		require.NoError(t, err, "create %s", dist.ID)
	}
	for _, v := range d.siteVia() {
		took := d.servedEverywhere(t, v.e, v.host, "/", http.StatusOK, home)
		t.Logf("%s: served by every node %s after the creation", v.host, took)
	}
	route, err := d.admin.ResolveHost(d.ctx, siteB)
	require.NoError(t, err)
	assert.Equal(t, "distribution", route.Kind)
	assert.Equal(t, "site", route.Distribution)

	// An alias added on the fly, then a whole distribution created and
	// deleted.
	cur, err := d.admin.GetDistribution(d.ctx, "site")
	require.NoError(t, err)
	cur.Aliases = append(cur.Aliases, "shop.beta.test")
	_, err = d.admin.UpdateDistribution(d.ctx, *cur, cur.ETag)
	require.NoError(t, err)
	d.servedEverywhere(t, d.edgeB, "shop.beta.test", "/", http.StatusOK, home)

	temp := siteDistribution(siteA, siteB)
	temp.ID, temp.Aliases = "temp", []string{"temp.alpha.test"}
	_, err = d.admin.CreateDistribution(d.ctx, temp)
	require.NoError(t, err)
	d.servedEverywhere(t, d.edgeA, "temp.alpha.test", "/", http.StatusOK, home)
	require.NoError(t, d.admin.DeleteDistribution(d.ctx, "temp"))
	// Gone: the host is no alias any more (the S3 API refuses the
	// anonymous request).
	d.servedEverywhere(t, d.edgeA, "temp.alpha.test", "/", http.StatusForbidden, "")
}

// testAccessPoints serves the distribution through both edges, from every
// node, and the S3 API and the console through edge A (TCD-01).
func (d *deployment) testAccessPoints(t *testing.T) {
	for _, v := range d.siteVia() {
		client := d.viewer(viewerIP, v.e)
		// The edge balances over every node (a node whose probe was slow
		// is skipped until the next one: a few more requests).
		upstreams := map[string]bool{}
		for i := 0; len(upstreams) < len(d.srv.Nodes) && i < 50*len(d.srv.Nodes); i++ {
			r := fetch(t, client, http.MethodGet, v.e.url(v.host)+"/", nil, nil)
			require.Equal(t, http.StatusOK, r.status, "%s: %s", v.host, r.body)
			assert.Equal(t, home, string(r.body))
			assert.Equal(t, "text/html", r.header.Get("Content-Type"))
			upstreams[r.header.Get(headerUpstream)] = true
		}
		assert.Len(t, upstreams, len(d.srv.Nodes), "%s: the edge balances over every node", v.host)
		r := fetch(t, client, http.MethodGet, v.e.url(v.host)+"/missing.html", nil, nil)
		assert.Equal(t, http.StatusNotFound, r.status)
		assert.Equal(t, notFound, string(r.body), "the error page of the distribution")
	}
	// Each edge serves its own domains only: it has no certificate for the
	// other ones, and refuses their requests (421).
	for _, v := range []struct {
		e    *edge
		host string
	}{{d.edgeA, siteB}, {d.edgeB, siteA}} {
		_, err := try(d.viewer(viewerIP, v.e), http.MethodGet, v.e.url(v.host)+"/", nil, nil)
		assert.ErrorContains(t, err, "certificate is valid for", "edge %s, %s", v.e.cfg.name, v.host)
		r := fetch(t, d.insecureViewer(v.e), http.MethodGet, v.e.url(v.host)+"/", nil, nil)
		assert.Equal(t, http.StatusMisdirectedRequest, r.status, "edge %s, %s", v.e.cfg.name, v.host)
	}

	// The S3 API (SigV4 with the public host) and the console through
	// edge A.
	list, err := d.root.ListBuckets(d.ctx, &s3.ListBucketsInput{})
	require.NoError(t, err)
	assert.Len(t, list.Buckets, 2)
	info, err := d.admin.ServerInfo(d.ctx)
	require.NoError(t, err)
	assert.NotEmpty(t, info)
}

// testDirectAccess sends the requests of the distribution domains straight
// to every node, bypassing the edges: without (or with a wrong) origin
// secret they are refused; with a secret but outside an edge, the
// forwarding headers a client sets are ignored (TCD-01, TDS-07, TDS-08).
func (d *deployment) testDirectAccess(t *testing.T) {
	for _, node := range d.nodeAddrs() {
		client := direct(viewerIP, node, nil)
		get := func(host string, hdr http.Header) response {
			if hdr == nil {
				hdr = http.Header{}
			}
			hdr.Set("Host", host)
			return fetch(t, client, http.MethodGet, "http://"+node+"/", hdr, nil)
		}
		for _, host := range []string{siteA, siteB, filesA, filesB} {
			assert.Equal(t, http.StatusForbidden, get(host, nil).status, "%s %s without secret", node, host)
			assert.Equal(t, http.StatusForbidden, get(host, http.Header{secretHeader: {"guess"}}).status, "%s %s wrong secret", node, host)
		}
		for _, secret := range []string{secretA, secretB} {
			// The secret is right, but the request is plain HTTP: the
			// distribution redirects it to HTTPS.
			r := get(siteA, http.Header{secretHeader: {secret}})
			assert.Equal(t, http.StatusMovedPermanently, r.status, "%s", r.body)
			assert.True(t, strings.HasPrefix(r.header.Get("Location"), "https://"+siteA), r.header.Get("Location"))
		}
		// A direct client claiming HTTPS and another address is not a
		// trusted proxy: its headers are ignored.
		r := get(siteA, http.Header{secretHeader: {secretA}, "X-Forwarded-Proto": {"https"}, "X-Forwarded-For": {"203.0.113.9"}})
		assert.Equal(t, http.StatusMovedPermanently, r.status)
	}
}

// testClientAddress checks that the nodes see the real client through
// both edges (X-Forwarded-For of edge A, PROXY protocol of edge B) and
// ignore the addresses a client forges (TDS-08 with real proxies).
func (d *deployment) testClientAddress(t *testing.T) {
	for _, v := range d.siteVia() {
		u := v.e.url(v.host) + "/"
		denied, viewer := d.viewer(deniedIP, v.e), d.viewer(viewerIP, v.e)
		assert.Equal(t, http.StatusForbidden, fetch(t, denied, http.MethodGet, u, nil, nil).status, "%s: denied client", v.host)
		assert.Equal(t, http.StatusForbidden, fetch(t, denied, http.MethodGet, u, http.Header{"X-Forwarded-For": {viewerIP}}, nil).status,
			"%s: a denied client forging X-Forwarded-For", v.host)
		assert.Equal(t, http.StatusForbidden, fetch(t, denied, http.MethodGet, u, http.Header{"Forwarded": {"for=" + viewerIP}}, nil).status,
			"%s: a denied client forging Forwarded", v.host)
		assert.Equal(t, http.StatusOK, fetch(t, viewer, http.MethodGet, u, http.Header{"X-Forwarded-For": {deniedIP}}, nil).status,
			"%s: an allowed client forging a denied address", v.host)
	}
	// A PROXY header is honoured from the edges only: sent by a client
	// straight to a node it is read and ignored (the address stays the
	// client's, the connection is not secure).
	for _, node := range d.nodeAddrs() {
		forged := direct(viewerIP, node, func(c net.Conn) error {
			src := &net.TCPAddr{IP: net.ParseIP(deniedIP), Port: 40000}
			dst, _ := net.ResolveTCPAddr("tcp", node)
			return writeProxyHeader(c, src, dst, true)
		})
		r := fetch(t, forged, http.MethodGet, "http://"+node+"/", http.Header{"Host": {siteA}, secretHeader: {secretA}}, nil)
		assert.Equal(t, http.StatusMovedPermanently, r.status, "%s: forged PROXY header: %s", node, r.body)
	}
}

// testPresigned uses the s3 distribution behind each edge as the S3
// endpoint of its bucket: SigV4 and presigned URLs with the public host of
// the edge, through the proxy that adds its origin secret; the links the
// console generates with the public URL work through edge A (TPS-19).
func (d *deployment) testPresigned(t *testing.T) {
	viaA := d.s3Client(d.viewer(viewerIP, d.edgeA), d.edgeA.url(filesA), false)
	viaB := d.s3Client(d.viewer(viewerIP, d.edgeB), d.edgeB.url(filesB), false)
	_, err := viaA.PutObject(d.ctx, &s3.PutObjectInput{Bucket: aws.String(filesBucket), Key: aws.String("docs/a.txt"),
		Body: strings.NewReader("hello from A")})
	require.NoError(t, err)
	got, err := viaB.GetObject(d.ctx, &s3.GetObjectInput{Bucket: aws.String(filesBucket), Key: aws.String("docs/a.txt")})
	require.NoError(t, err)
	body, _ := io.ReadAll(got.Body)
	_ = got.Body.Close()
	assert.Equal(t, "hello from A", string(body))

	for _, v := range []struct {
		e      *edge
		host   string
		client *s3.Client
		other  *edge
		oHost  string
	}{{d.edgeA, filesA, viaA, d.edgeB, filesB}, {d.edgeB, filesB, viaB, d.edgeA, filesA}} {
		ps, err := s3.NewPresignClient(v.client).PresignGetObject(d.ctx, &s3.GetObjectInput{Bucket: aws.String(filesBucket), Key: aws.String("docs/a.txt")})
		require.NoError(t, err)
		require.True(t, strings.HasPrefix(ps.URL, v.e.url(v.host)+"/docs/a.txt?"), ps.URL)
		for range len(d.srv.Nodes) {
			r := fetch(t, d.viewer(otherIP, v.e), http.MethodGet, ps.URL, nil, nil)
			require.Equal(t, http.StatusOK, r.status, "%s: %s", ps.URL, r.body)
			assert.Equal(t, "hello from A", string(r.body))
		}
		// The signature covers the host: the link of one domain is refused
		// on the other one.
		swapped := strings.Replace(ps.URL, v.e.url(v.host), v.other.url(v.oHost), 1)
		r := fetch(t, d.viewer(viewerIP, v.other), http.MethodGet, swapped, nil, nil)
		assert.Equal(t, http.StatusForbidden, r.status)
		assert.Contains(t, string(r.body), "SignatureDoesNotMatch")
		// Straight to a node, without the edge and its secret: refused.
		u, err := url.Parse(ps.URL)
		require.NoError(t, err)
		r = fetch(t, direct(viewerIP, d.nodeAddrs()[0], nil), http.MethodGet, "http://"+d.nodeAddrs()[0]+u.RequestURI(),
			http.Header{"Host": {u.Host}}, nil)
		assert.Equal(t, http.StatusForbidden, r.status)
	}

	// A presigned upload through edge B (PROXY protocol), read through
	// edge A.
	put, err := s3.NewPresignClient(viaB).PresignPutObject(d.ctx, &s3.PutObjectInput{Bucket: aws.String(filesBucket), Key: aws.String("up/b.txt")})
	require.NoError(t, err)
	r := fetch(t, d.viewer(viewerIP, d.edgeB), http.MethodPut, put.URL, nil, []byte("uploaded through B"))
	require.Equal(t, http.StatusOK, r.status, "%s", r.body)
	got, err = viaA.GetObject(d.ctx, &s3.GetObjectInput{Bucket: aws.String(filesBucket), Key: aws.String("up/b.txt")})
	require.NoError(t, err)
	body, _ = io.ReadAll(got.Body)
	_ = got.Body.Close()
	assert.Equal(t, "uploaded through B", string(body))

	// The link the console generates uses the public URL of the
	// deployment (NEXO_SERVER_URL, behind edge A).
	link, err := d.admin.Presign(d.ctx, nexoadmin.PresignRequest{Bucket: filesBucket, Key: "docs/a.txt", ExpiresIn: 600})
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(link.URL, d.edgeA.url(mainHost)+"/files/docs/a.txt?"), link.URL)
	r = fetch(t, d.viewer(otherIP, d.edgeA), http.MethodGet, link.URL, nil, nil)
	require.Equal(t, http.StatusOK, r.status, "%s", r.body)
	assert.Equal(t, "hello from A", string(r.body))
}

// testSignedURLs signs URLs of the distribution with the Admin API for
// each domain, restricted to the address of the viewer: the edges forward
// the real address, so the viewer downloads and another client does not
// (TDS-06 behind the proxies).
func (d *deployment) testSignedURLs(t *testing.T) {
	for _, v := range d.siteVia() {
		signed, err := d.admin.SignDistributionURL(d.ctx, "site", nexoadmin.SignRequest{
			URL: v.e.url(v.host) + "/downloads/v1.zip", PrivateKeyPEM: d.signingKey, ExpiresIn: 600, IPRange: viewerIP + "/32",
		})
		require.NoError(t, err)
		r := fetch(t, d.viewer(viewerIP, v.e), http.MethodGet, signed.URL, nil, nil)
		require.Equal(t, http.StatusOK, r.status, "%s: %s", signed.URL, r.body)
		assert.Equal(t, zipData, string(r.body))
		assert.Equal(t, http.StatusForbidden, fetch(t, d.viewer(otherIP, v.e), http.MethodGet, signed.URL, nil, nil).status,
			"%s: another client address", v.host)
		assert.Equal(t, http.StatusForbidden, fetch(t, d.viewer(viewerIP, v.e), http.MethodGet, v.e.url(v.host)+"/downloads/v1.zip", nil, nil).status,
			"%s: unsigned", v.host)
	}
}

// cacheState returns the X-Cache of path on every node through e.
func (d *deployment) cacheState(t *testing.T, e *edge, host, path string) map[string]string {
	t.Helper()
	client := d.viewer(viewerIP, e)
	out := map[string]string{}
	for _, node := range d.nodeAddrs() {
		r := fetch(t, client, http.MethodGet, e.url(host)+path, pinned(node), nil)
		require.Equal(t, http.StatusOK, r.status, "%s %s%s: %s", node, host, path, r.body)
		out[node] = r.xcache()
	}
	return out
}

// every returns the state expected on every node.
func (d *deployment) every(state string) map[string]string {
	out := map[string]string{}
	for _, node := range d.nodeAddrs() {
		out[node] = state
	}
	return out
}

// invalidate purges through the Admin API and waits until every node
// applied the invalidation.
func (d *deployment) invalidate(t *testing.T, req nexoadmin.CacheInvalidationRequest) {
	t.Helper()
	inv, err := d.admin.InvalidateCache(d.ctx, "site", req)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		got, err := d.admin.GetCacheInvalidation(d.ctx, "site", inv.ID)
		return err == nil && got.State == nexoadmin.InvalidationCompleted
	}, 10*time.Second, 20*time.Millisecond, "invalidation %s completed on every node", inv.ID)
}

// testPurgeOneDomain fills the cache of every node through both edges,
// then purges the domain of one edge: the other domain keeps its entries
// (TCC-C-05). A write is seen at once on both domains (TCC-05).
func (d *deployment) testPurgeOneDomain(t *testing.T) {
	d.cacheSynced(t)
	const path = "/assets/app.js"
	for _, v := range d.siteVia() {
		d.cacheState(t, v.e, v.host, path)
		require.Equal(t, d.every("HIT"), d.cacheState(t, v.e, v.host, path), "%s: cached on every node", v.host)
	}
	d.invalidate(t, nexoadmin.CacheInvalidationRequest{Domain: siteA, All: true})
	assert.Equal(t, d.every("MISS"), d.cacheState(t, d.edgeA, siteA, path), "the purged domain")
	assert.Equal(t, d.every("HIT"), d.cacheState(t, d.edgeB, siteB, path), "the other domain kept its entries")

	d.invalidate(t, nexoadmin.CacheInvalidationRequest{Domain: siteB, Paths: []string{path}})
	assert.Equal(t, d.every("MISS"), d.cacheState(t, d.edgeB, siteB, path), "the path purged on its domain")
	assert.Equal(t, d.every("HIT"), d.cacheState(t, d.edgeA, siteA, path), "the other domain kept its entries")

	// A write through the S3 API drops the entries of every domain on
	// every node: never the old content.
	d.put(t, siteBucket, "assets/app.js", "console.log(2)", "application/javascript")
	for _, v := range d.siteVia() {
		client := d.viewer(viewerIP, v.e)
		for _, node := range d.nodeAddrs() {
			r := fetch(t, client, http.MethodGet, v.e.url(v.host)+path, pinned(node), nil)
			assert.Equal(t, "console.log(2)", string(r.body), "%s %s", v.host, node)
		}
	}
}

// loadFailure is a request of the load that did not get the expected
// answer.
type loadFailure struct {
	what string
	err  error
	r    response
}

func (f loadFailure) String() string {
	if f.err != nil {
		return fmt.Sprintf("%s: %v", f.what, f.err)
	}
	return fmt.Sprintf("%s: %d %s %q", f.what, f.r.status, f.r.header.Get(headerUpstream), f.r.body)
}

// testNodeStopped stops a node under load through both edges: the edges
// fall back on the other nodes without a single error, the writes and the
// configuration changes keep working, and the node serves them again once
// restarted (TCD-02).
func (d *deployment) testNodeStopped(t *testing.T) {
	if len(d.srv.Nodes) < 2 {
		t.Skip("a single node: nothing to fall back on")
	}
	stopped := len(d.srv.Nodes) - 1
	stoppedAddr := d.srv.Nodes[stopped].Config.Address
	ctx, cancel := context.WithCancel(d.ctx)
	defer cancel()

	var (
		mu        sync.Mutex
		failures  []loadFailure
		total     atomic.Int64
		stoppedAt atomic.Int64
		onStopped atomic.Int64
		afterStop atomic.Int64
		wg        sync.WaitGroup
	)
	fail := func(f loadFailure) {
		mu.Lock()
		defer mu.Unlock()
		failures = append(failures, f)
	}
	for _, v := range d.siteVia() {
		for w := range 3 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				client := d.viewer(viewerIP, v.e)
				paths := []string{"/", "/assets/app.js", "/missing.html"}
				for i := 0; ctx.Err() == nil; i++ {
					path := paths[(i+w)%len(paths)]
					began := time.Now().UnixNano()
					r, err := try(client, http.MethodGet, v.e.url(v.host)+path, nil, nil)
					if ctx.Err() != nil {
						return
					}
					total.Add(1)
					want := http.StatusOK
					if path == "/missing.html" {
						want = http.StatusNotFound
					}
					if err != nil || r.status != want {
						fail(loadFailure{what: "GET " + v.host + path, err: err, r: r})
						continue
					}
					if at := stoppedAt.Load(); at != 0 && began > at {
						afterStop.Add(1)
						if r.header.Get(headerUpstream) == stoppedAddr {
							onStopped.Add(1)
						}
					}
				}
			}()
		}
	}
	// S3 reads through edge B (PROXY protocol, a connection per request).
	viaB := d.s3Client(d.viewer(viewerIP, d.edgeB), d.edgeB.url(filesB), false)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			out, err := viaB.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(filesBucket), Key: aws.String("docs/a.txt")})
			if ctx.Err() != nil {
				return
			}
			total.Add(1)
			if err != nil {
				fail(loadFailure{what: "S3 GetObject via edge B", err: err})
				continue
			}
			_, _ = io.Copy(io.Discard, out.Body)
			_ = out.Body.Close()
		}
	}()

	time.Sleep(300 * time.Millisecond)
	d.srv.StopNode(t, stopped)
	stoppedAt.Store(time.Now().UnixNano())
	t.Logf("node %s stopped after %d requests", stoppedAddr, total.Load())

	// While the node is down: writes through both edges and a change of
	// the distribution through the Admin API.
	viaA := d.s3Client(d.viewer(viewerIP, d.edgeA), d.edgeA.url(filesA), false)
	for i := range 4 {
		client := viaA
		if i%2 == 1 {
			client = viaB
		}
		_, err := client.PutObject(d.ctx, &s3.PutObjectInput{Bucket: aws.String(filesBucket), Key: aws.String(fmt.Sprintf("down/%d.txt", i)),
			Body: strings.NewReader(fmt.Sprintf("written while a node is down %d", i))})
		require.NoError(t, err, "write %d", i)
	}
	cur, err := d.admin.GetDistribution(d.ctx, "site")
	require.NoError(t, err)
	cur.Aliases = append(cur.Aliases, "www2.alpha.test")
	_, err = d.admin.UpdateDistribution(d.ctx, *cur, cur.ETag)
	require.NoError(t, err)
	r := fetch(t, d.viewer(viewerIP, d.edgeA), http.MethodGet, d.edgeA.url("www2.alpha.test")+"/", nil, nil)
	assert.Equal(t, http.StatusOK, r.status, "%s", r.body)

	time.Sleep(time.Second)
	cancel()
	wg.Wait()
	mu.Lock()
	for _, f := range failures {
		t.Errorf("client error while a node is stopped: %s", f)
	}
	mu.Unlock()
	assert.Zero(t, onStopped.Load(), "no answer from the stopped node")
	assert.Greater(t, afterStop.Load(), int64(50), "the load went on while the node was stopped")
	for _, e := range []*edge{d.edgeA, d.edgeB} {
		n, last := e.failed()
		assert.Zero(t, n, "edge %s answered 502, the last one: %v", e.cfg.name, last)
	}
	t.Logf("%d requests, %d of them while the node was stopped", total.Load(), afterStop.Load())

	// Back: the node serves the change and the objects written while it
	// was down.
	d.srv.StartNode(t, stopped)
	d.waitEdgesReady(t)
	r = fetch(t, d.viewer(viewerIP, d.edgeA), http.MethodGet, d.edgeA.url("www2.alpha.test")+"/", pinned(stoppedAddr), nil)
	assert.Equal(t, http.StatusOK, r.status, "the alias added while the node was down: %s", r.body)
	ps, err := s3.NewPresignClient(viaB).PresignGetObject(d.ctx, &s3.GetObjectInput{Bucket: aws.String(filesBucket), Key: aws.String("down/1.txt")})
	require.NoError(t, err)
	r = fetch(t, d.viewer(viewerIP, d.edgeB), http.MethodGet, ps.URL, pinned(stoppedAddr), nil)
	assert.Equal(t, http.StatusOK, r.status, "%s", r.body)
	assert.Equal(t, "written while a node is down 1", string(r.body))
}
