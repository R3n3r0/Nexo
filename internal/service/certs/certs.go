package certs

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/acme"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/certs/dns01"
	"github.com/r3n3r0/nexo/internal/service/cluster"
	"github.com/r3n3r0/nexo/internal/service/metrics"
	"github.com/r3n3r0/nexo/internal/service/sysstore"
)

// Locations of the documents in the system store.
const (
	certsDir     = "certs"
	acmeAccount  = "acme/account.json"
	http01Dir    = "acme/http01"
	alpnDir      = "acme/alpn"
	docSuffix    = ".json"
	configLock   = "nexo-config/certs"
	maxChainPEM  = 64 << 10
	maxIDLength  = 64
	sealedPrefix = "nexo-cert:"
)

// storedCert is the document of a certificate: its description and the
// sealed chain and key.
type storedCert struct {
	Meta   Certificate `json:"meta"`
	Sealed []byte      `json:"sealed"`
}

// bundle is the sealed part of a certificate document.
type bundle struct {
	Chain string `json:"chain"`
	Key   string `json:"key"`
}

// loaded is a certificate ready for the handshakes.
type loaded struct {
	meta Certificate
	cert *tls.Certificate
}

// index is an immutable index of the loaded certificates.
type index struct {
	byID  map[string]*loaded
	exact map[string][]*loaded
	// wild maps "example.com" to the certificates of "*.example.com".
	wild map[string][]*loaded
}

func newIndex(certs []*loaded) *index {
	ix := &index{byID: map[string]*loaded{}, exact: map[string][]*loaded{}, wild: map[string][]*loaded{}}
	for _, c := range certs {
		ix.byID[c.meta.ID] = c
		for _, d := range c.meta.Domains {
			if rest, ok := strings.CutPrefix(d, "*."); ok {
				ix.wild[rest] = append(ix.wild[rest], c)
				continue
			}
			ix.exact[d] = append(ix.exact[d], c)
		}
	}
	return ix
}

// service implements Service.
type service struct {
	deps Deps
	opts options

	certs atomic.Pointer[index]
	// http01 and alpn cache the challenge answers read from the store.
	http01 sync.Map // token → key authorization
	alpn   sync.Map // domain → *tls.Certificate

	expiry metrics.Gauge

	// reconcileMu serializes the reconciliations; failures and warned
	// remember the last failed order and the last expiration warning of
	// every certificate.
	reconcileMu sync.Mutex
	failures    map[string]time.Time
	warned      map[string]time.Time

	// propagation waits for the DNS-01 records (Deps.Propagation).
	propagation dns01.Checker

	acctMu     sync.Mutex
	client     *acme.Client
	finalizers *finalizeLocations

	lifeMu      sync.Mutex
	unsubscribe []func()
	trigger     chan struct{}
	loopCancel  context.CancelFunc
	loopDone    chan struct{}
}

var _ Service = (*service)(nil)

// New returns the certificate service. The store, the locker, the
// notifier, the leader election and the sealer are required
// (domain.ErrInvalidArgument).
func New(deps Deps, opts ...Option) (Service, error) {
	switch {
	case deps.Store == nil, deps.Locks == nil, deps.Notifier == nil, deps.Leader == nil, deps.Sealer == nil:
		return nil, fmt.Errorf("%w: certs: the store, the locker, the notifier, the leader and the sealer are required", domain.ErrInvalidArgument)
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	o := defaultOptions()
	for _, opt := range opts {
		opt(&o)
	}
	s := &service{deps: deps, opts: o, failures: map[string]time.Time{}, warned: map[string]time.Time{}, trigger: make(chan struct{}, 1)}
	s.propagation = deps.Propagation
	if s.propagation == nil {
		s.propagation = dns01.NewChecker()
	}
	s.certs.Store(newIndex(nil))
	if deps.Metrics != nil {
		g, err := deps.Metrics.Gauge(context.Background(), metrics.Definition{
			Subsystem: "tls", Name: "certificate_expiry_seconds",
			Help:   "Seconds until the expiration of the certificates of the distributions.",
			Labels: []string{"certificate"},
		})
		if err != nil {
			return nil, fmt.Errorf("certs: metrics: %w", err)
		}
		s.expiry = g
	}
	return s, nil
}

// --- lifecycle --------------------------------------------------------------------

// leaderHook runs the ACME job while this node leads.
type leaderHook struct{ s *service }

var _ cluster.LeaderListener = leaderHook{}

// OnElected implements cluster.LeaderListener.
func (h leaderHook) OnElected(ctx context.Context) { h.s.startLoop(ctx) }

// OnRevoked implements cluster.LeaderListener.
func (h leaderHook) OnRevoked(context.Context) { h.s.stopLoop() }

// Start implements lifecycle.Runner.
func (s *service) Start(ctx context.Context) error {
	s.lifeMu.Lock()
	if len(s.unsubscribe) == 0 {
		reload := cluster.HandlerFunc(func(ctx context.Context, _ cluster.Event) error { return s.Reload(ctx) })
		s.unsubscribe = append(s.unsubscribe, s.deps.Notifier.Subscribe(EventCertificate, reload))
		for _, kind := range s.opts.reconcileOn {
			s.unsubscribe = append(s.unsubscribe, s.deps.Notifier.Subscribe(kind, cluster.HandlerFunc(func(context.Context, cluster.Event) error {
				s.kick()
				return nil
			})))
		}
	}
	s.lifeMu.Unlock()
	if err := s.Reload(ctx); err != nil {
		s.deps.Logger.WarnContext(ctx, "certs: initial load failed, retried on the next resynchronization", "err", err)
	}
	// The leader job: subscribed last, it may start at once.
	unsub := s.deps.Leader.Subscribe(leaderHook{s: s})
	s.lifeMu.Lock()
	s.unsubscribe = append(s.unsubscribe, unsub)
	s.lifeMu.Unlock()
	return nil
}

// Stop implements lifecycle.Runner.
func (s *service) Stop(context.Context) error {
	s.lifeMu.Lock()
	unsubs := s.unsubscribe
	s.unsubscribe = nil
	s.lifeMu.Unlock()
	for _, u := range unsubs {
		u()
	}
	s.stopLoop()
	return nil
}

// Trigger implements Service.
func (s *service) Trigger(context.Context) { s.kick() }

// kick asks the leader job for a reconciliation.
func (s *service) kick() {
	select {
	case s.trigger <- struct{}{}:
	default:
	}
}

// startLoop starts the leader job.
func (s *service) startLoop(ctx context.Context) {
	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()
	if s.loopCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	s.loopCancel, s.loopDone = cancel, done
	go func() {
		defer close(done)
		s.loop(ctx)
	}()
}

// stopLoop stops the leader job and waits for it.
func (s *service) stopLoop() {
	s.lifeMu.Lock()
	cancel, done := s.loopCancel, s.loopDone
	s.loopCancel, s.loopDone = nil, nil
	s.lifeMu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

// loop reconciles at start, periodically and when kicked.
func (s *service) loop(ctx context.Context) {
	ticker := time.NewTicker(s.opts.checkInterval)
	defer ticker.Stop()
	for {
		if err := s.reconcile(ctx); err != nil && ctx.Err() == nil {
			s.deps.Logger.WarnContext(ctx, "certs: reconciliation failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.trigger:
		}
	}
}

// --- store -------------------------------------------------------------------------

// Reload implements Service.
func (s *service) Reload(ctx context.Context) error {
	docs, err := s.deps.Store.List(ctx, certsDir)
	if err != nil {
		return fmt.Errorf("certs: list: %w", err)
	}
	var out []*loaded
	for name, data := range docs {
		if !strings.HasSuffix(name, docSuffix) {
			continue
		}
		l, err := s.decode(ctx, data)
		if err != nil {
			s.deps.Logger.WarnContext(ctx, "certs: skipping an unreadable certificate", "document", name, "err", err)
			continue
		}
		out = append(out, l)
	}
	s.certs.Store(newIndex(out))
	s.observe(ctx)
	return nil
}

// observe exports the expiration of the certificates.
func (s *service) observe(ctx context.Context) {
	if s.expiry == nil {
		return
	}
	now := s.deps.Clock.Now()
	for _, l := range s.certs.Load().byID {
		s.expiry.Set(ctx, l.meta.NotAfter.Sub(now).Seconds(), l.meta.ID)
	}
}

// decode opens a certificate document.
func (s *service) decode(ctx context.Context, data []byte) (*loaded, error) {
	var doc storedCert
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	plain, err := s.deps.Sealer.Open(ctx, doc.Sealed, []byte(sealedPrefix+doc.Meta.ID))
	if err != nil {
		return nil, err
	}
	var b bundle
	if err := json.Unmarshal(plain, &b); err != nil {
		return nil, err
	}
	cert, err := tls.X509KeyPair([]byte(b.Chain), []byte(b.Key))
	if err != nil {
		return nil, err
	}
	return &loaded{meta: doc.Meta, cert: &cert}, nil
}

// put seals and stores a certificate, applies it locally and tells the
// peers.
func (s *service) put(ctx context.Context, meta Certificate, b bundle) (Certificate, error) {
	plain, err := json.Marshal(b)
	if err != nil {
		return Certificate{}, err
	}
	sealed, err := s.deps.Sealer.Seal(ctx, plain, []byte(sealedPrefix+meta.ID))
	if err != nil {
		return Certificate{}, err
	}
	meta.Status = ""
	data, err := json.Marshal(storedCert{Meta: meta, Sealed: sealed})
	if err != nil {
		return Certificate{}, err
	}
	if err := s.deps.Store.Put(ctx, certsDir+"/"+meta.ID+docSuffix, data); err != nil {
		return Certificate{}, fmt.Errorf("certs: store %s: %w", meta.ID, err)
	}
	if err := s.Reload(ctx); err != nil {
		s.deps.Logger.WarnContext(ctx, "certs: reload after a write failed", "err", err)
	}
	s.broadcast(ctx, meta.ID)
	return s.withStatus(meta), nil
}

func (s *service) broadcast(ctx context.Context, id string) {
	if err := s.deps.Notifier.Broadcast(context.WithoutCancel(ctx), cluster.Event{Kind: EventCertificate, Name: id}); err != nil {
		s.deps.Logger.WarnContext(ctx, "certs: change not delivered to every peer (retried in the background)", "id", id, "err", err)
	}
}

// withStatus computes the status of a certificate now.
func (s *service) withStatus(m Certificate) Certificate {
	now := s.deps.Clock.Now()
	switch {
	case now.Before(m.NotBefore):
		m.Status = StatusNotYetValid
	case !now.Before(m.NotAfter):
		m.Status = StatusExpired
	case m.NotAfter.Sub(now) <= s.opts.warnBefore:
		m.Status = StatusExpiring
	default:
		m.Status = StatusValid
	}
	return m
}

// validID reports a valid certificate id.
func validID(id string) bool {
	if id == "" || len(id) > maxIDLength || id == "." || id == ".." {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		alnum := c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
		punct := i > 0 && (c == '-' || c == '_' || c == '.')
		if !alnum && !punct {
			return false
		}
	}
	return true
}

// idFromDomain derives a certificate id from a DNS name.
func idFromDomain(d string) string {
	d = strings.TrimPrefix(strings.ToLower(d), "*.")
	var b strings.Builder
	for i := 0; i < len(d) && b.Len() < maxIDLength; i++ {
		c := d[i]
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.' {
			b.WriteByte(c)
		} else {
			b.WriteByte('-')
		}
	}
	id := strings.Trim(b.String(), "-._")
	if id == "" {
		id = "cert"
	}
	return id
}

// describe validates a chain and its key and describes the leaf.
func describe(chainPEM, keyPEM string, now time.Time) (Certificate, error) {
	if len(chainPEM) > maxChainPEM || len(keyPEM) > maxChainPEM {
		return Certificate{}, fmt.Errorf("%w: the PEM is too large", ErrInvalidCertificate)
	}
	pair, err := tls.X509KeyPair([]byte(chainPEM), []byte(keyPEM))
	if err != nil {
		return Certificate{}, fmt.Errorf("%w: %v", ErrInvalidCertificate, err)
	}
	chain := make([]*x509.Certificate, 0, len(pair.Certificate))
	for i, der := range pair.Certificate {
		c, err := x509.ParseCertificate(der)
		if err != nil {
			return Certificate{}, fmt.Errorf("%w: certificate %d: %v", ErrInvalidCertificate, i+1, err)
		}
		chain = append(chain, c)
	}
	// Each certificate is signed by the next one: a chain in the wrong
	// order, or with a missing intermediate in the middle, is refused.
	for i := 0; i+1 < len(chain); i++ {
		if err := chain[i].CheckSignatureFrom(chain[i+1]); err != nil {
			return Certificate{}, fmt.Errorf("%w: certificate %d is not signed by certificate %d: %v", ErrInvalidCertificate, i+1, i+2, err)
		}
	}
	leaf := chain[0]
	if len(leaf.DNSNames) == 0 {
		return Certificate{}, fmt.Errorf("%w: the certificate has no DNS name (subjectAltName)", ErrInvalidCertificate)
	}
	if !now.Before(leaf.NotAfter) {
		return Certificate{}, fmt.Errorf("%w: the certificate expired on %s", ErrInvalidCertificate, leaf.NotAfter.UTC().Format(time.RFC3339))
	}
	domains := make([]string, 0, len(leaf.DNSNames))
	for _, d := range leaf.DNSNames {
		domains = append(domains, strings.ToLower(d))
	}
	sum := sha256.Sum256(leaf.Raw)
	return Certificate{
		Domains: domains, Subject: leaf.Subject.String(), Issuer: leaf.Issuer.String(), Serial: leaf.SerialNumber.Text(16),
		NotBefore: leaf.NotBefore.UTC(), NotAfter: leaf.NotAfter.UTC(), Fingerprint: hex.EncodeToString(sum[:]),
	}, nil
}

// normalizeKeyPEM keeps the private key blocks of a PEM.
func normalizeKeyPEM(keyPEM string) string {
	var out []byte
	rest := []byte(keyPEM)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if strings.HasSuffix(block.Type, "PRIVATE KEY") {
			out = append(out, pem.EncodeToMemory(block)...)
		}
	}
	return string(out)
}

// Upload implements Service.
func (s *service) Upload(ctx context.Context, in UploadInput) (Certificate, error) {
	meta, err := describe(in.ChainPEM, in.KeyPEM, s.deps.Clock.Now())
	if err != nil {
		return Certificate{}, err
	}
	if err := s.checkReserved(meta.Domains); err != nil {
		return Certificate{}, err
	}
	id := strings.TrimSpace(in.ID)
	if id == "" {
		id = idFromDomain(meta.Domains[0])
	}
	if !validID(id) {
		return Certificate{}, fmt.Errorf("%w: id %q", ErrInvalidCertificate, id)
	}
	if err := s.deps.Locks.Lock(ctx, configLock, s.opts.lockTimeout); err != nil {
		return Certificate{}, fmt.Errorf("certs: lock: %w", err)
	}
	defer func() { _ = s.deps.Locks.Unlock(context.WithoutCancel(ctx), configLock) }()
	now := s.deps.Clock.Now().UTC()
	meta.ID, meta.Source, meta.CreatedAt, meta.UpdatedAt = id, SourceUpload, now, now
	if cur, err := s.Get(ctx, id); err == nil {
		meta.CreatedAt = cur.CreatedAt
	}
	out, err := s.put(ctx, meta, bundle{Chain: in.ChainPEM, Key: normalizeKeyPEM(in.KeyPEM)})
	if err != nil {
		return Certificate{}, err
	}
	s.deps.Logger.InfoContext(ctx, "certs: certificate uploaded", "id", id, "domains", meta.Domains, "notAfter", meta.NotAfter)
	return out, nil
}

// checkReserved refuses the names of the deployment: a certificate for
// the S3 or console host, or for the virtual hosts of the buckets, would
// replace the certificate of the server (or intercept its clients).
func (s *service) checkReserved(names []string) error {
	for _, n := range names {
		for _, h := range s.opts.mainHosts {
			if covers(n, h) {
				return fmt.Errorf("%w: %s covers the host %s of the deployment", ErrInvalidCertificate, n, h)
			}
		}
		for _, d := range s.opts.domains {
			if covers(n, d) || n == "*."+d {
				return fmt.Errorf("%w: %s covers the domain %s of the deployment", ErrInvalidCertificate, n, d)
			}
		}
	}
	return nil
}

// covers reports a certificate name (exact, or "*.parent" for one label)
// that matches host.
func covers(name, host string) bool {
	if name == host {
		return true
	}
	parent, ok := strings.CutPrefix(name, "*.")
	if !ok {
		return false
	}
	_, hostParent, ok := strings.Cut(host, ".")
	return ok && hostParent == parent
}

// Get implements Service: it reads the store.
func (s *service) Get(ctx context.Context, id string) (Certificate, error) {
	if !validID(id) {
		return Certificate{}, fmt.Errorf("%w: %q", ErrCertificateNotFound, id)
	}
	data, err := s.deps.Store.Get(ctx, certsDir+"/"+id+docSuffix)
	if errors.Is(err, sysstore.ErrNotFound) {
		return Certificate{}, fmt.Errorf("%w: %s", ErrCertificateNotFound, id)
	}
	if err != nil {
		return Certificate{}, fmt.Errorf("certs: get %s: %w", id, err)
	}
	var doc storedCert
	if err := json.Unmarshal(data, &doc); err != nil {
		return Certificate{}, fmt.Errorf("certs: decode %s: %w", id, err)
	}
	return s.withStatus(doc.Meta), nil
}

// List implements Service.
func (s *service) List(ctx context.Context) ([]Certificate, error) {
	docs, err := s.deps.Store.List(ctx, certsDir)
	if err != nil {
		return nil, fmt.Errorf("certs: list: %w", err)
	}
	out := make([]Certificate, 0, len(docs))
	for _, data := range docs {
		var doc storedCert
		if err := json.Unmarshal(data, &doc); err != nil || !validID(doc.Meta.ID) {
			continue
		}
		out = append(out, s.withStatus(doc.Meta))
	}
	slices.SortFunc(out, func(a, b Certificate) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

// Delete implements Service.
func (s *service) Delete(ctx context.Context, id string) error {
	if _, err := s.Get(ctx, id); err != nil {
		return err
	}
	if err := s.deps.Store.Delete(ctx, certsDir+"/"+id+docSuffix); err != nil {
		return fmt.Errorf("certs: delete %s: %w", id, err)
	}
	if err := s.Reload(ctx); err != nil {
		s.deps.Logger.WarnContext(ctx, "certs: reload after a delete failed", "err", err)
	}
	s.broadcast(ctx, id)
	return nil
}

// --- handshakes ---------------------------------------------------------------------

// GetCertificate implements Service.
func (s *service) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	name := strings.TrimSuffix(strings.ToLower(hello.ServerName), ".")
	if slices.Contains(hello.SupportedProtos, acme.ALPNProto) {
		return s.alpnCertificate(hello.Context(), name)
	}
	if name == "" {
		return nil, nil
	}
	ix := s.certs.Load()
	now := s.deps.Clock.Now()
	if s.deps.Hosts != nil {
		ctx := hello.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		// Only the certificate of the distribution serving the name:
		// never another certificate that happens to carry it.
		id, ok := s.deps.Hosts.CertificateFor(ctx, name)
		if !ok {
			return nil, nil
		}
		l := ix.byID[id]
		if l == nil || !unexpired(l, now) || !slices.ContainsFunc(l.meta.Domains, func(d string) bool { return covers(d, name) }) {
			return nil, nil
		}
		return l.cert, nil
	}
	if c := best(ix.exact[name], now); c != nil {
		return c, nil
	}
	if _, parent, ok := strings.Cut(name, "."); ok {
		if c := best(ix.wild[parent], now); c != nil {
			return c, nil
		}
	}
	return nil, nil
}

// best returns the certificate valid now that expires last, or the
// unexpired one expiring last when none is valid yet (a certificate just
// issued, seen by a node whose clock is a little behind); nil when every
// one expired: the listener falls back to its own certificate rather
// than presenting an expired one.
func best(list []*loaded, now time.Time) *tls.Certificate {
	var pick *loaded
	pickValid := false
	for _, l := range list {
		if !unexpired(l, now) {
			continue
		}
		valid := !now.Before(l.meta.NotBefore)
		switch {
		case pick == nil, valid && !pickValid, valid == pickValid && l.meta.NotAfter.After(pick.meta.NotAfter):
			pick, pickValid = l, valid
		}
	}
	if pick == nil {
		return nil
	}
	return pick.cert
}

// unexpired reports a certificate that has not expired at now.
func unexpired(l *loaded, now time.Time) bool { return now.Before(l.meta.NotAfter) }

// alpnCertificate returns the TLS-ALPN-01 certificate of a domain.
func (s *service) alpnCertificate(ctx context.Context, name string) (*tls.Certificate, error) {
	if c, ok := s.alpn.Load(name); ok {
		return c.(*tls.Certificate), nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	data, err := s.deps.Store.Get(ctx, alpnDir+"/"+name+docSuffix)
	if err != nil {
		return nil, fmt.Errorf("certs: no TLS-ALPN-01 challenge for %q: %w", name, ErrChallengeNotFound)
	}
	plain, err := s.deps.Sealer.Open(ctx, data, []byte(sealedPrefix+"alpn:"+name))
	if err != nil {
		return nil, err
	}
	var b bundle
	if err := json.Unmarshal(plain, &b); err != nil {
		return nil, err
	}
	cert, err := tls.X509KeyPair([]byte(b.Chain), []byte(b.Key))
	if err != nil {
		return nil, err
	}
	return &cert, nil
}

// validToken reports an ACME token (base64url).
func validToken(t string) bool {
	if t == "" || len(t) > 256 {
		return false
	}
	for i := 0; i < len(t); i++ {
		c := t[i]
		alnum := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		if !alnum && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

// HTTPChallenge implements Service.
func (s *service) HTTPChallenge(ctx context.Context, token string) (string, error) {
	if !validToken(token) {
		return "", ErrChallengeNotFound
	}
	if v, ok := s.http01.Load(token); ok {
		return v.(string), nil
	}
	data, err := s.deps.Store.Get(ctx, http01Dir+"/"+token+docSuffix)
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrChallengeNotFound, token)
	}
	var v struct {
		KeyAuth string `json:"keyAuth"`
	}
	if err := json.Unmarshal(data, &v); err != nil || v.KeyAuth == "" {
		return "", fmt.Errorf("%w: %s", ErrChallengeNotFound, token)
	}
	return v.KeyAuth, nil
}
