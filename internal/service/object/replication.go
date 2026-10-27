package object

import (
	"context"
	"fmt"
	"strings"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/crypto"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// replicaKey marks the context of a request of the replication of another
// site.
type replicaKey struct{}

// WithReplica marks ctx as the context of a write or a deletion made by
// the replication of another site (S-BKT-12): the replication of this
// site does not send it back (no loop between sites replicating to each
// other).
func WithReplica(ctx context.Context) context.Context {
	return context.WithValue(ctx, replicaKey{}, true)
}

// IsReplica reports a context marked by WithReplica.
func IsReplica(ctx context.Context) bool {
	v, _ := ctx.Value(replicaKey{}).(bool)
	return v
}

// setReplicationMeta records the replication state of a new version in
// meta: REPLICA for a version written by the replication of another site;
// PENDING, with every destination of the rules that select it, for a
// version the replication configuration of the bucket selects (by key and
// tags). An SSE-C version is never replicated: its key is the client's.
func setReplicationMeta(meta map[string]string, cfg BucketConfig, key string, replica bool) {
	for k := range meta {
		if k == metaReplicationStatus || strings.HasPrefix(k, metaReplicationTargetPrefix) {
			delete(meta, k)
		}
	}
	if replica {
		meta[metaReplicationStatus] = string(domain.ReplicationReplica)
		return
	}
	if cfg.Replication == nil || crypto.InfoOf(meta).Type == domain.SSEC {
		return
	}
	rules := cfg.Replication.Destinations(key, decodeTags(meta[metaTagging]))
	if len(rules) == 0 {
		return
	}
	meta[metaReplicationStatus] = string(domain.ReplicationPending)
	for _, r := range rules {
		meta[metaReplicationTargetPrefix+r.Destination.Bucket] = string(domain.ReplicationPending)
	}
}

// keepReplicaETag stores the ETag a replica has at its source in place of
// the one computed here, when the version is stored unencrypted (the ETag
// of an encrypted version is the one its encryption gives).
func keepReplicaETag(meta map[string]string, etag string) {
	if etag != "" && crypto.InfoOf(meta).Type == domain.SSENone {
		meta[metaETag] = etag
	}
}

// SetReplicationStatus implements Service.
func (l *layer) SetReplicationStatus(ctx context.Context, bucket, key, versionID, target string, status domain.ReplicationStatus) (domain.ObjectInfo, error) {
	if target == "" || !domain.ValidReplicationStatus(status) {
		return domain.ObjectInfo{}, fmt.Errorf("%w: replication state %q of target %q", domain.ErrInvalidArgument, status, target)
	}
	return l.updateMeta(ctx, bucket, key, versionID, func(_ storage.FileInfo, meta map[string]string) error {
		meta[metaReplicationTargetPrefix+target] = string(status)
		states := map[string]domain.ReplicationStatus{}
		for k, v := range meta {
			if t, ok := strings.CutPrefix(k, metaReplicationTargetPrefix); ok {
				states[t] = domain.ReplicationStatus(v)
			}
		}
		meta[metaReplicationStatus] = string(domain.OverallReplicationStatus(states))
		return nil
	}, "")
}
