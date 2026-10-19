# nexoctl media rule add

Add a video or image rule to a bucket, or replace the rule with the same ID

## Synopsis

Add a video or image rule to a bucket, or replace the rule with the same ID
(keeping its place: the first rule that selects a key applies). A video rule
produces at least one of --rendition, --poster and --thumbnails. --hls is the
rule of the HLS option of bucket creation (id "hls", every rendition, a poster
and thumbnails every 10s); the other flags change it.
Renditions: 2160p, 1440p, 1080p, 720p, 480p, 360p, 240p (those taller than the
source are skipped).

--images makes an image rule (id "images" by default; default suffixes: the
image formats the worker decodes): the worker derives from every image it
selects a variant in every --format (avif, webp, jpeg, png; default all) at every
--width (16 to 8192 pixels; default 160,320,640,1280,1920,2560), widths not
smaller than the image skipped, plus the image at its own size in every format.

```
nexoctl media rule add ALIAS/BUCKET (--id ID | --hls | --images) [flags]
```

## Examples

```
  nexoctl media rule add local/videos --id hd --prefix courses/ --rendition 720p,480p --poster
  nexoctl media rule add local/videos --id clips --suffix .mp4 --suffix .webm --thumbnails 5s
  nexoctl media rule add local/videos --hls
  nexoctl media rule add local/photos --images
  nexoctl media rule add local/photos --images --id thumbs --prefix gallery/ --format avif,webp --width 320,640
```

## Options

```
      --format strings      image rule: the formats of the variants (repeatable or comma-separated: avif,webp,jpeg,png; default all)
  -h, --help                help for add
      --hls                 the rule of the HLS option: id "hls", every rendition, a poster and thumbnails
      --id string           ID of the rule (1-64 letters, digits, '.', '_' or '-'; required)
      --images              an image rule (default id "images"): the variants of every image
      --poster              produce a JPEG poster frame
      --prefix string       select the keys under this prefix (default: the whole bucket)
      --rendition strings   produce these renditions (repeatable or comma-separated: 2160p,1440p,1080p,720p,480p,360p,240p)
      --suffix strings      select the keys with these suffixes, without case (repeatable or comma-separated; default .mp4,.mov,.m4v)
      --thumbnails string   produce timeline thumbnails, one every INTERVAL (1s to 10m, e.g. 10s)
      --width ints          image rule: the widths of the variants in pixels (repeatable or comma-separated; default 160,320,640,1280,1920,2560)
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

* [nexoctl media rule](nexoctl_media_rule.md)	 - Add or remove one media rule of a bucket

