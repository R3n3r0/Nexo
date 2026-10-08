package bucket

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
)

// Option configures the bucket service.
type Option func(*options)

type options struct {
	// region is the default region of new buckets.
	region string
	// maxDocumentSize bounds the stored XML/JSON documents.
	maxDocumentSize int
}

// WithRegion sets the default region of new buckets (default us-east-1).
func WithRegion(region string) Option {
	return func(o *options) {
		if region != "" {
			o.region = region
		}
	}
}

// WithMaxDocumentSize bounds policy and XML configuration documents
// (default 1 MiB).
func WithMaxDocumentSize(n int) Option {
	return func(o *options) {
		if n > 0 {
			o.maxDocumentSize = n
		}
	}
}

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

// New returns the bucket service. Objects and Store are required.
func New(deps Deps, opts ...Option) (Service, error) {
	if deps.Objects == nil {
		return nil, fmt.Errorf("%w: bucket: Objects dependency is required", domain.ErrInvalidArgument)
	}
	if deps.Store == nil {
		return nil, fmt.Errorf("%w: bucket: Store dependency is required", domain.ErrInvalidArgument)
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	o := options{region: "us-east-1", maxDocumentSize: 1 << 20}
	for _, opt := range opts {
		opt(&o)
	}
	return &service{deps: deps, opts: o}, nil
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
	now := s.deps.Clock.Now().UTC()
	m := &Metadata{
		Name:       in.Name,
		Created:    now,
		Region:     region,
		Versioning: versioning,
		ObjectLock: ObjectLockConfig{Enabled: in.ObjectLock},
		UpdatedAt:  now,
	}
	if err := s.deps.Store.Save(ctx, in.Name, m); err != nil {
		return nil, fmt.Errorf("bucket: create %s: %w", in.Name, err)
	}
	return &domain.Bucket{Name: in.Name, Created: now, Region: region}, nil
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
	infos, err := s.deps.Objects.ListBuckets(ctx)
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

// update loads, mutates and saves the metadata of an existing bucket.
func (s *service) update(ctx context.Context, name string, mutate func(m *Metadata) error) error {
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
	var doc domain.Policy
	if err := json.Unmarshal(policy, &doc); err != nil || doc.Version != domain.PolicyVersion || len(doc.Statement) == 0 {
		return domain.NewS3Error(domain.ErrInvalidArgument, "MalformedPolicy", "Policy has invalid version or no statements")
	}
	return s.update(ctx, name, func(m *Metadata) error {
		m.Policy = append(json.RawMessage(nil), policy...)
		return nil
	})
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
	if cfg.Mode != "" && cfg.Mode != RetentionGovernance && cfg.Mode != RetentionCompliance {
		return domain.NewS3Error(domain.ErrMalformedXML, "MalformedXML", "Invalid retention mode "+string(cfg.Mode))
	}
	if (cfg.Days > 0 && cfg.Years > 0) || cfg.Days < 0 || cfg.Years < 0 {
		return domain.NewS3Error(domain.ErrMalformedXML, "MalformedXML", "Specify either Days or Years, not both")
	}
	if (cfg.Days > 0 || cfg.Years > 0) && cfg.Mode == "" {
		return domain.NewS3Error(domain.ErrMalformedXML, "MalformedXML", "A default retention period requires a Mode")
	}
	return s.update(ctx, name, func(m *Metadata) error {
		if !cfg.Enabled {
			return domain.NewS3Error(domain.ErrMalformedXML, "MalformedXML", "ObjectLockEnabled must be Enabled")
		}
		if m.Versioning != domain.VersioningEnabled {
			return domain.NewS3Error(domain.ErrConflict, "InvalidBucketState", "Object Lock configuration cannot be enabled on buckets without versioning")
		}
		m.ObjectLock = cfg
		return nil
	})
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
	case "AES256":
		if cfg.KMSKeyID != "" {
			return domain.NewS3Error(domain.ErrInvalidArgument, "InvalidArgument", "KMSMasterKeyID is only valid with aws:kms")
		}
	case "aws:kms":
	default:
		return domain.NewS3Error(domain.ErrMalformedXML, "MalformedXML", "SSEAlgorithm must be AES256 or aws:kms")
	}
	return s.update(ctx, name, func(m *Metadata) error {
		m.Encryption = cfg
		return nil
	})
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
func (s *service) SetQuota(ctx context.Context, name string, q Quota) error {
	if q.Size > 0 && q.Type == "" {
		q.Type = QuotaHard
	}
	if q.Type != "" && q.Type != QuotaHard {
		return fmt.Errorf("%w: unsupported quota type %q", domain.ErrInvalidArgument, q.Type)
	}
	if q.Size == 0 {
		q = Quota{}
	}
	return s.update(ctx, name, func(m *Metadata) error {
		m.Quota = q
		return nil
	})
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
	if len(tags) > maxBucketTags {
		return domain.NewS3Error(domain.ErrInvalidArgument, "BadRequest", "Bucket tag count cannot be greater than 50")
	}
	for k, v := range tags {
		if k == "" || len(k) > maxTagKeyLength || len(v) > maxTagValueLength {
			return domain.NewS3Error(domain.ErrInvalidArgument, "InvalidTag", "The TagKey or TagValue you have provided is invalid")
		}
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
	return s.update(ctx, name, func(m *Metadata) error {
		m.CORS = append([]byte(nil), xmlDoc...)
		return nil
	})
}

// DeleteCORS implements Service.
func (s *service) DeleteCORS(ctx context.Context, name string) error {
	return s.update(ctx, name, func(m *Metadata) error {
		m.CORS = nil
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
