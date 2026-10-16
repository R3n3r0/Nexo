# nexoctl dist edit

Change the origin, the mode or the default behavior of a distribution

```
nexoctl dist edit ALIAS ID [--bucket B] [--prefix P] [--mode M] [--access A] [--index I] [--spa S] ... [flags]
```

## Examples

```
  nexoctl dist edit local website --prefix /v2 --cache-control 'public, max-age=300'
  nexoctl dist edit local website --access signed --key-group rilasci
```

## Options

```
      --access string          access: public, signed (URLs/cookies of --key-group), authenticated (SigV4) or deny
      --bucket string          origin bucket
      --cache-control string   Cache-Control sent to the browsers
      --certificate string     TLS certificate: auto, none, upload:ID
      --comment string         free text
      --header stringArray     response header 'Name: value' (repeatable)
  -h, --help                   help for edit
      --index string           index document appended to the paths ending with /
      --key-group string       key group of the signed URLs and cookies
      --methods stringArray    allowed methods (default GET,HEAD,OPTIONS)
      --mode string            website or s3
      --prefix string          origin prefix
      --spa string             path served (200) instead of a missing object (single page application)
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

