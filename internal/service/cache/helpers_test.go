package cache_test

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tmock "github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/cache"
	cachemock "github.com/r3n3r0/nexo/internal/service/cache/mock"
	"github.com/r3n3r0/nexo/internal/service/cluster"
	"github.com/r3n3r0/nexo/internal/service/distribution"
	"github.com/r3n3r0/nexo/internal/service/lock"
	"github.com/r3n3r0/nexo/internal/service/metrics"
	"github.com/r3n3r0/nexo/internal/service/object"
	objmock "github.com/r3n3r0/nexo/internal/service/object/mock"
	"github.com/r3n3r0/nexo/internal/service/sysstore"
	sysmock "github.com/r3n3r0/nexo/internal/service/sysstore/mock"
)

// t0 is the start of the fake clocks.
var t0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// memDocs is the shared state of the store mocks: the documents of a
// deployment, seen by every node of a test.
type memDocs struct {
	mu   sync.Mutex
	docs map[string][]byte
}

// newStore returns a store mock backed by docs.
func newStore(t testing.TB, docs *memDocs) *sysmock.Store {
	t.Helper()
	m := sysmock.NewStore(t)
	m.EXPECT().Get(tmock.Anything, tmock.Anything).RunAndReturn(func(_ context.Context, p string) ([]byte, error) {
		docs.mu.Lock()
		defer docs.mu.Unlock()
		d, ok := docs.docs[p]
		if !ok {
			return nil, sysstore.ErrNotFound
		}
		return append([]byte(nil), d...), nil
	}).Maybe()
	m.EXPECT().Put(tmock.Anything, tmock.Anything, tmock.Anything).RunAndReturn(func(_ context.Context, p string, data []byte) error {
		docs.mu.Lock()
		defer docs.mu.Unlock()
		docs.docs[p] = append([]byte(nil), data...)
		return nil
	}).Maybe()
	m.EXPECT().Delete(tmock.Anything, tmock.Anything).RunAndReturn(func(_ context.Context, p string) error {
		docs.mu.Lock()
		defer docs.mu.Unlock()
		delete(docs.docs, p)
		return nil
	}).Maybe()
	m.EXPECT().List(tmock.Anything, tmock.Anything).RunAndReturn(func(_ context.Context, dir string) (map[string][]byte, error) {
		docs.mu.Lock()
		defer docs.mu.Unlock()
		out := map[string][]byte{}
		for p, d := range docs.docs {
			if rest, ok := strings.CutPrefix(p, dir+"/"); ok && !strings.Contains(rest, "/") {
				out[rest] = append([]byte(nil), d...)
			}
		}
		return out, nil
	}).Maybe()
	return m
}

// memObject is an object of the origin of the tests.
type memObject struct {
	data []byte
	info domain.ObjectInfo
}

// origin is the shared state of the object layer mocks: the objects, the
// reads counted, the failures injected.
type origin struct {
	mu      sync.Mutex
	objects map[string]memObject
	clk     clock.Clock
	// gets and infos count the reads (GetObject, GetObjectInfo).
	gets  atomic.Int64
	infos atomic.Int64
	// fail makes every read fail with this error (a set without quorum).
	fail error
	// gate, when set, blocks the reads until it is closed.
	gate chan struct{}
}

func newOrigin(clk clock.Clock) *origin {
	return &origin{objects: map[string]memObject{}, clk: clk}
}

// put stores an object; opts set its metadata.
func (o *origin) put(bucket, key string, data []byte, opts ...func(*domain.ObjectInfo)) domain.ObjectInfo {
	o.mu.Lock()
	defer o.mu.Unlock()
	sum := md5.Sum(data)
	info := domain.ObjectInfo{Bucket: bucket, Key: key, Size: int64(len(data)), ETag: hex.EncodeToString(sum[:]), ModTime: o.clk.Now().UTC(),
		ContentType: "application/octet-stream"}
	for _, f := range opts {
		f(&info)
	}
	o.objects[bucket+"/"+key] = memObject{data: append([]byte(nil), data...), info: info}
	return info
}

func (o *origin) setFail(err error) {
	o.mu.Lock()
	o.fail = err
	o.mu.Unlock()
}

func (o *origin) lookup(bucket, key string) (memObject, error) {
	o.mu.Lock()
	gate, fail := o.gate, o.fail
	obj, ok := o.objects[bucket+"/"+key]
	o.mu.Unlock()
	if gate != nil {
		<-gate
	}
	if fail != nil {
		return memObject{}, fail
	}
	if !ok {
		return memObject{}, domain.NewS3Error(domain.ErrObjectNotFound, "NoSuchKey", "The specified key does not exist.")
	}
	return obj, nil
}

// conditions evaluates the If-Match of the block reads.
func conditions(info domain.ObjectInfo, c domain.Conditions) error {
	if c.IfMatch != "" && strings.Trim(c.IfMatch, `"`) != info.ETag {
		return fmt.Errorf("%w: If-Match", domain.ErrPreconditionFailed)
	}
	if c.IfNoneMatch != "" && strings.Trim(c.IfNoneMatch, `"`) == info.ETag {
		return fmt.Errorf("%w: If-None-Match", domain.ErrNotModified)
	}
	return nil
}

// layer returns an object layer mock backed by o.
func (o *origin) layer(t testing.TB) *objmock.Service {
	t.Helper()
	m := objmock.NewService(t)
	m.EXPECT().GetObject(tmock.Anything, tmock.Anything, tmock.Anything, tmock.Anything).RunAndReturn(
		func(_ context.Context, bucket, key string, opts object.GetObjectOptions) (*object.Reader, error) {
			o.gets.Add(1)
			obj, err := o.lookup(bucket, key)
			if err != nil {
				return nil, err
			}
			if err := conditions(obj.info, opts.Conditions); err != nil {
				return nil, err
			}
			off, n := int64(0), obj.info.Size
			if opts.Range != nil {
				if off, n, err = opts.Range.Resolve(obj.info.Size); err != nil {
					return nil, err
				}
			}
			return &object.Reader{ReadCloser: io.NopCloser(bytes.NewReader(obj.data[off : off+n])), Info: obj.info, Offset: off, Length: n}, nil
		}).Maybe()
	m.EXPECT().GetObjectInfo(tmock.Anything, tmock.Anything, tmock.Anything, tmock.Anything).RunAndReturn(
		func(_ context.Context, bucket, key string, opts object.GetObjectOptions) (domain.ObjectInfo, error) {
			o.infos.Add(1)
			obj, err := o.lookup(bucket, key)
			if err != nil {
				return domain.ObjectInfo{}, err
			}
			return obj.info, conditions(obj.info, opts.Conditions)
		}).Maybe()
	m.EXPECT().ListObjects(tmock.Anything, tmock.Anything).RunAndReturn(
		func(_ context.Context, in domain.ListObjectsInput) (domain.ListObjectsResult, error) {
			o.mu.Lock()
			defer o.mu.Unlock()
			var res domain.ListObjectsResult
			for k, obj := range o.objects {
				if rest, ok := strings.CutPrefix(k, in.Bucket+"/"); ok && strings.HasPrefix(rest, in.Prefix) {
					res.Objects = append(res.Objects, obj.info)
				}
			}
			sort.Slice(res.Objects, func(a, b int) bool { return res.Objects[a].Key < res.Objects[b].Key })
			return res, nil
		}).Maybe()
	return m
}

// dists is the shared state of the distribution mocks.
type dists struct {
	mu sync.Mutex
	m  map[string]*distribution.Distribution
}

func newDists(ds ...distribution.Distribution) *dists {
	out := &dists{m: map[string]*distribution.Distribution{}}
	for _, d := range ds {
		out.set(d)
	}
	return out
}

// set stores a distribution with a new ETag.
func (ds *dists) set(d distribution.Distribution) *distribution.Distribution {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	cp := d
	cp.ETag = fmt.Sprintf("etag-%d", time.Now().UnixNano())
	ds.m[d.ID] = &cp
	return &cp
}

func (ds *dists) get(id string) *distribution.Distribution {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	return ds.m[id]
}

// resolve finds the distribution of a host.
func (ds *dists) resolve(host string) distribution.Route {
	ds.mu.Lock()
	defer ds.mu.Unlock()
	h := strings.ToLower(host)
	if i := strings.LastIndexByte(h, ':'); i > 0 {
		h = h[:i]
	}
	for _, d := range ds.m {
		for _, a := range d.Aliases {
			if a == h || (strings.HasPrefix(a, "*.") && strings.HasSuffix(h, a[1:])) {
				return distribution.Route{Kind: distribution.RouteDistribution, Host: h, Alias: a, Distribution: d, Bucket: d.Origin.Bucket}
			}
		}
	}
	return distribution.Route{Kind: distribution.RoutePathStyle, Host: h}
}

// mock returns a Distributions mock backed by ds.
func (ds *dists) mock(t testing.TB) *cachemock.Distributions {
	t.Helper()
	m := cachemock.NewDistributions(t)
	m.EXPECT().Resolve(tmock.Anything, tmock.Anything).RunAndReturn(func(_ context.Context, host string) distribution.Route {
		return ds.resolve(host)
	}).Maybe()
	m.EXPECT().Get(tmock.Anything, tmock.Anything).RunAndReturn(func(_ context.Context, id string) (distribution.Distribution, error) {
		if d := ds.get(id); d != nil {
			return *d, nil
		}
		return distribution.Distribution{}, fmt.Errorf("%w: %s", distribution.ErrDistributionNotFound, id)
	}).Maybe()
	m.EXPECT().List(tmock.Anything).RunAndReturn(func(context.Context) ([]distribution.Distribution, error) {
		ds.mu.Lock()
		defer ds.mu.Unlock()
		var out []distribution.Distribution
		for _, d := range ds.m {
			out = append(out, *d)
		}
		return out, nil
	}).Maybe()
	m.EXPECT().Test(tmock.Anything, tmock.Anything).RunAndReturn(func(_ context.Context, in distribution.TestInput) (distribution.TestResult, error) {
		host := strings.TrimPrefix(strings.TrimPrefix(in.URL, "https://"), "http://")
		host, path, _ := strings.Cut(host, "/")
		path, _, _ = strings.Cut(path, "?")
		route := ds.resolve(host)
		res := distribution.TestResult{Route: route, BehaviorIndex: -1}
		if route.Distribution == nil {
			return res, nil
		}
		prefix := strings.Trim(route.Distribution.Origin.PathPrefix, "/")
		if prefix != "" {
			prefix += "/"
		}
		res.Behavior = route.Distribution.DefaultBehavior
		res.Bucket, res.Key = route.Distribution.Origin.Bucket, prefix+path
		return res, nil
	}).Maybe()
	return m
}

// site is a website distribution of the tests.
func site(id, bucket string, aliases ...string) distribution.Distribution {
	return distribution.Distribution{
		ID: id, Enabled: true, Aliases: aliases, Origin: distribution.Origin{Bucket: bucket}, Mode: distribution.ModeWebsite,
		DefaultBehavior: distribution.Behavior{Access: distribution.AccessPublic, IndexDocument: "index.html"},
	}
}

// env is a cache service of a test with its dependencies.
type env struct {
	t      testing.TB
	svc    cache.Service
	origin *origin
	dists  *dists
	clk    *clock.Fake
	docs   *memDocs
	node   string
	local  cluster.Notifier
}

// envOptions tune newEnv.
type envOptions struct {
	opts   []cache.Option
	peers  []cache.Peer
	node   string
	docs   *memDocs
	origin *origin
	dists  *dists
	clk    *clock.Fake
	http   cache.HTTPDoer
	secret cache.SecretSource
	reg    metrics.Registry
	noStop bool
}

// newEnv starts a cache service.
func newEnv(t testing.TB, eo envOptions) *env {
	t.Helper()
	if eo.clk == nil {
		eo.clk = clock.NewFake(t0)
	}
	if eo.docs == nil {
		eo.docs = &memDocs{docs: map[string][]byte{}}
	}
	if eo.origin == nil {
		eo.origin = newOrigin(eo.clk)
	}
	if eo.dists == nil {
		eo.dists = newDists(site("web", "web", "www.example.com", "example.com", "*.preview.example.com"))
	}
	if eo.node == "" {
		eo.node = cluster.LocalNodeID
	}
	local := cluster.NewLocalNotifier(cluster.NotifierDeps{NodeID: eo.node, Logger: slog.New(slog.DiscardHandler)})
	svc, err := cache.New(cache.Deps{
		Origin: eo.origin.layer(t), Store: newStore(t, eo.docs), Locks: lock.NewLocal(lock.Deps{}), Notifier: local,
		Distributions: eo.dists.mock(t), Peers: eo.peers, NodeID: eo.node, HTTP: eo.http, Secrets: eo.secret,
		Registry: eo.reg, Clock: eo.clk, Logger: slog.New(slog.DiscardHandler),
	}, append([]cache.Option{cache.WithSyncInterval(time.Hour), cache.WithRetryInterval(time.Hour)}, eo.opts...)...)
	require.NoError(t, err)
	require.NoError(t, svc.Start(context.Background()))
	if !eo.noStop {
		t.Cleanup(func() { _ = svc.Stop(context.Background()) })
	}
	return &env{t: t, svc: svc, origin: eo.origin, dists: eo.dists, clk: eo.clk, docs: eo.docs, node: eo.node, local: local}
}

// result is what a request through the cache got.
type result struct {
	status int
	body   []byte
	info   domain.ObjectInfo
	header http.Header
	err    error
	// handled is false when the cache left the read to the origin.
	handled bool
}

// reqOpt tunes a request.
type reqOpt func(*cache.Request, *object.GetObjectOptions)

func withHeader(k, v string) reqOpt {
	return func(r *cache.Request, _ *object.GetObjectOptions) { r.Header.Add(k, v) }
}

func withQuery(q string) reqOpt {
	return func(r *cache.Request, _ *object.GetObjectOptions) { r.RawQuery = q }
}

func withRange(start, end int64) reqOpt {
	return func(_ *cache.Request, o *object.GetObjectOptions) { o.Range = &domain.Range{Start: start, End: end} }
}

func withConditions(c domain.Conditions) reqOpt {
	return func(_ *cache.Request, o *object.GetObjectOptions) { o.Conditions = c }
}

func withMethod(m string) reqOpt {
	return func(r *cache.Request, _ *object.GetObjectOptions) { r.Method = m }
}

func withCredentials(ak string) reqOpt {
	return func(r *cache.Request, _ *object.GetObjectOptions) { r.Credentials, r.Credential = true, ak }
}

// get reads a path of a host of the default distribution through the
// cache, as the website engine does (the key is the path).
func (e *env) get(host, path string, opts ...reqOpt) result {
	e.t.Helper()
	return e.read(host, path, false, opts...)
}

// head describes a path through the cache.
func (e *env) head(host, path string, opts ...reqOpt) result {
	e.t.Helper()
	return e.read(host, path, true, opts...)
}

func (e *env) read(host, path string, head bool, opts ...reqOpt) result {
	e.t.Helper()
	route := e.dists.resolve(host)
	require.NotNil(e.t, route.Distribution, "no distribution for %s", host)
	d := route.Distribution
	req := cache.Request{Distribution: d, Alias: route.Alias, Host: route.Host, Method: http.MethodGet, Path: path, Header: http.Header{}}
	var gopts object.GetObjectOptions
	for _, o := range opts {
		o(&req, &gopts)
	}
	if head {
		req.Method = http.MethodHead
	}
	ctx, ex := e.svc.Begin(context.Background(), req)
	prefix := strings.Trim(d.Origin.PathPrefix, "/")
	if prefix != "" {
		prefix += "/"
	}
	key := prefix + strings.TrimPrefix(path, "/")
	if strings.HasSuffix(path, "/") && d.DefaultBehavior.IndexDocument != "" {
		// The engine reads the index document of a directory.
		key += d.DefaultBehavior.IndexDocument
	}
	var res result
	if head {
		res.info, res.handled, res.err = e.svc.GetObjectInfo(ctx, d.Origin.Bucket, key, gopts)
	} else {
		var rd *object.Reader
		rd, res.handled, res.err = e.svc.GetObject(ctx, d.Origin.Bucket, key, gopts)
		if rd != nil {
			res.info = rd.Info
			res.body, _ = io.ReadAll(rd)
			_ = rd.Close()
		}
	}
	res.status = http.StatusOK
	if res.err != nil {
		res.status = http.StatusNotFound
	}
	res.header = http.Header{}
	ex.Decorate(res.status, res.header)
	ex.Finish(context.Background(), res.status, int64(len(res.body)))
	return res
}

// xcache is the X-Cache of a result.
func (r result) xcache() string { return r.header.Get(cache.HeaderXCache) }

// setDist stores the cache settings of a distribution.
func (e *env) setDist(cfg cache.DistributionConfig) cache.DistributionConfig {
	e.t.Helper()
	cur, err := e.svc.GetDistribution(context.Background(), cfg.Distribution)
	require.NoError(e.t, err)
	out, err := e.svc.UpdateDistribution(context.Background(), cfg, cur.ETag)
	require.NoError(e.t, err)
	return out
}

func ptr[T any](v T) *T { return &v }

func dur(d time.Duration) *cache.Duration { return ptr(cache.Duration(d)) }

func size(n int64) *cache.Size { return ptr(cache.Size(n)) }

// textInfo marks an object textual.
func textInfo(ct string) func(*domain.ObjectInfo) {
	return func(i *domain.ObjectInfo) { i.ContentType = ct }
}

// cacheControl sets the Cache-Control of an object.
func cacheControl(cc string) func(*domain.ObjectInfo) {
	return func(i *domain.ObjectInfo) { i.CacheControl = cc }
}

// payload returns n deterministic bytes.
func payload(n int, seed byte) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(i*31) ^ seed
	}
	return out
}

// Bounds of the waits of the tests.
const (
	timeout = 10 * time.Second
	tick    = 10 * time.Millisecond
)
