## nexoctl dist cache export

Export the cache settings of a distribution as YAML

### Synopsis

Export the cache settings of a distribution (the document of "nexoctl dist cache
import"); the secrets of the webhooks are masked, and kept by an import.

```
nexoctl dist cache export ALIAS ID [-o FILE] [flags]
```

### Examples

```
  nexoctl dist cache export local sito-web -o sito-web-cache.yaml
```

### Options

```
  -h, --help            help for export
  -o, --output string   write the document to this file
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

* [nexoctl dist cache](nexoctl_dist_cache.md)	 - Manage the cache settings of a distribution, its domains and paths

