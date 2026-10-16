# nexoctl dist error-page

Replace the error answers of a distribution with pages

```
nexoctl dist error-page [flags]
```

## Options

```
  -h, --help   help for error-page
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

* [nexoctl dist](nexoctl_dist.md)	 - Manage the distributions: custom domains of the buckets
* [nexoctl dist error-page remove](nexoctl_dist_error-page_remove.md)	 - Remove the error page of a status
* [nexoctl dist error-page set](nexoctl_dist_error-page_set.md)	 - Answer an error status with a page of the distribution

