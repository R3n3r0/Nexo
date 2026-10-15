## nexoctl admin config export

Export the whole configuration as JSON (stdout or --output)

### Synopsis

Export the whole dynamic configuration as a JSON document, secrets included:
protect the file. Restore it with "nexoctl admin config import".

```
nexoctl admin config export [flags] ALIAS
```

### Examples

```
  nexoctl admin config export local --output nexo-config.json
```

### Options

```
  -h, --help            help for export
  -o, --output string   write the document to this file (mode 0600)
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

