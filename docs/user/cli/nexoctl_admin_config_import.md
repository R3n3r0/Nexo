## nexoctl admin config import

Import a configuration exported by "admin config export" (FILE, or - for stdin)

```
nexoctl admin config import ALIAS FILE [flags]
```

### Options

```
  -h, --help   help for import
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

* [nexoctl admin config](nexoctl_admin_config.md)	 - Read and change the dynamic configuration of the server

