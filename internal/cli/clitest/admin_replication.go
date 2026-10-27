package clitest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"slices"
	"sort"

	"github.com/stretchr/testify/mock"

	climock "github.com/r3n3r0/nexo/internal/cli/mock"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// The replication of the buckets (nexoctl replicate): the targets get an
// ARN derived from their endpoint and bucket like on the server, the rules
// are validated against them, the status is set by the tests
// (SetReplicationStatus) and the resyncs recorded.

// memReplState is the replication state of MemAdmin.
type memReplState struct {
	// targets and rules are by bucket.
	targets map[string][]nexoadmin.ReplicationTarget
	rules   map[string]nexoadmin.ReplicationConfiguration
	status  map[string]nexoadmin.ReplicationStatus
}

func (m *MemAdmin) replLocked() *memReplState {
	if m.repl.targets == nil {
		m.repl = memReplState{targets: map[string][]nexoadmin.ReplicationTarget{},
			rules: map[string]nexoadmin.ReplicationConfiguration{}, status: map[string]nexoadmin.ReplicationStatus{}}
	}
	return &m.repl
}

// SetReplicationStatus sets the status GetReplicationStatus returns for a
// bucket (its resync is kept).
func (m *MemAdmin) SetReplicationStatus(bucket string, st nexoadmin.ReplicationStatus) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.replLocked().status[bucket] = st
}

// ReplicationTargets returns the targets of a bucket, secrets included.
func (m *MemAdmin) ReplicationTargets(bucket string) []nexoadmin.ReplicationTarget {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.replLocked().targets[bucket])
}

// expectReplication registers the replication methods of the store on c.
func (s *memSession) expectReplication(c *climock.AdminClient) {
	x := mock.Anything
	c.EXPECT().GetBucketReplication(x, x).RunAndReturn(s.getBucketReplication).Maybe()
	c.EXPECT().SetBucketReplication(x, x, x).RunAndReturn(s.setBucketReplication).Maybe()
	c.EXPECT().DeleteBucketReplication(x, x).RunAndReturn(s.deleteBucketReplication).Maybe()
	c.EXPECT().ListReplicationTargets(x, x).RunAndReturn(s.listReplicationTargets).Maybe()
	c.EXPECT().SetReplicationTarget(x, x, x).RunAndReturn(s.setReplicationTarget).Maybe()
	c.EXPECT().RemoveReplicationTarget(x, x, x).RunAndReturn(s.removeReplicationTarget).Maybe()
	c.EXPECT().GetReplicationStatus(x, x).RunAndReturn(s.getReplicationStatus).Maybe()
	c.EXPECT().StartReplicationResync(x, x, x).RunAndReturn(s.startReplicationResync).Maybe()
}

// replBegin counts the call and checks the action; the caller unlocks.
func (s *memSession) replBegin(method, action string) error {
	if err := s.begin(method); err != nil {
		return err
	}
	return s.requireAdminLocked(action)
}

func memTargetARN(endpoint, bucket string) string {
	sum := sha256.Sum256([]byte(endpoint + "\x00" + bucket))
	return "arn:nexo:replication::" + hex.EncodeToString(sum[:6]) + ":" + bucket
}

func (s *memSession) getBucketReplication(_ context.Context, bucket string) (*nexoadmin.ReplicationConfiguration, error) {
	if err := s.replBegin("GetBucketReplication", "s3:GetReplicationConfiguration"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	cfg := s.m.replLocked().rules[bucket]
	cfg.Rules = slices.Clone(cfg.Rules)
	if cfg.Rules == nil {
		cfg.Rules = []nexoadmin.ReplicationRule{}
	}
	return &cfg, nil
}

func (s *memSession) setBucketReplication(_ context.Context, bucket string, cfg nexoadmin.ReplicationConfiguration) (*nexoadmin.ReplicationConfiguration, error) {
	if err := s.replBegin("SetBucketReplication", "s3:PutReplicationConfiguration"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	st := s.m.replLocked()
	ids := map[string]bool{}
	for i, r := range cfg.Rules {
		if r.ID == "" {
			return nil, invalid("rule %d has no id", i+1)
		}
		if ids[r.ID] {
			return nil, invalid("duplicate rule id %q", r.ID)
		}
		ids[r.ID] = true
		if !slices.ContainsFunc(st.targets[bucket], func(t nexoadmin.ReplicationTarget) bool { return t.ARN == r.Target }) {
			return nil, invalid("rule %q: %s is not a replication target of the bucket", r.ID, r.Target)
		}
	}
	if len(cfg.Rules) == 0 {
		delete(st.rules, bucket)
		return &nexoadmin.ReplicationConfiguration{Rules: []nexoadmin.ReplicationRule{}}, nil
	}
	cfg.Rules = slices.Clone(cfg.Rules)
	st.rules[bucket] = cfg
	out := nexoadmin.ReplicationConfiguration{Rules: slices.Clone(cfg.Rules)}
	return &out, nil
}

func (s *memSession) deleteBucketReplication(_ context.Context, bucket string) error {
	if err := s.replBegin("DeleteBucketReplication", "s3:PutReplicationConfiguration"); err != nil {
		return err
	}
	defer s.m.mu.Unlock()
	delete(s.m.replLocked().rules, bucket)
	return nil
}

func (s *memSession) listReplicationTargets(_ context.Context, bucket string) ([]nexoadmin.ReplicationTarget, error) {
	if err := s.replBegin("ListReplicationTargets", "admin:GetBucketTarget"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	out := make([]nexoadmin.ReplicationTarget, 0, len(s.m.replLocked().targets[bucket]))
	for _, t := range s.m.replLocked().targets[bucket] {
		t.SecretKey = ""
		out = append(out, t)
	}
	return out, nil
}

func (s *memSession) setReplicationTarget(_ context.Context, bucket string, t nexoadmin.ReplicationTarget) (*nexoadmin.ReplicationTarget, error) {
	if err := s.replBegin("SetReplicationTarget", "admin:SetBucketTarget"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	switch {
	case t.Endpoint == "" || t.Bucket == "":
		return nil, invalid("a target needs an endpoint and a bucket")
	case t.AccessKey == "":
		return nil, invalid("a target needs an access key")
	}
	st := s.m.replLocked()
	targets := st.targets[bucket]
	arn := memTargetARN(t.Endpoint, t.Bucket)
	if t.ARN != "" && t.ARN != arn {
		return nil, notFound("not_found", "no replication target %s", t.ARN)
	}
	t.ARN, t.Updated = arn, s.now()
	i := slices.IndexFunc(targets, func(o nexoadmin.ReplicationTarget) bool { return o.ARN == arn })
	switch {
	case i >= 0:
		t.Created = targets[i].Created
		if t.SecretKey == "" {
			t.SecretKey = targets[i].SecretKey
		}
		targets[i] = t
	case t.SecretKey == "":
		return nil, invalid("a new target needs a secret key")
	default:
		t.Created = t.Updated
		targets = append(targets, t)
	}
	sort.Slice(targets, func(a, b int) bool { return targets[a].ARN < targets[b].ARN })
	st.targets[bucket] = targets
	t.SecretKey = ""
	return &t, nil
}

func (s *memSession) removeReplicationTarget(_ context.Context, bucket, arn string) error {
	if err := s.replBegin("RemoveReplicationTarget", "admin:SetBucketTarget"); err != nil {
		return err
	}
	defer s.m.mu.Unlock()
	st := s.m.replLocked()
	if slices.ContainsFunc(st.rules[bucket].Rules, func(r nexoadmin.ReplicationRule) bool { return r.Target == arn }) {
		return conflict("the replication rules use the target %s", arn)
	}
	i := slices.IndexFunc(st.targets[bucket], func(t nexoadmin.ReplicationTarget) bool { return t.ARN == arn })
	if i < 0 {
		return notFound("not_found", "no replication target %s", arn)
	}
	st.targets[bucket] = slices.Delete(st.targets[bucket], i, i+1)
	return nil
}

func (s *memSession) getReplicationStatus(_ context.Context, bucket string) (*nexoadmin.ReplicationStatus, error) {
	if err := s.replBegin("GetReplicationStatus", "s3:GetReplicationConfiguration"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	out := s.m.replLocked().status[bucket]
	out.Targets = slices.Clone(out.Targets)
	if out.Targets == nil {
		out.Targets = []nexoadmin.ReplicationTargetStatus{}
	}
	return &out, nil
}

func (s *memSession) startReplicationResync(_ context.Context, bucket, target string) (*nexoadmin.ReplicationResync, error) {
	if err := s.replBegin("StartReplicationResync", "s3:ResetBucketReplicationState"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	st := s.m.replLocked()
	if !slices.ContainsFunc(st.rules[bucket].Rules, func(r nexoadmin.ReplicationRule) bool { return r.Target == target }) {
		return nil, apiError(http.StatusBadRequest, nexoadmin.CodeInvalidArgument, "no replication rule of the bucket uses "+target)
	}
	if cur := st.status[bucket].Resync; cur != nil && cur.State == "running" {
		return nil, conflict("a resync of the bucket is running")
	}
	s.m.seq++
	res := &nexoadmin.ReplicationResync{ID: fmt.Sprintf("resync-%04d", s.m.seq), Target: target, State: "running", StartedAt: s.now(), UpdatedAt: s.now()}
	status := st.status[bucket]
	cp := *res
	status.Resync = &cp
	st.status[bucket] = status
	return res, nil
}
