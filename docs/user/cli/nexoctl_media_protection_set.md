# nexoctl media protection set

Change the download protection of the videos of a bucket

## Synopsis

Change the download protection of the videos of a bucket: the flags given
replace those settings, the others keep their value.

```
nexoctl media protection set ALIAS/BUCKET [flags]
```

## Examples

```
  nexoctl media protection set local/courses --enable --prefix lessons/
  nexoctl media protection set local/courses --max-sessions 1 --origin https://learn.example.com
  nexoctl media protection set local/courses --max-sessions unlimited --token-ttl 4h
  nexoctl media protection set local/courses --disable
```

## Options

```
      --disable               stop protecting the videos
      --enable                protect the videos
  -h, --help                  help for set
      --max-sessions string   simultaneous streams of a viewer (1 to 100, or unlimited)
      --origin strings        web origin allowed to embed the player and fetch the keys (repeatable; "" clears)
      --pace int              keys served at most this many times faster than the video plays (1 to 20)
      --prefix string         protect only the keys under this prefix ("" for the whole bucket)
      --share-links           let share links play the protected videos (without watermark nor limits)
      --suffix strings        suffixes of the protected videos (repeatable; default .mp4, .mov, .m4v)
      --token-ttl duration    longest validity of a playback token (1m to 24h)
      --watermark             draw the viewer's watermark in the Nexo player (default true)
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

* [nexoctl media protection](nexoctl_media_protection.md)	 - Show or change the download protection of the videos of a bucket

