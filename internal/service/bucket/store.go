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
	"github.com/r3n3r0/nexo/internal/service/sysstore"
)

// StoreDeps are the dependencies of the drive-backed metadata store.
type StoreDeps struct {
	// Drives receive every document (all the local drives of the deployment).
	Drives []storage.StorageAPI
	// Docs, when set, holds the documents instead of Drives: the
	// replicated system document store of the erasure-coded deployments
	// (SNMD, distributed), shared by every node.
	Docs   sysstore.Store
	Logger *slog.Logger
}

// metadataFile is the name of the configuration document of a bucket,
// in its system directory (.nexo.sys/buckets/<bucket>/). The object layer
// keeps its own record (.bucket.json) in the same directory.
const metadataFile = ".metadata.json"

// metadataStore keeps one JSON document per bucket on every drive, under
// .nexo.sys/buckets/<bucket>/.metadata.json, with an in-memory cache.
//
// The cache is guarded by a generation counter bumped by every change, so
// that a Load racing with a Save, a Delete or an Invalidate never caches
// the document it read before the change.
type metadataStore struct {
	deps StoreDeps

	mu    sync.RWMutex
	cache map[string]*Metadata
	gen   uint64
}

var _ MetadataStore = (*metadataStore)(nil)

// NewStore returns a MetadataStore writing to every drive of deps.
func NewStore(deps StoreDeps) (MetadataStore, error) {
	if len(deps.Drives) == 0 && deps.Docs == nil {
		return nil, fmt.Errorf("%w: bucket: NewStore requires at least one drive or the document store", domain.ErrInvalidArgument)
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &metadataStore{deps: deps, cache: make(map[string]*Metadata)}, nil
}

func metadataPath(bucket string) string {
	return storage.BucketsDir + "/" + bucket + "/" + metadataFile
}

// docsPath is the path of the document of a bucket in the document store.
func docsPath(bucket string) string { return storage.BucketsDir + "/" + bucket + ".json" }

// quorum is the number of drives that must acknowledge a write.
func (s *metadataStore) quorum() int { return len(s.deps.Drives)/2 + 1 }

// Load implements MetadataStore.
func (s *metadataStore) Load(ctx context.Context, bucket string) (*Metadata, error) {
	if err := domain.ValidateBucketName(bucket); err != nil {
		return nil, err
	}
	s.mu.RLock()
	cached, ok := s.cache[bucket]
	gen := s.gen
	s.mu.RUnlock()
	if ok {
		return cached.clone(), nil
	}
	m, err := s.read(ctx, bucket)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.gen == gen {
		s.cache[bucket] = m.clone()
	}
	s.mu.Unlock()
	return m, nil
}

// read loads the document from the document store, or from the first
// drive that has a readable copy.
func (s *metadataStore) read(ctx context.Context, bucket string) (*Metadata, error) {
	if s.deps.Docs != nil {
		data, err := s.deps.Docs.Get(ctx, docsPath(bucket))
		if errors.Is(err, sysstore.ErrNotFound) {
			return &Metadata{Name: bucket}, nil
		}
		if err != nil {
			return nil, fmt.Errorf("bucket: load metadata of %s: %w", bucket, err)
		}
		var m Metadata
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, fmt.Errorf("bucket: load metadata of %s: %w: %v", bucket, domain.ErrInvalidFormat, err)
		}
		m.Name = bucket
		return &m, nil
	}
	var firstErr error
	for _, d := range s.deps.Drives {
		if d == nil || !d.IsOnline() {
			continue
		}
		data, err := d.ReadAll(ctx, domain.SystemBucket, metadataPath(bucket))
		if err != nil {
			if !isNotFound(err) && firstErr == nil {
				firstErr = err
			}
			continue
		}
		var m Metadata
		if err := json.Unmarshal(data, &m); err != nil {
			s.deps.Logger.WarnContext(ctx, "bucket: unreadable metadata document", "bucket", bucket, "drive", d.Endpoint().String(), "err", err)
			continue
		}
		m.Name = bucket
		return &m, nil
	}
	if firstErr != nil {
		return nil, fmt.Errorf("bucket: load metadata of %s: %w", bucket, firstErr)
	}
	return &Metadata{Name: bucket}, nil
}

// Save implements MetadataStore.
func (s *metadataStore) Save(ctx context.Context, bucket string, m *Metadata) error {
	if err := domain.ValidateBucketName(bucket); err != nil {
		return err
	}
	if m == nil {
		return fmt.Errorf("%w: nil bucket metadata", domain.ErrInvalidArgument)
	}
	saved := m.clone()
	saved.Name = bucket
	data, err := json.Marshal(saved)
	if err != nil {
		return fmt.Errorf("bucket: encode metadata of %s: %w", bucket, err)
	}
	s.drop(bucket)
	if s.deps.Docs != nil {
		err = s.deps.Docs.Put(ctx, docsPath(bucket), data)
	} else {
		err = s.each(ctx, func(d storage.StorageAPI) error {
			return d.WriteAll(ctx, domain.SystemBucket, metadataPath(bucket), data)
		})
	}
	s.mu.Lock()
	s.gen++
	if err == nil {
		s.cache[bucket] = saved
	} else {
		delete(s.cache, bucket)
	}
	s.mu.Unlock()
	if err != nil {
		return fmt.Errorf("bucket: save metadata of %s: %w", bucket, err)
	}
	m.Name = bucket
	return nil
}

// Delete implements MetadataStore.
func (s *metadataStore) Delete(ctx context.Context, bucket string) error {
	if err := domain.ValidateBucketName(bucket); err != nil {
		return err
	}
	s.drop(bucket)
	var err error
	if s.deps.Docs != nil {
		err = s.deps.Docs.Delete(ctx, docsPath(bucket))
	} else {
		err = s.each(ctx, func(d storage.StorageAPI) error {
			err := d.Delete(ctx, domain.SystemBucket, metadataPath(bucket), storage.DeleteOptions{})
			if isNotFound(err) {
				return nil
			}
			return err
		})
	}
	s.drop(bucket)
	if err != nil {
		return fmt.Errorf("bucket: delete metadata of %s: %w", bucket, err)
	}
	return nil
}

// Invalidate implements MetadataStore.
func (s *metadataStore) Invalidate(_ context.Context, bucket string) {
	if bucket == "" {
		s.mu.Lock()
		s.gen++
		s.cache = make(map[string]*Metadata)
		s.mu.Unlock()
		return
	}
	s.drop(bucket)
}

// drop removes a cache entry and bumps the generation.
func (s *metadataStore) drop(bucket string) {
	s.mu.Lock()
	s.gen++
	delete(s.cache, bucket)
	s.mu.Unlock()
}

// BucketConfig implements object.BucketConfigSource.
func (s *metadataStore) BucketConfig(ctx context.Context, bucket string) (object.BucketConfig, error) {
	m, err := s.Load(ctx, bucket)
	if err != nil {
		return object.BucketConfig{}, err
	}
	cfg := object.BucketConfig{
		Versioning: m.Versioning, ObjectLock: m.ObjectLock.Enabled, QuotaBytes: m.Quota.Size,
		Encryption: domain.BucketEncryption{Algorithm: m.Encryption.Algorithm, KMSKeyID: m.Encryption.KMSKeyID},
	}
	if m.ObjectLock.Enabled {
		cfg.DefaultRetention = m.ObjectLock.DefaultRetention()
	}
	return cfg, nil
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
	return fmt.Errorf("%w: %d of %d drives acknowledged: %w", domain.ErrWriteQuorum, ok, len(s.deps.Drives), first)
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
	c.Policy = cloneBytes(m.Policy)
	c.Lifecycle = cloneBytes(m.Lifecycle)
	c.CORS = cloneBytes(m.CORS)
	c.Notification = cloneBytes(m.Notification)
	return &c
}

// cloneBytes copies b, keeping nil as nil.
func cloneBytes[T ~[]byte](b T) T {
	if b == nil {
		return nil
	}
	return append(T(nil), b...)
}
