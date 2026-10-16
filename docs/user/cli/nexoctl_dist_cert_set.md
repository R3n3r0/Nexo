# nexoctl dist cert set

Set the TLS certificate of the domains of a distribution

```
nexoctl dist cert set ALIAS ID (--auto [--challenge C] [--dns-provider NAME] | --none | --cert FILE --key FILE [--cert-id ID]) [flags]
```

## Examples

```
  nexoctl dist cert set local website --auto
  nexoctl dist cert set local previews --auto --challenge dns-01 --dns-provider cf
  nexoctl dist cert set local website --cert crt.pem --key key.pem
```

## Options

```
      --auto                  obtain and renew the certificate with ACME
      --cert string           PEM certificate chain (leaf first)
      --cert-id string        id of the uploaded certificate (default dist-ID)
      --challenge string      ACME challenge with --auto: http-01, tls-alpn-01 or dns-01 (default: the one of the server)
      --dns-provider string   DNS provider of the dns-01 challenge (implies --challenge dns-01; required by a wildcard alias)
  -h, --help                  help for set
      --key string            PEM private key of the certificate
      --none                  no certificate (TLS terminated by a proxy)
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

* [nexoctl dist cert](nexoctl_dist_cert.md)	 - Manage the TLS certificates of the domains of the distributions

