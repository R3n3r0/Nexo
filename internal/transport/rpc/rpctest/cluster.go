// Package rpctest runs a distributed deployment inside the test process:
// N nodes on 127.0.0.1 with their own port, drives in a temporary
// directory, internode RPC over cleartext HTTP/2 and the cluster services
// (bootstrap, distributed locks, notifier, leader, info) wired like the
// composition root wires them. Links between nodes can be cut (crashes,
// partitions) or slowed down (latency) to exercise the failure scenarios
// of docs/03 §3.11.
package rpctest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/cluster"
	"github.com/r3n3r0/nexo/internal/service/lock"
	"github.com/r3n3r0/nexo/internal/service/storage"
	"github.com/r3n3r0/nexo/internal/service/topology"
	"github.com/r3n3r0/nexo/internal/transport/rpc"
)

// Test credentials of the deployment.
const (
	RootUser     = "nexoadmin"
	RootPassword = "nexoadmin-secret"
)

// Options shape the deployment. Zero values select the defaults.
type Options struct {
	// Nodes and DrivesPerNode (default 4 x 2).
	Nodes         int
	DrivesPerNode int
	// ArgsOf returns the server arguments node n (0-based) starts with,
	// given those of the deployment: a node started with another topology.
	ArgsOf func(n int, args []string) []string
	// Versions replaces the build version of some nodes.
	Versions map[int]string
	// Clocks replaces the clock of some nodes (clock skew).
	Clocks map[int]clock.Clock
	// Latency is added to every internode call.
	Latency time.Duration
	// LockStaleAfter and LockRefresh are the lease of the distributed
	// locks (default 1s and 100ms: orphans expire fast).
	LockStaleAfter time.Duration
	LockRefresh    time.Duration
	Logger         *slog.Logger
}

// Node is one server of the deployment and its services.
type Node struct {
	Index int
	// ID is "127.0.0.1:port", the node key of the topology.
	ID     string
	Port   int
	Args   []string
	Layout *topology.Layout
	Auth   rpc.Authenticator
	// Conns reach the other nodes, by node id.
	Conns map[string]rpc.Conn
	// LocalDrives are the drives of this node; Drives every drive of the
	// layout in pool order, the remote ones through the RPC client.
	LocalDrives  []storage.StorageAPI
	Drives       []storage.StorageAPI
	LockServer   lock.Server
	Locker       lock.Locker
	LockManager  lock.Manager
	Notifier     cluster.Notifier
	Info         cluster.Info
	Bootstrapper cluster.Bootstrapper
	Leader       cluster.Leader
	Server       rpc.Server

	c       *Cluster
	mu      sync.Mutex
	srv     *http.Server
	started bool
}

// Cluster is the in-process deployment.
type Cluster struct {
	T     testing.TB
	Base  string
	Args  []string
	Nodes []*Node
	opts  Options

	// cut[from][to] blocks the calls of node from to node to.
	cut     [][]atomic.Bool
	latency atomic.Int64
}

// New builds the deployment; no node serves until Start.
func New(t testing.TB, opts Options) *Cluster {
	t.Helper()
	if opts.Nodes == 0 {
		opts.Nodes = 4
	}
	if opts.DrivesPerNode == 0 {
		opts.DrivesPerNode = 2
	}
	if opts.LockStaleAfter == 0 {
		opts.LockStaleAfter = time.Second
	}
	if opts.LockRefresh == 0 {
		opts.LockRefresh = 100 * time.Millisecond
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	c := &Cluster{T: t, Base: t.TempDir(), opts: opts}
	c.latency.Store(int64(opts.Latency))
	c.cut = make([][]atomic.Bool, opts.Nodes)
	for i := range c.cut {
		c.cut[i] = make([]atomic.Bool, opts.Nodes)
	}
	ports := freePorts(t, opts.Nodes)
	for n, port := range ports {
		for d := 1; d <= opts.DrivesPerNode; d++ {
			c.Args = append(c.Args, fmt.Sprintf("http://127.0.0.1:%d%s/n%d/d%d", port, filepath.ToSlash(c.Base), n+1, d))
		}
	}
	for n, port := range ports {
		c.Nodes = append(c.Nodes, c.newNode(n, port, ports))
	}
	t.Cleanup(c.close)
	return c
}

// freePorts reserves distinct loopback ports.
func freePorts(t testing.TB, n int) []int {
	t.Helper()
	var listeners []net.Listener
	var ports []int
	for range n {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("rpctest: reserve port: %v", err)
		}
		listeners = append(listeners, ln)
		ports = append(ports, ln.Addr().(*net.TCPAddr).Port)
	}
	for _, ln := range listeners {
		_ = ln.Close()
	}
	return ports
}

func nodeID(port int) string { return fmt.Sprintf("127.0.0.1:%d", port) }

// newNode wires the services of node n like the composition root.
func (c *Cluster) newNode(n, port int, ports []int) *Node {
	t := c.T
	t.Helper()
	ctx := context.Background()
	logger := c.opts.Logger.With("node", n+1)
	args := c.Args
	if c.opts.ArgsOf != nil {
		args = c.opts.ArgsOf(n, append([]string(nil), c.Args...))
	}
	topo := topology.New(topology.Deps{Logger: logger}, topology.WithLocalPort(port), topology.WithDefaultPort(port))
	layout, err := topo.Parse(ctx, args)
	must(t, err, "parse topology")
	version := "v1.0.0"
	if v, ok := c.opts.Versions[n]; ok {
		version = v
	}
	clk := c.opts.Clocks[n]
	if clk == nil {
		clk = clock.NewReal()
	}
	node := &Node{c: c, Index: n, ID: nodeID(port), Port: port, Args: args, Layout: layout, Conns: map[string]rpc.Conn{}}
	node.Auth, err = rpc.NewAuthenticator(rpc.AuthDeps{Logger: logger},
		rpc.WithCredentials(RootUser, RootPassword), rpc.WithNodeID(node.ID), rpc.WithBuildVersion(version))
	must(t, err, "authenticator")

	// Connections to every other node, through the fault injector.
	for m, other := range ports {
		if m == n {
			continue
		}
		conn, err := rpc.NewConn(rpc.ConnDeps{Auth: node.Auth, Logger: logger}, nodeID(other),
			rpc.WithHealthInterval(20*time.Millisecond, 200*time.Millisecond), rpc.WithCallTimeout(5*time.Second),
			rpc.WithDialTimeout(time.Second),
			rpc.WithTransportWrapper(func(rt http.RoundTripper) http.RoundTripper { return &link{c: c, from: n, to: m, next: rt} }))
		must(t, err, "connection")
		node.Conns[nodeID(other)] = conn
	}

	// Drives: local ones opened here, remote ones through their node.
	for _, ep := range layout.Endpoints() {
		var d storage.StorageAPI
		if ep.IsLocal {
			d, err = storage.NewLocal(storage.Deps{Clock: clk, Logger: logger}, ep, storage.WithoutFsync())
			must(t, err, "open drive")
			node.LocalDrives = append(node.LocalDrives, d)
		} else {
			d, err = rpc.NewStorageClient(rpc.StorageClientDeps{Conn: node.Conns[ep.Host], Logger: logger}, ep,
				rpc.WithDiskCheckInterval(50*time.Millisecond))
			must(t, err, "remote drive")
		}
		node.Drives = append(node.Drives, d)
	}

	// Locks: the local server and the servers of the other nodes.
	node.LockServer = lock.NewServer(lock.ServerDeps{Clock: clk, Logger: logger}, lock.WithStaleAfter(c.opts.LockStaleAfter))
	servers := []lock.Server{node.LockServer}
	var peers []cluster.Peer
	var bootPeers []cluster.BootstrapPeer
	for _, other := range ports {
		conn, ok := node.Conns[nodeID(other)]
		if !ok {
			continue
		}
		ls, err := rpc.NewLockClient(rpc.LockClientDeps{Conn: conn})
		must(t, err, "lock client")
		servers = append(servers, ls)
		p, err := rpc.NewPeerClient(rpc.PeerClientDeps{Conn: conn})
		must(t, err, "peer client")
		peers = append(peers, p)
		bp, err := rpc.NewBootstrapClient(rpc.BootstrapClientDeps{Conn: conn})
		must(t, err, "bootstrap client")
		bootPeers = append(bootPeers, bp)
	}
	node.Locker, err = lock.NewDistributed(lock.DistributedDeps{Servers: servers, Owner: node.ID, Logger: logger},
		lock.WithRefreshInterval(c.opts.LockRefresh), lock.WithCallTimeout(time.Second),
		lock.WithRetryInterval(10*time.Millisecond, 50*time.Millisecond))
	must(t, err, "distributed locker")
	node.LockManager = lock.NewManager(lock.ManagerDeps{Servers: servers, Logger: logger})

	// Cluster services.
	node.Notifier = cluster.NewNotifier(cluster.NotifierDeps{NodeID: node.ID, Peers: peers, Clock: clk, Logger: logger},
		cluster.WithRetryInterval(50*time.Millisecond), cluster.WithNotifyTimeout(2*time.Second))
	node.Info = cluster.NewInfo(cluster.InfoDeps{NodeID: node.ID, Nodes: cluster.NodesOf(layout), Drives: node.LocalDrives,
		Peers: peers, Clock: clk, Logger: logger}, cluster.WithBuildInfo(version, "test"))
	if id := cluster.FormattedDeploymentID(ctx, node.LocalDrives); id != "" {
		node.Auth.SetDeploymentID(ctx, id)
	}
	node.Bootstrapper, err = cluster.NewBootstrapper(cluster.BootstrapDeps{Layout: layout, Topology: topo, Drives: node.Drives,
		Peers: bootPeers, Binder: node.Auth, Clock: clk, Logger: logger},
		cluster.WithBootstrapBackoff(20*time.Millisecond, 200*time.Millisecond), cluster.WithBootstrapVersion(version),
		cluster.WithBootstrapTimeout(2*time.Second))
	must(t, err, "bootstrapper")
	node.Leader, err = cluster.NewLeader(cluster.LeaderDeps{Locker: node.Locker, NodeID: node.ID, Logger: logger},
		cluster.WithElectionInterval(100*time.Millisecond), cluster.WithLeaderAcquireTimeout(100*time.Millisecond))
	must(t, err, "leader")

	// The RPC server, in front of a placeholder S3 API.
	storageRoutes, err := rpc.NewStorageRoutes(rpc.StorageRoutesDeps{Drives: node.LocalDrives, Auth: node.Auth})
	must(t, err, "storage routes")
	lockRoutes, err := rpc.NewLockRoutes(rpc.LockRoutesDeps{Server: node.LockServer})
	must(t, err, "lock routes")
	peerRoutes, err := rpc.NewPeerRoutes(rpc.PeerRoutesDeps{Notifier: node.Notifier, Info: node.Info})
	must(t, err, "peer routes")
	bootRoutes, err := rpc.NewBootstrapRoutes(rpc.BootstrapRoutesDeps{Bootstrapper: node.Bootstrapper})
	must(t, err, "bootstrap routes")
	node.Server, err = rpc.NewServer(rpc.ServerDeps{Auth: node.Auth, Clock: clk, Logger: logger,
		Routes: []rpc.Routes{storageRoutes, lockRoutes, peerRoutes, bootRoutes}})
	must(t, err, "rpc server")
	return node
}

func must(t testing.TB, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("rpctest: %s: %v", what, err)
	}
}

// S3Placeholder answers the requests outside of the RPC prefix.
const S3Placeholder = "s3 api"

// Start makes the node serve on its port (the S3 port of a real node).
func (n *Node) Start() {
	n.c.T.Helper()
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.started {
		return
	}
	var ln net.Listener
	var err error
	for range 100 {
		if ln, err = net.Listen("tcp", n.ID); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	must(n.c.T, err, "listen")
	s3 := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(S3Placeholder)) })
	// HTTP/1 and cleartext HTTP/2 served by net/http, like the S3 server.
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	n.srv = &http.Server{
		Handler:           n.Server.Wrap(s3),
		Protocols:         &protocols,
		HTTP2:             &http.HTTP2Config{MaxReceiveBufferPerStream: 16 << 20, MaxReceiveBufferPerConnection: 64 << 20, MaxReadFrameSize: 1 << 20},
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func(srv *http.Server) { _ = srv.Serve(ln) }(n.srv)
	n.started = true
	for m := range n.c.Nodes {
		n.c.cut[n.Index][m].Store(false)
		n.c.cut[m][n.Index].Store(false)
	}
}

// Crash stops the node abruptly: it stops serving, nobody reaches it and
// its calls to the other nodes fail (its goroutines keep running,
// isolated, like a process frozen or cut from the network). Its locks are
// released by nobody.
func (n *Node) Crash() {
	n.mu.Lock()
	defer n.mu.Unlock()
	for m := range n.c.Nodes {
		n.c.cut[n.Index][m].Store(true)
		n.c.cut[m][n.Index].Store(true)
	}
	if n.srv != nil {
		_ = n.srv.Close()
		n.srv = nil
	}
	n.started = false
}

// Partition cuts the links between the groups of node indexes: nodes of
// different groups cannot call each other.
func (c *Cluster) Partition(groups ...[]int) {
	group := map[int]int{}
	for g, members := range groups {
		for _, m := range members {
			group[m] = g
		}
	}
	for from := range c.Nodes {
		for to := range c.Nodes {
			gf, okF := group[from]
			gt, okT := group[to]
			c.cut[from][to].Store(okF && okT && gf != gt)
		}
	}
}

// Heal restores every link between running nodes.
func (c *Cluster) Heal() { c.Partition() }

// SetLatency changes the latency added to every internode call.
func (c *Cluster) SetLatency(d time.Duration) { c.latency.Store(int64(d)) }

// StartAll starts every node.
func (c *Cluster) StartAll() {
	for _, n := range c.Nodes {
		n.Start()
	}
}

// Bootstrap runs the bootstrap of the node and binds its authentication
// to the deployment, like the composition root.
func (n *Node) Bootstrap(ctx context.Context) (cluster.BootstrapResult, error) {
	res, err := n.Bootstrapper.Bootstrap(ctx)
	if err == nil {
		n.Auth.SetDeploymentID(ctx, res.DeploymentID)
	}
	return res, err
}

// close releases every node.
func (c *Cluster) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, n := range c.Nodes {
		_ = n.Leader.Stop(ctx)
		_ = n.Notifier.Stop(ctx)
	}
	for _, n := range c.Nodes {
		n.Crash()
		for _, d := range n.Drives {
			_ = d.Close(ctx)
		}
		for _, conn := range n.Conns {
			_ = conn.Close(ctx)
		}
	}
}

// errLinkCut is the network failure of a cut link.
var errLinkCut = errors.New("rpctest: link cut")

// link is the fault injector between two nodes.
type link struct {
	c        *Cluster
	from, to int
	next     http.RoundTripper
}

func (l *link) RoundTrip(req *http.Request) (*http.Response, error) {
	if d := time.Duration(l.c.latency.Load()); d > 0 {
		select {
		case <-time.After(d):
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}
	if l.c.cut[l.from][l.to].Load() {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errLinkCut}
	}
	return l.next.RoundTrip(req)
}

// Endpoint returns the endpoint of drive d (1-based) of node n (0-based)
// in the default arguments.
func (c *Cluster) Endpoint(n, d int) domain.Endpoint {
	ep, _ := domain.ParseEndpoint(fmt.Sprintf("http://%s%s/n%d/d%d", c.Nodes[n].ID, filepath.ToSlash(c.Base), n+1, d))
	return ep
}
