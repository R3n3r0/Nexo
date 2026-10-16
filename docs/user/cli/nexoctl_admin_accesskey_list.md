# nexoctl admin accesskey list

List access keys (of the user of the alias, or of --user)

```
nexoctl admin accesskey list [flags] ALIAS
```

## Aliases

```
nexoctl admin accesskey ls
```

## Options

```
  -h, --help          help for list
      --user string   list the keys of this user
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

* [nexoctl admin accesskey](nexoctl_admin_accesskey.md)	 - Manage the access keys (service accounts) of the users

