// Package health exposes liveness, readiness and cluster health checks.
package health

import "context"

// Service answers health probes.
type Service interface {
	// Live reports whether the process is alive. It never fails unless the
	// process is wedged.
	Live(ctx context.Context) Status
	// Ready reports whether the server can serve traffic (storage
	// initialized, quorum met).
	Ready(ctx context.Context) Status
	// Cluster reports the health of the whole deployment.
	Cluster(ctx context.Context) ClusterStatus
}

// Status is the outcome of a probe.
type Status struct {
	OK bool `json:"ok"`
	// Status is "ok" or a short reason.
	Status string `json:"status"`
	// Checks holds the individual check results by name.
	Checks map[string]string `json:"checks,omitempty"`
}

// ClusterStatus extends Status with topology figures.
type ClusterStatus struct {
	Status
	// Nodes and NodesOnline count the servers; Drives and DrivesOnline the drives.
	Nodes        int `json:"nodes"`
	NodesOnline  int `json:"nodesOnline"`
	Drives       int `json:"drives"`
	DrivesOnline int `json:"drivesOnline"`
	// WriteQuorum reports whether every erasure set can accept writes.
	WriteQuorum bool `json:"writeQuorum"`
	// Maintenance reports whether one more node can be taken down safely.
	Maintenance bool `json:"maintenance"`
}
