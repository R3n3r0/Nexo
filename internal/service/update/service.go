// Package update orchestrates the binary update and the coordinated
// restart and stop of a deployment (S-OPS-09, C-ADM-09, docs/03 §3.13).
//
// Every node runs an Agent: it downloads a new binary next to the one it
// runs (staging: the SHA-256 is verified and the binary is asked for its
// version), swaps it in place of the running one keeping the previous one
// as "<binary>.old", and replaces its process with it once the requests in
// flight are answered. The node receiving the request orchestrates the
// others through their agents (internode RPC):
//
//   - an update stages the binary on every node first (all or nothing),
//     then activates it node by node (rolling: a node only when one more
//     node can stop without losing the write quorum of any erasure set,
//     the next one once it is back with the new version and the
//     deployment healthy), or on every node at once (--all-at-once: a
//     short outage, required to change the major version);
//   - a coordinated restart restarts the nodes the same way, without a
//     new binary;
//   - a coordinated stop stops the other nodes, then this one.
//
// The orchestrating node restarts last. The job is kept in the system
// store: the node that restarted, or any other, completes it afterwards by
// checking the version and the run of every node.
package update

import (
	"context"
	"time"

	"github.com/r3n3r0/nexo/internal/lifecycle"
	"github.com/r3n3r0/nexo/internal/service/object"
)

// Service orchestrates the updates and the coordinated restarts of the
// deployment, and is the agent of this node.
type Service interface {
	// Runner interrupts the job this node orchestrates when it stops.
	lifecycle.Runner
	// Agent is the update side of this node, served to the peers.
	Agent

	// Update starts the update of every node to the binary at in.URL and
	// returns the job. Another job running is domain.ErrConflict; a node
	// unreachable, or a deployment that cannot lose a node for a rolling
	// update, domain.ErrPreconditionFailed; a server not run as a process
	// domain.ErrNotImplemented.
	Update(ctx context.Context, in UpdateInput) (Job, error)
	// Restart starts the coordinated restart of every node and returns
	// the job (same errors as Update).
	Restart(ctx context.Context, in RestartInput) (Job, error)
	// StopAll stops the other nodes, then this one, once the answer is
	// sent.
	StopAll(ctx context.Context) error
	// Job returns the last job (domain.ErrNotFound when none ran), completed
	// if its nodes are all back.
	Job(ctx context.Context) (Job, error)
}

// Agent is the update side of a node: this node (the Service), another one
// through the internode RPC.
type Agent interface {
	// NodeID returns the "host:port" of the node ("local" on a single
	// node). It is a pure accessor.
	NodeID() string
	// State returns the version and the run of the node.
	State(ctx context.Context) (NodeState, error)
	// Stage downloads the binary, verifies its SHA-256 and its version,
	// and keeps it next to the running one until Activate.
	Stage(ctx context.Context, in StageInput) (NodeState, error)
	// Activate swaps the staged binary of digest sha256 in place of the
	// running one and replaces the process with it, after the answer.
	Activate(ctx context.Context, sha256 string) error
	// RestartNode restarts the server of the node, after the answer.
	RestartNode(ctx context.Context) error
	// StopNode stops the server of the node, after the answer.
	StopNode(ctx context.Context) error
}

// Process controls the process of this node (the composition root).
type Process interface {
	// Executable returns the path of the binary the process runs;
	// domain.ErrNotImplemented when the server is not run as a process of
	// its own (tests, embedding).
	Executable() (string, error)
	// Exec replaces the process with the binary at the executable path
	// once the requests in flight are answered.
	Exec() error
	// Restart restarts the server in the process.
	Restart() error
	// Stop stops the process.
	Stop() error
}

// Health reports the health of the storage (object.Service).
type Health interface {
	Health(ctx context.Context, opts object.HealthOptions) object.HealthResult
}

// Mode is how the nodes restart.
type Mode string

const (
	// ModeRolling restarts the nodes one by one, keeping the deployment
	// available.
	ModeRolling Mode = "rolling"
	// ModeAll restarts every node at once.
	ModeAll Mode = "all"
)

// UpdateInput describes an update.
type UpdateInput struct {
	// URL is the http(s) URL of the binary for the platform of the nodes.
	URL string
	// SHA256 is its hex digest.
	SHA256 string
	// Mode is ModeRolling (default) or ModeAll.
	Mode Mode
}

// RestartInput describes a coordinated restart.
type RestartInput struct {
	Mode Mode
}

// StageInput is the binary to stage.
type StageInput struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// NodeState is the state of a node.
type NodeState struct {
	Node    string `json:"node"`
	Version string `json:"version"`
	// Boot identifies the run of the server: it changes at every
	// restart.
	Boot      string    `json:"boot"`
	StartedAt time.Time `json:"startedAt"`
	// Staged is the binary staged, if any.
	Staged *Staged `json:"staged,omitempty"`
}

// Staged is a binary staged on a node.
type Staged struct {
	SHA256  string `json:"sha256"`
	Version string `json:"version"`
}

// JobKind is the operation of a job.
type JobKind string

const (
	KindUpdate  JobKind = "update"
	KindRestart JobKind = "restart"
)

// JobState is the state of a job.
type JobState string

const (
	JobRunning   JobState = "running"
	JobCompleted JobState = "completed"
	JobFailed    JobState = "failed"
)

// Phase is the progress of a node in a job.
type Phase string

const (
	PhasePending    Phase = "pending"
	PhaseStaged     Phase = "staged"
	PhaseRestarting Phase = "restarting"
	PhaseDone       Phase = "done"
	PhaseFailed     Phase = "failed"
)

// Job is an update or a coordinated restart.
type Job struct {
	ID    string   `json:"id"`
	Kind  JobKind  `json:"kind"`
	Mode  Mode     `json:"mode"`
	State JobState `json:"state"`
	// URL, SHA256 and Version (the version of the staged binary) describe
	// an update.
	URL     string `json:"url,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
	Version string `json:"version,omitempty"`
	// Node orchestrates the job during its run Boot.
	Node     string         `json:"node"`
	Boot     string         `json:"boot"`
	Started  time.Time      `json:"started"`
	Finished time.Time      `json:"finished,omitzero"`
	Error    string         `json:"error,omitempty"`
	Nodes    []NodeProgress `json:"nodes"`
}

// NodeProgress is the progress of a node in a job.
type NodeProgress struct {
	Node    string `json:"node"`
	Phase   Phase  `json:"phase"`
	Version string `json:"version,omitempty"`
	// Boot is the run of the node before its restart.
	Boot  string `json:"boot,omitempty"`
	Error string `json:"error,omitempty"`
}
