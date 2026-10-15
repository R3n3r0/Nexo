## nexoctl admin kms key list

List the keys of the KMS

```
nexoctl admin kms key list ALIAS [PREFIX] [flags]
```

### Aliases

```
nexoctl admin kms key ls
```

### Examples

```
  nexoctl admin kms key ls local
```

### Options

```
  -h, --help   help for list
```

### Options inherited from parent commands

```
      --config-dir string   configuration directory (default $NEXOCTL_CONFIG_DIR or ~/.nexoctl)
      --debug               dump the HTTP requests and responses on stderr, with the secrets redacted
      --json                print JSON Lines (one object per line with a "status" member)
      --no-color            disable colors (also with the NO_COLOR environment variable)
  -q, --quiet               hide confirmations and progress (results and errors are still printed)
```

### SEE ALSO

* [nexoctl admin kms key](nexoctl_admin_kms_key.md)	 - Create and list the keys of the KMS

