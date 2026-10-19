# nexoctl find

Find objects and files by name, size and age

## Synopsis

Search the objects under a prefix (or the files under a local directory)
recursively and print the matches, or run a command for each one with --exec
("{}" is replaced by the path; the command is split on spaces, no shell).
The media sidecars of a bucket (.nexo-media/) are skipped unless --media is
given or the target is under them.

```
nexoctl find [flags] TARGET...
```

## Examples

```
  nexoctl find local/logs --name "*.log" --older-than 1d
  nexoctl find local/media --larger 1GiB
  nexoctl find local/logs --name "*.gz" --exec "nexoctl rm {}"
```

## Options

```
      --exec string         run a command for each match ("{}" is the path)
  -h, --help                help for find
      --larger string       only objects larger than the size (e.g. 10MiB)
      --maxdepth int        descend at most this many levels (0: unlimited)
      --media               also search the media sidecars (.nexo-media/)
      --name string         glob on the base name (e.g. "*.log")
      --newer-than string   only objects newer than the duration
      --older-than string   only objects older than the duration (e.g. 7d)
      --path string         glob on the path under the target (e.g. "2026/*/*.csv")
      --smaller string      only objects smaller than the size
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

