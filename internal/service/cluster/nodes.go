package cluster

import (
	"fmt"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/topology"
)

// Sentinel errors of the cluster services (docs/12 §12.2 rule 5).
var (
	// ErrTopologyMismatch reports a node whose command line describes
	// another deployment than the majority of the nodes.
	ErrTopologyMismatch = fmt.Errorf("%w: cluster: the nodes run different topologies", domain.ErrInvalidLayout)
	// ErrIncompatibleVersion reports nodes whose major versions differ.
	ErrIncompatibleVersion = fmt.Errorf("%w: cluster: incompatible node versions", domain.ErrConflict)
)

// LocalNodeID is the id of the implicit node of a single-node deployment.
const LocalNodeID = "local"

// NodesOf returns the nodes of a layout in topology order (the membership
// of the deployment is static, docs/03 §3.1).
func NodesOf(layout *topology.Layout) []Node {
	if layout == nil {
		return nil
	}
	out := make([]Node, 0, len(layout.Nodes))
	for _, n := range layout.Nodes {
		id := n.String()
		if id == "local" {
			id = LocalNodeID
		}
		out = append(out, Node{ID: id, Local: n.Local, Endpoints: append([]domain.Endpoint(nil), n.Endpoints...)})
	}
	return out
}

// LocalNode returns the node of this process among nodes.
func LocalNode(nodes []Node) (Node, bool) {
	for _, n := range nodes {
		if n.Local {
			return n, true
		}
	}
	return Node{}, false
}
