package nexoadmin

import (
	"context"
	"net/http"
	"time"
)

// UpdateRequest starts the update of the binary of every node (S-OPS-09).
type UpdateRequest struct {
	// URL is the http(s) URL of the server binary for the platform of the
	// nodes, SHA256 its hex digest.
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	// Mode is "rolling" (default: one node at a time, the deployment
	// stays available) or "all" (every node at once).
	Mode string `json:"mode,omitempty"`
}

// UpdateJob is an update or a coordinated restart of the deployment.
type UpdateJob struct {
	ID string `json:"id"`
	// Kind is "update" or "restart", Mode "rolling" or "all", State
	// "running", "completed" or "failed".
	Kind  string `json:"kind"`
	Mode  string `json:"mode"`
	State string `json:"state"`
	// URL, SHA256 and Version (the version of the new binary) describe
	// an update.
	URL     string `json:"url,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
	Version string `json:"version,omitempty"`
	// Node orchestrates the job; it restarts last.
	Node     string       `json:"node"`
	Started  time.Time    `json:"started"`
	Finished time.Time    `json:"finished,omitzero"`
	Error    string       `json:"error,omitempty"`
	Nodes    []UpdateNode `json:"nodes"`
}

// UpdateNode is the progress of a node in a job: Phase is "pending",
// "staged", "restarting", "done" or "failed".
type UpdateNode struct {
	Node    string `json:"node"`
	Phase   string `json:"phase"`
	Version string `json:"version,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Done reports whether the job ended.
func (j *UpdateJob) Done() bool { return j.State == "completed" || j.State == "failed" }

// StartUpdate implements AdminClient.
func (c *client) StartUpdate(ctx context.Context, in UpdateRequest) (*UpdateJob, error) {
	var out UpdateJob
	return &out, c.do(ctx, request{method: http.MethodPost, path: "/service/update", body: in}, &out)
}

// UpdateStatus implements AdminClient.
func (c *client) UpdateStatus(ctx context.Context) (*UpdateJob, error) {
	var out UpdateJob
	return &out, c.do(ctx, request{method: http.MethodGet, path: "/service/update"}, &out)
}

// RestartCluster implements AdminClient.
func (c *client) RestartCluster(ctx context.Context, mode string) (*UpdateJob, error) {
	var out UpdateJob
	return &out, c.do(ctx, request{method: http.MethodPost, path: "/service/cluster/restart", body: map[string]string{"mode": mode}}, &out)
}

// StopCluster implements AdminClient.
func (c *client) StopCluster(ctx context.Context) error {
	return c.do(ctx, request{method: http.MethodPost, path: "/service/cluster/stop"}, nil)
}
