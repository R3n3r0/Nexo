# nexoctl dist keygroup create

Create a key group with a first key pair

```
nexoctl dist keygroup create ALIAS ID [--no-key] [--algorithm ed25519|rsa] [--key-out FILE] [flags]
```

## Examples

```
  nexoctl dist keygroup create local rilasci
  nexoctl dist keygroup create local rilasci --algorithm rsa --bits 4096 --key-out rilasci.pem
```

## Options

```
      --algorithm string   ed25519 (default) or rsa
      --bits int           size of an RSA key (2048, 3072, 4096)
      --comment string     free text
  -h, --help               help for create
      --key-out string     file of the private key ("-": stderr; default: the key store of nexoctl)
      --no-key             create an empty group (add keys with "dist keygroup key add")
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

* [nexoctl dist keygroup](nexoctl_dist_keygroup.md)	 - Manage the key groups of the signed URLs and cookies

