package bucket

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/lock"
	"github.com/r3n3r0/nexo/internal/service/object"
)

// Limits of the bucket configuration documents.
const (
	maxBucketTags     = 50
	maxTagKeyLength   = 128
	maxTagValueLength = 256
)

// service is the default bucket implementation: the object layer owns the
// bucket directories, the metadata store owns the configuration documents.
type service struct {
	deps Deps
	opts options
}

var _ Service = (*service)(nil)

// New returns the bucket service. Objects, Store and Policy are required.
func New(deps Deps, opts ...Option) (Service, error) {
	if deps.Objects == nil {
		return nil, fmt.Errorf("%w: bucket: Objects dependency is required", domain.ErrInvalidArgument)
	}
	if deps.Store == nil {
		return nil, fmt.Errorf("%w: bucket: Store dependency is required", domain.ErrInvalidArgument)
	}
	if deps.Policy == nil {
		return nil, fmt.Errorf("%w: bucket: Policy dependency is required", domain.ErrInvalidArgument)
	}
	if deps.Locks == nil {
		deps.Locks = lock.NewLocal(lock.Deps{})
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &service{deps: deps, opts: newOptions(opts)}, nil
}

// noConfig builds the S3 "no such configuration" error of a sub-resource.
func noConfig(code, message, bucket string) error {
	return domain.NewS3Error(domain.ErrPolicyNotFound, code, message).WithResource(bucket)
}

// Create implements Service.
func (s *service) Create(ctx context.Context, in CreateInput) (*domain.Bucket, error) {
	if err := domain.ValidateBucketName(in.Name); err != nil {
		return nil, err
	}
	if domain.IsReservedBucket(in.Name) {
		return nil, fmt.Errorf("%w: %q is reserved", domain.ErrInvalidBucketName, in.Name)
	}
	region := in.Region
	if region == "" {
		region = s.opts.region
	}
	versioning := domain.VersioningOff
	if in.ObjectLock {
		versioning = domain.VersioningEnabled
	}
	if err := s.deps.Objects.MakeBucket(ctx, in.Name, object.MakeBucketOptions{Region: region, LockEnabled: in.ObjectLock, Versioning: versioning}); err != nil {
		return nil, fmt.Errorf("bucket: create %s: %w", in.Name, err)
	}
	created := s.deps.Clock.Now().UTC()
	if info, err := s.deps.Objects.GetBucketInfo(ctx, in.Name); err == nil && !info.Created.IsZero() {
		created = info.Created
	}
	m := &Metadata{
		Name:       in.Name,
		Created:    created,
		Region:     region,
		Versioning: versioning,
		ObjectLock: ObjectLockConfig{Enabled: in.ObjectLock},
		UpdatedAt:  s.deps.Clock.Now().UTC(),
	}
	if err := s.deps.Store.Save(ctx, in.Name, m); err != nil {
		// Without its configuration the bucket would silently lose the
		// requested object lock: undo the creation, even when the client
		// went away (the cause of many failed saves).
		if derr := s.deps.Objects.DeleteBucket(context.WithoutCancel(ctx), in.Name, object.DeleteBucketOptions{}); derr != nil {
			s.deps.Logger.WarnContext(ctx, "bucket: cannot roll back creation", "bucket", in.Name, "err", derr)
		}
		return nil, fmt.Errorf("bucket: create %s: %w", in.Name, err)
	}
	return &domain.Bucket{Name: in.Name, Created: created, Region: region}, nil
}

// Get implements Service.
func (s *service) Get(ctx context.Context, name string) (*domain.BucketInfo, error) {
	info, err := s.deps.Objects.GetBucketInfo(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("bucket: get %s: %w", name, err)
	}
	m, err := s.deps.Store.Load(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("bucket: get %s: %w", name, err)
	}
	merged := merge(info, m)
	return &merged, nil
}

// List implements Service.
func (s *service) List(ctx context.Context, in ListInput) ([]domain.BucketInfo, error) {
	var (
		infos []domain.BucketInfo
		err   error
	)
	if in.WithUsage {
		infos, err = s.deps.Objects.DataUsage(ctx, "")
	} else {
		infos, err = s.deps.Objects.ListBuckets(ctx)
	}
	if err != nil {
		return nil, fmt.Errorf("bucket: list: %w", err)
	}
	out := make([]domain.BucketInfo, 0, len(infos))
	for _, info := range infos {
		if in.Prefix != "" && !strings.HasPrefix(info.Name, in.Prefix) {
			continue
		}
		m, err := s.deps.Store.Load(ctx, info.Name)
		if err != nil {
			s.deps.Logger.WarnContext(ctx, "bucket: metadata unavailable", "bucket", info.Name, "err", err)
			out = append(out, info)
			continue
		}
		out = append(out, merge(info, m))
	}
	return out, nil
}

// ListPage implements Service. The continuation token is the URL-safe
// base64 of the last name of the previous page.
func (s *service) ListPage(ctx context.Context, in ListPageInput) (ListPageResult, error) {
	if in.MaxBuckets < 0 || in.MaxBuckets > MaxListBuckets {
		return ListPageResult{}, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidArgument",
			fmt.Sprintf("max-buckets must be between 1 and %d", MaxListBuckets))
	}
	after := ""
	if in.ContinuationToken != "" {
		raw, err := base64.RawURLEncoding.DecodeString(in.ContinuationToken)
		if err != nil || len(raw) == 0 {
			return ListPageResult{}, domain.NewS3Error(domain.ErrInvalidArgument, "InvalidArgument", "The continuation token provided is incorrect")
		}
		after = string(raw)
	}
	all, err := s.List(ctx, ListInput{Prefix: in.Prefix})
	if err != nil {
		return ListPageResult{}, err
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	start := sort.Search(len(all), func(i int) bool { return all[i].Name > after })
	page := all[start:]
	var res ListPageResult
	if in.MaxBuckets > 0 && len(page) > in.MaxBuckets {
		page = page[:in.MaxBuckets]
		res.ContinuationToken = base64.RawURLEncoding.EncodeToString([]byte(page[len(page)-1].Name))
	}
	res.Buckets = page
	return res, nil
}

// merge overlays the stored configuration on the object layer's view.
func merge(info domain.BucketInfo, m *Metadata) domain.BucketInfo {
	if m == nil {
		return info
	}
	if m.Region != "" {
		info.Region = m.Region
	}
	if !m.Created.IsZero() {
		info.Created = m.Created
	}
	info.Versioning = m.Versioning
	info.ObjectLockEnabled = m.ObjectLock.Enabled
	info.QuotaBytes = m.Quota.Size
	if len(m.Tags) > 0 {
		info.Tags = make(map[string]string, len(m.Tags))
		for k, v := range m.Tags {
			info.Tags[k] = v
		}
	}
	return info
}

// Delete implements Service.
func (s *service) Delete(ctx context.Context, name string, opts DeleteOptions) error {
	if err := s.deps.Objects.DeleteBucket(ctx, name, object.DeleteBucketOptions{Force: opts.Force}); err != nil {
		return fmt.Errorf("bucket: delete %s: %w", name, err)
	}
	if err := s.deps.Store.Delete(ctx, name); err != nil {
		return fmt.Errorf("bucket: delete %s: %w", name, err)
	}
	return nil
}

// load returns the metadata of an existing bucket.
func (s *service) load(ctx context.Context, name string) (*Metadata, error) {
	if _, err := s.deps.Objects.GetBucketInfo(ctx, name); err != nil {
		return nil, fmt.Errorf("bucket: %s: %w", name, err)
	}
	m, err := s.deps.Store.Load(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("bucket: %s: %w", name, err)
	}
	return m, nil
}

// update loads, mutates and saves the metadata of an existing bucket under
// the bucket's configuration lock, so that concurrent changes of different
// settings never overwrite each other.
func (s *service) update(ctx context.Context, name string, mutate func(m *Metadata) error) error {
	if err := domain.ValidateBucketName(name); err != nil {
		return err
	}
	lc, err := s.deps.Locks.NewNSLock(ctx, domain.SystemBucket, metadataPath(name)).GetLock(ctx, s.opts.lockTimeout)
	if err != nil {
		return fmt.Errorf("bucket: %s: %w", name, err)
	}
	defer lc.Unlock(ctx)
	m, err := s.load(ctx, name)
	if err != nil {
		return err
	}
	if err := mutate(m); err != nil {
		return err
	}
	m.UpdatedAt = s.deps.Clock.Now().UTC()
	if err := s.deps.Store.Save(ctx, name, m); err != nil {
		return fmt.Errorf("bucket: %s: %w", name, err)
	}
	return nil
}

// isNotFound reports a missing document.
func isNotFound(err error) bool { return errors.Is(err, domain.ErrObjectNotFound) }

// Invalidate implements Service.
func (s *service) Invalidate(ctx context.Context, name string) {
	s.deps.Store.Invalidate(ctx, name)
}

// GetVersioning implements Service.
func (s *service) GetVersioning(ctx context.Context, name string) (domain.Versioning, error) {
	m, err := s.load(ctx, name)
	if err != nil {
		return domain.VersioningOff, err
	}
	return m.Versioning, nil
}

// SetVersioning implements Service.
func (s *service) SetVersioning(ctx context.Context, name string, v domain.Versioning) error {
	if v != domain.VersioningEnabled && v != domain.VersioningSuspended {
		return domain.NewS3Error(domain.ErrInvalidArgument, "IllegalVersioningConfigurationException", "The Versioning element must be Enabled or Suspended")
	}
	return s.update(ctx, name, func(m *Metadata) error {
		if m.ObjectLock.Enabled && v != domain.VersioningEnabled {
			return domain.NewS3Error(domain.ErrConflict, "InvalidBucketState", "An Object Lock configuration is present on this bucket, so the versioning state cannot be changed.")
		}
		if len(m.Replication) > 0 && v != domain.VersioningEnabled {
			return domain.NewS3Error(domain.ErrConflict, "InvalidBucketState", "A replication configuration is present on this bucket, so the versioning state cannot be changed.")
		}
		m.Versioning = v
		return nil
	})
}

// GetPolicy implements Service.
func (s *service) GetPolicy(ctx context.Context, name string) (json.RawMessage, error) {
	m, err := s.load(ctx, name)
	if err != nil {
		return nil, err
	}
	if len(m.Policy) == 0 {
		return nil, noConfig("NoSuchBucketPolicy", "The bucket policy does not exist", name)
	}
	return m.Policy, nil
}

// SetPolicy implements Service.
func (s *service) SetPolicy(ctx context.Context, name string, policy json.RawMessage) error {
	if len(policy) == 0 || len(policy) > s.opts.maxDocumentSize || !json.Valid(policy) {
		return domain.NewS3Error(domain.ErrInvalidArgument, "MalformedPolicy", "Policies must be valid JSON and the first byte must be '{'")
	}
	if _, err := s.deps.Policy.ParseBucketPolicy(ctx, name, policy); err != nil {
		return fmt.Errorf("bucket: policy of %s: %w", name, err)
	}
	return s.update(ctx, name, func(m *Metadata) error {
		m.Policy = append(json.RawMessage(nil), policy...)
		return nil
	})
}

// IsPublic implements Service.
func (s *service) IsPublic(ctx context.Context, name string) (bool, error) {
	raw, err := s.GetPolicy(ctx, name)
	if err != nil {
		return false, err
	}
	doc, err := s.deps.Policy.Parse(ctx, raw)
	if err != nil {
		return false, fmt.Errorf("bucket: stored policy of %s: %w", name, err)
	}
	return s.deps.Policy.IsPublic(ctx, doc), nil
}

// DeletePolicy implements Service.
func (s *service) DeletePolicy(ctx context.Context, name string) error {
	return s.update(ctx, name, func(m *Metadata) error {
		m.Policy = nil
		return nil
	})
}

// validateXML checks that a configuration document is well-formed XML whose
// root element is one of the given names.
func (s *service) validateXML(doc []byte, roots ...string) error {
	if len(doc) == 0 || len(doc) > s.opts.maxDocumentSize {
		return fmt.Errorf("%w: configuration document is empty or too large", domain.ErrMalformedXML)
	}
	dec := xml.NewDecoder(bytes.NewReader(doc))
	rootSeen := false
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("%w: %v", domain.ErrMalformedXML, err)
		}
		if start, ok := tok.(xml.StartElement); ok && !rootSeen {
			rootSeen = true
			match := len(roots) == 0
			for _, r := range roots {
				if start.Name.Local == r {
					match = true
				}
			}
			if !match {
				return fmt.Errorf("%w: unexpected root element %q", domain.ErrMalformedXML, start.Name.Local)
			}
		}
	}
	if !rootSeen {
		return fmt.Errorf("%w: no root element", domain.ErrMalformedXML)
	}
	return nil
}

// GetLifecycle implements Service.
func (s *service) GetLifecycle(ctx context.Context, name string) ([]byte, error) {
	m, err := s.load(ctx, name)
	if err != nil {
		return nil, err
	}
	if len(m.Lifecycle) == 0 {
		return nil, noConfig("NoSuchLifecycleConfiguration", "The lifecycle configuration does not exist", name)
	}
	return m.Lifecycle, nil
}

// SetLifecycle implements Service.
func (s *service) SetLifecycle(ctx context.Context, name string, xmlDoc []byte) error {
	if err := s.validateXML(xmlDoc, "LifecycleConfiguration"); err != nil {
		return err
	}
	return s.update(ctx, name, func(m *Metadata) error {
		m.Lifecycle = append([]byte(nil), xmlDoc...)
		return nil
	})
}

// DeleteLifecycle implements Service.
func (s *service) DeleteLifecycle(ctx context.Context, name string) error {
	return s.update(ctx, name, func(m *Metadata) error {
		m.Lifecycle = nil
		return nil
	})
}

// GetReplication implements Service.
func (s *service) GetReplication(ctx context.Context, name string) ([]byte, error) {
	m, err := s.load(ctx, name)
	if err != nil {
		return nil, err
	}
	if len(m.Replication) == 0 {
		return nil, noConfig("ReplicationConfigurationNotFoundError", "The replication configuration was not found", name)
	}
	return m.Replication, nil
}

// SetReplication implements Service.
func (s *service) SetReplication(ctx context.Context, name string, xmlDoc []byte) error {
	if err := s.validateXML(xmlDoc, "ReplicationConfiguration"); err != nil {
		return err
	}
	return s.update(ctx, name, func(m *Metadata) error {
		if m.Versioning != domain.VersioningEnabled {
			return domain.NewS3Error(domain.ErrInvalidArgument, "InvalidRequest", "Versioning must be 'Enabled' on the bucket to apply a replication configuration")
		}
		m.Replication = append([]byte(nil), xmlDoc...)
		return nil
	})
}

// DeleteReplication implements Service.
func (s *service) DeleteReplication(ctx context.Context, name string) error {
	return s.update(ctx, name, func(m *Metadata) error {
		m.Replication = nil
		return nil
	})
}

// GetObjectLock implements Service.
func (s *service) GetObjectLock(ctx context.Context, name string) (ObjectLockConfig, error) {
	m, err := s.load(ctx, name)
	if err != nil {
		return ObjectLockConfig{}, err
	}
	if !m.ObjectLock.Enabled {
		return ObjectLockConfig{}, noConfig("ObjectLockConfigurationNotFoundError", "Object Lock configuration does not exist for this bucket", name)
	}
	return m.ObjectLock, nil
}

// SetObjectLock implements Service.
func (s *service) SetObjectLock(ctx context.Context, name string, cfg ObjectLockConfig) error {
	if !cfg.Enabled {
		return domain.NewS3Error(domain.ErrMalformedXML, "MalformedXML", "ObjectLockEnabled must be Enabled")
	}
	if err := cfg.DefaultRetention().Validate(); err != nil {
		return domain.NewS3Error(domain.ErrMalformedXML, "MalformedXML",
			"The XML you provided was not well-formed or did not validate against our published schema: "+err.Error())
	}
	return s.update(ctx, name, func(m *Metadata) error {
		// As in S3, a bucket created without object locking can enable it
		// once its versioning is Enabled (it can never be suspended then).
		if !m.ObjectLock.Enabled && m.Versioning != domain.VersioningEnabled {
			return domain.NewS3Error(domain.ErrConflict, "InvalidBucketState",
				"Versioning must be 'Enabled' on the bucket to apply a Object Lock configuration")
		}
		m.ObjectLock = cfg
		return nil
	})
}

// DefaultRetention returns the default retention of the configuration
// (zero when none).
func (c ObjectLockConfig) DefaultRetention() domain.DefaultRetention {
	return domain.DefaultRetention{Mode: c.Mode, Days: c.Days, Years: c.Years}
}

// GetEncryption implements Service.
func (s *service) GetEncryption(ctx context.Context, name string) (EncryptionConfig, error) {
	m, err := s.load(ctx, name)
	if err != nil {
		return EncryptionConfig{}, err
	}
	if m.Encryption.Algorithm == "" {
		return EncryptionConfig{}, noConfig("ServerSideEncryptionConfigurationNotFoundError", "The server side encryption configuration was not found", name)
	}
	return m.Encryption, nil
}

// SetEncryption implements Service.
func (s *service) SetEncryption(ctx context.Context, name string, cfg EncryptionConfig) error {
	switch cfg.Algorithm {
	case domain.SSEAlgorithmAES256:
		if cfg.KMSKeyID != "" {
			return domain.NewS3Error(domain.ErrInvalidArgument, "InvalidArgument", "KMSMasterKeyID is only valid with aws:kms")
		}
	case domain.SSEAlgorithmKMS:
	default:
		return domain.NewS3Error(domain.ErrMalformedXML, "MalformedXML", "SSEAlgorithm must be AES256 or aws:kms")
	}
	if err := s.checkKMS(ctx, cfg.KMSKeyID); err != nil {
		return err
	}
	return s.update(ctx, name, func(m *Metadata) error {
		m.Encryption = cfg
		return nil
	})
}

// checkKMS verifies that the KMS can serve a default encryption: it is
// configured and, for a key id, knows the key.
func (s *service) checkKMS(ctx context.Context, keyID string) error {
	if s.deps.KMS == nil {
		return domain.NewS3Error(domain.ErrKMSNotConfigured, "NotImplemented", "Server side encryption specified but KMS is not configured")
	}
	st, err := s.deps.KMS.Status(ctx)
	if err != nil {
		return fmt.Errorf("bucket: kms status: %w", err)
	}
	if !st.Configured {
		return domain.NewS3Error(domain.ErrKMSNotConfigured, "NotImplemented", "Server side encryption specified but KMS is not configured")
	}
	if keyID == "" || keyID == st.DefaultKey {
		return nil
	}
	keys, err := s.deps.KMS.ListKeys(ctx, keyID)
	if err != nil {
		return fmt.Errorf("bucket: kms keys: %w", err)
	}
	for _, k := range keys {
		if k.Name == keyID {
			return nil
		}
	}
	return domain.NewS3Error(domain.ErrKMSKeyNotFound, "KMS.NotFoundException", "Invalid keyId "+keyID)
}

// DeleteEncryption implements Service.
func (s *service) DeleteEncryption(ctx context.Context, name string) error {
	return s.update(ctx, name, func(m *Metadata) error {
		m.Encryption = EncryptionConfig{}
		return nil
	})
}

// GetQuota implements Service.
func (s *service) GetQuota(ctx context.Context, name string) (Quota, error) {
	m, err := s.load(ctx, name)
	if err != nil {
		return Quota{}, err
	}
	return m.Quota, nil
}

// SetQuota implements Service.
func (s *service) SetQuota(ctx context.Context, name string, q Quota) (Quota, error) {
	if q.Size > 0 && q.Type == "" {
		q.Type = QuotaHard
	}
	if q.Type != "" && q.Type != QuotaHard {
		return Quota{}, fmt.Errorf("%w: unsupported quota type %q", domain.ErrInvalidArgument, q.Type)
	}
	if q.Size == 0 {
		q = Quota{}
	}
	err := s.update(ctx, name, func(m *Metadata) error {
		m.Quota = q
		return nil
	})
	if err != nil {
		return Quota{}, err
	}
	return q, nil
}

// GetTags implements Service.
func (s *service) GetTags(ctx context.Context, name string) (map[string]string, error) {
	m, err := s.load(ctx, name)
	if err != nil {
		return nil, err
	}
	if len(m.Tags) == 0 {
		return nil, noConfig("NoSuchTagSet", "The TagSet does not exist", name)
	}
	return m.Tags, nil
}

// SetTags implements Service.
func (s *service) SetTags(ctx context.Context, name string, tags map[string]string) error {
	if err := validateTags(tags); err != nil {
		return err
	}
	return s.update(ctx, name, func(m *Metadata) error {
		if len(tags) == 0 {
			m.Tags = nil
			return nil
		}
		m.Tags = make(map[string]string, len(tags))
		for k, v := range tags {
			m.Tags[k] = v
		}
		return nil
	})
}

// DeleteTags implements Service.
func (s *service) DeleteTags(ctx context.Context, name string) error {
	return s.update(ctx, name, func(m *Metadata) error {
		m.Tags = nil
		return nil
	})
}

// GetCORS implements Service.
func (s *service) GetCORS(ctx context.Context, name string) ([]byte, error) {
	m, err := s.load(ctx, name)
	if err != nil {
		return nil, err
	}
	if len(m.CORS) == 0 {
		return nil, noConfig("NoSuchCORSConfiguration", "The CORS configuration does not exist", name)
	}
	return m.CORS, nil
}

// SetCORS implements Service.
func (s *service) SetCORS(ctx context.Context, name string, xmlDoc []byte) error {
	if err := s.validateXML(xmlDoc, "CORSConfiguration"); err != nil {
		return err
	}
	cfg, err := parseCORS(xmlDoc)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	return s.update(ctx, name, func(m *Metadata) error {
		m.CORS = append([]byte(nil), xmlDoc...)
		return nil
	})
}

// GetCORSConfig implements Service.
func (s *service) GetCORSConfig(ctx context.Context, name string) (domain.CORSConfiguration, error) {
	doc, err := s.GetCORS(ctx, name)
	if err != nil {
		return domain.CORSConfiguration{}, err
	}
	cfg, err := parseCORS(doc)
	if err != nil {
		return domain.CORSConfiguration{}, fmt.Errorf("bucket: stored CORS configuration of %s: %w", name, err)
	}
	return cfg, nil
}

// DeleteCORS implements Service.
func (s *service) DeleteCORS(ctx context.Context, name string) error {
	return s.update(ctx, name, func(m *Metadata) error {
		m.CORS = nil
		return nil
	})
}

// GetMedia implements Service.
func (s *service) GetMedia(ctx context.Context, name string) (domain.MediaConfig, error) {
	m, err := s.load(ctx, name)
	if err != nil {
		return domain.MediaConfig{}, err
	}
	if m.Media == nil || len(m.Media.Rules) == 0 {
		return domain.MediaConfig{}, noConfig("NoSuchMediaConfiguration", "The media configuration does not exist", name)
	}
	return m.Media.Clone(), nil
}

// SetMedia implements Service.
func (s *service) SetMedia(ctx context.Context, name string, cfg domain.MediaConfig) error {
	cfg = cfg.Clone()
	if err := cfg.Validate(); err != nil {
		return err
	}
	if len(cfg.Rules) == 0 {
		return s.DeleteMedia(ctx, name)
	}
	return s.update(ctx, name, func(m *Metadata) error {
		m.Media = &cfg
		return nil
	})
}

// DeleteMedia implements Service.
func (s *service) DeleteMedia(ctx context.Context, name string) error {
	return s.update(ctx, name, func(m *Metadata) error {
		m.Media = nil
		return nil
	})
}

// GetMediaProtection implements Service.
func (s *service) GetMediaProtection(ctx context.Context, name string) (domain.MediaProtection, error) {
	m, err := s.load(ctx, name)
	if err != nil {
		return domain.MediaProtection{}, err
	}
	if m.MediaProtection == nil {
		return domain.MediaProtection{}, nil
	}
	return m.MediaProtection.Clone(), nil
}

// SetMediaProtection implements Service.
func (s *service) SetMediaProtection(ctx context.Context, name string, p domain.MediaProtection) error {
	p = p.Clone()
	if err := p.Validate(); err != nil {
		return err
	}
	return s.update(ctx, name, func(m *Metadata) error {
		m.MediaProtection = &p
		return nil
	})
}

// GetNotification implements Service. A bucket without configuration
// yields an empty NotificationConfiguration document, as S3 does.
func (s *service) GetNotification(ctx context.Context, name string) ([]byte, error) {
	m, err := s.load(ctx, name)
	if err != nil {
		return nil, err
	}
	if len(m.Notification) == 0 {
		return []byte(`<NotificationConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></NotificationConfiguration>`), nil
	}
	return m.Notification, nil
}

// SetNotification implements Service.
func (s *service) SetNotification(ctx context.Context, name string, xmlDoc []byte) error {
	if err := s.validateXML(xmlDoc, "NotificationConfiguration"); err != nil {
		return err
	}
	return s.update(ctx, name, func(m *Metadata) error {
		m.Notification = append([]byte(nil), xmlDoc...)
		return nil
	})
}
