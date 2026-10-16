package commands

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/r3n3r0/nexo/internal/cli"
	"github.com/r3n3r0/nexo/pkg/nexoadmin"
)

// The DNS providers of the ACME DNS-01 challenge (docs/10 §10.2):
// nexoctl admin dns-provider.

// dnsProviderRecord is a DNS provider (never its credentials).
type dnsProviderRecord struct {
	Alias string `json:"alias"`
	nexoadmin.DNSProvider
}

// dnsTestRecord is the outcome of a test of a DNS provider.
type dnsTestRecord struct {
	Alias    string `json:"alias"`
	Provider string `json:"provider"`
	Domain   string `json:"domain"`
	nexoadmin.DNSProviderTestResult
}

func newAdminDNSProviderCmd(e *env) *cobra.Command {
	cmd := groupCmd("dns-provider", "Manage the DNS providers of the ACME DNS-01 challenge")
	cmd.Aliases = []string{"dns"}
	cmd.Long = `Manage the DNS providers that publish the TXT records of the ACME DNS-01 challenge:
the only challenge a wildcard alias ("*.example.com") can use. Types: cloudflare (API
token with Zone:DNS:Edit), route53 (access key, or the ambient AWS credentials of the
nodes) and rfc2136 (dynamic updates signed with a TSIG key).

The credentials are sealed in the system store and never shown again: give them with
the --*-file flags to keep them out of the shell history. A distribution uses a
provider with "nexoctl dist cert set ALIAS ID --auto --dns-provider NAME".
Needs admin:ManageCertificates.`
	cmd.AddCommand(newAdminDNSListCmd(e), newAdminDNSInfoCmd(e), newAdminDNSAddCmd(e), newAdminDNSEditCmd(e),
		newAdminDNSRemoveCmd(e), newAdminDNSTestCmd(e))
	return cmd
}

// dnsProviderFlags are the settings and credentials of a provider.
type dnsProviderFlags struct {
	typ, comment                          string
	zoneID, apiToken, apiTokenFile        string
	accessKeyID, secretKey, secretKeyFile string
	sessionToken, sessionTokenFile        string
	region, hostedZoneID                  string
	nameserver, zone, tsigKey, tsigAlgo   string
	tsigSecret, tsigSecretFile            string
	ttl                                   int
	propagationTimeout, pollingInterval   time.Duration
}

func (f *dnsProviderFlags) bind(cmd *cobra.Command, withType bool) {
	fs := cmd.Flags()
	if withType {
		fs.StringVar(&f.typ, "type", "", "provider type: cloudflare, route53 or rfc2136")
		_ = cmd.RegisterFlagCompletionFunc("type", cobra.FixedCompletions(
			[]string{nexoadmin.DNSProviderCloudflare, nexoadmin.DNSProviderRoute53, nexoadmin.DNSProviderRFC2136}, cobra.ShellCompDirectiveNoFileComp))
	}
	fs.StringVar(&f.comment, "comment", "", "free comment")
	fs.StringVar(&f.zoneID, "zone-id", "", "cloudflare: id of the zone (default: found by name)")
	fs.StringVar(&f.apiToken, "api-token", "", "cloudflare: API token (Zone:DNS:Edit)")
	fs.StringVar(&f.apiTokenFile, "api-token-file", "", "cloudflare: file holding the API token")
	fs.StringVar(&f.accessKeyID, "access-key-id", "", "route53: access key id (none: the ambient AWS credentials of the nodes)")
	fs.StringVar(&f.secretKey, "secret-access-key", "", "route53: secret access key")
	fs.StringVar(&f.secretKeyFile, "secret-access-key-file", "", "route53: file holding the secret access key")
	fs.StringVar(&f.sessionToken, "session-token", "", "route53: session token of temporary credentials")
	fs.StringVar(&f.sessionTokenFile, "session-token-file", "", "route53: file holding the session token")
	fs.StringVar(&f.region, "region", "", "route53: region of the API (default us-east-1)")
	fs.StringVar(&f.hostedZoneID, "hosted-zone-id", "", "route53: id of the hosted zone (default: the public zone found by name)")
	fs.StringVar(&f.nameserver, "nameserver", "", "rfc2136: primary nameserver accepting the updates (host[:port])")
	fs.StringVar(&f.zone, "zone", "", "rfc2136: zone of the records (default: from the SOA)")
	fs.StringVar(&f.tsigKey, "tsig-key", "", "rfc2136: name of the TSIG key (none: unsigned updates)")
	fs.StringVar(&f.tsigAlgo, "tsig-algorithm", "", "rfc2136: TSIG algorithm (default hmac-sha256)")
	fs.StringVar(&f.tsigSecret, "tsig-secret", "", "rfc2136: TSIG secret, base64")
	fs.StringVar(&f.tsigSecretFile, "tsig-secret-file", "", "rfc2136: file holding the TSIG secret")
	fs.IntVar(&f.ttl, "ttl", 0, "TTL of the records in seconds (default 60)")
	fs.DurationVar(&f.propagationTimeout, "propagation-timeout", 0, "wait of the records on the nameservers (default 2m)")
	fs.DurationVar(&f.pollingInterval, "polling-interval", 0, "delay between two checks of the propagation (default 4s)")
}

// apply sets on req the flags given on the command line.
func (f *dnsProviderFlags) apply(cmd *cobra.Command, req *nexoadmin.DNSProviderRequest) error {
	fs := cmd.Flags()
	changed := fs.Changed
	var err error
	if changed("type") {
		req.Type = strings.ToLower(f.typ)
	}
	if changed("comment") {
		req.Comment = f.comment
	}
	if changed("ttl") {
		req.TTL = f.ttl
	}
	if changed("propagation-timeout") {
		req.PropagationTimeout = int(f.propagationTimeout.Round(time.Second).Seconds())
	}
	if changed("polling-interval") {
		req.PollingInterval = int(f.pollingInterval.Round(time.Second).Seconds())
	}
	if req.Credentials.APIToken, err = readSecretFlag(cmd, f.apiToken, f.apiTokenFile, "api-token"); err != nil {
		return err
	}
	if req.Credentials.SecretAccessKey, err = readSecretFlag(cmd, f.secretKey, f.secretKeyFile, "secret-access-key"); err != nil {
		return err
	}
	if req.Credentials.SessionToken, err = readSecretFlag(cmd, f.sessionToken, f.sessionTokenFile, "session-token"); err != nil {
		return err
	}
	if req.Credentials.TSIGSecret, err = readSecretFlag(cmd, f.tsigSecret, f.tsigSecretFile, "tsig-secret"); err != nil {
		return err
	}
	switch req.Type {
	case nexoadmin.DNSProviderCloudflare:
		if req.Cloudflare == nil {
			req.Cloudflare = &nexoadmin.CloudflareSettings{}
		}
		if changed("zone-id") {
			req.Cloudflare.ZoneID = f.zoneID
		}
	case nexoadmin.DNSProviderRoute53:
		if req.Route53 == nil {
			req.Route53 = &nexoadmin.Route53Settings{}
		}
		for flag, dst := range map[string]*string{"access-key-id": &req.Route53.AccessKeyID, "region": &req.Route53.Region, "hosted-zone-id": &req.Route53.HostedZoneID} {
			if changed(flag) {
				*dst, _ = fs.GetString(flag)
			}
		}
	case nexoadmin.DNSProviderRFC2136:
		if req.RFC2136 == nil {
			req.RFC2136 = &nexoadmin.RFC2136Settings{}
		}
		for flag, dst := range map[string]*string{"nameserver": &req.RFC2136.Nameserver, "zone": &req.RFC2136.Zone,
			"tsig-key": &req.RFC2136.TSIGKeyName, "tsig-algorithm": &req.RFC2136.TSIGAlgorithm} {
			if changed(flag) {
				*dst, _ = fs.GetString(flag)
			}
		}
	case "":
		return cli.UsageError("--type is required: cloudflare, route53 or rfc2136")
	}
	return nil
}

// dnsProviderText renders a provider on one line.
func (e *env) dnsProviderText(p *nexoadmin.DNSProvider) string {
	var where string
	switch {
	case p.Cloudflare != nil:
		where = "zone " + orDash(p.Cloudflare.ZoneID)
	case p.Route53 != nil:
		where = "zone " + orDash(p.Route53.HostedZoneID)
		if p.Route53.AccessKeyID == "" {
			where += ", ambient credentials"
		}
	case p.RFC2136 != nil:
		where = p.RFC2136.Nameserver
		if p.RFC2136.TSIGKeyName != "" {
			where += ", key " + p.RFC2136.TSIGKeyName
		}
	}
	creds := "no"
	if p.HasCredentials {
		creds = "yes"
	}
	return strings.TrimRight(fmt.Sprintf("%s %-10s %-11s %-36s %s", e.out.Paint(cli.ColorCyan, fmt.Sprintf("%-20s", p.Name)), p.Type, creds,
		where, joinOrDash(p.UsedBy)), " ")
}

func newAdminDNSListCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "list ALIAS",
		Aliases:           []string{"ls"},
		Short:             "List the DNS providers",
		Args:              exactArgs(1),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			list, err := client.ListDNSProviders(ctx)
			if err != nil {
				return err
			}
			if len(list) == 0 {
				return e.out.Note("No DNS provider.")
			}
			if err := e.out.Text(fmt.Sprintf("%-20s %-10s %-11s %-36s %s", "NAME", "TYPE", "CREDENTIALS", "ZONE / SERVER", "USED BY")); err != nil {
				return err
			}
			for i := range list {
				if err := e.out.Result(dnsProviderRecord{Alias: alias, DNSProvider: list[i]}, e.dnsProviderText(&list[i])); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func newAdminDNSInfoCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "info ALIAS NAME",
		Short:             "Show a DNS provider (never its credentials)",
		Args:              exactArgs(2),
		ValidArgsFunction: completeAdmin(e, dnsProviderNames),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			p, err := client.GetDNSProvider(ctx, args[1])
			if err != nil {
				return fmt.Errorf("dns provider %q: %w", args[1], err)
			}
			var b strings.Builder
			fmt.Fprintf(&b, "Name:         %s\nType:         %s\n", p.Name, p.Type)
			if p.Comment != "" {
				fmt.Fprintf(&b, "Comment:      %s\n", p.Comment)
			}
			switch {
			case p.Cloudflare != nil:
				fmt.Fprintf(&b, "Zone id:      %s\n", orDash(p.Cloudflare.ZoneID))
			case p.Route53 != nil:
				fmt.Fprintf(&b, "Access key:   %s\nRegion:       %s\nHosted zone:  %s\n", orDash(p.Route53.AccessKeyID), orDash(p.Route53.Region),
					orDash(p.Route53.HostedZoneID))
			case p.RFC2136 != nil:
				fmt.Fprintf(&b, "Nameserver:   %s\nZone:         %s\nTSIG key:     %s %s\n", p.RFC2136.Nameserver, orDash(p.RFC2136.Zone),
					orDash(p.RFC2136.TSIGKeyName), p.RFC2136.TSIGAlgorithm)
			}
			creds := "not stored"
			if p.HasCredentials {
				creds = "stored (sealed, never shown)"
			}
			fmt.Fprintf(&b, "Credentials:  %s\nUsed by:      %s\nUpdated:      %s", creds, joinOrDash(p.UsedBy), cli.FormatTime(p.UpdatedAt))
			return e.out.Result(dnsProviderRecord{Alias: alias, DNSProvider: *p}, b.String())
		},
	}
}

func newAdminDNSAddCmd(e *env) *cobra.Command {
	var f dnsProviderFlags
	cmd := &cobra.Command{
		Use:   "add ALIAS NAME --type TYPE [flags]",
		Short: "Create a DNS provider",
		Example: `  nexoctl admin dns-provider add local cf --type cloudflare --api-token-file cf-token.txt
  nexoctl admin dns-provider add local aws --type route53 --hosted-zone-id Z123EXAMPLE
  nexoctl admin dns-provider add local bind --type rfc2136 --nameserver ns1.example.com \
    --tsig-key nexo-acme --tsig-secret-file tsig.key`,
		Args:              exactArgs(2),
		ValidArgsFunction: completeAliasNames(e),
		RunE: func(cmd *cobra.Command, args []string) error {
			req := nexoadmin.DNSProviderRequest{Name: args[1]}
			if err := f.apply(cmd, &req); err != nil {
				return err
			}
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			p, err := client.CreateDNSProvider(ctx, req)
			if err != nil {
				return fmt.Errorf("dns provider %q: %w", args[1], err)
			}
			return e.out.Info(dnsProviderRecord{Alias: alias, DNSProvider: *p}, fmt.Sprintf("Created the %s DNS provider %s.", p.Type, p.Name))
		},
	}
	f.bind(cmd, true)
	return cmd
}

func newAdminDNSEditCmd(e *env) *cobra.Command {
	var f dnsProviderFlags
	cmd := &cobra.Command{
		Use:   "edit ALIAS NAME [flags]",
		Short: "Change the settings or the credentials of a DNS provider",
		Long: `Change the settings given on the command line; the others, and the credentials not
given, are kept. Changing --type needs the credentials of the new type.`,
		Example:           "  nexoctl admin dns-provider edit local cf --api-token-file new-token.txt",
		Args:              exactArgs(2),
		ValidArgsFunction: completeAdmin(e, dnsProviderNames),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			cur, err := client.GetDNSProvider(ctx, args[1])
			if err != nil {
				return fmt.Errorf("dns provider %q: %w", args[1], err)
			}
			req := nexoadmin.DNSProviderRequest{Name: cur.Name, Type: cur.Type, Comment: cur.Comment, Cloudflare: cur.Cloudflare,
				Route53: cur.Route53, RFC2136: cur.RFC2136, TTL: cur.TTL, PropagationTimeout: cur.PropagationTimeout, PollingInterval: cur.PollingInterval}
			if cmd.Flags().Changed("type") && !strings.EqualFold(f.typ, cur.Type) {
				req.Cloudflare, req.Route53, req.RFC2136 = nil, nil, nil
			}
			if err := f.apply(cmd, &req); err != nil {
				return err
			}
			p, err := client.UpdateDNSProvider(ctx, args[1], req)
			if err != nil {
				return fmt.Errorf("dns provider %q: %w", args[1], err)
			}
			return e.out.Info(dnsProviderRecord{Alias: alias, DNSProvider: *p}, fmt.Sprintf("Updated the DNS provider %s.", p.Name))
		},
	}
	f.bind(cmd, true)
	return cmd
}

func newAdminDNSRemoveCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:               "remove ALIAS NAME",
		Aliases:           []string{"rm"},
		Short:             "Remove a DNS provider no distribution uses",
		Args:              exactArgs(2),
		ValidArgsFunction: completeAdmin(e, dnsProviderNames),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			if err := client.DeleteDNSProvider(ctx, args[1]); err != nil {
				return fmt.Errorf("dns provider %q: %w", args[1], err)
			}
			return e.out.Info(adminActionRecord{Alias: alias, Type: "dnsProvider", Name: args[1], Action: "removed"},
				fmt.Sprintf("Removed the DNS provider %s.", args[1]))
		},
	}
}

func newAdminDNSTestCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "test ALIAS NAME DOMAIN",
		Short: "Publish, check and remove a test record through a DNS provider",
		Long: `Publish a TXT record _acme-challenge.DOMAIN with a random value through the provider,
wait for it on the authoritative nameservers of the zone and remove it: the steps of a
DNS-01 validation, without the CA. Exits with an error when a step fails.`,
		Example:           "  nexoctl admin dns-provider test local cf example.com",
		Args:              exactArgs(3),
		ValidArgsFunction: completeAdmin(e, dnsProviderNames),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			alias, client, err := e.adminAlias(ctx, args[0])
			if err != nil {
				return err
			}
			res, err := client.TestDNSProvider(ctx, args[1], args[2])
			if err != nil {
				return fmt.Errorf("dns provider %q: %w", args[1], err)
			}
			took := cli.HumanDuration(time.Duration(res.DurationMS) * time.Millisecond)
			rec := dnsTestRecord{Alias: alias, Provider: args[1], Domain: args[2], DNSProviderTestResult: *res}
			if !res.OK {
				if err := e.out.Result(rec, fmt.Sprintf("%s %s failed at the %s step after %s: %s", e.out.Paint(cli.ColorRed, "FAIL"),
					res.Record, res.Step, took, res.Error)); err != nil {
					return err
				}
				return fmt.Errorf("dns provider %q: test failed at the %s step", args[1], res.Step)
			}
			return e.out.Result(rec, fmt.Sprintf("%s %s published, served by the nameservers and removed in %s.",
				e.out.Paint(cli.ColorGreen, "OK"), res.Record, took))
		},
	}
}

// dnsProviderNames lists the DNS providers (completion).
func dnsProviderNames(ctx context.Context, c cli.AdminClient) ([]string, error) {
	list, err := c.ListDNSProviders(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(list))
	for _, p := range list {
		out = append(out, p.Name)
	}
	return out, nil
}
