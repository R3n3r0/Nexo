## nexoctl admin speedtest

Measure the drives, the object layer or the network between the nodes

### Synopsis

Run a speed test on the node that serves the Admin API of the alias and print the
throughput of each phase (write, then read), the operations per second and the latencies:

  drive   files written then read on every local drive (its temporary area)
  object  objects uploaded then downloaded through the object layer (erasure coding,
          quorum, internode RPC) in a temporary bucket nexo-speedtest-*, removed afterwards
  net     files sent to a drive of every other node of a cluster, then read back (the
          network between the nodes; not available out of a cluster)

Each phase lasts --duration (at most 1m) and writes at most 1 GiB per target. Needs
admin:Speedtest (root, the diagnostics policy). The test loads the server: run it
outside of the peak hours.

```
nexoctl admin speedtest [flags] ALIAS drive|object|net
```

### Examples

```
  nexoctl admin speedtest local drive
  nexoctl admin speedtest --size 16MiB --duration 10s --concurrency 32 local object
```

### Options

```
      --concurrency int     concurrency of the object test and of the reads of the net test (default 8, at most 64)
      --duration duration   duration of each phase (default 5s, at most 1m)
  -h, --help                help for speedtest
      --size string         size of each file or object (default 4MiB, at most 64MiB)
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

* [nexoctl admin](nexoctl_admin.md)	 - Administer a Nexo server (Admin API)

