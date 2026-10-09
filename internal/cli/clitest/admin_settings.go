package clitest

import (
	"context"
	"net/http"
	"sort"
	"strings"

	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// SetBucket sets the summary GetBucket returns for a bucket (its usage).
func (m *MemAdmin) SetBucket(b nexoadmin.Bucket) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.buckets[b.Name] = b
}

// SetLifecycleApplyResult sets the outcome of ApplyBucketLifecycle.
func (m *MemAdmin) SetLifecycleApplyResult(res nexoadmin.LifecycleApplyResult) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lifecycleApply = res
}

// SetNotifyTargets sets the notification targets.
func (m *MemAdmin) SetNotifyTargets(targets ...nexoadmin.NotifyTarget) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.targets = append([]nexoadmin.NotifyTarget(nil), targets...)
}

// SetKMSReseal sets the outcome of ResealKMSKeys.
func (m *MemAdmin) SetKMSReseal(res nexoadmin.KMSResealResult) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.kmsReseal = res
}

// SetKMS sets the KMS state and its keys; nil means no KMS configured
// (the KMS methods answer 501).
func (m *MemAdmin) SetKMS(status *nexoadmin.KMSStatus, keys ...nexoadmin.KMSKey) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.kms, m.kmsKeys = status, append([]nexoadmin.KMSKey(nil), keys...)
}

func (s *memSession) getBucket(_ context.Context, bucket string) (*nexoadmin.Bucket, error) {
	if err := s.begin("GetBucket"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("s3:ListBucket"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	b, ok := s.m.buckets[bucket]
	if !ok {
		return nil, notFound("bucket_not_found", "bucket %s does not exist", bucket)
	}
	b.QuotaBytes = s.m.quotas[bucket].Size
	return &b, nil
}

func (s *memSession) applyBucketLifecycle(_ context.Context, bucket string) (*nexoadmin.LifecycleApplyResult, error) {
	if err := s.begin("ApplyBucketLifecycle"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:ApplyLifecycle"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	if _, ok := s.m.buckets[bucket]; !ok && len(s.m.buckets) > 0 {
		return nil, notFound("bucket_not_found", "bucket %s does not exist", bucket)
	}
	res := s.m.lifecycleApply
	return &res, nil
}

func (s *memSession) listNotifyTargets(context.Context) ([]nexoadmin.NotifyTarget, error) {
	if err := s.begin("ListNotifyTargets"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:ConfigUpdate"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	out := append([]nexoadmin.NotifyTarget{}, s.m.targets...)
	sort.Slice(out, func(i, j int) bool { return out[i].ARN < out[j].ARN })
	return out, nil
}

func (s *memSession) testNotifyTarget(_ context.Context, target string) (*nexoadmin.NotifyTargetTest, error) {
	if err := s.begin("TestNotifyTarget"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:ConfigUpdate"); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	for _, t := range s.m.targets {
		if t.ARN == target || t.ID+":"+t.Type == target {
			out := &nexoadmin.NotifyTargetTest{Target: t.ARN, Delivered: t.Online}
			if !t.Online {
				out.Error = t.LastError
				if out.Error == "" {
					out.Error = "target offline"
				}
			}
			return out, nil
		}
	}
	return nil, notFound("not_found", "notification target %s does not exist", target)
}

// kmsLocked returns the KMS state, or the 501 of a server without KMS
// (unlocking).
func (s *memSession) kmsLocked() (*nexoadmin.KMSStatus, error) {
	if s.m.kms == nil {
		s.m.mu.Unlock()
		return nil, apiError(http.StatusNotImplemented, "not_implemented", "server side encryption specified but KMS is not configured")
	}
	return s.m.kms, nil
}

func (s *memSession) kmsStatus(context.Context) (*nexoadmin.KMSStatus, error) {
	if err := s.begin("KMSStatus"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:KMSKeyStatus"); err != nil {
		return nil, err
	}
	st, err := s.kmsLocked()
	if err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	out := *st
	return &out, nil
}

func (s *memSession) listKMSKeys(_ context.Context, prefix string) ([]nexoadmin.KMSKey, error) {
	if err := s.begin("ListKMSKeys"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:KMSKeyStatus"); err != nil {
		return nil, err
	}
	if _, err := s.kmsLocked(); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	out := []nexoadmin.KMSKey{}
	for _, k := range s.m.kmsKeys {
		if strings.HasPrefix(k.ID, prefix) {
			out = append(out, k)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *memSession) createKMSKey(_ context.Context, id string) (*nexoadmin.KMSKey, error) {
	if err := s.begin("CreateKMSKey"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:KMSCreateKey"); err != nil {
		return nil, err
	}
	if _, err := s.kmsLocked(); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	if strings.TrimSpace(id) == "" {
		return nil, invalid("the key name is required")
	}
	for _, k := range s.m.kmsKeys {
		if k.ID == id {
			return nil, conflict("kms key %s already exists", id)
		}
	}
	created := s.now()
	key := nexoadmin.KMSKey{ID: id, CreatedAt: &created}
	s.m.kmsKeys = append(s.m.kmsKeys, key)
	return &key, nil
}

func (s *memSession) resealKMSKeys(_ context.Context, bucket string) (*nexoadmin.KMSResealResult, error) {
	if err := s.begin("ResealKMSKeys"); err != nil {
		return nil, err
	}
	if err := s.requireAdminLocked("admin:KMSCreateKey"); err != nil {
		return nil, err
	}
	if _, err := s.kmsLocked(); err != nil {
		return nil, err
	}
	defer s.m.mu.Unlock()
	if _, ok := s.m.buckets[bucket]; bucket != "" && !ok && len(s.m.buckets) > 0 {
		return nil, notFound("bucket_not_found", "bucket %s does not exist", bucket)
	}
	res := s.m.kmsReseal
	return &res, nil
}
