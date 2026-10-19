# nexoctl media rule add

Add a video rule to a bucket, or replace the rule with the same ID

## Synopsis

Add a video rule to a bucket, or replace the rule with the same ID (keeping its
place: the first rule that selects a key applies). A rule produces at least one
of --rendition, --poster and --thumbnails.
Renditions: 2160p, 1440p, 1080p, 720p, 480p, 360p, 240p (those taller than the
source are skipped).

```
nexoctl media rule add ALIAS/BUCKET --id ID [flags]
```

## Examples

```
  nexoctl media rule add local/videos --id hd --prefix courses/ --rendition 720p,480p --poster
  nexoctl media rule add local/videos --id clips --suffix .mp4 --suffix .webm --thumbnails 5s
```

## Options

```
  -h, --help                help for add
      --id string           ID of the rule (1-64 letters, digits, '.', '_' or '-'; required)
      --poster              produce a JPEG poster frame
      --prefix string       select the keys under this prefix (default: the whole bucket)
      --rendition strings   produce these renditions (repeatable or comma-separated: 2160p,1440p,1080p,720p,480p,360p,240p)
      --suffix strings      select the keys with these suffixes, without case (repeatable or comma-separated; default .mp4,.mov,.m4v)
      --thumbnails string   produce timeline thumbnails, one every INTERVAL (1s to 10m, e.g. 10s)
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

* [nexoctl media rule](nexoctl_media_rule.md)	 - Add or remove one video rule of a bucket

