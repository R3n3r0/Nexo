# nexoctl dist import

Create or replace the distributions of a YAML file

## Synopsis

Create, or replace when they exist, the distributions of a YAML (or JSON) file of
one or more documents (the format of "nexoctl dist export"). The server
validates every distribution.

```
nexoctl dist import ALIAS FILE [flags]
```

## Examples

```
  nexoctl dist import local distributions.yaml
```

## Options

```
  -h, --help   help for import
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

