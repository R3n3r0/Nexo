# nexoctl admin dns-provider

Manage the DNS providers of the ACME DNS-01 challenge

## Synopsis

Manage the DNS providers that publish the TXT records of the ACME DNS-01 challenge:
the only challenge a wildcard alias ("\*.example.com") can use. Types: cloudflare (API
token with Zone:DNS:Edit), route53 (access key, or the ambient AWS credentials of the
nodes) and rfc2136 (dynamic updates signed with a TSIG key).

The credentials are sealed in the system store and never shown again: give them with
the --\*-file flags to keep them out of the shell history. A distribution uses a
provider with "nexoctl dist cert set ALIAS ID --auto --dns-provider NAME".
Needs admin:ManageCertificates.

```
nexoctl admin dns-provider [flags]
```

## Aliases

```
nexoctl admin dns
```

## Options

```
  -h, --help   help for dns-provider
```

## Options inherited from parent commands

```
      --config-dir string   configuration directory (default $NEXOCTL_CONFIG_DIR or ~/.nexoctl)
      --debug               dump the HTTP requests and responses on stderr, with the secrets redacted
      --json                print JSON Lines (one object per line with a "status" member)
      --no-color            disable colors (also with the NO_COLOR environment variable)
  -q, --quiet               hide confirmations and progress (results and errors are still printed)
```

## SEE ALSO

* [nexoctl admin](nexoctl_admin.md)	 - Administer a Nexo server (Admin API)
* [nexoctl admin dns-provider add](nexoctl_admin_dns-provider_add.md)	 - Create a DNS provider
* [nexoctl admin dns-provider edit](nexoctl_admin_dns-provider_edit.md)	 - Change the settings or the credentials of a DNS provider
* [nexoctl admin dns-provider info](nexoctl_admin_dns-provider_info.md)	 - Show a DNS provider (never its credentials)
* [nexoctl admin dns-provider list](nexoctl_admin_dns-provider_list.md)	 - List the DNS providers
* [nexoctl admin dns-provider remove](nexoctl_admin_dns-provider_remove.md)	 - Remove a DNS provider no distribution uses
* [nexoctl admin dns-provider test](nexoctl_admin_dns-provider_test.md)	 - Publish, check and remove a test record through a DNS provider

