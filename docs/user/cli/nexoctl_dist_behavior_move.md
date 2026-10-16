# nexoctl dist behavior move

Move a behavior (1-based positions): the first match wins

```
nexoctl dist behavior move ALIAS ID FROM TO [flags]
```

## Examples

```
  nexoctl dist behavior move local website 3 1
```

## Options

```
  -h, --help   help for move
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

* [nexoctl dist behavior](nexoctl_dist_behavior.md)	 - Manage the behaviors per path of a distribution

