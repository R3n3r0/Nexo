## nexoctl alias list

List aliases (secret keys are never shown)

```
nexoctl alias list [flags]
```

### Aliases

```
nexoctl alias ls
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

* [nexoctl alias](nexoctl_alias.md)	 - Manage server aliases (saved in ~/.nexoctl/config.json)

