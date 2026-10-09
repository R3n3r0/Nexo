// Package preflight validates the configuration of a node without
// starting anything nor writing to its drives: the checks of
// "nexo server --dry-run" (docs/08 §8.3, TM-12). The composition root
// (internal/app) gives it the settings of the static configuration; the
// service checks the topology (mode, erasure sets, recognition of this
// node among the URL endpoints), the parities of the storage classes, the
// local drive directories (existing and writable, or creatable), the
// format.json already on them (same deployment, compatible with the
// topology), the listen addresses (free) and the TLS certificates
// (readable key pair, not expired).
package preflight

import (
	"context"

	"github.com/r3n3r0/nexo/internal/domain"
)

// Address is a listen address of the node and its name in the reports
// ("s3 address").
type Address struct {
	Name string
	Addr string
}

// Input is the configuration of the node to validate.
type Input struct {
	// Endpoints are the drive arguments of the server (topology).
	Endpoints []string
	// Listen are the addresses the node listens on.
	Listen []Address
	// StorageClassStandard and StorageClassRRS are the parities of the
	// storage classes ("EC:n"; empty for the default).
	StorageClassStandard string
	StorageClassRRS      string
	// CertsDir holds the TLS key pair (public.crt, private.key); empty
	// without TLS.
	CertsDir string
}

// Report describes a validated configuration.
type Report struct {
	Mode domain.Mode
	// NodeID is the "host:port" of this node in a cluster, "local"
	// otherwise; Nodes the number of nodes of the deployment.
	NodeID string
	Nodes  int
	// Pools summarizes the erasure pools of the layout.
	Pools []domain.PoolInfo
	// LocalDrives are the directories of the drives of this node.
	LocalDrives []string
	// DeploymentID is the deployment found in the format.json of the
	// local drives, empty for fresh drives.
	DeploymentID string
	// Formatted counts the local drives that carry a format.json.
	Formatted int
	// Warnings are the problems that do not prevent the start (a
	// certificate close to its expiry).
	Warnings []string
}

// Service validates the configuration of a node.
type Service interface {
	// Check validates in and returns what it found. Every problem found
	// is returned, joined; the report is returned with them once the
	// topology is parsed (nil when it cannot be).
	Check(ctx context.Context, in Input) (*Report, error)
}
