package app

import (
	"context"
	"path/filepath"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/event"
)

// eventsDir is the directory of the event queues on a local drive, under
// the system volume (.nexo.sys/events/<type>-<id>).
const eventsDir = "events"

// notificationRules adapts the bucket metadata store to
// event.BucketRules: the dispatcher routes the events with the
// notification configuration the bucket service stored (and validated).
type notificationRules struct {
	store bucket.MetadataStore
}

var _ event.BucketRules = notificationRules{}

// NotificationConfig implements event.BucketRules.
func (n notificationRules) NotificationConfig(ctx context.Context, name string) ([]byte, error) {
	m, err := n.store.Load(ctx, name)
	if err != nil {
		return nil, err
	}
	return m.Notification, nil
}

// eventQueueRoot returns the directory of the persistent queues of the
// notification targets without queue_dir: the system volume of the first
// local drive of this node (each node queues the events of the requests
// it serves); empty (in-memory queues) without a local drive.
func eventQueueRoot(f *foundation) string {
	for _, d := range f.local {
		if d != nil && d.Endpoint().Path != "" {
			return filepath.Join(d.Endpoint().Path, domain.SystemBucket, eventsDir)
		}
	}
	return ""
}
