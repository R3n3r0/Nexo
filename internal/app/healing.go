package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/lifecycle"
	"github.com/r3n3r0/nexo/internal/service/cluster"
	configsvc "github.com/r3n3r0/nexo/internal/service/config"
	"github.com/r3n3r0/nexo/internal/service/heal"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/scanner"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// healingServices are the background services that keep the data whole
// and measured (docs/03 §3.7): the MRF queue the object layer reports to
// and the healing service that drains it, formats and heals the local
// drives and runs the healing jobs of the Admin API (erasure-coded modes
// only: nil in SNSD), and the scanner (every mode, on the leader).
type healingServices struct {
	queue   heal.Queue
	heal    heal.Service
	scanner scanner.Service
}

// poolSpecs returns the pools of the erasure object layer: the drives of
// every set (local and remote) with the parity of the pool.
func poolSpecs(f *foundation) ([]object.PoolSpec, error) {
	sets, err := f.poolSets()
	if err != nil {
		return nil, err
	}
	pools := make([]object.PoolSpec, 0, len(sets))
	for i, p := range f.layout.Pools {
		pools = append(pools, object.PoolSpec{Sets: sets[i], Parity: p.Parity})
	}
	return pools, nil
}

// newHealQueue returns the MRF queue of a node, persisted on its local
// drives; nil in SNSD (a single drive has nothing to heal from). A
// positive maxRetryDelay caps the backoff between two attempts at an
// object (default 5m).
func newHealQueue(mode domain.Mode, local []storage.StorageAPI, clk clock.Clock, logger *slog.Logger, maxRetryDelay time.Duration) heal.Queue {
	if mode == domain.ModeSNSD {
		return nil
	}
	var opts []heal.QueueOption
	if maxRetryDelay > 0 {
		opts = append(opts, heal.WithBackoff(min(time.Second, maxRetryDelay), maxRetryDelay))
	}
	return heal.NewQueue(heal.QueueDeps{Drives: local, Clock: clk, Logger: logger.With("component", "mrf")}, opts...)
}

// healQueueReporter publishes the counters of the MRF queue in the node
// information (Admin API server info, peer RPC); nil without a queue.
func healQueueReporter(q heal.Queue) cluster.HealQueueReporter {
	if q == nil {
		return nil
	}
	return cluster.HealQueueReporterFunc(func(ctx context.Context) domain.HealQueueStats {
		st := q.Stats(ctx)
		return domain.HealQueueStats{Pending: st.Pending + st.Leased, Healed: st.Healed, Retried: st.Retried, Dropped: st.Dropped}
	})
}

// newHealing builds the healing service (erasure-coded modes) and the
// scanner over the object layer, which applies the lifecycle of the
// buckets over its cycles. The usage of the scanner is saved on the
// single drive (SNSD) or on the drives of the first erasure set, which
// every node reads.
func newHealing(d composeDeps, objects object.Service, queue heal.Queue, dyn configsvc.Service, lifecycle scanner.Lifecycle, load scanner.LoadSampler) (healingServices, error) {
	out := healingServices{queue: queue}
	scanDeps := scanner.Deps{
		Objects: objects, Leader: d.f.leader, Config: dyn, Lifecycle: lifecycle, Load: load, Clock: d.clock, Logger: d.logger.With("component", "scanner"),
	}
	if d.f.layout.Mode == domain.ModeSNSD {
		scanDeps.Drives = d.f.local
	} else {
		healer, ok := objects.(object.Healer)
		if !ok {
			return out, fmt.Errorf("%w: the erasure object layer does not implement object.Healer", domain.ErrInvalidArgument)
		}
		pools, err := poolSpecs(d.f)
		if err != nil {
			return out, err
		}
		out.heal, err = heal.New(heal.Deps{
			Healer: healer, Pools: pools, Queue: queue, NodeID: d.f.nodeID, Clock: d.clock, Logger: d.logger.With("component", "heal"),
		})
		if err != nil {
			return out, err
		}
		scanDeps.Healer = healer
		scanDeps.Drives = pools[0].Sets[0]
	}
	var err error
	if out.scanner, err = scanner.New(scanDeps); err != nil {
		return out, err
	}
	return out, nil
}

// runner starts the queue, the healing service and the scanner in this
// order and stops them in reverse order.
func (h healingServices) runner() lifecycle.Runner {
	var runners []lifecycle.Runner
	for _, r := range []lifecycle.Runner{h.queue, h.heal, h.scanner} {
		if r != nil {
			runners = append(runners, r)
		}
	}
	return lifecycle.Funcs{
		OnStart: func(ctx context.Context) error {
			for i, r := range runners {
				if err := r.Start(ctx); err != nil {
					for j := i - 1; j >= 0; j-- {
						_ = runners[j].Stop(context.WithoutCancel(ctx))
					}
					return err
				}
			}
			return nil
		},
		OnStop: func(ctx context.Context) error {
			var first error
			for i := len(runners) - 1; i >= 0; i-- {
				if err := runners[i].Stop(ctx); err != nil && first == nil {
					first = err
				}
			}
			return first
		},
	}
}
