# nexoctl dist domain add

Add domains (exact or \*.wildcard) to a distribution

```
nexoctl dist domain add ALIAS ID DOMAIN... [flags]
```

## Examples

```
  nexoctl dist domain add local sito-web '*.preview.example.com'
  nexoctl dist domain rm local sito-web example.com
```

## Options

```
  -h, --help   help for add
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

* [nexoctl dist domain](nexoctl_dist_domain.md)	 - Add and remove the domains (aliases) of a distribution

