# nexoctl media revoke

Revoke a playback token, or every token of a viewer

## Synopsis

Revoke a playback token by its session id, or every token of the videos of the
bucket issued to a viewer so far: they stop playing on every node within
seconds. A token issued to the viewer afterwards plays.

```
nexoctl media revoke ALIAS/BUCKET (--session ID | --subject VIEWER) [flags]
```

## Examples

```
  nexoctl media revoke local/courses --session 9f2c4e...
  nexoctl media revoke local/courses --subject student-1842
```

## Options

```
  -h, --help             help for revoke
      --session string   the session id of the token
      --subject string   the viewer whose tokens are revoked
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

