// Package cluster holds the services that make the nodes of a deployment
// one cluster (docs/03 §3.5, §3.9): the startup sequence (Bootstrapper:
// same topology on every node, quorum of drives per set, coordinated
// format.json), the broadcast of cache invalidations (Notifier), the
// aggregated server information and clock checks (Info) and the leader
// election of the singleton background jobs (Leader).
//
// Every service has a distributed implementation, which reaches the other
// nodes through the Peer and BootstrapPeer clients of the internode RPC
// (internal/transport/rpc), and a single-node one (NewLocalNotifier,
// NewLocalLeader, NewLocalBootstrapper, NewInfo without peers) for the
// SNSD and SNMD modes, so that the composition root wires the same
// interfaces in every mode.
package cluster

import (
	"context"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/lifecycle"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	configsvc "github.com/r3n3r0/nexo/internal/service/config"
	"github.com/r3n3r0/nexo/internal/service/iam"
	"github.com/r3n3r0/nexo/internal/service/policy"
)

// Node is a server of the deployment.
type Node struct {
	// ID is the "host:port" of the node, "local" for the implicit node of
	// a single-node deployment.
	ID string
	// Local is true for the node running this process.
	Local bool
	// Endpoints are the drives of the node, across the pools.
	Endpoints []domain.Endpoint
}

// EventKind is the kind of state an Event invalidates.
type EventKind string

// Event kinds. The services that cache a state subscribe to its kind and
// reload it (all of it when Event.Name is empty).
const (
	// EventIAM invalidates users, groups, access keys and their policies.
	EventIAM EventKind = "iam"
	// EventPolicy invalidates the named policies.
	EventPolicy EventKind = "policy"
	// EventBucketMetadata invalidates the metadata of a bucket (policy,
	// versioning, lifecycle, lock, quota, tags, CORS, notifications).
	EventBucketMetadata EventKind = "bucket-metadata"
	// EventConfig invalidates the dynamic configuration.
	EventConfig EventKind = "config"
	// EventShare invalidates the managed share links.
	EventShare EventKind = "share"
	// EventSessionRevocation invalidates console sessions (logout,
	// revocation, secret rotation).
	EventSessionRevocation EventKind = "session-revocation"
	// EventResync asks every subscriber, whatever its kind, to reload its
	// whole state: the node missed events (it was unreachable).
	EventResync EventKind = "resync"
)

// Event is a cache invalidation broadcast to the peers.
type Event struct {
	Kind EventKind
	// Name is the entity that changed (user, policy, bucket, subsystem,
	// share or session id); empty means every entity of the kind.
	Name string
	// Origin is the node that broadcast the event and Time when, both set
	// by Broadcast.
	Origin string
	Time   time.Time
}

// Handler reloads the state an event invalidates.
type Handler interface {
	HandleEvent(ctx context.Context, e Event) error
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(ctx context.Context, e Event) error

// HandleEvent implements Handler.
func (f HandlerFunc) HandleEvent(ctx context.Context, e Event) error { return f(ctx, e) }

// Notifier broadcasts the invalidations of the state a node changed to the
// other nodes, and delivers the invalidations it receives to the local
// subscribers. It runs a background retry of the deliveries that failed
// (lifecycle.Runner): a peer that was unreachable receives the events it
// missed, or a single EventResync when it missed too many. Since an
// invalidation can still be lost (the node that queued it crashed), the
// background loop also reloads the local state (a local EventResync)
// periodically and when the node notices it was frozen, and sends an
// EventResync to every peer that comes back online (docs/03 §3.9).
type Notifier interface {
	lifecycle.Runner
	// Broadcast sends e to every peer, synchronously, so that the other
	// nodes have dropped the stale state when it returns (docs/06 TC-04).
	// The local subscribers are not notified: the caller holds the new
	// state. The error lists the peers that could not be reached; their
	// delivery is retried in the background.
	Broadcast(ctx context.Context, e Event) error
	// Subscribe registers h for the events of kind (and for every
	// EventResync) and returns the function that unregisters it. It is a
	// pure registration (no I/O), the documented exception to the context
	// rule.
	Subscribe(kind EventKind, h Handler) (unsubscribe func())
	// Deliver dispatches an event received from a peer to the local
	// subscribers and returns their errors.
	Deliver(ctx context.Context, e Event) error
}

// Coherence keeps the caches of the nodes coherent (docs/03 §3.9): the
// shared stores it wraps broadcast, after every successful write, the
// invalidation event of what they changed (IAM, named policies, dynamic
// configuration, bucket metadata, share links, session revocations), and
// Subscribe makes this node drop the caches the events of its peers name.
// A broadcast that misses a peer does not fail the write (it succeeded):
// the notifier retries the delivery and resynchronizes the nodes. In the
// single-node modes the notifier is local and the broadcasts are no-ops.
// The wrappers are pure decorations and Subscribe a pure registration (no
// I/O, the documented exception to the context rule).
type Coherence interface {
	// IAMStore wraps the IAM store (users, groups, service accounts, STS
	// credentials, share links, session revocations).
	IAMStore(s iam.Store) iam.Store
	// PolicyStore wraps the store of the named policies.
	PolicyStore(s policy.SharedStore) policy.SharedStore
	// ConfigStore wraps the store of the dynamic configuration.
	ConfigStore(s configsvc.Store) configsvc.Store
	// BucketStore wraps the store of the bucket metadata.
	BucketStore(s bucket.MetadataStore) bucket.MetadataStore
	// Subscribe registers the invalidations of caches on the notifier and
	// returns the function that unregisters them. The share links and the
	// session revocations are read through on every use: their events
	// need no handler.
	Subscribe(caches Caches) (unsubscribe func())
}

// Caches are the caches a node drops when a peer changed the state they
// hold.
type Caches struct {
	// IAM drops the identities (required).
	IAM interface{ Invalidate(ctx context.Context) }
	// Policies drops the named policies; nil when they are not shared
	// (SNSD).
	Policies interface{ Invalidate(ctx context.Context) }
	// Config reloads the dynamic configuration (required).
	Config interface {
		Reload(ctx context.Context) error
	}
	// Buckets drops the metadata of a bucket ("" for every bucket)
	// (required).
	Buckets interface {
		Invalidate(ctx context.Context, name string)
	}
}

// PingResult is the answer of a node to a ping: its identity, version and
// clock.
type PingResult struct {
	NodeID  string
	Version string
	Time    time.Time
	Uptime  time.Duration
}

// Peer is the client of the peer service of another node (internode RPC).
type Peer interface {
	// NodeID returns the "host:port" of the peer. It is a pure accessor.
	NodeID() string
	// IsOnline reports whether the peer is reachable (circuit closed). It
	// is a pure accessor.
	IsOnline() bool
	// Ping returns the identity, version and clock of the peer.
	Ping(ctx context.Context) (PingResult, error)
	// Notify delivers an invalidation to the peer.
	Notify(ctx context.Context, e Event) error
	// NodeInfo describes the peer and its drives.
	NodeInfo(ctx context.Context) (domain.NodeInfo, error)
}

// NodeConfig is the startup configuration a node announces to its peers:
// they must run the same topology and compatible versions.
type NodeConfig struct {
	NodeID string
	// TopologyHash is topology.Layout.Hash of the command line.
	TopologyHash string
	Mode         domain.Mode
	// Version is the build version of the node.
	Version string
	// DeploymentID is empty until the node read or wrote its format.
	DeploymentID string
	// KMSFingerprint identifies the KMS configuration of the node without
	// revealing it (empty without KMS): the nodes must share their master
	// keys or KES servers, or the objects encrypted by one node would be
	// unreadable on the others.
	KMSFingerprint string
	// Time is the clock of the node when it answered.
	Time time.Time
	// Endpoints are the drive endpoints of its command line, in order
	// (empty from a node of an older release): a node at odds with the
	// others names the endpoints that differ.
	Endpoints []string
}

// BootstrapPeer is the client of the bootstrap service of another node.
type BootstrapPeer interface {
	// NodeID returns the "host:port" of the peer. It is a pure accessor.
	NodeID() string
	// Config returns the startup configuration of the peer.
	Config(ctx context.Context) (NodeConfig, error)
}

// BootstrapResult is the outcome of a successful bootstrap.
type BootstrapResult struct {
	DeploymentID string
	// Healing lists the local drives found empty in an existing
	// deployment: they received their format.json and a healing tracker,
	// the healing service repopulates them.
	Healing []domain.Endpoint
	// Joined lists the local drives formatted after their peers formatted
	// a fresh deployment (the regular first start of a node that started
	// after the others): their set holds only the configuration written
	// at the bootstrap, which the healing copies. Not replacements.
	Joined []domain.Endpoint
	// Offline lists the drives that were unreachable or unformatted when
	// the quorum was reached.
	Offline []domain.Endpoint
}

// DeploymentBinder binds the internode authentication of the node to its
// deployment (rpc.Authenticator implements it). The bootstrapper calls it
// as soon as it knows the deployment id, read from the formats or
// generated for a fresh deployment (before it writes the formats on the
// peers), so that the node refuses the callers of other deployments from
// then on instead of at the end of the bootstrap.
type DeploymentBinder interface {
	SetDeploymentID(ctx context.Context, id string)
}

// Bootstrapper runs the startup sequence of a node (docs/03 §3.5).
type Bootstrapper interface {
	// LocalConfig returns the configuration this node announces to its
	// peers (the server side of the bootstrap RPC).
	LocalConfig(ctx context.Context) (NodeConfig, error)
	// Bootstrap verifies that a quorum of the nodes run the same topology,
	// compatible versions and the same KMS configuration (a node whose
	// topology differs from the majority fails with ErrTopologyMismatch,
	// whose KMS differs with ErrKMSMismatch), waits for a quorum of
	// drives in every erasure set, formats a fresh deployment (only the
	// node of the first endpoint does, the others wait for it), formats
	// and marks for healing the empty local drives of an existing one, and
	// returns the deployment id. It retries with backoff, logging what is
	// missing, until ctx ends.
	Bootstrap(ctx context.Context) (BootstrapResult, error)
}

// LeaderListener is notified of the leadership changes of this node: the
// singleton jobs (scanner, ILM, ...) start in OnElected, with a context
// that is canceled when the leadership is lost, and stop in OnRevoked.
type LeaderListener interface {
	OnElected(ctx context.Context)
	OnRevoked(ctx context.Context)
}

// Leader elects one node of the deployment for the singleton background
// jobs: the leader holds the distributed "leader" lock, whose lease is
// renewed by the lock refresh; it loses the leadership with the lock (lost
// quorum, crash) and another node takes over.
type Leader interface {
	lifecycle.Runner
	// IsLeader reports whether this node is the leader now. It is a pure
	// accessor.
	IsLeader() bool
	// Subscribe registers a listener and returns the function that
	// unregisters it; a listener registered while this node leads is
	// notified at once. It is a pure registration (no I/O).
	Subscribe(l LeaderListener) (unsubscribe func())
}

// Skew is the clock offset of a peer measured by a ping.
type Skew struct {
	NodeID string
	// Offset is the peer clock minus the local clock, corrected by half
	// the round trip.
	Offset time.Duration
	RTT    time.Duration
	// Exceeded reports an offset above the threshold (2s by default).
	Exceeded bool
	// Err is set when the peer could not be reached.
	Err string
}

// HealQueueReporter reports the counters of the background healing queue
// (MRF) of this node, which Info.Local publishes to the other nodes.
type HealQueueReporter interface {
	HealQueueStats(ctx context.Context) domain.HealQueueStats
}

// HealQueueReporterFunc adapts a function to HealQueueReporter.
type HealQueueReporterFunc func(ctx context.Context) domain.HealQueueStats

// HealQueueStats implements HealQueueReporter.
func (f HealQueueReporterFunc) HealQueueStats(ctx context.Context) domain.HealQueueStats {
	return f(ctx)
}

// Info describes the nodes of the deployment.
type Info interface {
	// Ping answers the ping of a peer (the server side of the peer RPC).
	Ping(ctx context.Context) (PingResult, error)
	// Local describes this node and its drives.
	Local(ctx context.Context) (domain.NodeInfo, error)
	// Nodes describes every node, this one first, in topology order;
	// unreachable peers are reported offline with their drives.
	Nodes(ctx context.Context) ([]domain.NodeInfo, error)
	// ClockSkew measures the clock offset of every peer and logs a warning
	// for the ones above the threshold.
	ClockSkew(ctx context.Context) ([]Skew, error)
}
