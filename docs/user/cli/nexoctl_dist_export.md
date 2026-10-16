# nexoctl dist export

Export distributions as YAML

## Synopsis

Export a distribution (every distribution without ID, one YAML document each) to
the standard output, or to FILE with --output: the documents of "nexoctl dist
import", for the GitOps management of the distributions.

```
nexoctl dist export ALIAS [ID] [-o FILE] [flags]
```

## Examples

```
  nexoctl dist export local website -o website.yaml
  nexoctl dist export local > distributions.yaml
```

## Options

```
  -h, --help            help for export
  -o, --output string   write the documents to this file
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

