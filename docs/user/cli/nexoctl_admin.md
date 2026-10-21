# nexoctl admin

Administer a Nexo server (Admin API)

## Synopsis

Administer a Nexo server through its Admin API: server information and usage,
users, groups, policies, access keys, configuration, service control, the export and
import of the state of a deployment, the lock diagnostics, the KMS of the
server-side encryption, the notification targets, the live trace of the requests
and the live logs, the audit log, the metrics and their Prometheus configuration,
the runtime profiles, the speed tests, the metadata log of the drives and the DNS
providers of the ACME DNS-01 challenge.

The Admin API is served by the console listener of the server: see --admin-url
of "nexoctl alias set". Every subcommand takes the alias as its first argument.

```
nexoctl admin [flags]
```

## Options

```
  -h, --help   help for admin
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

* [nexoctl](nexoctl.md)	 - Nexo command-line client
* [nexoctl admin accesskey](nexoctl_admin_accesskey.md)	 - Manage the access keys (service accounts) of the users
* [nexoctl admin audit](nexoctl_admin_audit.md)	 - Search the audit log of the server
* [nexoctl admin cluster](nexoctl_admin_cluster.md)	 - Export and import the state of a deployment (IAM, policies, configuration, bucket settings)
* [nexoctl admin config](nexoctl_admin_config.md)	 - Read and change the dynamic configuration of the server
* [nexoctl admin decommission](nexoctl_admin_decommission.md)	 - Drain a server pool so that it can be removed
* [nexoctl admin dns-provider](nexoctl_admin_dns-provider.md)	 - Manage the DNS providers of the ACME DNS-01 challenge
* [nexoctl admin group](nexoctl_admin_group.md)	 - Manage the IAM groups and their members
* [nexoctl admin heal](nexoctl_admin_heal.md)	 - Heal the drives of the server (erasure-coded deployments)
* [nexoctl admin info](nexoctl_admin_info.md)	 - Show the server: version, mode, nodes and drives
* [nexoctl admin kms](nexoctl_admin_kms.md)	 - Show the KMS of the server-side encryption and manage its keys
* [nexoctl admin lock](nexoctl_admin_lock.md)	 - List and release the namespace locks of a cluster
* [nexoctl admin logs](nexoctl_admin_logs.md)	 - Show the logs of the server, live
* [nexoctl admin metadata-log](nexoctl_admin_metadata-log.md)	 - Inspect and compact the metadata log of the drives
* [nexoctl admin metrics](nexoctl_admin_metrics.md)	 - Query the recent metrics kept by the server
* [nexoctl admin notify](nexoctl_admin_notify.md)	 - Inspect and test the notification targets
* [nexoctl admin policy](nexoctl_admin_policy.md)	 - Manage the IAM policies (AWS policy language)
* [nexoctl admin profile](nexoctl_admin_profile.md)	 - Collect a runtime profile (pprof) of the server
* [nexoctl admin prometheus](nexoctl_admin_prometheus.md)	 - Integrate the metrics of the server with Prometheus
* [nexoctl admin rebalance](nexoctl_admin_rebalance.md)	 - Move objects from the fullest pools to the emptiest ones
* [nexoctl admin service](nexoctl_admin_service.md)	 - Restart or stop the server
* [nexoctl admin speedtest](nexoctl_admin_speedtest.md)	 - Measure the drives, the object layer or the network between the nodes
* [nexoctl admin trace](nexoctl_admin_trace.md)	 - Show the requests served by the server, live
* [nexoctl admin usage](nexoctl_admin_usage.md)	 - Show the data usage of the server and of each bucket
* [nexoctl admin user](nexoctl_admin_user.md)	 - Manage the IAM users

