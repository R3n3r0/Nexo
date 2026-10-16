# nexoctl dist cert set

Set the TLS certificate of the domains of a distribution

```
nexoctl dist cert set ALIAS ID (--auto | --none | --cert FILE --key FILE [--cert-id ID]) [flags]
```

## Examples

```
  nexoctl dist cert set local sito-web --auto
  nexoctl dist cert set local sito-web --cert crt.pem --key key.pem
```

## Options

```
      --auto             obtain and renew the certificate with ACME
      --cert string      PEM certificate chain (leaf first)
      --cert-id string   id of the uploaded certificate (default dist-ID)
  -h, --help             help for set
      --key string       PEM private key of the certificate
      --none             no certificate (TLS terminated by a proxy)
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

