package app

import (
	"context"
	"path/filepath"
	"sync"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/certs"
	"github.com/r3n3r0/nexo/internal/service/event"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/replication"
	"github.com/r3n3r0/nexo/internal/service/sysstore"
)

// replicationDir holds the replication tasks of a node on its first local
// drive (.nexo.sys/replication).
const replicationDir = "replication"

// replicationEvents publishes the events of the object layer and turns
// them into replication tasks (S-BKT-12) once the replication service is
// composed: the object layer is built before it.
type replicationEvents struct {
	event.Service
	mu  sync.RWMutex
	svc replication.Service
}

// Publish implements event.Service: the event first, then its task.
func (r *replicationEvents) Publish(ctx context.Context, e event.Event) error {
	err := r.Service.Publish(ctx, e)
	r.mu.RLock()
	svc := r.svc
	r.mu.RUnlock()
	if svc != nil {
		svc.Notify(ctx, e)
	}
	return err
}

func (r *replicationEvents) bind(svc replication.Service) {
	r.mu.Lock()
	r.svc = svc
	r.mu.Unlock()
}

// newReplication builds the bucket replication: its targets in the system
// documents, their secrets sealed with a key of the deployment, its tasks
// queued on the first local drive of this node, its resyncs on the leader.
func newReplication(d composeDeps, objects object.Service, buckets object.BucketConfigSource, docs sysstore.Store) (replication.Service, error) {
	sealer, err := certs.NewSealer(replication.DeriveKey(d.cfg.RootUser, d.cfg.RootPassword, d.deploymentID))
	if err != nil {
		return nil, err
	}
	queueDir := ""
	for _, drive := range d.f.local {
		if drive != nil && drive.Endpoint().Path != "" {
			queueDir = filepath.Join(drive.Endpoint().Path, domain.SystemBucket, replicationDir)
			break
		}
	}
	deps := replication.Deps{Objects: objects, Buckets: buckets, Docs: docs, Sealer: sealer, QueueDir: queueDir,
		Clock: d.clock, Logger: d.logger.With("component", "replication")}
	if d.f.leader != nil {
		deps.Leader = d.f.leader
	}
	return replication.New(deps)
}
