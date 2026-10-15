//go:build proxy && linux

package proxy_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/r3n3r0/nexo/internal/service/cache"
	"github.com/r3n3r0/nexo/internal/testutil"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// The configurations of the edges of deploy/docker/compose.multi-proxy.yml.
const (
	multiProxyDir = "../../deploy/docker/multi-proxy"
	// Domains of the compose file: proxy-a (nginx) serves *.alpha.example,
	// proxy-b (HAProxy) *.beta.example.
	realSiteA    = "www.alpha.example"
	realSiteB    = "www.beta.example"
	realFilesA   = "files.alpha.example"
	realFilesB   = "files.beta.example"
	realMain     = "s3.alpha.example"
	realConsole  = "console.alpha.example"
	nodeIDHeader = cache.HeaderXCacheNode
)

// realProxy is an nginx or HAProxy process running the configuration of
// compose.multi-proxy.yml, adapted to the loopback addresses of the test:
// it listens and connects from ip (its address on the compose network).
type realProxy struct {
	name string
	ip   string
	// httpPort and tlsPort replace the listen ports of the configuration
	// (80/443, 8080/8443), healthPort the monitor port of HAProxy.
	httpPort, tlsPort, healthPort string
	// dir holds the rendered configuration, the certificates and the log.
	dir     string
	logFile string
	cmd     *exec.Cmd
	// stopped is closed when the process exited, with waitErr.
	stopped chan struct{}
	waitErr error
}

// httpsURL is the base URL of host on the TLS port of the proxy.
func (p *realProxy) httpsURL(host string) string { return "https://" + host + ":" + p.tlsPort }

// httpURL is the base URL of host on the plain HTTP port of the proxy.
func (p *realProxy) httpURL(host string) string { return "http://" + host + ":" + p.httpPort }

// requireProxyBinaries returns the paths of nginx and haproxy, skipping
// the test without them unless NEXO_PROXY_REAL=1 requires them (CI).
func requireProxyBinaries(t *testing.T) (string, string) {
	t.Helper()
	nginx, errN := exec.LookPath("nginx")
	haproxy, errH := exec.LookPath("haproxy")
	if errN != nil || errH != nil {
		if os.Getenv("NEXO_PROXY_REAL") == "1" {
			t.Fatalf("NEXO_PROXY_REAL=1 but nginx or haproxy is missing: %v %v", errN, errH)
		}
		t.Skip("nginx and haproxy are needed to run the configurations of compose.multi-proxy.yml")
	}
	return nginx, haproxy
}

// freePort returns a free TCP port of ip.
func freePort(t *testing.T, ip string) string {
	t.Helper()
	ln, err := net.Listen("tcp", net.JoinHostPort(ip, "0"))
	require.NoError(t, err)
	defer ln.Close()
	_, port, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)
	return port
}

// newRealProxy reserves the ports of a proxy.
func newRealProxy(t *testing.T, name, ip string) *realProxy {
	t.Helper()
	return &realProxy{name: name, ip: ip, httpPort: freePort(t, ip), tlsPort: freePort(t, ip), healthPort: freePort(t, ip),
		dir: t.TempDir(), stopped: make(chan struct{})}
}

// nodeAddr returns the address of the node behind the compose host name
// nexo<i> (i from 1 to 4).
func nodeAddr(nodes []*testutil.Node, i int, console bool) string {
	n := nodes[(i-1)%len(nodes)]
	if console {
		return n.Config.ConsoleAddress
	}
	return n.Config.Address
}

// renderNginx adapts the template of proxy-a: the secret of the
// environment, the node addresses instead of the compose host names
// (resolved by Docker there), the ports and certificates of the test, no
// HTTP/2 (older nginx) and the source address of the proxy.
func renderNginx(t *testing.T, p *realProxy, nodes []*testutil.Node, certFile, keyFile string) string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(multiProxyDir, "nginx.conf.template"))
	require.NoError(t, err)
	s := strings.ReplaceAll(string(src), "${NEXO_ORIGIN_SECRET}", secretA)
	for i := 1; i <= 4; i++ {
		s = strings.ReplaceAll(s, fmt.Sprintf("nexo%d:9000", i), nodeAddr(nodes, i, false))
		s = strings.ReplaceAll(s, fmt.Sprintf("nexo%d:9001", i), nodeAddr(nodes, i, true))
	}
	s = regexp.MustCompile(`(?m)^\s*resolver .*;\n`).ReplaceAllString(s, "")
	s = regexp.MustCompile(`(?m)^\s*http2 on;\n`).ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, " resolve ", " ")
	s = strings.ReplaceAll(s, "listen 80", "listen "+p.ip+":"+p.httpPort)
	s = strings.ReplaceAll(s, "listen 443", "listen "+p.ip+":"+p.tlsPort)
	s = strings.ReplaceAll(s, "/etc/nginx/certs/fullchain.pem", certFile)
	s = strings.ReplaceAll(s, "/etc/nginx/certs/privkey.pem", keyFile)
	require.NotContains(t, s, "nexo1:")
	require.NotContains(t, s, "${NEXO_")
	site := filepath.Join(p.dir, "site.conf")
	require.NoError(t, os.WriteFile(site, []byte(s), 0o600))

	user := ""
	if os.Geteuid() == 0 {
		// The workers read the files of the test directory (0700).
		user = "user root;\n"
	}
	main := fmt.Sprintf(`%sdaemon off;
pid %[2]s/nginx.pid;
events { worker_connections 1024; }
http {
    access_log off;
    client_body_temp_path %[2]s/body;
    proxy_temp_path       %[2]s/proxy;
    fastcgi_temp_path     %[2]s/fastcgi;
    uwsgi_temp_path       %[2]s/uwsgi;
    scgi_temp_path        %[2]s/scgi;
    # The compose network gives the proxy its own address: connect from it.
    proxy_bind %[3]s;
    include %[4]s;
}
`, user, p.dir, p.ip, site)
	conf := filepath.Join(p.dir, "nginx.conf")
	require.NoError(t, os.WriteFile(conf, []byte(main), 0o600))
	return conf
}

// renderHAProxy adapts the configuration of proxy-b: the node addresses
// instead of the Docker resolution of the compose host names, the ports
// and the certificate of the test and the source address of the proxy.
func renderHAProxy(t *testing.T, p *realProxy, nodes []*testutil.Node, pemFile string) string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(multiProxyDir, "haproxy.cfg"))
	require.NoError(t, err)
	s := regexp.MustCompile(`(?ms)^resolvers docker\n.*?\n\n`).ReplaceAllString(string(src), "")
	s = strings.ReplaceAll(s, " resolvers docker init-addr last,libc,none", "")
	for i := 1; i <= 4; i++ {
		s = strings.ReplaceAll(s, fmt.Sprintf("server nexo%d nexo%d:9000", i, i), fmt.Sprintf("server nexo%d %s", i, nodeAddr(nodes, i, false)))
	}
	s = strings.ReplaceAll(s, "bind :8080", "bind "+p.ip+":"+p.httpPort)
	s = strings.ReplaceAll(s, "bind :8443 ssl crt /usr/local/etc/haproxy/certs/beta.pem", "bind "+p.ip+":"+p.tlsPort+" ssl crt "+pemFile)
	s = strings.ReplaceAll(s, "bind :8404", "bind "+p.ip+":"+p.healthPort)
	s = strings.Replace(s, "\ndefaults\n", "\ndefaults\n    source "+p.ip+"\n", 1)
	require.NotContains(t, s, "nexo1:")
	require.NotContains(t, s, "resolvers")
	conf := filepath.Join(p.dir, "haproxy.cfg")
	require.NoError(t, os.WriteFile(conf, []byte(s), 0o600))
	return conf
}

// start runs the proxy until the test ends and waits for its TLS port.
func (p *realProxy) start(t *testing.T, bin string, env []string, args ...string) {
	t.Helper()
	p.logFile = filepath.Join(p.dir, p.name+".log")
	logOut, err := os.Create(p.logFile)
	require.NoError(t, err)
	p.cmd = exec.Command(bin, args...)
	p.cmd.Env = append(os.Environ(), env...)
	p.cmd.Stdout, p.cmd.Stderr = logOut, logOut
	// Its own process group: the workers stop with it.
	p.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	require.NoError(t, p.cmd.Start(), "start %s", p.name)
	go func() {
		p.waitErr = p.cmd.Wait()
		_ = logOut.Close()
		close(p.stopped)
	}()
	t.Cleanup(func() {
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-p.stopped:
		case <-time.After(5 * time.Second):
			_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
			<-p.stopped
		}
		if t.Failed() {
			if data, err := os.ReadFile(p.logFile); err == nil {
				t.Logf("%s log:\n%s", p.name, tail(string(data), 40))
			}
		}
	})
	require.Eventually(t, func() bool {
		select {
		case <-p.stopped:
			return true
		default:
		}
		c, err := net.DialTimeout("tcp", net.JoinHostPort(p.ip, p.tlsPort), 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return true
		}
		return false
	}, 10*time.Second, 50*time.Millisecond, "%s listens", p.name)
	select {
	case <-p.stopped:
		data, _ := os.ReadFile(p.logFile)
		t.Fatalf("%s exited: %v\n%s", p.name, p.waitErr, data)
	default:
	}
}

// tail returns the last n lines of s.
func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// realEnv is the cluster behind the real proxies.
type realEnv struct {
	*deployment
	nginx, haproxy *realProxy
}

// via returns the viewer of ip and the base URL of host on proxy p.
func (r *realEnv) via(p *realProxy, ip, host string) (*http.Client, string) {
	return r.viewerTo(ip, net.JoinHostPort(p.ip, p.tlsPort)), p.httpsURL(host)
}

// perNode sends GETs of url through client until every node answered
// (X-Cache-Node) and returns the X-Cache of the first answer of each node.
func (r *realEnv) perNode(t *testing.T, client *http.Client, url, body string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for i := 0; len(out) < len(r.srv.Nodes); i++ {
		require.Less(t, i, 30*len(r.srv.Nodes), "every node answers %s: %v", url, out)
		resp := fetch(t, client, http.MethodGet, url, nil, nil)
		require.Equal(t, http.StatusOK, resp.status, "%s: %s", url, resp.body)
		if body != "" {
			require.Equal(t, body, string(resp.body), url)
		}
		node := resp.header.Get(nodeIDHeader)
		require.NotEmpty(t, node, "X-Cache-Node of %s", url)
		if _, seen := out[node]; !seen {
			out[node] = resp.xcache()
		}
	}
	return out
}

// all is the state of every node.
func (r *realEnv) all(state string) []string {
	out := make([]string, len(r.srv.Nodes))
	for i := range out {
		out[i] = state
	}
	return out
}

// states returns the states of a perNode answer, in a stable order.
func states(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// TestRealProxies runs the edge configurations of
// deploy/docker/compose.multi-proxy.yml (nginx with X-Forwarded-*, HAProxy
// with the PROXY protocol v2, each with its origin secret and domain) with
// the real nginx and haproxy binaries in front of a cluster of 4 nodes:
// TCD-01..03 and TCC-C-05 on the very files the compose file mounts. It
// needs nginx and haproxy in PATH (skipped otherwise, unless
// NEXO_PROXY_REAL=1).
//
// covers: S-DST-01, S-DST-05, S-DST-07b, TPS-19
func TestRealProxies(t *testing.T) {
	requireLoopbackAliases(t)
	nginxBin, haproxyBin := requireProxyBinaries(t)
	d := &deployment{t: t, ctx: context.Background(), mode: "cluster", pki: newPKI(t)}
	r := &realEnv{deployment: d, nginx: newRealProxy(t, "nginx", edgeAIP), haproxy: newRealProxy(t, "haproxy", edgeBIP)}
	d.srv = startNexo(t, "cluster", r.nginx.httpsURL(realMain))
	d.region = d.srv.Config.Region

	certA, keyA := d.pki.leafPEM(t, "alpha.example", "*.alpha.example")
	certFile, keyFile := filepath.Join(r.nginx.dir, "fullchain.pem"), filepath.Join(r.nginx.dir, "privkey.pem")
	require.NoError(t, os.WriteFile(certFile, certA, 0o600))
	require.NoError(t, os.WriteFile(keyFile, keyA, 0o600))
	conf := renderNginx(t, r.nginx, d.srv.Nodes, certFile, keyFile)
	r.nginx.start(t, nginxBin, nil, "-p", r.nginx.dir, "-e", filepath.Join(r.nginx.dir, "error.log"), "-c", conf)

	certB, keyB := d.pki.leafPEM(t, "beta.example", "*.beta.example")
	pemFile := filepath.Join(r.haproxy.dir, "beta.pem")
	require.NoError(t, os.WriteFile(pemFile, append(certB, keyB...), 0o600))
	conf = renderHAProxy(t, r.haproxy, d.srv.Nodes, pemFile)
	r.haproxy.start(t, haproxyBin, []string{"NEXO_ORIGIN_SECRET=" + secretB}, "-db", "-f", conf)

	clientA, _ := r.via(r.nginx, viewerIP, realConsole)
	d.connect(r.nginx.httpsURL(realConsole), r.nginx.httpsURL(realMain), clientA)
	d.seed(t)
	// The node of every cached answer, to check each node through the
	// proxies.
	settings, err := d.admin.GetCacheSettings(d.ctx)
	require.NoError(t, err)
	settings.NodeHeader = true
	_, err = d.admin.SetCacheSettings(d.ctx, *settings, settings.ETag)
	require.NoError(t, err)

	require.True(t, t.Run("TCD-03 distributions of the Admin API on every node", func(t *testing.T) {
		for _, dist := range []nexoadmin.Distribution{siteDistribution(realSiteA, realSiteB), filesDistribution(realFilesA, realFilesB)} {
			_, err := d.admin.CreateDistribution(d.ctx, dist)
			require.NoError(t, err, "create %s", dist.ID)
		}
		for _, v := range []struct {
			p    *realProxy
			host string
		}{{r.nginx, realSiteA}, {r.haproxy, realSiteB}} {
			client, base := r.via(v.p, viewerIP, v.host)
			got := r.perNode(t, client, base+"/", home)
			assert.Len(t, got, len(d.srv.Nodes), "%s through %s", v.host, v.p.name)
		}
	}), "the next steps need the distributions")

	t.Run("TCD-01 each proxy serves its domains", func(t *testing.T) {
		for _, v := range []struct {
			p           *realProxy
			own, others string
		}{{r.nginx, realSiteA, realSiteB}, {r.haproxy, realSiteB, realSiteA}} {
			client, base := r.via(v.p, viewerIP, v.own)
			resp := fetch(t, client, http.MethodGet, base+"/missing.html", nil, nil)
			assert.Equal(t, http.StatusNotFound, resp.status)
			assert.Equal(t, notFound, string(resp.body))
			// The internode RPC is never exposed on the domain of a
			// distribution: the proxy answers 404 itself.
			resp = fetch(t, client, http.MethodPost, base+"/nexo/rpc/v1/peer/health", nil, nil)
			assert.Equal(t, http.StatusNotFound, resp.status, "%s: internode RPC on %s", v.p.name, v.own)
			assert.NotEqual(t, notFound, string(resp.body), "%s: refused by the proxy, not by Nexo", v.p.name)
			insecure := d.viewerTo(viewerIP, net.JoinHostPort(v.p.ip, v.p.tlsPort))
			if tr, ok := insecure.Transport.(*http.Transport); ok {
				tr.TLSClientConfig.InsecureSkipVerify = true // the other domain has no certificate here
			}
			resp = fetch(t, insecure, http.MethodGet, v.p.httpsURL(v.others)+"/", nil, nil)
			assert.Equal(t, http.StatusMisdirectedRequest, resp.status, "%s serves %s", v.p.name, v.others)
		}
		// The S3 API (SigV4 with the public host) through nginx.
		list, err := d.root.ListBuckets(d.ctx, &s3.ListBucketsInput{})
		require.NoError(t, err)
		assert.Len(t, list.Buckets, 2)
	})

	t.Run("TDS-08 client address and HTTPS through the proxies", func(t *testing.T) {
		for _, v := range []struct {
			p    *realProxy
			host string
		}{{r.nginx, realSiteA}, {r.haproxy, realSiteB}} {
			denied, base := r.via(v.p, deniedIP, v.host)
			viewer, _ := r.via(v.p, viewerIP, v.host)
			assert.Equal(t, http.StatusForbidden, fetch(t, denied, http.MethodGet, base+"/", nil, nil).status, "%s: denied client", v.p.name)
			assert.Equal(t, http.StatusForbidden, fetch(t, denied, http.MethodGet, base+"/", http.Header{"X-Forwarded-For": {viewerIP}}, nil).status,
				"%s: a denied client forging X-Forwarded-For", v.p.name)
			assert.Equal(t, http.StatusOK, fetch(t, viewer, http.MethodGet, base+"/", http.Header{"X-Forwarded-For": {deniedIP}}, nil).status,
				"%s: an allowed client forging a denied address", v.p.name)
			// HTTPS is known through the proxy (X-Forwarded-Proto, the SSL
			// TLV of the PROXY header); plain HTTP is redirected.
			plain := d.viewerTo(viewerIP, net.JoinHostPort(v.p.ip, v.p.httpPort))
			resp := fetch(t, plain, http.MethodGet, v.p.httpURL(v.host)+"/", nil, nil)
			assert.Equal(t, http.StatusMovedPermanently, resp.status, "%s: plain HTTP: %s", v.p.name, resp.body)
			assert.True(t, strings.HasPrefix(resp.header.Get("Location"), "https://"+v.host), resp.header.Get("Location"))
		}
	})

	t.Run("TPS-19 presigned URLs on the domains of both proxies", func(t *testing.T) {
		viaA := d.s3Client(r.viewerTo(viewerIP, net.JoinHostPort(r.nginx.ip, r.nginx.tlsPort)), r.nginx.httpsURL(realFilesA), false)
		viaB := d.s3Client(r.viewerTo(viewerIP, net.JoinHostPort(r.haproxy.ip, r.haproxy.tlsPort)), r.haproxy.httpsURL(realFilesB), false)
		_, err := viaB.PutObject(d.ctx, &s3.PutObjectInput{Bucket: aws.String(filesBucket), Key: aws.String("docs/b.txt"),
			Body: strings.NewReader("through HAProxy")})
		require.NoError(t, err)
		for _, v := range []struct {
			p      *realProxy
			client *s3.Client
		}{{r.nginx, viaA}, {r.haproxy, viaB}} {
			ps, err := s3.NewPresignClient(v.client).PresignGetObject(d.ctx, &s3.GetObjectInput{Bucket: aws.String(filesBucket), Key: aws.String("docs/b.txt")})
			require.NoError(t, err)
			resp := fetch(t, r.viewerTo(otherIP, net.JoinHostPort(v.p.ip, v.p.tlsPort)), http.MethodGet, ps.URL, nil, nil)
			require.Equal(t, http.StatusOK, resp.status, "%s: %s", ps.URL, resp.body)
			assert.Equal(t, "through HAProxy", string(resp.body))
		}
		link, err := d.admin.Presign(d.ctx, nexoadmin.PresignRequest{Bucket: filesBucket, Key: "docs/b.txt", ExpiresIn: 600})
		require.NoError(t, err)
		require.True(t, strings.HasPrefix(link.URL, r.nginx.httpsURL(realMain)+"/"), link.URL)
		resp := fetch(t, r.viewerTo(otherIP, net.JoinHostPort(r.nginx.ip, r.nginx.tlsPort)), http.MethodGet, link.URL, nil, nil)
		require.Equal(t, http.StatusOK, resp.status, "%s", resp.body)
	})

	t.Run("TCC-C-05 purge of the domain of one proxy", func(t *testing.T) {
		d.cacheSynced(t)
		const path = "/assets/app.js"
		clientA, baseA := r.via(r.nginx, viewerIP, realSiteA)
		clientB, baseB := r.via(r.haproxy, viewerIP, realSiteB)
		for _, v := range []struct {
			c    *http.Client
			base string
		}{{clientA, baseA}, {clientB, baseB}} {
			r.perNode(t, v.c, v.base+path, "")
			assert.ElementsMatch(t, r.all("HIT"), states(r.perNode(t, v.c, v.base+path, "")), "%s: cached on every node", v.base)
		}
		inv, err := d.admin.InvalidateCache(d.ctx, "site", nexoadmin.CacheInvalidationRequest{Domain: realSiteA, All: true})
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			got, err := d.admin.GetCacheInvalidation(d.ctx, "site", inv.ID)
			return err == nil && got.State == nexoadmin.InvalidationCompleted
		}, 10*time.Second, 20*time.Millisecond)
		assert.ElementsMatch(t, r.all("MISS"), states(r.perNode(t, clientA, baseA+path, "")), "the purged domain")
		assert.ElementsMatch(t, r.all("HIT"), states(r.perNode(t, clientB, baseB+path, "")), "the other domain")
	})

	t.Run("TCD-02 node stopped behind the proxies", func(t *testing.T) {
		stopped := len(d.srv.Nodes) - 1
		ctx, cancel := context.WithCancel(d.ctx)
		defer cancel()
		var (
			wg        sync.WaitGroup
			total     atomic.Int64
			afterStop atomic.Int64
			stoppedAt atomic.Bool
			mu        sync.Mutex
			failures  []string
		)
		for _, v := range []struct {
			p    *realProxy
			host string
		}{{r.nginx, realSiteA}, {r.haproxy, realSiteB}} {
			for range 3 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					client, base := r.via(v.p, viewerIP, v.host)
					for ctx.Err() == nil {
						resp, err := try(client, http.MethodGet, base+"/", nil, nil)
						if ctx.Err() != nil {
							return
						}
						total.Add(1)
						if stoppedAt.Load() {
							afterStop.Add(1)
						}
						if err != nil || resp.status != http.StatusOK || string(resp.body) != home {
							mu.Lock()
							failures = append(failures, fmt.Sprintf("%s: %v %d %q", v.p.name, err, resp.status, resp.body))
							mu.Unlock()
						}
					}
				}()
			}
		}
		time.Sleep(300 * time.Millisecond)
		d.srv.StopNode(t, stopped)
		stoppedAt.Store(true)
		// A write through each proxy while the node is down.
		for _, v := range []struct {
			p    *realProxy
			host string
		}{{r.nginx, realFilesA}, {r.haproxy, realFilesB}} {
			client := d.s3Client(r.viewerTo(viewerIP, net.JoinHostPort(v.p.ip, v.p.tlsPort)), v.p.httpsURL(v.host), false)
			_, err := client.PutObject(d.ctx, &s3.PutObjectInput{Bucket: aws.String(filesBucket), Key: aws.String("down/" + v.p.name),
				Body: strings.NewReader("written while a node is down")})
			assert.NoError(t, err, "write through %s", v.p.name)
		}
		time.Sleep(1500 * time.Millisecond)
		cancel()
		wg.Wait()
		mu.Lock()
		defer mu.Unlock()
		for _, f := range failures {
			t.Errorf("client error while a node is stopped: %s", f)
		}
		assert.Greater(t, afterStop.Load(), int64(50), "the load went on while the node was stopped")
		t.Logf("%d requests, %d of them while the node was stopped", total.Load(), afterStop.Load())
	})

	t.Run("the example distributions of multi-proxy/site.yaml", func(t *testing.T) {
		for _, id := range []string{"site", "files"} {
			require.NoError(t, d.admin.DeleteDistribution(d.ctx, id))
		}
		_, err := d.root.CreateBucket(d.ctx, &s3.CreateBucketInput{Bucket: aws.String("web")})
		require.NoError(t, err)
		d.put(t, "web", "index.html", "<h1>example</h1>", "text/html")
		data, err := os.ReadFile(filepath.Join(multiProxyDir, "site.yaml"))
		require.NoError(t, err)
		dec := yaml.NewDecoder(bytes.NewReader(data))
		for {
			var dist nexoadmin.Distribution
			err := dec.Decode(&dist)
			if errors.Is(err, io.EOF) {
				break
			}
			require.NoError(t, err)
			_, err = d.admin.CreateDistribution(d.ctx, dist)
			require.NoError(t, err, "create %s", dist.ID)
		}
		for _, v := range []struct {
			p    *realProxy
			host string
		}{{r.nginx, realSiteA}, {r.haproxy, realSiteB}} {
			client, base := r.via(v.p, viewerIP, v.host)
			resp := fetch(t, client, http.MethodGet, base+"/", nil, nil)
			assert.Equal(t, http.StatusOK, resp.status, "%s: %s", v.p.name, resp.body)
			assert.Equal(t, "<h1>example</h1>", string(resp.body))
		}
		viaB := d.s3Client(r.viewerTo(viewerIP, net.JoinHostPort(r.haproxy.ip, r.haproxy.tlsPort)), r.haproxy.httpsURL(realFilesB), false)
		_, err = viaB.HeadObject(d.ctx, &s3.HeadObjectInput{Bucket: aws.String(filesBucket), Key: aws.String("docs/b.txt")})
		assert.NoError(t, err)
	})
}
