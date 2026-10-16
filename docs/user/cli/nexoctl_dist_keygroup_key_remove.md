# nexoctl dist keygroup key remove

Remove a key from a key group (its signatures stop verifying)

```
nexoctl dist keygroup key remove ALIAS GROUP KEYID [flags]
```

## Aliases

```
nexoctl dist keygroup key rm
```

## Options

```
  -h, --help   help for remove
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

* [nexoctl dist keygroup key](nexoctl_dist_keygroup_key.md)	 - Add and remove the keys of a key group

