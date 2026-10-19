# nexoctl media reprocess

Queue the processing of a video or an image again

## Synopsis

Queue a job of the media worker for a video or an image (default its latest
version), with the rule of its bucket that selects it now: after a change of the
rules, or to retry a failed job. "media status" follows the job.

```
nexoctl media reprocess ALIAS/BUCKET/KEY [--version-id V] [flags]
```

## Examples

```
  nexoctl media reprocess local/videos/courses/intro.mp4
  nexoctl media reprocess --version-id 3f2a... local/videos/courses/intro.mp4
```

## Options

```
  -h, --help                help for reprocess
      --version-id string   the version to process (default: the latest)
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

