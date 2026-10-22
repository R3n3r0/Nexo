# nexoctl admin replicate remove

Dissolve the group of the site (on every site reached)

## Synopsis

Dissolve the group: every site reached forgets it and the bucket replication rules it
added. Nothing that was replicated is deleted. A site that cannot be reached keeps the group:
run the command on it too.

```
nexoctl admin replicate remove ALIAS [flags]
```

## Aliases

```
nexoctl admin replicate rm
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

* [nexoctl admin replicate](nexoctl_admin_replicate.md)	 - Keep several deployments in sync (site replication)

