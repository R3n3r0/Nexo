// Package dnstest runs an authoritative DNS server in the tests of the ACME
// DNS-01 challenge.
package dnstest

import (
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// Server is an authoritative nameserver of one zone for the tests of
// the ACME DNS-01 challenge: it answers the SOA, NS and TXT queries of the
// zone over UDP and TCP and applies the dynamic updates of RFC 2136,
// signed with a TSIG key when one is configured (WithTSIG). The
// tests point the propagation check (and the validation authority of the
// test CA) at its address.
type Server struct {
	// Addr is the host:port of the server (UDP and TCP).
	Addr string
	// Zone is the zone, fully qualified ("example.test.").
	Zone string

	tsigName, tsigSecret string

	mu      sync.Mutex
	txt     map[string][]string
	updates int
	refused int
}

// Option configures a Server.
type Option func(*Server)

// WithTSIG requires the updates to be signed with the TSIG key name
// (secret in base64).
func WithTSIG(name, secret string) Option {
	return func(s *Server) { s.tsigName, s.tsigSecret = dns.Fqdn(strings.ToLower(name)), secret }
}

// Start starts the nameserver of zone on a free loopback port;
// it stops with the test.
func Start(t testing.TB, zone string, opts ...Option) *Server {
	t.Helper()
	s := &Server{Zone: dns.Fqdn(strings.ToLower(zone)), txt: map[string][]string{}}
	for _, o := range opts {
		o(s)
	}
	var (
		pc  net.PacketConn
		ln  net.Listener
		err error
	)
	for range 20 {
		if pc, err = net.ListenPacket("udp", "127.0.0.1:0"); err != nil {
			t.Fatalf("dns server: %v", err)
		}
		if ln, err = net.Listen("tcp", pc.LocalAddr().String()); err == nil {
			break
		}
		_ = pc.Close()
	}
	if err != nil {
		t.Fatalf("dns server: no free port: %v", err)
	}
	s.Addr = pc.LocalAddr().String()
	var secrets map[string]string
	if s.tsigName != "" {
		secrets = map[string]string{s.tsigName: s.tsigSecret}
	}
	handler := dns.HandlerFunc(s.serve)
	accept := func(dh dns.Header) dns.MsgAcceptAction {
		if int(dh.Bits>>11)&0xF == dns.OpcodeUpdate {
			return dns.MsgAccept
		}
		return dns.DefaultMsgAcceptFunc(dh)
	}
	udp := &dns.Server{PacketConn: pc, Handler: handler, TsigSecret: secrets, MsgAcceptFunc: accept}
	tcp := &dns.Server{Listener: ln, Handler: handler, TsigSecret: secrets, MsgAcceptFunc: accept}
	started := make(chan struct{}, 2)
	udp.NotifyStartedFunc = func() { started <- struct{}{} }
	tcp.NotifyStartedFunc = func() { started <- struct{}{} }
	go func() { _ = udp.ActivateAndServe() }()
	go func() { _ = tcp.ActivateAndServe() }()
	for range 2 {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("dns server: not started")
		}
	}
	t.Cleanup(func() {
		_ = udp.Shutdown()
		_ = tcp.Shutdown()
	})
	return s
}

// TXT returns the values of the TXT record name.
func (s *Server) TXT(name string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.txt[dns.Fqdn(strings.ToLower(name))])
}

// AddTXT adds a value to the TXT record name (a fake provider).
func (s *Server) AddTXT(name, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name = dns.Fqdn(strings.ToLower(name))
	if !slices.Contains(s.txt[name], value) {
		s.txt[name] = append(s.txt[name], value)
	}
}

// RemoveTXT removes a value from the TXT record name.
func (s *Server) RemoveTXT(name, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name = dns.Fqdn(strings.ToLower(name))
	s.txt[name] = slices.DeleteFunc(s.txt[name], func(v string) bool { return v == value })
	if len(s.txt[name]) == 0 {
		delete(s.txt, name)
	}
}

// Updates returns the number of updates applied and refused.
func (s *Server) Updates() (applied, refused int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updates, s.refused
}

func (s *Server) soa() *dns.SOA {
	return &dns.SOA{Hdr: dns.RR_Header{Name: s.Zone, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 60},
		Ns: "ns1." + s.Zone, Mbox: "hostmaster." + s.Zone, Serial: 1, Refresh: 3600, Retry: 600, Expire: 86400, Minttl: 60}
}

func (s *Server) serve(w dns.ResponseWriter, r *dns.Msg) {
	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative = true
	if r.Opcode == dns.OpcodeUpdate {
		m.Rcode = s.update(w, r)
		if t := r.IsTsig(); t != nil && m.Rcode != dns.RcodeNotAuth {
			m.SetTsig(t.Hdr.Name, t.Algorithm, 300, time.Now().Unix())
		}
		_ = w.WriteMsg(m)
		return
	}
	if len(r.Question) != 1 {
		m.Rcode = dns.RcodeFormatError
		_ = w.WriteMsg(m)
		return
	}
	q := r.Question[0]
	name := strings.ToLower(q.Name)
	if !dns.IsSubDomain(s.Zone, name) {
		m.Rcode = dns.RcodeRefused
		_ = w.WriteMsg(m)
		return
	}
	s.mu.Lock()
	values := slices.Clone(s.txt[name])
	s.mu.Unlock()
	switch {
	case q.Qtype == dns.TypeSOA && name == s.Zone:
		m.Answer = append(m.Answer, s.soa())
	case q.Qtype == dns.TypeNS && name == s.Zone:
		m.Answer = append(m.Answer, &dns.NS{Hdr: dns.RR_Header{Name: s.Zone, Rrtype: dns.TypeNS, Class: dns.ClassINET, Ttl: 60}, Ns: "ns1." + s.Zone})
	case q.Qtype == dns.TypeA && name == "ns1."+s.Zone:
		m.Answer = append(m.Answer, &dns.A{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.IPv4(127, 0, 0, 1)})
	case q.Qtype == dns.TypeTXT && len(values) > 0:
		for _, v := range values {
			m.Answer = append(m.Answer, &dns.TXT{Hdr: dns.RR_Header{Name: name, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 60}, Txt: []string{v}})
		}
	default:
		// No data: the SOA of the zone in the authority section.
		m.Ns = append(m.Ns, s.soa())
	}
	_ = w.WriteMsg(m)
}

// update applies an RFC 2136 update of TXT records.
func (s *Server) update(w dns.ResponseWriter, r *dns.Msg) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tsigName != "" {
		t := r.IsTsig()
		if t == nil || w.TsigStatus() != nil || !strings.EqualFold(t.Hdr.Name, s.tsigName) {
			s.refused++
			return dns.RcodeNotAuth
		}
	}
	if len(r.Question) != 1 || !strings.EqualFold(r.Question[0].Name, s.Zone) {
		s.refused++
		return dns.RcodeNotZone
	}
	for _, rr := range r.Ns {
		t, ok := rr.(*dns.TXT)
		name := strings.ToLower(rr.Header().Name)
		if !dns.IsSubDomain(s.Zone, name) {
			s.refused++
			return dns.RcodeNotZone
		}
		switch rr.Header().Class {
		case dns.ClassINET:
			if ok && !slices.Contains(s.txt[name], strings.Join(t.Txt, "")) {
				s.txt[name] = append(s.txt[name], strings.Join(t.Txt, ""))
			}
		case dns.ClassNONE:
			if ok {
				v := strings.Join(t.Txt, "")
				s.txt[name] = slices.DeleteFunc(s.txt[name], func(x string) bool { return x == v })
			}
		case dns.ClassANY:
			delete(s.txt, name)
		}
		if len(s.txt[name]) == 0 {
			delete(s.txt, name)
		}
	}
	s.updates++
	return dns.RcodeSuccess
}
