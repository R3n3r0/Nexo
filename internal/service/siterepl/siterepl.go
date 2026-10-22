package siterepl

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/bucket"
	"github.com/r3n3r0/nexo/internal/service/replication"
	"github.com/r3n3r0/nexo/internal/service/sysstore"
)

// System documents of the site replication.
const (
	// Dir holds the documents.
	Dir        = "site-replication"
	configPath = Dir + "/config.json"
	statusPath = Dir + "/status.json"
	baseDir    = Dir + "/base"
)

// Prefixes of the keys of the replicated entries.
const (
	keyIAM    = "iam/"
	keyPolicy = "policy/"
	keyBucket = "bucket/"
)

// sealAAD binds the sealed entries to their use.
const sealAAD = "nexo-site-replication-v1"

// Deps are the dependencies of the service.
type Deps struct {
	// DeploymentID identifies this site (required).
	DeploymentID string
	// RootUser and RootPassword are the root credentials, shared by the
	// sites: they call each other and replicate the buckets with them
	// (required).
	RootUser     string
	RootPassword string
	IAM          IAM
	Policies     Policies
	Buckets      bucket.Service
	Settings     BucketSettings
	Replication  replication.Service
	// Docs keeps the group, the bases and the status (required).
	Docs sysstore.Store
	// Sealer seals the entries exchanged with the other sites (required).
	Sealer Sealer
	// Dialer opens the clients of the other sites (default: the Admin
	// API over HTTP).
	Dialer Dialer
	// Leader runs the rounds on one node; nil runs them here.
	Leader Leader
	Clock  clock.Clock
	Logger *slog.Logger
}

// Option configures the service.
type Option func(*service)

// WithInterval sets the time between two rounds (default 5 s).
func WithInterval(d time.Duration) Option {
	return func(s *service) {
		if d > 0 {
			s.interval = d
		}
	}
}

// WithRecheck sets how often the replication rules of a bucket already
// checked are checked again (default 10 min).
func WithRecheck(d time.Duration) Option {
	return func(s *service) {
		if d > 0 {
			s.recheck = d
		}
	}
}

type service struct {
	deps     Deps
	interval time.Duration
	recheck  time.Duration

	trigger chan struct{}
	cancel  context.CancelFunc
	done    chan struct{}

	// round serializes the rounds.
	round sync.Mutex
	// mu guards what follows: the state of the leader.
	mu sync.Mutex
	// checked is when the rules of each bucket were last made right, for
	// the sites of signature.
	checked   map[string]time.Time
	signature string
	// resyncs are the resyncs to start, per bucket, one at a time.
	resyncs map[string][]string
	// logged is the last error logged per peer and entry.
	logged map[string]string
}

var _ Service = (*service)(nil)

// New returns the site replication service.
func New(deps Deps, opts ...Option) (Service, error) {
	switch {
	case deps.DeploymentID == "" || deps.RootUser == "" || deps.RootPassword == "":
		return nil, fmt.Errorf("%w: site replication requires the deployment ID and the root credentials", domain.ErrInvalidArgument)
	case deps.IAM == nil || deps.Policies == nil || deps.Buckets == nil || deps.Settings == nil || deps.Replication == nil:
		return nil, fmt.Errorf("%w: site replication requires the IAM, policy, bucket and replication services", domain.ErrInvalidArgument)
	case deps.Docs == nil || deps.Sealer == nil:
		return nil, fmt.Errorf("%w: site replication requires the system documents and a sealer", domain.ErrInvalidArgument)
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	s := &service{deps: deps, interval: 5 * time.Second, recheck: 10 * time.Minute, trigger: make(chan struct{}, 1),
		checked: map[string]time.Time{}, resyncs: map[string][]string{}, logged: map[string]string{}}
	if s.deps.Dialer == nil {
		s.deps.Dialer = adminDialer{user: deps.RootUser, password: deps.RootPassword}
	}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// errNotEnabled is the error of the calls of the other sites when this
// one is in no group.
func errNotEnabled() error {
	return fmt.Errorf("%w: this site is in no site replication group", domain.ErrConflict)
}

// --- documents --------------------------------------------------------------------------

type configDoc struct {
	Sites   []Site    `json:"sites"`
	Updated time.Time `json:"updated"`
}

type statusDoc struct {
	Peers   map[string]PeerStatus `json:"peers"`
	Entries int                   `json:"entries"`
	// Resyncs are the resyncs still to start, per bucket.
	Resyncs map[string][]string `json:"resyncs,omitempty"`
}

func basePath(peer string) string { return baseDir + "/" + peer + ".json" }

func (s *service) readJSON(ctx context.Context, path string, v any) (bool, error) {
	data, err := s.deps.Docs.Get(ctx, path)
	if errors.Is(err, domain.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return false, fmt.Errorf("siterepl: %s: %w", path, err)
	}
	return true, nil
}

func (s *service) writeJSON(ctx context.Context, path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.deps.Docs.Put(ctx, path, data)
}

func (s *service) deleteDoc(ctx context.Context, path string) error {
	if err := s.deps.Docs.Delete(ctx, path); err != nil && !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	return nil
}

// config returns the group (nil when this site is in none).
func (s *service) config(ctx context.Context) (*configDoc, error) {
	var c configDoc
	ok, err := s.readJSON(ctx, configPath, &c)
	if err != nil || !ok || len(c.Sites) == 0 {
		return nil, err
	}
	return &c, nil
}

func (s *service) self(c *configDoc) (Site, []Site) {
	var me Site
	var peers []Site
	for _, site := range c.Sites {
		if site.DeploymentID == s.deps.DeploymentID {
			me = site
		} else {
			peers = append(peers, site)
		}
	}
	return me, peers
}

// --- group management -----------------------------------------------------------------

// validSiteName reports a name of 1-63 letters, digits, '.', '_' or '-',
// starting with a letter or a digit.
func validSiteName(name string) bool {
	if name == "" || len(name) > 63 {
		return false
	}
	for i, c := range name {
		alnum := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		if !alnum && (i == 0 || !strings.ContainsRune("._-", c)) {
			return false
		}
	}
	return true
}

func validURL(raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || (u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("%w: %q is not an http(s)://host[:port] URL", domain.ErrInvalidArgument, raw)
	}
	return raw, nil
}

// Add implements Service.
func (s *service) Add(ctx context.Context, inputs []SiteInput) (Info, error) {
	if len(inputs) < 2 {
		return Info{}, fmt.Errorf("%w: site replication needs at least two sites", domain.ErrInvalidArgument)
	}
	if cur, err := s.config(ctx); err != nil {
		return Info{}, err
	} else if cur != nil {
		return Info{}, fmt.Errorf("%w: this site is already in a site replication group: remove it first", domain.ErrConflict)
	}
	names := map[string]bool{}
	ids := map[string]string{}
	sites := make([]Site, 0, len(inputs))
	for _, in := range inputs {
		if !validSiteName(in.Name) {
			return Info{}, fmt.Errorf("%w: site name %q: 1-63 letters, digits, '.', '_' or '-'", domain.ErrInvalidArgument, in.Name)
		}
		if names[in.Name] {
			return Info{}, fmt.Errorf("%w: site name %q given twice", domain.ErrInvalidArgument, in.Name)
		}
		names[in.Name] = true
		endpoint, err := validURL(in.Endpoint)
		if err != nil {
			return Info{}, err
		}
		admin, err := validURL(in.AdminEndpoint)
		if err != nil {
			return Info{}, err
		}
		site := Site{Name: in.Name, Endpoint: endpoint, AdminEndpoint: admin}
		peer, err := s.deps.Dialer.Dial(ctx, site)
		if err != nil {
			return Info{}, fmt.Errorf("site %s: %w", in.Name, err)
		}
		info, err := peer.Info(ctx)
		if err != nil {
			return Info{}, fmt.Errorf("%w: site %s (%s) cannot be reached with the root credentials of this site: %v",
				domain.ErrInvalidArgument, in.Name, admin, err)
		}
		if other, dup := ids[info.DeploymentID]; dup {
			return Info{}, fmt.Errorf("%w: sites %s and %s are the same deployment", domain.ErrInvalidArgument, other, in.Name)
		}
		ids[info.DeploymentID] = in.Name
		if info.Enabled {
			return Info{}, fmt.Errorf("%w: site %s is already in a site replication group", domain.ErrConflict, in.Name)
		}
		site.DeploymentID = info.DeploymentID
		sites = append(sites, site)
	}
	if _, ok := ids[s.deps.DeploymentID]; !ok {
		return Info{}, fmt.Errorf("%w: the sites must include this one (deployment %s)", domain.ErrInvalidArgument, s.deps.DeploymentID)
	}
	sort.Slice(sites, func(i, j int) bool { return sites[i].Name < sites[j].Name })
	var joined []Site
	for _, site := range sites {
		if site.DeploymentID == s.deps.DeploymentID {
			continue
		}
		peer, err := s.deps.Dialer.Dial(ctx, site)
		if err == nil {
			err = peer.Join(ctx, sites)
		}
		if err != nil {
			for _, j := range joined {
				if p, derr := s.deps.Dialer.Dial(ctx, j); derr == nil {
					_ = p.Leave(context.WithoutCancel(ctx))
				}
			}
			return Info{}, fmt.Errorf("site %s did not join: %w", site.Name, err)
		}
		joined = append(joined, site)
	}
	if err := s.writeJSON(ctx, configPath, configDoc{Sites: sites, Updated: s.deps.Clock.Now().UTC()}); err != nil {
		return Info{}, err
	}
	s.deps.Logger.InfoContext(ctx, "siterepl: site replication group created", "sites", len(sites))
	_ = s.Sync(ctx)
	return s.Info(ctx)
}

// Join implements Service.
func (s *service) Join(ctx context.Context, sites []Site) error {
	found := false
	for _, site := range sites {
		if site.DeploymentID == s.deps.DeploymentID {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("%w: the group does not hold this site", domain.ErrInvalidArgument)
	}
	if cur, err := s.config(ctx); err != nil {
		return err
	} else if cur != nil {
		return fmt.Errorf("%w: this site is already in a site replication group", domain.ErrConflict)
	}
	if err := s.writeJSON(ctx, configPath, configDoc{Sites: sites, Updated: s.deps.Clock.Now().UTC()}); err != nil {
		return err
	}
	s.deps.Logger.InfoContext(ctx, "siterepl: joined a site replication group", "sites", len(sites))
	s.kick()
	return nil
}

// Leave implements Service.
func (s *service) Leave(ctx context.Context) error {
	s.round.Lock()
	defer s.round.Unlock()
	cur, err := s.config(ctx)
	if err != nil || cur == nil {
		return err
	}
	if err := s.deleteDoc(ctx, configPath); err != nil {
		return err
	}
	for _, site := range cur.Sites {
		_ = s.deleteDoc(ctx, basePath(site.DeploymentID))
	}
	_ = s.deleteDoc(ctx, statusPath)
	s.mu.Lock()
	s.checked, s.signature, s.resyncs = map[string]time.Time{}, "", map[string][]string{}
	s.mu.Unlock()
	s.removeSiteRules(ctx)
	s.deps.Logger.InfoContext(ctx, "siterepl: left the site replication group")
	return nil
}

// Remove implements Service.
func (s *service) Remove(ctx context.Context) error {
	cur, err := s.config(ctx)
	if err != nil {
		return err
	}
	if cur == nil {
		return fmt.Errorf("%w: this site is in no site replication group", domain.ErrNotFound)
	}
	_, peers := s.self(cur)
	var failed []string
	for _, site := range peers {
		peer, err := s.deps.Dialer.Dial(ctx, site)
		if err == nil {
			err = peer.Leave(ctx)
		}
		if err != nil {
			s.deps.Logger.WarnContext(ctx, "siterepl: a site did not leave the group", "site", site.Name, "err", err)
			failed = append(failed, site.Name)
		}
	}
	if err := s.Leave(ctx); err != nil {
		return err
	}
	if len(failed) > 0 {
		return fmt.Errorf("%w: the group is removed here, but sites %s were not reached: remove it there too",
			domain.ErrConflict, strings.Join(failed, ", "))
	}
	return nil
}

// Info implements Service.
func (s *service) Info(ctx context.Context) (Info, error) {
	out := Info{DeploymentID: s.deps.DeploymentID, Sites: []Site{}, Peers: []PeerStatus{}}
	cur, err := s.config(ctx)
	if err != nil || cur == nil {
		return out, err
	}
	me, peers := s.self(cur)
	out.Enabled, out.Name, out.Sites = true, me.Name, cur.Sites
	var st statusDoc
	if _, err := s.readJSON(ctx, statusPath, &st); err != nil {
		return out, err
	}
	out.Entries = st.Entries
	for _, p := range peers {
		ps, ok := st.Peers[p.DeploymentID]
		if !ok {
			ps = PeerStatus{}
		}
		ps.Name, ps.DeploymentID = p.Name, p.DeploymentID
		out.Peers = append(out.Peers, ps)
	}
	return out, nil
}

// Peer implements Service.
func (s *service) Peer(ctx context.Context) (PeerInfo, error) {
	out := PeerInfo{DeploymentID: s.deps.DeploymentID}
	cur, err := s.config(ctx)
	if err != nil {
		return out, err
	}
	if cur != nil {
		out.Enabled, out.Sites = true, cur.Sites
	}
	return out, nil
}

// --- entries -------------------------------------------------------------------------------

// item is a local entry with its value.
type item struct {
	value []byte
	Entry
}

func hashOf(v []byte) string {
	sum := sha256.Sum256(v)
	return hex.EncodeToString(sum[:])
}

// local returns the replicated entries of this site. Any failure fails
// it whole: a missing entry would read as a deletion.
func (s *service) local(ctx context.Context) (map[string]item, error) {
	out := map[string]item{}
	recs, err := s.deps.IAM.SiteRecords(ctx)
	if err != nil {
		return nil, fmt.Errorf("siterepl: IAM: %w", err)
	}
	for k, r := range recs {
		out[keyIAM+k] = item{value: r.Value, Entry: Entry{Hash: hashOf(r.Value), ModTime: r.ModTime}}
	}
	policies, err := s.deps.Policies.ListPolicies(ctx)
	if err != nil {
		return nil, fmt.Errorf("siterepl: policies: %w", err)
	}
	for _, p := range policies {
		if p.BuiltIn {
			continue
		}
		out[keyPolicy+p.Name] = item{value: p.Doc, Entry: Entry{Hash: hashOf(p.Doc), ModTime: p.UpdatedAt}}
	}
	buckets, err := s.deps.Buckets.List(ctx, bucket.ListInput{})
	if err != nil {
		return nil, fmt.Errorf("siterepl: buckets: %w", err)
	}
	for _, b := range buckets {
		doc, err := s.deps.Settings.SiteBucket(ctx, b.Name)
		if errors.Is(err, domain.ErrBucketNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("siterepl: bucket %s: %w", b.Name, err)
		}
		out[keyBucket+b.Name] = item{value: doc, Entry: Entry{Hash: hashOf(doc)}}
	}
	return out, nil
}

// State implements Service.
func (s *service) State(ctx context.Context) (map[string]Entry, error) {
	if cur, err := s.config(ctx); err != nil {
		return nil, err
	} else if cur == nil {
		return nil, errNotEnabled()
	}
	items, err := s.local(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Entry, len(items))
	for k, it := range items {
		out[k] = it.Entry
	}
	return out, nil
}

// wireValue is a sealed entry.
type wireValue struct {
	Key    string `json:"key"`
	Delete bool   `json:"delete,omitempty"`
	Value  []byte `json:"value,omitempty"`
}

func (s *service) seal(ctx context.Context, values []wireValue) ([]byte, error) {
	plain, err := json.Marshal(values)
	if err != nil {
		return nil, err
	}
	return s.deps.Sealer.Seal(ctx, plain, []byte(sealAAD))
}

func (s *service) open(ctx context.Context, sealed []byte) ([]wireValue, error) {
	plain, err := s.deps.Sealer.Open(ctx, sealed, []byte(sealAAD))
	if err != nil {
		return nil, fmt.Errorf("%w: the entries cannot be opened: the sites must share the root credentials", domain.ErrInvalidArgument)
	}
	var values []wireValue
	if err := json.Unmarshal(plain, &values); err != nil {
		return nil, fmt.Errorf("%w: entries: %v", domain.ErrInvalidArgument, err)
	}
	return values, nil
}

// Items implements Service.
func (s *service) Items(ctx context.Context, keys []string) ([]byte, error) {
	if cur, err := s.config(ctx); err != nil {
		return nil, err
	} else if cur == nil {
		return nil, errNotEnabled()
	}
	items, err := s.local(ctx)
	if err != nil {
		return nil, err
	}
	values := make([]wireValue, 0, len(keys))
	for _, k := range keys {
		if it, ok := items[k]; ok {
			values = append(values, wireValue{Key: k, Value: it.value})
		} else {
			values = append(values, wireValue{Key: k, Delete: true})
		}
	}
	return s.seal(ctx, values)
}

// Apply implements Service.
func (s *service) Apply(ctx context.Context, sealed []byte) ([]Result, error) {
	if cur, err := s.config(ctx); err != nil {
		return nil, err
	} else if cur == nil {
		return nil, errNotEnabled()
	}
	values, err := s.open(ctx, sealed)
	if err != nil {
		return nil, err
	}
	res := s.applyLocal(ctx, values)
	s.kick()
	return res, nil
}

// kindOrder orders the changes: policies before the users that name them,
// users before their groups and access keys, buckets last; deletions in
// the reverse order.
func kindOrder(key string, del bool) int {
	order := 0
	switch {
	case strings.HasPrefix(key, keyPolicy):
		order = 0
	case strings.HasPrefix(key, keyIAM+"user/"):
		order = 1
	case strings.HasPrefix(key, keyIAM+"group/"):
		order = 2
	case strings.HasPrefix(key, keyIAM):
		order = 3
	default:
		order = 4
	}
	if del {
		return 10 - order
	}
	return order
}

// applyLocal applies changes here and reports each outcome.
func (s *service) applyLocal(ctx context.Context, values []wireValue) []Result {
	sort.SliceStable(values, func(i, j int) bool {
		a, b := kindOrder(values[i].Key, values[i].Delete), kindOrder(values[j].Key, values[j].Delete)
		if a != b {
			return a < b
		}
		return values[i].Key < values[j].Key
	})
	results := make([]Result, 0, len(values))
	for _, v := range values {
		err := s.applyOne(ctx, v)
		r := Result{Key: v.Key}
		if err != nil {
			r.Error = err.Error()
		}
		results = append(results, r)
	}
	return results
}

func (s *service) applyOne(ctx context.Context, v wireValue) error {
	switch {
	case strings.HasPrefix(v.Key, keyIAM):
		name := strings.TrimPrefix(v.Key, keyIAM)
		if v.Delete {
			return s.deps.IAM.ApplySiteRecords(ctx, nil, []string{name})
		}
		return s.deps.IAM.ApplySiteRecords(ctx, map[string][]byte{name: v.Value}, nil)
	case strings.HasPrefix(v.Key, keyPolicy):
		name := strings.TrimPrefix(v.Key, keyPolicy)
		if v.Delete {
			err := s.deps.Policies.DeletePolicy(ctx, name)
			if errors.Is(err, domain.ErrPolicyNotFound) {
				return nil
			}
			return err
		}
		return s.deps.Policies.SetPolicy(ctx, name, v.Value)
	case strings.HasPrefix(v.Key, keyBucket):
		name := strings.TrimPrefix(v.Key, keyBucket)
		if v.Delete {
			// Never forced: a bucket that still holds objects stays, and
			// the deletion is retried.
			err := s.deps.Buckets.Delete(ctx, name, bucket.DeleteOptions{})
			if errors.Is(err, domain.ErrBucketNotFound) {
				return nil
			}
			return err
		}
		var probe struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(v.Value, &probe); err != nil || probe.Name != name {
			return fmt.Errorf("%w: the settings of %s name another bucket", domain.ErrInvalidArgument, name)
		}
		if err := s.deps.Settings.ApplySiteBucket(ctx, v.Value); err != nil {
			return err
		}
		s.mu.Lock()
		delete(s.checked, name)
		s.mu.Unlock()
		return nil
	default:
		return fmt.Errorf("%w: unknown entry %q", domain.ErrInvalidArgument, v.Key)
	}
}
