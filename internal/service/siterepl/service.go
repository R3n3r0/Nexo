// Package siterepl keeps several deployments in sync (site replication,
// S-STO-11, docs/03 §3.10): their users, groups, access keys and named
// policies, their buckets with their settings, and the replication of
// every bucket to the same bucket of every other site.
//
// The sites of a group share the root credentials: they call each other
// through the Admin API with them, and seal the entries they exchange
// with a key derived from them. The leader of each site synchronizes it
// with every other site in rounds: it compares the entries of both sides
// (a hash per user, group, access key, policy and bucket) with those both
// had after the last round (the base, kept per peer in the system
// documents), sends what changed here, takes what changed there and, when
// both changed, keeps the most recent change (a change wins over a
// deletion). A deletion is never inferred from an entry missing at the
// first round: the first round only adds. Each round then makes sure that
// every bucket has versioning and replicates to the same bucket of every
// other site (bucket replication rules named site-<name>, existing objects
// resynced).
package siterepl

import (
	"context"
	"time"

	"github.com/r3n3r0/nexo/internal/lifecycle"
	"github.com/r3n3r0/nexo/internal/service/iam"
)

// Service manages the site replication of this deployment.
type Service interface {
	// Runner runs the rounds on the leader.
	lifecycle.Runner

	// Add groups the sites, this one among them (matched by deployment
	// ID): each must be reachable at its Admin API with the root
	// credentials of this one and not be in a group. The group is saved
	// on every site and a first round starts.
	Add(ctx context.Context, sites []SiteInput) (Info, error)
	// Info describes the group and the last rounds.
	Info(ctx context.Context) (Info, error)
	// Remove dissolves the group: every site reached forgets it and the
	// replication rules it added; what was replicated stays.
	Remove(ctx context.Context) error
	// Sync asks for a round now.
	Sync(ctx context.Context) error

	// The calls of the other sites.

	// Peer describes this site.
	Peer(ctx context.Context) (PeerInfo, error)
	// Join saves the group of sites (it must hold this one).
	Join(ctx context.Context, sites []Site) error
	// Leave forgets the group and the replication rules it added.
	Leave(ctx context.Context) error
	// State returns the replicated entries of this site
	// (domain.ErrConflict when it is not in a group).
	State(ctx context.Context) (map[string]Entry, error)
	// Items returns the sealed values of entries.
	Items(ctx context.Context, keys []string) ([]byte, error)
	// Apply applies sealed changes and reports each outcome.
	Apply(ctx context.Context, sealed []byte) ([]Result, error)
}

// SiteInput is a site to group.
type SiteInput struct {
	Name          string
	Endpoint      string
	AdminEndpoint string
}

// Site is a site of the group.
type Site struct {
	Name         string `json:"name"`
	DeploymentID string `json:"deploymentId"`
	// Endpoint is the URL of its S3 API, AdminEndpoint of its Admin API.
	Endpoint      string `json:"endpoint"`
	AdminEndpoint string `json:"adminEndpoint"`
}

// PeerInfo is what a site tells the others about itself.
type PeerInfo struct {
	DeploymentID string `json:"deploymentId"`
	Enabled      bool   `json:"enabled"`
	Sites        []Site `json:"sites,omitempty"`
}

// PeerStatus is the synchronization with one other site.
type PeerStatus struct {
	Name         string    `json:"name"`
	DeploymentID string    `json:"deploymentId"`
	LastSync     time.Time `json:"lastSync,omitzero"`
	LastError    string    `json:"lastError,omitempty"`
	LastErrorAt  time.Time `json:"lastErrorAt,omitzero"`
	Pushed       int64     `json:"pushed"`
	Pulled       int64     `json:"pulled"`
	// Pending counts the changes not applied at the last round.
	Pending int `json:"pending"`
}

// Info is the site replication of this deployment.
type Info struct {
	Enabled      bool         `json:"enabled"`
	Name         string       `json:"name,omitempty"`
	DeploymentID string       `json:"deploymentId"`
	Sites        []Site       `json:"sites"`
	Peers        []PeerStatus `json:"peers"`
	// Entries counts the replicated entries at the last round.
	Entries int `json:"entries"`
}

// Entry is the state of a replicated entry.
type Entry struct {
	Hash    string    `json:"hash"`
	ModTime time.Time `json:"modTime,omitzero"`
}

// Result is the outcome of one change applied by a site.
type Result struct {
	Key   string `json:"key"`
	Error string `json:"error,omitempty"`
}

// IAM provides the users, groups and access keys (iam.Service).
type IAM interface {
	SiteRecords(ctx context.Context) (map[string]iam.SiteRecord, error)
	ApplySiteRecords(ctx context.Context, puts map[string][]byte, deletes []string) error
}

// Policies provides the named policies (policy.Service).
type Policies interface {
	// ListPolicies returns the named policies, built-in ones marked.
	ListPolicies(ctx context.Context) ([]NamedPolicy, error)
	SetPolicy(ctx context.Context, name string, doc []byte) error
	DeletePolicy(ctx context.Context, name string) error
}

// NamedPolicy is a named policy as Policies lists it.
type NamedPolicy struct {
	Name string
	// Doc is the policy document (JSON).
	Doc       []byte
	UpdatedAt time.Time
	BuiltIn   bool
}

// BucketSettings reads and applies the settings of a bucket
// (admin.ClusterManager).
type BucketSettings interface {
	SiteBucket(ctx context.Context, name string) ([]byte, error)
	ApplySiteBucket(ctx context.Context, doc []byte) error
}

// Sealer seals the entries exchanged between the sites.
type Sealer interface {
	Seal(ctx context.Context, plaintext, aad []byte) ([]byte, error)
	Open(ctx context.Context, sealed, aad []byte) ([]byte, error)
}

// Leader reports whether this node runs the rounds (cluster.Leader).
type Leader interface {
	IsLeader() bool
}

// Dialer opens the client of another site.
type Dialer interface {
	Dial(ctx context.Context, site Site) (Peer, error)
}

// Peer is the Admin API of another site, called with the root
// credentials.
type Peer interface {
	Info(ctx context.Context) (PeerInfo, error)
	Join(ctx context.Context, sites []Site) error
	Leave(ctx context.Context) error
	State(ctx context.Context) (map[string]Entry, error)
	Items(ctx context.Context, keys []string) ([]byte, error)
	Apply(ctx context.Context, sealed []byte) ([]Result, error)
}
