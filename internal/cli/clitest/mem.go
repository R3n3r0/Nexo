// Package clitest provides the test doubles of the nexoctl commands: MemS3
// is an in-memory S3 store that drives the generated mock of cli.S3Client
// with RunAndReturn (docs/12 §12.2 rule 8: no hand-written fake of an
// interface that has a mock; the shared state is modeled on the mock).
package clitest

import (
	"bytes"
	"context"
	"crypto/md5" // #nosec G501 -- the S3 ETag of the in-memory test server
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/stretchr/testify/mock"

	"github.com/r3n3r0/nexo/internal/cli"
	climock "github.com/r3n3r0/nexo/internal/cli/mock"
	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
)

// TestingT is the test handle of the generated mocks (testing.TB).
type TestingT interface {
	mock.TestingT
	Cleanup(func())
}

// nullVersion is the version ID of the objects written while versioning
// is off or suspended.
const nullVersion = "null"

// MemS3 is an in-memory S3 store shared by the mocks it configures. Dates
// come from the clock, version IDs are sequential ("v0001", ...) so that
// outputs are deterministic. It is safe for concurrent use.
type MemS3 struct {
	clock clock.Clock

	mu      sync.Mutex
	buckets map[string]*memBucket
	seq     int
	fail    map[string]error
	calls   map[string]int
}

type memBucket struct {
	created    time.Time
	versioning cli.Versioning
	policy     string
	tags       map[string]string
	// objects holds the versions of each key, the latest first.
	objects map[string][]*memVersion
	uploads []cli.UploadInfo
	// objectLock and defaultRetention are the Object Lock configuration.
	objectLock       bool
	defaultRetention domain.DefaultRetention
	// The bucket configurations of the S3 API (nil: none).
	lifecycle     *cli.LifecycleConfiguration
	cors          *cli.CORSConfiguration
	notifications *cli.BucketNotifications
}

type memVersion struct {
	id           string
	data         []byte
	etag         string
	modTime      time.Time
	contentType  string
	metadata     map[string]string
	tags         map[string]string
	deleteMarker bool
	retention    domain.ObjectRetention
	legalHold    domain.LegalHoldStatus
	checksum     domain.Checksum
	encryption   domain.EncryptionInfo
}

// NewMemS3 returns an empty store dated by clk (nil: a fake clock at
// 2026-01-02 03:04:05 UTC).
func NewMemS3(clk clock.Clock) *MemS3 {
	if clk == nil {
		clk = clock.NewFake(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	}
	return &MemS3{clock: clk, buckets: map[string]*memBucket{}, fail: map[string]error{}, calls: map[string]int{}}
}

// FailOn makes every call of the S3Client method fail with err (nil
// clears it).
func (m *MemS3) FailOn(method string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err == nil {
		delete(m.fail, method)
		return
	}
	m.fail[method] = err
}

// Calls returns how many times the S3Client method was called.
func (m *MemS3) Calls(method string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls[method]
}

// Factory returns a mock factory whose clients (for any alias) all use the
// store.
func (m *MemS3) Factory(t TestingT) *climock.S3Factory {
	f := climock.NewS3Factory(t)
	client := m.Client(t)
	f.EXPECT().Client(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(context.Context, cli.Alias, cli.ClientOptions) (cli.S3Client, error) { return client, nil }).Maybe()
	return f
}

// Client returns a mock S3Client backed by the store.
func (m *MemS3) Client(t TestingT) *climock.S3Client {
	c := climock.NewS3Client(t)
	x := mock.Anything
	c.EXPECT().ListBuckets(x).RunAndReturn(m.listBuckets).Maybe()
	c.EXPECT().BucketExists(x, x).RunAndReturn(m.bucketExists).Maybe()
	c.EXPECT().MakeBucket(x, x, x).RunAndReturn(m.makeBucket).Maybe()
	c.EXPECT().RemoveBucket(x, x).RunAndReturn(m.removeBucket).Maybe()
	c.EXPECT().List(x, x, x).RunAndReturn(m.list).Maybe()
	c.EXPECT().Stat(x, x, x, x).RunAndReturn(m.stat).Maybe()
	c.EXPECT().Get(x, x, x, x).RunAndReturn(m.get).Maybe()
	c.EXPECT().Put(x, x, x, x, x).RunAndReturn(m.put).Maybe()
	c.EXPECT().Copy(x, x, x, x).RunAndReturn(m.copy).Maybe()
	c.EXPECT().Remove(x, x, x, x).RunAndReturn(m.remove).Maybe()
	c.EXPECT().ListUploads(x, x, x).RunAndReturn(m.listUploads).Maybe()
	c.EXPECT().AbortUpload(x, x, x, x).RunAndReturn(m.abortUpload).Maybe()
	c.EXPECT().Presign(x, x).RunAndReturn(m.presign).Maybe()
	c.EXPECT().PostPolicy(x, x).RunAndReturn(m.postPolicy).Maybe()
	c.EXPECT().GetBucketPolicy(x, x).RunAndReturn(m.getBucketPolicy).Maybe()
	c.EXPECT().PutBucketPolicy(x, x, x).RunAndReturn(m.putBucketPolicy).Maybe()
	c.EXPECT().DeleteBucketPolicy(x, x).RunAndReturn(m.deleteBucketPolicy).Maybe()
	c.EXPECT().GetVersioning(x, x).RunAndReturn(m.getVersioning).Maybe()
	c.EXPECT().SetVersioning(x, x, x).RunAndReturn(m.setVersioning).Maybe()
	c.EXPECT().GetTags(x, x).RunAndReturn(m.getTags).Maybe()
	c.EXPECT().PutTags(x, x, x).RunAndReturn(m.putTags).Maybe()
	c.EXPECT().DeleteTags(x, x).RunAndReturn(m.deleteTags).Maybe()
	c.EXPECT().GetObjectLock(x, x).RunAndReturn(m.getObjectLock).Maybe()
	c.EXPECT().SetObjectLock(x, x, x).RunAndReturn(m.setObjectLock).Maybe()
	c.EXPECT().GetRetention(x, x).RunAndReturn(m.getRetention).Maybe()
	c.EXPECT().PutRetention(x, x, x, x).RunAndReturn(m.putRetention).Maybe()
	c.EXPECT().GetLegalHold(x, x).RunAndReturn(m.getLegalHold).Maybe()
	c.EXPECT().PutLegalHold(x, x, x).RunAndReturn(m.putLegalHold).Maybe()
	c.EXPECT().GetLifecycle(x, x).RunAndReturn(m.getLifecycle).Maybe()
	c.EXPECT().PutLifecycle(x, x, x).RunAndReturn(m.putLifecycle).Maybe()
	c.EXPECT().GetCORS(x, x).RunAndReturn(m.getCORS).Maybe()
	c.EXPECT().PutCORS(x, x, x).RunAndReturn(m.putCORS).Maybe()
	c.EXPECT().GetNotifications(x, x).RunAndReturn(m.getNotifications).Maybe()
	c.EXPECT().PutNotifications(x, x, x).RunAndReturn(m.putNotifications).Maybe()
	return c
}

// --- seeding and inspection ---------------------------------------------------

// ObjectSpec describes an object seeded with PutObject.
type ObjectSpec struct {
	ContentType string
	Metadata    map[string]string
	Tags        map[string]string
	// ModTime overrides the date of the version (zero: the clock).
	ModTime time.Time
	// Retention and LegalHold set the Object Lock state of the version
	// (the bucket needs Object Lock, see MakeLockedBucket).
	Retention domain.ObjectRetention
	LegalHold domain.LegalHoldStatus
	// SSE encrypts the version (SSE-C versions are then read with the
	// same key only).
	SSE domain.SSERequest
}

// MakeBucket creates a bucket (seeding helper); an existing one is kept.
func (m *MemS3) MakeBucket(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.buckets[name] == nil {
		m.buckets[name] = newMemBucket(m.clock.Now())
	}
}

// PutObject stores an object (creating the bucket) and returns its version
// ID ("" when versioning is off).
func (m *MemS3) PutObject(bucket, key, data string, spec ObjectSpec) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.buckets[bucket]
	if b == nil {
		b = newMemBucket(m.clock.Now())
		m.buckets[bucket] = b
	}
	v := m.newVersionLocked(b, []byte(data), spec.ContentType, spec.Metadata, spec.Tags)
	if !spec.ModTime.IsZero() {
		v.modTime = spec.ModTime.UTC()
	}
	if !spec.Retention.IsZero() {
		v.retention = spec.Retention
	}
	if spec.LegalHold != "" {
		v.legalHold = spec.LegalHold
	}
	v.encryption = memEncryption(spec.SSE)
	m.addVersionLocked(b, key, v)
	return outVersion(v.id)
}

// MakeLockedBucket creates a bucket with Object Lock (and versioning) and
// an optional default retention (seeding helper).
func (m *MemS3) MakeLockedBucket(name string, def domain.DefaultRetention) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.buckets[name]
	if b == nil {
		b = newMemBucket(m.clock.Now())
		m.buckets[name] = b
	}
	b.objectLock, b.versioning, b.defaultRetention = true, cli.VersioningEnabled, def
}

// VersionState is the Object Lock state, checksum and encryption of an
// object version (inspection helper).
type VersionState struct {
	Retention  domain.ObjectRetention
	LegalHold  domain.LegalHoldStatus
	Checksum   domain.Checksum
	Encryption domain.EncryptionInfo
}

// State returns the state of a version ("" is the latest).
func (m *MemS3) State(bucket, key, versionID string) (VersionState, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, err := m.versionLocked(bucket, key, versionID)
	if err != nil {
		return VersionState{}, false
	}
	return VersionState{Retention: v.retention, LegalHold: v.legalHold, Checksum: v.checksum, Encryption: v.encryption}, true
}

// Lifecycle returns the lifecycle configuration of a bucket (nil: none).
func (m *MemS3) Lifecycle(bucket string) *cli.LifecycleConfiguration {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b := m.buckets[bucket]; b != nil {
		return b.lifecycle
	}
	return nil
}

// Notifications returns the event subscriptions of a bucket (nil: none).
func (m *MemS3) Notifications(bucket string) *cli.BucketNotifications {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b := m.buckets[bucket]; b != nil {
		return b.notifications
	}
	return nil
}

// AddUpload registers an incomplete multipart upload.
func (m *MemS3) AddUpload(bucket, key, uploadID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.buckets[bucket]
	if b == nil {
		b = newMemBucket(m.clock.Now())
		m.buckets[bucket] = b
	}
	b.uploads = append(b.uploads, cli.UploadInfo{Key: key, UploadID: uploadID, Initiated: m.clock.Now().UTC()})
}

// Object returns the content of the latest version of an object.
func (m *MemS3) Object(bucket, key string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, err := m.latestLocked(bucket, key)
	if err != nil {
		return "", false
	}
	return string(v.data), true
}

// Keys returns the keys of a bucket whose latest version is an object.
func (m *MemS3) Keys(bucket string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.buckets[bucket]
	if b == nil {
		return nil
	}
	var out []string
	for k, vs := range b.objects {
		if len(vs) > 0 && !vs[0].deleteMarker {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// BucketNames returns the names of the buckets.
func (m *MemS3) BucketNames() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return cli.SortedKeys(m.buckets)
}

// Policy returns the bucket policy document ("" when none).
func (m *MemS3) Policy(bucket string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b := m.buckets[bucket]; b != nil {
		return b.policy
	}
	return ""
}

// --- S3Client behavior -----------------------------------------------------------

func newMemBucket(now time.Time) *memBucket {
	return &memBucket{created: now.UTC(), objects: map[string][]*memVersion{}, tags: map[string]string{}}
}

// begin counts a call and returns the injected failure, if any.
func (m *MemS3) begin(method string) error {
	m.calls[method]++
	return m.fail[method]
}

func (m *MemS3) bucketLocked(name string) (*memBucket, error) {
	b := m.buckets[name]
	if b == nil {
		return nil, fmt.Errorf("%w: %s", domain.ErrBucketNotFound, name)
	}
	return b, nil
}

func (m *MemS3) newVersionLocked(b *memBucket, data []byte, contentType string, meta, tags map[string]string) *memVersion {
	id := nullVersion
	if b.versioning == cli.VersioningEnabled {
		m.seq++
		id = fmt.Sprintf("v%04d", m.seq)
	}
	sum := md5.Sum(data) // #nosec G401 -- the S3 ETag of the in-memory test server is an MD5, not a security primitive
	now := m.clock.Now().UTC()
	return &memVersion{
		id: id, data: data, etag: hex.EncodeToString(sum[:]), modTime: now,
		contentType: contentType, metadata: cloneMap(meta), tags: cloneMap(tags),
		retention: b.defaultRetention.RetentionAt(now.Truncate(time.Second)),
	}
}

// addVersionLocked makes v the latest version of key; a null version
// replaces the previous null version.
func (m *MemS3) addVersionLocked(b *memBucket, key string, v *memVersion) {
	vs := b.objects[key]
	if v.id == nullVersion {
		kept := vs[:0:0]
		for _, old := range vs {
			if old.id != nullVersion {
				kept = append(kept, old)
			}
		}
		vs = kept
	}
	b.objects[key] = append([]*memVersion{v}, vs...)
}

func (m *MemS3) latestLocked(bucket, key string) (*memVersion, error) {
	b, err := m.bucketLocked(bucket)
	if err != nil {
		return nil, err
	}
	vs := b.objects[key]
	if len(vs) == 0 || vs[0].deleteMarker {
		return nil, fmt.Errorf("%w: %s", domain.ErrObjectNotFound, key)
	}
	return vs[0], nil
}

func (m *MemS3) versionLocked(bucket, key, id string) (*memVersion, error) {
	if id == "" {
		return m.latestLocked(bucket, key)
	}
	b, err := m.bucketLocked(bucket)
	if err != nil {
		return nil, err
	}
	for _, v := range b.objects[key] {
		if v.id == id {
			if v.deleteMarker {
				return nil, fmt.Errorf("%w: %s is a delete marker", domain.ErrMethodNotAllowed, id)
			}
			return v, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", domain.ErrNoSuchVersion, id)
}

func (m *MemS3) listBuckets(context.Context) ([]cli.BucketInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin("ListBuckets"); err != nil {
		return nil, err
	}
	out := make([]cli.BucketInfo, 0, len(m.buckets))
	for _, name := range cli.SortedKeys(m.buckets) {
		out = append(out, cli.BucketInfo{Name: name, Created: m.buckets[name].created})
	}
	return out, nil
}

func (m *MemS3) bucketExists(_ context.Context, bucket string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin("BucketExists"); err != nil {
		return false, err
	}
	return m.buckets[bucket] != nil, nil
}

func (m *MemS3) makeBucket(_ context.Context, bucket string, opts cli.MakeBucketOptions) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin("MakeBucket"); err != nil {
		return err
	}
	if m.buckets[bucket] != nil {
		return fmt.Errorf("%w: %s", domain.ErrBucketAlreadyExists, bucket)
	}
	if len(bucket) < 3 || strings.ToLower(bucket) != bucket || strings.ContainsAny(bucket, "_ /") {
		return fmt.Errorf("%w: %s", domain.ErrInvalidBucketName, bucket)
	}
	b := newMemBucket(m.clock.Now())
	if opts.ObjectLock {
		b.objectLock, b.versioning = true, cli.VersioningEnabled
	}
	m.buckets[bucket] = b
	return nil
}

func (m *MemS3) removeBucket(_ context.Context, bucket string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin("RemoveBucket"); err != nil {
		return err
	}
	b, err := m.bucketLocked(bucket)
	if err != nil {
		return err
	}
	for _, vs := range b.objects {
		if len(vs) > 0 {
			return fmt.Errorf("%w: %s", domain.ErrBucketNotEmpty, bucket)
		}
	}
	if len(b.uploads) > 0 {
		return fmt.Errorf("%w: %s has incomplete uploads", domain.ErrBucketNotEmpty, bucket)
	}
	delete(m.buckets, bucket)
	return nil
}

func (m *MemS3) list(_ context.Context, bucket string, opts cli.ListOptions) iter.Seq2[cli.ObjectInfo, error] {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin("List"); err != nil {
		return errSeq[cli.ObjectInfo](err)
	}
	b, err := m.bucketLocked(bucket)
	if err != nil {
		return errSeq[cli.ObjectInfo](err)
	}
	var out []cli.ObjectInfo
	seen := map[string]bool{}
	for _, key := range cli.SortedKeys(b.objects) {
		vs := b.objects[key]
		if len(vs) == 0 || !strings.HasPrefix(key, opts.Prefix) {
			continue
		}
		if !opts.Versions && vs[0].deleteMarker {
			continue
		}
		if !opts.Recursive {
			if i := strings.Index(key[len(opts.Prefix):], "/"); i >= 0 {
				p := key[:len(opts.Prefix)+i+1]
				if !seen[p] {
					seen[p] = true
					out = append(out, cli.ObjectInfo{Key: p, Prefix: true})
				}
				continue
			}
		}
		if !opts.Versions {
			out = append(out, info(key, vs[0], true, false))
			continue
		}
		for i, v := range vs {
			out = append(out, info(key, v, i == 0, true))
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return func(yield func(cli.ObjectInfo, error) bool) {
		for _, o := range out {
			if !yield(o, nil) {
				return
			}
		}
	}
}

func info(key string, v *memVersion, latest, versions bool) cli.ObjectInfo {
	o := cli.ObjectInfo{
		Key: key, Size: int64(len(v.data)), ETag: v.etag, LastModified: v.modTime,
		ContentType: v.contentType, VersionID: outVersion(v.id), IsLatest: latest,
		DeleteMarker: v.deleteMarker, Metadata: cloneMap(v.metadata), StorageClass: "STANDARD",
	}
	if !versions {
		o.Retention, o.LegalHold, o.Encryption = v.retention, v.legalHold, v.encryption
	}
	if versions {
		o.VersionID = v.id
	}
	if v.deleteMarker {
		o.Size, o.ETag, o.StorageClass = 0, "", ""
	}
	return o
}

func outVersion(id string) string {
	if id == nullVersion {
		return ""
	}
	return id
}

func (m *MemS3) stat(_ context.Context, bucket, key string, opts cli.StatOptions) (*cli.ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin("Stat"); err != nil {
		return nil, err
	}
	v, err := m.versionLocked(bucket, key, opts.VersionID)
	if errors.Is(err, domain.ErrBucketNotFound) {
		// Like the SDK client: a HEAD answers a bare 404, without the
		// code that tells a missing bucket from a missing key.
		err = fmt.Errorf("%w: %s", domain.ErrObjectNotFound, key)
	}
	if err != nil {
		return nil, err
	}
	if err := checkCustomerKey(v, opts.SSE); err != nil {
		return nil, err
	}
	o := info(key, v, opts.VersionID == "", false)
	if opts.Checksum {
		o.Checksum = v.checksum
	}
	return &o, nil
}

func (m *MemS3) get(_ context.Context, bucket, key string, opts cli.GetOptions) (io.ReadCloser, *cli.ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin("Get"); err != nil {
		return nil, nil, err
	}
	v, err := m.versionLocked(bucket, key, opts.VersionID)
	if err != nil {
		return nil, nil, err
	}
	if err := checkCustomerKey(v, opts.SSE); err != nil {
		return nil, nil, err
	}
	data := v.data
	if r := opts.Range; r != nil {
		if r.Offset > int64(len(data)) {
			return nil, nil, fmt.Errorf("%w: offset %d", domain.ErrInvalidRange, r.Offset)
		}
		end := int64(len(data))
		if r.Length > 0 && r.Offset+r.Length < end {
			end = r.Offset + r.Length
		}
		data = data[r.Offset:end]
	}
	o := info(key, v, opts.VersionID == "", false)
	o.Size = int64(len(data))
	return io.NopCloser(bytes.NewReader(bytes.Clone(data))), &o, nil
}

func (m *MemS3) put(ctx context.Context, bucket, key string, body io.Reader, opts cli.PutOptions) (*cli.PutResult, error) {
	m.mu.Lock()
	err := m.begin("Put")
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if err := cli.CheckPartSize(opts.PartSize); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.Size >= 0 && int64(len(data)) != opts.Size {
		return nil, fmt.Errorf("%w: read %d bytes, expected %d", io.ErrUnexpectedEOF, len(data), opts.Size)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	b, err := m.bucketLocked(bucket)
	if err != nil {
		return nil, err
	}
	v := m.newVersionLocked(b, data, opts.ContentType, opts.Metadata, opts.Tags)
	v.encryption = memEncryption(opts.SSE)
	if opts.Checksum != domain.ChecksumNone {
		if v.checksum, err = memChecksum(opts.Checksum, data); err != nil {
			return nil, err
		}
	}
	m.addVersionLocked(b, key, v)
	if opts.Progress != nil {
		opts.Progress(int64(len(data)))
	}
	parts := 0
	if size := int64(len(data)); size > cli.DefaultPartSize || (opts.PartSize > 0 && size > opts.PartSize) {
		_, parts, _ = cli.PartSize(size, opts.PartSize)
	}
	return &cli.PutResult{ETag: v.etag, VersionID: outVersion(v.id), Size: int64(len(data)), Parts: parts, Checksum: v.checksum}, nil
}

func (m *MemS3) copy(_ context.Context, src, dst cli.ObjectRef, opts cli.CopyOptions) (*cli.PutResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin("Copy"); err != nil {
		return nil, err
	}
	sv, err := m.versionLocked(src.Bucket, src.Key, src.VersionID)
	if err != nil {
		return nil, err
	}
	if err := checkCustomerKey(sv, opts.SourceSSE); err != nil {
		return nil, err
	}
	b, err := m.bucketLocked(dst.Bucket)
	if err != nil {
		return nil, err
	}
	meta, ct, tags := sv.metadata, sv.contentType, sv.tags
	if opts.Metadata != nil || opts.ContentType != "" {
		if opts.Metadata != nil {
			meta = opts.Metadata
		}
		if opts.ContentType != "" {
			ct = opts.ContentType
		}
	}
	if opts.Tags != nil {
		tags = opts.Tags
	}
	v := m.newVersionLocked(b, bytes.Clone(sv.data), ct, meta, tags)
	v.encryption = memEncryption(opts.SSE)
	if opts.Checksum != domain.ChecksumNone {
		if v.checksum, err = memChecksum(opts.Checksum, v.data); err != nil {
			return nil, err
		}
	}
	m.addVersionLocked(b, dst.Key, v)
	if opts.Progress != nil {
		opts.Progress(int64(len(v.data)))
	}
	return &cli.PutResult{ETag: v.etag, VersionID: outVersion(v.id), Size: int64(len(v.data)), Checksum: v.checksum}, nil
}

func (m *MemS3) remove(_ context.Context, bucket string, objects []cli.ObjectVersion, opts cli.DeleteOptions) ([]cli.RemoveResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin("Remove"); err != nil {
		return nil, err
	}
	b, err := m.bucketLocked(bucket)
	if err != nil {
		return nil, err
	}
	out := make([]cli.RemoveResult, 0, len(objects))
	for _, o := range objects {
		r := cli.RemoveResult{Key: o.Key, VersionID: o.VersionID}
		vs := b.objects[o.Key]
		switch {
		case o.VersionID != "":
			idx := -1
			for i, v := range vs {
				if v.id == o.VersionID {
					idx = i
				}
			}
			if idx < 0 {
				r.Err = fmt.Errorf("%w: %s", domain.ErrNoSuchVersion, o.VersionID)
				break
			}
			if err := m.lockedLocked(vs[idx], opts.BypassGovernance); err != nil {
				r.Err = err
				break
			}
			r.DeleteMarker = vs[idx].deleteMarker
			vs = append(vs[:idx:idx], vs[idx+1:]...)
			if len(vs) == 0 {
				delete(b.objects, o.Key)
			} else {
				b.objects[o.Key] = vs
			}
		case b.versioning == cli.VersioningOff:
			delete(b.objects, o.Key)
		default:
			marker := m.newVersionLocked(b, nil, "", nil, nil)
			marker.deleteMarker, marker.retention = true, domain.ObjectRetention{}
			m.addVersionLocked(b, o.Key, marker)
			r.DeleteMarker, r.DeleteMarkerVersionID = true, marker.id
		}
		out = append(out, r)
	}
	return out, nil
}

func (m *MemS3) listUploads(_ context.Context, bucket, prefix string) iter.Seq2[cli.UploadInfo, error] {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin("ListUploads"); err != nil {
		return errSeq[cli.UploadInfo](err)
	}
	b, err := m.bucketLocked(bucket)
	if err != nil {
		return errSeq[cli.UploadInfo](err)
	}
	var out []cli.UploadInfo
	for _, u := range b.uploads {
		if strings.HasPrefix(u.Key, prefix) {
			out = append(out, u)
		}
	}
	return func(yield func(cli.UploadInfo, error) bool) {
		for _, u := range out {
			if !yield(u, nil) {
				return
			}
		}
	}
}

func (m *MemS3) abortUpload(_ context.Context, bucket, key, uploadID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin("AbortUpload"); err != nil {
		return err
	}
	b, err := m.bucketLocked(bucket)
	if err != nil {
		return err
	}
	for i, u := range b.uploads {
		if u.Key == key && u.UploadID == uploadID {
			b.uploads = append(b.uploads[:i:i], b.uploads[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("%w: %s", domain.ErrNoSuchUpload, uploadID)
}

// memEndpoint is the base URL of the presigned URLs of the store.
const memEndpoint = "https://s3.mem.test"

func (m *MemS3) presign(_ context.Context, in cli.PresignInput) (*cli.PresignedRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin("Presign"); err != nil {
		return nil, err
	}
	if err := cli.CheckExpiry(in.Expiry); err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("X-Amz-Expires", fmt.Sprint(int64(in.Expiry/time.Second)))
	q.Set("X-Amz-Signature", "fake")
	if in.VersionID != "" {
		q.Set("versionId", in.VersionID)
	}
	if in.ResponseContentDisposition != "" {
		q.Set("response-content-disposition", in.ResponseContentDisposition)
	}
	if in.ResponseContentType != "" {
		q.Set("response-content-type", in.ResponseContentType)
	}
	headers := map[string]string{}
	for k, v := range in.Headers {
		headers[k] = v
	}
	u := fmt.Sprintf("%s/%s/%s?%s", memEndpoint, in.Bucket, (&url.URL{Path: in.Key}).EscapedPath(), q.Encode())
	return &cli.PresignedRequest{Method: strings.ToUpper(in.Method), URL: u, Headers: headers, ExpiresAt: m.clock.Now().UTC().Add(in.Expiry)}, nil
}

func (m *MemS3) postPolicy(_ context.Context, in cli.PostPolicyInput) (*cli.PostPolicyForm, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin("PostPolicy"); err != nil {
		return nil, err
	}
	if err := cli.CheckExpiry(in.Expiry); err != nil {
		return nil, err
	}
	key := in.Key
	if key == "" {
		key = in.KeyPrefix + "${filename}"
	}
	expires := m.clock.Now().UTC().Add(in.Expiry)
	doc, _ := json.Marshal(map[string]any{"expiration": expires.Format(time.RFC3339), "bucket": in.Bucket, "key": key, "maxSize": in.MaxSize})
	// #nosec G101 -- the form fields of a test POST policy, not credentials
	fields := map[string]string{
		"key": key, "policy": base64.StdEncoding.EncodeToString(doc),
		"x-amz-algorithm": "AWS4-HMAC-SHA256", "x-amz-credential": "MEM/20260102/us-east-1/s3/aws4_request",
		"x-amz-date": m.clock.Now().UTC().Format("20060102T150405Z"), "x-amz-signature": "fake",
	}
	if in.ContentType != "" {
		fields["Content-Type"] = in.ContentType
	}
	return &cli.PostPolicyForm{URL: memEndpoint + "/" + in.Bucket, Fields: fields, ExpiresAt: expires}, nil
}

func (m *MemS3) getBucketPolicy(_ context.Context, bucket string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin("GetBucketPolicy"); err != nil {
		return "", err
	}
	b, err := m.bucketLocked(bucket)
	if err != nil {
		return "", err
	}
	if b.policy == "" {
		return "", fmt.Errorf("%w: bucket %s has no policy", domain.ErrPolicyNotFound, bucket)
	}
	return b.policy, nil
}

func (m *MemS3) putBucketPolicy(_ context.Context, bucket, policy string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin("PutBucketPolicy"); err != nil {
		return err
	}
	b, err := m.bucketLocked(bucket)
	if err != nil {
		return err
	}
	if !json.Valid([]byte(policy)) {
		return fmt.Errorf("%w: invalid JSON", domain.ErrMalformedPolicy)
	}
	b.policy = policy
	return nil
}

func (m *MemS3) deleteBucketPolicy(_ context.Context, bucket string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin("DeleteBucketPolicy"); err != nil {
		return err
	}
	b, err := m.bucketLocked(bucket)
	if err != nil {
		return err
	}
	b.policy = ""
	return nil
}

func (m *MemS3) getVersioning(_ context.Context, bucket string) (cli.Versioning, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin("GetVersioning"); err != nil {
		return cli.VersioningOff, err
	}
	b, err := m.bucketLocked(bucket)
	if err != nil {
		return cli.VersioningOff, err
	}
	return b.versioning, nil
}

func (m *MemS3) setVersioning(_ context.Context, bucket string, v cli.Versioning) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin("SetVersioning"); err != nil {
		return err
	}
	b, err := m.bucketLocked(bucket)
	if err != nil {
		return err
	}
	if b.objectLock && v != cli.VersioningEnabled {
		return fmt.Errorf("%w: versioning cannot be suspended on a bucket with Object Lock (InvalidBucketState)", domain.ErrConflict)
	}
	b.versioning = v
	return nil
}

func (m *MemS3) getTags(_ context.Context, ref cli.ObjectRef) (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin("GetTags"); err != nil {
		return nil, err
	}
	if ref.Key == "" {
		b, err := m.bucketLocked(ref.Bucket)
		if err != nil {
			return nil, err
		}
		return cloneMap(b.tags), nil
	}
	v, err := m.versionLocked(ref.Bucket, ref.Key, ref.VersionID)
	if err != nil {
		return nil, err
	}
	out := cloneMap(v.tags)
	if out == nil {
		out = map[string]string{}
	}
	return out, nil
}

func (m *MemS3) putTags(_ context.Context, ref cli.ObjectRef, tags map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.begin("PutTags"); err != nil {
		return err
	}
	if ref.Key == "" {
		b, err := m.bucketLocked(ref.Bucket)
		if err != nil {
			return err
		}
		b.tags = cloneMap(tags)
		return nil
	}
	v, err := m.versionLocked(ref.Bucket, ref.Key, ref.VersionID)
	if err != nil {
		return err
	}
	v.tags = cloneMap(tags)
	return nil
}

func (m *MemS3) deleteTags(ctx context.Context, ref cli.ObjectRef) error {
	if err := m.putTags(ctx, ref, map[string]string{}); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls["PutTags"]--
	m.calls["DeleteTags"]++
	return nil
}

func cloneMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// errSeq is an iteration that yields err.
func errSeq[T any](err error) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		var zero T
		yield(zero, err)
	}
}
