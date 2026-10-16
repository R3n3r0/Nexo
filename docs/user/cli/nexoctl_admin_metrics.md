# nexoctl admin metrics

Query the recent metrics kept by the server

## Synopsis

Query the series of metrics the nodes sample every 10 seconds and keep for 24 hours (the
charts of the console): requests, errors, latencies, traffic, drives, healing, scanner,
internode calls, CPU and memory, per node and for the cluster. Prometheus scrapes the
complete metrics: see "nexoctl admin prometheus generate".

Needs admin:Prometheus (root, consoleAdmin, the diagnostics policy).

## Options

```
  -h, --help   help for metrics
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
* [nexoctl admin metrics list](nexoctl_admin_metrics_list.md)	 - List the series of metrics
* [nexoctl admin metrics query](nexoctl_admin_metrics_query.md)	 - Show the points of a series of metrics

