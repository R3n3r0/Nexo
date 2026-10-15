## nexoctl admin prometheus generate

Generate the Prometheus scrape configuration of the server

### Synopsis

Print the scrape\_configs section of prometheus.yml that scrapes the metrics of the
server of the alias (default: cluster) on its S3 endpoint, under /nexo/metrics/v1/:

  cluster  the metrics of every node and of the deployment (one scrape for the cluster)
  node     the metrics of the node that answers (scrape each node)
  bucket   the metrics of the buckets (every minute)

With NEXO\_PROMETHEUS\_AUTH\_TYPE=jwt (the default) the scrapes carry a bearer token: a
JWT (HS512, issuer "prometheus") signed locally with the secret key of the alias,
whose subject is its access key, valid until --expiry. The access key needs
admin:Prometheus (root, consoleAdmin, the diagnostics policy): prefer a dedicated
access key with the diagnostics policy. The token stops working when it expires or
when the secret key changes; keep the configuration as secret as the key. With
NEXO\_PROMETHEUS\_AUTH\_TYPE=public the endpoints need no token: see --public.

```
nexoctl admin prometheus generate [flags] ALIAS [cluster|node|bucket]
```

### Examples

```
  nexoctl admin prometheus generate local
  nexoctl admin prometheus generate --expiry 30d local node
  nexoctl admin prometheus generate local bucket >> prometheus.yml
  nexoctl admin prometheus generate --public local
```

### Options

```
      --expiry string   validity of the token: a duration (30d, 12h) or a date (2027-12-31, RFC 3339) (default "365d")
  -h, --help            help for generate
      --job string      name of the scrape job (default nexo-<type>)
      --public          no bearer token: the endpoints of the server are public (NEXO_PROMETHEUS_AUTH_TYPE=public)
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

* [nexoctl admin prometheus](nexoctl_admin_prometheus.md)	 - Integrate the metrics of the server with Prometheus

