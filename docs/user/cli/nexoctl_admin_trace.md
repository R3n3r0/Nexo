# nexoctl admin trace

Show the requests served by the server, live

## Synopsis

Show the requests served by every node of the deployment as they complete, one line
per request: time, node, type, operation, status, duration, method and path (queries
redacted), bytes received and sent, user and error. Headers and bodies are never part of
the trace. The internode calls (type rpc) are shown only when asked with --type.

The stream lasts until Ctrl-C, --timeout or --count, and reconnects by itself when the
server restarts. With --json each request is a JSON line {"alias":...,"trace":{...}}.

Needs admin:ServerTrace (root, consoleAdmin, the diagnostics policy).

```
nexoctl admin trace [flags] ALIAS
```

## Examples

```
  nexoctl admin trace local
  nexoctl admin trace --errors --type s3,admin local
  nexoctl admin trace --api PutObject,GetObject --bucket photos --min-duration 500ms local
  nexoctl admin trace --status 5xx,404 --node n1:9000 --json local
```

## Options

```
      --api strings             operations (PutObject, "GET /buckets")
      --bucket strings          buckets
      --count int               stop after this many items (default: no limit)
  -e, --errors                  the failed requests only (status 400 or more)
  -h, --help                    help for trace
      --min-duration duration   the requests slower than this only (e.g. 500ms)
      --node strings            nodes of a cluster (host:port, repeated or comma-separated; default all)
      --status strings          status codes or classes (404, 5xx)
      --timeout duration        stop after this time (default: until Ctrl-C)
      --type strings            request types: s3, admin, web, rpc (default all but rpc)
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

