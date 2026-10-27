package nexoadmin

import (
	"context"
	"net/http"
	"time"
)

// SiteReplicationSite is one deployment of a site replication group.
type SiteReplicationSite struct {
	// Name names the site in the group (letters, digits, '.', '_', '-').
	Name string `json:"name"`
	// DeploymentID is filled by the server.
	DeploymentID string `json:"deploymentId,omitempty"`
	// Endpoint is the URL of the S3 API of the site, AdminEndpoint that
	// of its Admin API (its console listener).
	Endpoint      string `json:"endpoint"`
	AdminEndpoint string `json:"adminEndpoint"`
}

// SiteReplicationPeerStatus is the synchronization with one other site,
// as the leader of this site last recorded it.
type SiteReplicationPeerStatus struct {
	Name         string    `json:"name"`
	DeploymentID string    `json:"deploymentId"`
	LastSync     time.Time `json:"lastSync,omitzero"`
	LastError    string    `json:"lastError,omitempty"`
	LastErrorAt  time.Time `json:"lastErrorAt,omitzero"`
	// Pushed and Pulled count the entries sent to the site and taken from
	// it; Pending those whose change could not be applied yet.
	Pushed  int64 `json:"pushed"`
	Pulled  int64 `json:"pulled"`
	Pending int   `json:"pending"`
}

// SiteReplicationInfo is the site replication of a deployment.
type SiteReplicationInfo struct {
	Enabled bool `json:"enabled"`
	// Name and DeploymentID are those of the site that answered.
	Name         string                      `json:"name,omitempty"`
	DeploymentID string                      `json:"deploymentId"`
	Sites        []SiteReplicationSite       `json:"sites"`
	Peers        []SiteReplicationPeerStatus `json:"peers"`
	// Entries counts the replicated entries of this site (users, groups,
	// access keys, policies, buckets).
	Entries int `json:"entries"`
}

// SiteReplicationPeer is what a site tells another one about itself
// (internal call of the site replication).
type SiteReplicationPeer struct {
	DeploymentID string                `json:"deploymentId"`
	Enabled      bool                  `json:"enabled"`
	Sites        []SiteReplicationSite `json:"sites,omitempty"`
}

// SiteReplicationEntry is the state of one replicated entry.
type SiteReplicationEntry struct {
	Hash    string    `json:"hash"`
	ModTime time.Time `json:"modTime,omitzero"`
}

// SiteReplicationState is the state of the replicated entries of a site
// (internal call).
type SiteReplicationState struct {
	DeploymentID string                          `json:"deploymentId"`
	Entries      map[string]SiteReplicationEntry `json:"entries"`
}

// SiteReplicationResult is the outcome of one change applied by a site
// (internal call): Error is empty on success.
type SiteReplicationResult struct {
	Key   string `json:"key"`
	Error string `json:"error,omitempty"`
}

type siteReplicationSites struct {
	Sites []SiteReplicationSite `json:"sites"`
}

type siteReplicationKeys struct {
	Keys []string `json:"keys"`
}

// SiteReplicationSealed carries sealed entries between sites.
type SiteReplicationSealed struct {
	Data []byte `json:"data"`
}

type siteReplicationResults struct {
	Results []SiteReplicationResult `json:"results"`
}

// GetSiteReplication implements AdminClient.
func (c *client) GetSiteReplication(ctx context.Context) (*SiteReplicationInfo, error) {
	var out SiteReplicationInfo
	return &out, c.do(ctx, request{method: http.MethodGet, path: "/site-replication"}, &out)
}

// AddSiteReplication implements AdminClient.
func (c *client) AddSiteReplication(ctx context.Context, sites []SiteReplicationSite) (*SiteReplicationInfo, error) {
	var out SiteReplicationInfo
	return &out, c.do(ctx, request{method: http.MethodPost, path: "/site-replication", body: siteReplicationSites{Sites: sites}}, &out)
}

// RemoveSiteReplication implements AdminClient.
func (c *client) RemoveSiteReplication(ctx context.Context) error {
	return c.do(ctx, request{method: http.MethodDelete, path: "/site-replication"}, nil)
}

// SyncSiteReplication implements AdminClient.
func (c *client) SyncSiteReplication(ctx context.Context) error {
	return c.do(ctx, request{method: http.MethodPost, path: "/site-replication/sync"}, nil)
}

// SiteReplicationPeerInfo implements AdminClient.
func (c *client) SiteReplicationPeerInfo(ctx context.Context) (*SiteReplicationPeer, error) {
	var out SiteReplicationPeer
	return &out, c.do(ctx, request{method: http.MethodGet, path: "/site-replication/peer"}, &out)
}

// JoinSiteReplication implements AdminClient.
func (c *client) JoinSiteReplication(ctx context.Context, sites []SiteReplicationSite) error {
	return c.do(ctx, request{method: http.MethodPost, path: "/site-replication/peer/join", body: siteReplicationSites{Sites: sites}}, nil)
}

// LeaveSiteReplication implements AdminClient.
func (c *client) LeaveSiteReplication(ctx context.Context) error {
	return c.do(ctx, request{method: http.MethodPost, path: "/site-replication/peer/leave"}, nil)
}

// SiteReplicationState implements AdminClient.
func (c *client) SiteReplicationState(ctx context.Context) (*SiteReplicationState, error) {
	var out SiteReplicationState
	return &out, c.do(ctx, request{method: http.MethodGet, path: "/site-replication/peer/state"}, &out)
}

// SiteReplicationItems implements AdminClient.
func (c *client) SiteReplicationItems(ctx context.Context, keys []string) ([]byte, error) {
	var out SiteReplicationSealed
	err := c.do(ctx, request{method: http.MethodPost, path: "/site-replication/peer/items", body: siteReplicationKeys{Keys: keys}}, &out)
	return out.Data, err
}

// SiteReplicationApply implements AdminClient.
func (c *client) SiteReplicationApply(ctx context.Context, sealed []byte) ([]SiteReplicationResult, error) {
	var out siteReplicationResults
	err := c.do(ctx, request{method: http.MethodPost, path: "/site-replication/peer/apply", body: SiteReplicationSealed{Data: sealed}}, &out)
	return out.Results, err
}
