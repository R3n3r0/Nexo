# nexoctl admin replicate add

Group the deployments of the aliases (the alias names name the sites)

## Synopsis

Group the deployments of the aliases: the first one checks that it reaches every other
with its root credentials, saves the group on every site and starts a first round, which only adds
(nothing is deleted at the first round). The URL of each alias is the S3 endpoint of its site, its
Admin API URL the endpoint the sites call each other on: they must be reachable from every site.

```
nexoctl admin replicate add ALIAS ALIAS [ALIAS...] [flags]
```

## Examples

```
  nexoctl admin replicate add site-a site-b site-c
```

## Options

```
  -h, --help   help for add
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

