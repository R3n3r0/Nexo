# nexoctl media set

Replace the media rules of a bucket (JSON)

## Synopsis

Replace the media rules of a bucket with a JSON document read from FILE or
from the standard input ("-" or no FILE). The rules are checked before they are
sent; the server stores them normalized (default suffixes, renditions tallest
first, thumbnail interval, image formats the most compact first and widths
ascending).

```
nexoctl media set ALIAS/BUCKET [FILE] [flags]
```

## Examples

```
  nexoctl media set local/videos media.json
  nexoctl media get local/videos | nexoctl media set local/archive
```

## Options

```
  -h, --help   help for set
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

