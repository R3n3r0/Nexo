# nexoctl mb

Make buckets

## Synopsis

Make buckets. --with-lock creates them with Object Lock (WORM retention and
legal holds, see "nexoctl retention" and "nexoctl legalhold"), which implies
versioning; an existing bucket can get it later only once its versioning is
enabled. --with-hls makes a video bucket: it adds the media rule "hls", with
which the media worker derives every rendition (240p to 2160p, up to the
height of each video), a poster and timeline thumbnails from every video
uploaded (see "nexoctl media"; it needs the Admin API and a media worker).
--with-images makes an image bucket: it adds the media rule "images", with which
the media worker derives every format (AVIF, WebP, JPEG, PNG) at every width
from 160 to 2560 pixels (up to the width of each image) from every image
uploaded. Both make a bucket for videos and images.

```
nexoctl mb [flags] ALIAS/BUCKET...
```

## Examples

```
  nexoctl mb --with-versioning local/photos
  nexoctl mb --with-lock local/vault
  nexoctl mb --with-hls local/videos
  nexoctl mb --with-images local/pictures
```

## Options

```
  -h, --help              help for mb
  -p, --ignore-existing   do not fail when the bucket already exists
      --region string     location constraint (default: the region of the alias)
      --with-hls          make a video bucket: the media rule "hls" (every rendition, a poster and thumbnails)
      --with-images       make an image bucket: the media rule "images" (every format at every width)
  -l, --with-lock         create the bucket with Object Lock (implies versioning)
      --with-versioning   enable versioning on the new bucket
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

