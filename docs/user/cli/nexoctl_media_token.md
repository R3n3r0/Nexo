# nexoctl media token

Issue a playback token of a video for a viewer

## Synopsis

Issue a playback token of a video for a viewer: a capability to stream the
video for a while without credentials, which an application hands to its user.
It prints the master playlist with the token, for any HLS player (hls.js,
Safari, ExoPlayer, VLC...), and the Nexo player page. The keys of a protected
video go to the token at the pace of playback, to at most the simultaneous
streams of its subject; "nexoctl media revoke" stops it.

```
nexoctl media token ALIAS/BUCKET/KEY --subject VIEWER [--watermark TEXT] [--ttl D] [flags]
```

## Examples

```
  nexoctl media token local/courses/lessons/intro.mp4 --subject student-1842 --watermark mario.rossi@example.com
  nexoctl media token --json --ttl 30m local/courses/lessons/intro.mp4 --subject student-1842
```

## Options

```
  -h, --help                help for token
      --subject string      the viewer the token is for (an account id of the application)
      --ttl duration        validity of the token (default 2h, at most the token TTL of the protection)
      --version-id string   pin the token to a version (default: the current one)
      --watermark string    text the Nexo player draws over the video (default: the subject)
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

