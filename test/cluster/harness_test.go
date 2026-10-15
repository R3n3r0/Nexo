//go:build cluster

package cluster_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	"github.com/r3n3r0/nexo/internal/service/storage"
	"github.com/r3n3r0/nexo/internal/service/topology"
	"github.com/r3n3r0/nexo/internal/testutil"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// Credentials and region of the test deployments.
const (
	rootUser     = "nexoadmin"
	rootPassword = "nexoadmin-secret"
	region       = "us-east-1"
)

// Timeouts of the harness.
const (
	readyTimeout = 90 * time.Second
	stopTimeout  = 20 * time.Second
)

// clusterEnv tunes the internode protocol of the test deployments so that
// the failure scenarios resolve in seconds: orphan locks expire after 6s
// (refreshed every 2s) and an internode call gives up after 5s. The
// distributions of the tests are reached through the S3 port, which names
// no S3 endpoint: the S3 listener serves them on opt-in.
var clusterEnv = map[string]string{
	"NEXO_LOCK_STALE_AFTER":      "6s",
	"NEXO_LOCK_REFRESH_INTERVAL": "2s",
	"NEXO_RPC_CALL_TIMEOUT":      "5s",
	"NEXO_S3_DISTRIBUTIONS":      "on",
}

// staleAfter is NEXO_LOCK_STALE_AFTER of clusterEnv.
const staleAfter = 6 * time.Second

// cluster is a deployment of bin/nexo processes on 127.0.0.1. Node i
// (1-based) listens on base+i for S3 and the internode RPC, has its own
// console port and runs in Dir/n<i>, its drives being Dir/n<i>/p<k>/d<j>
// for pool k. On Linux the endpoints use the ellipsis on the port and the
// per-process /proc/self/cwd, as one command line for every node:
//
//	http://127.0.0.1:{base+1...base+n}/proc/self/cwd/p0/d{1...m}
//
// elsewhere they list every drive (a single pool).
type cluster struct {
	t   *testing.T
	Dir string
	// base is the S3 port of node 0 minus one.
	base  int
	nodes []*node
	// pools are the drives per node of every pool.
	pools []int
	// LB is a round-robin load balancer with health checks in front of
	// the S3 listeners.
	LB *testutil.LoadBalancer
	// links are the proxies of the internode connections (withLinks):
	// links[i][j] carries the connections of node i+1 to node j+1.
	links [][]*link
	// withLinks is set by the withLinks option.
	withLinks bool
}

// node is one bin/nexo process of a cluster.
type node struct {
	c   *cluster
	idx int // 1-based
	// Dir is the working directory of the process (its drives are below).
	Dir     string
	S3      string // host:port
	Console string // host:port

	mu   sync.Mutex
	cmd  *exec.Cmd
	done chan struct{}
	err  error
	// args overrides the endpoints of the cluster (TC-02).
	args []string
	// env adds variables to the environment of the process (the test
	// hooks of bin/nexo: NEXO_TEST_CLOCK_OFFSET, NEXO_TEST_DIAL_MAP).
	env []string
	// bin overrides the server binary (TC-12).
	bin string
	// logStart is the size of the log when the current process started:
	// logLines reads the lines of this run only.
	logStart int64
}

// clusterOption customizes newCluster.
type clusterOption func(*cluster)

// withPools sets the drives per node of the pools (default one pool of 2
// drives per node).
func withPools(drives ...int) clusterOption {
	return func(c *cluster) { c.pools = drives }
}

// newCluster prepares a deployment of n nodes (nothing is started):
// consecutive free S3 ports, free console ports (both below the ephemeral
// range of the kernel) and the directories. The
// processes are killed when the test ends; their logs are printed when
// it failed.
func newCluster(t *testing.T, n int, opts ...clusterOption) *cluster {
	t.Helper()
	c := &cluster{t: t, Dir: clusterDir(t), pools: []int{2}}
	for _, o := range opts {
		o(c)
	}
	c.base = consecutivePorts(t, n) - 1
	// The console ports come from the same claimed range: a port of the
	// ephemeral range, free when probed, can be taken by an outgoing
	// connection (of any node or client) before its node binds it, and
	// the node exits with "address already in use".
	consoleBase := consecutivePorts(t, n)
	consoles := make([]int, n)
	for i := range consoles {
		consoles[i] = consoleBase + i
	}
	for i := 1; i <= n; i++ {
		nd := &node{c: c, idx: i, Dir: filepath.Join(c.Dir, "n"+strconv.Itoa(i)),
			S3: "127.0.0.1:" + strconv.Itoa(c.base+i), Console: "127.0.0.1:" + strconv.Itoa(consoles[i-1])}
		require.NoError(t, os.MkdirAll(nd.Dir, 0o755))
		c.nodes = append(c.nodes, nd)
	}
	if c.withLinks {
		c.startLinks()
	}
	c.LB = testutil.NewLoadBalancer(t, func() []string {
		out := make([]string, 0, len(c.nodes))
		for _, nd := range c.nodes {
			out = append(out, nd.S3)
		}
		return out
	}, testutil.LBHealthCheck(domain.HealthLivePath, 200*time.Millisecond))
	t.Cleanup(func() {
		for _, nd := range c.nodes {
			nd.signal(syscall.SIGCONT)
			nd.kill()
		}
		if t.Failed() || os.Getenv("NEXO_CLUSTER_LOGS") != "" {
			c.dumpLogs()
		}
	})
	return c
}

// clusterDir returns the directory of the drives of a test deployment:
// under NEXO_CLUSTER_DIR when set, else on the tmpfs of Linux
// (/dev/shm, when it has room: fast, and the pools are not excluded as
// full when the disk of the temporary directory is nearly full), else in
// the temporary directory of the test. It is removed when the test ends.
func clusterDir(t *testing.T) string {
	t.Helper()
	root := os.Getenv("NEXO_CLUSTER_DIR")
	if root == "" && runtime.GOOS == "linux" && roomy("/dev/shm") {
		root = "/dev/shm"
	}
	if root == "" {
		return t.TempDir()
	}
	dir, err := os.MkdirTemp(root, "nexo-cluster-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// node returns node i (1-based).
func (c *cluster) node(i int) *node { return c.nodes[i-1] }

// multiPool reports whether the endpoints can name several pools (the
// per-process /proc/self/cwd of Linux).
func multiPool() bool { return runtime.GOOS == "linux" }

// endpoints returns the drive arguments of the deployment.
func (c *cluster) endpoints() []string {
	n := len(c.nodes)
	if multiPool() {
		out := make([]string, 0, len(c.pools))
		for k, drives := range c.pools {
			out = append(out, fmt.Sprintf("http://127.0.0.1:{%d...%d}/proc/self/cwd/p%d/d{1...%d}", c.base+1, c.base+n, k, drives))
		}
		return out
	}
	if len(c.pools) != 1 {
		c.t.Fatalf("several pools need the per-process /proc/self/cwd of Linux")
	}
	var out []string
	for _, nd := range c.nodes {
		for j := 1; j <= c.pools[0]; j++ {
			out = append(out, fmt.Sprintf("http://%s%s/p0/d%d", nd.S3, filepath.ToSlash(nd.Dir), j))
		}
	}
	return out
}

// driveDirs returns the drive directories of node i in pool k.
func (c *cluster) driveDirs(i, k int) []string {
	var out []string
	for j := 1; j <= c.pools[k]; j++ {
		out = append(out, filepath.Join(c.node(i).Dir, "p"+strconv.Itoa(k), "d"+strconv.Itoa(j)))
	}
	return out
}

// start starts every node, in order, and waits until each is ready.
func (c *cluster) start() {
	c.t.Helper()
	for _, nd := range c.nodes {
		nd.start()
	}
	c.waitReady(c.nodes...)
}

// stop stops every node gracefully (SIGTERM), concurrently.
func (c *cluster) stop() {
	c.t.Helper()
	var wg sync.WaitGroup
	for _, nd := range c.nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			nd.stop()
		}()
	}
	wg.Wait()
}

// waitReady waits until every node answers its readiness probe.
func (c *cluster) waitReady(nodes ...*node) {
	c.t.Helper()
	deadline := time.Now().Add(readyTimeout)
	for _, nd := range nodes {
		for !nd.ready() {
			if nd.exited() {
				c.t.Fatalf("node %d exited before it was ready: %v", nd.idx, nd.exitErr())
			}
			if time.Now().After(deadline) {
				c.t.Fatalf("node %d not ready after %s", nd.idx, readyTimeout)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

// dumpLogs prints the end of the log of every node.
func (c *cluster) dumpLogs() {
	for _, nd := range c.nodes {
		data, err := os.ReadFile(nd.logPath())
		if err != nil {
			continue
		}
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		lines = slicesTail(lines, logTail())
		c.t.Logf("--- node %d (%s) log tail ---\n%s", nd.idx, nd.S3, strings.Join(lines, "\n"))
	}
}

// logTail is how many lines of every node log a failed test prints
// (NEXO_CLUSTER_LOG_LINES overrides it).
func logTail() int {
	if n, err := strconv.Atoi(os.Getenv("NEXO_CLUSTER_LOG_LINES")); err == nil && n > 0 {
		return n
	}
	return 60
}

func slicesTail(lines []string, n int) []string {
	if len(lines) > n {
		return lines[len(lines)-n:]
	}
	return lines
}

// client returns a path-style S3 client of base ("http://host:port") for
// the credentials, with the standard retryer of the SDK allowed attempts
// tries (no client-side retry quota: the failure scenarios exhaust it).
func client(base, accessKey, secretKey string, attempts int, timeout time.Duration) *s3.Client {
	return s3.New(s3.Options{
		Region:       region,
		Credentials:  credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""),
		BaseEndpoint: aws.String(base),
		UsePathStyle: true,
		HTTPClient:   &http.Client{Timeout: timeout},
		Retryer: retry.NewStandard(func(o *retry.StandardOptions) {
			o.MaxAttempts = attempts
			o.MaxBackoff = time.Second
			o.RateLimiter = ratelimit.None
		}),
	})
}

// rootClient returns a root S3 client of the node, with the default
// retries of the SDK.
func (n *node) rootClient() *s3.Client {
	return client("http://"+n.S3, rootUser, rootPassword, 3, time.Minute)
}

// lbClient returns a root S3 client of the load balancer that retries
// for a while: the client of an application in front of a cluster that
// loses a node.
func (c *cluster) lbClient() *s3.Client {
	return client(c.LB.URL, rootUser, rootPassword, 20, time.Minute)
}

// admin returns an Admin API client of the node, signed as root.
func (n *node) admin() nexoadmin.AdminClient {
	n.c.t.Helper()
	a, err := nexoadmin.New(nexoadmin.Config{Endpoint: "http://" + n.Console, AccessKey: rootUser, SecretKey: rootPassword, Region: region})
	require.NoError(n.c.t, err)
	return a
}

// deploymentID returns the deployment id of the format.json of the first
// drive of node i.
func (c *cluster) deploymentID(i int) string {
	c.t.Helper()
	data, err := os.ReadFile(filepath.Join(c.driveDirs(i, 0)[0], domain.SystemBucket, storage.FormatFile))
	require.NoError(c.t, err)
	f, err := topology.UnmarshalFormat(data)
	require.NoError(c.t, err)
	return f.ID
}

// logPath is the log file of the node (every run appends to it).
func (n *node) logPath() string { return filepath.Join(n.Dir, "nexo.log") }

// start launches the process (it does not wait for it to be ready).
func (n *node) start() {
	t := n.c.t
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
	if st, err := logFile.Stat(); err == nil {
		n.logStart = st.Size()
	}
	args := []string{"server", "--address", n.S3, "--console-address", n.Console, "--log-format", "text"}
	if n.args != nil {
		args = append(args, n.args...)
	} else {
		args = append(args, n.c.endpoints()...)
	}
	bin := nexoBin
	if n.bin != "" {
		bin = n.bin
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = n.Dir
	cmd.Env = append(nodeEnv(), n.env...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = sysProcAttr()
	require.NoError(t, cmd.Start(), "start node %d", n.idx)
	done := make(chan struct{})
	n.cmd, n.done, n.err = cmd, done, nil
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
// NEXO_* variables, plus the credentials and clusterEnv.
func nodeEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "NEXO_") {
			env = append(env, kv)
		}
	}
	env = append(env, "NEXO_ROOT_USER="+rootUser, "NEXO_ROOT_PASSWORD="+rootPassword)
	for k, v := range clusterEnv {
		env = append(env, k+"="+v)
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

// wait waits for the process to exit, up to timeout; it reports whether
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

// kill kills the process (SIGKILL: a crash) and waits for it.
func (n *node) kill() {
	n.signal(syscall.SIGKILL)
	n.wait(stopTimeout)
}

// stop stops the process gracefully (SIGTERM), killing it when it does
// not exit in time.
func (n *node) stop() {
	n.signal(syscall.SIGTERM)
	if !n.wait(stopTimeout) {
		n.c.t.Errorf("node %d did not stop within %s: killed", n.idx, stopTimeout)
		n.kill()
	}
}

// pause freezes the process (SIGSTOP): it holds its connections but
// answers nothing, like a node behind a network partition.
func (n *node) pause() { n.signal(syscall.SIGSTOP) }

// resume resumes a paused process.
func (n *node) resume() { n.signal(syscall.SIGCONT) }

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

// probe returns the status of a GET of path on the S3 listener of the
// node (0 when it does not answer within a second).
func (n *node) probe(path string) int {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+n.S3+path, nil)
	if err != nil {
		return 0
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

// ready reports whether the node answers its readiness probe.
func (n *node) ready() bool { return n.probe(domain.HealthReadyPath) == http.StatusOK }

// logLines returns the lines of the log of the current (or last) run of
// the node containing substr.
func (n *node) logLines(substr string) []string {
	n.mu.Lock()
	offset := n.logStart
	n.mu.Unlock()
	f, err := os.Open(n.logPath())
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil
	}
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		if line := sc.Text(); strings.Contains(line, substr) {
			out = append(out, line)
		}
	}
	return out
}

// claimed are the S3 ports handed out to the deployments of this test
// binary: the tests running in parallel never share one (a port is free
// between its probe and the start of its node).
var claimed = struct {
	sync.Mutex
	ports map[int]bool
}{ports: map[int]bool{}}

// consecutivePorts returns the first of n consecutive free ports of
// 127.0.0.1 (the ellipsis of the endpoints spans them).
func consecutivePorts(t *testing.T, n int) int {
	t.Helper()
	claimed.Lock()
	defer claimed.Unlock()
	for range 200 {
		// Below the ephemeral range of the kernel (32768+ on Linux), where
		// the console and client ports are taken.
		base := 20000 + rand.IntN(12000)
		var lns []net.Listener
		ok := true
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
			t.Fatalf("%s: not done after %s: %v", what, timeout, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// timed logs how long fn took.
func timed(t *testing.T, what string, fn func()) {
	t.Helper()
	start := time.Now()
	fn()
	t.Logf("%s: %s", what, time.Since(start).Round(time.Millisecond))
}

// eventually polls cond every 200ms until it holds or timeout elapses.
func eventually(t *testing.T, timeout time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not true after %s", msg, timeout)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// mustCreateBucket creates a bucket.
func mustCreateBucket(t *testing.T, c *s3.Client, bucket string) {
	t.Helper()
	_, err := c.CreateBucket(context.Background(), &s3.CreateBucketInput{Bucket: aws.String(bucket)})
	require.NoError(t, err)
}

// put writes data under key.
func put(ctx context.Context, c *s3.Client, bucket, key string, data []byte) error {
	_, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(data)})
	return err
}

// bodyAttempts is how many times get reads an object whose body is cut
// (the node streaming it crashed): the retries of the SDK end with the
// response headers, the application retries the download.
const bodyAttempts = 3

// get reads key; found is false when it does not exist.
func get(ctx context.Context, c *s3.Client, bucket, key string) (data []byte, found bool, err error) {
	for attempt := 1; ; attempt++ {
		out, err := c.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
		if err != nil {
			if isNotFound(err) {
				return nil, false, nil
			}
			return nil, false, err
		}
		data, err = io.ReadAll(out.Body)
		_ = out.Body.Close()
		if err == nil {
			return data, true, nil
		}
		if attempt == bodyAttempts || ctx.Err() != nil {
			return nil, false, fmt.Errorf("read the body (attempt %d): %w", attempt, err)
		}
	}
}

// isNotFound reports a NoSuchKey (or 404) answer.
func isNotFound(err error) bool {
	var nsk interface{ ErrorCode() string }
	if errors.As(err, &nsk) {
		switch nsk.ErrorCode() {
		case "NoSuchKey", "NotFound":
			return true
		}
	}
	var re interface{ HTTPStatusCode() int }
	return errors.As(err, &re) && re.HTTPStatusCode() == http.StatusNotFound
}

// objectPath returns the directory of key in bucket on a drive.
func objectPath(drive, bucket, key string) string {
	return filepath.Join(drive, bucket, filepath.FromSlash(key))
}

// hasMeta reports whether the drive holds the xl.meta of key.
func hasMeta(drive, bucket, key string) bool {
	_, err := os.Stat(filepath.Join(objectPath(drive, bucket, key), "xl.meta"))
	return err == nil
}
