## nexoctl legalhold

Manage the Object Lock legal hold of objects

### Synopsis

Manage the legal hold of object versions: a version under legal hold cannot be
deleted, whatever its retention, until the hold is cleared. The bucket must have
been created with Object Lock ("nexoctl mb --with-lock").

```
nexoctl legalhold [flags]
```

### Options

```
  -h, --help   help for legalhold
```

### Options inherited from parent commands

```
      --config-dir string   configuration directory (default $NEXOCTL_CONFIG_DIR or ~/.nexoctl)
      --debug               dump the HTTP requests and responses on stderr, with the secrets redacted
      --json                print JSON Lines (one object per line with a "status" member)
      --no-color            disable colors (also with the NO_COLOR environment variable)
  -q, --quiet               hide confirmations and progress (results and errors are still printed)
```

### SEE ALSO

* [nexoctl](nexoctl.md)	 - Nexo command-line client
* [nexoctl legalhold clear](nexoctl_legalhold_clear.md)	 - Release the legal hold of objects
* [nexoctl legalhold info](nexoctl_legalhold_info.md)	 - Show the legal hold of objects
* [nexoctl legalhold set](nexoctl_legalhold_set.md)	 - Put objects under legal hold

