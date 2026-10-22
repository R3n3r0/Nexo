package replication_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/certs"
	"github.com/r3n3r0/nexo/internal/service/event"
	"github.com/r3n3r0/nexo/internal/service/lock"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/replication"
	"github.com/r3n3r0/nexo/internal/service/storage"
	"github.com/r3n3r0/nexo/internal/service/sysstore"
	"github.com/r3n3r0/nexo/internal/testutil"
)

// configs is a BucketConfigSource of fixed settings.
type configs struct {
	mu  sync.Mutex
	cfg map[string]object.BucketConfig
}

func (c *configs) BucketConfig(_ context.Context, bucket string) (object.BucketConfig, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg[bucket], nil
}

func (c *configs) set(bucket string, cfg object.BucketConfig) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cfg[bucket] = cfg
}

// notifier forwards the events of the source layer to the service.
type notifier struct {
	event.Service
	mu  sync.Mutex
	svc replication.Service
}

func (n *notifier) Publish(ctx context.Context, e event.Event) error {
	n.mu.Lock()
	svc := n.svc
	n.mu.Unlock()
	if svc != nil {
		svc.Notify(ctx, e)
	}
	return nil
}

// fakeDialer opens remotes writing to the destination layer like the S3
// API of another site would (replica writes).
type fakeDialer struct {
	dst   object.Service
	fails atomic.Int32
	puts  atomic.Int32
}

func (d *fakeDialer) Dial(_ context.Context, cfg replication.RemoteConfig) (replication.Remote, error) {
	return &fakeRemote{d: d, bucket: cfg.Bucket, uploads: map[string]*bytes.Buffer{}}, nil
}

type fakeRemote struct {
	d       *fakeDialer
	bucket  string
	mu      sync.Mutex
	uploads map[string]*bytes.Buffer
	puts    map[string]replication.RemotePut
}

func (r *fakeRemote) fail() error {
	if r.d.fails.Load() > 0 {
		r.d.fails.Add(-1)
		return errors.New("remote offline")
	}
	return nil
}

func (r *fakeRemote) CheckBucket(context.Context) error { return r.fail() }

func (r *fakeRemote) Head(ctx context.Context, key, versionID string) (replication.RemoteVersion, error) {
	info, err := r.d.dst.GetObjectInfo(ctx, r.bucket, key, object.GetObjectOptions{VersionID: versionID})
	if err != nil {
		return replication.RemoteVersion{}, nil
	}
	return replication.RemoteVersion{Exists: true, ETag: info.ETag, Size: info.Size}, nil
}

func (r *fakeRemote) Put(ctx context.Context, v replication.RemotePut, data []byte) error {
	if err := r.fail(); err != nil {
		return err
	}
	r.d.puts.Add(1)
	_, err := r.d.dst.PutObject(object.WithReplica(ctx), object.PutObjectInput{Bucket: r.bucket, Key: v.Key, Reader: bytes.NewReader(data), Size: int64(len(data)),
		VersionID: v.VersionID, ModTime: v.ModTime, Replica: true, ReplicaETag: v.ETag, ContentType: v.ContentType, UserMetadata: v.UserMetadata, Tags: v.Tags})
	return err
}

func (r *fakeRemote) CreateUpload(_ context.Context, v replication.RemotePut) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := "up-" + v.Key
	r.uploads[id] = &bytes.Buffer{}
	if r.puts == nil {
		r.puts = map[string]replication.RemotePut{}
	}
	r.puts[id] = v
	return id, nil
}

func (r *fakeRemote) UploadPart(_ context.Context, _, uploadID string, _ int, data []byte) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.uploads[uploadID].Write(data)
	return "etag", nil
}

func (r *fakeRemote) CompleteUpload(ctx context.Context, _, uploadID string, _ []replication.RemotePart) error {
	r.mu.Lock()
	data, v := r.uploads[uploadID].Bytes(), r.puts[uploadID]
	r.mu.Unlock()
	return r.Put(ctx, v, data)
}

func (r *fakeRemote) AbortUpload(context.Context, string, string) error { return nil }

func (r *fakeRemote) DeleteMarker(ctx context.Context, key, versionID string, modTime time.Time) error {
	if err := r.fail(); err != nil {
		return err
	}
	_, err := r.d.dst.DeleteObject(object.WithReplica(ctx), r.bucket, key, object.DeleteObjectOptions{MarkerVersionID: versionID, ModTime: modTime})
	return err
}

func (r *fakeRemote) DeleteVersion(ctx context.Context, key, versionID string) error {
	if err := r.fail(); err != nil {
		return err
	}
	_, err := r.d.dst.DeleteObject(object.WithReplica(ctx), r.bucket, key, object.DeleteObjectOptions{VersionID: versionID})
	return err
}

func (r *fakeRemote) PutTags(ctx context.Context, key, versionID string, tags map[string]string) error {
	if err := r.fail(); err != nil {
		return err
	}
	_, err := r.d.dst.PutObjectTags(object.WithReplica(ctx), r.bucket, key, tags, object.TagOptions{VersionID: versionID})
	return err
}

type fixture struct {
	src, dst object.Service
	cfgs     *configs
	dialer   *fakeDialer
	svc      replication.Service
	docs     sysstore.Store
}

func layer(t *testing.T, root string, cfgs object.BucketConfigSource, events event.Service) (object.Service, storage.StorageAPI) {
	t.Helper()
	drive := testutil.NewLocalDriveIndex(t, root, 0)
	svc, err := object.NewFS(object.Deps{Store: drive, Buckets: cfgs, Events: events, Logger: slog.New(slog.DiscardHandler)}, object.WithInlineLimit(128))
	require.NoError(t, err)
	return svc, drive
}

func newFixture(t *testing.T, opts ...replication.Option) *fixture {
	t.Helper()
	base := t.TempDir()
	if dir, err := os.MkdirTemp("/dev/shm", "nexo-repl-"); err == nil {
		base = dir
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
	}
	f := &fixture{cfgs: &configs{cfg: map[string]object.BucketConfig{}}}
	n := &notifier{}
	var drive storage.StorageAPI
	f.src, drive = layer(t, filepath.Join(base, "src"), f.cfgs, n)
	dstCfgs := &configs{cfg: map[string]object.BucketConfig{"dst": {Versioning: domain.VersioningEnabled}}}
	f.dst, _ = layer(t, filepath.Join(base, "dst"), dstCfgs, nil)
	ctx := context.Background()
	require.NoError(t, f.src.MakeBucket(ctx, "src", object.MakeBucketOptions{}))
	require.NoError(t, f.dst.MakeBucket(ctx, "dst", object.MakeBucketOptions{}))
	f.cfgs.set("src", object.BucketConfig{Versioning: domain.VersioningEnabled})
	docs, err := sysstore.New(sysstore.Deps{Drives: []storage.StorageAPI{drive}, Locks: lock.NewLocal(lock.Deps{})})
	require.NoError(t, err)
	f.docs = docs
	sealer, err := certs.NewSealer(bytes.Repeat([]byte{7}, 32))
	require.NoError(t, err)
	f.dialer = &fakeDialer{dst: f.dst}
	opts = append([]replication.Option{replication.WithBackoff(10*time.Millisecond, 50*time.Millisecond, 100), replication.WithPollInterval(20 * time.Millisecond),
		replication.WithPartSize(5<<20, 1<<10)}, opts...)
	f.svc, err = replication.New(replication.Deps{Objects: f.src, Buckets: f.cfgs, Docs: docs, Sealer: sealer, Dialer: f.dialer,
		QueueDir: filepath.Join(base, "queue"), Logger: slog.New(slog.DiscardHandler)}, opts...)
	require.NoError(t, err)
	n.mu.Lock()
	n.svc = f.svc
	n.mu.Unlock()
	return f
}

// configure adds a target and a rule replicating prefix (with the delete
// markers and the deletions) to it.
func (f *fixture) configure(t *testing.T, prefix string) string {
	t.Helper()
	ctx := context.Background()
	target, err := f.svc.SetTarget(ctx, "src", replication.TargetInput{Endpoint: "https://remote.example:9000", Bucket: "dst", AccessKey: "repl", SecretKey: "secret-key"})
	require.NoError(t, err)
	doc := []byte(`<ReplicationConfiguration><Rule><Status>Enabled</Status><Priority>1</Priority><Filter><Prefix>` + prefix + `</Prefix></Filter>` +
		`<DeleteMarkerReplication><Status>Enabled</Status></DeleteMarkerReplication><DeleteReplication><Status>Enabled</Status></DeleteReplication>` +
		`<Destination><Bucket>` + target.ARN + `</Bucket></Destination></Rule></ReplicationConfiguration>`)
	canonical, err := f.svc.ValidateConfig(ctx, "src", doc)
	require.NoError(t, err)
	cfg, err := domain.ParseReplicationConfig(canonical, nil)
	require.NoError(t, err)
	f.cfgs.set("src", object.BucketConfig{Versioning: domain.VersioningEnabled, Replication: cfg})
	return target.ARN
}

func (f *fixture) put(t *testing.T, key string, data []byte) domain.ObjectInfo {
	t.Helper()
	info, err := f.src.PutObject(context.Background(), object.PutObjectInput{Bucket: "src", Key: key, Reader: bytes.NewReader(data), Size: int64(len(data)),
		ContentType: "text/plain", UserMetadata: map[string]string{"owner": "me"}})
	require.NoError(t, err)
	return info
}

func read(t *testing.T, svc object.Service, bucket, key, versionID string) (domain.ObjectInfo, []byte, error) {
	t.Helper()
	r, err := svc.GetObject(context.Background(), bucket, key, object.GetObjectOptions{VersionID: versionID})
	if err != nil {
		return domain.ObjectInfo{}, nil, err
	}
	defer func() { _ = r.Close() }()
	data, err := io.ReadAll(r)
	return r.Info, data, err
}

func (f *fixture) start(t *testing.T) {
	t.Helper()
	require.NoError(t, f.svc.Start(context.Background()))
	t.Cleanup(func() { _ = f.svc.Stop(context.Background()) })
}

func (f *fixture) waitState(t *testing.T, key, versionID string, want domain.ReplicationStatus) {
	t.Helper()
	require.Eventually(t, func() bool {
		info, err := f.src.GetObjectInfo(context.Background(), "src", key, object.GetObjectOptions{VersionID: versionID})
		return err == nil && info.ReplicationStatus == want
	}, 20*time.Second, 10*time.Millisecond, "%s %s %s", key, versionID, want)
}

// TestReplication (S-BKT-12): the versions the rules select reach the
// destination with their id, time, ETag and metadata (small ones in one
// request, large ones in parts); delete markers and deletions follow;
// the others stay here; a failing destination is retried until it
// succeeds, the version FAILED meanwhile.
//
// covers: S-BKT-12
func TestReplication(t *testing.T) {
	f := newFixture(t)
	arn := f.configure(t, "docs/")
	f.start(t)
	ctx := context.Background()

	small := f.put(t, "docs/small", []byte("hello"))
	big := bytes.Repeat([]byte("0123456789abcdef"), (6<<20)/16+3)
	large := f.put(t, "docs/large", big)
	other := f.put(t, "other/x", []byte("local only"))
	assert.Empty(t, other.ReplicationStatus)

	f.waitState(t, "docs/small", small.VersionID, domain.ReplicationCompleted)
	f.waitState(t, "docs/large", large.VersionID, domain.ReplicationCompleted)
	for _, v := range []struct {
		info domain.ObjectInfo
		data []byte
	}{{small, []byte("hello")}, {large, big}} {
		got, data, err := read(t, f.dst, "dst", v.info.Key, v.info.VersionID)
		require.NoError(t, err)
		assert.Equal(t, v.data, data)
		assert.Equal(t, v.info.VersionID, got.VersionID)
		assert.True(t, v.info.ModTime.Equal(got.ModTime))
		assert.Equal(t, v.info.ETag, got.ETag)
		assert.Equal(t, domain.ReplicationReplica, got.ReplicationStatus)
		assert.Equal(t, "me", got.UserMetadata["owner"])
	}
	_, _, err := read(t, f.dst, "dst", "other/x", "")
	require.ErrorIs(t, err, domain.ErrObjectNotFound)

	// A delete marker, then the permanent deletion of a version.
	del, err := f.src.DeleteObject(ctx, "src", "docs/small", object.DeleteObjectOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		_, _, err := read(t, f.dst, "dst", "docs/small", "")
		return errors.Is(err, domain.ErrObjectNotFound) || errors.Is(err, domain.ErrNoSuchVersion) || errors.Is(err, domain.ErrMethodNotAllowed)
	}, 20*time.Second, 10*time.Millisecond)
	_, err = f.src.DeleteObject(ctx, "src", "docs/large", object.DeleteObjectOptions{VersionID: large.VersionID})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		_, _, err := read(t, f.dst, "dst", "docs/large", large.VersionID)
		return err != nil
	}, 20*time.Second, 10*time.Millisecond)
	versions, err := f.dst.ListObjectVersions(ctx, domain.ListObjectsInput{Bucket: "dst", Prefix: "docs/small", IncludeVersions: true})
	require.NoError(t, err)
	require.Len(t, versions.Objects, 2)
	assert.Equal(t, del.DeleteMarkerVersionID, versions.Objects[0].VersionID, "the marker keeps its id")

	// An offline destination: FAILED, then COMPLETED once it is back.
	f.dialer.fails.Store(3)
	again := f.put(t, "docs/retry", []byte("retry"))
	f.waitState(t, "docs/retry", again.VersionID, domain.ReplicationCompleted)
	_, data, err := read(t, f.dst, "dst", "docs/retry", again.VersionID)
	require.NoError(t, err)
	assert.Equal(t, "retry", string(data))

	st, err := f.svc.Status(ctx, "src")
	require.NoError(t, err)
	require.Len(t, st.Targets, 1)
	assert.Equal(t, arn, st.Targets[0].ARN)
	assert.True(t, st.Targets[0].Online)
	assert.GreaterOrEqual(t, st.Targets[0].Replicated, int64(3))
	assert.GreaterOrEqual(t, st.Targets[0].Failed, int64(1))
	assert.Equal(t, int64(2), st.Targets[0].Deletes)
}

// TestReplicationQueueSurvivesRestart (S-BKT-12): the tasks queued while
// the service was stopped run at the next start.
//
// covers: S-BKT-12
func TestReplicationQueueSurvivesRestart(t *testing.T) {
	f := newFixture(t)
	f.configure(t, "")
	// Not started: the tasks wait in the queue.
	v := f.put(t, "k", []byte("queued"))
	st, err := f.svc.Status(context.Background(), "src")
	require.NoError(t, err)
	assert.Equal(t, 1, st.Queued)
	f.start(t)
	f.waitState(t, "k", v.VersionID, domain.ReplicationCompleted)
}

// TestResync (S-BKT-12): a resync replicates the versions written before
// the rule (and those the destination lost), checking first what it has.
//
// covers: S-BKT-12
func TestResync(t *testing.T) {
	f := newFixture(t)
	before := f.put(t, "a", []byte("before the rule"))
	marker, err := f.src.DeleteObject(context.Background(), "src", "gone", object.DeleteObjectOptions{})
	require.NoError(t, err)
	arn := f.configure(t, "")
	f.start(t)
	after := f.put(t, "b", []byte("after"))
	f.waitState(t, "b", after.VersionID, domain.ReplicationCompleted)
	_, _, err = read(t, f.dst, "dst", "a", before.VersionID)
	require.Error(t, err, "not replicated yet")

	puts := f.dialer.puts.Load()
	r, err := f.svc.StartResync(context.Background(), "src", arn)
	require.NoError(t, err)
	assert.Equal(t, replication.ResyncRunning, r.State)
	require.Eventually(t, func() bool {
		st, err := f.svc.Status(context.Background(), "src")
		require.NoError(t, err)
		return st.Resync != nil && st.Resync.State == replication.ResyncComplete && st.Queued == 0
	}, 20*time.Second, 10*time.Millisecond)
	f.waitState(t, "a", before.VersionID, domain.ReplicationCompleted)
	_, data, err := read(t, f.dst, "dst", "a", before.VersionID)
	require.NoError(t, err)
	assert.Equal(t, "before the rule", string(data))
	assert.Equal(t, puts+1, f.dialer.puts.Load(), "the version the destination had is not sent again")
	versions, err := f.dst.ListObjectVersions(context.Background(), domain.ListObjectsInput{Bucket: "dst", Prefix: "gone", IncludeVersions: true})
	require.NoError(t, err)
	require.Len(t, versions.Objects, 1)
	assert.Equal(t, marker.DeleteMarkerVersionID, versions.Objects[0].VersionID)

	_, err = f.svc.StartResync(context.Background(), "src", "arn:nexo:replication::nope:dst")
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
}

// TestTargets (S-BKT-12): targets are checked before they are stored,
// their secret sealed and never listed, and a target the configuration
// uses cannot be removed.
//
// covers: S-BKT-12
func TestTargets(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.dialer.fails.Store(1)
	_, err := f.svc.SetTarget(ctx, "src", replication.TargetInput{Endpoint: "https://r:9000", Bucket: "dst", AccessKey: "a", SecretKey: "s"})
	require.ErrorIs(t, err, domain.ErrInvalidArgument, "an unreachable remote")
	for _, bad := range []replication.TargetInput{
		{Endpoint: "ftp://r", Bucket: "dst", AccessKey: "a", SecretKey: "s"},
		{Endpoint: "https://r/path", Bucket: "dst", AccessKey: "a", SecretKey: "s"},
		{Endpoint: "https://r", Bucket: "Bad_Bucket", AccessKey: "a", SecretKey: "s"},
		{Endpoint: "https://r", Bucket: "dst", SecretKey: "s"},
		{Endpoint: "https://r", Bucket: "dst", AccessKey: "a"},
	} {
		_, err := f.svc.SetTarget(ctx, "src", bad)
		require.ErrorIs(t, err, domain.ErrInvalidArgument, "%+v", bad)
	}
	arn := f.configure(t, "")
	raw, err := f.docs.Get(ctx, replication.TargetsDir+"/src.json")
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "secret-key")

	// Replaced without its secret: the stored one is kept.
	updated, err := f.svc.SetTarget(ctx, "src", replication.TargetInput{ARN: arn, Endpoint: "https://other:9000", Bucket: "dst", AccessKey: "repl2", Label: "dr"})
	require.NoError(t, err)
	assert.Equal(t, arn, updated.ARN)
	list, err := f.svc.ListTargets(ctx, "src")
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "https://other:9000", list[0].Endpoint)
	assert.Equal(t, "dr", list[0].Label)

	require.ErrorIs(t, f.svc.RemoveTarget(ctx, "src", arn), domain.ErrInvalidArgument, "still used by the rules")
	f.cfgs.set("src", object.BucketConfig{Versioning: domain.VersioningEnabled})
	require.NoError(t, f.svc.RemoveTarget(ctx, "src", arn))
	require.ErrorIs(t, f.svc.RemoveTarget(ctx, "src", arn), domain.ErrNotFound)
	_, err = f.svc.ValidateConfig(ctx, "src", []byte(`<ReplicationConfiguration><Rule><Status>Enabled</Status><Destination><Bucket>`+arn+`</Bucket></Destination></Rule></ReplicationConfiguration>`))
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
	require.NoError(t, f.svc.RemoveBucket(ctx, "src"))
}

// TestReplicaNotReplicated (S-BKT-12): the writes of the replication of
// another site are never sent back (active-active without loop).
//
// covers: S-BKT-12
func TestReplicaNotReplicated(t *testing.T) {
	f := newFixture(t)
	f.configure(t, "")
	f.start(t)
	ctx := object.WithReplica(context.Background())
	_, err := f.src.PutObject(ctx, object.PutObjectInput{Bucket: "src", Key: "from-b", Reader: bytes.NewReader([]byte("b")), Size: 1,
		VersionID: "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a50", ModTime: time.Now().Add(-time.Minute), Replica: true})
	require.NoError(t, err)
	_, err = f.src.DeleteObject(ctx, "src", "from-b", object.DeleteObjectOptions{})
	require.NoError(t, err)
	st, err := f.svc.Status(context.Background(), "src")
	require.NoError(t, err)
	assert.Zero(t, st.Queued)
	assert.Zero(t, f.dialer.puts.Load())
}
