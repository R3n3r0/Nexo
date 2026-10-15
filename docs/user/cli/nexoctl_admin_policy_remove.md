## nexoctl admin policy remove

Remove policies (built-in policies cannot be removed)

```
nexoctl admin policy remove ALIAS NAME... [flags]
```

### Aliases

```
nexoctl admin policy rm
```

### Options

```
  -h, --help   help for remove
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

* [nexoctl admin policy](nexoctl_admin_policy.md)	 - Manage the IAM policies (AWS policy language)

