package dns01_test

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/r3n3r0/nexo/internal/service/certs/dns01"
	"github.com/r3n3r0/nexo/internal/testutil/dnstest"
)

const (
	tsigName   = "nexo-acme."
	tsigSecret = "c2VjcmV0LXRzaWcta2V5LW9mLXRoZS10ZXN0cy0wMTIzNDU2Nzg5"
	record     = "_acme-challenge.site.test."
)

func TestParentNames(t *testing.T) {
	assert.Equal(t, []string{"_acme-challenge.a.site.test", "a.site.test", "site.test"}, dns01.ParentNames("_acme-challenge.a.site.test."))
}

// TestRFC2136 publishes and removes TXT values with TSIG-signed updates on
// an in-process nameserver; the propagation check sees them.
func TestRFC2136(t *testing.T) {
	ctx := context.Background()
	srv := dnstest.Start(t, "site.test", dnstest.WithTSIG(tsigName, tsigSecret))
	for _, zone := range []string{"site.test", ""} { // explicit zone, then from the SOA
		t.Run("zone="+zone, func(t *testing.T) {
			p, err := dns01.NewRFC2136(dns01.RFC2136Config{Nameserver: srv.Addr, TSIGKeyName: tsigName, TSIGSecret: tsigSecret, Zone: zone,
				Settings: dns01.Settings{PropagationTimeout: 5 * time.Second, PollingInterval: 50 * time.Millisecond}})
			require.NoError(t, err)
			require.NoError(t, p.Present(ctx, record, "one"))
			require.NoError(t, p.Present(ctx, record, "two"))
			assert.ElementsMatch(t, []string{"one", "two"}, srv.TXT(record))

			checker := dns01.NewChecker(dns01.WithNameservers(srv.Addr))
			timeout, interval := p.Propagation()
			require.NoError(t, checker.Wait(ctx, record, "two", timeout, interval))

			require.NoError(t, p.CleanUp(ctx, record, "one"))
			assert.Equal(t, []string{"two"}, srv.TXT(record), "only its value is removed")
			require.NoError(t, p.CleanUp(ctx, record, "two"))
			require.NoError(t, p.CleanUp(ctx, record, "two"), "removing a missing value is not an error")
			assert.Empty(t, srv.TXT(record))
		})
	}

	t.Run("wrong key", func(t *testing.T) {
		p, err := dns01.NewRFC2136(dns01.RFC2136Config{Nameserver: srv.Addr, TSIGKeyName: tsigName, TSIGSecret: "d3Jvbmc=", Zone: "site.test"})
		require.NoError(t, err)
		err = p.Present(ctx, record, "x")
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "d3Jvbmc=", "the secret never appears in an error")
		assert.Empty(t, srv.TXT(record))
		_, refused := srv.Updates()
		assert.Positive(t, refused)
	})
	t.Run("unsigned refused", func(t *testing.T) {
		p, err := dns01.NewRFC2136(dns01.RFC2136Config{Nameserver: srv.Addr, Zone: "site.test"})
		require.NoError(t, err)
		require.ErrorContains(t, p.Present(ctx, record, "x"), "NOTAUTH")
	})
}

func TestRFC2136Config(t *testing.T) {
	for name, cfg := range map[string]dns01.RFC2136Config{
		"no nameserver":      {},
		"secret without key": {Nameserver: "ns.site.test", TSIGSecret: tsigSecret},
		"key without secret": {Nameserver: "ns.site.test", TSIGKeyName: "k"},
		"secret not base64":  {Nameserver: "ns.site.test", TSIGKeyName: "k", TSIGSecret: "%%%"},
		"unknown algorithm":  {Nameserver: "ns.site.test", TSIGKeyName: "k", TSIGSecret: tsigSecret, TSIGAlgorithm: "hmac-md5"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := dns01.NewRFC2136(cfg)
			require.ErrorIs(t, err, dns01.ErrInvalidConfig)
		})
	}
	_, err := dns01.NewRFC2136(dns01.RFC2136Config{Nameserver: "ns.site.test:5353", TSIGKeyName: "k", TSIGSecret: tsigSecret, TSIGAlgorithm: "HMAC-SHA512."})
	require.NoError(t, err)
}

// TestChecker finds the authoritative nameservers of the zone through the
// resolvers, then waits for the value; a value never published times out.
func TestChecker(t *testing.T) {
	ctx := context.Background()
	srv := dnstest.Start(t, "site.test")
	_, port, err := net.SplitHostPort(srv.Addr)
	require.NoError(t, err)
	// The test server is its own resolver: SOA, NS ns1.site.test and its
	// address 127.0.0.1.
	checker := dns01.NewChecker(dns01.WithResolvers(srv.Addr), dns01.WithNameserverPort(port))

	go func() {
		time.Sleep(150 * time.Millisecond)
		srv.AddTXT(record, "late")
	}()
	start := time.Now()
	require.NoError(t, checker.Wait(ctx, record, "late", 5*time.Second, 50*time.Millisecond))
	assert.GreaterOrEqual(t, time.Since(start), 100*time.Millisecond, "waited for the record")

	err = checker.Wait(ctx, record, "never", 300*time.Millisecond, 50*time.Millisecond)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not served")

	// Outside of the zone the server refuses: no zone is found.
	err = checker.Wait(ctx, "_acme-challenge.other.test.", "x", 200*time.Millisecond, 50*time.Millisecond)
	require.Error(t, err)
}

// --- Cloudflare --------------------------------------------------------------

// fakeCloudflare is the part of the Cloudflare v4 API the provider uses.
type fakeCloudflare struct {
	t       *testing.T
	token   string
	mu      sync.Mutex
	records map[string]map[string]any // id → record
	next    int
}

func (f *fakeCloudflare) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reply := func(status int, success bool, result any, errs ...string) {
		var e []map[string]any
		for _, m := range errs {
			e = append(e, map[string]any{"code": 1000, "message": m})
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": success, "errors": e, "result": result})
	}
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		reply(http.StatusForbidden, false, nil, "Invalid API Token")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/zones":
		if r.URL.Query().Get("name") == "site.test" {
			reply(http.StatusOK, true, []map[string]any{{"id": "zone-1", "name": "site.test"}})
			return
		}
		reply(http.StatusOK, true, []any{})
	case r.Method == http.MethodGet && r.URL.Path == "/zones/zone-1/dns_records":
		out := []any{}
		for _, rec := range f.records {
			if rec["name"] == r.URL.Query().Get("name") && rec["type"] == r.URL.Query().Get("type") {
				out = append(out, rec)
			}
		}
		reply(http.StatusOK, true, out)
	case r.Method == http.MethodPost && r.URL.Path == "/zones/zone-1/dns_records":
		var rec map[string]any
		require.NoError(f.t, json.NewDecoder(r.Body).Decode(&rec))
		f.next++
		rec["id"] = "rec-" + strconv.Itoa(f.next)
		f.records[rec["id"].(string)] = rec
		reply(http.StatusOK, true, rec)
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/zones/zone-1/dns_records/"):
		id := strings.TrimPrefix(r.URL.Path, "/zones/zone-1/dns_records/")
		delete(f.records, id)
		reply(http.StatusOK, true, map[string]any{"id": id})
	default:
		reply(http.StatusNotFound, false, nil, "not found")
	}
}

func (f *fakeCloudflare) values() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, rec := range f.records {
		out = append(out, rec["content"].(string))
	}
	return out
}

func TestCloudflare(t *testing.T) {
	ctx := context.Background()
	fake := &fakeCloudflare{t: t, token: "cf-secret-token", records: map[string]map[string]any{}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	p, err := dns01.NewCloudflare(dns01.CloudflareConfig{APIToken: "cf-secret-token"}, dns01.WithEndpoint(srv.URL), dns01.WithHTTPClient(srv.Client()))
	require.NoError(t, err)
	require.NoError(t, p.Present(ctx, record, "one"))
	require.NoError(t, p.Present(ctx, record, "one"), "idempotent")
	require.NoError(t, p.Present(ctx, record, "two"))
	assert.ElementsMatch(t, []string{"one", "two"}, fake.values())
	require.NoError(t, p.CleanUp(ctx, record, "one"))
	assert.Equal(t, []string{"two"}, fake.values())
	require.NoError(t, p.CleanUp(ctx, record, "two"))
	assert.Empty(t, fake.values())

	// A record outside of the zones of the account.
	err = p.Present(ctx, "_acme-challenge.other.test.", "x")
	require.ErrorContains(t, err, "no zone")

	// A wrong token: the API error, never the token.
	bad, err := dns01.NewCloudflare(dns01.CloudflareConfig{APIToken: "wrong-token", ZoneID: "zone-1"}, dns01.WithEndpoint(srv.URL))
	require.NoError(t, err)
	err = bad.Present(ctx, record, "x")
	require.ErrorContains(t, err, "Invalid API Token")
	assert.NotContains(t, err.Error(), "wrong-token")

	_, err = dns01.NewCloudflare(dns01.CloudflareConfig{})
	require.ErrorIs(t, err, dns01.ErrInvalidConfig)
}

// --- Route 53 ----------------------------------------------------------------

// fakeRoute53 is the part of the Route 53 REST/XML API the provider uses.
type fakeRoute53 struct {
	t       *testing.T
	mu      sync.Mutex
	sets    map[string][]string // name → quoted values
	changes int
	gets    int
	auth    []string
}

type r53Change struct {
	Action string `xml:"Action"`
	Set    struct {
		Name    string   `xml:"Name"`
		Type    string   `xml:"Type"`
		TTL     int      `xml:"TTL"`
		Records []string `xml:"ResourceRecords>ResourceRecord>Value"`
	} `xml:"ResourceRecordSet"`
}

func (f *fakeRoute53) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	write := func(body string) {
		w.Header().Set("Content-Type", "text/xml")
		_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>`+body)
	}
	const ns = ` xmlns="https://route53.amazonaws.com/doc/2013-04-01/"`
	path := strings.TrimSuffix(r.URL.Path, "/")
	switch {
	case r.Method == http.MethodGet && path == "/2013-04-01/hostedzonesbyname":
		name := r.URL.Query().Get("dnsname")
		zones := ""
		if strings.TrimSuffix(name, ".") == "site.test" {
			zones = `<HostedZone><Id>/hostedzone/ZPRIVATE</Id><Name>site.test.</Name><CallerReference>p</CallerReference><Config><PrivateZone>true</PrivateZone></Config></HostedZone>` +
				`<HostedZone><Id>/hostedzone/Z1</Id><Name>site.test.</Name><CallerReference>c</CallerReference><Config><PrivateZone>false</PrivateZone></Config></HostedZone>`
		}
		write(`<ListHostedZonesByNameResponse` + ns + `><HostedZones>` + zones + `</HostedZones><DNSName>` + name +
			`</DNSName><IsTruncated>false</IsTruncated><MaxItems>100</MaxItems></ListHostedZonesByNameResponse>`)
	case r.Method == http.MethodGet && path == "/2013-04-01/hostedzone/Z1/rrset":
		name := strings.TrimSuffix(r.URL.Query().Get("name"), ".") + "."
		sets := ""
		if vals := f.sets[name]; len(vals) > 0 {
			recs := ""
			for _, v := range vals {
				recs += "<ResourceRecord><Value>" + xmlEscape(v) + "</Value></ResourceRecord>"
			}
			sets = `<ResourceRecordSet><Name>` + name + `</Name><Type>TXT</Type><TTL>60</TTL><ResourceRecords>` + recs + `</ResourceRecords></ResourceRecordSet>`
		}
		write(`<ListResourceRecordSetsResponse` + ns + `><ResourceRecordSets>` + sets +
			`</ResourceRecordSets><IsTruncated>false</IsTruncated><MaxItems>1</MaxItems></ListResourceRecordSetsResponse>`)
	case r.Method == http.MethodPost && path == "/2013-04-01/hostedzone/Z1/rrset":
		var req struct {
			Changes []r53Change `xml:"ChangeBatch>Changes>Change"`
		}
		require.NoError(f.t, xml.NewDecoder(r.Body).Decode(&req))
		for _, c := range req.Changes {
			name := strings.TrimSuffix(c.Set.Name, ".") + "."
			switch c.Action {
			case "UPSERT":
				f.sets[name] = c.Set.Records
			case "DELETE":
				if strings.Join(f.sets[name], ",") != strings.Join(c.Set.Records, ",") {
					w.WriteHeader(http.StatusBadRequest)
					write(`<ErrorResponse` + ns + `><Error><Type>Sender</Type><Code>InvalidChangeBatch</Code><Message>values do not match</Message></Error></ErrorResponse>`)
					return
				}
				delete(f.sets, name)
			}
		}
		f.changes++
		write(`<ChangeResourceRecordSetsResponse` + ns + `><ChangeInfo><Id>/change/C` + strconv.Itoa(f.changes) +
			`</Id><Status>PENDING</Status><SubmittedAt>2026-10-16T00:00:00Z</SubmittedAt></ChangeInfo></ChangeResourceRecordSetsResponse>`)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/2013-04-01/change/"):
		f.gets++
		write(`<GetChangeResponse` + ns + `><ChangeInfo><Id>/change/` + strings.TrimPrefix(path, "/2013-04-01/change/") +
			`</Id><Status>INSYNC</Status><SubmittedAt>2026-10-16T00:00:00Z</SubmittedAt></ChangeInfo></GetChangeResponse>`)
	default:
		w.WriteHeader(http.StatusNotFound)
		write(fmt.Sprintf(`<ErrorResponse%s><Error><Code>NotFound</Code><Message>%s %s</Message></Error></ErrorResponse>`, ns, r.Method, path))
	}
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func (f *fakeRoute53) values(name string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sets[name]
}

func TestRoute53(t *testing.T) {
	ctx := context.Background()
	fake := &fakeRoute53{t: t, sets: map[string][]string{}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	p, err := dns01.NewRoute53(dns01.Route53Config{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "r53-secret-key",
		Settings: dns01.Settings{PollingInterval: 10 * time.Millisecond, PropagationTimeout: 5 * time.Second}},
		dns01.WithEndpoint(srv.URL), dns01.WithHTTPClient(srv.Client()))
	require.NoError(t, err)
	require.NoError(t, p.Present(ctx, record, "one"))
	require.NoError(t, p.Present(ctx, record, "two"))
	require.NoError(t, p.Present(ctx, record, "two"), "idempotent")
	assert.Equal(t, []string{`"one"`, `"two"`}, fake.values(record), "the public zone, the values quoted")
	require.NoError(t, p.CleanUp(ctx, record, "one"))
	assert.Equal(t, []string{`"two"`}, fake.values(record))
	require.NoError(t, p.CleanUp(ctx, record, "two"))
	assert.Empty(t, fake.values(record))
	require.NoError(t, p.CleanUp(ctx, record, "two"), "nothing to remove")

	fake.mu.Lock()
	assert.Equal(t, 4, fake.changes)
	assert.Positive(t, fake.gets, "waited for INSYNC")
	for _, a := range fake.auth {
		assert.True(t, strings.HasPrefix(a, "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/"), "signed with SigV4: %s", a)
		assert.NotContains(t, a, "r53-secret-key")
	}
	fake.mu.Unlock()

	err = p.Present(ctx, "_acme-challenge.other.test.", "x")
	require.ErrorContains(t, err, "no public hosted zone")

	for name, cfg := range map[string]dns01.Route53Config{
		"secret without key": {SecretAccessKey: "s"},
		"key without secret": {AccessKeyID: "AKID"},
		"session alone":      {SessionToken: "t"},
	} {
		_, err := dns01.NewRoute53(cfg)
		require.ErrorIs(t, err, dns01.ErrInvalidConfig, name)
	}
}

// TestRoute53Ambient uses the ambient credentials of the node (here the
// environment) when no access key is configured.
func TestRoute53Ambient(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDAMBIENT")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "ambient-secret")
	t.Setenv("AWS_CONFIG_FILE", t.TempDir()+"/none")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", t.TempDir()+"/none")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	fake := &fakeRoute53{t: t, sets: map[string][]string{}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	p, err := dns01.NewRoute53(dns01.Route53Config{HostedZoneID: "Z1", Settings: dns01.Settings{PollingInterval: 10 * time.Millisecond}},
		dns01.WithEndpoint(srv.URL))
	require.NoError(t, err)
	require.NoError(t, p.Present(context.Background(), record, "v"))
	fake.mu.Lock()
	defer fake.mu.Unlock()
	assert.Contains(t, fake.auth[0], "Credential=AKIDAMBIENT/")
}
