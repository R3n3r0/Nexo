package certs

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme"

	"github.com/r3n3r0/nexo/internal/service/sysstore"
)

// orderTimeout bounds an ACME order (authorizations, validation,
// finalization).
const orderTimeout = 5 * time.Minute

// account is the sealed ACME account of the deployment.
type account struct {
	KeyPEM    string `json:"key"`
	Email     string `json:"email,omitempty"`
	Directory string `json:"directory"`
}

// Reconcile implements Service.
func (s *service) Reconcile(ctx context.Context) error {
	if !s.deps.Leader.IsLeader() {
		return nil
	}
	return s.reconcile(ctx)
}

// reconcile obtains the missing ACME certificates, renews the expiring
// ones, removes the ones no distribution wants and warns about the
// certificates close to their expiration.
func (s *service) reconcile(ctx context.Context) error {
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()
	now := s.deps.Clock.Now()
	s.warnExpiring(ctx, now)
	if s.deps.Domains == nil {
		return nil
	}
	wanted, err := s.deps.Domains.ACMEDomains(ctx)
	if err != nil {
		return fmt.Errorf("certs: managed domains: %w", err)
	}
	ix := s.certs.Load()
	keep := map[string]bool{}
	var errs []error
	for _, m := range wanted {
		domains := acmeDomains(m.Domains)
		if len(domains) < len(m.Domains) {
			s.deps.Logger.WarnContext(ctx, "certs: wildcard and IP names need DNS-01, not supported: left out of the ACME certificate",
				"id", m.ID, "domains", m.Domains)
		}
		if len(domains) == 0 || !validID(m.ID) {
			continue
		}
		keep[m.ID] = true
		cur := ix.byID[m.ID]
		if cur != nil && slices.Equal(sortedCopy(cur.meta.Domains), domains) && now.Before(cur.meta.NotAfter.Add(-s.opts.renewBefore)) {
			continue
		}
		if last, failed := s.failures[m.ID]; failed && now.Sub(last) < s.opts.retryInterval {
			continue
		}
		reason := "new"
		if cur != nil {
			reason = "renewal"
		}
		s.deps.Logger.InfoContext(ctx, "certs: ordering an ACME certificate", "id", m.ID, "domains", domains, "reason", reason)
		if err := s.obtain(ctx, m.ID, domains); err != nil {
			s.failures[m.ID] = now
			s.recordFailure(ctx, cur, err)
			errs = append(errs, fmt.Errorf("certificate %s: %w", m.ID, err))
			continue
		}
		delete(s.failures, m.ID)
	}
	for id, l := range ix.byID {
		if l.meta.Source == SourceACME && !keep[id] {
			s.deps.Logger.InfoContext(ctx, "certs: removing an ACME certificate no distribution uses", "id", id)
			if err := s.Delete(ctx, id); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// warnExpiring logs, once a day, the certificates within the warning
// window of their expiration.
func (s *service) warnExpiring(ctx context.Context, now time.Time) {
	for id, l := range s.certs.Load().byID {
		left := l.meta.NotAfter.Sub(now)
		if left > s.opts.warnBefore {
			continue
		}
		if last, ok := s.warned[id]; ok && now.Sub(last) < 24*time.Hour {
			continue
		}
		s.warned[id] = now
		s.deps.Logger.WarnContext(ctx, "certs: certificate close to its expiration", "id", id, "domains", l.meta.Domains,
			"notAfter", l.meta.NotAfter, "daysLeft", int(left.Hours()/24), "source", string(l.meta.Source))
	}
}

// recordFailure keeps the error of a failed renewal on the certificate.
func (s *service) recordFailure(ctx context.Context, cur *loaded, err error) {
	s.deps.Logger.ErrorContext(ctx, "certs: ACME order failed", "err", err)
	if cur == nil {
		return
	}
	data, gerr := s.deps.Store.Get(ctx, certsDir+"/"+cur.meta.ID+docSuffix)
	if gerr != nil {
		return
	}
	var doc storedCert
	if json.Unmarshal(data, &doc) != nil {
		return
	}
	doc.Meta.LastError = err.Error()
	if out, merr := json.Marshal(doc); merr == nil {
		_ = s.deps.Store.Put(ctx, certsDir+"/"+cur.meta.ID+docSuffix, out)
	}
}

// acmeDomains returns the names an ACME order can validate with HTTP-01
// or TLS-ALPN-01, sorted: no wildcard, no IP.
func acmeDomains(in []string) []string {
	var out []string
	for _, d := range in {
		d = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(d)), ".")
		if d == "" || strings.Contains(d, "*") || net.ParseIP(d) != nil || slices.Contains(out, d) {
			continue
		}
		out = append(out, d)
	}
	slices.Sort(out)
	return out
}

func sortedCopy(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}

// acmeClient returns the client of the ACME account, registering the
// account on first use (its key sealed in the store).
func (s *service) acmeClient(ctx context.Context) (*acme.Client, error) {
	s.acctMu.Lock()
	defer s.acctMu.Unlock()
	if s.client != nil {
		return s.client, nil
	}
	if err := s.deps.Locks.Lock(ctx, configLock, s.opts.lockTimeout); err != nil {
		return nil, fmt.Errorf("certs: lock: %w", err)
	}
	defer func() { _ = s.deps.Locks.Unlock(context.WithoutCancel(ctx), configLock) }()
	acct, err := s.loadAccount(ctx)
	if err != nil {
		return nil, err
	}
	if acct == nil || acct.Directory != s.opts.directory {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("certs: account key: %w", err)
		}
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return nil, err
		}
		acct = &account{KeyPEM: string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), Email: s.opts.email, Directory: s.opts.directory}
		if err := s.saveAccount(ctx, acct); err != nil {
			return nil, err
		}
	}
	block, _ := pem.Decode([]byte(acct.KeyPEM))
	if block == nil {
		return nil, fmt.Errorf("certs: the ACME account key is not PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("certs: ACME account key: %w", err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("certs: the ACME account key is not ECDSA")
	}
	httpClient := *s.opts.httpClient
	base := httpClient.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	s.finalizers = &finalizeLocations{base: base}
	httpClient.Transport = s.finalizers
	client := &acme.Client{Key: key, DirectoryURL: s.opts.directory, HTTPClient: &httpClient, UserAgent: "nexo"}
	reg := &acme.Account{}
	if acct.Email != "" {
		reg.Contact = []string{"mailto:" + acct.Email}
	}
	if _, err := client.Register(ctx, reg, acme.AcceptTOS); err != nil && !errors.Is(err, acme.ErrAccountAlreadyExists) {
		return nil, fmt.Errorf("certs: ACME registration: %w", err)
	}
	s.client = client
	return client, nil
}

func (s *service) loadAccount(ctx context.Context) (*account, error) {
	data, err := s.deps.Store.Get(ctx, acmeAccount)
	if errors.Is(err, sysstore.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("certs: ACME account: %w", err)
	}
	plain, err := s.deps.Sealer.Open(ctx, data, []byte(sealedPrefix+"account"))
	if err != nil {
		return nil, err
	}
	var a account
	if err := json.Unmarshal(plain, &a); err != nil {
		return nil, fmt.Errorf("certs: ACME account: %w", err)
	}
	return &a, nil
}

func (s *service) saveAccount(ctx context.Context, a *account) error {
	plain, err := json.Marshal(a)
	if err != nil {
		return err
	}
	sealed, err := s.deps.Sealer.Seal(ctx, plain, []byte(sealedPrefix+"account"))
	if err != nil {
		return err
	}
	return s.deps.Store.Put(ctx, acmeAccount, sealed)
}

// obtain orders a certificate for domains and stores it as id.
func (s *service) obtain(ctx context.Context, id string, domains []string) error {
	ctx, cancel := context.WithTimeout(ctx, orderTimeout)
	defer cancel()
	client, err := s.acmeClient(ctx)
	if err != nil {
		return err
	}
	order, err := client.AuthorizeOrder(ctx, acme.DomainIDs(domains...))
	if err != nil {
		return fmt.Errorf("new order: %w", err)
	}
	for _, zurl := range order.AuthzURLs {
		if err := s.authorize(ctx, client, zurl); err != nil {
			return err
		}
	}
	finalizeURL := order.FinalizeURL
	ready, err := client.WaitOrder(ctx, order.URI)
	if err != nil {
		return fmt.Errorf("order: %w", err)
	}
	if ready.FinalizeURL != "" {
		finalizeURL = ready.FinalizeURL
	}
	s.deps.Logger.DebugContext(ctx, "certs: ACME order ready", "id", id, "status", ready.Status, "finalize", finalizeURL)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: domains[0]}, DNSNames: domains}, key)
	if err != nil {
		return err
	}
	s.finalizers.register(finalizeURL, order.URI)
	defer s.finalizers.unregister(finalizeURL)
	ders, _, err := client.CreateOrderCert(ctx, finalizeURL, csr, true)
	if err != nil {
		return fmt.Errorf("finalize: %w", err)
	}
	var chain []byte
	for _, der := range ders {
		chain = append(chain, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	now := s.deps.Clock.Now()
	meta, err := describe(string(chain), keyPEM, now)
	if err != nil {
		return err
	}
	meta.ID, meta.Source = id, SourceACME
	meta.CreatedAt, meta.UpdatedAt = now.UTC(), now.UTC()
	if cur, err := s.Get(ctx, id); err == nil {
		meta.CreatedAt = cur.CreatedAt
	}
	if _, err := s.put(ctx, meta, bundle{Chain: string(chain), Key: keyPEM}); err != nil {
		return err
	}
	s.deps.Logger.InfoContext(ctx, "certs: ACME certificate obtained", "id", id, "domains", domains, "notAfter", meta.NotAfter)
	return nil
}

// authorize answers the challenge of an authorization and waits for it.
func (s *service) authorize(ctx context.Context, client *acme.Client, zurl string) error {
	z, err := client.GetAuthorization(ctx, zurl)
	if err != nil {
		return fmt.Errorf("authorization: %w", err)
	}
	if z.Status == acme.StatusValid {
		return nil
	}
	var chal *acme.Challenge
	for _, c := range z.Challenges {
		if c.Type == s.opts.challenge {
			chal = c
		}
	}
	if chal == nil {
		return fmt.Errorf("the CA offers no %s challenge for %s", s.opts.challenge, z.Identifier.Value)
	}
	cleanup, err := s.prepare(ctx, client, z.Identifier.Value, chal)
	if err != nil {
		return err
	}
	defer cleanup()
	if _, err := client.Accept(ctx, chal); err != nil {
		return fmt.Errorf("accept %s: %w", chal.Type, err)
	}
	if _, err := client.WaitAuthorization(ctx, z.URI); err != nil {
		return fmt.Errorf("validation of %s: %w", z.Identifier.Value, err)
	}
	return nil
}

// prepare publishes the answer of a challenge to every node (system
// store) and returns the function that removes it.
func (s *service) prepare(ctx context.Context, client *acme.Client, domainName string, chal *acme.Challenge) (func(), error) {
	switch chal.Type {
	case ChallengeHTTP01:
		keyAuth, err := client.HTTP01ChallengeResponse(chal.Token)
		if err != nil {
			return nil, err
		}
		if !validToken(chal.Token) {
			return nil, fmt.Errorf("invalid challenge token")
		}
		data, _ := json.Marshal(struct {
			KeyAuth string `json:"keyAuth"`
		}{keyAuth})
		path := http01Dir + "/" + chal.Token + docSuffix
		if err := s.deps.Store.Put(ctx, path, data); err != nil {
			return nil, fmt.Errorf("publish the HTTP-01 answer: %w", err)
		}
		s.http01.Store(chal.Token, keyAuth)
		return func() {
			s.http01.Delete(chal.Token)
			_ = s.deps.Store.Delete(context.WithoutCancel(ctx), path)
		}, nil
	case ChallengeTLSALPN01:
		cert, err := client.TLSALPN01ChallengeCert(chal.Token, domainName)
		if err != nil {
			return nil, err
		}
		chain, keyPEM, err := encodeCert(cert.Certificate, cert.PrivateKey)
		if err != nil {
			return nil, err
		}
		plain, _ := json.Marshal(bundle{Chain: chain, Key: keyPEM})
		sealed, err := s.deps.Sealer.Seal(ctx, plain, []byte(sealedPrefix+"alpn:"+domainName))
		if err != nil {
			return nil, err
		}
		path := alpnDir + "/" + domainName + docSuffix
		if err := s.deps.Store.Put(ctx, path, sealed); err != nil {
			return nil, fmt.Errorf("publish the TLS-ALPN-01 certificate: %w", err)
		}
		s.alpn.Store(domainName, &cert)
		return func() {
			s.alpn.Delete(domainName)
			_ = s.deps.Store.Delete(context.WithoutCancel(ctx), path)
		}, nil
	}
	return nil, fmt.Errorf("unsupported challenge %s", chal.Type)
}

// encodeCert encodes a chain and its key in PEM.
func encodeCert(ders [][]byte, key any) (string, string, error) {
	var chain []byte
	for _, der := range ders {
		chain = append(chain, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", "", err
	}
	return string(chain), string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})), nil
}

// finalizeLocations is the transport of the ACME client: a CA that answers
// the finalization of an order without its Location header (RFC 8555
// §7.4 does not require it; Pebble omits it) gets the URL of the order
// added, so that the client can poll the order until it is valid.
type finalizeLocations struct {
	base   http.RoundTripper
	orders sync.Map // finalize URL → order URL
}

func (f *finalizeLocations) register(finalizeURL, orderURL string) {
	f.orders.Store(finalizeURL, orderURL)
}

func (f *finalizeLocations) unregister(finalizeURL string) { f.orders.Delete(finalizeURL) }

// RoundTrip implements http.RoundTripper.
func (f *finalizeLocations) RoundTrip(r *http.Request) (*http.Response, error) {
	res, err := f.base.RoundTrip(r)
	if err != nil || r.Method != http.MethodPost || res.Header.Get("Location") != "" {
		return res, err
	}
	if orderURL, ok := f.orders.Load(r.URL.String()); ok {
		res.Header.Set("Location", orderURL.(string))
	}
	return res, nil
}
