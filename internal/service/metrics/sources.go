package metrics

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/cluster"
	"github.com/r3n3r0/nexo/internal/service/event"
	"github.com/r3n3r0/nexo/internal/service/heal"
	"github.com/r3n3r0/nexo/internal/service/kms"
	"github.com/r3n3r0/nexo/internal/service/scanner"
)

// Sources are the services whose state RegisterSources exposes as
// families read at every collection. A nil field is left out (the mode
// has no such service: no MRF queue in SNSD, no peers on a single node).
type Sources struct {
	// Node describes this node and its local drives (drive families,
	// uptime, build).
	Node cluster.Info
	// HealQueue is the MRF queue of this node (nexo_mrf_queue_*).
	HealQueue heal.Queue
	// Heal lists the healing jobs (nexo_heal_jobs).
	Heal heal.Service
	// Scanner reports the scanner of this node (nexo_scanner_*).
	Scanner scanner.Service
	// Events reports the notification targets (nexo_events_pending,
	// nexo_events_target_online).
	Events event.Service
	// KMS reports the state of the KMS (nexo_kms_online); a deployment
	// without KMS leaves it nil.
	KMS kms.Service
	// Peers are the other nodes (nexo_rpc_peer_online).
	Peers []cluster.Peer
}

// observed is one family registered by RegisterSources.
type observed struct {
	def  Definition
	kind Kind
	fn   ObserveFunc
}

// RegisterSources registers the families read from the sources at every
// collection. The node information (one DiskInfo per drive) is read once
// per collection for every drive family.
func RegisterSources(ctx context.Context, reg Registry, src Sources) error {
	if reg == nil {
		return fmt.Errorf("%w: metrics: the sources need a registry", domain.ErrInvalidArgument)
	}
	var all []observed
	if src.Node != nil {
		all = append(all, nodeSources(src.Node)...)
	}
	if src.HealQueue != nil {
		all = append(all, healQueueSources(src.HealQueue)...)
	}
	if src.Heal != nil {
		all = append(all, healJobSources(src.Heal)...)
	}
	if src.Scanner != nil {
		all = append(all, scannerSources(src.Scanner)...)
	}
	if src.Events != nil {
		all = append(all, eventSources(src.Events)...)
	}
	if src.KMS != nil {
		all = append(all, kmsSources(src.KMS)...)
	}
	if len(src.Peers) > 0 {
		peers := src.Peers
		all = append(all, observed{Definition{Subsystem: "rpc", Name: "peer_online", Help: "Whether this node reaches a peer (circuit closed).",
			Labels: []string{"peer"}}, KindGauge, func(context.Context) []Observation {
			out := make([]Observation, 0, len(peers))
			for _, p := range peers {
				out = append(out, Observation{LabelValues: []string{p.NodeID()}, Value: boolValue(p.IsOnline())})
			}
			return out
		}})
	}
	for _, o := range all {
		if err := reg.Observe(ctx, o.def, o.kind, o.fn); err != nil {
			return err
		}
	}
	return nil
}

// memo calls fn at most once per ttl, sharing its result between the
// families of one collection.
type memo[T any] struct {
	ttl time.Duration
	fn  func(ctx context.Context) (T, error)

	mu  sync.Mutex
	at  time.Time
	val T
	err error
}

func (m *memo[T]) get(ctx context.Context) (T, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.at.IsZero() && time.Since(m.at) < m.ttl {
		return m.val, m.err
	}
	m.val, m.err = m.fn(ctx)
	m.at = time.Now()
	return m.val, m.err
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// driveLabels are the labels of the drive families.
func driveLabels() []string { return []string{"drive", "pool", "set"} }

// nodeSources are the families of the node and of its local drives.
func nodeSources(info cluster.Info) []observed {
	m := &memo[domain.NodeInfo]{ttl: time.Second, fn: info.Local}
	drives := func(value func(d domain.DiskInfo) (float64, bool)) ObserveFunc {
		return func(ctx context.Context) []Observation {
			ni, err := m.get(ctx)
			if err != nil {
				return nil
			}
			out := make([]Observation, 0, len(ni.Disks))
			for _, d := range ni.Disks {
				v, ok := value(d)
				if !ok {
					continue
				}
				out = append(out, Observation{LabelValues: []string{d.Endpoint, strconv.Itoa(d.PoolIdx), strconv.Itoa(d.SetIdx)}, Value: v})
			}
			return out
		}
	}
	online := func(d domain.DiskInfo) bool { return d.State == domain.StateOnline || d.State == domain.StateHealing }
	size := func(get func(d domain.DiskInfo) uint64) func(d domain.DiskInfo) (float64, bool) {
		return func(d domain.DiskInfo) (float64, bool) { return float64(get(d)), online(d) }
	}
	healing := func(get func(p *domain.HealingProgress) float64) func(d domain.DiskInfo) (float64, bool) {
		return func(d domain.DiskInfo) (float64, bool) {
			if d.HealingProgress == nil {
				return 0, false
			}
			return get(d.HealingProgress), true
		}
	}
	gauge := func(name, help string, fn ObserveFunc) observed {
		return observed{Definition{Subsystem: "drive", Name: name, Help: help, Labels: driveLabels()}, KindGauge, fn}
	}
	healGauge := func(name, help string, fn ObserveFunc) observed {
		return observed{Definition{Subsystem: "heal", Name: name, Help: help, Labels: driveLabels()}, KindGauge, fn}
	}
	return []observed{
		gauge("online", "Whether a local drive answers (1) or not (0); a healing drive is online.",
			drives(func(d domain.DiskInfo) (float64, bool) { return boolValue(online(d)), true })),
		gauge("healing", "Whether a local drive is being repopulated by the healing.",
			drives(func(d domain.DiskInfo) (float64, bool) {
				return boolValue(d.Healing || d.State == domain.StateHealing), true
			})),
		gauge("total_bytes", "Capacity of a local drive.", drives(size(func(d domain.DiskInfo) uint64 { return d.Total }))),
		gauge("used_bytes", "Used space of a local drive.", drives(size(func(d domain.DiskInfo) uint64 { return d.Used }))),
		gauge("free_bytes", "Free space of a local drive.", drives(size(func(d domain.DiskInfo) uint64 { return d.Free }))),
		gauge("used_inodes", "Used inodes of a local drive.", drives(size(func(d domain.DiskInfo) uint64 { return d.UsedInodes }))),
		gauge("free_inodes", "Free inodes of a local drive.", drives(size(func(d domain.DiskInfo) uint64 { return d.FreeInodes }))),
		healGauge("drive_objects_healed", "Objects healed on a drive being repopulated.",
			drives(healing(func(p *domain.HealingProgress) float64 { return float64(p.ObjectsHealed) }))),
		healGauge("drive_objects_failed", "Objects that could not be healed on a drive being repopulated.",
			drives(healing(func(p *domain.HealingProgress) float64 { return float64(p.ObjectsFailed) }))),
		healGauge("drive_bytes_healed", "Bytes healed on a drive being repopulated.",
			drives(healing(func(p *domain.HealingProgress) float64 { return float64(p.BytesDone) }))),
		healGauge("drive_started_timestamp_seconds", "When the healing of a drive started.",
			drives(healing(func(p *domain.HealingProgress) float64 { return float64(p.Started.Unix()) }))),
		healGauge("drive_last_update_timestamp_seconds", "When the healing of a drive last made progress (a stalled healing stops moving it).",
			drives(healing(func(p *domain.HealingProgress) float64 { return float64(p.LastUpdate.Unix()) }))),
		{Definition{Subsystem: "node", Name: "uptime_seconds", Help: "Time since the server process started."}, KindGauge,
			func(ctx context.Context) []Observation {
				ni, err := m.get(ctx)
				if err != nil {
					return nil
				}
				return []Observation{{Value: ni.Uptime.Seconds()}}
			}},
		{Definition{Subsystem: "node", Name: "info", Help: "Build of the server (value 1).", Labels: []string{"version", "commit"}}, KindGauge,
			func(ctx context.Context) []Observation {
				ni, err := m.get(ctx)
				if err != nil {
					return nil
				}
				return []Observation{{LabelValues: []string{ni.Version, ni.Commit}, Value: 1}}
			}},
	}
}

// healQueueSources are the families of the MRF queue.
func healQueueSources(q heal.Queue) []observed {
	m := &memo[heal.QueueStats]{ttl: time.Second, fn: func(ctx context.Context) (heal.QueueStats, error) { return q.Stats(ctx), nil }}
	one := func(name, help string, kind Kind, get func(s heal.QueueStats) float64) observed {
		return observed{Definition{Subsystem: "mrf", Name: name, Help: help}, kind, func(ctx context.Context) []Observation {
			st, _ := m.get(ctx)
			return []Observation{{Value: get(st)}}
		}}
	}
	return []observed{
		one("queue_pending", "Objects waiting in the background healing queue (MRF) of this node.", KindGauge,
			func(s heal.QueueStats) float64 { return float64(s.Pending) }),
		one("queue_leased", "Objects of the healing queue being healed.", KindGauge,
			func(s heal.QueueStats) float64 { return float64(s.Leased) }),
		one("queue_added_total", "Objects reported to the healing queue.", KindCounter,
			func(s heal.QueueStats) float64 { return float64(s.Queued) }),
		one("queue_merged_total", "Reports merged into an object already queued.", KindCounter,
			func(s heal.QueueStats) float64 { return float64(s.Merged) }),
		one("queue_dropped_total", "Objects refused (queue full) or abandoned (too many attempts).", KindCounter,
			func(s heal.QueueStats) float64 { return float64(s.Dropped) }),
		one("queue_healed_total", "Objects of the queue healed.", KindCounter,
			func(s heal.QueueStats) float64 { return float64(s.Healed) }),
		one("queue_retried_total", "Healing attempts of the queue that failed and were rescheduled.", KindCounter,
			func(s heal.QueueStats) float64 { return float64(s.Retried) }),
	}
}

// healJobSources are the families of the healing jobs.
func healJobSources(h heal.Service) []observed {
	m := &memo[[]heal.Job]{ttl: time.Second, fn: h.Jobs}
	running := func(get func(j heal.Job) uint64) ObserveFunc {
		return func(ctx context.Context) []Observation {
			jobs, err := m.get(ctx)
			if err != nil {
				return nil
			}
			var sum float64
			for _, j := range jobs {
				if j.State == heal.JobRunning {
					sum += float64(get(j))
				}
			}
			return []Observation{{Value: sum}}
		}
	}
	return []observed{
		{Definition{Subsystem: "heal", Name: "jobs", Help: "Healing jobs known to this node, by state.", Labels: []string{"state"}}, KindGauge,
			func(ctx context.Context) []Observation {
				jobs, err := m.get(ctx)
				if err != nil {
					return nil
				}
				counts := map[heal.JobState]float64{heal.JobRunning: 0, heal.JobDone: 0, heal.JobFailed: 0, heal.JobCanceled: 0}
				for _, j := range jobs {
					counts[j.State]++
				}
				out := make([]Observation, 0, len(counts))
				for s, n := range counts {
					out = append(out, Observation{LabelValues: []string{string(s)}, Value: n})
				}
				return out
			}},
		{Definition{Subsystem: "heal", Name: "running_objects_scanned", Help: "Items examined by the running healing jobs."}, KindGauge,
			running(func(j heal.Job) uint64 { return j.Scanned })},
		{Definition{Subsystem: "heal", Name: "running_objects_healed", Help: "Items healed by the running healing jobs."}, KindGauge,
			running(func(j heal.Job) uint64 { return j.Healed })},
		{Definition{Subsystem: "heal", Name: "running_objects_failed", Help: "Items the running healing jobs could not heal."}, KindGauge,
			running(func(j heal.Job) uint64 { return j.Failed })},
	}
}

// scannerSources are the families of the scanner.
func scannerSources(s scanner.Service) []observed {
	m := &memo[scanner.Status]{ttl: time.Second, fn: func(ctx context.Context) (scanner.Status, error) { return s.Status(ctx), nil }}
	one := func(name, help string, kind Kind, get func(st scanner.Status) (float64, bool)) observed {
		return observed{Definition{Subsystem: "scanner", Name: name, Help: help}, kind, func(ctx context.Context) []Observation {
			st, _ := m.get(ctx)
			v, ok := get(st)
			if !ok {
				return nil
			}
			return []Observation{{Value: v}}
		}}
	}
	last := func(get func(c scanner.CycleStats) float64) func(st scanner.Status) (float64, bool) {
		return func(st scanner.Status) (float64, bool) { return get(st.Last), st.Last.Cycle > 0 }
	}
	return []observed{
		one("active", "Whether this node runs the scanner (the leader).", KindGauge,
			func(st scanner.Status) (float64, bool) { return boolValue(st.Active), true }),
		one("scanning", "Whether a scanner cycle is in progress.", KindGauge,
			func(st scanner.Status) (float64, bool) { return boolValue(st.Scanning), true }),
		one("cycles_total", "Scanner cycles started by this node.", KindCounter,
			func(st scanner.Status) (float64, bool) { return float64(st.Cycle), true }),
		one("last_cycle_duration_seconds", "Duration of the last completed scanner cycle.", KindGauge,
			last(func(c scanner.CycleStats) float64 { return c.Finished.Sub(c.Started).Seconds() })),
		one("last_cycle_timestamp_seconds", "When the last completed scanner cycle ended.", KindGauge,
			last(func(c scanner.CycleStats) float64 { return float64(c.Finished.Unix()) })),
		one("last_cycle_buckets", "Buckets scanned by the last cycle.", KindGauge,
			last(func(c scanner.CycleStats) float64 { return float64(c.Buckets) })),
		one("last_cycle_objects", "Objects scanned by the last cycle.", KindGauge,
			last(func(c scanner.CycleStats) float64 { return float64(c.Objects) })),
		one("last_cycle_versions", "Versions and delete markers scanned by the last cycle.", KindGauge,
			last(func(c scanner.CycleStats) float64 { return float64(c.Versions) })),
		one("last_cycle_heal_checked", "Objects of the healing sample of the last cycle.", KindGauge,
			last(func(c scanner.CycleStats) float64 { return float64(c.HealChecked) })),
		one("last_cycle_heal_repaired", "Objects of the sample found damaged and repaired by the last cycle.", KindGauge,
			last(func(c scanner.CycleStats) float64 { return float64(c.HealRepaired) })),
		one("last_cycle_heal_failed", "Objects of the sample that could not be healed by the last cycle.", KindGauge,
			last(func(c scanner.CycleStats) float64 { return float64(c.HealFailed) })),
		one("last_cycle_ilm_actions", "Versions the lifecycle rules acted on during the last cycle.", KindGauge,
			last(func(c scanner.CycleStats) float64 { return float64(c.LifecycleActions) })),
		one("last_cycle_uploads_cleaned", "Multipart uploads aborted during the last cycle.", KindGauge,
			last(func(c scanner.CycleStats) float64 { return float64(c.UploadsCleaned) })),
	}
}

// eventSources are the families of the notification targets.
func eventSources(e event.Service) []observed {
	m := &memo[[]event.TargetStatus]{ttl: time.Second, fn: e.TargetsStatus}
	per := func(get func(st event.TargetStatus) float64) ObserveFunc {
		return func(ctx context.Context) []Observation {
			targets, err := m.get(ctx)
			if err != nil {
				return nil
			}
			out := make([]Observation, 0, len(targets))
			for _, t := range targets {
				out = append(out, Observation{LabelValues: []string{t.Config.TargetID().String()}, Value: get(t)})
			}
			return out
		}
	}
	return []observed{
		{Definition{Subsystem: "events", Name: "pending", Help: "Bucket notifications waiting in the queue of a target.", Labels: []string{"target"}}, KindGauge,
			per(func(st event.TargetStatus) float64 { return float64(st.Pending) })},
		{Definition{Subsystem: "events", Name: "target_online", Help: "Whether the last delivery to a target succeeded.", Labels: []string{"target"}}, KindGauge,
			per(func(st event.TargetStatus) float64 { return boolValue(st.Online) })},
	}
}

// kmsSources are the families of the KMS.
func kmsSources(k kms.Service) []observed {
	return []observed{
		{Definition{Subsystem: "kms", Name: "online", Help: "Whether the KMS of the server-side encryption answers."}, KindGauge,
			func(ctx context.Context) []Observation {
				st, err := k.Status(ctx)
				if errors.Is(err, domain.ErrKMSNotConfigured) || (err == nil && !st.Configured) {
					return nil
				}
				return []Observation{{Value: boolValue(err == nil && st.Online)}}
			}},
	}
}
