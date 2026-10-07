package topology

import "fmt"

// DefaultParity returns the default number of parity shards for an erasure set
// of n drives: n <= 5 -> 2, 6..7 -> 3, >= 8 -> 4, capped at n/2 so that there is
// always at least as many data shards as parity shards (n=2,3 -> 1). A single
// drive (SNSD) has no parity.
func DefaultParity(n int) int {
	var p int
	switch {
	case n <= 1:
		return 0
	case n <= 5:
		p = 2
	case n <= 7:
		p = 3
	default:
		p = 4
	}
	if p > n/2 {
		p = n / 2
	}
	return p
}

// isSymmetric reports whether erasure sets of size setSize can take the same
// number of drives from every node of a pool with nodeCount nodes: either a
// set spans all nodes evenly (setSize multiple of nodeCount) or the nodes are
// partitioned evenly among sets (nodeCount multiple of setSize).
func isSymmetric(setSize, nodeCount int) bool {
	if setSize <= 0 || nodeCount <= 0 {
		return false
	}
	if setSize >= nodeCount {
		return setSize%nodeCount == 0
	}
	return nodeCount%setSize == 0
}

// chooseSetDriveCount returns the largest set size in [MinSetDriveCount,
// MaxSetDriveCount] that divides totalDrives and is symmetric with nodeCount.
func chooseSetDriveCount(totalDrives, nodeCount int) (int, error) {
	for s := MaxSetDriveCount; s >= MinSetDriveCount; s-- {
		if totalDrives%s == 0 && isSymmetric(s, nodeCount) {
			return s, nil
		}
	}
	return 0, fmt.Errorf("%w: %d drives on %d node(s)", ErrInvalidSetSize, totalDrives, nodeCount)
}

// validateSetDriveCount checks a user supplied set size against a pool.
func validateSetDriveCount(setSize, totalDrives, nodeCount int) error {
	if setSize < MinSetDriveCount || setSize > MaxSetDriveCount {
		return fmt.Errorf("%w: set size %d is outside [%d..%d]", ErrInvalidSetSize, setSize, MinSetDriveCount, MaxSetDriveCount)
	}
	if totalDrives%setSize != 0 {
		return fmt.Errorf("%w: set size %d does not divide %d drives", ErrInvalidSetSize, setSize, totalDrives)
	}
	if !isSymmetric(setSize, nodeCount) {
		return fmt.Errorf("%w: set size %d is not symmetric with %d node(s)", ErrInvalidSetSize, setSize, nodeCount)
	}
	return nil
}

// validateParity checks that parity is admissible for a set of n drives.
func validateParity(parity, n int) error {
	if n <= 1 {
		if parity != 0 {
			return fmt.Errorf("%w: parity %d on a single drive", ErrInvalidParity, parity)
		}
		return nil
	}
	if parity < 1 || parity > n/2 {
		return fmt.Errorf("%w: parity %d must be in [1..%d] for %d drives", ErrInvalidParity, parity, n/2, n)
	}
	return nil
}

// distributeRoundRobin orders the endpoints of a pool so that consecutive
// drives belong to different nodes: the first drive of every node (in order of
// first appearance), then the second drive of every node, and so on. Slicing
// the result into sets of setSize drives spreads every set across nodes.
func distributeRoundRobin(eps []Endpoint) (ordered []Endpoint, nodeKeys []string, perNode map[string][]Endpoint) {
	perNode = make(map[string][]Endpoint)
	for _, ep := range eps {
		key := ep.NodeKey()
		if _, seen := perNode[key]; !seen {
			nodeKeys = append(nodeKeys, key)
		}
		perNode[key] = append(perNode[key], ep)
	}
	ordered = make([]Endpoint, 0, len(eps))
	for d := 0; len(ordered) < len(eps); d++ {
		for _, key := range nodeKeys {
			if d < len(perNode[key]) {
				ordered = append(ordered, perNode[key][d])
			}
		}
	}
	return ordered, nodeKeys, perNode
}
