# nexoctl media remove

Remove the media rules of a bucket

## Synopsis

Remove the media rules of a bucket: the media worker no longer processes its new
videos and images. What it already derived stays in .nexo-media/ and is still served.

```
nexoctl media remove ALIAS/BUCKET [flags]
```

## Aliases

```
nexoctl media rm
```

## Options

```
  -h, --help   help for remove
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

* [nexoctl media](nexoctl_media.md)	 - Manage the video and image rules of buckets and the media worker (Admin API)

