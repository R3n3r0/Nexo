package admin

import (
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/topology"
)

// poolStates completes the pools of the layout with their quorums and the
// state of every erasure set, from the drives reported by the nodes: a
// drive counts as online unless it is offline (a healing drive answers);
// a set is ok when every drive is online and none heals, degraded while
// it keeps the write quorum, readonly with the read quorum only and
// unavailable below it. Drives the nodes do not report count as offline.
func poolStates(pools []domain.PoolInfo, nodes []domain.NodeInfo) []domain.PoolInfo {
	type key struct{ pool, set int }
	type counts struct{ online, healing int }
	byKey := map[key]*counts{}
	for _, n := range nodes {
		for _, d := range n.Disks {
			k := key{d.PoolIdx, d.SetIdx}
			c := byKey[k]
			if c == nil {
				c = &counts{}
				byKey[k] = c
			}
			if d.State == domain.StateOffline {
				continue
			}
			c.online++
			if d.Healing || d.State == domain.StateHealing {
				c.healing++
			}
		}
	}
	out := make([]domain.PoolInfo, 0, len(pools))
	for _, p := range pools {
		p.ReadQuorum, p.WriteQuorum = topology.SetQuorums(p.DrivesPerSet, p.Parity)
		p.ErasureSets = make([]domain.ErasureSetInfo, 0, p.Sets)
		for i := range p.Sets {
			set := domain.ErasureSetInfo{Index: i, Drives: p.DrivesPerSet}
			if c := byKey[key{p.Index, i}]; c != nil {
				set.Online, set.Healing = min(c.online, p.DrivesPerSet), c.healing
			}
			set.Offline = set.Drives - set.Online
			switch {
			case set.Online < p.ReadQuorum:
				set.State = domain.SetStateUnavailable
			case set.Online < p.WriteQuorum:
				set.State = domain.SetStateReadOnly
			case set.Offline > 0 || set.Healing > 0:
				set.State = domain.SetStateDegraded
			default:
				set.State = domain.SetStateOK
			}
			p.ErasureSets = append(p.ErasureSets, set)
		}
		out = append(out, p)
	}
	return out
}
