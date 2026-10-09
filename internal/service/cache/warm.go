package cache

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/distribution"
	"github.com/r3n3r0/nexo/internal/service/object"
	"github.com/r3n3r0/nexo/internal/service/sysstore"
)

// Limits of the warm-ups.
const (
	maxWarmObjects     = 100000
	maxWarmConcurrency = 64
	warmReport         = time.Second
	// warmOnWriteSlots bounds the concurrent warm-ups on write.
	warmOnWriteSlots = 4
)

// warmNodeDoc is the document of the progress of a warm-up on a node.
func warmNodeDoc(id, node string) string {
	return warmDir + "/" + id + "/" + base64.RawURLEncoding.EncodeToString([]byte(node)) + docSuffix
}

// Warm implements Operations.
func (s *service) Warm(ctx context.Context, req WarmRequest) (WarmJob, error) {
	d, err := s.deps.Distributions.Get(ctx, req.Distribution)
	if err != nil {
		return WarmJob{}, err
	}
	if err := s.validateWarm(&req, d); err != nil {
		return WarmJob{}, err
	}
	job := WarmJob{ID: s.newID("warm"), Request: req, CreatedAt: s.now().UTC(), State: WarmRunning}
	job.Nodes = append(job.Nodes, WarmNode{NodeStatus: NodeStatus{Node: s.deps.NodeID, State: NodePending}})
	local := !s.shared()
	if local {
		for _, p := range s.deps.Peers {
			job.Nodes = append(job.Nodes, WarmNode{NodeStatus: NodeStatus{Node: p.NodeID(), State: NodePending}})
		}
	}
	if _, err := s.putDoc(ctx, docPath(warmDir, job.ID), job); err != nil {
		return WarmJob{}, err
	}
	if err := s.StartWarm(ctx, job); err != nil {
		return WarmJob{}, err
	}
	if local {
		// Each node warms its own cache.
		for _, p := range s.deps.Peers {
			if err := p.StartWarm(ctx, job); err != nil {
				s.deps.Logger.WarnContext(ctx, "cache: warm-up not started on a peer", "peer", p.NodeID(), "job", job.ID, "err", err)
				s.writeWarmStatus(ctx, job.ID, WarmNode{NodeStatus: NodeStatus{Node: p.NodeID(), State: WarmFailed, Error: err.Error(), At: s.now().UTC()}})
			}
		}
	}
	return job, nil
}

// validateWarm checks a warm-up and fills its defaults.
func (s *service) validateWarm(req *WarmRequest, d distribution.Distribution) error {
	if req.Domain == "" {
		for _, a := range d.Aliases {
			if !strings.HasPrefix(a, "*.") {
				req.Domain = a
				break
			}
		}
	}
	req.Domain = normalizeHost(req.Domain)
	if req.Domain == "" || strings.HasPrefix(req.Domain, "*.") || !domainOf(d, req.Domain) {
		return fmt.Errorf("%w: warm-up domain %q is not a host of %s", ErrInvalid, req.Domain, d.ID)
	}
	for _, raw := range req.URLs {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("%w: %q is not an http(s) URL", ErrInvalid, raw)
		}
		if normalizeHost(u.Host) != req.Domain {
			return fmt.Errorf("%w: %s is not on the domain %s", ErrInvalid, raw, req.Domain)
		}
	}
	for _, p := range req.Paths {
		if !strings.HasPrefix(p, "/") {
			return fmt.Errorf("%w: path %q must start with /", ErrInvalid, p)
		}
	}
	if req.Prefix != "" && !strings.HasPrefix(req.Prefix, "/") {
		return fmt.Errorf("%w: prefix %q must start with /", ErrInvalid, req.Prefix)
	}
	if req.Pattern != nil {
		if _, err := s.deps.Matcher.Compile(context.Background(), *req.Pattern); err != nil {
			return fmt.Errorf("%w: pattern: %v", ErrInvalid, err)
		}
	}
	if len(req.Paths)+len(req.URLs) == 0 && req.Prefix == "" && req.Pattern == nil {
		return fmt.Errorf("%w: nothing to warm (paths, urls, prefix or pattern)", ErrInvalid)
	}
	if len(req.Paths)+len(req.URLs) > maxWarmObjects {
		return fmt.Errorf("%w: at most %d paths", ErrInvalid, maxWarmObjects)
	}
	if req.Concurrency <= 0 {
		req.Concurrency = defaultWarmConcurrency
	}
	req.Concurrency = min(req.Concurrency, maxWarmConcurrency)
	if req.Bandwidth <= 0 {
		req.Bandwidth = defaultWarmBandwidth
	}
	return nil
}

// StartWarm implements Node.
func (s *service) StartWarm(ctx context.Context, job WarmJob) error {
	d, err := s.deps.Distributions.Get(ctx, job.Request.Distribution)
	if err != nil {
		return err
	}
	req := job.Request
	if err := s.validateWarm(&req, d); err != nil {
		return err
	}
	job.Request = req
	life := s.lifeContext()
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		s.runWarm(life, job, d)
	}()
	return nil
}

// runWarm reads every object of a warm-up through the cache of this node.
func (s *service) runWarm(ctx context.Context, job WarmJob, d distribution.Distribution) {
	st := WarmNode{NodeStatus: NodeStatus{Node: s.deps.NodeID, State: WarmRunning, At: s.now().UTC()}}
	paths, err := s.warmPaths(ctx, job.Request, d)
	if err != nil {
		st.State, st.Error = WarmFailed, err.Error()
		s.writeWarmStatus(ctx, job.ID, st)
		return
	}
	st.Total = len(paths)
	s.writeWarmStatus(ctx, job.ID, st)
	limiter := newByteLimiter(int64(job.Request.Bandwidth))
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		next     = make(chan string)
		lastSave = s.now()
	)
	for range job.Request.Concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range next {
				n, err := s.prefetch(ctx, &d, job.Request.Domain, p, limiter)
				mu.Lock()
				st.Done++
				st.Bytes += n
				if err != nil {
					st.Failed++
					if st.Error == "" {
						st.Error = p + ": " + err.Error()
					}
				}
				save := s.now().Sub(lastSave) >= warmReport
				if save {
					lastSave = s.now()
				}
				cp := st
				mu.Unlock()
				if save {
					s.writeWarmStatus(ctx, job.ID, cp)
				}
			}
		}()
	}
	for _, p := range paths {
		select {
		case next <- p:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
	}
	close(next)
	wg.Wait()
	st.State, st.At = WarmCompleted, s.now().UTC()
	if ctx.Err() != nil {
		st.State, st.Error = WarmFailed, "interrupted"
	}
	s.writeWarmStatus(context.WithoutCancel(ctx), job.ID, st)
	s.deps.Logger.InfoContext(ctx, "cache: warm-up done", "job", job.ID, "objects", st.Done, "failed", st.Failed, "bytes", st.Bytes)
}

// warmPaths lists the paths of a warm-up.
func (s *service) warmPaths(ctx context.Context, req WarmRequest, d distribution.Distribution) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if !seen[p] && len(out) < maxWarmObjects {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, p := range req.Paths {
		add(p)
	}
	for _, raw := range req.URLs {
		if u, err := url.Parse(raw); err == nil {
			add(cmpOr(u.Path, "/"))
		}
	}
	if req.Prefix == "" && req.Pattern == nil {
		return out, nil
	}
	var pattern distribution.CompiledPattern
	listPrefix := req.Prefix
	if req.Pattern != nil {
		var err error
		if pattern, err = s.deps.Matcher.Compile(ctx, *req.Pattern); err != nil {
			return nil, err
		}
		if pre, _ := literalPrefix(*req.Pattern); len(pre) > len(listPrefix) {
			listPrefix = pre
		}
	}
	keyPrefix := keyPrefixOf(d) + strings.TrimPrefix(listPrefix, "/")
	in := domain.ListObjectsInput{Bucket: d.Origin.Bucket, Prefix: keyPrefix, MaxKeys: 1000}
	for len(out) < maxWarmObjects {
		res, err := s.deps.Origin.ListObjects(ctx, in)
		if err != nil {
			return nil, fmt.Errorf("cache: list %s/%s: %w", d.Origin.Bucket, keyPrefix, err)
		}
		for _, o := range res.Objects {
			p := "/" + strings.TrimPrefix(o.Key, keyPrefixOf(d))
			if strings.HasSuffix(o.Key, "/") || (req.Prefix != "" && !strings.HasPrefix(p, req.Prefix)) || (pattern != nil && !pattern.Match(p)) {
				continue
			}
			add(p)
		}
		if !res.IsTruncated || res.NextContinuationToken == "" {
			break
		}
		in.ContinuationToken = res.NextContinuationToken
	}
	return out, nil
}

// prefetch reads a path of a distribution through the cache of this node
// (or of the owner of its key) and returns the bytes read.
func (s *service) prefetch(ctx context.Context, d *distribution.Distribution, host, path string, limiter *byteLimiter) (int64, error) {
	key := keyPrefixOf(*d) + strings.TrimPrefix(path, "/")
	if strings.HasSuffix(path, "/") {
		if d.DefaultBehavior.IndexDocument == "" {
			return 0, fmt.Errorf("%w: no index document", domain.ErrObjectNotFound)
		}
		key += d.DefaultBehavior.IndexDocument
	}
	// The exchange is not finished: a warm-up is not a request served.
	rctx, _ := s.Begin(ctx, Request{Distribution: d, Host: host, Method: http.MethodGet, Path: path, Header: http.Header{}})
	rd, handled, err := s.GetObject(rctx, d.Origin.Bucket, key, object.GetObjectOptions{})
	if err != nil {
		return 0, err
	}
	if !handled {
		return 0, errors.New("not cacheable")
	}
	defer func() { _ = rd.Close() }()
	return io.Copy(io.Discard, limiter.reader(ctx, rd))
}

// writeWarmStatus stores the progress of this node (or of a peer it could
// not reach).
func (s *service) writeWarmStatus(ctx context.Context, id string, st WarmNode) {
	if _, err := s.putDoc(context.WithoutCancel(ctx), warmNodeDoc(id, st.Node), st); err != nil {
		s.deps.Logger.DebugContext(ctx, "cache: warm-up progress not stored", "job", id, "err", err)
	}
}

// GetWarmJob implements Operations.
func (s *service) GetWarmJob(ctx context.Context, dist, id string) (WarmJob, error) {
	if !validHistoryID(id) {
		return WarmJob{}, fmt.Errorf("%w: %q", ErrWarmJobNotFound, id)
	}
	data, err := s.deps.Store.Get(ctx, docPath(warmDir, id))
	if errors.Is(err, sysstore.ErrNotFound) {
		return WarmJob{}, fmt.Errorf("%w: %s", ErrWarmJobNotFound, id)
	}
	if err != nil {
		return WarmJob{}, fmt.Errorf("cache: read warm-up %s: %w", id, err)
	}
	var job WarmJob
	if err := json.Unmarshal(data, &job); err != nil {
		return WarmJob{}, fmt.Errorf("cache: decode warm-up %s: %w", id, err)
	}
	if dist != "" && job.Request.Distribution != dist {
		return WarmJob{}, fmt.Errorf("%w: %s", ErrWarmJobNotFound, id)
	}
	docs, err := s.deps.Store.List(ctx, warmDir+"/"+id)
	if err != nil {
		return WarmJob{}, err
	}
	reported := map[string]WarmNode{}
	for _, raw := range docs {
		var st WarmNode
		if json.Unmarshal(raw, &st) == nil && st.Node != "" {
			reported[st.Node] = st
		}
	}
	running, failed := false, false
	for i, n := range job.Nodes {
		if st, ok := reported[n.Node]; ok {
			job.Nodes[i] = st
		}
		switch job.Nodes[i].State {
		case WarmRunning, NodePending:
			running = true
		case WarmFailed:
			failed = true
		}
	}
	switch {
	case running:
		job.State = WarmRunning
	case failed:
		job.State = WarmFailed
	default:
		job.State = WarmCompleted
	}
	return job, nil
}

// warmOnWrite caches at once the written objects the distributions ask to
// warm (docs/11 §11.6 warmOnWrite).
func (s *service) warmOnWrite(ctx context.Context, change ObjectChange) {
	sn := s.snap.Load()
	for _, d := range s.distributionsOf(change.Bucket) {
		cd := sn.dists[d.ID]
		prefix := keyPrefixOf(d)
		if cd == nil || len(cd.warm) == 0 || !strings.HasPrefix(change.Key, prefix) {
			continue
		}
		path := "/" + strings.TrimPrefix(change.Key, prefix)
		matched := false
		for _, p := range cd.warm {
			if p.Match(path) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		hosts := []string{}
		for _, a := range d.Aliases {
			if !strings.HasPrefix(a, "*.") {
				hosts = append(hosts, a)
			}
		}
		dist := d
		for _, h := range hosts {
			select {
			case s.warmSlots <- struct{}{}:
			default:
				s.deps.Logger.DebugContext(ctx, "cache: warm-up on write skipped (busy)", "key", change.Key)
				return
			}
			life := s.lifeContext()
			s.bg.Add(1)
			go func() {
				defer s.bg.Done()
				defer func() { <-s.warmSlots }()
				if _, err := s.prefetch(life, &dist, h, path, newByteLimiter(defaultWarmBandwidth)); err != nil {
					s.deps.Logger.DebugContext(life, "cache: warm-up on write failed", "key", change.Key, "host", h, "err", err)
				}
			}()
			if !sn.resolveFor(d.ID, h, path).p.IncludeHost {
				// The aliases share the entries: one read warms them all.
				break
			}
		}
	}
}

// byteLimiter bounds the bytes read per second.
type byteLimiter struct {
	rate int64
	mu   sync.Mutex
	next time.Time
}

func newByteLimiter(rate int64) *byteLimiter { return &byteLimiter{rate: rate} }

// wait blocks until n more bytes fit in the rate.
func (l *byteLimiter) wait(ctx context.Context, n int) error {
	if l.rate <= 0 || n <= 0 {
		return nil
	}
	l.mu.Lock()
	now := time.Now()
	if l.next.Before(now) {
		l.next = now
	}
	l.next = l.next.Add(time.Duration(float64(n) / float64(l.rate) * float64(time.Second)))
	delay := l.next.Sub(now)
	l.mu.Unlock()
	if delay <= 0 {
		return nil
	}
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// reader wraps r with the limit.
func (l *byteLimiter) reader(ctx context.Context, r io.Reader) io.Reader {
	return readerFunc(func(p []byte) (int, error) {
		n, err := r.Read(p)
		if werr := l.wait(ctx, n); werr != nil && err == nil {
			err = werr
		}
		return n, err
	})
}

// readerFunc adapts a function to io.Reader.
type readerFunc func(p []byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }
