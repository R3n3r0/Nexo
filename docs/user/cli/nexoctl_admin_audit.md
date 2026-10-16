# nexoctl admin audit

Search the audit log of the server

## Synopsis

Search the recent audit records kept in memory by every node of the deployment
(the external audit targets keep the complete log: see "nexoctl admin config set
ALIAS audit\_webhook ..."), newest first. One line per record: time, status, duration,
user, operation, bucket/object and error.

--since and --until take a duration before now (15m, 2h, 7d) or a date (RFC 3339).

Needs admin:ConsoleLog (root, consoleAdmin, the diagnostics policy).

```
nexoctl admin audit [flags] ALIAS
```

## Examples

```
  nexoctl admin audit local
  nexoctl admin audit --user alice --since 1h local
  nexoctl admin audit --action DeleteObject --bucket photos --since 2026-10-15T09:00:00Z local
  nexoctl admin audit --errors --status 403 --limit 1000 --json local
```

## Options

```
      --action string       operation (PutObject, DeleteObject, ...)
      --bucket string       bucket
  -e, --errors              the failed requests only (status 400 or more)
  -h, --help                help for audit
      --limit int           maximum number of records (default 100, at most 1000)
      --node strings        nodes of a cluster (host:port; default all)
      --request-id string   the records of a request ID
      --since string        records after this time (a duration before now, or RFC 3339)
      --status strings      status codes or classes (404, 5xx)
      --type string         request type: s3 or admin (default both)
      --until string        records before this time (a duration before now, or RFC 3339)
      --user string         access key or parent user
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

* [nexoctl admin](nexoctl_admin.md)	 - Administer a Nexo server (Admin API)

