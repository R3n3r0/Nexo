//go:build upgrade

package upgrade_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/ratelimit"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/testutil"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// Credentials, region and KMS key of the deployments: the same for both
// releases (the KMS key seals the object keys of SSE-S3 and SSE-KMS).
const (
	rootUser     = "upgradeadmin"
	rootPassword = "upgradeadmin-secret"
	region       = "us-east-1"
	kmsKeyName   = "upgrade-key"
)

// Timeouts of the harness.
const (
	readyTimeout = 120 * time.Second
	stopTimeout  = 30 * time.Second
)

// kmsSecret is the NEXO_KMS_SECRET_KEY of the builtin KMS ("name:base64").
func kmsSecret() string {
	return kmsKeyName + ":" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x6e}, 32))
}

// deployment is a Nexo deployment of processes on 127.0.0.1: one node
// with its drives under Dir/n1 (SNMD), or a cluster whose node i (1-based)
// listens on base+i, runs in Dir/n<i> and has its drives below it, every
// node running the same command line:
//
//	http://127.0.0.1:{base+1...base+n}/proc/self/cwd/d{1...m}
type deployment struct {
	t      *testing.T
	Dir    string
	base   int
	drives int
	nodes  []*node
	pki    pki
	// LB is a round-robin load balancer with health checks in front of
	// the S3 listeners: the clients of an application, which a rolling
	// upgrade must not disturb.
	LB *testutil.LoadBalancer
}

// node is one server process of a deployment.
type node struct {
	d       *deployment
	idx     int
	Dir     string
	S3      string // host:port
	Console string // host:port

	mu   sync.Mutex
	cmd  *exec.Cmd
	bin  string
	done chan struct{}
	err  error
}

// newDeployment prepares a deployment of n nodes of drives each (nothing
// is started). The processes are killed when the test ends; their logs
// are printed when it failed.
func newDeployment(t *testing.T, n, drives int) *deployment {
	t.Helper()
	d := &deployment{t: t, Dir: deploymentDir(t), drives: drives}
	d.pki = newPKI(t, filepath.Join(d.Dir, "certs"))
	d.base = consecutivePorts(t, n) - 1
	consoles := consecutivePorts(t, n)
	for i := 1; i <= n; i++ {
		nd := &node{d: d, idx: i, Dir: filepath.Join(d.Dir, "n"+strconv.Itoa(i)),
			S3: "127.0.0.1:" + strconv.Itoa(d.base+i), Console: "127.0.0.1:" + strconv.Itoa(consoles+i-1)}
		require.NoError(t, os.MkdirAll(nd.Dir, 0o755))
		d.nodes = append(d.nodes, nd)
	}
	d.LB = testutil.NewLoadBalancer(t, func() []string {
		out := make([]string, 0, len(d.nodes))
		for _, nd := range d.nodes {
			out = append(out, nd.S3)
		}
		return out
	}, testutil.LBHealthCheck(domain.HealthLivePath, 200*time.Millisecond), testutil.LBTLS(d.pki.server, d.pki.client))
	t.Cleanup(func() {
		for _, nd := range d.nodes {
			nd.kill()
		}
		if t.Failed() || os.Getenv("NEXO_UPGRADE_LOGS") != "" {
			d.dumpLogs()
		}
	})
	return d
}

// deploymentDir returns the directory of the drives: under
// NEXO_UPGRADE_DIR when set, else on the tmpfs /dev/shm when it has room,
// else in the temporary directory of the test. It is removed at the end.
func deploymentDir(t *testing.T) string {
	t.Helper()
	root := os.Getenv("NEXO_UPGRADE_DIR")
	if root == "" && roomy("/dev/shm") {
		root = "/dev/shm"
	}
	if root == "" {
		return t.TempDir()
	}
	dir, err := os.MkdirTemp(root, "nexo-upgrade-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// roomy reports whether dir is a writable directory of a file system with
// at least 2 GiB and 20% free.
func roomy(dir string) bool {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil || syscall.Access(dir, 2) != nil {
		return false
	}
	free := st.Bavail * uint64(st.Bsize)  //nolint:gosec // block counts of a real file system
	total := st.Blocks * uint64(st.Bsize) //nolint:gosec // block counts of a real file system
	return free >= 2<<30 && free*5 >= total
}

// node returns node i (1-based).
func (d *deployment) node(i int) *node { return d.nodes[i-1] }

// endpoints returns the drive arguments of the deployment.
func (d *deployment) endpoints() []string {
	if len(d.nodes) == 1 {
		return []string{filepath.Join(d.nodes[0].Dir, fmt.Sprintf("d{1...%d}", d.drives))}
	}
	return []string{fmt.Sprintf("https://127.0.0.1:{%d...%d}/proc/self/cwd/d{1...%d}", d.base+1, d.base+len(d.nodes), d.drives)}
}

// driveDirs returns the drive directories of node i.
func (d *deployment) driveDirs(i int) []string {
	out := make([]string, 0, d.drives)
	for j := 1; j <= d.drives; j++ {
		out = append(out, filepath.Join(d.node(i).Dir, "d"+strconv.Itoa(j)))
	}
	return out
}

// start starts every node with bin and waits until each is ready.
func (d *deployment) start(bin string) {
	d.t.Helper()
	for _, nd := range d.nodes {
		nd.start(bin)
	}
	for _, nd := range d.nodes {
		nd.waitReady()
	}
}

// stop stops every node gracefully, concurrently.
func (d *deployment) stop() {
	d.t.Helper()
	var wg sync.WaitGroup
	for _, nd := range d.nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			nd.stop()
		}()
	}
	wg.Wait()
}

// dumpLogs prints the end of the log of every node.
func (d *deployment) dumpLogs() {
	for _, nd := range d.nodes {
		d.t.Logf("--- node %d (%s) log tail ---\n%s", nd.idx, nd.S3, nd.logTail(80))
	}
}

// logPath is the log of the node (every run appends to it).
func (n *node) logPath() string { return filepath.Join(n.Dir, "nexo.log") }

// logTail returns the last lines of the log.
func (n *node) logTail(lines int) string {
	data, err := os.ReadFile(n.logPath())
	if err != nil {
		return ""
	}
	all := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	return strings.Join(all, "\n")
}

// start launches the node with bin (it does not wait for it to be ready).
func (n *node) start(bin string) {
	t := n.d.t
	t.Helper()
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.cmd != nil {
		select {
		case <-n.done:
		default:
			t.Fatalf("node %d is already running", n.idx)
		}
	}
	logFile, err := os.OpenFile(n.logPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	require.NoError(t, err)
	_, _ = fmt.Fprintf(logFile, "=== %s %s\n", time.Now().Format(time.RFC3339Nano), bin)
	args := append([]string{"server", "--address", n.S3, "--console-address", n.Console, "--certs-dir", n.d.pki.dir,
		"--log-format", "text", "--log-level", "warn"}, n.d.endpoints()...)
	cmd := exec.Command(bin, args...) //nolint:gosec // the binaries built by TestMain
	cmd.Dir = n.Dir
	cmd.Env = nodeEnv(len(n.d.nodes) > 1)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	// The node dies with the test binary.
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	require.NoError(t, cmd.Start(), "start node %d", n.idx)
	done := make(chan struct{})
	n.cmd, n.bin, n.done, n.err = cmd, bin, done, nil
	go func() {
		err := cmd.Wait()
		_ = logFile.Close()
		n.mu.Lock()
		n.err = err
		n.mu.Unlock()
		close(done)
	}()
}

// nodeEnv is the environment of the processes: this one without its
// NEXO_* variables, plus the credentials, the KMS key and, in a cluster,
// an internode protocol tuned to detect the stopped nodes in seconds.
func nodeEnv(cluster bool) []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "NEXO_") {
			env = append(env, kv)
		}
	}
	env = append(env, "NEXO_ROOT_USER="+rootUser, "NEXO_ROOT_PASSWORD="+rootPassword, "NEXO_KMS_SECRET_KEY="+kmsSecret())
	if cluster {
		env = append(env, "NEXO_LOCK_STALE_AFTER=6s", "NEXO_LOCK_REFRESH_INTERVAL=2s", "NEXO_RPC_CALL_TIMEOUT=5s")
	}
	return env
}

// signal sends sig to the process when it runs.
func (n *node) signal(sig syscall.Signal) {
	n.mu.Lock()
	cmd, done := n.cmd, n.done
	n.mu.Unlock()
	if cmd == nil {
		return
	}
	select {
	case <-done:
	default:
		_ = cmd.Process.Signal(sig)
	}
}

// wait waits up to timeout for the process to exit; it reports whether
// it did.
func (n *node) wait(timeout time.Duration) bool {
	n.mu.Lock()
	done := n.done
	n.mu.Unlock()
	if done == nil {
		return true
	}
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// kill kills the process and waits for it.
func (n *node) kill() {
	n.signal(syscall.SIGKILL)
	n.wait(stopTimeout)
}

// stop stops the process gracefully (SIGTERM), as an operator upgrading
// it does; a process that does not exit in time fails the test.
func (n *node) stop() {
	n.signal(syscall.SIGTERM)
	if !n.wait(stopTimeout) {
		n.d.t.Errorf("node %d did not stop within %s: killed", n.idx, stopTimeout)
		n.kill()
	}
}

// exited reports whether the process exited.
func (n *node) exited() bool {
	n.mu.Lock()
	done := n.done
	n.mu.Unlock()
	if done == nil {
		return false
	}
	select {
	case <-done:
		return true
	default:
		return false
	}
}

// exitErr is the exit status of an exited process.
func (n *node) exitErr() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.err
}

// probe returns the status of a GET of path on the S3 listener (0 when
// it does not answer within a second).
func (n *node) probe(path string) int {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.url()+path, nil)
	if err != nil {
		return 0
	}
	resp, err := n.d.pki.httpClient(time.Second).Do(req)
	if err != nil {
		return 0
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

// ready reports whether the node answers its readiness probe.
func (n *node) ready() bool { return n.probe(domain.HealthReadyPath) == http.StatusOK }

// waitReady waits until the node is ready; it fails the test when the
// process exits or is not ready in time.
func (n *node) waitReady() {
	n.d.t.Helper()
	if err := n.awaitReady(readyTimeout); err != nil {
		n.d.t.Fatal(err)
	}
}

// awaitReady waits until the node is ready, or reports why it is not.
func (n *node) awaitReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for !n.ready() {
		if n.exited() {
			return fmt.Errorf("node %d (%s) exited before it was ready: %v\n%s", n.idx, filepath.Base(n.bin), n.exitErr(), n.logTail(20))
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("node %d (%s) not ready after %s\n%s", n.idx, filepath.Base(n.bin), timeout, n.logTail(20))
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}

// clusterHealthy reports whether the node reports the write quorum of the
// whole cluster.
func (n *node) clusterHealthy() bool { return n.probe(domain.HealthClusterPath) == http.StatusOK }

// url is the base URL of the S3 listener of the node.
func (n *node) url() string { return "https://" + n.S3 }

// client returns a path-style S3 client of base ("https://host:port") for
// the credentials, with the standard retryer of the SDK allowed attempts
// tries (no client-side retry quota).
func (d *deployment) client(base, accessKey, secretKey string, attempts int) *s3.Client {
	return s3.New(s3.Options{
		Region:       region,
		Credentials:  credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""),
		BaseEndpoint: aws.String(base),
		UsePathStyle: true,
		HTTPClient:   d.pki.httpClient(2 * time.Minute),
		Retryer: retry.NewStandard(func(o *retry.StandardOptions) {
			o.MaxAttempts = attempts
			o.MaxBackoff = 2 * time.Second
			o.RateLimiter = ratelimit.None
		}),
	})
}

// rootClient returns a root S3 client of the node.
func (n *node) rootClient() *s3.Client {
	return n.d.client(n.url(), rootUser, rootPassword, 3)
}

// lbClient returns a root S3 client of the load balancer that retries
// for a while, as the SDK of an application does.
func (d *deployment) lbClient() *s3.Client {
	return d.client(d.LB.URL, rootUser, rootPassword, 10)
}

// admin returns an Admin API client of the node, signed as root.
func (n *node) admin() nexoadmin.AdminClient {
	n.d.t.Helper()
	a, err := nexoadmin.New(nexoadmin.Config{Endpoint: "https://" + n.Console, AccessKey: rootUser, SecretKey: rootPassword, Region: region,
		HTTPClient: n.d.pki.httpClient(time.Minute)})
	require.NoError(n.d.t, err)
	return a
}

// claimed are the ports handed out to the deployments of this test
// binary.
var claimed = struct {
	sync.Mutex
	ports map[int]bool
}{ports: map[int]bool{}}

// consecutivePorts returns the first of n consecutive free ports of
// 127.0.0.1, below the ephemeral range of the kernel (where the client
// connections take theirs).
func consecutivePorts(t *testing.T, n int) int {
	t.Helper()
	claimed.Lock()
	defer claimed.Unlock()
	for range 200 {
		base := 20000 + rand.IntN(12000) //nolint:gosec // a port number
		ok := true
		var lns []net.Listener
		for i := range n {
			if claimed.ports[base+i] {
				ok = false
				break
			}
			ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(base+i))
			if err != nil {
				ok = false
				break
			}
			lns = append(lns, ln)
		}
		for _, ln := range lns {
			_ = ln.Close()
		}
		if ok {
			for i := range n {
				claimed.ports[base+i] = true
			}
			return base
		}
	}
	t.Fatalf("no %d consecutive free ports", n)
	return 0
}

// waitFor polls check every 200ms until it returns nil or timeout
// elapses; the failure reports the last error of check.
func waitFor(t *testing.T, timeout time.Duration, what string, check func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		err := check()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %s: %v", what, timeout, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
