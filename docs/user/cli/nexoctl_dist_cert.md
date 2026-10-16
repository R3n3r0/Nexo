# nexoctl dist cert

Manage the TLS certificates of the domains of the distributions

## Synopsis

Manage the TLS certificates of the domains of the distributions (served by SNI on
every node): ACME (--auto: obtained and renewed by the leader), uploaded PEM
certificates (--cert --key), or none when a proxy terminates TLS (--none).
A wildcard alias needs the DNS-01 challenge: --auto --dns-provider NAME, a provider
of "nexoctl admin dns-provider".
Needs admin:ManageCertificates (and admin:ManageDistributions for "set").

```
nexoctl dist cert [flags]
```

## Options

```
  -h, --help   help for cert
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

* [nexoctl dist](nexoctl_dist.md)	 - Manage the distributions: custom domains of the buckets
* [nexoctl dist cert list](nexoctl_dist_cert_list.md)	 - List the certificates with their status and expiration
* [nexoctl dist cert remove](nexoctl_dist_cert_remove.md)	 - Remove a certificate no distribution names
* [nexoctl dist cert set](nexoctl_dist_cert_set.md)	 - Set the TLS certificate of the domains of a distribution

