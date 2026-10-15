## nexoctl dist cache import

Replace the cache settings of distributions with those of a YAML file

### Synopsis

Replace the cache settings of the distributions of a YAML (or JSON) file of one
or more documents (the format of "nexoctl dist cache export"), each naming its
distribution.

```
nexoctl dist cache import ALIAS FILE [flags]
```

### Examples

```
  nexoctl dist cache import local sito-web-cache.yaml
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

* [nexoctl dist cache](nexoctl_dist_cache.md)	 - Manage the cache settings of a distribution, its domains and paths

