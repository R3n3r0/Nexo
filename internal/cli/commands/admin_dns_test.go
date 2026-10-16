package commands_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestScenarioDNSProviders manages the DNS providers of the DNS-01
// challenge and binds a wildcard distribution to one (TCL-D-04): the
// credentials are read from files and never printed.
func TestScenarioDNSProviders(t *testing.T) {
	h := newHarness(t)
	h.write("cf-token.txt", "cf-secret-token\n")
	h.write("tsig.key", "c2VjcmV0LXRzaWcta2V5\n")
	s := h.session()
	s.run("admin", "dns-provider", "ls", "mem")
	s.run("admin", "dns-provider", "add", "mem", "cf", "--type", "cloudflare", "--api-token-file", "cf-token.txt", "--zone-id", "zone-1")
	s.run("admin", "dns-provider", "add", "mem", "cf", "--type", "cloudflare", "--api-token", "x")
	s.run("admin", "dns-provider", "add", "mem", "nokey", "--type", "cloudflare")
	s.run("admin", "dns-provider", "add", "mem", "bind", "--type", "rfc2136", "--nameserver", "ns1.example.com",
		"--tsig-key", "nexo-acme", "--tsig-secret-file", "tsig.key", "--propagation-timeout", "3m")
	s.run("admin", "dns-provider", "add", "mem", "aws", "--type", "route53", "--hosted-zone-id", "Z123EXAMPLE")
	s.run("admin", "dns-provider", "add", "mem", "notype")
	s.run("admin", "dns-provider", "ls", "mem")
	s.run("admin", "dns-provider", "info", "mem", "bind")
	s.run("admin", "dns-provider", "edit", "mem", "cf", "--comment", "main zone")
	s.run("admin", "dns-provider", "info", "mem", "cf")
	s.run("admin", "dns-provider", "test", "mem", "cf", "example.test")
	s.run("admin", "dns-provider", "test", "mem", "cf", "example.com")
	s.run("dist", "create", "mem", "previews", "--bucket", "web", "--domain", "*.preview.example.com")
	s.run("dist", "cert", "set", "mem", "previews", "--auto")
	s.run("dist", "cert", "set", "mem", "previews", "--auto", "--dns-provider", "nope")
	s.run("dist", "cert", "set", "mem", "previews", "--none", "--dns-provider", "cf")
	s.run("dist", "cert", "set", "mem", "previews", "--auto", "--challenge", "dns-01")
	s.run("dist", "cert", "set", "mem", "previews", "--auto", "--dns-provider", "cf")
	s.run("dist", "enable", "mem", "previews")
	s.run("dist", "cert", "ls", "mem")
	s.run("admin", "dns-provider", "rm", "mem", "cf")
	s.run("admin", "dns-provider", "rm", "mem", "aws")
	s.run("admin", "dns-provider", "ls", "mem", "--json")
	out := s.b.String()
	for _, secret := range []string{"cf-secret-token", "c2VjcmV0LXRzaWcta2V5"} {
		assert.NotContains(t, out, secret, "credentials are never printed")
	}
	golden(t, "scenario/dns-providers", strings.ReplaceAll(out, h.dir, "$WORK"))
}
