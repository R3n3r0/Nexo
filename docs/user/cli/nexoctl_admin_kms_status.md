# nexoctl admin kms status

Show the state of the KMS

```
nexoctl admin kms status ALIAS [flags]
```

## Examples

```
  nexoctl admin kms status local
```

## Options

```
  -h, --help   help for status
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

* [nexoctl admin kms](nexoctl_admin_kms.md)	 - Show the KMS of the server-side encryption and manage its keys

