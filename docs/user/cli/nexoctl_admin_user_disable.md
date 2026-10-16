# nexoctl admin user disable

Disable users (a disabled user cannot sign any request)

```
nexoctl admin user disable ALIAS ACCESS_KEY... [flags]
```

## Options

```
  -h, --help   help for disable
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

* [nexoctl admin user](nexoctl_admin_user.md)	 - Manage the IAM users

