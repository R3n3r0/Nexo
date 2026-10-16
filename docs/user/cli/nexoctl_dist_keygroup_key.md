# nexoctl dist keygroup key

Add and remove the keys of a key group

```
nexoctl dist keygroup key [flags]
```

## Options

```
  -h, --help   help for key
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

* [nexoctl dist keygroup](nexoctl_dist_keygroup.md)	 - Manage the key groups of the signed URLs and cookies
* [nexoctl dist keygroup key add](nexoctl_dist_keygroup_key_add.md)	 - Add a key to a key group (generated, or your public key)
* [nexoctl dist keygroup key remove](nexoctl_dist_keygroup_key_remove.md)	 - Remove a key from a key group (its signatures stop verifying)

