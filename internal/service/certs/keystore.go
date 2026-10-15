package certs

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/r3n3r0/nexo/internal/clock"
	"github.com/r3n3r0/nexo/internal/domain"
	"github.com/r3n3r0/nexo/internal/service/metrics"
)

// Files of the certificates directory.
const (
	// PublicCertFile and PrivateKeyFile are the certificate chain and the
	// private key of a key pair, in the directory and in each of its
	// sub-directories.
	PublicCertFile = "public.crt"
	PrivateKeyFile = "private.key"
	// CAsDir holds the CA certificates trusted for the peers.
	CAsDir = "CAs"
	// maxKeystoreFile bounds the size of a file of the directory.
	maxKeystoreFile = 1 << 20
	// maxSymlinkHops bounds the symbolic links followed to watch the
	// directories of their targets.
	maxSymlinkHops = 8
)

// Defaults of the keystore.
const (
	// DefaultKeystoreDebounce groups the file events of a rotation (the
	// certificate and the key written one after the other) into one
	// reload.
	DefaultKeystoreDebounce = 500 * time.Millisecond
	// DefaultKeystorePoll is the period of the safety-net scan of the
	// files (a change the watcher missed: a file system without
	// notifications, a symbolic link retargeted outside the directory).
	DefaultKeystorePoll = time.Minute
)

// KeystoreOption configures the keystore.
type KeystoreOption func(*keystoreOptions)

type keystoreOptions struct {
	debounce time.Duration
	poll     time.Duration
	// systemRoots adds the system roots to RootCAs.
	systemRoots bool
	// noWatch disables the file notifications (tests of the poller).
	noWatch bool
}

// WithKeystoreDebounce sets the delay between the last file event and the
// reload (default DefaultKeystoreDebounce).
func WithKeystoreDebounce(d time.Duration) KeystoreOption {
	return func(o *keystoreOptions) {
		if d > 0 {
			o.debounce = d
		}
	}
}

// WithKeystorePoll sets the period of the safety-net scan of the files
// (default DefaultKeystorePoll); a negative period disables it.
func WithKeystorePoll(d time.Duration) KeystoreOption {
	return func(o *keystoreOptions) {
		if d != 0 {
			o.poll = d
		}
	}
}

// WithoutSystemRoots leaves the system roots out of RootCAs: only the
// certificates of the directory are trusted (tests, closed deployments).
func WithoutSystemRoots() KeystoreOption {
	return func(o *keystoreOptions) { o.systemRoots = false }
}

// keyEntry is a loaded key pair.
type keyEntry struct {
	info KeyPair
	cert *tls.Certificate
}

// material is an immutable snapshot of the directory.
type material struct {
	def   *keyEntry
	named []*keyEntry
	// exact and wild index the named certificates by DNS name ("*.x.com"
	// under "x.com").
	exact map[string][]*keyEntry
	wild  map[string][]*keyEntry
	// clientCAs verify the peers as clients, rootCAs as servers.
	clientCAs *x509.CertPool
	rootCAs   *x509.CertPool
	// signature identifies the state of the files read (paths, sizes,
	// modification times): the poller reloads when it changes.
	signature string
	// watch are the directories whose events concern the material.
	watch []string
}

// keystore implements Keystore.
type keystore struct {
	deps KeystoreDeps
	dir  string
	opts keystoreOptions

	cur      atomic.Pointer[material]
	reloadMu sync.Mutex

	expiry  metrics.Gauge
	reloads metrics.Counter

	lifeMu sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

var _ Keystore = (*keystore)(nil)

// NewKeystore loads the certificates directory dir (docs/07 §7.3). The
// default key pair is required; an invalid file fails the load
// (domain.ErrInvalidArgument). The watcher starts with Start.
func NewKeystore(deps KeystoreDeps, dir string, opts ...KeystoreOption) (Keystore, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("%w: certs: the certificates directory is required", domain.ErrInvalidArgument)
	}
	if deps.Clock == nil {
		deps.Clock = clock.NewReal()
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	o := keystoreOptions{debounce: DefaultKeystoreDebounce, poll: DefaultKeystorePoll, systemRoots: true}
	for _, opt := range opts {
		opt(&o)
	}
	k := &keystore{deps: deps, dir: filepath.Clean(dir), opts: o}
	if deps.Metrics != nil {
		ctx := context.Background()
		var err error
		if k.expiry, err = deps.Metrics.Gauge(ctx, metrics.Definition{
			Subsystem: "tls", Name: "server_certificate_expiry_seconds",
			Help:   "Seconds until the expiration of the certificates of the certificates directory.",
			Labels: []string{"certificate"},
		}); err != nil {
			return nil, fmt.Errorf("certs: keystore metrics: %w", err)
		}
		if k.reloads, err = deps.Metrics.Counter(ctx, metrics.Definition{
			Subsystem: "tls", Name: "server_certificate_reloads_total",
			Help:   "Reloads of the certificates directory, by result.",
			Labels: []string{"result"},
		}); err != nil {
			return nil, fmt.Errorf("certs: keystore metrics: %w", err)
		}
	}
	m, err := k.load()
	if err != nil {
		return nil, err
	}
	k.install(context.Background(), m)
	return k, nil
}

// --- TLS callbacks and accessors ------------------------------------------------------

// GetCertificate implements Keystore.
func (k *keystore) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	m := k.cur.Load()
	name := strings.TrimSuffix(strings.ToLower(hello.ServerName), ".")
	if name != "" && len(m.named) > 0 {
		now := time.Now()
		if c := bestEntry(m.exact[name], now); c != nil {
			return c, nil
		}
		if _, parent, ok := strings.Cut(name, "."); ok {
			if c := bestEntry(m.wild[parent], now); c != nil {
				return c, nil
			}
		}
	}
	return m.def.cert, nil
}

// bestEntry returns the certificate of list valid now that expires last,
// nil when none is valid.
func bestEntry(list []*keyEntry, now time.Time) *tls.Certificate {
	var pick *keyEntry
	for _, e := range list {
		if now.Before(e.info.NotBefore) || !now.Before(e.info.NotAfter) {
			continue
		}
		if pick == nil || e.info.NotAfter.After(pick.info.NotAfter) {
			pick = e
		}
	}
	if pick == nil {
		return nil
	}
	return pick.cert
}

// GetClientCertificate implements Keystore.
func (k *keystore) GetClientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	return k.cur.Load().def.cert, nil
}

// ClientCAs implements Keystore.
func (k *keystore) ClientCAs() *x509.CertPool { return k.cur.Load().clientCAs }

// RootCAs implements Keystore.
func (k *keystore) RootCAs() *x509.CertPool { return k.cur.Load().rootCAs }

// KeyPairs implements Keystore.
func (k *keystore) KeyPairs(context.Context) []KeyPair {
	m := k.cur.Load()
	out := make([]KeyPair, 0, 1+len(m.named))
	out = append(out, m.def.info)
	for _, e := range m.named {
		out = append(out, e.info)
	}
	return out
}

// --- loading ------------------------------------------------------------------------------

// Reload implements Keystore.
func (k *keystore) Reload(ctx context.Context) error {
	k.reloadMu.Lock()
	defer k.reloadMu.Unlock()
	m, err := k.load()
	if err != nil {
		k.count(ctx, "error")
		k.deps.Logger.ErrorContext(ctx, "tls: reload of the certificates directory failed, the current certificates are kept",
			"dir", k.dir, "err", err)
		return err
	}
	k.install(ctx, m)
	k.count(ctx, "ok")
	k.deps.Logger.InfoContext(ctx, "tls: certificates reloaded", "dir", k.dir, "certificates", 1+len(m.named))
	return nil
}

// count records a reload.
func (k *keystore) count(ctx context.Context, result string) {
	if k.reloads != nil {
		k.reloads.Add(ctx, 1, result)
	}
}

// install publishes m and its metrics, and warns about the certificates
// that expired or expire soon.
func (k *keystore) install(ctx context.Context, m *material) {
	k.cur.Store(m)
	now := k.deps.Clock.Now()
	for _, e := range append([]*keyEntry{m.def}, m.named...) {
		label := e.info.Name
		if label == "" {
			label = "default"
		}
		if k.expiry != nil {
			k.expiry.Set(ctx, e.info.NotAfter.Sub(now).Seconds(), label)
		}
		switch left := e.info.NotAfter.Sub(now); {
		case left <= 0:
			k.deps.Logger.ErrorContext(ctx, "tls: certificate expired", "certificate", label, "not_after", e.info.NotAfter)
		case left < DefaultExpiryWarning:
			k.deps.Logger.WarnContext(ctx, "tls: certificate expiring soon", "certificate", label, "not_after", e.info.NotAfter)
		}
	}
}

// load reads the directory into a new material.
func (k *keystore) load() (*material, error) {
	sig := &signature{}
	m := &material{exact: map[string][]*keyEntry{}, wild: map[string][]*keyEntry{}}
	watch := map[string]bool{k.dir: true}
	def, err := k.loadPair(k.dir, "", sig, watch)
	if err != nil {
		return nil, err
	}
	m.def = def

	entries, err := os.ReadDir(k.dir)
	if err != nil {
		return nil, fmt.Errorf("certs: read %s: %w", k.dir, err)
	}
	for _, e := range entries {
		name := e.Name()
		if name == CAsDir || strings.HasPrefix(name, ".") {
			continue
		}
		sub := filepath.Join(k.dir, name)
		// Follow a symbolic link to a directory (os.Stat), as mounted
		// secrets and certbot layouts use them.
		info, err := os.Stat(sub)
		if err != nil || !info.IsDir() {
			continue
		}
		watch[sub] = true
		if !exists(filepath.Join(sub, PublicCertFile)) && !exists(filepath.Join(sub, PrivateKeyFile)) {
			continue
		}
		entry, err := k.loadPair(sub, name, sig, watch)
		if err != nil {
			return nil, err
		}
		m.named = append(m.named, entry)
		for _, d := range entry.info.Domains {
			if rest, ok := strings.CutPrefix(d, "*."); ok {
				m.wild[rest] = append(m.wild[rest], entry)
				continue
			}
			m.exact[d] = append(m.exact[d], entry)
		}
	}
	sort.Slice(m.named, func(i, j int) bool { return m.named[i].info.Name < m.named[j].info.Name })

	trusted, err := k.loadCAs(sig, watch)
	if err != nil {
		return nil, err
	}
	trusted = append(trusted, m.def.cert.Leaf)
	for _, der := range m.def.cert.Certificate[1:] {
		if c, err := x509.ParseCertificate(der); err == nil {
			trusted = append(trusted, c)
		}
	}
	m.clientCAs = x509.NewCertPool()
	m.rootCAs = x509.NewCertPool()
	if k.opts.systemRoots {
		if sys, err := x509.SystemCertPool(); err == nil && sys != nil {
			m.rootCAs = sys
		}
	}
	for _, c := range trusted {
		m.clientCAs.AddCert(c)
		m.rootCAs.AddCert(c)
	}
	m.signature = sig.sum()
	for d := range watch {
		m.watch = append(m.watch, d)
	}
	sort.Strings(m.watch)
	return m, nil
}

// loadPair loads the key pair of dir.
func (k *keystore) loadPair(dir, name string, sig *signature, watch map[string]bool) (*keyEntry, error) {
	certPath, keyPath := filepath.Join(dir, PublicCertFile), filepath.Join(dir, PrivateKeyFile)
	label := name
	if label == "" {
		label = "default"
	}
	certPEM, err := readBounded(certPath, sig, watch)
	if err != nil {
		return nil, fmt.Errorf("%w: certs: certificate %s: %w", domain.ErrInvalidArgument, label, err)
	}
	keyPEM, err := readBounded(keyPath, sig, watch)
	if err != nil {
		return nil, fmt.Errorf("%w: certs: private key %s: %w", domain.ErrInvalidArgument, label, err)
	}
	k.checkKeyMode(keyPath)
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("%w: certs: key pair %s (%s): %w", domain.ErrInvalidArgument, label, dir, err)
	}
	leaf := pair.Leaf
	if leaf == nil {
		if leaf, err = x509.ParseCertificate(pair.Certificate[0]); err != nil {
			return nil, fmt.Errorf("%w: certs: key pair %s: %w", domain.ErrInvalidArgument, label, err)
		}
		pair.Leaf = leaf
	}
	sum := sha256.Sum256(leaf.Raw)
	info := KeyPair{
		Name:        name,
		Domains:     normalizeNames(leaf.DNSNames),
		Subject:     leaf.Subject.String(),
		Issuer:      leaf.Issuer.String(),
		NotBefore:   leaf.NotBefore.UTC(),
		NotAfter:    leaf.NotAfter.UTC(),
		Fingerprint: hex.EncodeToString(sum[:]),
	}
	for _, ip := range leaf.IPAddresses {
		info.IPs = append(info.IPs, ip.String())
	}
	if name != "" && len(info.Domains) == 0 {
		return nil, fmt.Errorf("%w: certs: certificate %s names no DNS name to select it by", domain.ErrInvalidArgument, label)
	}
	return &keyEntry{info: info, cert: &pair}, nil
}

// checkKeyMode warns about a private key readable by other users.
func (k *keystore) checkKeyMode(path string) {
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(path)
	if err == nil && info.Mode().Perm()&0o077 != 0 {
		k.deps.Logger.Warn("tls: the private key is readable by other users: restrict it to the server (chmod 600)",
			"file", path, "mode", info.Mode().Perm().String())
	}
}

// loadCAs reads the certificates of the CAs directory.
func (k *keystore) loadCAs(sig *signature, watch map[string]bool) ([]*x509.Certificate, error) {
	dir := filepath.Join(k.dir, CAsDir)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("certs: read %s: %w", dir, err)
	}
	watch[dir] = true
	var out []*x509.Certificate
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			continue
		}
		data, err := readBounded(path, sig, watch)
		if err != nil {
			return nil, fmt.Errorf("%w: certs: CA %s: %w", domain.ErrInvalidArgument, e.Name(), err)
		}
		certs := parseCertificates(data)
		if len(certs) == 0 {
			k.deps.Logger.Warn("tls: no certificate in a file of the CAs directory, ignored", "file", path)
			continue
		}
		out = append(out, certs...)
	}
	return out, nil
}

// parseCertificates returns the certificates of the PEM blocks of data.
func parseCertificates(data []byte) []*x509.Certificate {
	var out []*x509.Certificate
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return out
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		if c, err := x509.ParseCertificate(block.Bytes); err == nil {
			out = append(out, c)
		}
	}
}

// readBounded reads a file of at most maxKeystoreFile bytes, records it in
// the signature and the directories of its symbolic links in watch.
func readBounded(path string, sig *signature, watch map[string]bool) ([]byte, error) {
	f, err := os.Open(path) // #nosec G304 -- the files of the certificates directory the operator configured
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxKeystoreFile+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxKeystoreFile {
		return nil, fmt.Errorf("%s is larger than %d bytes", filepath.Base(path), maxKeystoreFile)
	}
	sig.add(path, info)
	for _, d := range linkDirs(path) {
		watch[d] = true
	}
	return data, nil
}

// linkDirs returns the directories of the targets of the symbolic links
// leading from path to its file (certbot's live/ and archive/, the ..data
// link of a mounted secret).
func linkDirs(path string) []string {
	var out []string
	cur := path
	for range maxSymlinkHops {
		info, err := os.Lstat(cur)
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			return out
		}
		target, err := os.Readlink(cur)
		if err != nil {
			return out
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(cur), target)
		}
		out = append(out, filepath.Dir(target))
		cur = target
	}
	return out
}

// exists reports whether path exists (following symbolic links).
func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// signature accumulates the identity of the files read.
type signature struct{ parts []string }

func (s *signature) add(path string, info os.FileInfo) {
	s.parts = append(s.parts, fmt.Sprintf("%s|%d|%d", path, info.Size(), info.ModTime().UnixNano()))
}

func (s *signature) sum() string {
	sorted := slices.Clone(s.parts)
	sort.Strings(sorted)
	h := sha256.Sum256([]byte(strings.Join(sorted, "\n")))
	return hex.EncodeToString(h[:])
}

// scan computes the signature of the files of the current material
// without parsing them: the poller reloads when it differs.
func (k *keystore) scan() string {
	sig := &signature{}
	add := func(path string) {
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			sig.add(path, info)
		}
	}
	add(filepath.Join(k.dir, PublicCertFile))
	add(filepath.Join(k.dir, PrivateKeyFile))
	if entries, err := os.ReadDir(k.dir); err == nil {
		for _, e := range entries {
			name := e.Name()
			if strings.HasPrefix(name, ".") {
				continue
			}
			sub := filepath.Join(k.dir, name)
			if name == CAsDir {
				if cas, err := os.ReadDir(sub); err == nil {
					for _, c := range cas {
						if !strings.HasPrefix(c.Name(), ".") {
							add(filepath.Join(sub, c.Name()))
						}
					}
				}
				continue
			}
			add(filepath.Join(sub, PublicCertFile))
			add(filepath.Join(sub, PrivateKeyFile))
		}
	}
	return sig.sum()
}

// --- watcher ------------------------------------------------------------------------------

// Start implements lifecycle.Runner: it watches the directory (and polls
// it as a safety net) until Stop.
func (k *keystore) Start(ctx context.Context) error {
	k.lifeMu.Lock()
	defer k.lifeMu.Unlock()
	if k.cancel != nil {
		return nil
	}
	var watcher *fsnotify.Watcher
	if !k.opts.noWatch {
		var err error
		if watcher, err = fsnotify.NewWatcher(); err != nil {
			// Without notifications (inotify limits...) the poller
			// alone notices the changes.
			k.deps.Logger.WarnContext(ctx, "tls: cannot watch the certificates directory, polling it", "dir", k.dir, "err", err)
			watcher = nil
		}
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	k.cancel = cancel
	k.done = make(chan struct{})
	go k.run(runCtx, watcher, k.done)
	return nil
}

// Stop implements lifecycle.Runner.
func (k *keystore) Stop(ctx context.Context) error {
	k.lifeMu.Lock()
	cancel, done := k.cancel, k.done
	k.cancel, k.done = nil, nil
	k.lifeMu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// run reloads the material after the file events (debounced) and when the
// poller sees the files change.
func (k *keystore) run(ctx context.Context, watcher *fsnotify.Watcher, done chan<- struct{}) {
	defer close(done)
	var events <-chan fsnotify.Event
	var errs <-chan error
	watched := map[string]bool{}
	if watcher != nil {
		defer func() { _ = watcher.Close() }()
		events, errs = watcher.Events, watcher.Errors
		k.syncWatches(ctx, watcher, watched)
	}
	var poll <-chan time.Time
	if k.opts.poll > 0 {
		ticker := time.NewTicker(k.opts.poll)
		defer ticker.Stop()
		poll = ticker.C
	}
	debounce := time.NewTimer(time.Hour)
	debounce.Stop()
	defer debounce.Stop()
	reload := func() {
		_ = k.Reload(ctx)
		if watcher != nil {
			k.syncWatches(ctx, watcher, watched)
		}
	}
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if ev.Op == fsnotify.Chmod {
				continue
			}
			debounce.Reset(k.opts.debounce)
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			k.deps.Logger.WarnContext(ctx, "tls: watcher of the certificates directory", "err", err)
		case <-debounce.C:
			reload()
		case <-poll:
			if k.scan() != k.cur.Load().signature {
				reload()
			}
		}
	}
}

// syncWatches watches the directories of the current material and stops
// watching the others.
func (k *keystore) syncWatches(ctx context.Context, watcher *fsnotify.Watcher, watched map[string]bool) {
	want := map[string]bool{}
	for _, d := range k.cur.Load().watch {
		want[d] = true
	}
	for d := range watched {
		if !want[d] {
			_ = watcher.Remove(d)
			delete(watched, d)
		}
	}
	for d := range want {
		if watched[d] {
			continue
		}
		if err := watcher.Add(d); err != nil {
			k.deps.Logger.WarnContext(ctx, "tls: cannot watch a directory of the certificates", "dir", d, "err", err)
			continue
		}
		watched[d] = true
	}
}
