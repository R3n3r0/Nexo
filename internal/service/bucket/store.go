package bucket

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/storage"
)

// StoreDeps are the dependencies of the drive-backed metadata store.
type StoreDeps struct {
	// Drives receive every document (all the local drives of the deployment).
	Drives []storage.StorageAPI
	Logger *slog.Logger
}

// metadataStore keeps one JSON document per bucket on every drive, under
// .nexo.sys/buckets/<bucket>/metadata.json, with an in-memory cache.
type metadataStore struct {
	deps StoreDeps

	mu    sync.RWMutex
	cache map[string]*Metadata
}

var _ MetadataStore = (*metadataStore)(nil)

// NewStore returns a MetadataStore writing to every drive of deps.
func NewStore(deps StoreDeps) (MetadataStore, error) {
	if len(deps.Drives) == 0 {
		return nil, fmt.Errorf("%w: bucket: NewStore requires at least one drive", domain.ErrInvalidArgument)
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &metadataStore{deps: deps, cache: make(map[string]*Metadata)}, nil
}

func metadataPath(bucket string) string {
	return storage.BucketsDir + "/" + bucket + "/metadata.json"
}

// quorum is the number of drives that must acknowledge a write.
func (s *metadataStore) quorum() int { return len(s.deps.Drives)/2 + 1 }

// Load implements MetadataStore.
func (s *metadataStore) Load(ctx context.Context, bucket string) (*Metadata, error) {
	if err := domain.ValidateBucketName(bucket); err != nil {
		return nil, err
	}
	s.mu.RLock()
	cached, ok := s.cache[bucket]
	s.mu.RUnlock()
	if ok {
		return cached.clone(), nil
	}
	var firstErr error
	found := false
	var m Metadata
	for _, d := range s.deps.Drives {
		if d == nil || !d.IsOnline() {
			continue
		}
		data, err := d.ReadAll(ctx, domain.SystemBucket, metadataPath(bucket))
		if err != nil {
			if !errors.Is(err, domain.ErrObjectNotFound) && firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := json.Unmarshal(data, &m); err != nil {
			s.deps.Logger.WarnContext(ctx, "bucket: unreadable metadata document", "bucket", bucket, "drive", d.Endpoint().String(), "err", err)
			continue
		}
		found = true
		break
	}
	if !found {
		if firstErr != nil {
			return nil, fmt.Errorf("bucket: load metadata of %s: %w", bucket, firstErr)
		}
		m = Metadata{Name: bucket}
	}
	s.mu.Lock()
	s.cache[bucket] = m.clone()
	s.mu.Unlock()
	return &m, nil
}

// Save implements MetadataStore.
func (s *metadataStore) Save(ctx context.Context, bucket string, m *Metadata) error {
	if err := domain.ValidateBucketName(bucket); err != nil {
		return err
	}
	if m == nil {
		return fmt.Errorf("%w: nil bucket metadata", domain.ErrInvalidArgument)
	}
	m.Name = bucket
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("bucket: encode metadata of %s: %w", bucket, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cache, bucket)
	if err := s.each(ctx, func(d storage.StorageAPI) error {
		return d.WriteAll(ctx, domain.SystemBucket, metadataPath(bucket), data)
	}); err != nil {
		return fmt.Errorf("bucket: save metadata of %s: %w", bucket, err)
	}
	s.cache[bucket] = m.clone()
	return nil
}

// Delete implements MetadataStore.
func (s *metadataStore) Delete(ctx context.Context, bucket string) error {
	if err := domain.ValidateBucketName(bucket); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cache, bucket)
	if err := s.each(ctx, func(d storage.StorageAPI) error {
		err := d.Delete(ctx, domain.SystemBucket, storage.BucketsDir+"/"+bucket, storage.DeleteOptions{Recursive: true})
		if errors.Is(err, domain.ErrObjectNotFound) {
			return nil
		}
		return err
	}); err != nil {
		return fmt.Errorf("bucket: delete metadata of %s: %w", bucket, err)
	}
	return nil
}

// BucketConfig implements object.BucketConfigSource.
func (s *metadataStore) BucketConfig(ctx context.Context, bucket string) (object.BucketConfig, error) {
	m, err := s.Load(ctx, bucket)
	if err != nil {
		return object.BucketConfig{}, err
	}
	return object.BucketConfig{Versioning: m.Versioning, ObjectLock: m.ObjectLock.Enabled, QuotaBytes: m.Quota.Size}, nil
}

// each runs fn on every online drive and succeeds when a majority did.
func (s *metadataStore) each(ctx context.Context, fn func(d storage.StorageAPI) error) error {
	errs := make([]error, len(s.deps.Drives))
	var wg sync.WaitGroup
	for i, d := range s.deps.Drives {
		if d == nil || !d.IsOnline() {
			errs[i] = fmt.Errorf("%w: drive %d offline", domain.ErrDiskNotFound, i)
			continue
		}
		wg.Add(1)
		go func(i int, d storage.StorageAPI) {
			defer wg.Done()
			errs[i] = fn(d)
		}(i, d)
	}
	wg.Wait()
	ok := 0
	var first error
	for _, err := range errs {
		if err == nil {
			ok++
		} else if first == nil {
			first = err
		}
	}
	if ok >= s.quorum() {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("%w: %d of %d drives acknowledged: %v", domain.ErrWriteQuorum, ok, len(s.deps.Drives), first)
}

// clone returns a deep copy so that callers cannot mutate the cache.
func (m *Metadata) clone() *Metadata {
	if m == nil {
		return nil
	}
	c := *m
	if m.Tags != nil {
		c.Tags = make(map[string]string, len(m.Tags))
		for k, v := range m.Tags {
			c.Tags[k] = v
		}
	}
	c.Policy = append(json.RawMessage(nil), m.Policy...)
	c.Lifecycle = append([]byte(nil), m.Lifecycle...)
	c.CORS = append([]byte(nil), m.CORS...)
	c.Notification = append([]byte(nil), m.Notification...)
	return &c
}
