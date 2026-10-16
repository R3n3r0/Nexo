# nexoctl rb

Remove buckets

## Synopsis

Remove empty buckets. With --force every object, version, delete marker and
incomplete upload of the bucket is removed first.

--force respects Object Lock: a bucket that holds versions under retention or
legal hold is left untouched (exit code 4), nothing is removed.

```
nexoctl rb [flags] ALIAS/BUCKET...
```

## Examples

```
  nexoctl rb --force local/scratch
```

## Options

```
      --force   remove the content of the bucket first
  -h, --help    help for rb
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

* [nexoctl](nexoctl.md)	 - Nexo command-line client

