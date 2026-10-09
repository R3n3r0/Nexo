package clitest

import (
	"context"
	"crypto/sha1" //nolint:gosec // the SHA1 checksum of S3, not a security primitive.
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"hash"
	"hash/crc32"
	"hash/crc64"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/internal/domain"
)

// memKMSKey is the KMS key reported for the SSE-S3 versions and the
// SSE-KMS ones written without a key.
const memKMSKey = "mem-kms-key"

// crc64NVMEPolynomial is the reversed polynomial of CRC-64/NVME.
const crc64NVMEPolynomial = 0x9a6c9329ac4bc9b5

// memEncryption returns the encryption stored for a write.
func memEncryption(r domain.SSERequest) domain.EncryptionInfo {
	switch r.Type {
	case domain.SSES3:
		return domain.EncryptionInfo{Type: domain.SSES3, KMSKeyID: memKMSKey}
	case domain.SSEKMS:
		key := r.KMSKeyID
		if key == "" {
			key = memKMSKey
		}
		return domain.EncryptionInfo{Type: domain.SSEKMS, KMSKeyID: key}
	case domain.SSEC:
		return domain.EncryptionInfo{Type: domain.SSEC, CustomerKeyMD5: r.CustomerKey.KeyMD5}
	}
	return domain.EncryptionInfo{}
}

// checkCustomerKey checks the SSE-C key of a read: an SSE-C version needs
// its key (400 without, 403 with another one); a key for a version that
// is not SSE-C is an error too.
func checkCustomerKey(v *memVersion, r domain.SSERequest) error {
	given := r.Type == domain.SSEC && !r.CustomerKey.IsZero()
	switch {
	case v.encryption.Type != domain.SSEC && given:
		return fmt.Errorf("%w: the object was not stored with SSE-C (InvalidRequest)", domain.ErrInvalidArgument)
	case v.encryption.Type == domain.SSEC && !given:
		return fmt.Errorf("%w: the object was stored using SSE-C: the key is required (InvalidRequest)", domain.ErrInvalidArgument)
	case v.encryption.Type == domain.SSEC && r.CustomerKey.KeyMD5 != v.encryption.CustomerKeyMD5:
		return fmt.Errorf("%w: the SSE-C key does not match", domain.ErrAccessDenied)
	}
	return nil
}

// memChecksum computes the additional checksum of data.
func memChecksum(algo domain.ChecksumAlgorithm, data []byte) (domain.Checksum, error) {
	var h hash.Hash
	switch algo {
	case domain.ChecksumCRC32:
		h = crc32.NewIEEE()
	case domain.ChecksumCRC32C:
		h = crc32.New(crc32.MakeTable(crc32.Castagnoli))
	case domain.ChecksumCRC64NVME:
		sum := crc64.Checksum(data, crc64.MakeTable(crc64NVMEPolynomial))
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], sum)
		return domain.Checksum{Algorithm: algo, Value: base64.StdEncoding.EncodeToString(b[:]), Type: domain.ChecksumTypeFullObject}, nil
	case domain.ChecksumSHA1:
		h = sha1.New() //nolint:gosec // the SHA1 checksum of S3.
	case domain.ChecksumSHA256:
		h = sha256.New()
	default:
		return domain.Checksum{}, fmt.Errorf("%w: unsupported checksum algorithm %q", domain.ErrInvalidArgument, algo)
	}
	_, _ = h.Write(data)
	return domain.Checksum{Algorithm: algo, Value: base64.StdEncoding.EncodeToString(h.Sum(nil)), Type: domain.ChecksumTypeFullObject}, nil
}

// lockedLocked reports why a version cannot be removed: a legal hold, a
// COMPLIANCE retention, or a GOVERNANCE retention without bypass.
func (m *MemS3) lockedLocked(v *memVersion, bypass bool) error {
	now := m.clock.Now()
	switch {
	case v.legalHold == domain.LegalHoldOn:
		return fmt.Errorf("%w: the version is under legal hold", domain.ErrAccessDenied)
	case v.retention.Active(now) && (v.retention.Mode == domain.RetentionCompliance || !bypass):
		return fmt.Errorf("%w: the version is protected by a %s retention", domain.ErrAccessDenied, v.retention.Mode)
	}
	return nil
}

// lockedBucketLocked returns a bucket that must have Object Lock.
func (m *MemS3) lockedBucketLocked(name string) (*memBucket, error) {
	b, err := m.bucketLocked(name)
	if err != nil {
		return nil, err
	}
	if !b.objectLock {
		return nil, fmt.Errorf("%w: bucket %s is missing Object Lock configuration (InvalidRequest)", domain.ErrInvalidArgument, name)
	}
	return b, nil
}

func (m *MemS3) getObjectLock(_ context.Context, bucket string) (*cli.ObjectLockConfig, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin("GetObjectLock"); err != nil {
		return nil, err
	}
	b, err := m.bucketLocked(bucket)
	if err != nil {
		return nil, err
	}
	return &cli.ObjectLockConfig{Enabled: b.objectLock, Default: b.defaultRetention}, nil
}

func (m *MemS3) setObjectLock(_ context.Context, bucket string, def domain.DefaultRetention) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin("SetObjectLock"); err != nil {
		return err
	}
	b, err := m.bucketLocked(bucket)
	if err != nil {
		return err
	}
	if !b.objectLock {
		return fmt.Errorf("%w: Object Lock can only be enabled at the creation of a bucket (InvalidBucketState)", domain.ErrConflict)
	}
	if err := def.Validate(); err != nil {
		return err
	}
	b.defaultRetention = def
	return nil
}

func (m *MemS3) getRetention(_ context.Context, ref cli.ObjectRef) (domain.ObjectRetention, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin("GetRetention"); err != nil {
		return domain.ObjectRetention{}, err
	}
	if _, err := m.lockedBucketLocked(ref.Bucket); err != nil {
		return domain.ObjectRetention{}, err
	}
	v, err := m.versionLocked(ref.Bucket, ref.Key, ref.VersionID)
	if err != nil {
		return domain.ObjectRetention{}, err
	}
	return v.retention, nil
}

func (m *MemS3) putRetention(_ context.Context, ref cli.ObjectRef, r domain.ObjectRetention, bypass bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin("PutRetention"); err != nil {
		return err
	}
	if _, err := m.lockedBucketLocked(ref.Bucket); err != nil {
		return err
	}
	v, err := m.versionLocked(ref.Bucket, ref.Key, ref.VersionID)
	if err != nil {
		return err
	}
	now := m.clock.Now()
	if !r.IsZero() && (!r.Mode.IsValid() || !r.RetainUntil.After(now)) {
		return fmt.Errorf("%w: the retain until date must be in the future", domain.ErrInvalidArgument)
	}
	// As the server: an active retention may be extended (a COMPLIANCE
	// one staying COMPLIANCE); shortening or removing a GOVERNANCE one
	// needs the bypass, a COMPLIANCE one never.
	if cur := v.retention; cur.Active(now) {
		extends := !r.IsZero() && !r.RetainUntil.Before(cur.RetainUntil)
		switch {
		case cur.Mode == domain.RetentionCompliance && (!extends || r.Mode != domain.RetentionCompliance):
			return fmt.Errorf("%w: a COMPLIANCE retention can only be extended", domain.ErrAccessDenied)
		case cur.Mode == domain.RetentionGovernance && !extends && !bypass:
			return fmt.Errorf("%w: shortening a GOVERNANCE retention needs the bypass", domain.ErrAccessDenied)
		}
	}
	v.retention = r
	return nil
}

func (m *MemS3) getLegalHold(_ context.Context, ref cli.ObjectRef) (domain.LegalHoldStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin("GetLegalHold"); err != nil {
		return "", err
	}
	if _, err := m.lockedBucketLocked(ref.Bucket); err != nil {
		return "", err
	}
	v, err := m.versionLocked(ref.Bucket, ref.Key, ref.VersionID)
	if err != nil {
		return "", err
	}
	if v.legalHold == "" {
		return domain.LegalHoldOff, nil
	}
	return v.legalHold, nil
}

func (m *MemS3) putLegalHold(_ context.Context, ref cli.ObjectRef, hold domain.LegalHoldStatus) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin("PutLegalHold"); err != nil {
		return err
	}
	if _, err := m.lockedBucketLocked(ref.Bucket); err != nil {
		return err
	}
	if !hold.IsValid() {
		return fmt.Errorf("%w: invalid legal hold %q", domain.ErrMalformedXML, hold)
	}
	v, err := m.versionLocked(ref.Bucket, ref.Key, ref.VersionID)
	if err != nil {
		return err
	}
	v.legalHold = hold
	return nil
}

func (m *MemS3) getLifecycle(_ context.Context, bucket string) (*cli.LifecycleConfiguration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin("GetLifecycle"); err != nil {
		return nil, err
	}
	b, err := m.bucketLocked(bucket)
	if err != nil {
		return nil, err
	}
	out := &cli.LifecycleConfiguration{Rules: []cli.LifecycleRule{}}
	if b.lifecycle != nil {
		out.Rules = append(out.Rules, b.lifecycle.Rules...)
		for i := range out.Rules {
			if out.Rules[i].Enabled == nil {
				enabled := true
				out.Rules[i].Enabled = &enabled
			}
		}
	}
	return out, nil
}

func (m *MemS3) putLifecycle(_ context.Context, bucket string, cfg cli.LifecycleConfiguration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin("PutLifecycle"); err != nil {
		return err
	}
	b, err := m.bucketLocked(bucket)
	if err != nil {
		return err
	}
	if len(cfg.Rules) == 0 {
		b.lifecycle = nil
		return nil
	}
	ids := map[string]bool{}
	for i, r := range cfg.Rules {
		switch {
		case r.ID == "":
			return fmt.Errorf("%w: rule %d has no ID", domain.ErrInvalidArgument, i+1)
		case ids[r.ID]:
			return fmt.Errorf("%w: duplicate rule ID %q", domain.ErrInvalidArgument, r.ID)
		case r.Expiration == nil && r.NoncurrentExpiration == nil && r.AbortIncompleteUpload == nil &&
			len(r.Transitions) == 0 && len(r.NoncurrentTransitions) == 0:
			return fmt.Errorf("%w: rule %q has no action (MalformedXML)", domain.ErrMalformedXML, r.ID)
		}
		if e := r.Expiration; e != nil && e.Days != nil && *e.Days <= 0 {
			return fmt.Errorf("%w: 'Days' for Expiration action must be a positive integer", domain.ErrInvalidArgument)
		}
		ids[r.ID] = true
	}
	stored := cfg
	stored.Rules = append([]cli.LifecycleRule(nil), cfg.Rules...)
	b.lifecycle = &stored
	return nil
}

func (m *MemS3) getCORS(_ context.Context, bucket string) (*cli.CORSConfiguration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin("GetCORS"); err != nil {
		return nil, err
	}
	b, err := m.bucketLocked(bucket)
	if err != nil {
		return nil, err
	}
	out := &cli.CORSConfiguration{Rules: []cli.CORSRule{}}
	if b.cors != nil {
		out.Rules = append(out.Rules, b.cors.Rules...)
	}
	return out, nil
}

func (m *MemS3) putCORS(_ context.Context, bucket string, cfg cli.CORSConfiguration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin("PutCORS"); err != nil {
		return err
	}
	b, err := m.bucketLocked(bucket)
	if err != nil {
		return err
	}
	if len(cfg.Rules) == 0 {
		b.cors = nil
		return nil
	}
	if err := cli.CORSDomain(cfg).Validate(); err != nil {
		return err
	}
	stored := cfg
	stored.Rules = append([]cli.CORSRule(nil), cfg.Rules...)
	b.cors = &stored
	return nil
}

func (m *MemS3) getNotifications(_ context.Context, bucket string) (*cli.BucketNotifications, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin("GetNotifications"); err != nil {
		return nil, err
	}
	b, err := m.bucketLocked(bucket)
	if err != nil {
		return nil, err
	}
	out := &cli.BucketNotifications{Subscriptions: []cli.EventSubscription{}}
	if b.notifications != nil {
		out.Subscriptions = append(out.Subscriptions, b.notifications.Subscriptions...)
	}
	return out, nil
}

func (m *MemS3) putNotifications(_ context.Context, bucket string, cfg cli.BucketNotifications) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin("PutNotifications"); err != nil {
		return err
	}
	b, err := m.bucketLocked(bucket)
	if err != nil {
		return err
	}
	for _, sub := range cfg.Subscriptions {
		if err := cli.ValidateARN(sub.ARN); err != nil {
			return fmt.Errorf("%w: %v", domain.ErrInvalidArgument, err)
		}
	}
	if len(cfg.Subscriptions) == 0 {
		b.notifications = nil
		return nil
	}
	stored := cfg
	stored.Subscriptions = make([]cli.EventSubscription, 0, len(cfg.Subscriptions))
	for i, sub := range cfg.Subscriptions {
		if sub.ID == "" {
			sub.ID = fmt.Sprintf("sub-%d", i+1)
		}
		stored.Subscriptions = append(stored.Subscriptions, sub)
	}
	b.notifications = &stored
	return nil
}
