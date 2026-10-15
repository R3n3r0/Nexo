## nexoctl alias

Manage server aliases (saved in ~/.nexoctl/config.json)

### Synopsis

An alias names a server endpoint and its credentials. The configuration file
is ~/.nexoctl/config.json (or $NEXOCTL\_CONFIG\_DIR/config.json, or --config-dir),
written with owner-only permissions (0600).

```
nexoctl alias [flags]
```

### Options

```
  -h, --help   help for alias
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

* [nexoctl](nexoctl.md)	 - Nexo command-line client
* [nexoctl alias list](nexoctl_alias_list.md)	 - List aliases (secret keys are never shown)
* [nexoctl alias remove](nexoctl_alias_remove.md)	 - Remove an alias
* [nexoctl alias set](nexoctl_alias_set.md)	 - Add or update an alias

