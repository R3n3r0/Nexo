## nexoctl admin profile

Collect a runtime profile (pprof) of the server

### Synopsis

Collect a runtime profile of the node that serves the Admin API of the alias, in the
pprof format: analyze it with "go tool pprof FILE". cpu, mutex and block sample for
--seconds (or --duration, 1s to 2m); heap and goroutine are snapshots. In a cluster
each node is profiled through its own console address (or the alias of that node).

Needs admin:Profiling (root, the diagnostics policy).

```
nexoctl admin profile [flags] ALIAS
```

### Examples

```
  nexoctl admin profile local
  nexoctl admin profile --type heap --output heap.pb.gz local
  nexoctl admin profile --type mutex --seconds 30 --output mutex.pb.gz local
```

### Options

```
  -d, --duration duration   sampling of the cpu, mutex and block profiles (default 10s, at most 2m)
  -h, --help                help for profile
  -o, --output string       file of the profile (default: the name suggested by the server, nexo-<node>-<type>.pb.gz)
  -s, --seconds int         sampling of the cpu, mutex and block profiles in seconds (same as --duration)
  -t, --type string         profile: cpu, heap, goroutine, mutex or block (default "cpu")
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

