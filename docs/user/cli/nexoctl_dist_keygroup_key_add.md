# nexoctl dist keygroup key add

Add a key to a key group (generated, or your public key)

```
nexoctl dist keygroup key add ALIAS GROUP [--public-key FILE] [--algorithm ed25519|rsa] [--key-out FILE] [flags]
```

## Examples

```
  nexoctl dist keygroup key add local rilasci
  nexoctl dist keygroup key add local rilasci --public-key mykey.pub.pem
```

## Options

```
      --algorithm string    ed25519 (default) or rsa
      --bits int            size of an RSA key (2048, 3072, 4096)
      --comment string      free text
  -h, --help                help for add
      --key-out string      file of the private key ("-": stderr; default: the key store of nexoctl)
      --public-key string   PEM public key (PKIX) to add instead of generating a key pair
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

* [nexoctl dist keygroup key](nexoctl_dist_keygroup_key.md)	 - Add and remove the keys of a key group

