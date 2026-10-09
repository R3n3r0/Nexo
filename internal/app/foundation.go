package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/config"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/cluster"
	"github.com/r3n3r0/nexo/internal/service/heal"
	"github.com/r3n3r0/nexo/internal/service/lock"
	"github.com/r3n3r0/nexo/internal/service/metrics"
	"github.com/r3n3r0/nexo/internal/service/storage"
	"github.com/r3n3r0/nexo/internal/service/topology"
	"github.com/r3n3r0/nexo/internal/transport/http/s3"
	"github.com/r3n3r0/nexo/internal/transport/rpc"
	"github.com/r3n3r0/nexo/internal/version"
)

// foundation is the mode-dependent ground of the application (docs/08
// §8.1): the drives, the locks and the cluster services. A single-node
// deployment (SNSD, SNMD) opens its local drives and uses the local locker
// and the single-node cluster services; a distributed one also reaches
// the drives, lock servers and peers of the other nodes through the
// internode RPC, served on the S3 port.
type foundation struct {
	layout *topology.Layout
	topo   topology.Service
	// nodeID is "host:port" of this node in a cluster, "local" otherwise.
	nodeID string
	// local are the drives of this node; drives every drive of the layout
	// in pool order, the remote ones through their node (the same as
	// local in a single-node deployment).
	local  []storage.StorageAPI
	drives []storage.StorageAPI

	// mrf is the background healing queue of this node, persisted on its
	// local drives (erasure-coded modes only: nil in SNSD, where a single
	// drive has nothing to heal from). It is built with the drives so that
	// the node information published to the peers reports it.
	mrf heal.Queue

	locks        lock.Locker
	lockManager  lock.Manager
	notifier     cluster.Notifier
	leader       cluster.Leader
	info         cluster.Info
	bootstrapper cluster.Bootstrapper

	// registry is the metrics registry of the node: the internode RPC
	// server counts its refusals there before the services are composed.
	registry metrics.Registry

	// Distributed deployments only: the internode authentication, the RPC
	// server mounted in front of the S3 API and the connections to the
	// other nodes.
	auth  rpc.Authenticator
	rpc   rpc.Server
	conns []rpc.Conn
}

// distributed reports a cluster deployment.
func (f *foundation) distributed() bool { return f.layout.Mode == domain.ModeDistributed }

// close releases the drives and the connections.
func (f *foundation) close(ctx context.Context) error {
	var errs []error
	for _, d := range f.drives {
		if err := d.Close(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	for _, c := range f.conns {
		if err := c.Close(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// poolSets returns the drives of every erasure set of every pool, matched
// to the layout by endpoint.
func (f *foundation) poolSets() ([][][]storage.StorageAPI, error) {
	byEndpoint := make(map[string]storage.StorageAPI, len(f.drives))
	for _, d := range f.drives {
		byEndpoint[d.Endpoint().String()] = d
	}
	out := make([][][]storage.StorageAPI, 0, len(f.layout.Pools))
	for _, p := range f.layout.Pools {
		sets := make([][]storage.StorageAPI, 0, p.SetCount)
		for _, eps := range p.Sets() {
			set := make([]storage.StorageAPI, 0, len(eps))
			for _, ep := range eps {
				d, ok := byEndpoint[ep.String()]
				if !ok {
					return nil, fmt.Errorf("%w: no drive for endpoint %s", domain.ErrInvalidLayout, ep)
				}
				set = append(set, d)
			}
			sets = append(sets, set)
		}
		out = append(out, sets)
	}
	return out, nil
}

// foundationDeps are the inputs of newFoundation.
type foundationDeps struct {
	cfg    config.Static
	layout *topology.Layout
	topo   topology.Service
	clock  clock.Clock
	logger *slog.Logger
	// noFsync opens the local drives without fsync (WithoutFsync).
	noFsync bool
	// dial opens the internode connections (WithInternodeDialer).
	dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// driveOptions are the options of the local drives.
func (d foundationDeps) driveOptions() []storage.Option {
	if d.noFsync {
		return []storage.Option{storage.WithoutFsync()}
	}
	return nil
}

// newFoundation opens the drives and builds the locks and the cluster
// services of the deployment mode. On error everything opened so far is
// released.
func newFoundation(d foundationDeps) (f *foundation, err error) {
	f = &foundation{layout: d.layout, topo: d.topo, nodeID: cluster.LocalNodeID, registry: metrics.New(metrics.Deps{Logger: d.logger})}
	defer func() {
		if err != nil {
			_ = f.close(context.Background())
		}
	}()
	if f.distributed() {
		err = f.openDistributed(d)
	} else {
		err = f.openLocal(d)
	}
	if err != nil {
		return nil, err
	}
	return f, nil
}

// openLocal builds the single-node foundation: local drives, the local
// locker, the formatter-based bootstrap and the single-node cluster
// services (no RPC is served).
func (f *foundation) openLocal(d foundationDeps) error {
	for _, ep := range d.layout.Endpoints() {
		drive, err := storage.NewLocal(storage.Deps{Clock: d.clock, Logger: d.logger}, ep, d.driveOptions()...)
		if err != nil {
			return fmt.Errorf("open drive %s: %w", ep, err)
		}
		f.local = append(f.local, drive)
		f.drives = append(f.drives, drive)
	}
	if len(f.local) == 0 {
		return fmt.Errorf("%w: no local drives in the deployment", domain.ErrInvalidArgument)
	}
	f.mrf = newHealQueue(d.layout.Mode, f.local, d.clock, d.logger, d.cfg.HealRetryMaxDelay)
	f.locks = lock.NewLocal(lock.Deps{})
	f.lockManager = lock.NewManager(lock.ManagerDeps{Logger: d.logger})
	f.notifier = cluster.NewLocalNotifier(cluster.NotifierDeps{NodeID: f.nodeID, Clock: d.clock, Logger: d.logger})
	f.leader = cluster.NewLocalLeader(cluster.LeaderDeps{NodeID: f.nodeID, Logger: d.logger})
	build := version.Get()
	f.info = cluster.NewInfo(cluster.InfoDeps{NodeID: f.nodeID, Nodes: cluster.NodesOf(d.layout), Drives: f.local,
		HealQueue: healQueueReporter(f.mrf), Clock: d.clock, Logger: d.logger},
		cluster.WithBuildInfo(build.Version, build.Commit))
	var err error
	formatter := storage.NewFormatter(storage.FormatterDeps{Topology: d.topo, Logger: d.logger})
	f.bootstrapper, err = cluster.NewLocalBootstrapper(cluster.BootstrapDeps{
		Layout: d.layout, Topology: d.topo, Formatter: formatter, Drives: f.local, Clock: d.clock, Logger: d.logger,
	}, cluster.WithBootstrapVersion(build.Version))
	return err
}

// Defaults of the internode protocol (config.Static overrides them).
const (
	defaultLockStaleAfter = time.Minute
	defaultLockRefresh    = 10 * time.Second
	defaultRPCCallTimeout = 30 * time.Second
)

// openDistributed builds the foundation of a cluster node: the local
// drives, an RPC client for every drive of the other nodes, the
// distributed locker (dsync) over the lock servers of every node, the
// notifier, leader election, server info and bootstrap over the peers, and
// the RPC server exposing this node to them.
func (f *foundation) openDistributed(d foundationDeps) error {
	cfg := d.cfg
	local, ok := cluster.LocalNode(cluster.NodesOf(d.layout))
	if !ok {
		return fmt.Errorf("%w: no node of the layout is local", domain.ErrInvalidLayout)
	}
	f.nodeID = local.ID
	build := version.Get()
	logger := d.logger.With("node", f.nodeID)
	var err error
	f.auth, err = rpc.NewAuthenticator(rpc.AuthDeps{Logger: logger},
		rpc.WithCredentials(cfg.RootUser, cfg.RootPassword), rpc.WithNodeID(f.nodeID), rpc.WithBuildVersion(build.Version))
	if err != nil {
		return err
	}
	tlsCfg, err := internodeTLS(cfg, d.layout)
	if err != nil {
		return err
	}
	if tlsCfg == nil {
		logger.Warn("internode traffic and tokens in cleartext: use https endpoints (TLS) unless the network between the nodes is trusted",
			"nodes", len(d.layout.Nodes))
	}
	callTimeout := cmpOr(cfg.RPCCallTimeout, defaultRPCCallTimeout)
	conns := map[string]rpc.Conn{}
	var peers []cluster.Peer
	var bootPeers []cluster.BootstrapPeer
	lockServer := lock.NewServer(lock.ServerDeps{Clock: d.clock, Logger: logger}, lock.WithStaleAfter(cmpOr(cfg.LockStaleAfter, defaultLockStaleAfter)))
	servers := []lock.Server{lockServer}
	for _, n := range d.layout.Nodes {
		id := n.String()
		if n.Local {
			continue
		}
		opts := []rpc.ConnOption{rpc.WithCallTimeout(callTimeout)}
		if tlsCfg != nil {
			opts = append(opts, rpc.WithTLSConfig(tlsCfg))
		}
		if d.dial != nil {
			opts = append(opts, rpc.WithDialer(d.dial))
		}
		conn, err := rpc.NewConn(rpc.ConnDeps{Auth: f.auth, Logger: logger}, id, opts...)
		if err != nil {
			return err
		}
		f.conns = append(f.conns, conn)
		conns[id] = conn
		ls, err := rpc.NewLockClient(rpc.LockClientDeps{Conn: conn})
		if err != nil {
			return err
		}
		servers = append(servers, ls)
		p, err := rpc.NewPeerClient(rpc.PeerClientDeps{Conn: conn})
		if err != nil {
			return err
		}
		peers = append(peers, p)
		bp, err := rpc.NewBootstrapClient(rpc.BootstrapClientDeps{Conn: conn})
		if err != nil {
			return err
		}
		bootPeers = append(bootPeers, bp)
	}

	// Drives: local ones opened here, remote ones through their node.
	for _, ep := range d.layout.Endpoints() {
		var drive storage.StorageAPI
		if ep.IsLocal {
			drive, err = storage.NewLocal(storage.Deps{Clock: d.clock, Logger: logger}, ep, d.driveOptions()...)
			if err != nil {
				return fmt.Errorf("open drive %s: %w", ep, err)
			}
			f.local = append(f.local, drive)
		} else {
			conn, ok := conns[ep.Host]
			if !ok {
				return fmt.Errorf("%w: no node for drive %s", domain.ErrInvalidLayout, ep)
			}
			drive, err = rpc.NewStorageClient(rpc.StorageClientDeps{Conn: conn, Logger: logger}, ep)
			if err != nil {
				return err
			}
		}
		f.drives = append(f.drives, drive)
	}

	// A node whose drives are formatted knows its deployment before it
	// accepts any call: the callers of another deployment are refused
	// even while the peers are away and the bootstrap waits for them.
	if id := cluster.FormattedDeploymentID(context.Background(), f.local); id != "" {
		f.auth.SetDeploymentID(context.Background(), id)
	}

	f.mrf = newHealQueue(d.layout.Mode, f.local, d.clock, logger, cfg.HealRetryMaxDelay)

	f.locks, err = lock.NewDistributed(lock.DistributedDeps{Servers: servers, Owner: f.nodeID, Logger: logger},
		lock.WithRefreshInterval(cmpOr(cfg.LockRefreshInterval, defaultLockRefresh)))
	if err != nil {
		return err
	}
	f.lockManager = lock.NewManager(lock.ManagerDeps{Servers: servers, Logger: logger})
	f.notifier = cluster.NewNotifier(cluster.NotifierDeps{NodeID: f.nodeID, Peers: peers, Clock: d.clock, Logger: logger},
		cluster.WithResyncInterval(cmpOr(cfg.CacheResyncInterval, cluster.DefaultResyncInterval)))
	f.info = cluster.NewInfo(cluster.InfoDeps{NodeID: f.nodeID, Nodes: cluster.NodesOf(d.layout), Drives: f.local, Peers: peers,
		HealQueue: healQueueReporter(f.mrf), Clock: d.clock, Logger: logger},
		cluster.WithBuildInfo(build.Version, build.Commit))
	kmsFP, err := kmsFingerprint(cfg)
	if err != nil {
		return err
	}
	f.bootstrapper, err = cluster.NewBootstrapper(cluster.BootstrapDeps{Layout: d.layout, Topology: d.topo, Drives: f.drives, Peers: bootPeers,
		Binder: f.auth, Clock: d.clock, Logger: logger},
		cluster.WithBootstrapVersion(build.Version), cluster.WithBootstrapKMSFingerprint(kmsFP))
	if err != nil {
		return err
	}
	f.leader, err = cluster.NewLeader(cluster.LeaderDeps{Locker: f.locks, NodeID: f.nodeID, Logger: logger})
	if err != nil {
		return err
	}

	storageRoutes, err := rpc.NewStorageRoutes(rpc.StorageRoutesDeps{Drives: f.local, Auth: f.auth})
	if err != nil {
		return err
	}
	lockRoutes, err := rpc.NewLockRoutes(rpc.LockRoutesDeps{Server: lockServer})
	if err != nil {
		return err
	}
	peerRoutes, err := rpc.NewPeerRoutes(rpc.PeerRoutesDeps{Notifier: f.notifier, Info: f.info})
	if err != nil {
		return err
	}
	bootRoutes, err := rpc.NewBootstrapRoutes(rpc.BootstrapRoutesDeps{Bootstrapper: f.bootstrapper})
	if err != nil {
		return err
	}
	serverOpts := []rpc.ServerOption{rpc.WithSkip(s3.VirtualHostRequest(domainsOf(cfg)...))}
	if cfg.TLSInternodeVerify {
		serverOpts = append(serverOpts, rpc.WithClientCertRequired())
	}
	f.rpc, err = rpc.NewServer(rpc.ServerDeps{Auth: f.auth, Metrics: f.registry, Clock: d.clock, Logger: logger,
		Routes: []rpc.Routes{storageRoutes, lockRoutes, peerRoutes, bootRoutes}}, serverOpts...)
	return err
}

// cmpOr returns v when positive, def otherwise.
func cmpOr(v, def time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return def
}

// internodeTLS returns the TLS configuration of the internode connections
// of a deployment whose endpoints are https URLs: the system roots plus
// the certificates of <certs>/CAs and the server certificate itself (a
// cluster commonly shares one certificate). With mutual TLS
// (TLSInternodeVerify) the node presents its own certificate to its
// peers. It is nil for http endpoints.
func internodeTLS(cfg config.Static, layout *topology.Layout) (*tls.Config, error) {
	eps := layout.Endpoints()
	if len(eps) == 0 || eps[0].Scheme != "https" {
		if cfg.TLSInternodeVerify {
			return nil, fmt.Errorf("%w: mutual TLS between the nodes needs https endpoints", domain.ErrInvalidArgument)
		}
		return nil, nil
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	appendCerts(pool, cfg)
	out := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	if cfg.TLSInternodeVerify {
		cert, err := tls.LoadX509KeyPair(filepath.Join(cfg.CertsDir, "public.crt"), filepath.Join(cfg.CertsDir, "private.key"))
		if err != nil {
			return nil, fmt.Errorf("internode client certificate: %w", err)
		}
		out.Certificates = []tls.Certificate{cert}
	}
	return out, nil
}

// deploymentCAs returns the certificates the nodes of the deployment are
// verified with as TLS clients (mutual TLS): <certs>/CAs and the server
// certificate itself, not the system roots.
func deploymentCAs(cfg config.Static) *x509.CertPool {
	pool := x509.NewCertPool()
	appendCerts(pool, cfg)
	return pool
}

// appendCerts adds the certificates of <certs>/CAs and the server
// certificate to pool.
func appendCerts(pool *x509.CertPool, cfg config.Static) {
	if cfg.CertsDir == "" {
		return
	}
	files, _ := filepath.Glob(filepath.Join(cfg.CertsDir, "CAs", "*"))
	files = append(files, filepath.Join(cfg.CertsDir, "public.crt"))
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		pool.AppendCertsFromPEM(data)
	}
}
