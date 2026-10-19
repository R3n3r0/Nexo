# nexoctl media status

Show the processing state of a video or an image by the media worker

## Synopsis

Show the processing state of a video or an image (default its latest version):
the state of its job (none, processing, done or failed), the step in progress or
that failed, the rule that selected it, the source and what the worker derived
(the renditions, poster and thumbnails of a video, the variants of an image).
The outputs are current while they derive from the content of the version;
stale outputs (the object was overwritten since) are not served.

```
nexoctl media status ALIAS/BUCKET/KEY [--version-id V] [flags]
```

## Examples

```
  nexoctl media status local/videos/courses/intro.mp4
  nexoctl media status --json local/videos/courses/intro.mp4
```

## Options

```
  -h, --help                help for status
      --version-id string   the version (default: the latest)
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

