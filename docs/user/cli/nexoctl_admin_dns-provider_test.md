# nexoctl admin dns-provider test

Publish, check and remove a test record through a DNS provider

## Synopsis

Publish a TXT record \_acme-challenge.DOMAIN with a random value through the provider,
wait for it on the authoritative nameservers of the zone and remove it: the steps of a
DNS-01 validation, without the CA. Exits with an error when a step fails.

```
nexoctl admin dns-provider test ALIAS NAME DOMAIN [flags]
```

## Examples

```
  nexoctl admin dns-provider test local cf example.com
```

## Options

```
  -h, --help   help for test
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

* [nexoctl admin dns-provider](nexoctl_admin_dns-provider.md)	 - Manage the DNS providers of the ACME DNS-01 challenge

