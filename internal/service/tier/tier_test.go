package tier_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/certs"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/sysstore"
	sysmock "github.com/r3n3r0/nexo/internal/service/sysstore/mock"
	"github.com/r3n3r0/nexo/internal/service/tier"
	tiermock "github.com/r3n3r0/nexo/internal/service/tier/mock"
)

// bucketStore is the remote buckets of the tests, by bucket and object
// name.
type bucketStore struct {
	mu      sync.Mutex
	buckets map[string]map[string]tier.RemoteObject
	data    map[string][]byte
	clk     clock.Clock
	creds   map[string]string
}

// fakeRemote is a bucket of the bucketStore reached with credentials.
type fakeRemote struct {
	s      *bucketStore
	bucket string
	ok     bool
}

func (s *bucketStore) Dial(_ context.Context, cfg tier.RemoteConfig) (tier.Remote, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return &fakeRemote{s: s, bucket: cfg.Bucket, ok: s.creds[cfg.AccessKey] == cfg.SecretKey}, nil
}

func (r *fakeRemote) objects() (map[string]tier.RemoteObject, error) {
	if !r.ok {
		return nil, errors.New("access denied")
	}
	b, ok := r.s.buckets[r.bucket]
	if !ok {
		return nil, errors.New("no such bucket")
	}
	return b, nil
}

func (r *fakeRemote) CheckBucket(context.Context) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	_, err := r.objects()
	return err
}

func (r *fakeRemote) Put(_ context.Context, name string, body io.Reader, size int64, meta map[string]string) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if int64(len(data)) != size {
		return fmt.Errorf("size %d, %d declared", len(data), size)
	}
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	objs, err := r.objects()
	if err != nil {
		return err
	}
	objs[name] = tier.RemoteObject{Name: name, Size: size, ModTime: r.s.clk.Now(), Metadata: meta}
	r.s.data[r.bucket+"/"+name] = data
	return nil
}

func (r *fakeRemote) Head(_ context.Context, name string) (tier.RemoteObject, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	objs, err := r.objects()
	if err != nil {
		return tier.RemoteObject{}, err
	}
	o, ok := objs[name]
	if !ok {
		return tier.RemoteObject{}, domain.ErrObjectNotFound
	}
	return o, nil
}

func (r *fakeRemote) Open(_ context.Context, name string, offset, length int64) (io.ReadCloser, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if _, err := r.objects(); err != nil {
		return nil, err
	}
	data := r.s.data[r.bucket+"/"+name]
	return io.NopCloser(bytes.NewReader(data[offset : offset+length])), nil
}

func (r *fakeRemote) Delete(_ context.Context, name string) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	objs, err := r.objects()
	if err != nil {
		return err
	}
	delete(objs, name)
	delete(r.s.data, r.bucket+"/"+name)
	return nil
}

func (r *fakeRemote) List(_ context.Context, prefix, _ string) ([]tier.RemoteObject, string, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	objs, err := r.objects()
	if err != nil {
		return nil, "", err
	}
	var out []tier.RemoteObject
	for name, o := range objs {
		if strings.HasPrefix(name, prefix) {
			out = append(out, tier.RemoteObject{Name: name, Size: o.Size, ModTime: o.ModTime})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, "", nil
}

func (s *bucketStore) names(bucket string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for n := range s.buckets[bucket] {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// memDocs backs the generated sysstore mock with a map.
func memDocs(t *testing.T) sysstore.Store {
	var mu sync.Mutex
	docs := map[string][]byte{}
	m := sysmock.NewStore(t)
	m.EXPECT().Get(mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, p string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		d, ok := docs[p]
		if !ok {
			return nil, sysstore.ErrNotFound
		}
		return d, nil
	}).Maybe()
	m.EXPECT().Put(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(func(_ context.Context, p string, d []byte) error {
		mu.Lock()
		defer mu.Unlock()
		docs[p] = bytes.Clone(d)
		return nil
	}).Maybe()
	return m
}

type fixture struct {
	svc     tier.Service
	remotes *bucketStore
	objects *tiermock.Objects
	clk     *clock.Fake
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC))
	remotes := &bucketStore{
		buckets: map[string]map[string]tier.RemoteObject{"cold": {}, "other": {}},
		data:    map[string][]byte{}, clk: clk, creds: map[string]string{"ak": "sk", "ak2": "sk2"},
	}
	sealer, err := certs.NewSealer(tier.DeriveKey("root", "rootpass", "dep-1"))
	require.NoError(t, err)
	objects := tiermock.NewObjects(t)
	svc, err := tier.New(tier.Deps{
		Docs: memDocs(t), Sealer: sealer, DeploymentID: "dep-1", Objects: objects, Dialer: remotes,
		Clock: clk, Logger: slog.New(slog.DiscardHandler),
	})
	require.NoError(t, err)
	return &fixture{svc: svc, remotes: remotes, objects: objects, clk: clk}
}

func coldInput() tier.Input {
	return tier.Input{Name: "COLD", Endpoint: "https://s3.example.com/", Bucket: "cold", Prefix: "nexo/", AccessKey: "ak", SecretKey: "sk"}
}

// TestTiers covers the management of the tiers (S-STO-12): validation,
// the check of the remote bucket, the secret never listed, the credentials
// replaced, and the removal refused while the tier holds data.
//
// covers: S-STO-12
func TestTiers(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t)

	for name, in := range map[string]tier.Input{
		"lower case name": {Name: "cold", Endpoint: "https://s3.example.com", Bucket: "cold", AccessKey: "ak", SecretKey: "sk"},
		"reserved name":   {Name: domain.StorageClassStandard, Endpoint: "https://s3.example.com", Bucket: "cold", AccessKey: "ak", SecretKey: "sk"},
		"endpoint path":   {Name: "COLD", Endpoint: "https://s3.example.com/x", Bucket: "cold", AccessKey: "ak", SecretKey: "sk"},
		"prefix":          {Name: "COLD", Endpoint: "https://s3.example.com", Bucket: "cold", Prefix: "nexo", AccessKey: "ak", SecretKey: "sk"},
		"no secret":       {Name: "COLD", Endpoint: "https://s3.example.com", Bucket: "cold", AccessKey: "ak"},
		"bad credentials": {Name: "COLD", Endpoint: "https://s3.example.com", Bucket: "cold", AccessKey: "ak", SecretKey: "wrong"},
		"missing bucket":  {Name: "COLD", Endpoint: "https://s3.example.com", Bucket: "nope", AccessKey: "ak", SecretKey: "sk"},
	} {
		_, err := fx.svc.Add(ctx, in)
		require.ErrorIs(t, err, domain.ErrInvalidArgument, name)
	}

	added, err := fx.svc.Add(ctx, coldInput())
	require.NoError(t, err)
	assert.Equal(t, "https://s3.example.com", added.Endpoint)
	_, err = fx.svc.Add(ctx, coldInput())
	require.ErrorIs(t, err, domain.ErrConflict)
	ok, err := fx.svc.Exists(ctx, "COLD")
	require.NoError(t, err)
	assert.True(t, ok)

	list, err := fx.svc.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "ak", list[0].AccessKey)

	_, err = fx.svc.Edit(ctx, "COLD", tier.Input{AccessKey: "ak2", SecretKey: "bad"})
	require.ErrorIs(t, err, domain.ErrInvalidArgument)
	edited, err := fx.svc.Edit(ctx, "COLD", tier.Input{AccessKey: "ak2", SecretKey: "sk2", Region: "eu-south-1"})
	require.NoError(t, err)
	assert.Equal(t, "ak2", edited.AccessKey)
	assert.Equal(t, "eu-south-1", edited.Region)
	_, err = fx.svc.Edit(ctx, "WARM", tier.Input{AccessKey: "ak", SecretKey: "sk"})
	require.ErrorIs(t, err, domain.ErrNotFound)

	// Data written with the new credentials, read back by range.
	name, err := fx.svc.PutTiered(ctx, "COLD", strings.NewReader("0123456789"), 10, object.TierRef{Bucket: "b", Key: "k", VersionID: "v"})
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(name, "nexo/dep-1/"), name)
	rc, err := fx.svc.OpenTiered(ctx, "COLD", name, 3, 4)
	require.NoError(t, err)
	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, "3456", string(got))

	err = fx.svc.Remove(ctx, "COLD")
	require.ErrorIs(t, err, domain.ErrConflict, "the tier holds data")
	require.NoError(t, fx.svc.DeleteTiered(ctx, "COLD", name))
	require.NoError(t, fx.svc.Remove(ctx, "COLD"))
	ok, err = fx.svc.Exists(ctx, "COLD")
	require.NoError(t, err)
	assert.False(t, ok)
	require.ErrorIs(t, fx.svc.Remove(ctx, "COLD"), domain.ErrNotFound)
}

// TestSweep: the orphaned objects of a tier old enough are removed; those
// a version refers to, the young ones, the objects of other deployments
// and those whose version cannot be checked stay.
//
// covers: S-STO-12
func TestSweep(t *testing.T) {
	ctx := context.Background()
	fx := newFixture(t)
	_, err := fx.svc.Add(ctx, coldInput())
	require.NoError(t, err)
	put := func(key string) string {
		name, err := fx.svc.PutTiered(ctx, "COLD", strings.NewReader("data"), 4, object.TierRef{Bucket: "b", Key: key, VersionID: "v-" + key})
		require.NoError(t, err)
		return name
	}
	kept := put("kept")
	deleted := put("deleted")
	replaced := put("replaced")
	unknown := put("unknown")
	// An object of another deployment sharing the bucket.
	r, err := fx.remotes.Dial(ctx, tier.RemoteConfig{Bucket: "cold", AccessKey: "ak", SecretKey: "sk"})
	require.NoError(t, err)
	foreign := "nexo/dep-1/foreign"
	require.NoError(t, r.Put(ctx, foreign, strings.NewReader("x"), 1, map[string]string{"nexo-deployment": "dep-2", "nexo-bucket": "b", "nexo-key": "eA", "nexo-version": ""}))

	fx.clk.Advance(2 * time.Hour)
	young := put("young")
	fx.objects.EXPECT().TierLocation(mock.Anything, "b", mock.Anything, mock.Anything).RunAndReturn(
		func(_ context.Context, _, key, _ string) (object.TierLocation, error) {
			switch key {
			case "kept":
				return object.TierLocation{Exists: true, Tier: "COLD", Object: kept}, nil
			case "replaced":
				return object.TierLocation{Exists: true}, nil
			case "unknown":
				return object.TierLocation{}, fmt.Errorf("%w: injected", domain.ErrReadQuorum)
			}
			return object.TierLocation{}, nil
		})

	res, err := fx.svc.Sweep(ctx)
	require.NoError(t, err)
	assert.Equal(t, tier.SweepResult{Scanned: 5, Removed: 2, Failed: 1}, res)
	got := fx.remotes.names("cold")
	assert.ElementsMatch(t, []string{kept, unknown, foreign, young}, got)
	assert.NotContains(t, got, deleted)
	assert.NotContains(t, got, replaced)
}
